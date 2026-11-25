package cache

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	service  = "/twirp/github.actions.results.api.v1.CacheService/"
	uploadAt = "/pullcache/u/"
	loadAt   = "/pullcache/d/"
	// A block larger than this is refused (the actions client sends 32 MiB blocks and a 128 MiB single shot).
	maxBlock = 512 << 20
	// A restore key's listing is read to this many objects.
	maxListed = 5000
)

// Server speaks GitHub's cache service to the job's actions and keeps what they save in Store. What is not the
// cache (artifacts, logs) goes on to GitHub's results service unchanged.
type Server struct {
	Store         Store
	Verify        func(token string) (Access, error)
	DefaultBranch string
	// Base is the address the job's actions are told to upload to and download from (http://<private ip>:<port>).
	Base     string
	Upstream *url.URL
	Log      *log.Logger
	Now      func() time.Time
	// Mark, when set, is told each restore and save, for the job timeline (SpoolMarks).
	Mark func(name string, ms int64, kind string, bytes int64, key string)
	// ReadChunk and ReadAhead set how a large read of an entry is split (see stream); zero is the default.
	ReadChunk int64
	ReadAhead int
	// Uses, when set, is told of each entry a restore returns, so that the janitor can tell a used entry from one
	// that only sits there.
	Uses *Uses

	proxy     *httputil.ReverseProxy
	mu        sync.Mutex
	uploads   map[string]*upload
	downloads map[string]*download
}

type blockInfo struct {
	key  string
	size int64
}

// An entry being saved: where it will be, how it is written, and its blocks so far (by the id the client's SDK
// gave each).
type upload struct {
	mu      sync.Mutex
	access  Access
	w       Writer
	stage   string
	final   string
	key     string
	version string
	blocks  map[string]blockInfo
	order   []string
	began   time.Time
	first   time.Time
	last    time.Time
	done    bool
}

// A download is of one version of an entry: every range of it is read with that version's ETag, so an entry
// replaced while it is read ends the download short instead of sending the bytes of two archives.
type download struct {
	obj  Object
	name string
	made time.Time

	mu       sync.Mutex
	sent     int64
	first    time.Time
	logged   bool
	ranges   int
	inflight int
	peak     int
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Server) logf(format string, a ...any) {
	if s.Log != nil {
		s.Log.Printf(format, a...)
	}
}

func (s *Server) init() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.uploads != nil {
		return
	}
	s.uploads, s.downloads = map[string]*upload{}, map[string]*download{}
	if s.Upstream != nil {
		up := s.Upstream
		s.proxy = &httputil.ReverseProxy{
			Rewrite: func(r *httputil.ProxyRequest) {
				r.SetURL(up)
				r.Out.Host = up.Host
			},
			ErrorLog: s.Log,
		}
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.init()
	switch p := r.URL.Path; {
	case strings.HasPrefix(p, loadAt):
		s.serveDownload(w, r, strings.TrimPrefix(p, loadAt))
	case strings.HasPrefix(p, uploadAt):
		s.serveBlob(w, r, strings.TrimPrefix(p, uploadAt))
	case strings.HasPrefix(p, service):
		s.serveTwirp(w, r, strings.TrimPrefix(p, service))
	case s.proxy != nil:
		s.proxy.ServeHTTP(w, r)
	default:
		http.NotFound(w, r)
	}
}

func randomID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func twirpError(w http.ResponseWriter, status int, code, msg string) {
	reply(w, status, map[string]string{"code": code, "msg": msg})
}

type twirpRequest struct {
	Key          string          `json:"key"`
	Version      string          `json:"version"`
	RestoreKeys  []string        `json:"restore_keys"`
	RestoreCamel []string        `json:"restoreKeys"`
	SizeBytes    json.RawMessage `json:"size_bytes"`
	SizeCamel    json.RawMessage `json:"sizeBytes"`
}

func (t twirpRequest) restore() []string {
	if len(t.RestoreKeys) > 0 {
		return t.RestoreKeys
	}
	return t.RestoreCamel
}

func (t twirpRequest) size() (int64, bool) {
	raw := t.SizeBytes
	if len(raw) == 0 {
		raw = t.SizeCamel
	}
	n, err := strconv.ParseInt(strings.Trim(string(raw), `"`), 10, 64)
	return n, err == nil
}

