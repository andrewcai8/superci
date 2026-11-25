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
	cmd := exec.Command("bash", "hydrate-stop.sh")
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
