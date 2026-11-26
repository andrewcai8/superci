package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const secretToken = "AABBCC-registration-token-secret"

// fakeREST is GitHub's REST API as the identity endpoints use it, behind a real HTTP server.
type fakeREST struct {
	mu        sync.Mutex
	calls     []string // "METHOD path" in order
	bodies    map[string]string
	runners   []map[string]any // what the listings page through
	mintCode  int              // status of the registration token, 201 when 0
	groupList string
	srv       *httptest.Server
}

func newFakeREST(t *testing.T) *fakeREST {
	f := &fakeREST{bodies: map[string]string{}, groupList: `{"runner_groups":[]}`}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeREST) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, _ := io.ReadAll(r.Body)
	key := r.Method + " " + r.URL.Path
	f.calls = append(f.calls, key)
	f.bodies[key] = string(b)
	path := r.URL.Path
	switch {
	case strings.HasSuffix(path, "/registration-token"):
		if f.mintCode != 0 {
			w.WriteHeader(f.mintCode)
			return
		}
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"token":%q,"expires_at":"2026-11-27T01:00:00Z"}`, secretToken)
	case strings.HasPrefix(path, "/repos/o/") && r.Method == http.MethodGet && strings.Count(path, "/") == 3:
		fmt.Fprint(w, `{"id":42}`)
	case path == "/orgs/o/actions/runner-groups" && r.Method == http.MethodGet:
		fmt.Fprint(w, f.groupList)
	case path == "/orgs/o/actions/runner-groups" && r.Method == http.MethodPost:
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"id":77}`)
	case r.Method == http.MethodPatch:
		fmt.Fprint(w, `{}`)
	case r.Method == http.MethodPut && strings.HasSuffix(path, "/repositories"):
		w.WriteHeader(http.StatusNoContent)
	case strings.HasSuffix(path, "/labels") && r.Method == http.MethodPut:
		fmt.Fprint(w, `{"total_count":1,"labels":[]}`)
	case strings.HasSuffix(path, "/runners"):
		page := 1
		fmt.Sscan(r.URL.Query().Get("page"), &page)
		lo, hi := min((page-1)*100, len(f.runners)), min(page*100, len(f.runners))
		out, _ := json.Marshal(map[string]any{"total_count": len(f.runners), "runners": f.runners[lo:hi]})
		w.Write(out)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeREST) called(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

type toServer struct{ to *url.URL }

func (t toServer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.URL.Scheme, r.URL.Host = t.to.Scheme, t.to.Host
	return http.DefaultTransport.RoundTrip(r)
}

func (f *fakeREST) rest() *REST {
	u, _ := url.Parse(f.srv.URL)
	return &REST{org: "o", http: &http.Client{Transport: toServer{u}}, token: "installation", expires: time.Now().Add(time.Hour)}
}

// spare is a budget with about 9 tokens saved up and a reserve of 2, so a batch lease of more than 7 is granted less.
func spare() *Budget {
	c := reg()
	c.Reserve, c.CeilingBurst = 2, 9
	return NewBudget(c, time.Now().Add(-9*time.Second))
}

func idsAdmin(f *fakeREST, b *Budget, scope string) http.Handler {
	cfg := Config{Deployment: "dep", Repositories: []string{"r1", "r2"}, Scope: scope}
	return Admin(func() Status { return Status{} }, func() error { return nil }, func(string, int, int) {}, b, NewIdentities(f.rest(), b, cfg).Routes)
}

func do(h http.Handler, method, target string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(method, target, nil))
	return w
}

func logs(t *testing.T) *bytes.Buffer {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })
	return &buf
}

func TestATokenIsMintedForTheRegistrationsALeaseGrantedAndNoMore(t *testing.T) {
	f, b := newFakeREST(t), spare()
	h := idsAdmin(f, b, "")
	w := do(h, http.MethodPost, "/identities/token?n=50")
	var got IdentityToken
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if got.Granted < 1 || got.Granted >= 50 || got.Lease == "" || got.Token != secretToken || got.Expires.IsZero() {
		t.Fatalf("a lease of 50 on about 7 batch tokens: %+v", got)
	}
	if snap := b.Snapshot(time.Now()); snap.Leased != got.Granted {
		t.Errorf("the budget holds %d leased, the answer says %d granted: the lease is the rate bound", snap.Leased, got.Granted)
	}
	if got.URL != "https://github.com/o" || got.GroupName != "dep-identities" || got.Group != 77 {
		t.Errorf("registration target: %+v", got)
	}
	if w := do(h, http.MethodPost, "/lease/settle?id="+got.Lease+"&made=1"); w.Code != http.StatusNoContent {
		t.Errorf("the lease it took settles through /lease/settle: %d", w.Code)
	}
}

