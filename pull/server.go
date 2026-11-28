package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/actions/scaleset"
)

// A Set is one scale set as the listener serves it.
type Set struct {
	spec  SetSpec
	id    int // GitHub's scale set id
	state *State
	fleet *SetFleet
	drain bool // the config dropped it: it takes no job, and is deleted once it holds none
}

// Server answers the machines. A machine proves who it is with the identity document EC2 signed for it; the listener
// serves it only if that instance is one of its sets' machines.
type Server struct {
	sets     map[string]*Set
	budget   *Budget
	verifier interface {
		Verify(body []byte) (InstanceDocument, error)
	}
	client interface {
		GenerateJitRunnerConfig(ctx context.Context, setting *scaleset.RunnerScaleSetJitRunnerSetting, scaleSetID int) (*scaleset.RunnerScaleSetJitRunnerConfig, error)
	}
	refresh  func(ctx context.Context, region string)                // lists a region again, at most once a second
	claim    func(ctx context.Context, instance, token string) error // Claims.Take: nil once the grant holds the machine's claim
	issue    func(ctx context.Context, m *Machine) error             // writes the machine's issued tag; nil once EC2 has it
	notices  func(ctx context.Context, m *Machine, j JobRef) error   // records on the machine the job a spot notice found
	slots    chan struct{}                                           // bounds concurrent registrations
	maxAge   func() time.Duration
	dispatch http.Handler // the identity plane's /work, /shim and /agent (Dispatcher)
	// dispatchMachine is the dispatch set and machine an instance is, if it is one; nil when the identity plane is off.
	dispatchMachine func(id string) (set string, m *Machine)
}

// noticeTaker is what of the dispatcher takes a dispatch machine's spot notice (Dispatcher.Notice).
type noticeTaker interface {
	Notice(doc InstanceDocument, action string, now time.Time) (known bool, err error)
}

func (h *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST the instance's signed identity document", http.StatusMethodNotAllowed)
		return
	}
	switch r.URL.Path {
	case "/jit":
		h.jit(w, r)
	case "/notice":
		h.notice(w, r)
	case "/mark":
		h.mark(w, r)
	case "/work", "/shim", "/agent":
		if h.dispatch == nil {
			http.NotFound(w, r)
			return
		}
		h.dispatch.ServeHTTP(w, r)
	default:
		http.NotFound(w, r)
	}
}

// machine verifies the request's identity document and returns the set and machine it names, listing EC2 again if
// the instance is too new for the last listing.
func (h *Server) machine(r *http.Request) (*Set, *Machine, error) {
	doc, err := h.document(r)
	if err != nil {
		return nil, nil, err
	}
	return h.machineOf(r, doc)
}

// document is the request's verified identity document.
func (h *Server) document(r *http.Request) (InstanceDocument, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 16<<10))
	if err != nil {
		return InstanceDocument{}, err
	}
	return h.verifier.Verify(body)
}

// machineOf is the scale set's machine the document names, listing its region again (at most once a second) while
// no set knows it.
func (h *Server) machineOf(r *http.Request, doc InstanceDocument) (*Set, *Machine, error) {
	for try := 0; try < 3; try++ {
		if s, m := h.scaleMachine(doc.InstanceID); m != nil {
			return s, m, nil
		}
		h.refresh(r.Context(), doc.Region)
	}
	return nil, nil, fmt.Errorf("instance %s is no machine of ours", doc.InstanceID)
}

// scaleMachine is the scale set and machine an instance is, if some set holds it.
func (h *Server) scaleMachine(id string) (*Set, *Machine) {
	for _, s := range h.sets {
		if m := s.state.Machine(id); m != nil {
			return s, m
		}
	}
	return nil, nil
}

