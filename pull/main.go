// superci-pull runs GitHub runner scale sets on EC2, one per runner size (superci-2cpu … superci-32cpu), optionally
// several per size. It long-polls GitHub's queue of each set (GitHub holds every job until a runner takes it),
// converges each set's machines to "jobs assigned + hot pool" with batched instant EC2 Fleet requests, and registers a
// just-in-time runner for a machine when the machine asks with its signed identity, inside what GitHub's
// organization-wide registration cap leaves spare. Its state is EC2's tags and GitHub's queues; a restart rebuilds the
// rest.
package main

import (
	"context"
	"crypto/tls"
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
)

//go:embed boot.sh
var bootScript string

// hydrateStop is the runner's job-started hook, which ends the hydration a machine still has running.
//
//go:embed hydrate-stop.sh
var hydrateStop string

func main() {
	var (
		configPath = flag.String("config", "/etc/superci-pull/config.json", "override file, decoded over the built-in config and reread when it changes")
		publicIP   = flag.String("public-ip", "", "the listener's stable address (its Elastic IP), which machines reach")
		groups     = flag.String("security-groups", "", "runner security group of each region: us-east-1=sg-…,us-east-2=sg-…")
		listen     = flag.String("listen", ":8443", "address of the machines' endpoint")
		admin      = flag.String("admin", "127.0.0.1:8080", "address of the operator's endpoint")
		tlsParam   = flag.String("tls-param", "/superci-pull/tls", "SSM SecureString (us-east-1) holding the listener's TLS key")
		spendParam = flag.String("spend-param", "/superci-pull/spend", "SSM parameter (us-east-1) holding today's and this month's spend")
		claims     = flag.String("claims", "", "S3 bucket (us-east-1) of the machines' claims; superci-pull-claims-<account> by default")
		poolParam  = flag.String("pool-param", "/superci-pull/pool", "SSM parameter prefix (us-east-1); the deployment keeps each set's burst and launch times across a restart in <prefix>/<deployment>")
		noMetrics  = flag.Bool("no-metrics", false, "publish no CloudWatch metrics")
		deleteSets = flag.Bool("delete-scale-sets", false, "delete every configured scale set and exit")
		validate   = flag.Bool("validate", false, "check the override file named by -config over the built-in config, and exit")
		version    = flag.Bool("version", false, "print the commit this binary was built from, and exit")
	)
	flag.Parse()
	if *version {
		fmt.Println(revision)
		return
	}
	if *validate {
		if _, err := LoadConfig(*configPath); err != nil {
			fmt.Fprintf(os.Stderr, "invalid: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("valid: %s\n", *configPath)
		return
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	slog.Info("start", "revision", revision)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err := run(ctx, flags{*configPath, *publicIP, pairs(*groups), *listen, *admin, *tlsParam, *spendParam, *claims, *poolParam, *noMetrics, *deleteSets})
	if err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("exit", "err", err.Error())
		os.Exit(1)
	}
}

type flags struct {
	configPath, publicIP  string
	groups                map[string]string
	listen, admin         string
	tlsParam, spendParam  string
	claims                string
	poolParam             string
	noMetrics, deleteSets bool
}

func pairs(s string) map[string]string {
	out := map[string]string{}
	for _, kv := range strings.Split(s, ",") {
		if k, v, ok := strings.Cut(kv, "="); ok {
			out[k] = v
		}
	}
	return out
}

// Listener is the process: every set, and what they share (the registration budget, the spend, EC2, GitHub).
type Listener struct {
	path       string
	mu         sync.Mutex
	cfg        Config
	modified   time.Time
	pins       map[string]int // pool pinned by the operator per set, until the config file changes
	sets       []*Set
	dsets      []*DispatchSet // the sizes the identity plane serves
	dispatch   *Dispatcher
	group      int // the deployment's runner group at GitHub
	fleet      *Fleet
	tagger     *Tagger
	budget     *Budget
	spend      *Spend
	rest       *REST
	client     *scaleset.Client
	rerun      *Rerunner
	reconciled time.Time
	strays     map[string]*Machine // machines of the deployment of no configured set, by id
	short      int                 // machines asked of EC2 and not started (capacity, quotas), since the metrics last took them
	leaked     int                 // probe runners found registered past their grace, since the metrics last took them
	identities []IdentityCount     // identities online and busy per dispatch size, as the last listing of the identities group counted them
	counted    time.Time           // when that listing was read
}

// defaultGroup is the organization's Default runner group, where an earlier build made the sets.
const defaultGroup = 1

func run(ctx context.Context, f flags) error {
	cfg, err := LoadConfig(f.configPath)
	if err != nil {
		return err
	}
	app, err := LoadApp(ctx, "us-east-1", cfg.AppParam)
	if err != nil {
		return fmt.Errorf("app credentials: %w", err)
	}
	client, err := scaleset.NewClientWithGitHubApp(scaleset.ClientWithGitHubAppConfig{
		GitHubConfigURL: "https://github.com/" + cfg.Org,
		GitHubAppAuth:   scaleset.GitHubAppAuth{ClientID: strconv.FormatInt(app.ID, 10), InstallationID: cfg.Installation, PrivateKey: app.PEM},
		SystemInfo:      scaleset.SystemInfo{System: "superci-pull", Version: "0.2.0", Subsystem: "listener"},
	})
	if err != nil {
		return err
	}
	specs := cfg.Sets()
	rest := NewREST(app, cfg.Installation, cfg.Org)
	group, err := rest.RunnerGroup(ctx, cfg.Deployment, cfg.Repositories)
	if err != nil {
		return fmt.Errorf("runner group %s: %w", cfg.Deployment, err)
	}
	slog.Info("runner group", "name", cfg.Deployment, "id", group, "repositories", cfg.Repositories)
	tlStart(cfg.Deployment, time.Now())
	if f.deleteSets {
		for _, spec := range specs {
			for _, g := range []int{group, defaultGroup} {
				s, err := client.GetRunnerScaleSet(ctx, g, spec.Name)
				if err != nil || s == nil {
					continue
				}
				slog.Info("deleting scale set", "name", spec.Name, "id", s.ID, "group", g, "err", errString(client.DeleteRunnerScaleSet(ctx, s.ID)))
			}
		}
		return nil
	}
	awsCfg, err := config.LoadDefaultConfig(ctx, config.WithRegion("us-east-1"))
	if err != nil {
		return err
	}
	ssmc := ssm.NewFromConfig(awsCfg)
	// No grant goes on without its claim, so a listener that cannot reach the claims does not start.
	bucket := f.claims
	if bucket == "" {
		bucket = "superci-pull-claims-" + cfg.Account
	}
	s3c := s3.NewFromConfig(awsCfg)
	if _, err := s3c.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: &bucket}); err != nil {
		return fmt.Errorf("claims bucket %s (deploy/up.sh makes it): %w", bucket, err)
	}
	claims := &Claims{store: s3c, bucket: bucket}
	id, err := LoadIdentity(ctx, ssmc, f.tlsParam)
	if err != nil {
		return fmt.Errorf("tls identity: %w", err)
	}
	verifier, err := NewVerifier(cfg.Account)
	if err != nil {
		return err
	}
	fleet, err := NewFleet(ctx, cfg.Deployment, cfg.Regions, ImageConfig{Owner: "135269210855", Name: "runs-on-v2.2-ubuntu24-full-x64-*", Pin: "20261102081424"})
	if err != nil {
		return err
	}
	listenAt := fmt.Sprintf("https://%s%s", f.publicIP, f.listen)
	now := time.Now()
	l := &Listener{path: f.configPath, cfg: cfg, modified: modTime(f.configPath), pins: map[string]int{}, group: group, fleet: fleet,
		tagger: NewTagger(ctx, fleet), budget: NewBudget(cfg.Registration, now), spend: NewSpend(cfg.DailyUSD, cfg.MonthlyUSD, specs, now),
		rest: rest, client: client, strays: map[string]*Machine{}, reconciled: now}
	l.rerun = NewRerunner(l.rest, fleet.SpotReclaimed)
	// The organization's rate is measured from the start, while the sets are set up: it takes two samples 61 s apart.
	go func() {
		l.sampleOrg(ctx)
		every(ctx, 5*time.Second, func() { l.sampleOrg(ctx) })
	}()
	if err := l.spend.Load(ctx, ssmc, f.spendParam); err != nil {
		slog.Error("load spend", "err", err.Error())
	}
	regions := make([]string, 0, len(fleet.regions))
	for _, r := range fleet.regions {
		regions = append(regions, r.Name)
	}
	var scaled []SetSpec
	for _, spec := range specs {
		if spec.Dispatch {
			sf, err := fleet.AddSet(ctx, spec, bootDataMode(cfg, listenAt, id.Pin, spec.Label, "dispatch"), f.groups)
			if err != nil {
				return err
			}
			ds := NewDispatchSet(spec, sf, regions, now)
			ds.SetNoDemand(cfg.NoDemand)
			l.dsets = append(l.dsets, ds)
			continue
		}
		scaled = append(scaled, spec)
		gid, err := ensureScaleSet(ctx, client, group, spec)
		if err != nil {
			return fmt.Errorf("scale set %s: %w", spec.Name, err)
		}
		sf, err := fleet.AddSet(ctx, spec, bootData(cfg, listenAt, id.Pin, spec.Label), f.groups)
		if err != nil {
			return err
		}
		l.sets = append(l.sets, &Set{spec: spec, id: gid, state: setState(spec, cfg, regions, now), fleet: sf})
	}
	// A size the identity plane serves has no scale set: one left from before it moved is drained, then deleted.
	dropped, err := droppedScaleSets(ctx, client, group, scaled)
	if err != nil {
		return fmt.Errorf("prune scale sets: %w", err)
	}
	for _, d := range dropped {
		slog.Warn("draining scale set the config no longer has", "name", d.Name, "id", d.ID)
		l.sets = append(l.sets, &Set{spec: SetSpec{Name: d.Name}, id: d.ID, state: NewState(d.Name, 0, 0, regions, now), drain: true})
	}
	for _, s := range l.sets {
		s.state.SetPricer(l.spend.Price)
	}
	for _, d := range l.dsets {
		d.SetPricer(l.spend.Price)
	}
	poolParam := poolParamName(f.poolParam, cfg.Deployment)
	if err := loadPools(ctx, ssmc, poolParam, l.sets, l.dsets, cfg.Pool.window(), now); err != nil {
		slog.Error("load pools", "err", err.Error())
	}
	refresh := l.refresher()
	for _, r := range fleet.regions {
		refresh(ctx, r.Name)
	}
	l.setPools(now)

	byName := map[string]*Set{}
	for _, s := range l.sets {
		byName[s.spec.Name] = s
	}
	l.dispatch = NewDispatcher(cfg.Deployment, l.dsets, verifier, claims, fleet, refresh)
	defer l.dispatch.Close()
	srv := &Server{sets: byName, budget: l.budget, verifier: verifier, client: client, refresh: refresh, claim: claims.Take, issue: l.tagger.Issue, notices: fleet.TagNoticed,
		slots: make(chan struct{}, max(1, cfg.Registration.Concurrency)), maxAge: l.maxAge, dispatch: l.dispatch, dispatchMachine: l.dispatchMachine}
	go serve(ctx, publicServer(f.listen, srv), &id)
	ids := NewIdentities(rest, l.budget, cfg)
	go serve(ctx, &http.Server{Addr: f.admin, Handler: Admin(l.status, l.healthy, l.override, l.budget, ids.Routes)}, nil)
	errs := make(chan error, len(l.sets))
	var sessions sync.WaitGroup
	for _, s := range l.sets {
		wake := make(chan struct{}, 1)
		sctx, stop := context.WithCancel(ctx)
		defer stop()
		if s.drain {
			go l.drain(ctx, s, stop)
		} else {
			go l.launcher(ctx, s, wake)
		}
		sessions.Add(1)
		go func() {
			defer sessions.Done()
			// A drained set's session ends with the set; only the process's own end, or a failure, ends the process.
			if err := listenSet(sctx, client, s, wake); sctx.Err() == nil || ctx.Err() != nil {
				errs <- err
			}
		}()
	}
	for _, d := range l.dsets {
		go l.dispatchLauncher(ctx, d)
	}
	go l.reconciler(ctx, refresh)
	go l.lister(ctx)
	go every(ctx, 15*time.Second, func() { l.setPools(time.Now()) })
	go every(ctx, 30*time.Minute, func() { l.price(ctx) })
	go every(ctx, time.Minute, func() {
		if err := l.spend.Save(ctx, ssmc, f.spendParam); err != nil {
			slog.Error("save spend", "err", err.Error())
		}
		if err := savePools(ctx, ssmc, poolParam, l.sets, l.dsets, time.Now(), l.config().Pool); err != nil {
			slog.Error("save pools", "err", err.Error())
		}
	})
	go every(ctx, 30*time.Second, l.watchConfig)
	if !f.noMetrics {
		cw := cloudwatch.NewFromConfig(awsCfg)
		var last minute
		go every(ctx, time.Minute, func() {
			publish(ctx, cw, last.of(l.metrics()), time.Now())
		})
		if len(l.dsets) > 0 {
			go func() {
				l.countIdentities(ctx, ids)
				every(ctx, identityCountEvery, func() { l.countIdentities(ctx, ids) })
			}()
		}
	}
	if l.cfg.Rerun {
		if jobs, err := fleet.NoticedJobs(ctx); err != nil {
			slog.Error("rebuild lost jobs", "err", err.Error())
		} else {
			l.rerun.Rebuild(jobs, now)
		}
		go l.rerun.Run(ctx)
	}
	go l.price(ctx)
	select {
	case <-ctx.Done():
		// Each set closes its message session on the way out; a session left open would refuse the next listener's
		// for about a minute.
		closed := make(chan struct{})
		go func() { sessions.Wait(); close(closed) }()
		select {
		case <-closed:
		case <-time.After(10 * time.Second):
		}
		return ctx.Err()
	case err := <-errs:
		return err
	}
}