func bearer(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}

func (s *Server) serveTwirp(w http.ResponseWriter, r *http.Request, method string) {
	if r.Method != http.MethodPost {
		twirpError(w, 405, "bad_route", "POST only")
		return
	}
	var req twirpRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		twirpError(w, 400, "malformed", "the request is not JSON")
		return
	}
	access, err := s.Verify(bearer(r))
	if err != nil {
		s.logf("%s %q: refused: %v", method, req.Key, err)
		if errors.Is(err, ErrUnavailable) {
			twirpError(w, 503, "unavailable", err.Error())
		} else {
			twirpError(w, 401, "unauthenticated", err.Error())
		}
		return
	}
	switch method {
	case "GetCacheEntryDownloadURL":
		s.restore(w, r.Context(), access, req)
	case "CreateCacheEntry":
		s.begin(w, r.Context(), bearer(r), access, req)
	case "FinalizeCacheEntryUpload":
		s.finish(w, r.Context(), access, req)
	default:
		twirpError(w, 404, "bad_route", "not a cache method")
	}
}

// restore: GitHub's order, scope by scope in the token's order: the key itself, then each restore key as the start
// of a key (the newest such entry), before the next scope. An entry of another version never matches.
func (s *Server) restore(w http.ResponseWriter, ctx context.Context, a Access, req twirpRequest) {
	miss := func() { reply(w, 200, map[string]any{"ok": false}) }
	if !okKey(req.Key) || !okVersion(req.Version) {
		miss()
		return
	}
	obj, matched, found, err := s.lookup(ctx, a, req.Key, req.restore(), req.Version)
	if err != nil {
		s.logf("restore %q: the bucket could not be read, a miss: %v", req.Key, err)
		miss()
		return
	}
	if !found {
		s.logf("restore %q: not found", req.Key)
		s.mark("cache.restore", 0, "miss", 0, req.Key)
		miss()
		return
	}
	id := randomID()
	s.mu.Lock()
	s.gc()
	s.downloads[id] = &download{obj: obj, name: matched, made: s.now()}
	s.mu.Unlock()
	if s.Uses != nil {
		s.Uses.Hit(obj.Key)
	}
	s.logf("restore %q: found %q (%d MB)", req.Key, matched, obj.Size>>20)
	reply(w, 200, map[string]any{"ok": true, "signed_download_url": s.Base + loadAt + id, "matched_key": matched})
}

func (s *Server) lookup(ctx context.Context, a Access, key string, restore []string, version string) (obj Object, matched string, found bool, err error) {
	for _, sc := range a.Scopes {
		if k := ObjectKey(a.Repo, sc, version, key, s.DefaultBranch); k != "" {
			o, ok, err := s.Store.Head(ctx, k)
			if err != nil {
				return Object{}, "", false, err
			}
			if ok {
				return o, key, true, nil
			}
		}
		base := ScopePrefix(a.Repo, sc, version, s.DefaultBranch)
		for _, prefix := range restore {
			if !okKey(prefix) {
				continue
			}
			objs, err := s.Store.List(ctx, base+esc(prefix), maxListed)
			if err != nil {
				return Object{}, "", false, err
			}
			newest, ok, err := Newest(ctx, s.Store, objs)
			if err != nil {
				return Object{}, "", false, err
			}
			if ok {
				name, err := url.PathUnescape(strings.TrimPrefix(newest.Key, base))
				if err != nil {
					continue
				}
				return newest, name, true, nil
			}
		}
	}
	return Object{}, "", false, nil
}

