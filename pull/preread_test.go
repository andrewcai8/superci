package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

type prereadBox struct {
	dir, hr, spool string
	cmd            *exec.Cmd
}

func (b *prereadBox) path(name string) string { return filepath.Join(b.dir, name) }

func (b *prereadBox) read(name string) string {
	raw, _ := os.ReadFile(b.path(name))
	return strings.TrimSpace(string(raw))
}

// startPreread runs boot.sh's hydrate() as a machine's user data holds it, in a sandbox: no manifest, so it goes straight
// to the pre-read; findmnt answers for a root on /dev/nvme0n1p1 and fio is the given script. rootBytes is what disk() exports.
func startPreread(t *testing.T, fio, rootBytes string, pullcache string) *prereadBox {
	t.Helper()
	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	got := bootData(cfg, "https://203.0.113.1:8443", "sha256//pin", "superci-4cpu")
	start := strings.Index(got, "hydrate() {")
	end := strings.Index(got[start:], "\n}\n")
	if start < 0 || end < 0 {
		t.Fatal("boot data has no hydrate()")
	}
	b := &prereadBox{dir: t.TempDir()}
	b.hr, b.spool = b.path("hydrated"), b.path("marks")
	bin := b.path("bin")
	for _, d := range []string{bin, b.hr} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	stub := func(name, body string) {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/bash\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	stub("findmnt", `echo /dev/nvme0n1p1`)
	stub("fio", fio)
	stub("aws", `echo '{}' > "${@: -1}"`)
	stub("setpriv", `shift 4; exec "$@"`)
	stub("chown", `true`)
	stub("pullcache", pullcache)
	if err := os.WriteFile(b.spool, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	fn := strings.NewReplacer("/run/superci-marks", b.spool, "/usr/local/bin/pullcache", filepath.Join(bin, "pullcache")).Replace(got[start : start+end+3])
	b.cmd = exec.Command("bash", "-c", fn+"\nhydrate")
	b.cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "HR="+b.hr, "HM="+b.path("m.json"), "PB=b", "CR=r", "ROOT_BYTES="+rootBytes, "BOX="+b.dir)
	if err := b.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	return b
}

func (b *prereadBox) wait(t *testing.T, within time.Duration) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- b.cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("hydrate failed: %v", err)
		}
	case <-time.After(within):
		b.cmd.Process.Kill()
		t.Fatalf("hydrate still running after %v", within)
	}
}

// marks is each mark as "name=value": the spool's lines are name=epoch=ms[=value].
func (b *prereadBox) marks() []string {
	var out []string
	for _, l := range strings.Split(b.read("marks"), "\n") {
		if f := strings.Split(l, "="); l != "" {
			out = append(out, f[0]+"="+strings.Join(f[min(3, len(f)):], "="))
		}
	}
	return out
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestThePrereadReadsOnlyTheSnapshotsPartOfTheRootAndPostsItsStartAndEnd(t *testing.T) {
	b := startPreread(t, `echo "$@" > "$BOX/fio.args"`, "30083776512", `true`)
	b.wait(t, 20*time.Second)
	args := b.read("fio.args")
	for _, want := range []string{"--filename=/dev/nvme0n1p1", "--size=30083776512", "--readonly", "--rw=read", "--direct=1", "--iodepth=32"} {
		if !strings.Contains(args, want) {
			t.Errorf("fio ran as %q, want %s", args, want)
		}
	}
	if got, want := strings.Join(b.marks(), " "), "hydrate.start= hydrate.done= preread.start= preread.done=full"; got != want {
		t.Errorf("marks %q, want %q", got, want)
	}
	if _, err := os.Stat(filepath.Join(b.hr, ".pid")); !os.IsNotExist(err) {
		t.Errorf("a finished pre-read leaves no pid for the hook to signal: %v", err)
	}
}

func TestAJobThatStartsDuringThePrereadEndsItWithinASecond(t *testing.T) {
	b := startPreread(t, `echo $$ > "$BOX/fio.pid"; exec sleep 300`, "30083776512", `true`)
	waitFor(t, "fio to start", func() bool { return b.read("fio.pid") != "" })
	pid, _ := strconv.Atoi(b.read("fio.pid"))
	// What the job-started hook does last: it removes the pid file (the hydration's own processes are the runner's to signal; fio is root's).
	if err := os.Remove(filepath.Join(b.hr, ".pid")); err != nil {
		t.Fatal(err)
	}
	began := time.Now()
	b.wait(t, 5*time.Second)
	if took := time.Since(began); took > 2*time.Second {
		t.Errorf("the pre-read outlived the job's start by %v", took)
	}
	waitFor(t, "fio to end", func() bool { return syscall.Kill(pid, 0) != nil })
	if got, want := strings.Join(b.marks(), " "), "hydrate.start= hydrate.done= preread.start= preread.done=stopped"; got != want {
		t.Errorf("marks %q, want %q", got, want)
	}
}

func TestNoPrereadStartsAfterTheJobDidOrWithoutARootSize(t *testing.T) {
	for name, c := range map[string]struct{ rootBytes, pullcache string }{
		"the hook ran during the hydration": {"30083776512", `rm -f "$HR/.pid"; echo "hydrated 1/1 entries 5 MB"`},
		"no size for the root":              {"", `true`},
		"something else as the size":        {"nvme0n1", `true`},
	} {
		t.Run(name, func(t *testing.T) {
			b := startPreread(t, `echo ran > "$BOX/fio.ran"`, c.rootBytes, c.pullcache)
			b.wait(t, 20*time.Second)
			if b.read("fio.ran") != "" {
				t.Error("fio ran")
			}
			for _, m := range b.marks() {
				if strings.HasPrefix(m, "preread.") {
					t.Errorf("mark %s without a pre-read", m)
				}
			}
		})
	}
}
