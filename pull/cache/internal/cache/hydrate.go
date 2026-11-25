package cache

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"
)

// A Manifest is what a machine unpacks while it waits for its job (`pullcache hydrate`): for each entry, the newest
// entry of one repository's cache whose key matches, in the first of Scopes that has one (as a job's restore reads
// its own scope, then main's), laid out as actions/cache would restore it into Workspace. The machine's one job takes
// what it needs with a rename (pull/README.md, "Hydration").
type Manifest struct {
	Repository string   `json:"repository"`
	Scopes     []string `json:"scopes"`
	Workspace  string   `json:"workspace"`
	// Compression is the method actions/cache puts in an entry's version on these machines: its zstd probe finds
	// the image's zstd too old for --long, so it is "zstd-without-long" (read off main's entries, see Version).
	Compression string          `json:"compression,omitempty"`
	Entries     []ManifestEntry `json:"entries"`
}

// DefaultCompression is the method of the entries the runner image's actions/cache saves.
const DefaultCompression = "zstd-without-long"

// A ManifestEntry is a restore key in which * stands for any run of characters, of the entry saved from Paths (the
// cache action's `path` lines, as a job passes them). An entry is (scope, version, key) and the version is made of
// the paths, so two path sets that share a key are two entries; Paths picks the one a job's own restore would read.
// Checkout checks out the work tree of the git repository the entry holds, so a job's checkout over it writes only
// what its commit changed. Or it is Python, a CPython version ("3.13") installed into the machine's tool cache as
// actions/setup-python installs one, so a job's setup-python of that version finds it there.
type ManifestEntry struct {
	Key      string   `json:"key,omitempty"`
	Paths    []string `json:"paths,omitempty"`
	Checkout bool     `json:"checkout,omitempty"`
	Python   string   `json:"python,omitempty"`
}

// Version is the version actions/cache gives an entry saved from paths with a compression method: the SHA-256 of
// the paths, the method and the toolkit's salt joined with "|" (toolkit getCacheVersion). It is the directory of
// the entry's object under its scope, and the first part of its hydrated directory.
func Version(paths []string, compression string) string {
	sum := sha256.Sum256([]byte(strings.Join(append(slices.Clone(paths), compression, "1.0"), "|")))
	return hex.EncodeToString(sum[:])
}

// Version is the entry's version under the manifest's compression method.
func (m Manifest) Version(e ManifestEntry) string {
	c := m.Compression
	if c == "" {
		c = DefaultCompression
	}
	return Version(e.Paths, c)
}

func ParseManifest(raw []byte) (Manifest, error) {
	var m Manifest
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&m); err != nil {
		return m, fmt.Errorf("manifest: %w", err)
	}
	if m.Repository == "" || strings.Trim(m.Repository, "0123456789") != "" {
		return m, errors.New("manifest: repository is the repository's numeric id")
	}
	if m.Compression != "" && !okVersion(strings.ReplaceAll(m.Compression, "-", "")) {
		return m, errors.New("manifest: compression is a method name such as zstd-without-long")
	}
	if len(m.Scopes) == 0 || slices.Contains(m.Scopes, "") || !filepath.IsAbs(m.Workspace) || filepath.Clean(m.Workspace) != m.Workspace || len(m.Entries) == 0 {
		return m, errors.New("manifest: scopes, an absolute clean workspace and at least one entry are needed")
	}
	for _, e := range m.Entries {
		switch {
		case e.Python != "" && e.Key == "" && !e.Checkout:
		case e.Python == "" && okKey(e.Key) && len(e.Paths) > 0 && !slices.Contains(e.Paths, "") && !slices.ContainsFunc(e.Paths, func(p string) bool { return strings.Contains(p, "|") }):
		default:
			return m, fmt.Errorf("manifest: an entry is a key with the paths it was saved from, or a Python version: %+v", e)
		}
	}
	return m, nil
}

// literal is the restore key up to its first *: the prefix every key it matches starts with.
func (e ManifestEntry) literal() string {
	l, _, _ := strings.Cut(e.Key, "*")
	return l
}

// pattern is the restore key as a match from the start of a key.
func (e ManifestEntry) pattern() *regexp.Regexp {
	parts := strings.Split(e.Key, "*")
	for i, p := range parts {
		parts[i] = regexp.QuoteMeta(p)
	}
	return regexp.MustCompile("^" + strings.Join(parts, ".*"))
}