// healthyWithin is how long the reconciler may go without finishing a pass before the listener reads as dead.
const healthyWithin = 2 * time.Minute

// recycleBatch is how many machines of an older boot a set replaces each reconcile pass (10 s): a pool of 100 turns over
// in about 3 minutes, and its idle runners never drop by more than a launch's worth of passes.
const recycleBatch = 5

// minute turns the budget's totals since the start into what happened in one minute: each call fills m's Regs, Refused,
// Failed and Denied with the difference from the call before.
type minute struct {
	made, refused, failed int
	denied                map[string]int
}

func (p *minute) of(m Metrics) Metrics {
	b := m.Budget
	m.Regs, m.Refused, m.Failed = b.Made-p.made, b.Refused-p.refused, b.Failed-p.failed
	m.Denied = deniedSince(p.denied, b.Denied)
	p.made, p.refused, p.failed, p.denied = b.Made, b.Refused, b.Failed, b.Denied
	return m
}

// deniedSince is what the budget denied between two snapshots, by reason.
func deniedSince(before, now map[string]int) map[string]int {
	out := make(map[string]int, len(now))
	for reason, n := range now {
		out[reason] = n - before[reason]
	}
	return out
}

// setState is a configured set's state, with the config's batch rule.
func setState(spec SetSpec, cfg Config, regions []string, now time.Time) *State {
	s := NewState(spec.Name, 0, spec.Max, regions, now)
	s.SetBatch(cfg.Batch)
	s.SetNoDemand(cfg.NoDemand)
	return s
}

