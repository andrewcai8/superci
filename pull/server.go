package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/actions/scaleset"
)

// A Set is one scale set as the listener serves it.
type Set struct {
	spec  SetSpec
	id    int // GitHub's scale set id
	state *State
	fleet *SetFleet
}

// Server answers the machines. A machine proves who it is with the identity document EC2 signed for it; the listener
// serves it only if that instance is one of its sets' machines.
type Server struct {
	sets     map[string]*Set
	budget   *Budget
	verifier *Verifier
	client   *scaleset.Client
	refresh  func(ctx context.Context, region string) // lists a region again, at most once a second
	slots    chan struct{}                            // bounds concurrent registrations
	maxAge   func() time.Duration
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
	default:
		http.NotFound(w, r)
	}
}

// machine verifies the request's identity document and returns the set and machine it names, listing EC2 again if
// the instance is too new for the last listing.
func (h *Server) machine(r *http.Request) (*Set, *Machine, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 16<<10))
	if err != nil {
		return nil, nil, err
	}
	doc, err := h.verifier.Verify(body)
	if err != nil {
		return nil, nil, err
	}
	for try := 0; try < 3; try++ {
		for _, s := range h.sets {
			if m := s.state.Machine(doc.InstanceID); m != nil {
				return s, m, nil
			}
		}
		h.refresh(r.Context(), doc.Region)
	}
	return nil, nil, fmt.Errorf("instance %s is no machine of ours", doc.InstanceID)
}

func (h *Server) jit(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	s, m, err := h.machine(r)
	if err != nil {
		slog.Warn("refused machine", "remote", r.RemoteAddr, "err", err.Error())
		http.Error(w, "unknown machine", http.StatusForbidden)
		return
	}
	// Its last runner took a job and ended: wait (a few seconds at most) until the queue reports that job complete,
	// so that the decision counts it as gone.
	for i := 0; i < 25 && !s.state.PreviousDone(m.ID); i++ {
		time.Sleep(200 * time.Millisecond)
	}
	for attempt := 0; ; attempt++ {
		verdict, old := s.state.Ask(m.ID, time.Now(), h.budget, h.maxAge())
		switch verdict {
		case Replace:
			err := h.client.RemoveRunner(context.WithoutCancel(r.Context()), old.ID)
			removed := err == nil || errors.Is(err, scaleset.RunnerNotFoundError) || errors.Is(err, scaleset.NotFoundError)
			s.state.Replaced(m, old, removed)
			slog.Info("replace runner", "set", s.spec.Name, "machine", m.ID, "runner", old.Name, "removed", removed, "err", errString(err))
			if removed && attempt == 0 {
				continue
			}
			fallthrough
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
		break
	}
	h.slots <- struct{}{}
	defer func() { <-h.slots }()
	suffix := make([]byte, 2)
	_, _ = rand.Read(suffix)
	name := fmt.Sprintf("%s-%s-%s", s.spec.Name, m.ID, hex.EncodeToString(suffix))
	// A refusal comes back at once, not after the client's own waits: the machine is told to ask again, and the
	// listener stops asking GitHub for a while.
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	ctx = scaleset.WithRequestRetry(ctx, func(rc *scaleset.RetryConfig) {
		rc.OnRateLimit = func(context.Context, *scaleset.RateLimitError) (time.Duration, bool) { return 0, false }
	})
	cfg, err := h.client.GenerateJitRunnerConfig(ctx, &scaleset.RunnerScaleSetJitRunnerSetting{Name: name}, s.id)
	if err != nil {
		var rl *scaleset.RateLimitError
		refused := errors.As(err, &rl) || strings.Contains(err.Error(), "ConfigureTooManyRunners")
		slog.Error("register", "set", s.spec.Name, "machine", m.ID, "runner", name, "refused", refused, "err", err.Error(), "ms", time.Since(started).Milliseconds())
		if refused {
			h.budget.Done(time.Now(), Refused, 0)
			w.Header().Set("Retry-After", "5")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		h.budget.Done(time.Now(), Failed, 0)
		http.Error(w, "registration failed", http.StatusServiceUnavailable)
		return
	}
	now := time.Now()
	h.budget.Done(now, Made, int64(cfg.Runner.ID))
	s.state.Registered(m.ID, &Runner{ID: int64(cfg.Runner.ID), Name: name, Issued: now})
	slog.Info("register", "set", s.spec.Name, "machine", m.ID, "runner", name, "runner_id", cfg.Runner.ID, "ms", time.Since(started).Milliseconds())
	w.Header().Set("Content-Type", "text/plain")
	_, _ = w.Write([]byte(cfg.EncodedJITConfig))
}

func (h *Server) notice(w http.ResponseWriter, r *http.Request) {
	s, m, err := h.machine(r)
	if err != nil {
		http.Error(w, "unknown machine", http.StatusForbidden)
		return
	}
	_, idle := s.state.Noticed(m.ID)
	slog.Warn("spot notice", "set", s.spec.Name, "machine", m.ID, "region", m.Region, "type", m.Type, "az", m.AZ, "idle_runner", idle != nil)
	w.WriteHeader(http.StatusNoContent)
	if idle != nil {
		err := h.client.RemoveRunner(context.WithoutCancel(r.Context()), idle.ID)
		removed := err == nil || errors.Is(err, scaleset.RunnerNotFoundError) || errors.Is(err, scaleset.NotFoundError)
		s.state.Removed(m, removed)
		slog.Info("noticed runner", "set", s.spec.Name, "machine", m.ID, "runner", idle.Name, "removed", removed, "err", errString(err))
	}
}

// Status is the whole listener as the admin endpoint shows it.
type Status struct {
	Budget BudgetSnapshot `json:"budget"`
	Spend  SpendSnapshot  `json:"spend"`
	Sets   []Snapshot     `json:"sets"`
}

// Admin is for the operator, on localhost only: the status, health, and a set's pool floor and cap to change at run
// time (the config file is the lasting way).
func Admin(status func() Status, healthy func() error, override func(set string, pool, max int)) http.Handler {
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
	return mux
}
