package main

import (
	"os/exec"
	"testing"
)

// image.sh end to end with EC2 and ssh stubbed: the hibernation-capable image is built once from the pin, proved by hibernating
// and resuming a machine and by booting one without hibernation as the pin boots, and tagged and copied only then:
// deploy/image_test.sh.
func TestTheImageScriptBuildsAProvedHibernationImageAndFailsClosed(t *testing.T) {
	cmd := exec.Command("bash", "deploy/image_test.sh")
	cmd.Env = append(cmd.Environ(), "AWS_ACCESS_KEY_ID=", "AWS_SECRET_ACCESS_KEY=", "AWS_SESSION_TOKEN=")
	out, err := cmd.CombinedOutput()
	t.Logf("%s", out)
	if err != nil {
		t.Fatal(err)
	}
}