// bootData is a machine's user data: boot.sh with the listener's address and key, the longest a machine may live
// (the oldest a pool machine may be, and the longest job after it), and its set's label, which names the hydration
// manifest it reads.
func bootData(c Config, listener, pin, label string) string {
	return bootDataMode(c, listener, pin, label, "scaleset")
}

// bootDataMode is bootData for a mode: "scaleset" asks for a just-in-time runner as always, "dispatch" runs the agent
// (agent.py) that takes the one job the dispatcher pairs the machine with.
func bootDataMode(c Config, listener, pin, label, mode string) string {
	return strings.NewReplacer("@MODE@", mode, "@LISTENER@", listener, "@PIN@", pin, "@LIFE_SECONDS@", strconv.Itoa(int(c.life().Seconds())), "@LABEL@", label, "@WARM_CAP_SECONDS@", strconv.Itoa(int(warmCap.Seconds())), "@HYDRATE_STOP@", strings.TrimSpace(hydrateStop)).Replace(bootScript)
}

// ensureScaleSet finds or makes the set at GitHub, in the deployment's runner group, with its labels: its name, and
// the size's label when it is not the size's first set. A set an earlier build made in the Default group is moved.
func ensureScaleSet(ctx context.Context, client *scaleset.Client, group int, spec SetSpec) (int, error) {
	labels := []scaleset.Label{{Name: spec.Name}}
	if spec.Name != spec.Label {
		labels = append(labels, scaleset.Label{Name: spec.Label})
	}
	s, err := client.GetRunnerScaleSet(ctx, group, spec.Name)
	if err != nil {
		return 0, err
	}
	if s == nil && group != defaultGroup {
		old, err := client.GetRunnerScaleSet(ctx, defaultGroup, spec.Name)
		if err != nil {
			return 0, err
		}
		if old != nil {
			old.RunnerGroupID, old.RunnerGroupName = group, ""
			if s, err = client.UpdateRunnerScaleSet(ctx, old.ID, old); err != nil {
				return 0, fmt.Errorf("move to runner group %d: %w", group, err)
			}
			slog.Info("moved scale set", "name", spec.Name, "id", s.ID, "group", s.RunnerGroupID)
			if s.RunnerGroupID != group {
				return 0, fmt.Errorf("moved to runner group %d, GitHub says %d", group, s.RunnerGroupID)
			}
		}
	}
	if s == nil {
		s, err = client.CreateRunnerScaleSet(ctx, &scaleset.RunnerScaleSet{Name: spec.Name, RunnerGroupID: group, Labels: labels,
			RunnerSetting: scaleset.RunnerSetting{DisableUpdate: true}})
		if err != nil {
			return 0, err
		}
		slog.Info("created scale set", "name", spec.Name, "id", s.ID, "labels", labelNames(s.Labels))
		return s.ID, nil
	}
	if !slices.Equal(labelNames(s.Labels), labelNames(labels)) {
		s.Labels = labels
		if s, err = client.UpdateRunnerScaleSet(ctx, s.ID, s); err != nil {
			return 0, err
		}
	}
	slog.Info("scale set", "name", spec.Name, "id", s.ID, "labels", labelNames(s.Labels))
	return s.ID, nil
}

// ownSetName is the form of every set this listener makes: superci-<cpu>cpu, or superci-<cpu>cpu-<k> for a split.
var ownSetName = regexp.MustCompile(`^superci-[0-9]+cpu(-[0-9]+)?$`)

// droppedScaleSets finds the sets of the deployment's runner group the config no longer has (a size removed, or a
// split made smaller): one left at GitHub would keep taking a share of its label's jobs with no listener to run them,
// and deleting one fails the jobs it holds, so each is drained before it is deleted (drain). Another deployment's
// sets are in its own group, so they are never touched.
func droppedScaleSets(ctx context.Context, client *scaleset.Client, group int, specs []SetSpec) ([]scaleset.RunnerScaleSet, error) {
	want := map[string]bool{}
	for _, s := range specs {
		want[s.Name] = true
	}
	sets, err := client.ListRunnerScaleSets(ctx, group)
	if err != nil {
		return nil, err
	}
	var dropped []scaleset.RunnerScaleSet
	for _, s := range sets {
		if ownSetName.MatchString(s.Name) && !want[s.Name] {
			dropped = append(dropped, s)
		}
	}
	// A set of this form left in the Default group (an earlier build's) takes a share of its label's jobs with no
	// listener; it is another deployment's to delete, or the operator's.
	if old, err := client.ListRunnerScaleSets(ctx, defaultGroup); err == nil {
		for _, s := range old {
			if ownSetName.MatchString(s.Name) {
				slog.Warn("a scale set of this form is in the Default runner group", "name", s.Name, "id", s.ID)
			}
		}
	}
	return dropped, nil
}

// drain retires a set the config dropped. Its session asks for no jobs (max 0) and acquires none, so no new job
// goes to it; its machines end as the rest do (a completed job, an idle runner past the supply, a machine that gets
// no runner). Once it holds no machine, so no runner of it can hold a job, it is deleted at GitHub and its session
// ends.
func (l *Listener) drain(ctx context.Context, s *Set, stop context.CancelFunc) {
	for tick := time.NewTicker(30 * time.Second); ; {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		if !s.state.Drained(time.Now()) {
			continue
		}
		err := l.client.DeleteRunnerScaleSet(ctx, s.id)
		slog.Warn("deleted drained scale set", "name", s.spec.Name, "id", s.id, "err", errString(err))
		if err == nil || errors.Is(err, scaleset.NotFoundError) {
			stop()
			return
		}
	}
}

