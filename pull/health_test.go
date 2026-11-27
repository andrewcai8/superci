package main

import (
	"os/exec"
	"testing"
)

// health.sh end to end with curl, systemctl and aws faked and the clock set by the test: deploy/health_test.sh.
func TestTheHealthCheckRestartsAHungListenerAndReplacesTheInstanceOnlyWhenARestartDidNotCureIt(t *testing.T) {
	out, err := exec.Command("bash", "deploy/health_test.sh").CombinedOutput()
	t.Logf("%s", out)
	if err != nil {
		t.Fatal(err)
	}
}
