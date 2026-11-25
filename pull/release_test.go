package main

import (
	"os/exec"
	"testing"
)

// The release scripts end to end, with AWS and the listener's SSH stubbed: deploy/release_test.sh.
func TestTheReleaseScriptsReleaseACommitOfMainAndRefuseAnythingElse(t *testing.T) {
	cmd := exec.Command("bash", "deploy/release_test.sh")
	cmd.Env = append(cmd.Environ(), "AWS_ACCESS_KEY_ID=", "AWS_SECRET_ACCESS_KEY=", "AWS_SESSION_TOKEN=")
	out, err := cmd.CombinedOutput()
	t.Logf("%s", out)
	if err != nil {
		t.Fatal(err)
	}
}
