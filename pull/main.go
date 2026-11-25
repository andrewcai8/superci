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
	"github.com/aws/aws-sdk-go-v2/service/ssm"
)

//go:embed boot.sh
var bootScript string

func main() {
	var (
		configPath = flag.String("config", "/etc/superci-pull/config.json", "override file, decoded over the built-in config and reread when it changes")
		publicIP   = flag.String("public-ip", "", "the listener's stable address (its Elastic IP), which machines reach")
		groups     = flag.String("security-groups", "", "runner security group of each region: us-east-1=sg-…,us-east-2=sg-…")
		listen     = flag.String("listen", ":8443", "address of the machines' endpoint")
		admin      = flag.String("admin", "127.0.0.1:8080", "address of the operator's endpoint")
		tlsParam   = flag.String("tls-param", "/superci-pull/tls", "SSM SecureString (us-east-1) holding the listener's TLS key")
		spendParam = flag.String("spend-param", "/superci-pull/spend", "SSM parameter (us-east-1) holding today's and this month's spend")
		noMetrics  = flag.Bool("no-metrics", false, "publish no CloudWatch metrics")
		deleteSets = flag.Bool("delete-scale-sets", false, "delete every configured scale set and exit")
	)
	flag.Parse()
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err := run(ctx, flags{*configPath, *publicIP, pairs(*groups), *listen, *admin, *tlsParam, *spendParam, *noMetrics, *deleteSets})
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
	fleet      *Fleet
	budget     *Budget
	spend      *Spend
	rest       *REST
	client     *scaleset.Client
	rerun      *Rerunner
	reconciled time.Time
	strays     map[string]*Machine // machines of no configured set, by id
}