// A Hydrator unpacks a manifest's entries under Root. A finished entry is Root/<version>/<key>/, in which that
// directory stands for the machine's /: the archive's files are where actions/cache would put them, under that
// prefix. An entry is unpacked in Root/.tmp-<version>-<key>/ and renamed into place whole, so a reader sees all of it
// or nothing, and the run may stop at any moment (a job started) with every finished entry in place.
type Hydrator struct {
	Reader        Reader
	DefaultBranch string
	// AnyScope lets a manifest name a scope other than the default branch's. Only main's jobs write main's scope, so
	// without it no PR's save can become what another machine's job starts from; a dev listener's A/B sets it.
	AnyScope bool
	Root     string
	Log      *log.Logger
	// Unpack is tar and Checkout is git; tests replace them.
	Unpack   func(ctx context.Context, archive io.Reader, dir string) error
	Checkout func(ctx context.Context, dir string) error
	// Python: the tool cache it installs into, the versions manifest it reads, and the platform it picks for.
	ToolCache      string
	PythonVersions string
	Platform       Platform
	HTTP           *http.Client
}

const (
	// An entry is read in ranges of 64 MiB, a few at a time: four times the daemon's 16 MiB, so a machine of a
	// burst makes a fourth of the requests to the one prefix every machine reads (S3 serves a prefix 5,500 GETs a
	// second), and holds no more than chunk times ahead of an entry in memory.
	hydrateChunk = 64 << 20
	hydrateAhead = 3
	// An entry's listing is of one version's keys under the entry's literal prefix, in key order. A seed's keys
	// are run ids, so the newest are the last: the cap is far above what a scope keeps, and a cut-off warns.
	hydrateMaxListed = 50000
)

// Platform is the build of a Python actions/python-versions publishes for this machine.
type Platform struct{ OS, Version, Arch string }

// Hydrated is what became of one entry of the manifest.
type Hydrated struct {
	Pattern string  `json:"pattern"`
	Version string  `json:"version,omitempty"`
	Key     string  `json:"key,omitempty"`
	MB      int64   `json:"mb,omitempty"`
	Seconds float64 `json:"seconds"`
	Error   string  `json:"error,omitempty"`
}

func (h *Hydrator) logf(format string, a ...any) {
	if h.Log != nil {
		h.Log.Printf(format, a...)
	}
}

// Run hydrates every entry at once and says what became of each. Each entry is looked up under its own version and
// key prefix, so what a lookup lists is that entry's candidates and no other path set's.
func (h *Hydrator) Run(ctx context.Context, m Manifest) []Hydrated {
	out := make([]Hydrated, len(m.Entries))
	for i, e := range m.Entries {
		out[i].Pattern = e.Key
	}
	for _, sc := range m.Scopes {
		if !h.AnyScope && Class(sc, h.DefaultBranch) != "d" {
			for i := range out {
				out[i].Error = fmt.Sprintf("scope %s is not the default branch's", sc)
			}
			return out
		}
	}
	type found struct {
		obj Object
		key string
		ok  bool
	}
	hits := make([]found, len(m.Entries))
	var wg sync.WaitGroup
	for i, e := range m.Entries {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if e.Python != "" {
				began := time.Now()
				version, err := h.python(ctx, e.Python)
				out[i].Pattern, out[i].Key, out[i].Seconds = "python "+e.Python, version, time.Since(began).Seconds()
				if err != nil {
					out[i].Error = err.Error()
				}
				return
			}
			out[i].Version = m.Version(e)
			obj, key, ok, err := h.lookup(ctx, m, e, out[i].Version)
			hits[i] = found{obj, key, ok}
			if err != nil {
				out[i].Error = err.Error()
			}
		}()
	}
	wg.Wait()
	taken := map[string]bool{}
	for i, e := range m.Entries {
		if e.Python != "" || out[i].Error != "" {
			continue
		}
		f, id := hits[i], out[i].Version+"/"+hits[i].key
		switch {
		case !f.ok:
			out[i].Error = "no entry matches"
		case taken[id]:
			out[i].Key, out[i].Error = f.key, "another entry of the manifest took it"
		default:
			taken[id] = true
			out[i].Key, out[i].MB = f.key, f.obj.Size>>20
			wg.Add(1)
			go func() {
				defer wg.Done()
				began := time.Now()
				if err := h.one(ctx, m.Workspace, f.obj, out[i].Version, f.key, e.Checkout); err != nil {
					out[i].Error = err.Error()
				}
				out[i].Seconds = time.Since(began).Seconds()
			}()
		}
	}
	wg.Wait()
	for _, r := range out {
		if r.Error != "" {
			h.logf("hydrate %q: %s", r.Pattern, r.Error)
		} else {
			h.logf("hydrate %q: %q, %d MB in %.1f s", r.Pattern, r.Key, r.MB, r.Seconds)
		}
	}
	return out
}

