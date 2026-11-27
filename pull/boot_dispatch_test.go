package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func renderBoot(t *testing.T, script, mode string) string {
	t.Helper()
	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	old := bootScript
	bootScript = script
	defer func() { bootScript = old }()
	return bootDataMode(cfg, "https://203.0.113.1:8443", "sha256//pin", "superci-4cpu", mode)
}

func TestUserDataFitsInBothModes(t *testing.T) {
	for _, mode := range []string{"scaleset", "dispatch"} {
		got := renderBoot(t, bootScript, mode)
		t.Logf("%s: %d bytes of user data", mode, len(got))
		if len(got) > 16*1024 {
			t.Errorf("%s: user data is %d bytes; EC2 takes 16 KiB", mode, len(got))
		}
		if left := regexp.MustCompile(`@[A-Z_]+@`).FindString(got); left != "" {
			t.Errorf("%s: a placeholder is left: %s", mode, left)
		}
	}
}

// runTail runs boot.sh's rendered end, from the dispatch branch on, with the commands that reach outside replaced by
// ones that record what they were asked and answer as the listener does. agent is the status the listener gives /agent.
func runTail(t *testing.T, mode string, agentStatuses ...string) []string {
	t.Helper()
	got := renderBoot(t, bootScript, mode)
	i := strings.Index(got, "# Dispatch mode (pull/agent.py)")
	if i < 0 {
		t.Fatal("no dispatch branch")
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	script := `log=` + log + `
SP=` + dir + `/spool; echo 0 > $SP.sent; : > $SP
L=https://203.0.113.1:8443; PIN=sha256//pin; SENDER=1
printf '%s\n' ` + strings.Join(agentStatuses, " ") + ` > $SP.statuses
call() {
  echo "call $*" >> $log
  case $1 in
    agent) head -1 $SP.statuses; [ "$(wc -l < $SP.statuses)" -gt 1 ] && sed -i 1d $SP.statuses ;;
    jit) echo 410 ;;
  esac
}
mark() { echo "mark $*" >> $log; }
send() { :; }
sleep() { echo "sleep $*" >> $log; }
kill() { :; }
python3() { echo "python3 $*" >> $log; return 7; }
poweroff() { echo poweroff >> $log; }
` + got[i:]
	if out, err := exec.Command("bash", "-c", script).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	raw, _ := os.ReadFile(log)
	return strings.Split(strings.TrimSpace(string(raw)), "\n")
}

func TestDispatchModeSkipsTheRunnerAndRunsTheAgent(t *testing.T) {
	calls := runTail(t, "dispatch", "200")
	want := []string{"call agent /run/superci-agent.py", "mark runner.start", "python3 -I /run/superci-agent.py https://203.0.113.1:8443 sha256//pin", "mark runner.exit 7", "mark poweroff", "poweroff"}
	if strings.Join(calls, "|") != strings.Join(want, "|") {
		t.Errorf("calls:\n%s\nwant:\n%s", strings.Join(calls, "\n"), strings.Join(want, "\n"))
	}
}

func TestDispatchModeTold410RunsNothingAndPowersOff(t *testing.T) {
	calls := runTail(t, "dispatch", "410")
	want := []string{"call agent /run/superci-agent.py", "mark poweroff", "poweroff"}
	if strings.Join(calls, "|") != strings.Join(want, "|") {
		t.Errorf("calls: %v, want %v", calls, want)
	}
}

func TestDispatchModeAsksAgainWhenTheListenerIsDown(t *testing.T) {
	calls := runTail(t, "dispatch", "000", "503", "200")
	var asks, jits int
	for _, c := range calls {
		if c == "call agent /run/superci-agent.py" {
			asks++
		}
		if strings.HasPrefix(c, "call jit") {
			jits++
		}
	}
	if asks != 3 || jits != 0 || !strings.Contains(strings.Join(calls, "\n"), "python3 -I /run/superci-agent.py") {
		t.Errorf("calls: %v", calls)
	}
}

func TestScaleSetModeStillAsksForItsRunner(t *testing.T) {
	calls := runTail(t, "scaleset", "200")
	joined := strings.Join(calls, "\n")
	if !strings.Contains(joined, "call jit /run/superci-jit") || strings.Contains(joined, "call agent") || strings.Contains(joined, "python3") {
		t.Errorf("calls: %v", calls)
	}
}
