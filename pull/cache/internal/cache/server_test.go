package cache_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strings"
	"testing"
	"time"

	"superci-pull-cache/internal/cache"
	"superci-pull-cache/internal/cachetest"
)

const version = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

type rig struct {
	t      *testing.T
	signer *cachetest.Signer
	mem    *cachetest.Mem
	srv    *httptest.Server
	log    *bytes.Buffer
}

// Every request the actions client makes goes to rig.srv; the redirects a download answers with are not followed.
func newRig(t *testing.T) *rig {
	t.Helper()
	signer := cachetest.NewSigner("k1")
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write(signer.JWKS()) }))
	t.Cleanup(jwks.Close)
	mem := cachetest.NewMem()
	mem.MinPart = 4
	v := &cache.Verifier{JWKSURL: jwks.URL, Issuer: cache.GitHubIssuer}
	r := &rig{t: t, signer: signer, mem: mem, log: &bytes.Buffer{}}
	s := &cache.Server{Store: mem, Verify: v.Verify, DefaultBranch: "main", Log: log.New(r.log, "", 0)}
	r.srv = httptest.NewServer(s)
	s.Base = r.srv.URL
	t.Cleanup(r.srv.Close)
	return r
}

func (r *rig) token(repo string, scopes ...string) string {
	return r.signer.Mint(cache.GitHubIssuer, repo, cachetest.AC(scopes...), time.Now().Add(time.Hour))
}

func (r *rig) twirp(token, method string, body map[string]any) (int, map[string]any) {
	r.t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", r.srv.URL+"/twirp/github.actions.results.api.v1.CacheService/"+method, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func blockID(i int) string {
	return base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("a1b2c3d4-%06d", i)))
}