// lookup is the newest entry of the entry's version whose key the restore key matches, in the first scope that has
// one.
func (h *Hydrator) lookup(ctx context.Context, m Manifest, e ManifestEntry, version string) (Object, string, bool, error) {
	for _, sc := range m.Scopes {
		base := ScopePrefix(m.Repository, Scope{Name: sc}, version, h.DefaultBranch)
		objs, err := h.Reader.List(ctx, base+esc(e.literal()), hydrateMaxListed)
		if err != nil {
			return Object{}, "", false, fmt.Errorf("scope %s could not be listed: %v", sc, err)
		}
		if len(objs) >= hydrateMaxListed {
			h.logf("hydrate: %q in scope %s listed its first %d keys only; keys sorting after them are not hydrated", e.Key, sc, hydrateMaxListed)
		}
		if obj, key, ok := newest(objs, base, e.pattern()); ok {
			return obj, key, true, nil
		}
	}
	return Object{}, "", false, nil
}

// newest is the entry under base (one version of one scope) whose key the pattern matches, the most recently
// written, as the server's restore keys pick one.
func newest(objs []Object, base string, pattern *regexp.Regexp) (Object, string, bool) {
	var best *Object
	var bestKey string
	for i := range objs {
		escaped, ok := strings.CutPrefix(objs[i].Key, base)
		if !ok || strings.Contains(escaped, "/") {
			continue
		}
		key, err := url.PathUnescape(escaped)
		if err != nil || !pattern.MatchString(key) || objs[i].Size == 0 {
			continue
		}
		if best == nil || objs[i].Modified.After(best.Modified) || objs[i].Modified.Equal(best.Modified) && objs[i].Key > best.Key {
			best, bestKey = &objs[i], key
		}
	}
	if best == nil {
		return Object{}, "", false
	}
	return *best, bestKey, true
}

// errUnpacked ends the read of an archive the unpack no longer reads: tar may stop at the archive's end-of-archive
// blocks before the stream's last bytes.
var errUnpacked = errors.New("the unpack stopped reading")

func (h *Hydrator) one(ctx context.Context, workspace string, obj Object, version, key string, checkout bool) error {
	dir, tmp := filepath.Join(h.Root, version, esc(key)), filepath.Join(h.Root, ".tmp-"+version+"-"+esc(key))
	if _, err := os.Lstat(dir); err == nil {
		return errors.New("already hydrated")
	}
	if err := os.RemoveAll(tmp); err != nil {
		return err
	}
	ws := filepath.Join(tmp, workspace)
	if err := os.MkdirAll(ws, 0o755); err != nil {
		return err
	}
	unpack, check := h.Unpack, h.Checkout
	if unpack == nil {
		unpack = untar
	}
	if check == nil {
		check = checkoutHead
	}
	pr, pw := io.Pipe()
	read := make(chan error, 1)
	go func() {
		_, err := readTo(ctx, h.Reader, hydrateChunk, hydrateAhead, pw, obj, 0, obj.Size-1)
		pw.CloseWithError(err)
		read <- err
	}()
	err := unpack(ctx, pr, ws)
	pr.CloseWithError(errUnpacked)
	if rerr := <-read; err == nil && rerr != nil && !errors.Is(rerr, errUnpacked) {
		err = fmt.Errorf("read: %w", rerr)
	}
	if err == nil && checkout {
		err = check(ctx, ws)
	}
	if err == nil {
		err = ctx.Err()
	}
	if err == nil {
		err = os.MkdirAll(filepath.Dir(dir), 0o755)
	}
	if err == nil {
		err = os.Rename(tmp, dir)
	}
	return err
}

// untar unpacks an archive actions/cache made, as it unpacks one: GNU tar with -P, from the workspace.
func untar(ctx context.Context, archive io.Reader, dir string) error {
	br := bufio.NewReaderSize(archive, 1<<20)
	magic, _ := br.Peek(4)
	args := []string{"-xf", "-", "-P", "-C", dir}
	switch {
	case bytes.HasPrefix(magic, []byte{0x28, 0xb5, 0x2f, 0xfd}):
		args = append(args, "--use-compress-program", "zstd -d --long=30")
	case bytes.HasPrefix(magic, []byte{0x1f, 0x8b}):
		args = append(args, "-z")
	}
	cmd := exec.CommandContext(ctx, "tar", args...)
	cmd.Stdin = br
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("tar: %v: %s", err, bytes.TrimSpace(out))
	}
	return nil
}