func labelNames(ls []scaleset.Label) []string {
	out := make([]string, 0, len(ls))
	for _, l := range ls {
		out = append(out, l.Name)
	}
	sort.Strings(out)
	return out
}

// publicServer is the listener's server on the public port. A request is read within 30 s (headers within 10), so a
// client that trickles a body holds no goroutine or descriptor for longer. The identity plane's /work and /shim
// connections are hijacked after their body is read: Hijack clears the connection's deadlines, and each wire sets its
// own (dispatch.go).
func publicServer(addr string, h http.Handler) *http.Server {
	return &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second}
}

func serve(ctx context.Context, s *http.Server, id *Identity) {
	go func() { <-ctx.Done(); _ = s.Shutdown(context.Background()) }()
	var err error
	if id != nil {
		s.TLSConfig = &tls.Config{Certificates: []tls.Certificate{id.Cert}, MinVersion: tls.VersionTLS12}
		err = s.ListenAndServeTLS("", "")
	} else {
		err = s.ListenAndServe()
	}
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("server", "addr", s.Addr, "err", err.Error())
		os.Exit(1)
	}
}

func every(ctx context.Context, d time.Duration, f func()) {
	for tick := time.NewTicker(d); ; {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			f()
		}
	}
}

// listenSet keeps one message session of a set open and hands each answer to the set's state. A lost session (a
// crash of ours, a network fault, the service's own refusal, or a dead listener's session not yet expired) is opened
// again; GitHub redelivers what was not acknowledged.
func listenSet(ctx context.Context, client *scaleset.Client, s *Set, wake chan struct{}) error {
	host, _ := os.Hostname()
	quiet := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelWarn})).With("set", s.spec.Name)
	for wait := time.Second; ; wait = min(wait*2, 30*time.Second) {
		session, err := client.MessageSessionClient(ctx, s.id, host)
		if err == nil {
			wait = time.Second
			l, lerr := listener.New(session, listener.Config{ScaleSetID: s.id, MaxRunners: s.spec.Max, Logger: quiet})
			if lerr != nil {
				return lerr
			}
			err = l.Run(ctx, &scaler{set: s, session: session, wake: wake})
			_ = session.Close(context.WithoutCancel(ctx))
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		slog.Error("session", "set", s.spec.Name, "err", err.Error(), "retry_in", wait.String())
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
}

type scaler struct {
	set     *Set
	session *scaleset.MessageSessionClient
	wake    chan struct{}
}

func (s *scaler) Scale(ctx context.Context, msg *scaleset.RunnerScaleSetMessage) error {
	defer func() {
		select {
		case s.wake <- struct{}{}:
		default:
		}
	}()
	if msg != nil && len(msg.JobAvailableMessages) > 0 && !s.set.drain {
		ids := make([]int64, 0, len(msg.JobAvailableMessages))
		for _, j := range msg.JobAvailableMessages {
			ids = append(ids, j.RunnerRequestID)
		}
		got, err := s.session.AcquireJobs(ctx, ids)
		if err != nil {
			return fmt.Errorf("acquire jobs: %w", err)
		}
		slog.Info("acquired", "set", s.set.spec.Name, "asked", len(ids), "got", len(got))
		tlAcquired(s.set.spec.Name, got, time.Now())
	}
	done := s.set.state.Observe(msg, time.Now())
	tlMessage(s.set.spec.Name, msg, time.Now())
	for _, r := range done {
		tlJobRecord(r, time.Now())
	}
	if msg == nil {
		return nil
	}
	st := msg.Statistics
	if st == nil {
		st = &scaleset.RunnerScaleSetStatistic{}
	}
	slog.Info("message", "set", s.set.spec.Name, "id", msg.MessageID, "available", len(msg.JobAvailableMessages), "assigned", len(msg.JobAssignedMessages),
		"started", len(msg.JobStartedMessages), "completed", len(msg.JobCompletedMessages),
		"s_available", st.TotalAvailableJobs, "s_acquired", st.TotalAcquiredJobs, "s_assigned", st.TotalAssignedJobs,
		"s_running", st.TotalRunningJobs, "s_registered", st.TotalRegisteredRunners, "s_busy", st.TotalBusyRunners, "s_idle", st.TotalIdleRunners)
	return nil
}

// launcher turns a set's deficit into fleet requests. A message wakes it; it waits a moment so a burst's messages make
// few launches, and lets several launches run at once (an instant fleet in us-east-1 can take half a minute to
// answer). Across every set, no more machines wait for a runner than the registration budget can serve in about a
// minute and a half.
func (l *Listener) launcher(ctx context.Context, s *Set, wake chan struct{}) {
	slots := make(chan struct{}, 16)
	for {
		select {
		case <-ctx.Done():
			return
		case <-wake:
		case <-time.After(2 * time.Second):
		}
		time.Sleep(300 * time.Millisecond)
		select {
		case slots <- struct{}{}:
		default:
			continue
		}
		now := time.Now()
		limit := max(10, int(l.budget.Rate()*90))
		for _, other := range l.sets {
			if other != s {
				limit -= other.state.Unregistered()
			}
		}
		round, n := s.state.Deficit(now, limit)
		if n == 0 {
			<-slots
			continue
		}
		tlDecision(s.state.Snapshot(now), round, n, limit, l.budget.Estimate(now).SpareRate, now)
		go func() {
			defer func() { <-slots }()
			now := time.Now()
			got := s.fleet.Launch(ctx, round, n, s.state.OnDemandRoom(l.config().OnDemand, n, now))
			s.state.Launched(round, got, now)
			l.mu.Lock()
			l.short += n - len(got)
			l.mu.Unlock()
			slog.Info("launched", "set", s.spec.Name, "round", round, "asked", n, "got", len(got), "ms", time.Since(now).Milliseconds())
			if len(got) < n {
				time.Sleep(5 * time.Second)
			}
		}()
	}
}

// refresher lists a region's machines of every set again, at most once a second per region however many ask, and
// gives each set its own. Machines of the deployment of no configured set are kept as strays.
func (l *Listener) refresher() func(context.Context, string) {
	var mu sync.Mutex
	locks := map[string]*sync.Mutex{}
	last := map[string]time.Time{}
	return func(ctx context.Context, region string) {
		mu.Lock()
		lk := locks[region]
		if lk == nil {
			lk = &sync.Mutex{}
			locks[region] = lk
		}
		mu.Unlock()
		lk.Lock()
		defer lk.Unlock()
		mu.Lock()
		fresh := time.Since(last[region]) < time.Second
		mu.Unlock()
		r := l.fleet.Region(region)
		if fresh || r == nil {
			return
		}
		began := time.Now()
		dctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		ms, err := l.fleet.Describe(dctx, r)
		cancel()
		if err != nil {
			slog.Error("describe", "region", region, "err", err.Error())
			return
		}
		now := time.Now()
		scaled, dispatched, strays := sortMachines(ms, l.sets, l.dsets)
		var gone []JobRef
		for _, s := range l.sets {
			gone = append(gone, s.state.Seen(region, scaled[s], began, now)...)
		}
		for _, d := range l.dsets {
			d.Seen(region, dispatched[d], began, now)
		}
		for _, j := range gone {
			l.rerun.Add(j, now, false)
		}
		l.mu.Lock()
		for id, m := range l.strays {
			if m.Region == region {
				delete(l.strays, id)
			}
		}
		for _, m := range strays {
			l.strays[m.ID] = m
		}
		l.mu.Unlock()
		mu.Lock()
		last[region] = time.Now()
		mu.Unlock()
	}
}

// sortMachines gives each set the listed machines of its own: a scale set those without the dispatch tag, a dispatch
// set those with it. A size moving from one to the other keeps machines of both for a while, and neither set may take
// the other's: a scale set ends a machine that never asks for a runner, and a dispatch set one that never comes to
// /work. The rest are of no configured set.
func sortMachines(ms []*Machine, sets []*Set, dsets []*DispatchSet) (map[*Set][]*Machine, map[*DispatchSet][]*Machine, []*Machine) {
	scale, disp := map[string]*Set{}, map[string]*DispatchSet{}
	for _, s := range sets {
		scale[s.spec.Name] = s
	}
	for _, d := range dsets {
		disp[d.spec.Name] = d
	}
	scaled, dispatched := map[*Set][]*Machine{}, map[*DispatchSet][]*Machine{}
	var strays []*Machine
	for _, m := range ms {
		switch s, d := scale[m.Set], disp[m.Set]; {
		case m.Dispatch && d != nil:
			dispatched[d] = append(dispatched[d], m)
		case !m.Dispatch && s != nil:
			scaled[s] = append(scaled[s], m)
		default:
			strays = append(strays, m)
		}
	}
	return scaled, dispatched, strays
}

// endStrays is the strays to terminate now: at 10 minutes old, and again a minute on while EC2 lists one. A machine of
// the dispatch mode may be running its job, as when its size has moved back to a scale set, so it is ended only past
// a machine's life; it powers off by itself once its job is over, or told there is none.
func (l *Listener) endStrays(now time.Time, life time.Duration) []*Machine {
	l.mu.Lock()
	defer l.mu.Unlock()
	var end []*Machine
	for _, m := range l.strays {
		after := 10 * time.Minute
		if m.Dispatch {
			after = life
		}
		if now.Sub(m.Launched) > after && now.Sub(m.Ended) > time.Minute {
			m.Ended = now
			end = append(end, m)
		}
	}
	return end
}

// dispatchLauncher launches a dispatch set's deficit, woken by a job's arrival at the dispatcher. It needs no
// registration, so nothing but the set's own demand bounds a launch.
func (l *Listener) dispatchLauncher(ctx context.Context, d *DispatchSet) {
	slots := make(chan struct{}, 16)
	for {
		select {
		case <-ctx.Done():
			return
		case <-d.wake:
		case <-time.After(2 * time.Second):
		}
		time.Sleep(300 * time.Millisecond)
		select {
		case slots <- struct{}{}:
		default:
			continue
		}
		now := time.Now()
		round, n := d.Deficit(now)
		if n == 0 {
			<-slots
			continue
		}
		go func() {
			defer func() { <-slots }()
			got := d.fleet.Launch(ctx, round, n, d.OnDemandRoom(l.config().OnDemand, n, now))
			d.Launched(round, got, time.Now())
			l.mu.Lock()
			l.short += n - len(got)
			l.mu.Unlock()
			slog.Info("launched", "set", d.spec.Name, "round", round, "asked", n, "got", len(got), "ms", time.Since(now).Milliseconds())
			if len(got) < n {
				time.Sleep(5 * time.Second)
			}
		}()
	}
}

// lister lists the runners of the deployment's group on its own, so a slow GitHub slows only it: every 30 s, or 3 s a
// page for a big pool (at most 1,200 requests an hour). Each set learns its runners, and stale ones are removed.
func (l *Listener) lister(ctx context.Context) {
	for wait := time.Duration(0); ; {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		started := time.Now()
		lctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		runners, pages, err := l.rest.Runners(lctx, l.group, "superci-")
		cancel()
		wait = max(30*time.Second, time.Duration(pages)*3*time.Second) - time.Since(started)
		if err != nil {
			slog.Error("list runners", "err", err.Error())
			wait = 10 * time.Second
			continue
		}
		l.sweep(ctx, l.client, runners, started)
	}
}

// sweep takes in one listing of the group's runners, begun at started: each set learns its machines' runners, and
// leaked probes and stale runners of a set are removed. A set's runners are named <set>-<instance id>; nothing else is
// any set's, so an identity's runner (superci-id-<size>-<n>, registered once and kept) is never a machine's runner, an
// orphan, or removed.
func (l *Listener) sweep(ctx context.Context, c registrar, runners map[string]ListedRunner, started time.Time) {
	if n := sweepProbes(ctx, c, runners, started, probeGrace); n > 0 {
		l.mu.Lock()
		l.leaked += n
		l.mu.Unlock()
	}
	for _, s := range l.sets {
		own := map[string]ListedRunner{}
		for name, r := range runners {
			if strings.HasPrefix(name, s.spec.Name+"-i-") {
				own[name] = r
			}
		}
		var stale []ListedRunner
		for _, o := range s.state.Listed(own, started) {
			if !o.Busy {
				stale = append(stale, o)
			}
		}
		parallel(stale, func(o ListedRunner) {
			err := c.RemoveRunner(ctx, o.ID)
			slog.Info("remove stale runner", "set", s.spec.Name, "runner", o.Name, "err", errString(err))
		})
	}
}

// reconciler is the level-triggered loop: it lists EC2, adopts what it finds, ends stuck, overdue and shed machines,
// retires surplus idle runners and removes the retiring ones (fenced by GitHub's refusal to remove a runner that
// holds a job), ends machines of no configured set, accrues the spend and applies its cap. Its calls to GitHub run on
// their own, so a slow GitHub never stalls a pass; each machine has at most one of them in flight.
func (l *Listener) reconciler(ctx context.Context, refresh func(context.Context, string)) {
	var logged time.Time
	for tick := time.NewTicker(10 * time.Second); ; {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		now := time.Now()
		for _, r := range l.fleet.regions {
			refresh(ctx, r.Name)
		}
		cfg := l.config()
		capped := l.spend.Over()
		var running []Machine
		var end []*Machine
		// Machines launched for registrations GitHub no longer leaves us wait paid for: past twice what the budget
		// serves in 90 s at its admitted rate, the excess ends (it holds no runner).
		limit, waiting := max(10, int(l.budget.Rate()*90)), 0
		for _, s := range l.sets {
			waiting += s.state.Unregistered()
		}
		if excess := waiting - limit; waiting > 2*limit {
			for _, s := range l.sets {
				shed := s.state.Shed(now, excess)
				excess -= len(shed)
				end = append(end, shed...)
			}
			slog.Warn("shed waiting machines", "waiting", waiting, "limit", limit, "shed", waiting-limit-excess)
			tlRetiredAll(end, "shed", now)
		}
		window := cfg.Pool.window()
		for _, s := range l.sets {
			s.state.SetCapped(capped)
			if stale := s.state.MarkStale(now, 3*time.Minute); stale > 0 {
				slog.Warn("stale waiting jobs", "set", s.spec.Name, "count", stale)
			}
			ended, unlisted := s.state.Stuck(now, 5*time.Minute, 3*time.Minute)
			for _, m := range ended {
				slog.Warn("stuck", "set", s.spec.Name, "machine", m.ID, "region", m.Region, "age_s", int(now.Sub(m.Launched).Seconds()))
				tlRetired(s.spec.Name, m, "stuck", now)
			}
			go l.check(ctx, s, unlisted)
			for _, m := range s.state.Overdue(now, cfg.job(), l.maxAge(), cfg.life()) {
				slog.Warn("overdue", "set", s.spec.Name, "machine", m.ID, "age_s", int(now.Sub(m.Launched).Seconds()))
				tlRetired(s.spec.Name, m, "overdue", now)
			}
			if s.fleet != nil {
				for _, m := range s.state.Recycle(now, s.fleet.Versions(), recycleBatch) {
					tlRetired(s.spec.Name, m, "recycle", now)
				}
			}
			s.state.Surplus(now, 20*time.Second, window, 100)
			go parallel(s.state.ToFence(now, 100), func(m *Machine) {
				r := m.Runner
				err := l.client.RemoveRunner(ctx, r.ID)
				out := removal(err)
				s.state.Fenced(m, out, time.Now())
				slog.Info("retire", "set", s.spec.Name, "machine", m.ID, "runner", r.Name, "removed", out == Removed, "held_job", out == HeldJob, "err", errString(err))
				tlRetired(s.spec.Name, m, "retire", time.Now())
			})
			end = append(end, s.state.ToEnd(now)...)
			running = append(running, s.state.Running()...)
			for _, j := range s.state.TakeLost() {
				l.rerun.Add(j, now, true)
			}
			go parallel(s.state.Untagged(), func(m *Machine) {
				if err := l.tagger.Issue(ctx, m); err != nil {
					s.state.TagFailed(m)
					slog.Error("issue tag", "set", s.spec.Name, "machine", m.ID, "err", err.Error())
				}
			})
		}
		for _, d := range l.dsets {
			d.SetCapped(capped)
			ended, lost := d.Reconcile(now, dispatchBounds{maxAge: l.maxAge(), life: cfg.life(), job: cfg.job(), warmup: window, window: resumeWindow})
			end = append(end, ended...)
			for _, j := range lost {
				l.rerun.Add(j, now, false)
			}
			running = append(running, d.Running()...)
		}
		end = append(end, l.endStrays(now, cfg.life())...)
		if len(end) > 0 {
			tctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			l.fleet.Terminate(tctx, end, "ending")
			cancel()
		}
		l.spend.Accrue(now, running)
		l.mu.Lock()
		l.reconciled = now
		l.mu.Unlock()
		if now.Sub(logged) >= 30*time.Second {
			logged = now
			b := l.budget.Snapshot(now)
			sp := l.spend.Snapshot()
			slog.Info("budget", "known", b.Known, "org_rate", b.OrgRate, "others_rate", b.OthersRate, "ours_rate", b.OursRate,
				"spare_rate", b.SpareRate, "window_use", b.WindowUse, "pending", b.Pending, "made", b.Made, "refused", b.Refused,
				"failed", b.Failed, "throttled", b.Throttled, "rate", b.Rate, "queued", b.Queued, "denied_tokens", b.Denied["tokens"],
				"denied_throttled", b.Denied["throttled"], "denied_queued", b.Denied["queued"], "spend_day", sp.DayUSD, "spend_month", sp.MonthUSD, "per_hour", sp.PerHour, "capped", sp.Capped)
			for _, s := range l.sets {
				snap := s.state.Snapshot(now)
				slog.Info("state", "set", snap.Set, "ready", snap.Ready, "desired", snap.Desired, "live", snap.Live, "inflight", snap.Inflight, "machines", snap.Machines,
					"booting", snap.Booting, "idle", snap.Idle, "busy", snap.Busy, "issued", snap.Issued, "retiring", snap.Retiring, "ending", snap.Ending,
					"on_demand", snap.OnDemand, "notices", snap.Notices, "pool", snap.Pool, "orphans", snap.Orphans, "stale_waiting", snap.StaleWaiting,
					"s_assigned", snap.Stats.TotalAssignedJobs, "s_available", snap.Stats.TotalAvailableJobs, "s_running", snap.Stats.TotalRunningJobs,
					"launch_s", snap.LaunchS, "launches", snap.Launches, "oldest_job_s", snap.OldestJobS, "oldest_unregistered_s", snap.OldestUnregisteredS)
				tlPoolTarget(snap, now)
			}
			for _, d := range l.dsets {
				snap := d.Snapshot(now)
				slog.Info("dispatch state", "set", snap.Set, "ready", snap.Ready, "desired", snap.Desired, "live", snap.Live, "inflight", snap.Inflight,
					"machines", snap.Machines, "booting", snap.Booting, "idle", snap.Idle, "running", snap.Running, "ending", snap.Ending,
					"jobs_waiting", snap.JobsWaiting, "pool", snap.Pool, "oldest_job_s", snap.OldestJobS, "launch_s", snap.LaunchS)
			}
		}
	}
}

// check asks GitHub about each runner a listing did not show, at most 100 a pass (the rest wait for the next): by id,
// or by the machine's runner name when a restart left the runner unknown. GitHub's word that it has no such runner
// ends the machine; anything else keeps it.
func (l *Listener) check(ctx context.Context, s *Set, ms []*Machine) {
	for _, m := range ms[min(len(ms), 100):] {
		s.state.Checked(m, Unsure, nil)
	}
	parallel(ms[:min(len(ms), 100)], func(m *Machine) {
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		c, found, err := Unsure, (*Runner)(nil), error(nil)
		id, name := s.state.RunnerRef(m)
		if id != 0 {
			_, err = l.client.GetRunner(cctx, int(id))
			switch {
			case err == nil:
				c = Present
			case errors.Is(err, scaleset.RunnerNotFoundError), errors.Is(err, scaleset.NotFoundError):
				c = Absent
			}
		} else {
			var ref *scaleset.RunnerReference
			ref, err = l.client.GetRunnerByName(cctx, name)
			switch {
			case err == nil && ref == nil:
				c = Absent
			case err == nil:
				c, found = Present, &Runner{ID: int64(ref.ID), Name: ref.Name, Issued: time.Now()}
			}
		}
		s.state.Checked(m, c, found)
		switch c {
		case Absent:
			slog.Warn("stuck", "set", s.spec.Name, "machine", m.ID, "runner", name, "why", "runner gone")
		case Unsure:
			slog.Info("runner check", "set", s.spec.Name, "runner", name, "err", errString(err))
		}
	})
}

// parallel calls f on every item, 16 at a time, and returns when all are done.
func parallel[T any](items []T, f func(T)) {
	sem := make(chan struct{}, 16)
	var wg sync.WaitGroup
	for _, it := range items {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			f(it)
		}()
	}
	wg.Wait()
}

