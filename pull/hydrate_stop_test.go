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

// groupAlive is whether any process of the process group exists.
func groupAlive(pgid int) bool { return syscall.Kill(-pgid, 0) == nil }

func runHook(t *testing.T, dir string) {
	t.Helper()
	// As the runner runs a hook: bash --noprofile --norc -e -o pipefail.
	cmd := exec.Command("bash", "--noprofile", "--norc", "-e", "-o", "pipefail", "hydrate-stop.sh")
	cmd.Env = append(os.Environ(), "SUPERCI_HYDRATED="+dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the hook must never fail the job: %v: %s", err, out)
	}
}

func TestTheJobStartedHookEndsTheWholeHydrationAndNothingElse(t *testing.T) {
	dir := t.TempDir()
	// What boot.sh starts: a process group of its own whose leader writes its id, and children below it.
	hydration := exec.Command("setsid", "bash", "-c", `echo $$ > "$1/.pid"; (sleep 300 & sleep 300 &) ; sleep 300`, "_", dir)
	if err := hydration.Start(); err != nil {
		t.Skip("no setsid: ", err)
	}
	go hydration.Wait()
	bystander := exec.Command("sleep", "300")
	if err := bystander.Start(); err != nil {
		t.Fatal(err)
	}
	defer bystander.Process.Kill()
	defer hydration.Process.Kill()
	var pgid int
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if b, err := os.ReadFile(filepath.Join(dir, ".pid")); err == nil {
			if pgid, _ = strconv.Atoi(strings.TrimSpace(string(b))); pgid != 0 {
				break
			}
		}
	}
	if pgid == 0 || !groupAlive(pgid) {
		t.Fatalf("the hydration did not start: %d", pgid)
	}

	began := time.Now()
	runHook(t, dir)

	if groupAlive(pgid) {
		t.Fatal("a process of the hydration is still running when the hook returns: it would compete with the job")
	}
	if took := time.Since(began); took > 3*time.Second {
		t.Fatalf("the hook holds the job back %v", took)
	}
	if bystander.ProcessState != nil || syscall.Kill(bystander.Process.Pid, 0) != nil {
		t.Fatal("the hook ended a process that is not the hydration")
	}
	if _, err := os.Stat(filepath.Join(dir, ".pid")); !os.IsNotExist(err) {
		t.Fatalf("the pid file is left for the next reader: %v", err)
	}
}

func TestTheJobStartedHookDoesNothingWhenThereIsNoHydrationToStop(t *testing.T) {
	dir := t.TempDir()
	runHook(t, dir)
	for _, junk := range []string{"", "not a number\n", "-1", "99999999999999999999"} {
		if err := os.WriteFile(filepath.Join(dir, ".pid"), []byte(junk), 0o644); err != nil {
			t.Fatal(err)
		}
		runHook(t, dir)
	}
	// A finished hydration's id: the group is gone, and the hook still succeeds.
	done := exec.Command("setsid", "true")
	if err := done.Run(); err != nil {
		t.Skip("no setsid")
	}
	if err := os.WriteFile(filepath.Join(dir, ".pid"), []byte(strconv.Itoa(done.Process.Pid)), 0o644); err != nil {
		t.Fatal(err)
	}
	runHook(t, dir)
}

func TestAMachineRegistersAtOnceAndHydratesInTheBackgroundUnderAHookThatStopsIt(t *testing.T) {
	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	got := bootData(cfg, "https://203.0.113.1:8443", "sha256//pin", "superci-4cpu")
	if len(got) > 16*1024 {
		t.Fatalf("user data is %d bytes; EC2 takes 16 KiB", len(got))
	}
	for _, want := range []string{
		"ACTIONS_RUNNER_HOOK_JOB_STARTED=",
		strings.TrimSpace(hydrateStop),
		`setsid bash -c "$(declare -f hydrate); hydrate" &`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("boot data lacks %q", want)
		}
	}
	// The hydration is started before the machine asks for its runner and is never waited for.
	if hydrate, jit := strings.Index(got, `hydrate" &`), strings.Index(got, "call jit"); hydrate < 0 || jit < 0 || hydrate > jit {
		t.Fatalf("the hydration must start in the background before the first /jit call (%d, %d)", hydrate, jit)
	}
	if strings.Contains(got, "\ntimeout -k") || strings.Contains(got, "\n  timeout -k 5 110") {
		t.Error("a timeout that makes its own process group would hide the hydration's children from the hook")
	}
}

// This runs boot.sh's hydrate() as a machine's user data holds it, with the machine's paths and commands replaced by a
// sandbox's: aws, setpriv and chown are stubs and pullcache is a script that says what it hydrated.
func TestTheHydrationPostsItsStartAndEndAsTimelineMarksAndEndsWithItsPidFileGone(t *testing.T) {
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
	fn := got[start : start+end+3]
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	stub := func(name, body string) {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/bash\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	stub("aws", `case "$*" in *bin/warm/*) exit 1 ;; esac; cp "$@" 2>/dev/null; echo '{}' > "${@: -1}"`)
	stub("setpriv", `shift 4; exec "$@"`)
	stub("chown", `true`)
	stub("pullcache", `echo "hydrate: log line on stderr" >&2; echo "hydrated 4/5 entries 1210 MB"`)
	spool, hr := filepath.Join(dir, "marks"), filepath.Join(dir, "hydrated")
	if err := os.WriteFile(spool, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(hr, 0o755); err != nil {
		t.Fatal(err)
	}
	fn = strings.NewReplacer("/run/superci-marks", spool, "/usr/local/bin/pullcache", filepath.Join(bin, "pullcache")).Replace(fn)
	cmd := exec.Command("bash", "-c", fn+"\nhydrate")
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "HR="+hr, "HM="+filepath.Join(dir, "m.json"), "PB=b", "CR=r")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	marks, _ := os.ReadFile(spool)
	lines := strings.Split(strings.TrimSpace(string(marks)), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "hydrate.start=") || !strings.HasSuffix(lines[0], "=0") ||
		!strings.HasPrefix(lines[1], "hydrate.done=") || !strings.HasSuffix(lines[1], "=4/5:1210MB") {
		t.Fatalf("want a start and a done mark with what was hydrated: %q", lines)
	}
	if _, err := os.Stat(filepath.Join(hr, ".pid")); !os.IsNotExist(err) {
		t.Fatalf("a finished hydration leaves no pid for the hook to signal: %v", err)
	}
	// With no spool (a listener without the timeline) the hydration still runs and posts nothing.
	if err := os.Remove(spool); err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command("bash", "-c", fn+"\nhydrate")
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "HR="+hr, "HM="+filepath.Join(dir, "m.json"), "PB=b", "CR=r")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("without a spool: %v: %s", err, out)
	}
	if _, err := os.Stat(spool); !os.IsNotExist(err) {
		t.Fatalf("no spool is made for marks: %v", err)
	}
}