// jit answers a machine's ask for its runner. A machine gets one runner, ever: its grant claims it (Claims), the
// runner is registered, EC2 is told the machine was issued it (its tag), and only then does the config leave. A claim
// another grant holds answers 410; a claim not known to be ours, or a config that cannot be recorded, never leaves.
func (h *Server) jit(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	s, m, err := h.machine(r)
	if err != nil {
		slog.Warn("refused machine", "remote", r.RemoteAddr, "err", err.Error())
		http.Error(w, "unknown machine", http.StatusForbidden)
		return
	}
	verdict, why := s.state.AskWhy(m.ID, time.Now(), h.budget, h.maxAge())
	tlAsk(s.spec.Name, m.ID, verdict, why, started)
	switch verdict {
	case Wait:
		w.Header().Set("Retry-After", "3")
		w.WriteHeader(http.StatusTooManyRequests)
		return
	case End:
		slog.Info("ask", "set", s.spec.Name, "machine", m.ID, "verdict", "end")
		w.WriteHeader(http.StatusGone)
		return
	case Unknown:
		http.Error(w, "unknown machine", http.StatusForbidden)
		return
	}
	h.slots <- struct{}{}
	defer func() { <-h.slots }()
	name := runnerName(s.spec.Name, m.ID)
	cctx, ccancel := context.WithTimeout(context.WithoutCancel(r.Context()), 10*time.Second)
	claimStart := time.Now()
	err = h.claim(cctx, m.ID, s.state.Claim(m))
	ccancel()
	tlClaim(s.spec.Name, m.ID, err, claimStart, time.Now())
	if err != nil {
		h.budget.Done(time.Now(), Unused, 0)
		slog.Error("claim", "set", s.spec.Name, "machine", m.ID, "elsewhere", errors.Is(err, ErrClaimed), "err", err.Error(), "ms", time.Since(started).Milliseconds())
		if errors.Is(err, ErrClaimed) {
			s.state.IssuedElsewhere(m)
			w.WriteHeader(http.StatusGone)
			return
		}
		s.state.Unissued(m)
		w.Header().Set("Retry-After", "3")
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}
	// A refusal comes back at once, not after the client's own waits: the machine is told to ask again, and the
	// listener stops asking GitHub for a while.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 15*time.Second)
	defer cancel()
	ctx = scaleset.WithRequestRetry(ctx, func(rc *scaleset.RetryConfig) {
		rc.OnRateLimit = func(context.Context, *scaleset.RateLimitError) (time.Duration, bool) { return 0, false }
	})
	cfg, err := h.client.GenerateJitRunnerConfig(ctx, &scaleset.RunnerScaleSetJitRunnerSetting{Name: name}, s.id)
	if err != nil {
		refused := refusedRegistration(err)
		exists := errors.Is(err, scaleset.RunnerExistsError)
		slog.Error("register", "set", s.spec.Name, "machine", m.ID, "runner", name, "refused", refused, "exists", exists, "err", err.Error(), "ms", time.Since(started).Milliseconds())
		switch {
		case exists:
			// GitHub holds a runner of this machine's name that was never handed out (an answer lost to a crash or a
			// timeout): the machine gets none, and the runner is removed as an orphan once the machine ends.
			tlRefused(s.spec.Name, m.ID, "exists", started, time.Now())
			h.budget.Done(time.Now(), Failed, 0)
			s.state.Withheld(m, nil)
			w.WriteHeader(http.StatusGone)
		case refused:
			tlRefused(s.spec.Name, m.ID, "refused", started, time.Now())
			h.budget.Done(time.Now(), Refused, 0)
			b := h.budget.Snapshot(time.Now())
			slog.Warn("registration refused by GitHub", "set", s.spec.Name, "machine", m.ID, "refused", b.Refused, "rate", b.Rate, "window_use_estimate", b.WindowUse)
			s.state.Unissued(m)
			w.Header().Set("Retry-After", "5")
			w.WriteHeader(http.StatusTooManyRequests)
		default:
			tlRefused(s.spec.Name, m.ID, "failed", started, time.Now())
			h.budget.Done(time.Now(), Failed, 0)
			s.state.Unissued(m)
			http.Error(w, "registration failed", http.StatusServiceUnavailable)
		}
		return
	}
	now := time.Now()
	h.budget.Done(now, Made, int64(cfg.Runner.ID))
	runner := &Runner{ID: int64(cfg.Runner.ID), Name: name, Issued: now}
	tctx, tcancel := context.WithTimeout(context.WithoutCancel(r.Context()), 10*time.Second)
	defer tcancel()
	if err := h.issue(tctx, m); err != nil {
		h.budget.Lost()
		s.state.Withheld(m, runner)
		slog.Error("issue", "set", s.spec.Name, "machine", m.ID, "runner", name, "runner_id", cfg.Runner.ID, "withheld", true, "err", err.Error())
		tlRefused(s.spec.Name, m.ID, "withheld", started, time.Now())
		w.WriteHeader(http.StatusGone)
		return
	}
	s.state.Issue(m, runner)
	first, asks := s.state.Asking(m)
	slog.Info("register", "set", s.spec.Name, "machine", m.ID, "runner", name, "runner_id", cfg.Runner.ID, "ms", time.Since(started).Milliseconds(),
		"waited_ms", started.Sub(first).Milliseconds(), "asks", asks)
	tlIssued(s.spec.Name, m.ID, name, cfg.Runner.ID, started, time.Now())
	w.Header().Set("Content-Type", "text/plain")
	_, _ = w.Write([]byte(cfg.EncodedJITConfig))
}

