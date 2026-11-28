package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

type prereadBox struct {
	dir, hr, spool string
	cmd            *exec.Cmd
	env            []string
}

func (b *prereadBox) path(name string) string { return filepath.Join(b.dir, name) }

func (b *prereadBox) read(name string) string {
	raw, _ := os.ReadFile(b.path(name))
	return strings.TrimSpace(string(raw))
}

// startPreread runs boot.sh's hydrate() as a machine's user data holds it, in a sandbox: no manifest, so it goes straight
// to the pre-read; findmnt answers for a root on /dev/nvme0n1p1 and fio is the given script. rootBytes is what disk() exports.
// find is the real one, its arguments logged to find.log.
func startPreread(t *testing.T, fio, rootBytes string, pullcache string) *prereadBox {
	return startPrereadOn(t, fio, rootBytes, pullcache, "0", "0", "16000000")
}

// startPrereadOn is startPreread on a machine of memKB kB, with the walk taking walkSleep seconds and each file of the warm
// catSleep.
func startPrereadOn(t *testing.T, fio, rootBytes, pullcache, catSleep, walkSleep, memKB string) *prereadBox {
	t.Helper()
	return startPrereadWith(t, fio, rootBytes, pullcache, catSleep, walkSleep, memKB, nil)
}

// startPrereadWith is startPrereadOn for a size whose read set (bin/warm/<label>.list) holds readset, each path's {BOX}
// standing for the sandbox; nil is a size with none.
func startPrereadWith(t *testing.T, fio, rootBytes, pullcache, catSleep, walkSleep, memKB string, readset []string) *prereadBox {
	t.Helper()
	b := prepPreread(t, fio, rootBytes, pullcache, catSleep, walkSleep, memKB, readset)
	if err := b.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	return b
}

