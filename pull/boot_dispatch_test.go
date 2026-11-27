package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
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
	return runTailWith(t, mode, "", agentStatuses...)
}

// runTailWith is runTail with shell lines of its own first (the hydration's marker, a sleep that ends it).
func runTailWith(t *testing.T, mode, pre string, agentStatuses ...string) []string {
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
HR=` + dir + `/hydrated; mkdir -p $HR
` + pre + `
` + got[i:]
	if out, err := exec.Command("bash", "-c", script).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	raw, _ := os.ReadFile(log)
	return strings.Split(strings.TrimSpace(string(raw)), "\n")
}

func TestDispatchModeSkipsTheRunnerAndRunsTheAgent(t *testing.T) {
	calls := runTail(t, "dispatch", "200")
	want := []string{"mark warm.ready", "call agent /run/superci-agent.py", "mark runner.start", "python3 -I /run/superci-agent.py https://203.0.113.1:8443 sha256//pin", "mark runner.exit 7", "mark poweroff", "poweroff"}
	if strings.Join(calls, "|") != strings.Join(want, "|") {
		t.Errorf("calls:\n%s\nwant:\n%s", strings.Join(calls, "\n"), strings.Join(want, "\n"))
	}
}

func TestDispatchModeTold410RunsNothingAndPowersOff(t *testing.T) {
	calls := runTail(t, "dispatch", "410")
	want := []string{"mark warm.ready", "call agent /run/superci-agent.py", "mark poweroff", "poweroff"}
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

// A machine asks for its job only once its hydration and warm reads are done: a cold machine's job waits on the
// snapshot's first reads (node's first exec 2.5 s against Blacksmith's 39 ms, Nov 27).
func TestDispatchModeAsksForWorkOnlyOnceItsWarmIsDone(t *testing.T) {
	// The hydration ends after the third half-second, at 60 s of uptime.
	calls := runTailWith(t, "dispatch", `: > $HR/.warming; uptime_s() { echo 60; }; n=0; sleep() { echo "sleep $*" >> $log; n=$((n+1)); [ $n -lt 3 ] || rm -f $HR/.warming; }`, "200")
	want := []string{"sleep 0.5", "sleep 0.5", "sleep 0.5", "mark warm.ready", "call agent /run/superci-agent.py"}
	if got := strings.Join(calls[:min(len(calls), len(want))], "|"); got != strings.Join(want, "|") {
		t.Errorf("calls:\n%s\nwant them to begin:\n%s", strings.Join(calls, "\n"), strings.Join(want, "\n"))
	}
}

// A warm that never ends holds the machine until its uptime reaches the cap, then it asks for work.
func TestDispatchModeWaitsForAWarmThatNeverEndsUntilItsUptimeReachesTheCap(t *testing.T) {
	// Booted 60 s ago; each half-second sleep here moves the uptime on a second, so the cap comes in cap-60 sleeps.
	calls := runTailWith(t, "dispatch", `: > $HR/.warming; u=60; uptime_s() { echo $u; }; sleep() { echo "sleep $*" >> $log; u=$((u+1)); }`, "200")
	sleeps, asked := 0, false
	for _, c := range calls {
		if c == "sleep 0.5" && !asked {
			sleeps++
		}
		if c == "call agent /run/superci-agent.py" {
			asked = true
		}
	}
	if want := int(warmCap.Seconds()) - 60; !asked || sleeps != want {
		t.Errorf("slept %d times before asking (asked %v), want %d: the cap is %s of uptime", sleeps, asked, want, warmCap)
	}
}

// The dispatcher ends a machine that has not come to /work bootGrace after its launch call. A machine that waits out its
// warm cap still comes in time after a slow launch (90 s from the call to the kernel's start, spot at its worst) and
// the agent's fetch (30 s).
func TestTheWarmCapLeavesASlowlyLaunchedMachineInsideBootGrace(t *testing.T) {
	if worst := 90*time.Second + warmCap + 30*time.Second; worst >= bootGrace {
		t.Errorf("a slow machine first asks %s after its launch call, at or past bootGrace %s", worst, bootGrace)
	}
	if got := renderBoot(t, bootScript, "dispatch"); !strings.Contains(got, `-lt `+strconv.Itoa(int(warmCap.Seconds()))+` ]`) {
		t.Error("the dispatch tail does not wait on warmCap's value")
	}
}

// The mark is made before the hydration starts, so the wait cannot miss it, and hydrate() removes it when it ends.
func TestTheWarmMarkIsMadeBeforeTheHydrationStartsAndHydrateRemovesIt(t *testing.T) {
	got := renderBoot(t, bootScript, "dispatch")
	made, started := strings.Index(got, `: > "$HR/.warming"`), strings.Index(got, `setsid bash -c "$(declare -f hydrate); hydrate" &`)
	if made < 0 || started < 0 || made > started {
		t.Errorf("the mark is made at %d, the hydration started at %d: want the mark first", made, started)
	}
	body := got[strings.Index(got, "hydrate() {"):started]
	if !strings.Contains(body, `rm -f "$HR/.pid" "$HR/.warming"`) {
		t.Error("hydrate() does not remove the mark when it ends")
	}
}