func (r *rig) put(u, query string, body []byte) int {
	r.t.Helper()
	req, _ := http.NewRequest("PUT", u+query, bytes.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// save does what actions/cache does with an archive in blocks of the given sizes ([]byte{...} blocks, or one
// block as a single Put Blob when single is set). It returns whether the entry was kept.
func (r *rig) save(token, key string, blocks [][]byte, single bool) bool {
	r.t.Helper()
	_, c := r.twirp(token, "CreateCacheEntry", map[string]any{"key": key, "version": version})
	if c["ok"] != true {
		return false
	}
	u := c["signed_upload_url"].(string)
	var all []byte
	if single {
		if st := r.put(u, "", blocks[0]); st != 201 {
			r.t.Fatalf("put blob: %d", st)
		}
		all = blocks[0]
	} else {
		var list strings.Builder
		list.WriteString(`<?xml version="1.0" encoding="utf-8"?><BlockList>`)
		for i, b := range blocks {
			id := blockID(i)
			if st := r.put(u, "?comp=block&blockid="+strings.ReplaceAll(url.PathEscape(id), "+", "+"), b); st != 201 {
				r.t.Fatalf("put block: %d", st)
			}
			list.WriteString("<Latest>" + id + "</Latest>")
			all = append(all, b...)
		}
		list.WriteString("</BlockList>")
		if st := r.put(u, "?comp=blocklist", []byte(list.String())); st != 201 {
			r.t.Fatalf("put block list: %d", st)
		}
	}
	_, f := r.twirp(token, "FinalizeCacheEntryUpload", map[string]any{"key": key, "version": version, "size_bytes": fmt.Sprint(len(all))})
	return f["ok"] == true
}

type restored struct {
	hit     bool
	matched string
	body    []byte
}

func (r *rig) restore(token, key string, restore ...string) restored {
	r.t.Helper()
	_, d := r.twirp(token, "GetCacheEntryDownloadURL", map[string]any{"key": key, "restore_keys": restore, "version": version})
	if d["ok"] != true {
		return restored{}
	}
	u := d["signed_download_url"].(string)
	head, _ := http.NewRequest("HEAD", u, nil)
	hr, err := http.DefaultClient.Do(head)
	if err != nil {
		r.t.Fatal(err)
	}
	hr.Body.Close()
	// The client's reads: a range at a time, as actions/cache does (4 MiB; here, 3 bytes).
	var body []byte
	for off := int64(0); off < hr.ContentLength; off += 3 {
		end := min(off+2, hr.ContentLength-1)
		get, _ := http.NewRequest("GET", u, nil)
		get.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", off, end))
		gr, err := http.DefaultClient.Do(get)
		if err != nil {
			r.t.Fatal(err)
		}
		part, _ := io.ReadAll(gr.Body)
		gr.Body.Close()
		if gr.StatusCode != 206 || gr.Header.Get("Content-Range") != fmt.Sprintf("bytes %d-%d/%d", off, end, hr.ContentLength) || int64(len(part)) != end-off+1 {
			r.t.Fatalf("range %d-%d: status %d, Content-Range %q, %d bytes", off, end, gr.StatusCode, gr.Header.Get("Content-Range"), len(part))
		}
		body = append(body, part...)
	}
	whole, _ := http.Get(u)
	all, _ := io.ReadAll(whole.Body)
	whole.Body.Close()
	if whole.StatusCode != 200 || !bytes.Equal(all, body) {
		r.t.Fatalf("a read with no range: status %d, %d bytes", whole.StatusCode, len(all))
	}
	return restored{hit: true, matched: d["matched_key"].(string), body: body}
}

func TestSaveAndRestoreInBlocks(t *testing.T) {
	r := newRig(t)
	tok := r.token("77", "refs/heads/main:rw")
	if !r.save(tok, "bun-abc", [][]byte{[]byte("aaaa"), []byte("bbbb"), []byte("cc")}, false) {
		t.Fatalf("not saved:\n%s", r.log)
	}
	got := r.restore(tok, "bun-abc")
	if !got.hit || string(got.body) != "aaaabbbbcc" || got.matched != "bun-abc" {
		t.Fatalf("restore = %+v", got)
	}
	if keys := r.mem.Keys("v1/"); len(keys) != 1 || keys[0] != "v1/d/77/refs%2Fheads%2Fmain/"+version+"/bun-abc" {
		t.Fatalf("the entry is at %v", keys)
	}
	// (The blocks are deleted off the request's goroutine.)
	for end := time.Now().Add(2 * time.Second); len(r.mem.Keys("u/")) != 0 && time.Now().Before(end); {
		runtime.Gosched()
	}
	if left := r.mem.Keys("u/"); len(left) != 0 {
		t.Fatalf("blocks left behind: %v", left)
	}
}

func TestPutBlobSingleShot(t *testing.T) {
	r := newRig(t)
	tok := r.token("77", "refs/heads/main:rw")
	if !r.save(tok, "k", [][]byte{[]byte("small archive")}, true) {
		t.Fatalf("not saved:\n%s", r.log)
	}
	if got := r.restore(tok, "k"); string(got.body) != "small archive" {
		t.Fatalf("restore = %+v", got)
	}
}

func TestPullRequestReadsMainAndCannotOverwriteIt(t *testing.T) {
	r := newRig(t)
	main := r.token("77", "refs/heads/main:rw")
	pr := r.token("77", "refs/pull/7/merge:rw", "refs/heads/main:r")
	if !r.save(main, "deps", [][]byte{[]byte("from main")}, true) {
		t.Fatal("main could not save")
	}
	if got := r.restore(pr, "deps"); !got.hit || string(got.body) != "from main" {
		t.Fatalf("the pull request does not read main's entry: %+v", got)
	}
	// It saves the same key: into its own scope, not over main's.
	if !r.save(pr, "deps", [][]byte{[]byte("from the pull request")}, true) {
		t.Fatalf("the pull request could not save into its own scope:\n%s", r.log)
	}
	mainKey := "v1/d/77/refs%2Fheads%2Fmain/" + version + "/deps"
	if b, _ := r.mem.Get(mainKey); string(b) != "from main" {
		t.Fatalf("main's entry became %q", b)
	}
	if got := r.restore(main, "deps"); string(got.body) != "from main" {
		t.Fatalf("main now reads %q", got.body)
	}
	if got := r.restore(pr, "deps"); string(got.body) != "from the pull request" {
		t.Fatalf("the pull request reads %q: its own scope comes first", got.body)
	}
	// A second save of the same key in the same scope is turned away, as GitHub does.
	if r.save(pr, "deps", [][]byte{[]byte("again")}, true) {
		t.Fatal("an entry was written twice")
	}
}

func TestScopesDoNotLeak(t *testing.T) {
	r := newRig(t)
	pr7 := r.token("77", "refs/pull/7/merge:rw", "refs/heads/main:r")
	pr8 := r.token("77", "refs/pull/8/merge:rw", "refs/heads/main:r")
	other := r.token("78", "refs/pull/7/merge:rw", "refs/heads/main:r")
	if !r.save(pr7, "mine", [][]byte{[]byte("seven")}, true) {
		t.Fatal("not saved")
	}
	if r.restore(pr8, "mine").hit {
		t.Fatal("pull request 8 read pull request 7's entry")
	}
	if r.restore(other, "mine").hit {
		t.Fatal("another repository read the entry")
	}
	if !r.restore(pr7, "mine").hit {
		t.Fatal("pull request 7 lost its own entry")
	}
}

func TestReadOnlyTokenCannotSave(t *testing.T) {
	r := newRig(t)
	forkPR := r.token("77", "refs/heads/main:r")
	_, c := r.twirp(forkPR, "CreateCacheEntry", map[string]any{"key": "k", "version": version})
	if c["ok"] != false || !strings.HasPrefix(fmt.Sprint(c["message"]), "cache write denied:") {
		t.Fatalf("answer = %v", c)
	}
	if keys := r.mem.Keys(""); len(keys) != 0 {
		t.Fatalf("something was written: %v", keys)
	}
}

func TestForgedTokensAreRefused(t *testing.T) {
	r := newRig(t)
	good := r.token("77", "refs/heads/main:rw")
	other := cachetest.NewSigner("k1").Mint(cache.GitHubIssuer, "77", cachetest.AC("refs/heads/main:rw"), time.Now().Add(time.Hour))
	wrongIss := r.signer.Mint("https://evil.example", "77", cachetest.AC("refs/heads/main:rw"), time.Now().Add(time.Hour))
	expired := r.signer.Mint(cache.GitHubIssuer, "77", cachetest.AC("refs/heads/main:rw"), time.Now().Add(-time.Hour))
	parts := strings.Split(good, ".")
	widened := parts[0] + "." + base64.RawURLEncoding.EncodeToString([]byte(`{"iss":"`+cache.GitHubIssuer+`","repository_id":"77","ac":"[{\"Scope\":\"refs/heads/main\",\"Permission\":3}]","exp":9999999999}`)) + "." + parts[2]
	none := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`)) + "." + parts[1] + "."
	noExpiry := r.signer.Sign(map[string]any{"iss": cache.GitHubIssuer, "repository_id": "77", "ac": cachetest.AC("refs/heads/main:rw")})
	for name, tok := range map[string]string{"another key": other, "another issuer": wrongIss, "expired": expired, "claims changed after signing": widened, "no algorithm": none, "no expiry": noExpiry, "not a token": "x", "none": ""} {
		if code, _ := r.twirp(tok, "CreateCacheEntry", map[string]any{"key": "k", "version": version}); code != 401 {
			t.Errorf("%s: status %d, want 401", name, code)
		}
	}
}

func TestRestoreKeysTakeTheNewestInTheFirstScopeThatHasOne(t *testing.T) {
	r := newRig(t)
	pr := r.token("77", "refs/pull/7/merge:rw", "refs/heads/main:r")
	base := time.Now()
	put := func(scope, class, key string, age time.Duration, body string) {
		r.mem.Set("v1/"+class+"/77/"+strings.ReplaceAll(scope, "/", "%2F")+"/"+version+"/"+key, []byte(body), base.Add(-age))
	}
	put("refs/heads/main", "d", "Linux-go-1", 3*time.Hour, "main old")
	put("refs/heads/main", "d", "Linux-go-2", time.Hour, "main new")
	put("refs/heads/main", "d", "Linux-go-1x", 5*time.Hour, "main older")
	if got := r.restore(pr, "Linux-go-9", "Linux-go-"); got.matched != "Linux-go-2" || string(got.body) != "main new" {
		t.Fatalf("restore = %+v: the newest of main's matches", got)
	}
	put("refs/pull/7/merge", "r", "Linux-go-0", 10*time.Hour, "own old")
	if got := r.restore(pr, "Linux-go-9", "Linux-go-"); got.matched != "Linux-go-0" {
		t.Fatalf("restore = %+v: the job's own scope is read first, even for an older entry", got)
	}
	if got := r.restore(pr, "Linux-go-1"); got.matched != "Linux-go-1" || string(got.body) != "main old" {
		t.Fatalf("restore = %+v: a key is itself, not a prefix, when it is there", got)
	}
	if got := r.restore(pr, "Linux-rust-1", "Linux-zig-"); got.hit {
		t.Fatalf("restore = %+v: nothing matches", got)
	}
}

func TestKeysWithSlashesAndSpacesKeepTheirOwnNames(t *testing.T) {
	r := newRig(t)
	tok := r.token("77", "refs/heads/main:rw")
	key := "gradle caches/Linux 100% x"
	if !r.save(tok, key, [][]byte{[]byte("ok")}, true) {
		t.Fatalf("not saved:\n%s", r.log)
	}
	if got := r.restore(tok, "gradle caches/Mac", "gradle caches/Linux "); got.matched != key {
		t.Fatalf("restore by prefix = %+v", got)
	}
}

func TestSizeMismatchAndRacesAreNotSaves(t *testing.T) {
	r := newRig(t)
	tok := r.token("77", "refs/heads/main:rw")
	_, c := r.twirp(tok, "CreateCacheEntry", map[string]any{"key": "k", "version": version})
	u := c["signed_upload_url"].(string)
	r.put(u, "", []byte("12345"))
	if _, f := r.twirp(tok, "FinalizeCacheEntryUpload", map[string]any{"key": "k", "version": version, "size_bytes": "99"}); f["ok"] != false {
		t.Fatal("an entry of the wrong size was kept")
	}
	if r.restore(tok, "k").hit {
		t.Fatal("a refused entry is a hit")
	}
	// Two jobs save one key at once: both begin, one finishes first, the other's save is turned away.
	_, a := r.twirp(tok, "CreateCacheEntry", map[string]any{"key": "race", "version": version})
	_, b := r.twirp(tok, "CreateCacheEntry", map[string]any{"key": "race", "version": version})
	r.put(a["signed_upload_url"].(string), "", []byte("first"))
	r.put(b["signed_upload_url"].(string), "", []byte("second"))
	fin := map[string]any{"key": "race", "version": version, "size_bytes": "5"}
	_, fa := r.twirp(tok, "FinalizeCacheEntryUpload", fin)
	fin["size_bytes"] = "6"
	_, fb := r.twirp(tok, "FinalizeCacheEntryUpload", fin)
	if fa["ok"] != true || fb["ok"] != false {
		t.Fatalf("finishes: %v %v", fa, fb)
	}
	if got := r.restore(tok, "race"); string(got.body) != "first" {
		t.Fatalf("the entry is %q", got.body)
	}
}

func TestWhatIsNotTheCacheGoesToGitHub(t *testing.T) {
	var seen string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		seen = req.Method + " " + req.URL.Path + " " + req.Header.Get("Authorization") + " " + string(b)
		w.WriteHeader(201)
	}))
	defer up.Close()
	u, _ := url.Parse(up.URL)
	s := &cache.Server{Store: cachetest.NewMem(), Verify: func(string) (cache.Access, error) { return cache.Access{}, fmt.Errorf("no") }, Upstream: u}
	front := httptest.NewServer(s)
	defer front.Close()
	req, _ := http.NewRequest("POST", front.URL+"/twirp/github.actions.results.api.v1.ArtifactService/CreateArtifact", strings.NewReader(`{"name":"x"}`))
	req.Header.Set("Authorization", "Bearer t")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 201 || seen != `POST /twirp/github.actions.results.api.v1.ArtifactService/CreateArtifact Bearer t {"name":"x"}` {
		t.Fatalf("status %d, GitHub saw %q", resp.StatusCode, seen)
	}
}

func TestTheBucketFailingIsAMissNotAFailedJob(t *testing.T) {
	r := newRig(t)
	tok := r.token("77", "refs/heads/main:rw")
	r.srv.Config.Handler.(*cache.Server).Store = failing{}
	_, d := r.twirp(tok, "GetCacheEntryDownloadURL", map[string]any{"key": "k", "version": version})
	_, c := r.twirp(tok, "CreateCacheEntry", map[string]any{"key": "k", "version": version})
	if d["ok"] != false || c["ok"] != false {
		t.Fatalf("answers: %v %v", d, c)
	}
}

func TestRangesAsTheClientAsksThem(t *testing.T) {
	r := newRig(t)
	tok := r.token("77", "refs/heads/main:rw")
	r.save(tok, "k", [][]byte{[]byte("0123456789")}, true)
	_, d := r.twirp(tok, "GetCacheEntryDownloadURL", map[string]any{"key": "k", "version": version})
	u := d["signed_download_url"].(string)
	for _, c := range []struct {
		header, want string
		status       int
	}{{"bytes=2-4", "234", 206}, {"bytes=8-", "89", 206}, {"bytes=7-99", "789", 206}, {"bytes=-3", "789", 206}, {"bytes=10-12", "", 416}, {"bytes=4-2", "", 416}, {"bytes=0-1,3-4", "", 416}, {"items=1-2", "", 416}} {
		req, _ := http.NewRequest("GET", u, nil)
		req.Header.Set("Range", c.header)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != c.status || c.status == 206 && string(got) != c.want {
			t.Errorf("%s: status %d, %q; want %d, %q", c.header, resp.StatusCode, got, c.status, c.want)
		}
	}
}

// A job's writes go through a writer for its own grant: a pull request's save is made with credentials that can
// reach only its own scope's entries and its own staged blocks, never main's.
func TestAJobWritesWithItsOwnScopesGrantOnly(t *testing.T) {
	r := newRig(t)
	pr := r.token("77", "refs/pull/7/merge:rw", "refs/heads/main:r")
	if !r.save(pr, "deps", [][]byte{[]byte("aaaa"), []byte("bb")}, false) {
		t.Fatalf("not saved:\n%s", r.log)
	}
	want := cache.Grant{Entries: "v1/r/77/refs%2Fpull%2F7%2Fmerge/", Stage: "u/77/refs%2Fpull%2F7%2Fmerge/"}
	if len(r.mem.Granted) != 1 || r.mem.Granted[0] != want {
		t.Fatalf("writers made for %+v, want one for %+v", r.mem.Granted, want)
	}
	if keys := r.mem.Keys("v1/"); len(keys) != 1 || !strings.HasPrefix(keys[0], want.Entries) {
		t.Fatalf("the entry is at %v", keys)
	}
}

func TestNoWriterNoSave(t *testing.T) {
	r := newRig(t)
	tok := r.token("77", "refs/heads/main:rw")
	srv := r.srv.Config.Handler.(*cache.Server)
	srv.Store = noWriter{r.mem}
	if r.save(tok, "k", [][]byte{[]byte("x")}, true) {
		t.Fatal("saved without credentials to write with")
	}
	if keys := r.mem.Keys(""); len(keys) != 0 {
		t.Fatalf("written: %v", keys)
	}
}

type noWriter struct{ *cachetest.Mem }

func (noWriter) Writer(context.Context, string, cache.Grant) (cache.Writer, error) {
	return nil, fmt.Errorf("the broker refused")
}

func TestRefusalsNameTheTokensIssuerAndKey(t *testing.T) {
	r := newRig(t)
	wrongIss := r.signer.Mint("https://vstoken.example", "77", cachetest.AC("refs/heads/main:rw"), time.Now().Add(time.Hour))
	r.twirp(wrongIss, "CreateCacheEntry", map[string]any{"key": "k", "version": version})
	if !strings.Contains(r.log.String(), `iss "https://vstoken.example", kid "k1"`) {
		t.Fatalf("the refusal does not name the token's issuer and key:\n%s", r.log)
	}
	if strings.Contains(r.log.String(), wrongIss) {
		t.Fatal("the token itself was logged")
	}
}

func TestAVerifierWithoutAnIssuerTakesNoToken(t *testing.T) {
	signer := cachetest.NewSigner("k1")
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write(signer.JWKS()) }))
	defer jwks.Close()
	v := &cache.Verifier{JWKSURL: jwks.URL}
	if _, err := v.Verify(signer.Mint("", "77", cachetest.AC("refs/heads/main:rw"), time.Now().Add(time.Hour))); err == nil {
		t.Fatal("a verifier with no issuer took a token with none")
	}
	v.Issuer = cache.GitHubIssuer
	if _, err := v.Verify(signer.Mint(cache.GitHubIssuer, "77", cachetest.AC("refs/heads/main:rw"), time.Now().Add(time.Hour))); err != nil {
		t.Fatalf("a good token was refused: %v", err)
	}
}
