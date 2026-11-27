package main

import (
	"os/exec"
	"testing"
)

// The identity scripts end to end, with AWS, SSH, config.sh and the listener's admin endpoint stubbed: deploy/identity_test.sh.
func TestTheIdentityScriptsRegisterAtABoundedRateAndRestoreWhatTheyStored(t *testing.T) {
	cmd := exec.Command("bash", "deploy/identity_test.sh")
	cmd.Env = append(cmd.Environ(), "AWS_ACCESS_KEY_ID=", "AWS_SECRET_ACCESS_KEY=", "AWS_SESSION_TOKEN=")
	out, err := cmd.CombinedOutput()
	t.Logf("%s", out)
	if err != nil {
		t.Fatal(err)
	}
}
