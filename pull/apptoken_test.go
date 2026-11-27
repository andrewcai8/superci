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
	// perms is what GET /app/installations/5 answers as the installation's permissions; nil answers 404, as an installation that cannot be read.
	perms     map[string]string
	permReads int
	// hold, when set, makes a mint whose request names holdPermission wait for it to close, and says on holding that one is waiting.
	hold           chan struct{}
	holdPermission string
	holding        chan struct{}
	srv            *httptest.Server
}

func newFakeInstallation(t *testing.T) *fakeInstallation {
	f := &fakeInstallation{lives: time.Hour}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && f.hold != nil {
			b, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(strings.NewReader(string(b)))
			if strings.Contains(string(b), f.holdPermission) {
				f.holding <- struct{}{}
				<-f.hold
			}
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Method == http.MethodGet && r.URL.Path == "/app/installations/5" && strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			if f.perms == nil {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			f.permReads++
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 5, "permissions": f.perms})
			return
		}
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

// ask posts for a scope and decodes the answer.
func askScope(h http.Handler, scope string) (code int, token, body string) {
	target := "/github/token"
	if scope != "" {
		target += "?scope=" + scope
	}
	w := do(h, http.MethodPost, target)
	var got struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	return w.Code, got.Token, w.Body.String()
}

// asked is the narrowing of one mint request.
type asked struct {
	Repositories []string          `json:"repositories"`
	Permissions  map[string]string `json:"permissions"`
}

func narrowing(t *testing.T, body string) asked {
	t.Helper()
	var a asked
	if err := json.Unmarshal([]byte(body), &a); err != nil {
		t.Fatalf("the mint request %q: %v", body, err)
	}
	return a
}

func TestEachScopeMintsWithExactlyItsPermissionAndRepository(t *testing.T) {
	f := newFakeInstallation(t)
	// The installation's permissions as GitHub reported them on Nov 27 after the grant.
	f.perms = map[string]string{"actions": "write", "actions_variables": "write", "contents": "read", "metadata": "read", "organization_self_hosted_runners": "write"}
	// The config names two repositories: only the actions scope reaches both.
	h := adminWith(NewAppTokens(f.rest(t), []string{"megpt-mono", "other"}))
	for _, want := range []struct {
		scope, token, perm, level string
		repos                     []string
	}{
		{"", "ghs_tool1", "actions", "read", []string{"megpt-mono", "other"}},
		{"variables", "ghs_tool2", "actions_variables", "write", []string{"megpt-mono"}},
		{"contents", "ghs_tool3", "contents", "read", []string{"megpt-mono"}},
	} {
		code, token, body := askScope(h, want.scope)
		if code != 200 || token != want.token {
			t.Fatalf("scope %q: %d %s", want.scope, code, body)
		}
		got := narrowing(t, f.bodies[len(f.bodies)-1])
		if len(got.Permissions) != 1 || got.Permissions[want.perm] != want.level {
			t.Errorf("scope %q must mint %s: %s and nothing else: %v", want.scope, want.perm, want.level, got.Permissions)
		}
		if strings.Join(got.Repositories, ",") != strings.Join(want.repos, ",") {
			t.Errorf("scope %q must reach %v only: %v", want.scope, want.repos, got.Repositories)
		}
	}
}

func TestEachScopeIsCachedSeparately(t *testing.T) {
	f := newFakeInstallation(t)
	f.perms = map[string]string{"actions": "read", "actions_variables": "write", "contents": "read"}
	h := adminWith(toolTokens(t, f))
	_, a1, _ := askScope(h, "actions")
	_, v1, _ := askScope(h, "variables")
	_, c1, _ := askScope(h, "contents")
	_, a2, _ := askScope(h, "actions")
	_, v2, _ := askScope(h, "variables")
	_, c2, _ := askScope(h, "contents")
	if a1 == v1 || v1 == c1 || a1 == c1 {
		t.Fatalf("one token served two scopes: %s %s %s", a1, v1, c1)
	}
	if a1 != a2 || v1 != v2 || c1 != c2 || f.minted != 3 {
		t.Errorf("a scope's token with an hour left is handed out again, not minted again: %d minted; %s %s %s / %s %s %s", f.minted, a1, v1, c1, a2, v2, c2)
	}
	if f.permReads != 1 {
		t.Errorf("the installation's permissions are read once and held briefly: %d reads", f.permReads)
	}
}

func TestAMissingPermissionIsRefusedByNameAndNoBroaderTokenIsAnswered(t *testing.T) {
	f := newFakeInstallation(t)
	f.perms = map[string]string{"actions": "write", "actions_variables": "read", "contents": "read"} // variables is held at read: write is missing
	h := adminWith(toolTokens(t, f))
	code, token, body := askScope(h, "variables")
	if code != http.StatusUnprocessableEntity || token != "" || strings.Contains(body, "ghs_") {
		t.Fatalf("%d %s", code, body)
	}
	if !strings.Contains(body, `"actions_variables: write"`) || !strings.Contains(body, "actions_variables: read") {
		t.Errorf("the refusal must name the permission asked for and the one held: %s", body)
	}
	if len(f.bodies) != 0 {
		t.Errorf("a token was minted for a scope the installation lacks, with %v", f.bodies)
	}
	// Another scope is untouched by the refusal, and the answer for it is its own token.
	if code, token, body := askScope(h, "contents"); code != 200 || token != "ghs_tool1" {
		t.Errorf("%d %s", code, body)
	}
	// An installation with no contents permission at all is refused for contents, and says so.
	f.perms = map[string]string{"actions": "read", "actions_variables": "write"}
	h = adminWith(toolTokens(t, f))
	if code, _, body := askScope(h, "contents"); code != http.StatusUnprocessableEntity || !strings.Contains(body, `"contents: read"`) || !strings.Contains(body, "no contents permission") {
		t.Errorf("%d %s", code, body)
	}
}