// sampleOrg learns the organization's highest runner id when the budget asks for one (SampleDue). It registers a probe
// runner and removes it at once: runner ids are one sequence for the organization, so the probe's id is the head,
// exact, for two calls to the Actions service. The probe is a registration like any other, inside the budget and
// counted as ours. It never runs (no process holds it), so GitHub gives it no job, and one whose removal failed is an
// orphan the lister removes. A listing of every organization runner is the fallback when the budget has no token or
// the probe fails: 44 pages at 4,300 runners, so at one every 100 s about 1,600 of the App's 5,000 requests an hour,
// which every listener of the App shares. A listing's head is dated when it began, so two samples 61 s apart span
// 61 s however long each took.
func (l *Listener) sampleOrg(ctx context.Context) {
	began := time.Now()
	if !l.budget.SampleDue(began) || l.probeHead(ctx, began) {
		return
	}
	id, err := l.rest.OrgHead(ctx)
	if err != nil {
		l.budget.SampleFailed(time.Now())
		slog.Error("org head", "err", err.Error())
		return
	}
	l.budget.Head(began, id)
	slog.Info("org head", "source", "listing", "id", id, "ms", time.Since(began).Milliseconds())
}

// price refreshes the spot prices the spend accrues at.
func (l *Listener) price(ctx context.Context) {
	names := l.priceTypes()
	for _, r := range l.fleet.regions {
		p, err := l.fleet.SpotPrices(ctx, r, names)
		if err != nil {
			slog.Error("spot prices", "region", r.Name, "err", err.Error())
			continue
		}
		l.spend.SetPrices(p)
	}
}