func TestALeaseOfNothingMintsNoTokenAndAsksGitHubNothing(t *testing.T) {
	f := newFakeREST(t)
	b := scarce(0)
	b.Done(time.Now(), Refused, 0) // GitHub just refused a registration: no lease is granted now
	w := do(idsAdmin(f, b, ""), http.MethodPost, "/identities/token?n=5")
	var got IdentityToken
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != 200 || got.Granted != 0 || got.Token != "" || got.Lease != "" {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if n := len(f.calls); n != 0 {
		t.Errorf("a lease of nothing made %d calls to GitHub: %v", n, f.calls)
	}
}

func TestTheTokenIsNeverLogged(t *testing.T) {
	buf := logs(t)
	f := newFakeREST(t)
	w := do(idsAdmin(f, spare(), ""), http.MethodPost, "/identities/token?n=3")
	if !strings.Contains(w.Body.String(), secretToken) {
		t.Fatalf("the caller gets no token: %s", w.Body.String())
	}
	if strings.Contains(buf.String(), secretToken) || !strings.Contains(buf.String(), "identities lease") {
		t.Errorf("the log (which must name the lease, not the token): %s", buf)
	}
	f.mintCode = http.StatusInternalServerError
	buf.Reset()
	do(idsAdmin(f, spare(), ""), http.MethodPost, "/identities/token?n=3")
	if strings.Contains(buf.String(), secretToken) {
		t.Errorf("a failed mint logged the token: %s", buf)
	}
}

func TestATokenThatCannotBeMintedGivesTheLeaseBack(t *testing.T) {
	f, b := newFakeREST(t), spare()
	f.mintCode = http.StatusForbidden
	w := do(idsAdmin(f, b, ""), http.MethodPost, "/identities/token?n=4")
	if w.Code != http.StatusBadGateway || strings.Contains(w.Body.String(), secretToken) {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if snap := b.Snapshot(time.Now()); snap.Leased != 0 || snap.Pending != 0 {
		t.Errorf("a lease held for the 10 minutes of its ttl for nothing: leased %d pending %d", snap.Leased, snap.Pending)
	}
}

func TestATokenCallIsBoundedToOneToAHundredRegistrations(t *testing.T) {
	f, b := newFakeREST(t), spare()
	h := idsAdmin(f, b, "")
	for _, n := range []string{"0", "101", "-1", "x", ""} {
		if w := do(h, http.MethodPost, "/identities/token?n="+n); w.Code != http.StatusBadRequest {
			t.Errorf("n=%q: %d", n, w.Code)
		}
	}
	if b.Snapshot(time.Now()).Leased != 0 || len(f.calls) != 0 {
		t.Error("a refused call took a lease or asked GitHub")
	}
}

func TestTheIdentitiesGroupIsOpenToTheConfigsRepositoriesAtOrganizationScope(t *testing.T) {
	f := newFakeREST(t)
	do(idsAdmin(f, spare(), "org"), http.MethodPost, "/identities/token?n=1")
	if f.called("POST /orgs/o/actions/runners/registration-token") != 1 {
		t.Errorf("org scope mints at the organization: %v", f.calls)
	}
	body := f.bodies["POST /orgs/o/actions/runner-groups"]
	for _, want := range []string{`"name":"dep-identities"`, `"visibility":"selected"`, `"allows_public_repositories":false`, `"selected_repository_ids":[42,42]`} {
		if !strings.Contains(body, want) {
			t.Errorf("the group made is %s, want %s", body, want)
		}
	}
	f2 := newFakeREST(t)
	f2.groupList = `{"runner_groups":[{"id":9,"name":"dep-identities"}]}`
	w := do(idsAdmin(f2, spare(), ""), http.MethodPost, "/identities/token?n=1")
	var got IdentityToken
	json.Unmarshal(w.Body.Bytes(), &got)
	if got.Group != 9 || f2.called("POST /orgs/o/actions/runner-groups") != 0 || f2.called("PUT /orgs/o/actions/runner-groups/9/repositories") != 1 {
		t.Errorf("an existing group is found and kept open to the repositories, not made again: %+v %v", got, f2.calls)
	}
}

func TestAtRepositoryScopeATokenIsTheRepositorysAndTheGroupIsTheDefault(t *testing.T) {
	f := newFakeREST(t)
	w := do(idsAdmin(f, spare(), "repository"), http.MethodPost, "/identities/token?n=1")
	var got IdentityToken
	json.Unmarshal(w.Body.Bytes(), &got)
	if got.Group != 1 || got.URL != "https://github.com/o/r1" || got.GroupName != "" {
		t.Errorf("%+v", got)
	}
	if f.called("POST /repos/o/r1/actions/runners/registration-token") != 1 || f.called("POST /orgs/") != 0 || f.called("GET /orgs/") != 0 {
		t.Errorf("a repository's token comes from its own endpoint, and no org group is made: %v", f.calls)
	}
}

func TestRelabellingReplacesOneRunnersLabelsAndMakesNoRegistration(t *testing.T) {
	f, b := newFakeREST(t), spare()
	h := idsAdmin(f, b, "")
	if w := do(h, http.MethodPost, "/identities/labels?id=123&labels=superci-park,extra"); w.Code != http.StatusNoContent {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if got := f.bodies["PUT /orgs/o/actions/runners/123/labels"]; strings.TrimSpace(got) != `{"labels":["superci-park","extra"]}` {
		t.Errorf("body %q", got)
	}
	if f.called("POST") != 0 || b.Snapshot(time.Now()).Leased != 0 {
		t.Errorf("a relabel registered something or took a lease: %v", f.calls)
	}
	do(idsAdmin(f, b, "repository"), http.MethodPost, "/identities/labels?id=5&labels=a")
	if f.called("PUT /repos/o/r1/actions/runners/5/labels") != 1 {
		t.Errorf("repository scope relabels at the repository: %v", f.calls)
	}
}

func TestARelabelOfNoIdOrNoLabelIsRefusedBeforeGitHubIsAsked(t *testing.T) {
	f := newFakeREST(t)
	h := idsAdmin(f, spare(), "")
	for _, q := range []string{"id=0&labels=a", "id=x&labels=a", "labels=a", "id=1", "id=1&labels=", "id=1&labels=a,,b", "id=1&labels=a,%20b"} {
		if w := do(h, http.MethodPost, "/identities/labels?"+q); w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", q, w.Code)
		}
	}
	if len(f.calls) != 0 {
		t.Errorf("asked GitHub: %v", f.calls)
	}
}

func TestTheListingPagesThroughAndKeepsOnlyTheIdentities(t *testing.T) {
	f := newFakeREST(t)
	f.groupList = `{"runner_groups":[{"id":9,"name":"dep-identities"},{"id":2,"name":"other"}]}`
	for i := 0; i < 130; i++ {
		f.runners = append(f.runners, map[string]any{"id": i + 1, "name": fmt.Sprintf("superci-id-2cpu-%04d", i), "status": "online", "busy": i%2 == 0,
			"labels": []map[string]any{{"name": "self-hosted"}, {"name": "superci-park"}}})
	}
	f.runners = append(f.runners, map[string]any{"id": 999, "name": "someone-else", "status": "online", "labels": []any{}})
	w := do(idsAdmin(f, spare(), ""), http.MethodGet, "/identities")
	var got struct {
		Group   int              `json:"group"`
		Runners []IdentityRunner `json:"runners"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if got.Group != 9 || len(got.Runners) != 130 || got.Runners[129].Name != "superci-id-2cpu-0129" || got.Runners[0].Labels[1] != "superci-park" || !got.Runners[0].Busy {
		t.Errorf("group %d, %d runners: %+v", got.Group, len(got.Runners), got.Runners[:1])
	}
	if f.called("GET /orgs/o/actions/runner-groups/9/runners") != 2 {
		t.Errorf("130 runners are 2 pages: %v", f.calls)
	}
	empty := newFakeREST(t)
	w = do(idsAdmin(empty, spare(), ""), http.MethodGet, "/identities")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"runners":[]`) {
		t.Errorf("no group yet is an empty list, not an error: %d %s", w.Code, w.Body.String())
	}
}

func TestTheConfigsScopeIsOrgOrRepository(t *testing.T) {
	load := func(scope string) error {
		p := filepath.Join(t.TempDir(), "o.json")
		if err := os.WriteFile(p, []byte(`{"scope":"`+scope+`"}`), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := LoadConfig(p)
		return err
	}
	for _, ok := range []string{"", "org", "repository"} {
		if err := load(ok); err != nil {
			t.Errorf("scope %q: %v", ok, err)
		}
	}
	if err := load("enterprise"); err == nil || !strings.Contains(err.Error(), "scope") {
		t.Errorf("scope enterprise: %v", err)
	}
}