const group = 1 // the organization's Default runner group, which every repository may use

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
	if f.deleteSets {
		for _, spec := range specs {
			s, err := client.GetRunnerScaleSet(ctx, group, spec.Name)
			if err != nil || s == nil {
				slog.Info("no scale set to delete", "name", spec.Name, "err", errString(err))
				continue
			}
			slog.Info("deleting scale set", "name", spec.Name, "id", s.ID, "err", errString(client.DeleteRunnerScaleSet(ctx, s.ID)))
		}
		return nil
	}
	awsCfg, err := config.LoadDefaultConfig(ctx, config.WithRegion("us-east-1"))
	if err != nil {
		return err
	}
	ssmc := ssm.NewFromConfig(awsCfg)
	id, err := LoadIdentity(ctx, ssmc, f.tlsParam)
	if err != nil {
		return fmt.Errorf("tls identity: %w", err)
	}
	verifier, err := NewVerifier(cfg.Account)
	if err != nil {
		return err
	}
	fleet, err := NewFleet(ctx, cfg.Regions, ImageConfig{Owner: "135269210855", Name: "runs-on-v2.2-ubuntu24-full-x64-*", Pin: "20261102081424"})
	if err != nil {
		return err
	}
	userData := bootData(cfg, fmt.Sprintf("https://%s%s", f.publicIP, f.listen), id.Pin)
	now := time.Now()
	l := &Listener{path: f.configPath, cfg: cfg, modified: modTime(f.configPath), pins: map[string]int{}, fleet: fleet,
		budget: NewBudget(cfg.Registration, now), spend: NewSpend(cfg.DailyUSD, cfg.MonthlyUSD, specs, now),
		rest: NewREST(app, cfg.Installation, cfg.Org), client: client, strays: map[string]*Machine{}, reconciled: now}
	l.rerun = NewRerunner(l.rest)
	if err := l.spend.Load(ctx, ssmc, f.spendParam); err != nil {
		slog.Error("load spend", "err", err.Error())
	}
	for _, spec := range specs {
		gid, err := ensureScaleSet(ctx, client, spec)
		if err != nil {
			return fmt.Errorf("scale set %s: %w", spec.Name, err)
		}
		sf, err := fleet.AddSet(ctx, spec, userData, f.groups)
		if err != nil {
			return err
		}
		l.sets = append(l.sets, &Set{spec: spec, id: gid, state: NewState(spec.Name, 0, spec.Max), fleet: sf})
	}
	if err := pruneScaleSets(ctx, client, specs); err != nil {
		return fmt.Errorf("prune scale sets: %w", err)
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
	srv := &Server{sets: byName, budget: l.budget, verifier: verifier, client: client, refresh: refresh,
		slots: make(chan struct{}, max(1, cfg.Registration.Concurrency)), maxAge: l.maxAge}
	go serve(ctx, &http.Server{Addr: f.listen, Handler: srv, ReadHeaderTimeout: 10 * time.Second}, &id)
	go serve(ctx, &http.Server{Addr: f.admin, Handler: Admin(l.status, l.healthy, l.override)}, nil)
	errs := make(chan error, len(l.sets))
	var sessions sync.WaitGroup
	for _, s := range l.sets {
		wake := make(chan struct{}, 1)
		go l.launcher(ctx, s, wake)
		sessions.Add(1)
		go func() { defer sessions.Done(); errs <- listenSet(ctx, client, s, wake) }()
	}
	go l.reconciler(ctx, refresh)
	go every(ctx, 15*time.Second, func() { l.setPools(time.Now()) })
	go every(ctx, 10*time.Second, func() { l.sampleOrg(ctx) })
	go every(ctx, 30*time.Minute, func() { l.price(ctx) })
	go every(ctx, time.Minute, func() {
		if err := l.spend.Save(ctx, ssmc, f.spendParam); err != nil {
			slog.Error("save spend", "err", err.Error())
		}
	})
	go every(ctx, 30*time.Second, l.watchConfig)
	if !f.noMetrics {
		cw := cloudwatch.NewFromConfig(awsCfg)
		var made, refused, failed int
		go every(ctx, time.Minute, func() {
			m := l.metrics()
			m.Regs, m.Refused, m.Failed = m.Budget.Made-made, m.Budget.Refused-refused, m.Budget.Failed-failed
			made, refused, failed = m.Budget.Made, m.Budget.Refused, m.Budget.Failed
			publish(ctx, cw, m, time.Now())
		})
	}
	if l.cfg.Rerun {
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

// bootData is a machine's user data: boot.sh with the listener's address and key, the longest a job may run, and the
// longest a machine may live.
func bootData(c Config, listener, pin string) string {
	job := (c.JobMinutes + c.MarginMin) * 60
	life := int(c.MaxAgeHours*3600) + job
	return strings.NewReplacer("@LISTENER@", listener, "@PIN@", pin, "@JOB_SECONDS@", strconv.Itoa(job),
		"@LIFE_SECONDS@", strconv.Itoa(life)).Replace(bootScript)
}

// ensureScaleSet finds or makes the set at GitHub with its labels: its name, and the size's label when it is not the
// size's first set.
func ensureScaleSet(ctx context.Context, client *scaleset.Client, spec SetSpec) (int, error) {
	labels := []scaleset.Label{{Name: spec.Name}}
	if spec.Name != spec.Label {
		labels = append(labels, scaleset.Label{Name: spec.Label})
	}
	s, err := client.GetRunnerScaleSet(ctx, group, spec.Name)
	if err != nil {
		return 0, err
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

// pruneScaleSets deletes the sets of ours the config no longer has (a size removed, or a split made smaller): one
// left at GitHub would keep taking a share of its label's jobs with no listener to run them. GitHub removes a
// deleted set's runners, so a job one of them held fails; shrink a split while the size is idle, or re-run them.
func pruneScaleSets(ctx context.Context, client *scaleset.Client, specs []SetSpec) error {
	want := map[string]bool{}
	for _, s := range specs {
		want[s.Name] = true
	}
	sets, err := client.ListRunnerScaleSets(ctx, group)
	if err != nil {
		return err
	}
	for _, s := range sets {
		if ownSetName.MatchString(s.Name) && !want[s.Name] {
			slog.Warn("deleting scale set the config no longer has", "name", s.Name, "id", s.ID, "err", errString(client.DeleteRunnerScaleSet(ctx, s.ID)))
		}
	}
	return nil
}

func labelNames(ls []scaleset.Label) []string {
	out := make([]string, 0, len(ls))
	for _, l := range ls {
		out = append(out, l.Name)
	}
	sort.Strings(out)
	return out
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
	if msg != nil && len(msg.JobAvailableMessages) > 0 {
		ids := make([]int64, 0, len(msg.JobAvailableMessages))
		for _, j := range msg.JobAvailableMessages {
			ids = append(ids, j.RunnerRequestID)
		}
		got, err := s.session.AcquireJobs(ctx, ids)
		if err != nil {
			return fmt.Errorf("acquire jobs: %w", err)
		}
		slog.Info("acquired", "set", s.set.spec.Name, "asked", len(ids), "got", len(got))
	}
	s.set.state.Observe(msg, time.Now())
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
		limit := max(10, int(l.budget.Estimate(now).SpareRate*90))
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
		go func() {
			defer func() { <-slots }()
			now := time.Now()
			got := s.fleet.Launch(ctx, round, n, s.state.OnDemandRoom(l.config().OnDemand, n, now))
			s.state.Launched(round, got, now)
			slog.Info("launched", "set", s.spec.Name, "round", round, "asked", n, "got", len(got), "ms", time.Since(now).Milliseconds())
			if len(got) < n {
				time.Sleep(5 * time.Second)
			}
		}()
	}
}

// refresher lists a region's machines of every set again, at most once a second per region however many ask, and
// gives each set its own. Machines of no configured set are kept as strays.
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
		ms, err := l.fleet.Describe(ctx, r)
		if err != nil {
			slog.Error("describe", "region", region, "err", err.Error())
			return
		}
		now := time.Now()
		by := map[string][]*Machine{}
		for _, m := range ms {
			by[m.Set] = append(by[m.Set], m)
		}
		known := map[string]bool{}
		for _, s := range l.sets {
			known[s.spec.Name] = true
			for _, j := range s.state.Seen(region, by[s.spec.Name], now) {
				l.rerun.Add(j, now)
			}
		}
		l.mu.Lock()
		for id, m := range l.strays {
			if m.Region == region {
				delete(l.strays, id)
			}
		}
		for _, m := range ms {
			if !known[m.Set] {
				l.strays[m.ID] = m
			}
		}
		l.mu.Unlock()
		mu.Lock()
		last[region] = time.Now()
		mu.Unlock()
	}
}

// reconciler is the level-triggered loop: it lists EC2 and GitHub, adopts what it finds, removes stale runners, ends
// stuck machines and surplus idle runners (fenced by GitHub's refusal to remove a runner that holds a job), ends
// machines of no configured set, accrues the spend and applies its cap.
func (l *Listener) reconciler(ctx context.Context, refresh func(context.Context, string)) {
	var listed, logged time.Time
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
		if now.Sub(listed) > 30*time.Second {
			if runners, err := l.rest.Runners(ctx, group, "superci-"); err != nil {
				slog.Error("list runners", "err", err.Error())
			} else {
				listed = now
				for _, s := range l.sets {
					own := map[string]ListedRunner{}
					for name, r := range runners {
						if strings.HasPrefix(name, s.spec.Name+"-i-") {
							own[name] = r
						}
					}
					for _, o := range s.state.Listed(own, now) {
						if !o.Busy {
							err := l.client.RemoveRunner(ctx, o.ID)
							slog.Info("remove stale runner", "set", s.spec.Name, "runner", o.Name, "err", errString(err))
						}
					}
				}
			}
		}
		capped := l.spend.Over()
		var running []Machine
		var end []*Machine
		// Machines launched for registrations the organization no longer leaves us wait paid for: past twice what the
		// budget serves in 90 s (once the organization's rate is measured), the excess ends (it holds no runner).
		est := l.budget.Estimate(now)
		limit, waiting := max(10, int(est.SpareRate*90)), 0
		for _, s := range l.sets {
			waiting += s.state.Unregistered()
		}
		if excess := waiting - limit; est.Known && waiting > 2*limit {
			for _, s := range l.sets {
				shed := s.state.Shed(now, excess)
				excess -= len(shed)
				end = append(end, shed...)
			}
			slog.Warn("shed waiting machines", "waiting", waiting, "limit", limit, "shed", waiting-limit-excess)
		}
		for _, s := range l.sets {
			s.state.SetCapped(capped)
			if stale := s.state.MarkStale(now, 3*time.Minute); stale > 0 {
				slog.Warn("stale waiting jobs", "set", s.spec.Name, "count", stale)
			}
			for _, m := range s.state.Stuck(now, 5*time.Minute, 3*time.Minute) {
				slog.Warn("stuck", "set", s.spec.Name, "machine", m.ID, "region", m.Region, "age_s", int(now.Sub(m.Launched).Seconds()))
			}
			for _, m := range s.state.Surplus(now, 20*time.Second, 100) {
				err := l.client.RemoveRunner(ctx, m.Runner.ID)
				removed := err == nil || errors.Is(err, scaleset.RunnerNotFoundError) || errors.Is(err, scaleset.NotFoundError)
				s.state.Removed(m, removed)
				slog.Info("surplus", "set", s.spec.Name, "machine", m.ID, "runner", m.Runner.Name, "removed", removed, "err", errString(err))
			}
			end = append(end, s.state.ToEnd(now)...)
			running = append(running, s.state.Running()...)
			for _, j := range s.state.TakeLost() {
				l.rerun.Add(j, now)
			}
		}
		l.mu.Lock()
		for _, m := range l.strays {
			if now.Sub(m.Launched) > 10*time.Minute && now.Sub(m.Ended) > time.Minute {
				m.Ended = now
				end = append(end, m)
			}
		}
		l.mu.Unlock()
		if len(end) > 0 {
			l.fleet.Terminate(ctx, end, "ending")
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
				"failed", b.Failed, "throttled", b.Throttled, "spend_day", sp.DayUSD, "spend_month", sp.MonthUSD, "per_hour", sp.PerHour, "capped", sp.Capped)
			for _, s := range l.sets {
				snap := s.state.Snapshot(now)
				slog.Info("state", "set", snap.Set, "desired", snap.Desired, "live", snap.Live, "inflight", snap.Inflight, "machines", snap.Machines,
					"booting", snap.Booting, "idle", snap.Idle, "busy", snap.Busy, "ending", snap.Ending, "on_demand", snap.OnDemand,
					"notices", snap.Notices, "pool", snap.Pool, "orphans", snap.Orphans, "stale_waiting", snap.StaleWaiting,
					"s_assigned", snap.Stats.TotalAssignedJobs, "s_available", snap.Stats.TotalAvailableJobs, "s_running", snap.Stats.TotalRunningJobs)
			}
		}
	}
}

