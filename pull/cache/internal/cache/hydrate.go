package cache

import (
	"bufio"
	"bytes"
	"context"
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

// A Manifest is what a machine unpacks before it asks for its runner (`pullcache hydrate`): for each entry, the
// newest entry of one repository's cache whose key matches, in the first of Scopes that has one (as a job's restore
// reads its own scope, then main's), laid out as actions/cache would restore it into Workspace. The machine's one
// job takes what it needs with a rename (pull/README.md, "Hydration").
type Manifest struct {
	Repository string          `json:"repository"`
	Scopes     []string        `json:"scopes"`
	Workspace  string          `json:"workspace"`
	Entries    []ManifestEntry `json:"entries"`
}

// A ManifestEntry is a restore key in which * stands for any run of characters. Checkout checks out the work tree of
// the git repository the entry holds, so a job's checkout over it writes only what its commit changed. Or it is
// Python, a CPython version ("3.13") installed into the machine's tool cache as actions/setup-python installs one,
// so a job's setup-python of that version finds it there.
type ManifestEntry struct {
	Key      string `json:"key,omitempty"`
	Checkout bool   `json:"checkout,omitempty"`
	Python   string `json:"python,omitempty"`
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
	if len(m.Scopes) == 0 || slices.Contains(m.Scopes, "") || !filepath.IsAbs(m.Workspace) || filepath.Clean(m.Workspace) != m.Workspace || len(m.Entries) == 0 {
		return m, errors.New("manifest: scopes, an absolute clean workspace and at least one entry are needed")
	}
	for _, e := range m.Entries {
		switch {
		case e.Python != "" && e.Key == "" && !e.Checkout:
		case e.Python == "" && okKey(e.Key):
		default:
			return m, fmt.Errorf("manifest: an entry is a key or a Python version: %+v", e)
		}
	}
	return m, nil
}

// pattern is the restore key as a match from the start of a key.
func (e ManifestEntry) pattern() *regexp.Regexp {
	parts := strings.Split(e.Key, "*")
	for i, p := range parts {
		parts[i] = regexp.QuoteMeta(p)
	}
	return regexp.MustCompile("^" + strings.Join(parts, ".*"))
}

// A Hydrator unpacks a manifest's entries under Root. A finished entry is Root/<key>/, in which Root/ stands for the
// machine's /: the archive's files are where actions/cache would put them, under that prefix. An entry is unpacked
// in Root/.tmp-<key>/ and renamed into place whole, so a reader sees all of it or nothing.
type Hydrator struct {
	Reader        Reader
	DefaultBranch string
	Root          string
	Log           *log.Logger
	// Unpack is tar and Checkout is git; tests replace them.
	Unpack   func(ctx context.Context, archive io.Reader, dir string) error
	Checkout func(ctx context.Context, dir string) error
	// Python: the tool cache it installs into, the versions manifest it reads, and the platform it picks for.
	ToolCache      string
	PythonVersions string
	Platform       Platform
	HTTP           *http.Client
}

// Platform is the build of a Python actions/python-versions publishes for this machine.
type Platform struct{ OS, Version, Arch string }

// Hydrated is what became of one entry of the manifest.
type Hydrated struct {
	Pattern string  `json:"pattern"`
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

// Run hydrates every entry at once and says what became of each. One listing of each scope serves every entry.
func (h *Hydrator) Run(ctx context.Context, m Manifest) []Hydrated {
	out := make([]Hydrated, len(m.Entries))
	for i, e := range m.Entries {
		out[i].Pattern = e.Key
	}
	roots := make([]string, len(m.Scopes))
	listed := make([][]Object, len(m.Scopes))
	for i, sc := range m.Scopes {
		roots[i] = scopeRoot(m.Repository, Scope{Name: sc}, h.DefaultBranch)
		objs, err := h.Reader.List(ctx, roots[i], maxListed)
		if err != nil {
			for i := range out {
				out[i].Error = fmt.Sprintf("scope %s could not be listed: %v", sc, err)
			}
			return out
		}
		listed[i] = objs
	}
	taken := map[string]bool{}
	var wg sync.WaitGroup
	for i, e := range m.Entries {
		if e.Python != "" {
			wg.Add(1)
			go func() {
				defer wg.Done()
				began := time.Now()
				version, err := h.python(ctx, e.Python)
				out[i].Pattern, out[i].Key, out[i].Seconds = "python "+e.Python, version, time.Since(began).Seconds()
				if err != nil {
					out[i].Error = err.Error()
				}
			}()
			continue
		}
		var obj Object
		var key string
		ok := false
		for s := range roots {
			if obj, key, ok = newest(listed[s], roots[s], e.pattern()); ok {
				break
			}
		}
		switch {
		case !ok:
			out[i].Error = "no entry matches"
		case taken[key]:
			out[i].Key, out[i].Error = key, "another entry of the manifest took it"
		default:
			taken[key] = true
			out[i].Key, out[i].MB = key, obj.Size>>20
			wg.Add(1)
			go func() {
				defer wg.Done()
				began := time.Now()
				if err := h.one(ctx, m.Workspace, obj, key, e.Checkout); err != nil {
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

// newest is the entry under root (every version of one scope) whose key the pattern matches, the most recently
// written, as the server's restore keys pick one.
func newest(objs []Object, root string, pattern *regexp.Regexp) (Object, string, bool) {
	var best *Object
	var bestKey string
	for i := range objs {
		_, escaped, ok := strings.Cut(strings.TrimPrefix(objs[i].Key, root), "/")
		if !ok {
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

func (h *Hydrator) one(ctx context.Context, workspace string, obj Object, key string, checkout bool) error {
	dir, tmp := filepath.Join(h.Root, esc(key)), filepath.Join(h.Root, ".tmp-"+esc(key))
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
		_, err := readTo(ctx, h.Reader, 0, 0, pw, obj, 0, obj.Size-1)
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
