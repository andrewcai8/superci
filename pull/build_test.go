package main

import (
	"os/exec"
	"testing"
)

// build.sh and build-check.sh with AWS, ssh and the host's tools stubbed: deploy/build_test.sh.
func TestTheBuildRunsItsTestsUnderTheMachinesPython(t *testing.T) {
	cmd := exec.Command("bash", "deploy/build_test.sh")
	cmd.Env = append(cmd.Environ(), "AWS_ACCESS_KEY_ID=", "AWS_SECRET_ACCESS_KEY=", "AWS_SESSION_TOKEN=")
	out, err := cmd.CombinedOutput()
	t.Logf("%s", out)
	if err != nil {
		t.Fatal(err)
	}
}