// sampleOrg learns the organization's highest runner id from a listing when our own registrations have not told it
// lately. A listing reads every page of the organization's runners (2,600 at a busy hour, 27 requests of the App's
// 5,000 an hour), so it runs at most every 100 s.
func (l *Listener) sampleOrg(ctx context.Context) {
	if time.Since(l.budget.LastHead()) < 100*time.Second {
		return
	}
	id, err := l.rest.OrgHead(ctx)
	if err != nil {
		slog.Error("org head", "err", err.Error())
		return
	}
	l.budget.Head(time.Now(), id)
}

// price refreshes the spot prices the spend accrues at.
func (l *Listener) price(ctx context.Context) {
	types := map[string]bool{}
	for _, s := range l.sets {
		for _, t := range s.spec.Types {
			types[t] = true
		}
	}
	names := make([]string, 0, len(types))
	for t := range types {
		names = append(names, t)
	}
	for _, r := range l.fleet.regions {
		p, err := l.fleet.SpotPrices(ctx, r, names)
		if err != nil {
			slog.Error("spot prices", "region", r.Name, "err", err.Error())
			continue
		}
		l.spend.SetPrices(p)
	}
}

// setPools sets each set's hot pool: the operator's pin, or what the set's recent arrivals ask for within the hour's
// floor and cap.
func (l *Listener) setPools(now time.Time) {
	cfg := l.config()
	l.mu.Lock()
	pins := maps(l.pins)
	l.mu.Unlock()
	window := time.Duration(max(1, cfg.Pool.WindowMinutes)) * time.Minute
	for _, s := range l.sets {
		n, pinned := pins[s.spec.Name]
		if !pinned {
			n = cfg.PoolTarget(s.spec, now, s.state.ArrivalRate(now, window))
		}
		s.state.SetPool(n)
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

func (l *Listener) maxAge() time.Duration {
	return time.Duration(l.config().MaxAgeHours * float64(time.Hour))
}

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
		if n, ok := maxes[s.spec.Name]; ok {
			s.state.SetMax(n)
		}
	}
	l.setPools(time.Now())
	slog.Info("config reloaded", "path", l.path)
}

