package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// AppTokens serves the operator's tools a token to read GitHub with on the App's own rate budget (an installation
// token's 5,000 an hour), so the gate and the canary never spend the personal token every session on the operator's
// host shares. The App's private key stays here: the tools reach the admin port through deploy/ssh.sh, and only the
// short-lived token leaves. It is narrowed to the repositories the config names and to `actions: read`, so the token
// that leaves can read runs, jobs and logs and can write nothing. What the tools need beyond that (the contents of
// ci-limits.tsv, the SUPERCI_JOBS variable) is not asked for here: the README lists those calls as named exceptions.
type AppTokens struct {
	rest  *REST
	repos []string

	mu      sync.Mutex
	token   string
	expires time.Time
}

// toolTokenReserve is how long a token must still live to be handed out: a tool holds it for a whole command.
const toolTokenReserve = 10 * time.Minute

func NewAppTokens(rest *REST, repos []string) *AppTokens {
	return &AppTokens{rest: rest, repos: repos}
}

// Routes adds the endpoint to the admin mux, and to no other.
func (a *AppTokens) Routes(mux *http.ServeMux) {
	mux.HandleFunc("POST /github/token", a.serve)
}

// Token is the read-only installation token, minted again only when the one held has under toolTokenReserve left.
func (a *AppTokens) Token(ctx context.Context) (string, time.Time, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.token != "" && time.Until(a.expires) > toolTokenReserve {
		return a.token, a.expires, nil
	}
	token, expires, err := a.rest.mint(ctx, map[string]any{"repositories": a.repos, "permissions": map[string]string{"actions": "read"}})
	if err != nil {
		return "", time.Time{}, err
	}
	a.token, a.expires = token, expires
	return token, expires, nil
}

func (a *AppTokens) serve(w http.ResponseWriter, r *http.Request) {
	token, expires, err := a.Token(r.Context())
	if err != nil {
		// GitHub's refusal names the permission the App lacks; the answer carries it and never a token.
		slog.Warn("tool token refused", "err", err.Error())
		code := http.StatusBadGateway
		var me *MintError
		if errors.As(err, &me) && me.Code == http.StatusUnprocessableEntity {
			code = http.StatusUnprocessableEntity
		}
		http.Error(w, err.Error(), code)
		return
	}
	slog.Info("tool token issued", "expires", expires.UTC().Format(time.RFC3339))
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{"token": token, "expires": expires.UTC().Format(time.RFC3339)})
}
