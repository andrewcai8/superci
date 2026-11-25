package cache_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"superci-pull-cache/internal/cache"
	"superci-pull-cache/internal/cachetest"
)

const (
	hydrateRepo = "1351063449"
	workspace   = "/home/runner/_work/megpt-mono/megpt-mono"
)

var (
	mainScope = cache.Scope{Name: "refs/heads/main"}
	paths     = []string{"p"}
	// entryVersion is where actions/cache keeps what a job saves from `path: p` on this image.
	entryVersion = cache.Version(paths, cache.DefaultCompression)
)

// archive is a gzip tar of files named as actions/cache names them: relative to the workspace.
func archive(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var b bytes.Buffer
	z := gzip.NewWriter(&b)
	tw := tar.NewWriter(z)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(tw, body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func put(mem *cachetest.Mem, sc cache.Scope, key string, body []byte, at time.Time) {
	putFrom(mem, sc, paths, key, body, at)
}

func putFrom(mem *cachetest.Mem, sc cache.Scope, from []string, key string, body []byte, at time.Time) {
	mem.Set(cache.ObjectKey(hydrateRepo, sc, cache.Version(from, cache.DefaultCompression), key, "main"), body, at)
}

// at is where a finished entry is: its version, then its key.
func at(root, key string) string { return filepath.Join(root, entryVersion, key) }

func manifest(entries ...cache.ManifestEntry) cache.Manifest {
	return cache.Manifest{Repository: hydrateRepo, Scopes: []string{"refs/heads/main"}, Workspace: workspace, Entries: entries}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestHydratesTheNewestMatchingEntryOfTheScopeWhereTheJobWouldRestoreIt(t *testing.T) {
	mem, root, now := cachetest.NewMem(), t.TempDir(), time.Now()
	const old, cur = "bun-modules-v1-Linux-X64-bun1.4.2-aura-hono-api-scripts-on-aaa", "bun-modules-v1-Linux-X64-bun1.4.3-aura-hono-api-scripts-on-bbb"
	put(mem, mainScope, old, archive(t, map[string]string{"aura-hono-api/node_modules/x": "old"}), now.Add(-time.Hour))
	put(mem, mainScope, cur, archive(t, map[string]string{"aura-hono-api/node_modules/x": "new", "../../_temp/py/bin": "venv"}), now)
	put(mem, mainScope, "bun-modules-v1-Linux-X64-bun1.4.3-aura-hono-api-mind-scripts-on-ccc", archive(t, map[string]string{"x": "mind"}), now.Add(time.Hour))
	put(mem, cache.Scope{Name: "refs/pull/7/merge"}, "bun-modules-v1-Linux-X64-bun1.4.3-aura-hono-api-scripts-on-ddd", archive(t, map[string]string{"x": "pr"}), now.Add(2*time.Hour))
	h := &cache.Hydrator{Reader: mem, DefaultBranch: "main", Root: root}

	res := h.Run(context.Background(), manifest(cache.ManifestEntry{Paths: paths, Key: "bun-modules-v1-Linux-X64-bun*-aura-hono-api-scripts-on-"}))

	if len(res) != 1 || res[0].Error != "" || res[0].Key != cur {
		t.Fatalf("want main's newest root tree, not an older one, the Mind's or a pull request's: %+v", res)
	}
	got := at(root, cur)
	if s := read(t, filepath.Join(got, workspace, "aura-hono-api/node_modules/x")); s != "new" {
		t.Fatalf("the tree holds %q", s)
	}
	if s := read(t, filepath.Join(got, "/home/runner/_work/_temp/py/bin")); s != "venv" {
		t.Fatalf("a path outside the workspace lands where the job would restore it, under the entry: %q", s)
	}
	if left, _ := filepath.Glob(filepath.Join(root, ".tmp-*")); len(left) != 0 {
		t.Fatalf("nothing half-made is left: %v", left)
	}
}

func TestAMirroredCopyOfAnOlderSaveIsNotTheNewest(t *testing.T) {
	mem, root, now := cachetest.NewMem(), t.TempDir(), time.Now()
	version := cache.Version(paths, cache.DefaultCompression)
	put(mem, mainScope, "git-seed-v1-Linux-X64-2", archive(t, map[string]string{"x": "saved here, newer"}), now.Add(-time.Hour))
	mem.SetCopy(cache.ObjectKey(hydrateRepo, mainScope, version, "git-seed-v1-Linux-X64-1", "main"),
		archive(t, map[string]string{"x": "copied in, older"}), now.Add(-3*time.Hour), now)
	h := &cache.Hydrator{Reader: mem, DefaultBranch: "main", Root: root}

	res := h.Run(context.Background(), manifest(cache.ManifestEntry{Paths: paths, Key: "git-seed-v1-Linux-X64-"}))

	if len(res) != 1 || res[0].Error != "" || res[0].Key != "git-seed-v1-Linux-X64-2" {
		t.Fatalf("want the newest save, not a copy made later of an older one: %+v", res)
	}
}

func TestTheFirstScopeWithAMatchWinsAsAJobsRestoreReadsItsOwnScopeThenMains(t *testing.T) {
	mem, root, now := cachetest.NewMem(), t.TempDir(), time.Now()
	branch := cache.Scope{Name: "refs/heads/ci/load"}
	put(mem, branch, "git-seed-v1-Linux-X64-2", archive(t, map[string]string{".git/HEAD": "branch"}), now.Add(-time.Hour))
	put(mem, mainScope, "git-seed-v1-Linux-X64-3", archive(t, map[string]string{".git/HEAD": "main"}), now)
	put(mem, mainScope, "mods-1", archive(t, map[string]string{"node_modules/x": "main"}), now)
	h := &cache.Hydrator{Reader: mem, DefaultBranch: "main", AnyScope: true, Root: root}
	m := manifest(cache.ManifestEntry{Paths: paths, Key: "git-seed-v1-Linux-X64-"}, cache.ManifestEntry{Paths: paths, Key: "mods-"})
	m.Scopes = []string{branch.Name, "refs/heads/main"}

	res := h.Run(context.Background(), m)

	if res[0].Key != "git-seed-v1-Linux-X64-2" || res[1].Key != "mods-1" || res[0].Error != "" || res[1].Error != "" {
		t.Fatalf("want the branch's own seed though main's is newer, and main's tree the branch lacks: %+v", res)
	}
}

func TestAManifestNamingAScopeOtherThanMainsHydratesNothing(t *testing.T) {
	mem, root, now := cachetest.NewMem(), t.TempDir(), time.Now()
	pr := cache.Scope{Name: "refs/pull/7/merge"}
	put(mem, pr, "seed-1", archive(t, map[string]string{".git/HEAD": "pr"}), now)
	put(mem, mainScope, "seed-0", archive(t, map[string]string{".git/HEAD": "main"}), now.Add(-time.Hour))
	h := &cache.Hydrator{Reader: mem, DefaultBranch: "main", Root: root}
	m := manifest(cache.ManifestEntry{Paths: paths, Key: "seed-"})
	m.Scopes = []string{pr.Name, "refs/heads/main"}

	res := h.Run(context.Background(), m)

	if res[0].Key != "" || res[0].Error == "" {
		t.Fatalf("a PR's save must never become what another machine's job starts from: %+v", res)
	}
	for _, k := range []string{"seed-1", "seed-0"} {
		if _, err := os.Stat(at(root, k)); !os.IsNotExist(err) {
			t.Fatalf("nothing may be in place when a manifest names a PR scope (%s): %v", k, err)
		}
	}
}

func TestAnEntryThatFailsToUnpackIsNeverInPlace(t *testing.T) {
	mem, root := cachetest.NewMem(), t.TempDir()
	put(mem, mainScope, "seed-1", []byte("\x1f\x8bnot a gzip stream at all"), time.Now())
	h := &cache.Hydrator{Reader: mem, DefaultBranch: "main", Root: root}

	res := h.Run(context.Background(), manifest(cache.ManifestEntry{Paths: paths, Key: "seed-"}))

	if res[0].Error == "" {
		t.Fatalf("a broken archive is an error: %+v", res)
	}
	if _, err := os.Stat(at(root, "seed-1")); !os.IsNotExist(err) {
		t.Fatalf("a failed entry must not be where a job looks: %v", err)
	}
}

func TestAnEntryStoppedByTheDeadlineIsNeverInPlace(t *testing.T) {
	mem, root := cachetest.NewMem(), t.TempDir()
	put(mem, mainScope, "seed-1", archive(t, map[string]string{"a": "b"}), time.Now())
	ctx, cancel := context.WithCancel(context.Background())
	h := &cache.Hydrator{Reader: mem, DefaultBranch: "main", Root: root,
		Checkout: func(context.Context, string) error { cancel(); return context.Canceled }}

	res := h.Run(ctx, manifest(cache.ManifestEntry{Paths: paths, Key: "seed-", Checkout: true}))

	if res[0].Error == "" {
		t.Fatalf("want an error, got %+v", res)
	}
	if _, err := os.Stat(at(root, "seed-1")); !os.IsNotExist(err) {
		t.Fatalf("an entry stopped before its checkout ended must not be where a job looks: %v", err)
	}
}

func TestEachMatchedEntryIsHydratedOnceAndOnlyCheckoutEntriesAreCheckedOut(t *testing.T) {
	mem, root, now := cachetest.NewMem(), t.TempDir(), time.Now()
	put(mem, mainScope, "seed-9", archive(t, map[string]string{".git/HEAD": "ref"}), now)
	put(mem, mainScope, "mods-1", archive(t, map[string]string{"node_modules/x": "y"}), now)
	var checked []string
	h := &cache.Hydrator{Reader: mem, DefaultBranch: "main", Root: root,
		Checkout: func(_ context.Context, dir string) error { checked = append(checked, dir); return nil }}

	res := h.Run(context.Background(), manifest(cache.ManifestEntry{Paths: paths, Key: "seed-", Checkout: true}, cache.ManifestEntry{Paths: paths, Key: "s*d-"}, cache.ManifestEntry{Paths: paths, Key: "mods-"}, cache.ManifestEntry{Paths: paths, Key: "none-"}))

	if res[0].Error != "" || res[2].Error != "" {
		t.Fatalf("want seed and mods hydrated: %+v", res)
	}
	if !strings.Contains(res[1].Error, "another entry") || res[3].Error != "no entry matches" {
		t.Fatalf("want the duplicate and the miss reported: %+v", res)
	}
	if len(checked) != 1 || !strings.HasSuffix(checked[0], ".tmp-"+entryVersion+"-seed-9"+workspace) {
		t.Fatalf("want one checkout, in the seed's workspace before it is in place: %v", checked)
	}
}

func TestACheckedOutSeedLeavesAWorkTreeGitFindsClean(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	src := t.TempDir()
	git := func(dir string, args ...string) string {
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return string(out)
	}
	git(src, "init", "-q")
	for i := 0; i < 50; i++ {
		if err := os.MkdirAll(filepath.Join(src, "d", string(rune('a'+i%5))), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(src, "d", string(rune('a'+i%5)), strings.Repeat("f", i+1)), []byte(strings.Repeat("x", i)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git(src, "add", "-A")
	git(src, "commit", "-q", "-m", "seed")
	// The seed is .git alone, as ci-cache-warm saves it: the work tree is not in the archive.
	tarCmd := exec.Command("tar", "-czf", "-", ".git")
	tarCmd.Dir = src
	seed, err := tarCmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	mem, root := cachetest.NewMem(), t.TempDir()
	put(mem, mainScope, "git-seed-v1-Linux-X64-1", seed, time.Now())
	h := &cache.Hydrator{Reader: mem, DefaultBranch: "main", Root: root}

	res := h.Run(context.Background(), manifest(cache.ManifestEntry{Paths: paths, Key: "git-seed-v1-Linux-X64-", Checkout: true}))

	if res[0].Error != "" {
		t.Fatalf("%+v", res)
	}
	ws := filepath.Join(at(root, "git-seed-v1-Linux-X64-1"), workspace)
	if s := read(t, filepath.Join(ws, "d/a/f")); s != "" {
		t.Fatalf("the work tree is checked out: %q", s)
	}
	if s := git(ws, "status", "--porcelain"); s != "" {
		t.Fatalf("the checked-out tree is clean: %q", s)
	}
	if out, err := exec.Command("git", "-C", ws, "diff-files", "--quiet").CombinedOutput(); err != nil {
		t.Fatalf("no file needs reading again: %v %s", err, out)
	}
}

func TestTheManifestIsCheckedBeforeAnythingIsRead(t *testing.T) {
	ok := `{"repository":"1351063449","scopes":["refs/heads/main"],"workspace":"/home/runner/_work/r/r","entries":[{"key":"a-","paths":[".git"],"checkout":true}]}`
	if _, err := cache.ParseManifest([]byte(ok)); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		strings.Replace(ok, `"checkout"`, `"chekout"`, 1),
		strings.Replace(ok, `"1351063449"`, `"megpt-mono"`, 1),
		strings.Replace(ok, `/home/runner/_work/r/r`, `_work/r/r`, 1),
		strings.Replace(ok, `[{"key":"a-","paths":[".git"],"checkout":true}]`, `[]`, 1),
		strings.Replace(ok, `"paths":[".git"],`, ``, 1),
		strings.Replace(ok, `[".git"]`, `[""]`, 1),
		strings.Replace(ok, `"entries"`, `"compression":"zstd long","entries"`, 1),
		strings.Replace(ok, `"a-"`, `"a,b"`, 1),
		strings.Replace(ok, `["refs/heads/main"]`, `[]`, 1),
		strings.Replace(ok, `{"key":"a-","paths":[".git"],"checkout":true}`, `{"key":"a-","python":"3.13"}`, 1),
	} {
		if _, err := cache.ParseManifest([]byte(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}

func TestPythonIsInstalledIntoTheToolCacheAsSetupPythonInstallsIt(t *testing.T) {
	// python-versions' setup.sh copies the version into the tool cache and writes its .complete marker last.
	setup := `set -e; d="$RUNNER_TOOL_CACHE/Python/3.13.9/x64"; mkdir -p "$d/bin"; echo py > "$d/bin/python"; touch "$d.complete"`
	release := archive(t, map[string]string{"setup.sh": setup})
	var downloads atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/rel", func(w http.ResponseWriter, _ *http.Request) { downloads.Add(1); w.Write(release) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	file := func(version, url string) string {
		return fmt.Sprintf(`{"filename":"x","arch":"x64","platform":"linux","platform_version":%q,"download_url":%q}`, version, url)
	}
	mux.HandleFunc("/versions", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `[{"version":"3.14.1","stable":true,"files":[%s]},{"version":"3.13.10","stable":false,"files":[%s]},{"version":"3.13.9","stable":true,"files":[%s,%s]}]`,
			file("24.04", srv.URL+"/no"), file("24.04", srv.URL+"/no"), file("22.04", srv.URL+"/no"), file("24.04", srv.URL+"/rel"))
	})
	tools := t.TempDir()
	h := &cache.Hydrator{Reader: cachetest.NewMem(), DefaultBranch: "main", Root: t.TempDir(), ToolCache: tools,
		PythonVersions: srv.URL + "/versions", Platform: cache.Platform{OS: "linux", Version: "24.04", Arch: "x64"}}

	for range 2 {
		res := h.Run(context.Background(), manifest(cache.ManifestEntry{Python: "3.13"}))
		if res[0].Error != "" || res[0].Key != "3.13.9" {
			t.Fatalf("want the newest stable 3.13 for this platform, got %+v", res)
		}
	}

	if s := read(t, filepath.Join(tools, "Python/3.13.9/x64/bin/python")); s != "py\n" {
		t.Fatalf("not installed: %q", s)
	}
	if n := downloads.Load(); n != 1 {
		t.Fatalf("a version the tool cache holds is not fetched again: %d downloads", n)
	}
}

// listed is a Reader that says what it was asked to list.
type listed struct {
	*cachetest.Mem
	mu       sync.Mutex
	prefixes []string
}

func (l *listed) List(ctx context.Context, prefix string, max int) ([]cache.Object, error) {
	l.mu.Lock()
	l.prefixes = append(l.prefixes, prefix)
	l.mu.Unlock()
	return l.Mem.List(ctx, prefix, max)
}

func TestAnEntryIsItsScopeVersionAndKeyNotItsKeyAlone(t *testing.T) {
	mem, root, now := cachetest.NewMem(), t.TempDir(), time.Now()
	a, b := []string{"aura-hono-api/node_modules"}, []string{"aura-hono-api/mind/node_modules"}
	putFrom(mem, mainScope, a, "bun-modules-1", archive(t, map[string]string{"x": "A"}), now.Add(-time.Hour))
	putFrom(mem, mainScope, b, "bun-modules-1", archive(t, map[string]string{"x": "B"}), now)
	h := &cache.Hydrator{Reader: mem, DefaultBranch: "main", Root: root}
	m := manifest(cache.ManifestEntry{Key: "bun-modules-", Paths: a}, cache.ManifestEntry{Key: "bun-modules-", Paths: b}, cache.ManifestEntry{Key: "bun-modules-", Paths: []string{"elsewhere"}})

	res := h.Run(context.Background(), m)

	if res[0].Error != "" || res[1].Error != "" || res[2].Error != "no entry matches" {
		t.Fatalf("want each path set its own entry, and none for a path set nothing was saved from: %+v", res)
	}
	for paths, want := range map[string]string{"A": filepath.Join(root, cache.Version(a, cache.DefaultCompression), "bun-modules-1"), "B": filepath.Join(root, cache.Version(b, cache.DefaultCompression), "bun-modules-1")} {
		if s := read(t, filepath.Join(want, workspace, "x")); s != paths {
			t.Fatalf("%s's directory holds %q", paths, s)
		}
	}
}

func TestTheVersionIsWhatActionsCacheNamesAnEntryOnTheRunnerImage(t *testing.T) {
	// Read off main's bucket (Nov 25): the objects of these keys sit in these version directories.
	for paths, want := range map[string]string{
		".git":                            "e147b628583c5531a3df3c6023ec0bba34378b29ba54ba408f1613f67f53a817",
		"aura-hono-api/mind/node_modules": "bc0252d31637939426fee46aa18cd3112b9769bdfcaa7e83f6cbd1ad3adfb58f",
		"/home/runner/_work/_temp/harness-python": "a666248dce0b82ae9c1deb5180eaf65598b56ff1b558869a2f508bec22b1b866",
	} {
		if got := cache.Version([]string{paths}, cache.DefaultCompression); got != want {
			t.Errorf("%s: version %s, want %s", paths, got, want)
		}
	}
}

func TestEachEntryIsListedUnderItsOwnVersionAndKeyPrefixAndTheNewestSurvivesALongList(t *testing.T) {
	mem, root, now := cachetest.NewMem(), t.TempDir(), time.Now()
	// More seeds than the daemon's 5,000-object listing holds: a listing in key order cut there loses the newest.
	for i := 0; i < 5200; i++ {
		put(mem, mainScope, fmt.Sprintf("git-seed-v1-Linux-X64-%06d", i), []byte("x"), now.Add(-time.Duration(5200-i)*time.Minute))
	}
	put(mem, mainScope, "git-seed-v1-Linux-X64-005200", archive(t, map[string]string{".git/HEAD": "newest"}), now)
	putFrom(mem, mainScope, []string{"other"}, "mods-1", archive(t, map[string]string{"x": "other"}), now)
	put(mem, mainScope, "mods-2", archive(t, map[string]string{"x": "mine"}), now)
	l := &listed{Mem: mem}
	h := &cache.Hydrator{Reader: l, DefaultBranch: "main", Root: root}

	res := h.Run(context.Background(), manifest(cache.ManifestEntry{Key: "git-seed-v1-Linux-X64-", Paths: paths}, cache.ManifestEntry{Key: "mods-", Paths: paths}))

	if res[0].Error != "" || res[0].Key != "git-seed-v1-Linux-X64-005200" || res[1].Error != "" || res[1].Key != "mods-2" {
		t.Fatalf("want the newest seed past 5,000 listed before it, and the mods of this path set, not another's newer: %+v", res)
	}
	base := "v1/d/" + hydrateRepo + "/refs%2Fheads%2Fmain/" + entryVersion + "/"
	slices := map[string]bool{base + "git-seed-v1-Linux-X64-": true, base + "mods-": true}
	for _, p := range l.prefixes {
		if !slices[p] {
			t.Errorf("listed %q: want only an entry's version and key prefix", p)
		}
		delete(slices, p)
	}
	if len(slices) != 0 {
		t.Errorf("never listed %v", slices)
	}
}

func TestAnEntryIsReadInRangesOf64MiB(t *testing.T) {
	mem, root := cachetest.NewMem(), t.TempDir()
	body := make([]byte, 70<<20)
	put(mem, mainScope, "big-1", body, time.Now())
	h := &cache.Hydrator{Reader: mem, DefaultBranch: "main", Root: root,
		Unpack: func(_ context.Context, r io.Reader, _ string) error { _, err := io.Copy(io.Discard, r); return err }}

	res := h.Run(context.Background(), manifest(cache.ManifestEntry{Key: "big-", Paths: paths}))

	if res[0].Error != "" {
		t.Fatalf("%+v", res)
	}
	if mem.Opened != 2 {
		t.Fatalf("a 70 MiB entry is two ranged reads of up to 64 MiB, not %d", mem.Opened)
	}
}

func TestEntriesFinishedBeforeAStopStayInPlaceAndTheOneStoppedDoesNot(t *testing.T) {
	mem, root, now := cachetest.NewMem(), t.TempDir(), time.Now()
	put(mem, mainScope, "done-1", archive(t, map[string]string{"x": "y"}), now)
	put(mem, mainScope, "slow-1", archive(t, map[string]string{"x": "y"}), now)
	ctx, stop := context.WithCancel(context.Background())
	h := &cache.Hydrator{Reader: mem, DefaultBranch: "main", Root: root,
		Unpack: func(ctx context.Context, r io.Reader, dir string) error {
			if strings.Contains(dir, "slow-1") {
				for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
					if _, err := os.Stat(at(root, "done-1")); err == nil {
						break
					}
				}
				stop()
				<-ctx.Done()
				return ctx.Err()
			}
			_, err := io.Copy(io.Discard, r)
			return err
		}}

	res := h.Run(ctx, manifest(cache.ManifestEntry{Key: "done-", Paths: paths}, cache.ManifestEntry{Key: "slow-", Paths: paths}))

	if res[0].Error != "" {
		t.Fatalf("%+v", res)
	}
	if _, err := os.Stat(filepath.Join(at(root, "done-1"), workspace)); err != nil || res[1].Error == "" {
		t.Fatalf("the stopped entry is an error: %+v", res)
	}
	if _, err := os.Stat(at(root, "slow-1")); !os.IsNotExist(err) {
		t.Fatalf("a stopped entry must not be where a job looks: %v", err)
	}
}

func TestTheManifestTheReadmeShowsParsesAndNamesTheVersionsOfMainsEntries(t *testing.T) {
	readme := read(t, "../../../README.md")
	_, after, ok := strings.Cut(readme, "```json\n")
	if !ok {
		t.Fatal("the README shows no manifest")
	}
	block, _, _ := strings.Cut(after, "```")
	m, err := cache.ParseManifest([]byte(block))
	if err != nil {
		t.Fatalf("the README's manifest: %v", err)
	}
	// Read off main's bucket (Nov 25): the entries these keys are in.
	want := map[string]string{
		"git-seed-v1-Linux-X64-": "e147b628583c5531a3df3c6023ec0bba34378b29ba54ba408f1613f67f53a817",
		"bun-modules-v1-Linux-X64-bun*-aura-hono-api-mind-scripts-on-": "bc0252d31637939426fee46aa18cd3112b9769bdfcaa7e83f6cbd1ad3adfb58f",
		"mind-python-v1-Linux-X64-":                                    "a666248dce0b82ae9c1deb5180eaf65598b56ff1b558869a2f508bec22b1b866",
		"bun-modules-v1-Linux-X64-bun*-aura-hono-api-scripts-on-":      "f7137a0923f0ae710692383bb17d01f1aa3d3d179305be2bccb674392fc965f8",
	}
	for _, e := range m.Entries {
		if w, ok := want[e.Key]; ok && m.Version(e) != w {
			t.Errorf("%s: version %s, want %s", e.Key, m.Version(e), w)
		}
		delete(want, e.Key)
	}
	if len(want) != 0 {
		t.Errorf("the README's manifest lacks %v", want)
	}
}