// specs is every set's spec, scale sets and dispatch sets.
func (l *Listener) specs() []SetSpec {
	var out []SetSpec
	for _, s := range l.sets {
		out = append(out, s.spec)
	}
	for _, d := range l.dsets {
		out = append(out, d.spec)
	}
	return out
}

// priceTypes is every type a set launches, dispatch sets included: a type with no spot price accrues at the on-demand
// rate ($1.24 an hour for 8 vCPUs against m8azn.3xlarge's $0.51 spot), which at superci-8cpu's 250 machines would reach
// the daily cap in about 3 hours of what is really 7.
func (l *Listener) priceTypes() []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range l.specs() {
		for _, t := range s.Types {
			if !seen[t] {
				seen[t] = true
				out = append(out, t)
			}
		}
	}
	return out
}

// probePrefix names the probe runners: the time of the registration, in nanoseconds, follows it.
const probePrefix = "superci-probe-"

// probeGrace is how long a probe runner may stay registered before the lister takes it for a leak: the probe removes
// itself within seconds, so one this old failed to.
const probeGrace = 3 * time.Minute

// registrar is the part of the scale set client a probe uses.
type registrar interface {
	GenerateJitRunnerConfig(ctx context.Context, setting *scaleset.RunnerScaleSetJitRunnerSetting, scaleSetID int) (*scaleset.RunnerScaleSetJitRunnerConfig, error)
	RemoveRunner(ctx context.Context, runnerID int64) error
}