// begin starts a save in the job's own scope, written with credentials for that scope alone. A scope the token
// cannot write to is refused as GitHub's service says it ("cache write denied:", which actions/cache v5 and later
// tell apart from a key being saved already).
func (s *Server) begin(w http.ResponseWriter, ctx context.Context, token string, a Access, req twirpRequest) {
	sc, _ := a.Writable()
	g, ok := GrantFor(a, s.DefaultBranch)
	if !ok {
		s.logf("save %q: denied: the token has no scope it may write", req.Key)
		reply(w, 200, map[string]any{"ok": false, "message": "cache write denied: this job's token can read the cache, not write it"})
		return
	}
	final := ObjectKey(a.Repo, sc, req.Version, req.Key, s.DefaultBranch)
	if final == "" {
		s.logf("save %q: not taken (the key or version)", req.Key)
		reply(w, 200, map[string]any{"ok": false})
		return
	}
	_, there, err := s.Store.Head(ctx, final)
	if err != nil {
		s.logf("save %q: the bucket could not be read, not saved: %v", req.Key, err)
		reply(w, 200, map[string]any{"ok": false})
		return
	}
	if there {
		s.logf("save %q: there already in %s", req.Key, sc.Name)
		reply(w, 200, map[string]any{"ok": false})
		return
	}
	wr, err := s.Store.Writer(ctx, token, g)
	if err != nil {
		s.logf("save %q: no credentials to write with, not saved: %v", req.Key, err)
		reply(w, 200, map[string]any{"ok": false})
		return
	}
	id := randomID()
	s.mu.Lock()
	s.gc()
	s.uploads[id] = &upload{access: a, w: wr, stage: g.Stage + id + "/", final: final, key: req.Key, version: req.Version, blocks: map[string]blockInfo{}, began: s.now()}
	s.mu.Unlock()
	s.logf("save %q: begun in %s", req.Key, sc.Name)
	reply(w, 200, map[string]any{"ok": true, "signed_upload_url": s.Base + uploadAt + id})
}

// gc drops what a job left behind (called with mu held).
func (s *Server) gc() {
	cut := s.now().Add(-12 * time.Hour)
	for id, u := range s.uploads {
		if u.began.Before(cut) {
			delete(s.uploads, id)
		}
	}
	for id, d := range s.downloads {
		if d.made.Before(cut) {
			delete(s.downloads, id)
		}
	}
}

// findUpload is the save a Finalize is for. Jobs on one machine do not often save one key twice at once; when they
// do, the one whose blocks add up to the size the client says is the one.
func (s *Server) findUpload(a Access, key, version string, size int64, sized bool) (string, *upload) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var pickID string
	var pick *upload
	for id, u := range s.uploads {
		if u.done || u.key != key || u.version != version || u.access.Repo != a.Repo {
			continue
		}
		if sized && u.total() == size {
			return id, u
		}
		if pick == nil || u.began.After(pick.began) {
			pickID, pick = id, u
		}
	}
	return pickID, pick
}

// total is the bytes of the blocks its list names (u.mu is taken).
func (u *upload) total() (n int64) {
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, b := range u.order {
		n += u.blocks[b].size
	}
	return n
}

func (s *Server) finish(w http.ResponseWriter, ctx context.Context, a Access, req twirpRequest) {
	no := func() { reply(w, 200, map[string]any{"ok": false}) }
	said, sized := req.size()
	id, u := s.findUpload(a, req.Key, req.Version, said, sized)
	if u == nil {
		s.logf("save %q: finished before its upload", req.Key)
		no()
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.order) == 0 {
		s.logf("save %q: finished without its blocks", req.Key)
		no()
		return
	}
	parts := make([]Part, 0, len(u.order))
	var total int64
	for _, b := range u.order {
		bi, ok := u.blocks[b]
		if !ok {
			s.logf("save %q: its list names a block never uploaded", req.Key)
			no()
			return
		}
		parts = append(parts, Part{Key: bi.key, Size: bi.size})
		total += bi.size
	}
	if sized && said != total {
		s.logf("save %q: %d bytes uploaded, %d said", req.Key, total, said)
		no()
		return
	}
	began := s.now()
	err := u.w.Assemble(ctx, u.final, parts)
	switch {
	case errors.Is(err, ErrExists):
		s.logf("save %q: another job saved it first", req.Key)
		no()
	case err != nil:
		s.logf("save %q: not saved: %v", req.Key, err)
		no()
	default:
		u.done = true
		wire := u.last.Sub(u.first).Seconds()
		s.logf("save %q: saved %d MB; blocks in %.1f s (%.0f MB/s), joined in %.1f s", req.Key, total>>20, wire, rate(total, wire), s.now().Sub(began).Seconds())
		s.mark("cache.save", int64((wire+s.now().Sub(began).Seconds())*1000), "saved", total, req.Key)
		reply(w, 200, map[string]any{"ok": true, "entry_id": "1"})
	}
	// The blocks are of no use now, whichever way it ended. (Off this goroutine: it holds u.mu, and findUpload takes
	// s.mu and then u.mu.)
	staged := make([]string, 0, len(u.blocks))
	for _, b := range u.blocks {
		staged = append(staged, b.key)
	}
	go func() {
		c, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		_ = u.w.Delete(c, staged)
		s.mu.Lock()
		delete(s.uploads, id)
		s.mu.Unlock()
	}()
}