func TestAMintGitHubRefusesNamesThePermissionToo(t *testing.T) {
	f := newFakeInstallation(t) // its permissions cannot be read, so the narrowed mint is what refuses
	f.refuse = http.StatusUnprocessableEntity
	code, token, body := askScope(adminWith(toolTokens(t, f)), "variables")
	if code != http.StatusUnprocessableEntity || token != "" || !strings.Contains(body, `"actions_variables: write"`) || !strings.Contains(body, "permissions requested are not granted") {
		t.Errorf("%d %s", code, body)
	}
	if len(f.bodies) != 1 || narrowing(t, f.bodies[0]).Permissions["actions_variables"] != "write" || len(narrowing(t, f.bodies[0]).Permissions) != 1 {
		t.Errorf("even the mint GitHub refused was narrowed: %v", f.bodies)
	}
}

func TestAnUnknownScopeMintsNothing(t *testing.T) {
	f := newFakeInstallation(t)
	f.perms = map[string]string{"actions": "write", "administration": "write"}
	h := adminWith(toolTokens(t, f))
	for _, scope := range []string{"administration", "write", "actions,variables"} {
		if code, token, body := askScope(h, scope); code != http.StatusBadRequest || token != "" || f.minted != 0 {
			t.Errorf("scope %q: %d %s (%d minted)", scope, code, body, f.minted)
		}
	}
}

func TestNoScopeTokenOrKeyReachesTheLogOrARefusal(t *testing.T) {
	buf := logs(t)
	f := newFakeInstallation(t)
	f.perms = map[string]string{"actions": "read", "actions_variables": "write", "contents": "read"}
	h := adminWith(toolTokens(t, f))
	for _, scope := range []string{"actions", "variables", "contents"} {
		if code, token, _ := askScope(h, scope); code != 200 || token == "" {
			t.Fatalf("scope %s: %d", scope, code)
		}
	}
	f.perms = map[string]string{"actions": "read"}
	f.refuse = http.StatusUnprocessableEntity
	f.lives = 0
	for _, scope := range []string{"variables", "contents"} {
		if _, _, body := askScope(adminWith(toolTokens(t, f)), scope); strings.Contains(body, "ghs_") || strings.Contains(body, "BEGIN") {
			t.Errorf("the refusal of %s holds a token or a key: %s", scope, body)
		}
	}
	if strings.Contains(buf.String(), "ghs_") || strings.Contains(buf.String(), "BEGIN") {
		t.Errorf("the log holds a token or a key:\n%s", buf.String())
	}
	for _, scope := range []string{`scope=actions`, `scope=variables`, `scope=contents`} {
		if !strings.Contains(buf.String(), scope) {
			t.Errorf("the issue of %s is not logged:\n%s", scope, buf.String())
		}
	}
}

func TestAHungMintOfOneScopeNeverBlocksAnotherScopesHeldToken(t *testing.T) {
	f := newFakeInstallation(t)
	f.perms = map[string]string{"actions": "read", "actions_variables": "write", "contents": "read"}
	h := adminWith(toolTokens(t, f))
	if code, token, body := askScope(h, "contents"); code != 200 || token == "" {
		t.Fatalf("%d %s", code, body)
	}
	f.hold, f.holdPermission, f.holding = make(chan struct{}), "actions_variables", make(chan struct{}, 1)
	released := false
	release := func() {
		if !released {
			released = true
			close(f.hold)
		}
	}
	t.Cleanup(release)
	go askScope(h, "variables")
	<-f.holding // GitHub is now holding the variables mint
	answered := make(chan string, 1)
	go func() {
		_, token, _ := askScope(h, "contents")
		answered <- token
	}()
	select {
	case token := <-answered:
		if token == "" {
			t.Errorf("the held contents token was not answered")
		}
	case <-time.After(5 * time.Second):
		t.Errorf("a mint of the variables scope that GitHub has not answered blocks the contents scope's held token")
	}
	release()
}

func TestTwoAsksForOneScopeMintOnce(t *testing.T) {
	f := newFakeInstallation(t)
	f.perms = map[string]string{"actions": "read", "actions_variables": "write", "contents": "read"}
	f.hold, f.holdPermission, f.holding = make(chan struct{}), "actions_variables", make(chan struct{}, 2)
	h := adminWith(toolTokens(t, f))
	tokens := make(chan string, 2)
	go func() { _, tok, _ := askScope(h, "variables"); tokens <- tok }()
	<-f.holding
	go func() { _, tok, _ := askScope(h, "variables"); tokens <- tok }()
	close(f.hold)
	a, b := <-tokens, <-tokens
	if a == "" || a != b {
		t.Errorf("two asks for one scope: %q %q", a, b)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.minted != 1 {
		t.Errorf("the second ask waited for the first mint and was answered from it: %d minted", f.minted)
	}
}
