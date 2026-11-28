package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// A job on superci makes files with the mask a job on Blacksmith has (022), whatever mask boot was started with: under
// the 077 boot used to set, dpkg-deb refused a package's control directory (700) and tests that make a file 444 read
// it back 400 (Nov 28). Boot's own secrets stay private by their own modes.

// The job's path in boot.sh, from a caller's mask of 077: boot's own mask statements before the job's branch, then the
// branch, with the agent (dispatch) or the runner (scale set) recording the mask it starts under.
func TestTheJobStartsUnderBlacksmithsMaskInBothModes(t *testing.T) {
	for _, mode := range []string{"dispatch", "scaleset"} {
		got := renderBoot(t, bootScript, mode)
		i := strings.Index(got, "# Dispatch mode (pull/agent.py)")
		if i < 0 {
			t.Fatal("no dispatch branch")
		}
		masks := strings.Join(regexp.MustCompile(`(?m)^umask .*$`).FindAllString(got[:i], -1), "\n")
		dir := t.TempDir()
		log := filepath.Join(dir, "calls")
		script := `umask 077
` + masks + `
log=` + log + `
SP=` + dir + `/spool; echo 0 > $SP.sent; : > $SP
L=https://203.0.113.1:8443; PIN=sha256//pin; SENDER=1; HR=` + dir + `/hydrated
call() { case $1 in agent) echo 200 ;; jit) [ -e $SP.jit ] && echo 410 || { : > $SP.jit; echo 200; } ;; esac; }
mark() { :; }; send() { :; }; sleep() { :; }; kill() { :; }; poweroff() { :; }; rm() { :; }; cat() { :; }; cd() { :; }
python3() { echo "agent umask $(umask)" >> $log; }
sudo() { echo "runner umask $(umask)" >> $log; }
` + got[i:]
		if out, err := exec.Command("bash", "-c", script).CombinedOutput(); err != nil {
			t.Fatalf("%s: %v: %s", mode, err, out)
		}
		raw, _ := os.ReadFile(log)
		want := map[string]string{"dispatch": "agent umask 0022", "scaleset": "runner umask 0022"}[mode]
		if strings.TrimSpace(string(raw)) != want {
			t.Errorf("%s: the job started with %q, want %q", mode, strings.TrimSpace(string(raw)), want)
		}
	}
}

// What the listener answers (the runner's JIT config, the agent) is root's alone under any mask, even when install,
// which makes the file 600 before curl writes it, fails; and a call whose answer is thrown away writes no file.
func TestBootsAnswersFromTheListenerArePrivateUnderEitherMask(t *testing.T) {
	got := renderBoot(t, bootScript, "dispatch")
	fn := regexp.MustCompile(`(?s)\ncall\(\) \{\n.*?\n\}\n`).FindString(got)
	if fn == "" {
		fn = regexp.MustCompile(`\ncall\(\) \{[^\n]*\}\n`).FindString(got)
	}
	if fn == "" {
		t.Fatal("boot.sh has no call function")
	}
	for _, c := range []struct{ mask, install string }{{"022", ""}, {"077", ""}, {"000", ""},
		{"022", "install() { return 1; }"}, {"000", "install() { return 1; }"}} {
		mask, what := c.mask, "umask "+c.mask
		if c.install != "" {
			what += " with install failing"
		}
		dir := t.TempDir()
		script := `umask ` + mask + `
` + c.install + `
curl() { local o; while [ $# -gt 0 ]; do [ "$1" = -o ] && o=$2; shift; done; [ "$o" = /dev/null ] || echo answer > "$o"; echo 200; }
` + fn + `
call jit "$1/jit" > /dev/null
call agent "$1/agent.py" > /dev/null
call notice /dev/null -H 'X-Superci-Action: x' > /dev/null
`
		if out, err := exec.Command("bash", "-c", script, "_", dir).CombinedOutput(); err != nil {
			t.Fatalf("%s: %v: %s", what, err, out)
		}
		for _, f := range []string{"jit", "agent.py"} {
			st, err := os.Stat(filepath.Join(dir, f))
			if err != nil {
				t.Fatalf("%s: %s: %v", what, f, err)
			}
			if m := st.Mode().Perm(); m != 0o600 {
				t.Errorf("%s: %s is %o, want 600", what, f, m)
			}
		}
	}
}

// The agent's job path: the job-started hook and the Worker start with mask 022 and the runner's settings are written
// 600, whatever mask the agent runs under.
func TestTheWorkerStartsUnderBlacksmithsMaskAndTheRunnerFileIsPrivate(t *testing.T) {
	runAgent(t, "start_job", "hook", "worker")
}

// agent.py --probe, which the release's parity check reads the job environment through, runs as the Worker runs.
func TestTheParityProbeRunsAsTheWorkerRuns(t *testing.T) {
	runAgent(t, "probe", "probe")
}

// runAgent runs the agent's start_job (the hook and the Worker) or its probe under masks 022 and 077, with a home whose
// scripts write the mask they started under to the files named, and checks each is 0022 and, after a job, that .runner
// is 600.
func runAgent(t *testing.T, what string, files ...string) {
	t.Helper()
	for _, mask := range []string{"022", "077"} {
		home := t.TempDir()
		if err := os.MkdirAll(filepath.Join(home, "bin"), 0o755); err != nil {
			t.Fatal(err)
		}
		put := func(path, body string) {
			if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		put(filepath.Join(home, "bin", "Runner.Worker"), "#!/bin/bash\numask > \"$HOME/worker\"\n")
		put(filepath.Join(home, "superci-hydrate-stop.sh"), "#!/bin/bash\numask > \"$HOME/hook\"\n")
		put(filepath.Join(home, "probe.sh"), "umask > \"$HOME/probe\"\n")
		put(filepath.Join(home, "marks"), "")
		put(filepath.Join(home, "environment"), "PATH=\"/usr/bin:/bin\"\n")
		run := fmt.Sprintf(`import json, os, sys
os.umask(0o%s)
sys.path.insert(0, %q)
import agent
home = sys.argv[1]
a = agent.Agent(agent.Config(listener="https://127.0.0.1:1", pin="sha256//pin", home=home, user=None, spool=os.path.join(home, "marks"),
                             environment=os.path.join(home, "environment"), first_message_wait=60))
if sys.argv[2] == "probe":
    sys.exit(a.probe(os.path.join(home, "probe.sh")))
a.start_job(json.dumps({"runner": "{}", "token": "t"}))
if a.proc is None or a.proc.wait() != 0:
    sys.exit("the Worker did not run")
a.worker_done.set()
`, mask, mustAbs(t, "."))
		cmd := exec.Command("python3", "-I", "-c", run, home, what)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("umask %s: %v: %s", mask, err, out)
		}
		for _, f := range files {
			raw, err := os.ReadFile(filepath.Join(home, f))
			if err != nil {
				t.Fatalf("umask %s: %s: %v", mask, f, err)
			}
			if got := strings.TrimSpace(string(raw)); got != "0022" {
				t.Errorf("agent under umask %s: the %s started with umask %s, want 0022", mask, f, got)
			}
		}
		if what != "start_job" {
			continue
		}
		st, err := os.Stat(filepath.Join(home, ".runner"))
		if err != nil {
			t.Fatal(err)
		}
		if m := st.Mode().Perm(); m != 0o600 {
			t.Errorf("agent under umask %s: .runner is %o, want 600", mask, m)
		}
	}
}

func mustAbs(t *testing.T, p string) string {
	t.Helper()
	a, err := filepath.Abs(p)
	if err != nil {
		t.Fatal(err)
	}
	return a
}