func rate(n int64, secs float64) float64 {
	if secs < 0.001 {
		secs = 0.001
	}
	return float64(n) / 1e6 / secs
}

// serveDownload answers the client's reads of an entry: its size by HEAD, then GETs of 4 MiB ranges, ten at a time.
// The bytes come through here, not by a redirect to the bucket: the client's HTTP agent, once made for an http
// address, cannot follow a redirect to an https one.
func (s *Server) serveDownload(w http.ResponseWriter, r *http.Request, id string) {
	s.mu.Lock()
	d := s.downloads[id]
	s.mu.Unlock()
	if d == nil {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Accept-Ranges", "bytes")
	h.Set("Content-Type", "application/octet-stream")
	size := d.obj.Size
	start, end, partial, ok := byteRange(r.Header.Get("Range"), size)
	if !ok {
		h.Set("Content-Range", fmt.Sprintf("bytes */%d", size))
		http.Error(w, "a range outside the entry", http.StatusRequestedRangeNotSatisfiable)
		return
	}
	h.Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	if partial {
		h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
	}
	status := http.StatusOK
	if partial {
		status = http.StatusPartialContent
	}
	switch r.Method {
	case http.MethodHead:
		w.WriteHeader(status)
	case http.MethodGet:
		if size == 0 {
			w.WriteHeader(status)
			return
		}
		d.mu.Lock()
		d.inflight++
		d.peak = max(d.peak, d.inflight)
		d.mu.Unlock()
		began := s.now()
		w.WriteHeader(status)
		n, err := s.stream(r.Context(), w, d.obj, start, end)
		if err != nil {
			s.logf("restore %q: cut short after %d of %d bytes: %v", d.name, n, end-start+1, err)
		}
		s.served(d, began, n)
	default:
		http.Error(w, "GET or HEAD", http.StatusMethodNotAllowed)
	}
}

// served counts the bytes a download has sent and says, once they are all of the entry, how fast they went.
func (s *Server) served(d *download, began time.Time, n int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.first.IsZero() || began.Before(d.first) {
		d.first = began
	}
	d.inflight--
	d.ranges++
	d.sent += n
	if size := d.obj.Size; d.sent >= size && !d.logged {
		d.logged = true
		secs := s.now().Sub(d.first).Seconds()
		s.logf("restore %q: sent %d MB in %.1f s (%.0f MB/s) as %d reads, at most %d at once", d.name, size>>20, secs, rate(size, secs), d.ranges, d.peak)
		s.mark("cache.restore", int64(secs*1000), "hit", size, d.name)
	}
}

// byteRange reads a Range header of one range ("bytes=a-b", "bytes=a-", "bytes=-n") against an entry of size
// bytes: the first and last byte, whether it is a part. None is the whole entry.
func byteRange(h string, size int64) (start, end int64, partial, ok bool) {
	if h == "" {
		return 0, size - 1, false, true
	}
	spec, found := strings.CutPrefix(h, "bytes=")
	a, b, dash := strings.Cut(spec, "-")
	if !found || !dash || strings.Contains(spec, ",") {
		return 0, 0, false, false
	}
	switch {
	case a == "":
		n, err := strconv.ParseInt(b, 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, false, false
		}
		return max(size-n, 0), size - 1, true, size > 0
	default:
		from, err := strconv.ParseInt(a, 10, 64)
		if err != nil || from < 0 || from >= size {
			return 0, 0, false, false
		}
		to := size - 1
		if b != "" {
			if to, err = strconv.ParseInt(b, 10, 64); err != nil || to < from {
				return 0, 0, false, false
			}
			to = min(to, size-1)
		}
		return from, to, true, true
	}
}

func azureHeaders(w http.ResponseWriter, now time.Time) {
	h := w.Header()
	h.Set("x-ms-request-id", randomID()[:8]+"-0000-4000-8000-"+randomID())
	h.Set("x-ms-version", "2023-11-03")
	h.Set("Date", now.UTC().Format(http.TimeFormat))
	h.Set("Last-Modified", now.UTC().Format(http.TimeFormat))
	h.Set("ETag", fmt.Sprintf(`"0x%X"`, now.UnixNano()))
	h.Set("x-ms-request-server-encrypted", "true")
}

// rawQuery reads a query as Azure's SDK writes it: percent-escapes undone, a "+" left as it is (block ids are
// base64, which has them).
func rawQuery(q string) map[string]string {
	out := map[string]string{}
	for _, kv := range strings.Split(q, "&") {
		k, v, _ := strings.Cut(kv, "=")
		if k, err := url.PathUnescape(k); err == nil {
			if v, err := url.PathUnescape(v); err == nil {
				out[k] = v
			}
		}
	}
	return out
}

// serveBlob is the part of Azure's Blob service that actions/cache's upload uses: Put Blob (a small archive in one
// request), Put Block (one block of a larger one) and Put Block List (the blocks, in order).
func (s *Server) serveBlob(w http.ResponseWriter, r *http.Request, id string) {
	s.mu.Lock()
	u := s.uploads[id]
	s.mu.Unlock()
	if u == nil || r.Method != http.MethodPut {
		http.NotFound(w, r)
		return
	}
	q := rawQuery(r.URL.RawQuery)
	switch q["comp"] {
	case "":
		s.putBlock(w, r, u, "")
	case "block":
		if q["blockid"] == "" {
			http.Error(w, "no block id", 400)
			return
		}
		s.putBlock(w, r, u, q["blockid"])
	case "blocklist":
		var list struct {
			Items []struct {
				ID string `xml:",chardata"`
			} `xml:",any"`
		}
		if err := xml.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(&list); err != nil {
			http.Error(w, "the block list is not XML", 400)
			return
		}
		u.mu.Lock()
		u.order = u.order[:0]
		for _, it := range list.Items {
			u.order = append(u.order, strings.TrimSpace(it.ID))
		}
		u.mu.Unlock()
		azureHeaders(w, s.now())
		w.WriteHeader(201)
	default:
		http.Error(w, "not a call this cache takes", 400)
	}
}

// putBlock stages a block as an object of its own. "" is a Put Blob: the whole archive, a list of itself.
func (s *Server) putBlock(w http.ResponseWriter, r *http.Request, u *upload, blockID string) {
	if r.ContentLength > maxBlock {
		http.Error(w, "a block too large", 413)
		return
	}
	var body []byte
	if r.ContentLength >= 0 {
		body = make([]byte, r.ContentLength)
		if _, err := io.ReadFull(r.Body, body); err != nil {
			http.Error(w, "the block was cut short", 400)
			return
		}
	} else {
		var err error
		if body, err = io.ReadAll(io.LimitReader(r.Body, maxBlock+1)); err != nil || len(body) > maxBlock {
			http.Error(w, "a block unreadable or too large", 400)
			return
		}
	}
	name := "single"
	if blockID != "" {
		sum := sha256.Sum256([]byte(blockID))
		name = "b" + hex.EncodeToString(sum[:8])
	}
	key := u.stage + name
	if err := u.w.Put(r.Context(), key, body); err != nil {
		s.logf("save %q: a block could not be kept: %v", u.key, err)
		http.Error(w, "the bucket could not take the block", 503)
		return
	}
	u.mu.Lock()
	now := s.now()
	if u.first.IsZero() {
		u.first = now
	}
	u.last = now
	u.blocks[blockID] = blockInfo{key: key, size: int64(len(body))}
	if blockID == "" {
		u.order = []string{""}
	}
	u.mu.Unlock()
	azureHeaders(w, now)
	w.WriteHeader(201)
}