// notice takes in a machine's word that AWS gave notice it takes the machine back. A dispatch machine's goes to the
// dispatcher, which fails and re-runs the job it was handed at once (Dispatcher.Notice). A scale set's machine is no
// longer supply; an idle runner of it retires (the reconciler removes it before the machine ends). A job it runs is
// recorded on the machine (EC2's superci-pull-noticed tag), so a restart can still re-run the job once AWS has taken
// the machine. The machine is looked up as a /mark's is (marker), listing EC2 again only while no set knows it.
func (h *Server) notice(w http.ResponseWriter, r *http.Request) {
	doc, err := h.document(r)
	if err != nil {
		http.Error(w, "unknown machine", http.StatusForbidden)
		return
	}
	for try := 0; try < 3; try++ {
		if s, m := h.scaleMachine(doc.InstanceID); m != nil {
			h.scaleNotice(w, r, s, m)
			return
		}
		if d, ok := h.dispatch.(noticeTaker); ok {
			if known, err := d.Notice(doc, r.Header.Get(actionHeader), time.Now()); known {
				h.dispatchNoticed(w, doc, r.Header.Get(actionHeader), err)
				return
			}
		}
		h.refresh(r.Context(), doc.Region)
	}
	http.Error(w, "unknown machine", http.StatusForbidden)
}

