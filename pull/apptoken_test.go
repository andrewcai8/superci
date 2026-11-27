package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeInstallation is GitHub's /app/installations/<id>/access_tokens, which hands out a numbered token that lives
// for `lives`, or refuses with `refuse`, and keeps the narrowing each request asked for.
type fakeInstallation struct {
	mu     sync.Mutex
	minted int
	lives  time.Duration
	refuse int
	bodies []string
	srv    *httptest.Server
}

func newFakeInstallation(t *testing.T) *fakeInstallation {
	f := &fakeInstallation{lives: time.Hour}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		b, _ := io.ReadAll(r.Body)
		f.bodies = append(f.bodies, string(b))
		if r.URL.Path != "/app/installations/5/access_tokens" || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if f.refuse != 0 {
			w.WriteHeader(f.refuse)
			fmt.Fprint(w, `{"message":"The permissions requested are not granted to this installation."}`)
			return
		}
		f.minted++
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"token":"ghs_tool%d","expires_at":%q}`, f.minted, time.Now().Add(f.lives).UTC().Format(time.RFC3339))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeInstallation) rest(t *testing.T) *REST {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemKey := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	u, _ := url.Parse(f.srv.URL)
	return &REST{app: App{ID: 1, PEM: pemKey}, installation: 5, org: "o", http: &http.Client{Transport: toServer{u}}}
}

func toolTokens(t *testing.T, f *fakeInstallation) *AppTokens {
	return NewAppTokens(f.rest(t), []string{"megpt-mono"})
}

func adminWith(a *AppTokens) http.Handler {
	return Admin(func() Status { return Status{} }, func() error { return nil }, func(string, int, int) {}, spare(), a.Routes)
}

func TestTheToolTokenIsReadOnlyAndNarrowedToTheConfiguredRepositories(t *testing.T) {
	f := newFakeInstallation(t)
	w := do(adminWith(toolTokens(t, f)), http.MethodPost, "/github/token")
	var got struct {
		Token   string `json:"token"`
		Expires string `json:"expires"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != 200 || got.Token != "ghs_tool1" || got.Expires == "" {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	var asked struct {
		Repositories []string          `json:"repositories"`
		Permissions  map[string]string `json:"permissions"`
	}
	if err := json.Unmarshal([]byte(f.bodies[0]), &asked); err != nil {
		t.Fatalf("the request body %q: %v", f.bodies[0], err)
	}
	if len(asked.Repositories) != 1 || asked.Repositories[0] != "megpt-mono" || len(asked.Permissions) != 1 || asked.Permissions["actions"] != "read" {
		t.Errorf("the tools' token must read actions of the configured repository and nothing else: %s", f.bodies[0])
	}
}

func TestTheToolTokenIsServedOnTheAdminPortAndNeverOnThePublicOne(t *testing.T) {
	f := newFakeInstallation(t)
	tokens := toolTokens(t, f)
	public := &Server{sets: map[string]*Set{}}
	if w := do(public, http.MethodPost, "/github/token"); w.Code != http.StatusNotFound || strings.Contains(w.Body.String(), "ghs_") {
		t.Errorf("the machines' port answered /github/token: %d %s", w.Code, w.Body.String())
	}
	if f.minted != 0 {
		t.Errorf("a request to the public port minted %d tokens", f.minted)
	}
	if w := do(adminWith(tokens), http.MethodGet, "/github/token"); w.Code != http.StatusMethodNotAllowed || f.minted != 0 {
		t.Errorf("only POST mints: %d, %d minted", w.Code, f.minted)
	}
	if w := do(adminWith(tokens), http.MethodPost, "/github/token"); w.Code != 200 {
		t.Errorf("the admin mux must serve it: %d", w.Code)
	}
}

func TestTheToolTokenIsHeldUntilNearExpiryAndMintedAgainThen(t *testing.T) {
	f := newFakeInstallation(t)
	h := adminWith(toolTokens(t, f))
	first := do(h, http.MethodPost, "/github/token").Body.String()
	second := do(h, http.MethodPost, "/github/token").Body.String()
	if first != second || f.minted != 1 {
		t.Fatalf("a token with an hour left is handed out again, not minted again: %d minted\n%s\n%s", f.minted, first, second)
	}
	// A token that lives 5 minutes is under the reserve a tool needs for one command, so every ask mints.
	f2 := newFakeInstallation(t)
	f2.lives = 5 * time.Minute
	h2 := adminWith(toolTokens(t, f2))
	a, b := do(h2, http.MethodPost, "/github/token").Body.String(), do(h2, http.MethodPost, "/github/token").Body.String()
	if a == b || f2.minted != 2 {
		t.Errorf("a token near expiry must be replaced: %d minted\n%s\n%s", f2.minted, a, b)
	}
}

func TestTheToolTokenIsNeverLogged(t *testing.T) {
	buf := logs(t)
	f := newFakeInstallation(t)
	h := adminWith(toolTokens(t, f))
	if w := do(h, http.MethodPost, "/github/token"); w.Code != 200 || !strings.Contains(w.Body.String(), "ghs_tool1") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	f.refuse = http.StatusUnprocessableEntity
	f.lives = 0
	do(adminWith(toolTokens(t, f)), http.MethodPost, "/github/token")
	if strings.Contains(buf.String(), "ghs_") || strings.Contains(buf.String(), "BEGIN") {
		t.Errorf("the log holds a token or a key:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "tool token issued") {
		t.Errorf("the issue is not logged at all:\n%s", buf.String())
	}
}

func TestAPermissionTheAppLacksIsRefusedWithGitHubsOwnWords(t *testing.T) {
	f := newFakeInstallation(t)
	f.refuse = http.StatusUnprocessableEntity
	w := do(adminWith(toolTokens(t, f)), http.MethodPost, "/github/token")
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "permissions requested are not granted") {
		t.Errorf("%d %s", w.Code, w.Body.String())
	}
	f.refuse = http.StatusUnauthorized
	if w := do(adminWith(toolTokens(t, f)), http.MethodPost, "/github/token"); w.Code != http.StatusBadGateway {
		t.Errorf("a refusal that is not about permissions is the listener's bad gateway: %d", w.Code)
	}
}