// probeHead learns the organization's head from a probe runner of the first set it serves; it says whether it did.
func (l *Listener) probeHead(ctx context.Context, now time.Time) bool {
	for _, s := range l.sets {
		if !s.drain {
			return probeOrgHead(ctx, l.client, l.budget, s.id, now)
		}
	}
	return false
}

// probeOrgHead registers a probe runner in the scale set, takes its id as the head and removes it. The removal has a
// context of its own, so a slow registration or a shutdown does not leave the probe registered (a leak the lister
// would sweep, but a registration it counts against the organization's cap until then).
func probeOrgHead(ctx context.Context, c registrar, b *Budget, setID int, now time.Time) bool {
	if !b.Take(now, ForPool) {
		return false
	}
	gctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	cfg, err := c.GenerateJitRunnerConfig(gctx, &scaleset.RunnerScaleSetJitRunnerSetting{Name: fmt.Sprintf("%s%d", probePrefix, now.UnixNano())}, setID)
	cancel()
	if err != nil {
		outcome := Failed
		if refusedRegistration(err) {
			outcome = Refused
		}
		b.Done(time.Now(), outcome, 0)
		slog.Warn("org probe", "err", err.Error())
		return false
	}
	b.Done(time.Now(), Made, int64(cfg.Runner.ID))
	slog.Info("org head", "source", "probe", "id", cfg.Runner.ID)
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := c.RemoveRunner(rctx, int64(cfg.Runner.ID)); err != nil {
		slog.Warn("org probe left a runner", "runner_id", cfg.Runner.ID, "err", err.Error())
	}
	return true
}

// leakedProbes picks the probe runners of a listing of the deployment's group that were registered more than grace
// before now and hold no job. A probe's name carries its registration time; a name that does not parse is none of ours.
func leakedProbes(runners map[string]ListedRunner, now time.Time, grace time.Duration) []ListedRunner {
	var out []ListedRunner
	for name, r := range runners {
		ns, err := strconv.ParseInt(strings.TrimPrefix(name, probePrefix), 10, 64)
		if !strings.HasPrefix(name, probePrefix) || err != nil || r.Busy {
			continue
		}
		if now.Sub(time.Unix(0, ns)) > grace {
			out = append(out, r)
		}
	}
	return out
}

// sweepProbes removes the leaked probe runners of a listing and says how many it found.
func sweepProbes(ctx context.Context, c registrar, runners map[string]ListedRunner, now time.Time, grace time.Duration) int {
	leaked := leakedProbes(runners, now, grace)
	parallel(leaked, func(o ListedRunner) {
		err := c.RemoveRunner(ctx, o.ID)
		slog.Warn("remove leaked probe runner", "runner", o.Name, "err", errString(err))
	})
	return len(leaked)
}

// setPools sets each set's hot pool: the operator's pin, or the burst the set's recent jobs ask for within the hour's
// floor and cap.
func (l *Listener) setPools(now time.Time) {
	cfg := l.config()
	l.mu.Lock()
	pins := maps(l.pins)
	l.mu.Unlock()
	for _, s := range l.sets {
		n, pinned := pins[s.spec.Name]
		if !pinned {
			n = cfg.PoolTarget(s.spec, now, s.state.Burst(now, cfg.Pool))
		}
		s.state.SetPool(n)
	}
	for _, d := range l.dsets {
		n, pinned := pins[d.spec.Name]
		if !pinned {
			n = cfg.PoolTarget(d.spec, now, d.Burst(now, cfg.Pool))
		}
		d.SetPool(n)
	}
}