// checkoutHead writes the work tree of the repository in dir from its own objects (no fetch: a seed holds its
// commit's blobs). git cannot tell a file written in the same second as the index from one changed after it, and
// reads every such file again on its next refresh; a refresh a second later records them clean, so the job's own
// `git reset --hard` reads nothing.
func checkoutHead(ctx context.Context, dir string) error {
	git := func(args ...string) error {
		cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir, "-c", "checkout.workers=0"}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_NO_LAZY_FETCH=1", "GIT_TERMINAL_PROMPT=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git %s: %v: %s", args[0], err, bytes.TrimSpace(out))
		}
		return nil
	}
	if err := git("reset", "-q", "--hard", "HEAD"); err != nil {
		return err
	}
	select {
	case <-time.After(1100 * time.Millisecond):
	case <-ctx.Done():
		return ctx.Err()
	}
	return git("update-index", "-q", "--refresh")
}

// python installs the newest stable release of version for the machine's platform into the tool cache, as
// actions/setup-python does when it finds none there: it unpacks the release and runs its setup.sh, which writes the
// version's .complete marker last, so setup-python takes no half-installed one.
func (h *Hydrator) python(ctx context.Context, version string) (string, error) {
	client, cache, list := h.HTTP, h.ToolCache, h.PythonVersions
	if client == nil {
		client = http.DefaultClient
	}
	if cache == "" {
		cache = "/opt/hostedtoolcache"
	}
	if list == "" {
		list = "https://raw.githubusercontent.com/actions/python-versions/main/versions-manifest.json"
	}
	get := func(url string) (io.ReadCloser, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		res, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		if res.StatusCode != http.StatusOK {
			res.Body.Close()
			return nil, fmt.Errorf("%s: %s", url, res.Status)
		}
		return res.Body, nil
	}
	body, err := get(list)
	if err != nil {
		return "", err
	}
	var releases []struct {
		Version string `json:"version"`
		Stable  bool   `json:"stable"`
		Files   []struct {
			Platform        string `json:"platform"`
			PlatformVersion string `json:"platform_version"`
			Arch            string `json:"arch"`
			URL             string `json:"download_url"`
		} `json:"files"`
	}
	err = json.NewDecoder(body).Decode(&releases)
	body.Close()
	if err != nil {
		return "", fmt.Errorf("the versions manifest: %w", err)
	}
	p := h.Platform
	for _, r := range releases {
		if !r.Stable || r.Version != version && !strings.HasPrefix(r.Version, version+".") {
			continue
		}
		for _, f := range r.Files {
			if f.Platform != p.OS || f.PlatformVersion != p.Version || f.Arch != p.Arch {
				continue
			}
			if _, err := os.Stat(filepath.Join(cache, "Python", r.Version, p.Arch+".complete")); err == nil {
				return r.Version, nil
			}
			tmp := filepath.Join(h.Root, ".tmp-python-"+r.Version)
			if err := os.RemoveAll(tmp); err != nil {
				return r.Version, err
			}
			if err := os.MkdirAll(tmp, 0o755); err != nil {
				return r.Version, err
			}
			defer os.RemoveAll(tmp)
			release, err := get(f.URL)
			if err != nil {
				return r.Version, err
			}
			err = untar(ctx, release, tmp)
			release.Close()
			if err != nil {
				return r.Version, err
			}
			cmd := exec.CommandContext(ctx, "bash", "./setup.sh")
			cmd.Dir = tmp
			cmd.Env = append(os.Environ(), "RUNNER_TOOL_CACHE="+cache, "AGENT_TOOLSDIRECTORY="+cache)
			if out, err := cmd.CombinedOutput(); err != nil {
				return r.Version, fmt.Errorf("setup.sh: %v: %s", err, bytes.TrimSpace(out))
			}
			return r.Version, nil
		}
	}
	return "", fmt.Errorf("no stable Python %s for %s %s %s", version, p.OS, p.Version, p.Arch)
}

// ThisPlatform is the machine's platform as actions/python-versions names it.
func ThisPlatform() Platform {
	p := Platform{OS: runtime.GOOS, Arch: map[string]string{"amd64": "x64", "arm64": "arm64"}[runtime.GOARCH]}
	if b, err := os.ReadFile("/etc/os-release"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if v, ok := strings.CutPrefix(line, "VERSION_ID="); ok {
				p.Version = strings.Trim(v, `"`)
			}
		}
	}
	return p
}
