// superci-pull runs one GitHub runner scale set on EC2. It long-polls GitHub's queue for the set (GitHub holds every
// job until a runner takes it), converges the machines to "jobs assigned + hot pool" with batched instant EC2 Fleet
// requests, and registers a just-in-time runner for each machine when the machine asks. Its only state is EC2's tags
// and GitHub's queue; a restart rebuilds everything else.
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
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
)

//go:embed boot.sh
var bootScript string

func main() {
	var (
		org          = flag.String("org", "Authentic-Intelligence", "GitHub organization")
		installation = flag.Int64("installation", 185173804, "GitHub App installation id")
		appParam     = flag.String("app-param", "/superci-pull/github-app", "SSM parameter (us-east-1) holding the App's id and pem")
		name         = flag.String("name", "superci-pull-4cpu", "scale set name, which is the label jobs ask for")
		maxRunners   = flag.Int("max", 1200, "most machines at once")
		pool         = flag.Int("pool", 0, "idle runners kept ready beyond the queue's jobs")
		regBudget    = flag.Int("reg-budget", 1400, "most runner registrations in any 5 minutes (GitHub refuses past 1,500 per org)")
		odFraction   = flag.Float64("on-demand", 0.10, "most share of a half hour's launches that may be on-demand")
		regions      = flag.String("regions", "us-east-1=2,us-east-2=1", "regions and the weight of each")
		types        = flag.String("types", "c5.xlarge,c5a.xlarge,c6a.xlarge,c6i.xlarge,c7a.xlarge,c7i.xlarge,c7i-flex.xlarge,c8a.xlarge,c8i.xlarge,m5.xlarge,m5a.xlarge,m6a.xlarge,m6i.xlarge,m7a.xlarge,m7i.xlarge,m7i-flex.xlarge,m8a.xlarge,m8i.xlarge", "machine types, all of one size")
		groups       = flag.String("security-groups", "", "runner security group of each region: us-east-1=sg-…,us-east-2=sg-…")
		publicIP     = flag.String("public-ip", "", "this listener's public address, which machines reach")
		maxMinutes   = flag.Int("max-minutes", 120, "a machine powers off after this long whatever it is doing")
		stateDir     = flag.String("state-dir", "/var/lib/superci-pull", "where the TLS key is kept")
		listen       = flag.String("listen", ":8443", "address of the machines' endpoint")
		admin        = flag.String("admin", "127.0.0.1:8080", "address of the operator's endpoint")
		jitSlots     = flag.Int("jit-concurrency", 64, "most runner registrations in flight at once")
		deleteSet    = flag.Bool("delete-scale-set", false, "delete the scale set and exit")
	)
	flag.Parse()
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, options{*org, *installation, *appParam, *name, *maxRunners, *pool, *regBudget, *odFraction,
		pairs(*regions), strings.Split(*types, ","), pairs(*groups), *publicIP, *maxMinutes, *stateDir, *listen, *admin, *jitSlots, *deleteSet}); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("exit", "err", err.Error())
		os.Exit(1)
	}
}

