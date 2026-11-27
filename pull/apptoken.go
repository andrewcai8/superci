package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// AppTokens serves the operator's tools a token to read and edit GitHub with on the App's own budget (an installation
// token's 5,000 an hour), so the gate and the canary never spend the personal token every session on the operator's
// host shares. The App's private key stays here: the tools reach the admin port through deploy/ssh.sh, and only a
// short-lived token leaves. A tool names the scope it needs, and each scope is a token of exactly one permission:
//
//	actions    actions: read on the config's repositories (runs, jobs, logs)
//	variables  actions_variables: write on megpt-mono alone (GitHub's "Variables" repository permission; reads and writes
//	           SUPERCI_JOBS, the fleet switch and the canary's rollback)
//	contents   contents: read on megpt-mono alone (scripts/verify/ci-limits.tsv)
//
// A scope the installation does not hold is refused by name, and never answered with a broader token: the mint always
// carries the narrowing, and the installation's own permissions are read first, so the refusal says which permission to grant.
type AppTokens struct {
	rest  *REST
	repos []string

	mu    sync.Mutex // guards held only, and is never held across a call to GitHub: a hung mint of one scope never blocks another scope's held token
	held  map[string]heldToken
	mints map[string]*sync.Mutex // one per scope, held across its permission read and mint

	permsMu  sync.Mutex // guards perms, permsAt and permsErr, and is held across the read of the installation's permissions
	perms    map[string]string
	permsAt  time.Time
	permsErr bool
}

type heldToken struct {
	token   string
	expires time.Time
}

// toolScope is one kind of token: GitHub's permission key and the level it is minted at.
type toolScope struct {
	permission string
	level      string
	configured bool // the config's repositories; false: megpt-mono only
}

// toolRepository is the one repository the variables and contents tokens reach.
const toolRepository = "megpt-mono"

var toolScopes = map[string]toolScope{
	"actions":   {permission: "actions", level: "read", configured: true},
	"variables": {permission: "actions_variables", level: "write"},
	"contents":  {permission: "contents", level: "read"},
}

// toolTokenReserve is how long a token must still live to be handed out: a tool holds it for a whole command.
const toolTokenReserve = 10 * time.Minute

// permissionsTTL is how long the installation's permissions are held: a grant takes effect within a minute, and a tool asks far less often.
const permissionsTTL = time.Minute

func NewAppTokens(rest *REST, repos []string) *AppTokens {
	return &AppTokens{rest: rest, repos: repos, held: map[string]heldToken{}, mints: map[string]*sync.Mutex{}}
}

// Routes adds the endpoint to the admin mux, and to no other.
func (a *AppTokens) Routes(mux *http.ServeMux) {
	mux.HandleFunc("POST /github/token", a.serve)
}

// ScopeRefused is the installation lacking a permission a scope needs. Its text names the permission and holds no token.
type ScopeRefused struct {
	Scope, Permission, Level, Have, Cause string
}

func (e *ScopeRefused) Error() string {
	have := "no " + e.Permission + " permission"
	if e.Have != "" {
		have = e.Permission + ": " + e.Have
	}
	msg := fmt.Sprintf("the superci App's installation lacks the repository permission %q (%s); scope %q was not minted. The installation owner grants it on the App and accepts the change",
		e.Permission+": "+e.Level, have, e.Scope)
	if e.Cause != "" {
		msg += " (GitHub: " + e.Cause + ")"
	}
	return msg
}

// permissionRank orders the levels a permission is held at.
func permissionRank(level string) int {
	switch level {
	case "read":
		return 1
	case "write":
		return 2
	case "admin":
		return 3
	}
	return 0
}

// installed is the installation's permissions, read again when the held ones are over permissionsTTL old. A read that fails is
// not an answer: the narrowed mint is then what refuses, with GitHub's own 422.
func (a *AppTokens) installed(ctx context.Context) (map[string]string, bool) {
	a.permsMu.Lock()
	defer a.permsMu.Unlock()
	if !a.permsAt.IsZero() && time.Since(a.permsAt) < permissionsTTL {
		return a.perms, !a.permsErr
	}
	perms, err := a.rest.InstallationPermissions(ctx)
	a.permsAt, a.perms, a.permsErr = time.Now(), perms, err != nil
	if err != nil {
		slog.Warn("installation permissions unread", "err", err.Error())
	}
	return perms, err == nil
}

// forgetPermissions makes the next ask read the installation's permissions again.
func (a *AppTokens) forgetPermissions() {
	a.permsMu.Lock()
	a.permsAt = time.Time{}
	a.permsMu.Unlock()
}

// holding is a scope's held token, when it has more than toolTokenReserve left.
func (a *AppTokens) holding(name string) (heldToken, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	h, ok := a.held[name]
	return h, ok && time.Until(h.expires) > toolTokenReserve
}

// Token is the installation token of a scope, minted again only when the one held has under toolTokenReserve left. A held token is
// answered at once; a mint holds only its own scope's lock, so a hung mint delays the asks for that scope alone.
func (a *AppTokens) Token(ctx context.Context, name string) (string, time.Time, error) {
	sc, ok := toolScopes[name]
	if !ok {
		return "", time.Time{}, fmt.Errorf("unknown scope %q", name)
	}
	if h, ok := a.holding(name); ok {
		return h.token, h.expires, nil
	}
	a.mu.Lock()
	mint := a.mints[name]
	if mint == nil {
		mint = &sync.Mutex{}
		a.mints[name] = mint
	}
	a.mu.Unlock()
	mint.Lock()
	defer mint.Unlock()
	if h, ok := a.holding(name); ok { // another ask minted it while this one waited
		return h.token, h.expires, nil
	}
	if perms, known := a.installed(ctx); known && permissionRank(perms[sc.permission]) < permissionRank(sc.level) {
		return "", time.Time{}, &ScopeRefused{Scope: name, Permission: sc.permission, Level: sc.level, Have: perms[sc.permission]}
	}
	repos := []string{toolRepository}
	if sc.configured {
		repos = a.repos
	}
	token, expires, err := a.rest.mint(ctx, map[string]any{"repositories": repos, "permissions": map[string]string{sc.permission: sc.level}})
	if err != nil {
		var me *MintError
		if errors.As(err, &me) && me.Code == http.StatusUnprocessableEntity {
			a.forgetPermissions() // the installation changed under the held permissions: read them again
			return "", time.Time{}, &ScopeRefused{Scope: name, Permission: sc.permission, Level: sc.level, Cause: me.Message}
		}
		return "", time.Time{}, err
	}
	a.mu.Lock()
	a.held[name] = heldToken{token, expires}
	a.mu.Unlock()
	return token, expires, nil
}

func (a *AppTokens) serve(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("scope")
	if name == "" {
		name = "actions"
	}
	if _, ok := toolScopes[name]; !ok {
		http.Error(w, fmt.Sprintf("unknown scope %q: actions, variables or contents", name), http.StatusBadRequest)
		return
	}
	token, expires, err := a.Token(r.Context(), name)
	if err != nil {
		// The refusal names the permission the App lacks; the answer carries it and never a token.
		slog.Warn("tool token refused", "scope", name, "err", err.Error())
		code := http.StatusBadGateway
		var sr *ScopeRefused
		if errors.As(err, &sr) {
			code = http.StatusUnprocessableEntity
		}
		http.Error(w, err.Error(), code)
		return
	}
	slog.Info("tool token issued", "scope", name, "expires", expires.UTC().Format(time.RFC3339))
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{"token": token, "expires": expires.UTC().Format(time.RFC3339)})
}
