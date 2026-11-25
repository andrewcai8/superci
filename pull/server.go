package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/actions/scaleset"
)

// Identity is the listener's TLS key, kept on its disk so that machines launched before a restart still trust it.
// Machines pin its public key (curl --pinnedpubkey); no certificate authority is involved.
type Identity struct {
	Cert tls.Certificate
	Pin  string // sha256//<base64 of the SubjectPublicKeyInfo's SHA-256>
}

func LoadIdentity(dir string) (Identity, error) {
	keyPath, certPath := filepath.Join(dir, "tls.key"), filepath.Join(dir, "tls.crt")
	if _, err := os.Stat(keyPath); os.IsNotExist(err) {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return Identity{}, err
		}
		tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "superci-pull"},
			NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().AddDate(10, 0, 0), KeyUsage: x509.KeyUsageDigitalSignature,
			ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
		if err != nil {
			return Identity{}, err
		}
		kder, err := x509.MarshalECPrivateKey(key)
		if err != nil {
			return Identity{}, err
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return Identity{}, err
		}
		if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
			return Identity{}, err
		}
		if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}), 0o600); err != nil {
			return Identity{}, err
		}
	}
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return Identity{}, err
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return Identity{}, err
	}
	sum := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
	return Identity{Cert: cert, Pin: "sha256//" + base64.StdEncoding.EncodeToString(sum[:])}, nil
}

// Server answers the machines. A machine proves who it is by asking from its own public address: the listener looks
// the instance up by the id it names, and serves it only if EC2 says that instance, tagged as ours, has that address.
type Server struct {
	state   *State
	fleet   *Fleet
	client  *scaleset.Client
	setID   int
	refresh func(ctx context.Context, region string) // lists a region again, at most once a second
	slots   chan struct{}                            // bounds concurrent registrations
}

func (h *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/jit":
		h.jit(w, r)
	case "/notice":
		h.notice(w, r)
	default:
		http.NotFound(w, r)
	}
}

// machine returns the asking instance if the request comes from its address.
func (h *Server) machine(r *http.Request) *Machine {
	id := r.URL.Query().Get("id")
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	for try := 0; try < 3; try++ {
		if m := h.state.Machine(id); m != nil && m.IP != "" {
			if m.IP == ip {
				return m
			}
			return nil
		}
		for _, region := range h.fleet.regions {
			h.refresh(r.Context(), region.Name)
		}
	}
	return nil
}

func (h *Server) jit(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	m := h.machine(r)
	if m == nil {
		http.Error(w, "unknown machine", http.StatusForbidden)
		return
	}
	// Its last runner took a job and ended: wait (a few seconds at most) until the queue reports that job complete,
	// so that the decision counts it as gone.
	for i := 0; i < 25 && !h.state.PreviousDone(m.ID); i++ {
		time.Sleep(200 * time.Millisecond)
	}
	switch h.state.Ask(m.ID, time.Now()) {
	case End:
		slog.Info("ask", "machine", m.ID, "verdict", "end")
		w.WriteHeader(http.StatusGone)
		return
	case Wait:
		slog.Info("ask", "machine", m.ID, "verdict", "wait")
		w.Header().Set("Retry-After", "3")
		w.WriteHeader(http.StatusTooManyRequests)
		return
	case Unknown:
		http.Error(w, "unknown machine", http.StatusForbidden)
		return
	}
	h.slots <- struct{}{}
	defer func() { <-h.slots }()
	suffix := make([]byte, 2)
	_, _ = rand.Read(suffix)
	name := fmt.Sprintf("%s-%s-%s", h.state.set, m.ID, hex.EncodeToString(suffix))
	// A refusal comes back at once, not after the client's own waits: the machine is told to ask again, and the
	// listener stops asking GitHub for a while.
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	ctx = scaleset.WithRequestRetry(ctx, func(rc *scaleset.RetryConfig) {
		rc.OnRateLimit = func(context.Context, *scaleset.RateLimitError) (time.Duration, bool) { return 0, false }
	})
	cfg, err := h.client.GenerateJitRunnerConfig(ctx, &scaleset.RunnerScaleSetJitRunnerSetting{Name: name}, h.setID)
	if err != nil {
		var rl *scaleset.RateLimitError
		refused := errors.As(err, &rl) || strings.Contains(err.Error(), "ConfigureTooManyRunners")
		slog.Error("register", "machine", m.ID, "runner", name, "refused", refused, "err", err.Error(), "ms", time.Since(started).Milliseconds())
		if refused {
			h.state.Registered(m.ID, nil, Refused, time.Now())
			w.Header().Set("Retry-After", "5")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		h.state.Registered(m.ID, nil, Failed, time.Now())
		http.Error(w, "registration failed", http.StatusServiceUnavailable)
		return
	}
	h.state.Registered(m.ID, &Runner{ID: int64(cfg.Runner.ID), Name: name, Issued: time.Now()}, Made, time.Now())
	slog.Info("register", "machine", m.ID, "runner", name, "runner_id", cfg.Runner.ID, "ms", time.Since(started).Milliseconds())
	w.Header().Set("Content-Type", "text/plain")
	_, _ = w.Write([]byte(cfg.EncodedJITConfig))
}

func (h *Server) notice(w http.ResponseWriter, r *http.Request) {
	m := h.machine(r)
	if m == nil {
		http.Error(w, "unknown machine", http.StatusForbidden)
		return
	}
	_, idle := h.state.Noticed(m.ID)
	slog.Warn("spot notice", "machine", m.ID, "region", m.Region, "type", m.Type, "az", m.AZ, "idle_runner", idle != nil)
	w.WriteHeader(http.StatusNoContent)
	if idle != nil {
		err := h.client.RemoveRunner(context.WithoutCancel(r.Context()), idle.ID)
		removed := err == nil || errors.Is(err, scaleset.RunnerNotFoundError) || errors.Is(err, scaleset.NotFoundError)
		h.state.Removed(m, removed)
		slog.Info("noticed runner", "machine", m.ID, "runner", idle.Name, "removed", removed, "err", errString(err))
	}
}

// Admin is for the operator, on localhost only: the state, and the pool and cap to change at run time.
func Admin(state *State) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /state", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(state.Snapshot(time.Now()))
	})
	set := func(apply func(int)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			n, err := strconv.Atoi(r.URL.Query().Get("n"))
			if err != nil || n < 0 {
				http.Error(w, "n must be a count", http.StatusBadRequest)
				return
			}
			apply(n)
			slog.Info("admin", "path", r.URL.Path, "n", n)
			w.WriteHeader(http.StatusNoContent)
		}
	}
	mux.HandleFunc("POST /pool", set(state.SetPool))
	mux.HandleFunc("POST /max", set(state.SetMax))
	return mux
}
