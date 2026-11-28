package main

import (
	"os/exec"
	"testing"
)

// bench/replay.sh against a local origin, in a temporary repository: bench/replay_test.sh.
func TestReplayScript(t *testing.T) {
	cmd := exec.Command("bash", "bench/replay_test.sh")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bench/replay_test.sh: %v\n%s", err, out)
	}
	t.Logf("%s", out)
}