func maps(m map[string]int) map[string]int {
	out := make(map[string]int, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func (l *Listener) config() Config {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.cfg
}

func (l *Listener) maxAge() time.Duration { return l.config().maxAge() }

func modTime(path string) time.Time {
	if fi, err := os.Stat(path); err == nil {
		return fi.ModTime()
	}
	return time.Time{}
}

// watchConfig rereads the override file when it changes, and applies what can change at run time: pools, caps, the
// registration budget, the on-demand share, job and machine lifetimes, and each set's most machines. A change clears
// the operator's pins.
func (l *Listener) watchConfig() {
	mod := modTime(l.path)
	l.mu.Lock()
	same := mod.Equal(l.modified)
	l.mu.Unlock()
	if same {
		return
	}
	cfg, err := LoadConfig(l.path)
	if err != nil {
		slog.Error("config", "path", l.path, "err", err.Error())
		return
	}
	l.mu.Lock()
	l.cfg, l.modified, l.pins = cfg, mod, map[string]int{}
	l.mu.Unlock()
	l.budget.SetConfig(cfg.Registration)
	l.spend.SetCaps(cfg.DailyUSD, cfg.MonthlyUSD)
	maxes := map[string]int{}
	for _, spec := range cfg.Sets() {
		maxes[spec.Name] = spec.Max
	}
	for _, s := range l.sets {
		s.state.SetBatch(cfg.Batch)
		s.state.SetNoDemand(cfg.NoDemand)
		if n, ok := maxes[s.spec.Name]; ok {
			s.state.SetMax(n)
		}
	}
	for _, d := range l.dsets {
		d.SetNoDemand(cfg.NoDemand)
		if n, ok := maxes[d.spec.Name]; ok {
			d.SetMax(n)
		}
	}
	l.setPools(time.Now())
	slog.Info("config reloaded", "path", l.path)
}

func (l *Listener) override(set string, pool, maxN int) {
	setMax := map[string]func(int){}
	for _, s := range l.sets {
		setMax[s.spec.Name] = s.state.SetMax
	}
	for _, d := range l.dsets {
		setMax[d.spec.Name] = d.SetMax
	}
	for name, setMaxOf := range setMax {
		if set != "" && name != set {
			continue
		}
		l.mu.Lock()
		if pool >= 0 {
			l.pins[name] = pool
		} else {
			delete(l.pins, name)
		}
		l.mu.Unlock()
		if maxN >= 0 {
			setMaxOf(maxN)
		}
	}
	l.setPools(time.Now())
}

func (l *Listener) status() Status {
	now := time.Now()
	st := Status{Budget: l.budget.Snapshot(now), Spend: l.spend.Snapshot()}
	for _, s := range l.sets {
		st.Sets = append(st.Sets, s.state.Snapshot(now))
	}
	for _, d := range l.dsets {
		st.Dispatch = append(st.Dispatch, d.Snapshot(now))
	}
	return st
}

// identityCountEvery is how often the identities group is listed to count a size's identities: one GitHub call a page.
// A count older than identityCountFresh (three missed listings) is not published, so an alarm reads no number rather than a stale one.
const (
	identityCountEvery = 5 * time.Minute
	identityCountFresh = 3 * identityCountEvery
)

// countIdentities lists the identities group once and keeps what each dispatch size has online and busy.
func (l *Listener) countIdentities(ctx context.Context, ids *Identities) {
	lctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	_, runners, err := ids.Listing(lctx)
	if err != nil {
		slog.Error("count identities", "err", err.Error())
		return
	}
	labels := map[string]string{}
	for _, d := range l.dsets {
		labels[d.spec.Name] = d.spec.Label
	}
	counts := CountIdentities(runners, labels)
	l.mu.Lock()
	l.identities, l.counted = counts, time.Now()
	l.mu.Unlock()
}

func (l *Listener) metrics() Metrics {
	now := time.Now()
	st := l.status()
	m := Metrics{Budget: st.Budget, Spend: st.Spend, Sets: st.Sets, SetWaits: map[string][]float64{}, Dispatch: st.Dispatch,
		DispatchCounts: map[string]DispatchCounts{}}
	cfg := l.config()
	m.OnDemandFirst = onDemandFirstSets(l.specs())
	for i, s := range l.sets {
		waits := s.state.TakeWaits()
		m.Jobs = append(m.Jobs, s.state.TakeJobs()...)
		m.Waits = append(m.Waits, waits...)
		m.SetWaits[s.spec.Name] = waits
		if s.state.Stalled(now, 10*time.Minute) {
			m.Stalled++
		}
		snap := st.Sets[i]
		waiting := snap.Stats.TotalAssignedJobs - snap.Stats.TotalRunningJobs + snap.Stats.TotalAvailableJobs
		if floor, _ := cfg.PoolBounds(s.spec, now); !s.drain && snap.Pool <= floor && waiting > snap.Idle {
			m.AtFloor++
		}
	}
	for _, s := range st.Sets {
		m.Orphans += s.Orphans
	}
	for _, d := range l.dsets {
		m.Jobs = append(m.Jobs, d.TakeJobs()...)
		waits := d.TakeWaits()
		m.Waits = append(m.Waits, waits...)
		m.SetWaits[d.spec.Name] = waits
		m.DispatchCounts[d.spec.Name] = d.TakeCounts()
	}
	for _, d := range st.Dispatch {
		if d.OldestJobS > (10 * time.Minute).Seconds() {
			m.Stalled++
		}
	}
	l.mu.Lock()
	for _, x := range l.strays {
		if now.Sub(x.Launched) > 15*time.Minute {
			m.Orphans++
		}
	}
	m.Short, l.short = l.short, 0
	m.Orphans, l.leaked = m.Orphans+l.leaked, 0
	l.mu.Unlock()
	m.Identities = l.identityCounts(now)
	return m
}

// identityCounts is the last count of the identities, or none when it is older than identityCountFresh.
func (l *Listener) identityCounts(now time.Time) []IdentityCount {
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.counted) >= identityCountFresh {
		return nil
	}
	return slices.Clone(l.identities)
}

// healthy says whether the process is alive: its reconciler has finished a pass within the last two minutes. GitHub's
// and EC2's slowness is not death (a replacement could not reach them either); a pass bounds its own EC2 calls, and its
// GitHub calls run on their own. A pass can spend 20 s describing each region one after the other and 20 s ending
// machines, so a minute is reached by a slow EC2 alone.
func (l *Listener) healthy() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if d := time.Since(l.reconciled); d > healthyWithin {
		return fmt.Errorf("reconciler idle for %s", d.Round(time.Second))
	}
	return nil
}

// dispatchMachine is the dispatch set and machine an instance is, if it is one.
func (l *Listener) dispatchMachine(id string) (string, *Machine) {
	for _, d := range l.dsets {
		if m := d.machine(id); m != nil {
			return d.spec.Name, m.Machine
		}
	}
	return "", nil
}