// dispatchNoticed answers a dispatch machine's notice: 204 once the dispatcher took it, 400 for one that is not current,
// which the machine does not send again. The log keeps the instance-action IMDS gave the machine, so a notice can be
// checked against EC2's own record of the interruption afterwards (the spot request's status).
func (h *Server) dispatchNoticed(w http.ResponseWriter, doc InstanceDocument, action string, err error) {
	if err != nil {
		slog.Warn("spot notice refused", "machine", doc.InstanceID, "err", err.Error())
		http.Error(w, "no current spot notice", http.StatusBadRequest)
		return
	}
	if h.dispatchMachine != nil {
		if set, m := h.dispatchMachine(doc.InstanceID); m != nil {
			slog.Warn("spot notice", "set", set, "machine", m.ID, "region", m.Region, "type", m.Type, "az", m.AZ, "action", clip(action, 256))
			tlNotice(set, m, time.Now())
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// scaleNotice takes in a scale set's machine's notice.
func (h *Server) scaleNotice(w http.ResponseWriter, r *http.Request, s *Set, m *Machine) {
	job := s.state.Noticed(m.ID)
	slog.Warn("spot notice", "set", s.spec.Name, "machine", m.ID, "region", m.Region, "type", m.Type, "az", m.AZ, "action", clip(r.Header.Get(actionHeader), 256))
	tlNotice(s.spec.Name, m, time.Now())
	w.WriteHeader(http.StatusNoContent)
	if job != nil && h.notices != nil {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 10*time.Second)
		defer cancel()
		if err := h.notices(ctx, m, *job); err != nil {
			slog.Error("notice tag", "set", s.spec.Name, "machine", m.ID, "err", err.Error())
		}
	}
}

// removal reads GitHub's answer to removing a runner: only GitHub's refusal for a runner that holds a job means busy.
func removal(err error) Removal {
	switch {
	case err == nil, errors.Is(err, scaleset.RunnerNotFoundError), errors.Is(err, scaleset.NotFoundError):
		return Removed
	case errors.Is(err, scaleset.JobStillRunningError):
		return HeldJob
	default:
		return NotSure
	}
}

// Status is the whole listener as the admin endpoint shows it.
type Status struct {
	Budget   BudgetSnapshot     `json:"budget"`
	Spend    SpendSnapshot      `json:"spend"`
	Sets     []Snapshot         `json:"sets"`
	Dispatch []DispatchSnapshot `json:"dispatch"`
}

// Admin is for the operator, on localhost only: the status, health, and a set's pool floor and cap to change at run
// time (the config file is the lasting way).
//
// A lane that registers runners outside the listener leases registrations from the budget first (lease.go):
// POST /lease?n=40&ttl=120 answers {"id", "granted", "expires"}, and POST /lease/settle?id=<id>&made=<k> ends it. A
// lane that GitHub refused a registration for the cap adds refused=1 (and failed=<f> for its other failures), which
// holds the budget back as the listener's own refusal does.
func Admin(status func() Status, healthy func() error, override func(set string, pool, max int), budget *Budget, routes ...func(*http.ServeMux)) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /state", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(status())
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := healthy(); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("POST /override", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		num := func(k string) int {
			n, err := strconv.Atoi(q.Get(k))
			if err != nil || n < 0 {
				return -1
			}
			return n
		}
		override(q.Get("set"), num("pool"), num("max"))
		slog.Info("admin override", "set", q.Get("set"), "pool", q.Get("pool"), "max", q.Get("max"))
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /lease", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		n, err1 := strconv.Atoi(q.Get("n"))
		ttl, err2 := strconv.Atoi(q.Get("ttl"))
		if err1 != nil || err2 != nil || n < 1 || ttl < 1 {
			http.Error(w, "n and ttl (seconds) are positive integers", http.StatusBadRequest)
			return
		}
		l := budget.Lease(time.Now(), n, time.Duration(ttl)*time.Second)
		slog.Info("lease", "asked", n, "granted", l.Granted, "ttl_s", ttl, "id", l.ID)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(l)
	})
	mux.HandleFunc("POST /lease/settle", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		made, err := strconv.Atoi(q.Get("made"))
		if err != nil || made < 0 {
			http.Error(w, "made is the number of registrations the lane made", http.StatusBadRequest)
			return
		}
		failed := 0
		if q.Has("failed") {
			if failed, err = strconv.Atoi(q.Get("failed")); err != nil || failed < 0 {
				http.Error(w, "failed is the number of registrations that failed", http.StatusBadRequest)
				return
			}
		}
		if v := q.Get("refused"); v != "" && v != "0" && v != "1" {
			http.Error(w, "refused is 1 when GitHub refused a registration", http.StatusBadRequest)
			return
		}
		refused := q.Get("refused") == "1"
		held := budget.Settle(time.Now(), q.Get("id"), Settlement{Made: made, Failed: failed, Refused: refused})
		slog.Info("lease settled", "id", q.Get("id"), "made", made, "failed", failed, "refused", refused, "held", held)
		if !held {
			http.Error(w, "no such lease: it expired or was settled", http.StatusGone)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	for _, add := range routes {
		add(mux)
	}
	return mux
}

// refusedRegistration says whether GitHub's answer to a registration is a refusal, which backs the listener off. GitHub
// documents the cap but not what its refusal looks like (429, a 403 with or without a header, a 400, 409 or 422 naming
// ConfigureTooManyRunners or not, a 5xx under load), so every error is one, except the answers that say something
// else: the runner exists, our credential is bad, the scale set is gone, or we cancelled the ask ourselves. Counted as
// a failure, a refusal has no backoff and the listener asks GitHub again at once.
func refusedRegistration(err error) bool {
	for _, other := range []error{scaleset.RunnerExistsError, scaleset.UnauthorizedError, scaleset.NotFoundError, context.Canceled} {
		if errors.Is(err, other) {
			return false
		}
	}
	return true
}