func (l *Listener) override(set string, pool, maxN int) {
	for _, s := range l.sets {
		if set != "" && s.spec.Name != set {
			continue
		}
		l.mu.Lock()
		if pool >= 0 {
			l.pins[s.spec.Name] = pool
		} else {
			delete(l.pins, s.spec.Name)
		}
		l.mu.Unlock()
		if maxN >= 0 {
			s.state.SetMax(maxN)
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
	return st
}

func (l *Listener) metrics() Metrics {
	now := time.Now()
	st := l.status()
	m := Metrics{Budget: st.Budget, Spend: st.Spend, Sets: st.Sets}
	for _, s := range l.sets {
		m.Waits = append(m.Waits, s.state.TakeWaits()...)
	}
	for _, s := range st.Sets {
		m.Orphans += s.Orphans
	}
	l.mu.Lock()
	for _, x := range l.strays {
		if now.Sub(x.Launched) > 15*time.Minute {
			m.Orphans++
		}
	}
	l.mu.Unlock()
	return m
}

// healthy is false when a set's message session has not answered for three minutes (its long poll answers about
// every minute) or the reconciler has not run for one.
func (l *Listener) healthy() error {
	now := time.Now()
	for _, s := range l.sets {
		if d := now.Sub(s.state.Polled()); d > 3*time.Minute {
			return fmt.Errorf("%s: no answer from its message session for %s", s.spec.Name, d.Round(time.Second))
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if d := now.Sub(l.reconciled); d > time.Minute {
		return fmt.Errorf("reconciler idle for %s", d.Round(time.Second))
	}
	return nil
}
