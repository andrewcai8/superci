//go:build linux

package pin

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
)

// tree makes files (name -> size) under a temp dir and returns it.
func tree(t *testing.T, files map[string]int) string {
	t.Helper()
	dir := t.TempDir()
	for name, n := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(strings.Repeat("x", n)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// recorder is a Locker that locks nothing and says which files it was asked to.
type recorder struct {
	root  string
	files []string
	fail  func(name string) error
}

func (r *recorder) lock(f *os.File, size int64) (func(), error) {
	rel, _ := filepath.Rel(r.root, f.Name())
	if r.fail != nil {
		if err := r.fail(rel); err != nil {
			return nil, err
		}
	}
	r.files = append(r.files, rel)
	return func() {}, nil
}

func TestItLocksTheWorkTreeAndSkipsNodeModulesAndGitButItsIndex(t *testing.T) {
	root := tree(t, map[string]int{
		"a.txt":                        10,
		"src/b.ts":                     20,
		"src/deep/c.ts":                30,
		".git/index":                   100,
		".git/objects/ab/cdef":         1000,
		".git/HEAD":                    5,
		"node_modules/x/index.js":      7,
		"src/node_modules/y/index.js":  7,
		"empty":                        0,
		"pkg/.git/index":               40,
		"pkg/.git/config":              3,
		"pkg/node_modules/z.js":        3,
		"third/.git":                   0, // a gitlink file with nothing in it
		"sub/.gitignore":               4,
		"sub/node_modules_not/keep.js": 4,
	})
	if err := os.Symlink(filepath.Join(root, "a.txt"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(root, "fifo"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := &recorder{root: root}
	res, err := Run(root, Options{Lock: rec.lock})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(rec.files)
	want := []string{".git/index", "a.txt", "pkg/.git/index", "src/b.ts", "src/deep/c.ts", "sub/.gitignore", "sub/node_modules_not/keep.js"}
	if !slices.Equal(rec.files, want) {
		t.Fatalf("locked %q, want %q", rec.files, want)
	}
	if res.Files != 7 || res.Bytes != 100+10+40+20+30+4+4 || res.Capped || res.Stopped || res.Failed != 0 {
		t.Fatalf("result %+v", res)
	}
}

func TestItStopsAtTheByteCapAndSaysSo(t *testing.T) {
	root := tree(t, map[string]int{"a": 400, "b": 400, "c": 400, "d": 400})
	rec := &recorder{root: root}
	res, err := Run(root, Options{MaxBytes: 1000, Lock: rec.lock})
	if err != nil {
		t.Fatal(err)
	}
	if res.Files != 2 || res.Bytes != 800 || !res.Capped {
		t.Fatalf("want 2 files, 800 bytes, capped: %+v", res)
	}
	if line := res.Line(); !strings.Contains(line, "files=2 bytes=800 ") || !strings.Contains(line, "capped=yes") || strings.Contains(line, "\n") {
		t.Fatalf("the summary is one line that says it was capped: %q", line)
	}
}

func TestItStopsAtTheFileCapAndSaysSo(t *testing.T) {
	root := tree(t, map[string]int{"a": 1, "b": 1, "c/d": 1, "c/e": 1, "f": 1})
	rec := &recorder{root: root}
	res, err := Run(root, Options{MaxFiles: 3, Lock: rec.lock})
	if err != nil {
		t.Fatal(err)
	}
	if res.Files != 3 || res.Bytes != 3 || !res.Capped || len(rec.files) != 3 {
		t.Fatalf("want 3 files and capped: %+v %q", res, rec.files)
	}
}

func TestATreeUnderItsCapsIsNotCappedAndTheSummaryHasEveryField(t *testing.T) {
	root := tree(t, map[string]int{"a": 5, "b/c": 7})
	res, err := Run(root, Options{MaxBytes: 12, MaxFiles: 2, Lock: (&recorder{root: root}).lock})
	if err != nil {
		t.Fatal(err)
	}
	if res.Capped || res.Files != 2 || res.Bytes != 12 {
		t.Fatalf("exactly at the caps is under them: %+v", res)
	}
	line := res.Line()
	for _, want := range []string{"pinned ", "files=2", "bytes=12", "seconds=", "capped=no", "failed=0"} {
		if !strings.Contains(line, want) {
			t.Errorf("summary %q lacks %q", line, want)
		}
	}
}

func TestAFileTheMachineWillNotLockIsCountedAndTheRestStillLock(t *testing.T) {
	root := tree(t, map[string]int{"a": 1, "b": 1, "c": 1})
	rec := &recorder{root: root, fail: func(name string) error {
		if name == "b" {
			return syscall.EINVAL
		}
		return nil
	}}
	res, err := Run(root, Options{Lock: rec.lock})
	if err != nil {
		t.Fatal(err)
	}
	if res.Files != 2 || res.Failed != 1 || res.Stopped || !errors.Is(res.FirstError, syscall.EINVAL) {
		t.Fatalf("want 2 locked and 1 failed: %+v", res)
	}
	if line := res.Line(); !strings.Contains(line, "failed=1") || !strings.Contains(line, "invalid argument") {
		t.Fatalf("the line names the failure: %q", line)
	}
}

func TestALimitOfLockedMemoryEndsTheRunAfterItsFirstRefusal(t *testing.T) {
	root := tree(t, map[string]int{"a": 1, "b": 1, "c": 1, "d": 1})
	n := 0
	rec := &recorder{root: root, fail: func(string) error {
		n++
		if n > 2 {
			return syscall.ENOMEM
		}
		return nil
	}}
	res, err := Run(root, Options{Lock: rec.lock})
	if err != nil {
		t.Fatal(err)
	}
	if res.Files != 2 || res.Failed != 1 || !res.Stopped || res.Capped || n != 3 {
		t.Fatalf("want the run to end at the first ENOMEM (asked %d times): %+v", n, res)
	}
	if line := res.Line(); !strings.Contains(line, "capped=stopped") || !strings.Contains(line, "cannot allocate memory") {
		t.Fatalf("the line says the machine stopped it: %q", line)
	}
}

func TestARootThatIsNotADirectoryIsAnError(t *testing.T) {
	root := tree(t, map[string]int{"a": 1})
	for _, bad := range []string{filepath.Join(root, "a"), filepath.Join(root, "missing")} {
		if _, err := Run(bad, Options{Lock: (&recorder{root: root}).lock}); err == nil {
			t.Errorf("%s: want an error", bad)
		}
	}
}

func TestARootThatIsASymlinkToTheTreeIsPinned(t *testing.T) {
	root := tree(t, map[string]int{"a": 1})
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	res, err := Run(link, Options{Lock: (&recorder{root: root}).lock})
	if err != nil || res.Files != 1 {
		t.Fatalf("%v %+v", err, res)
	}
}

// The real lock. A machine that forbids it (a container without the memory limit raised, a low RLIMIT_MEMLOCK) skips these:
// the rest of the package proves the walk without locking anything.
func TestRealLocksAreCountedInTheKernelsMlockedPages(t *testing.T) {
	root := tree(t, map[string]int{"a": 4096, "b/c": 4096})
	before := mlockedKB(t)
	res, err := Run(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer res.Release()
	if res.Failed != 0 {
		t.Skipf("this machine refuses to mlock (%v): RLIMIT_MEMLOCK is %s", res.FirstError, memlockLimit())
	}
	if res.Files != 2 || res.Bytes != 8192 {
		t.Fatalf("%+v", res)
	}
	if got := mlockedKB(t) - before; got < 8 {
		t.Fatalf("Mlocked grew by %d kB, want at least 8: nothing was locked", got)
	}
	res.Release()
	if got := mlockedKB(t) - before; got > 0 {
		t.Fatalf("Mlocked is %d kB over where it began after Release", got)
	}
}

func TestAMachineWithTooLittleLockedMemoryReportsItAndDoesNotCrash(t *testing.T) {
	var old syscall.Rlimit
	if err := syscall.Getrlimit(rlimitMemlock, &old); err != nil {
		t.Skipf("no RLIMIT_MEMLOCK here: %v", err)
	}
	// 16 KiB is test-sized: three 8 KiB files cannot all be locked. Lowering the soft limit is always allowed.
	small := syscall.Rlimit{Cur: 16 << 10, Max: old.Max}
	if err := syscall.Setrlimit(rlimitMemlock, &small); err != nil {
		t.Skipf("cannot lower RLIMIT_MEMLOCK: %v", err)
	}
	defer syscall.Setrlimit(rlimitMemlock, &old)
	root := tree(t, map[string]int{"a": 8192, "b": 8192, "c": 8192})
	res, err := Run(root, Options{})
	if err != nil {
		t.Fatalf("a refused lock is no error: %v", err)
	}
	defer res.Release()
	if res.Failed == 0 {
		t.Skip("this process may lock past RLIMIT_MEMLOCK (root, or CAP_IPC_LOCK)")
	}
	if res.Files >= 3 || !res.Stopped || res.FirstError == nil || !strings.Contains(res.Line(), "capped=stopped") {
		t.Fatalf("want the lock the machine refused reported and the run ended: %+v", res)
	}
}