// prepPreread is startPrereadWith before the command starts.
func prepPreread(t *testing.T, fio, rootBytes, pullcache, catSleep, walkSleep, memKB string, readset []string) *prereadBox {
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
	stub("aws", `case "$*" in *bin/warm/*) [ -f "$BOX/readset.list" ] && exec /bin/cp "$BOX/readset.list" "${@: -1}"; exit 1 ;; esac; echo '{}' > "${@: -1}"`)
	stub("setpriv", `shift 4; exec "$@"`)
	stub("chown", `true`)
	stub("pullcache", pullcache)
	realFind, err := exec.LookPath("find")
	if err != nil {
		t.Fatal(err)
	}
	// The warm's file list (find ... -print0) is the sandbox's hot/ files, never the host's binaries; each cat is logged
	// and takes $CAT_SLEEP. systemctl is logged, never run.
	stub("find", `echo "$@" >> "$BOX/find.log"; case " $* " in *" -print0 "*) exec `+realFind+` "$BOX/hot" -type f -print0 ;; *" -printf "*) sleep "${WALK_SLEEP:-0}" ;; esac; exec `+realFind+` "$@"`)
	stub("cat", `[ "$1" = -- ] && { printf '%s\n' "${@:2}" >> "$BOX/cat.log"; sleep "${CAT_SLEEP:-0}"; }; exec /bin/cat "$@"`)
	stub("systemctl", `echo "$@" >> "$BOX/systemctl.log"`)
	if err := os.MkdirAll(filepath.Join(b.dir, "hot"), 0o755); err != nil {
		t.Fatal(err)
	}
	for i := range 5 {
		if err := os.WriteFile(filepath.Join(b.dir, "hot", "bin"+strconv.Itoa(i)), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// boot.sh makes the warm mark before it starts the hydration; a dispatch machine waits on it.
	if err := os.WriteFile(filepath.Join(b.hr, ".warming"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if readset != nil {
		list := strings.ReplaceAll(strings.Join(readset, "\n")+"\n", "{BOX}", b.dir)
		if err := os.WriteFile(b.path("readset.list"), []byte(list), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(b.spool, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b.path("meminfo"), []byte("MemTotal:       "+memKB+" kB\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fn := strings.NewReplacer("/run/superci-marks", b.spool, "/run/superci-readset.list", b.path("readset.fetched"), "/usr/local/bin/pullcache", filepath.Join(bin, "pullcache"), "/proc/meminfo", b.path("meminfo"), "/run/superci-pin.out", b.path("pin.out")).Replace(got[start : start+end+3])
	b.cmd = exec.Command("bash", "-c", fn+"\nhydrate")
	b.env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "HR="+b.hr, "HM="+b.path("m.json"), "PB=b", "CR=r", "ROOT_BYTES="+rootBytes, "BOX="+b.dir, "CAT_SLEEP="+catSleep, "WALK_SLEEP="+walkSleep)
	b.cmd.Env = b.env
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
	if got, want := strings.Join(b.marks(), " "), "hydrate.start= hydrate.done= preread.start= preread.done=full rewarm.done=full warm.done=full"; got != want {
		t.Errorf("marks %q, want %q", got, want)
	}
	if got := b.read("systemctl.log"); got != "start docker.service" {
		t.Errorf("systemctl ran %q, want the daemon started while the machine waits", got)
	}
	if n := len(strings.Fields(b.read("cat.log"))); n != 5 {
		t.Errorf("the warm read %d of the 5 hot files", n)
	}
	// The direct read dropped the cached inode tables and directories of what hydration wrote: they are walked again.
	if walked := strings.Split(b.read("find.log"), "\n")[0]; !strings.HasPrefix(walked, b.hr+" ") || !strings.HasSuffix(walked, "-printf %s") {
		t.Errorf("after the pre-read find walked %q, want the hydrated trees", walked)
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
	if walked := b.read("find.log"); walked != "" {
		t.Errorf("a walk ran beside the job: find %q", walked)
	}
	if got := b.read("systemctl.log") + b.read("cat.log"); got != "" {
		t.Errorf("the warm ran beside the job: %q", got)
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

func TestTheRunnerTakingItsJobEndsThePrereadBeforeSetUpJob(t *testing.T) {
	b := startPreread(t, `echo $$ > "$BOX/fio.pid"; exec sleep 300`, "30083776512", `true`)
	waitFor(t, "fio to start", func() bool { return b.read("fio.pid") != "" })
	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	got := bootData(cfg, "https://203.0.113.1:8443", "sha256//pin", "superci-4cpu")
	head := "while IFS= read -r line; do"
	start := strings.Index(got, head)
	end := strings.Index(got[start:], "\n      done\n")
	if start < 0 || end < 0 {
		t.Fatal("boot data has no runner output loop")
	}
	if err := os.WriteFile(b.path("hook.sh"), []byte(hydrateStop), 0o755); err != nil {
		t.Fatal(err)
	}
	sudo := `[ "$*" = "-u runner /home/runner/superci-hydrate-stop.sh" ] && exec env SUPERCI_HYDRATED="$HR" bash -e -o pipefail "$BOX/hook.sh"; exit 1`
	if err := os.WriteFile(b.path("bin/sudo"), []byte("#!/bin/bash\n"+sudo+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	loop := exec.Command("bash", "-c", `mark() { echo "$1" >> "$BOX/runner-marks"; }
printf '%s\n' '√ Connected to GitHub' '2026-11-26 14:26:01Z: Running job: test-go' | `+got[start:start+end]+"\n      done")
	loop.Env = append(os.Environ(), "PATH="+b.path("bin")+":"+os.Getenv("PATH"), "HR="+b.hr, "BOX="+b.dir)
	if out, err := loop.CombinedOutput(); err != nil {
		t.Fatalf("runner output loop: %v\n%s", err, out)
	}
	b.wait(t, 5*time.Second)
	if got := b.read("runner-marks"); got != "job.running" {
		t.Errorf("runner marks %q, want job.running", got)
	}
	if got, want := strings.Join(b.marks(), " "), "hydrate.start= hydrate.done= preread.start= preread.done=stopped"; got != want {
		t.Errorf("marks %q, want %q", got, want)
	}
}

func TestAJobThatStartsDuringTheWarmStopsItWithinAFile(t *testing.T) {
	b := startPrereadOn(t, `true`, "30083776512", `true`, "0.3", "0", "16000000")
	waitFor(t, "the warm to read a file", func() bool { return b.read("cat.log") != "" })
	if err := os.Remove(filepath.Join(b.hr, ".pid")); err != nil {
		t.Fatal(err)
	}
	b.wait(t, 5*time.Second)
	hot := filepath.Join(b.dir, "hot") + "/"
	for deadline := time.Now().Add(700 * time.Millisecond); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		procs, _ := filepath.Glob("/proc/[0-9]*/cmdline")
		for _, p := range procs {
			if raw, err := os.ReadFile(p); err == nil && strings.Contains(string(raw), hot) {
				t.Fatalf("a read of a hot file still runs after the job started: %q", strings.ReplaceAll(string(raw), "\x00", " "))
			}
		}
	}
	if n := len(strings.Fields(b.read("cat.log"))); n >= 5 {
		t.Errorf("the warm read all %d files after the job started", n)
	}
	if got, want := strings.Join(b.marks(), " "), "hydrate.start= hydrate.done= preread.start= preread.done=full rewarm.done=full warm.done=stopped"; got != want {
		t.Errorf("marks %q, want %q", got, want)
	}
}

func TestAJobThatStartsDuringTheWalkStartsNoDaemonAndNoWarm(t *testing.T) {
	b := startPrereadOn(t, `true`, "30083776512", `true`, "0", "1", "16000000")
	waitFor(t, "the walk to start", func() bool { return strings.Contains(b.read("find.log"), "-printf") })
	if err := os.Remove(filepath.Join(b.hr, ".pid")); err != nil {
		t.Fatal(err)
	}
	b.wait(t, 5*time.Second)
	if got := b.read("systemctl.log") + b.read("cat.log"); got != "" {
		t.Errorf("the warm ran after the job started during the walk: %q", got)
	}
	if got, want := strings.Join(b.marks(), " "), "hydrate.start= hydrate.done= preread.start= preread.done=full rewarm.done=stopped"; got != want {
		t.Errorf("marks %q, want %q", got, want)
	}
}

func TestOnlyAMachineOf12GBOrMoreWarmsTheLibraries(t *testing.T) {
	for memKB, libs := range map[string]bool{"3900000": false, "7800000": false, "16000000": true} {
		b := startPrereadOn(t, `true`, "30083776512", `true`, "0", "0", memKB)
		b.wait(t, 20*time.Second)
		lines := strings.Split(b.read("find.log"), "\n")
		list := lines[len(lines)-1]
		if got := strings.Contains(list, "/usr/lib/x86_64-linux-gnu"); got != libs {
			t.Errorf("%s kB: the warm's list %q has the libraries %v, want %v", memKB, list, got, libs)
		}
		if !strings.Contains(list, "/home/runner/externals") {
			t.Errorf("%s kB: the warm's list %q lacks the runner's externals", memKB, list)
		}
	}
}

// The hydration removes the warm mark when it ends, so a dispatch machine does not wait out its cap for a warm that is done.
func TestTheHydrationRemovesTheWarmMarkWhenItEnds(t *testing.T) {
	b := startPreread(t, `true`, "30083776512", `true`)
	b.wait(t, 20*time.Second)
	if _, err := os.Stat(filepath.Join(b.hr, ".warming")); !os.IsNotExist(err) {
		t.Errorf("the warm mark is still there after the hydration ended (%v)", err)
	}
}

func TestASizeWithAReadSetReadsOnlyItsFilesAndNotTheWholeRoot(t *testing.T) {
	b := startPrereadWith(t, `echo ran > "$BOX/fio.ran"`, "30083776512", `true`, "0", "0", "16000000",
		[]string{"{BOX}/hot/bin0", "{BOX}/hot/bin3", "{BOX}/not/in/this/image"})
	b.wait(t, 20*time.Second)
	if b.read("fio.ran") != "" {
		t.Error("fio read the whole root of a size with a read set")
	}
	read := strings.Fields(b.read("cat.log"))
	slices.Sort(read)
	if want := []string{b.path("hot/bin0"), b.path("hot/bin3"), b.path("not/in/this/image")}; !slices.Equal(read, want) {
		t.Errorf("read %q, want the read set's paths %q", read, want)
	}
	if walked := b.read("find.log"); walked != "" {
		t.Errorf("a size with a read set walked trees: find %q", walked)
	}
	if got, want := strings.Join(b.marks(), " "), "hydrate.start= readset.start= readset.done=full:missing hydrate.done="; got != want {
		t.Errorf("marks %q, want %q", got, want)
	}
	if got := b.read("systemctl.log"); got != "start docker.service" {
		t.Errorf("systemctl %q, want dockerd started once the read set is read", got)
	}
	if _, err := os.Stat(filepath.Join(b.hr, ".warming")); !os.IsNotExist(err) {
		t.Errorf("the warm is done, yet the dispatch machine's wait mark remains: %v", err)
	}
}

func TestAJobThatStartsDuringTheReadSetEndsItAndNothingReadsAfter(t *testing.T) {
	b := startPrereadWith(t, `echo ran > "$BOX/fio.ran"`, "30083776512", `true`, "300", "0", "16000000",
		[]string{"{BOX}/hot/bin0", "{BOX}/hot/bin1"})
	waitFor(t, "the read set to start", func() bool { return b.read("cat.log") != "" })
	if err := os.Remove(filepath.Join(b.hr, ".pid")); err != nil {
		t.Fatal(err)
	}
	began := time.Now()
	b.wait(t, 5*time.Second)
	if took := time.Since(began); took > 2*time.Second {
		t.Errorf("the read set outlived the job's start by %v", took)
	}
	if got, want := strings.Join(b.marks(), " "), "hydrate.start= readset.start= readset.done=stopped"; !strings.HasPrefix(got, want) {
		t.Errorf("marks %q, want them to start %q", got, want)
	}
	if b.read("fio.ran") != "" || b.read("systemctl.log") != "" || b.read("find.log") != "" {
		t.Error("the pre-read, the daemon or a walk started after the job")
	}
}

func TestAnEmptyReadSetPrereadsAsBeforeAndAWholeOneMarksFull(t *testing.T) {
	b := startPrereadWith(t, `echo ran > "$BOX/fio.ran"`, "30083776512", `true`, "0", "0", "16000000", []string{""})
	b.wait(t, 20*time.Second)
	if b.read("fio.ran") == "" {
		t.Error("an empty read set switched the pre-read off")
	}
	if strings.Contains(strings.Join(b.marks(), " "), "readset.") {
		t.Errorf("an empty read set posted read set marks: %q", b.marks())
	}
	b = startPrereadWith(t, `echo ran > "$BOX/fio.ran"`, "30083776512", `true`, "0", "0", "16000000", []string{"{BOX}/hot/bin0", "{BOX}/hot/bin1"})
	b.wait(t, 20*time.Second)
	if got, want := strings.Join(b.marks(), " "), "hydrate.start= readset.start= readset.done=full hydrate.done="; got != want {
		t.Errorf("marks %q, want %q", got, want)
	}
}