type options struct {
	org          string
	installation int64
	appParam     string
	name         string
	max, pool    int
	regBudget    int
	odFraction   float64
	regions      map[string]string
	types        []string
	groups       map[string]string
	publicIP     string
	maxMinutes   int
	stateDir     string
	listen       string
	admin        string
	jitSlots     int
	deleteSet    bool
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

func run(ctx context.Context, c options) error {
	app, err := LoadApp(ctx, "us-east-1", c.appParam)
	if err != nil {
		return fmt.Errorf("app credentials: %w", err)
	}
	client, err := scaleset.NewClientWithGitHubApp(scaleset.ClientWithGitHubAppConfig{
		GitHubConfigURL: "https://github.com/" + c.org,
		GitHubAppAuth:   scaleset.GitHubAppAuth{ClientID: strconv.FormatInt(app.ID, 10), InstallationID: c.installation, PrivateKey: app.PEM},
		SystemInfo:      scaleset.SystemInfo{System: "superci-pull", Version: "0.1.0", Subsystem: "listener"},
	})
	if err != nil {
		return err
	}
	const group = 1 // the organization's Default runner group, which every repository may use
	set, err := client.GetRunnerScaleSet(ctx, group, c.name)
	if err != nil {
		return fmt.Errorf("find scale set: %w", err)
	}
	if c.deleteSet {
		if set == nil {
			slog.Info("no scale set to delete", "name", c.name)
			return nil
		}
		slog.Info("deleting scale set", "name", c.name, "id", set.ID)
		return client.DeleteRunnerScaleSet(ctx, set.ID)
	}
	if set == nil {
		set, err = client.CreateRunnerScaleSet(ctx, &scaleset.RunnerScaleSet{Name: c.name, RunnerGroupID: group,
			Labels: []scaleset.Label{{Name: c.name}}, RunnerSetting: scaleset.RunnerSetting{DisableUpdate: true}})
		if err != nil {
			return fmt.Errorf("create scale set: %w", err)
		}
		slog.Info("created scale set", "name", c.name, "id", set.ID)
	}
	client.SetSystemInfo(scaleset.SystemInfo{System: "superci-pull", Version: "0.1.0", Subsystem: "listener", ScaleSetID: set.ID})

	id, err := LoadIdentity(c.stateDir)
	if err != nil {
		return fmt.Errorf("tls identity: %w", err)
	}
	weights := map[string]int{}
	for r, w := range c.regions {
		weights[r], _ = strconv.Atoi(w)
	}
	userData := strings.NewReplacer("@LISTENER@", fmt.Sprintf("https://%s%s", c.publicIP, c.listen), "@PIN@", id.Pin,
		"@MAX_SECONDS@", strconv.Itoa(c.maxMinutes*60)).Replace(bootScript)
	fleet, err := NewFleet(ctx, FleetConfig{Set: c.name, Regions: weights, Types: c.types, ImageOwner: "135269210855",
		ImageName: "runs-on-v2.2-ubuntu24-full-x64-*", ImagePin: "20261102081424", DiskGB: 30, UserData: userData, SecurityGrp: c.groups})
	if err != nil {
		return err
	}
	state := NewState(c.name, c.pool, c.max, c.regBudget)
	rest := NewREST(app, c.installation, c.org)
	refresh := refresher(fleet, state)
	for _, r := range fleet.regions {
		refresh(ctx, r.Name)
	}

	wake := make(chan struct{}, 1)
	srv := &Server{state: state, fleet: fleet, client: client, setID: set.ID, refresh: refresh, slots: make(chan struct{}, c.jitSlots)}
	go serve(ctx, &http.Server{Addr: c.listen, Handler: srv, ReadHeaderTimeout: 10 * time.Second}, &id)
	go serve(ctx, &http.Server{Addr: c.admin, Handler: Admin(state)}, nil)
	go launcher(ctx, state, fleet, wake, c.odFraction)
	go reconciler(ctx, state, fleet, client, rest, refresh)
	return listen(ctx, client, set.ID, state, wake)
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

// listen keeps one message session open and hands each message to the scaler. A lost session (a crash of ours, a
// network fault, the service's own refusal) is opened again; GitHub redelivers what was not acknowledged.
func listen(ctx context.Context, client *scaleset.Client, setID int, state *State, wake chan struct{}) error {
	host, _ := os.Hostname()
	for wait := time.Second; ; wait = min(wait*2, 30*time.Second) {
		session, err := client.MessageSessionClient(ctx, setID, host)
		if err == nil {
			wait = time.Second
			l, lerr := listener.New(session, listener.Config{ScaleSetID: setID, MaxRunners: state.max, Logger: slog.Default().WithGroup("listener")})
			if lerr != nil {
				return lerr
			}
			err = l.Run(ctx, &scaler{state: state, session: session, wake: wake})
			_ = session.Close(context.WithoutCancel(ctx))
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		slog.Error("session", "err", err.Error(), "retry_in", wait.String())
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
}

type scaler struct {
	state   *State
	session *scaleset.MessageSessionClient
	wake    chan struct{}
}

func (s *scaler) Scale(ctx context.Context, msg *scaleset.RunnerScaleSetMessage) error {
	defer s.poke()
	if msg == nil {
		return nil
	}
	if len(msg.JobAvailableMessages) > 0 {
		ids := make([]int64, 0, len(msg.JobAvailableMessages))
		for _, j := range msg.JobAvailableMessages {
			ids = append(ids, j.RunnerRequestID)
		}
		got, err := s.session.AcquireJobs(ctx, ids)
		if err != nil {
			return fmt.Errorf("acquire jobs: %w", err)
		}
		slog.Info("acquired", "asked", len(ids), "got", len(got))
	}
	s.state.Observe(msg)
	st := msg.Statistics
	if st == nil {
		st = &scaleset.RunnerScaleSetStatistic{}
	}
	slog.Info("message", "id", msg.MessageID, "available", len(msg.JobAvailableMessages), "assigned", len(msg.JobAssignedMessages),
		"started", len(msg.JobStartedMessages), "completed", len(msg.JobCompletedMessages),
		"s_available", st.TotalAvailableJobs, "s_acquired", st.TotalAcquiredJobs, "s_assigned", st.TotalAssignedJobs,
		"s_running", st.TotalRunningJobs, "s_registered", st.TotalRegisteredRunners, "s_busy", st.TotalBusyRunners, "s_idle", st.TotalIdleRunners)
	return nil
}

func (s *scaler) poke() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// launcher turns the deficit into fleet requests. A message wakes it; it waits a moment so a burst's messages make
// one launch, and lets a few launches run at once, each reserving its capacity so none is asked for twice.
func launcher(ctx context.Context, state *State, fleet *Fleet, wake chan struct{}, odFraction float64) {
	slots := make(chan struct{}, 4)
	for {
		select {
		case <-ctx.Done():
			return
		case <-wake:
		case <-time.After(2 * time.Second):
		}
		time.Sleep(300 * time.Millisecond)
		round, n := state.Deficit()
		if n == 0 {
			continue
		}
		slots <- struct{}{}
		go func() {
			defer func() { <-slots }()
			now := time.Now()
			got := fleet.Launch(ctx, round, n, state.OnDemandRoom(odFraction, n, now))
			state.Launched(round, got, now)
			slog.Info("launched", "round", round, "asked", n, "got", len(got), "ms", time.Since(now).Milliseconds())
			if len(got) < n {
				time.Sleep(5 * time.Second)
			}
		}()
	}
}

// refresher lists a region's machines again when a machine asks before the last listing showed it, at most once a
// second per region however many ask.
func refresher(fleet *Fleet, state *State) func(context.Context, string) {
	var mu sync.Mutex
	locks := map[string]*sync.Mutex{}
	last := map[string]time.Time{}
	return func(ctx context.Context, region string) {
		mu.Lock()
		l := locks[region]
		if l == nil {
			l = &sync.Mutex{}
			locks[region] = l
		}
		mu.Unlock()
		l.Lock()
		defer l.Unlock()
		mu.Lock()
		fresh := time.Since(last[region]) < time.Second
		mu.Unlock()
		if fresh {
			return
		}
		r := fleet.Region(region)
		ms, err := fleet.Describe(ctx, r)
		if err != nil {
			slog.Error("describe", "region", region, "err", err.Error())
			return
		}
		state.Seen(region, ms, time.Now())
		mu.Lock()
		last[region] = time.Now()
		mu.Unlock()
	}
}

// reconciler is the level-triggered loop: it lists EC2 and GitHub, adopts what it finds, removes stale runners, ends
// stuck machines and surplus idle runners (fenced by GitHub's refusal to remove a runner that holds a job), and
// terminates every machine that is ending.
func reconciler(ctx context.Context, state *State, fleet *Fleet, client *scaleset.Client, rest *REST, refresh func(context.Context, string)) {
	var listed time.Time
	for tick := time.NewTicker(10 * time.Second); ; {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		now := time.Now()
		for _, r := range fleet.regions {
			refresh(ctx, r.Name)
		}
		if now.Sub(listed) > 30*time.Second {
			if runners, err := rest.Runners(ctx, 1, state.set+"-"); err != nil {
				slog.Error("list runners", "err", err.Error())
			} else {
				listed = now
				for _, o := range state.Listed(runners, now) {
					if o.Busy {
						continue
					}
					err := client.RemoveRunner(ctx, o.ID)
					slog.Info("remove stale runner", "runner", o.Name, "err", errString(err))
				}
			}
		}
		for _, m := range state.Stuck(now, 5*time.Minute, 3*time.Minute) {
			slog.Warn("stuck", "machine", m.ID, "region", m.Region, "age_s", int(now.Sub(m.Launched).Seconds()))
		}
		for _, m := range state.Surplus(now, 20*time.Second, 100) {
			err := client.RemoveRunner(ctx, m.Runner.ID)
			removed := err == nil || errors.Is(err, scaleset.RunnerNotFoundError) || errors.Is(err, scaleset.NotFoundError)
			state.Removed(m, removed)
			slog.Info("surplus", "machine", m.ID, "runner", m.Runner.Name, "removed", removed, "err", errString(err))
		}
		if end := state.ToEnd(now); len(end) > 0 {
			fleet.Terminate(ctx, end, "ending")
		}
		snap := state.Snapshot(now)
		slog.Info("state", "desired", snap.Desired, "live", snap.Live, "inflight", snap.Inflight, "machines", snap.Machines,
			"booting", snap.Booting, "idle", snap.Idle, "busy", snap.Busy, "ending", snap.Ending, "on_demand", snap.OnDemand,
			"notices", snap.Notices, "regs_window", snap.RegsWindow, "max_regs_window", snap.MaxRegWindow, "total_regs", snap.TotalRegs, "reg_errors", snap.RegErrors)
	}
}
