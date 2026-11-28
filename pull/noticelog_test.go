package main

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// The listener keeps the instance-action IMDS gave the machine, so a notice can be checked afterwards against EC2's
// record of the interruption (Nov 28: ten notices whose spot requests read instance-terminated-by-user could not be).
func TestAnAcceptedSpotNoticeLogsItsInstanceAction(t *testing.T) {
	buf := logs(t)
	store := &fakeS3{objects: map[string]string{}}
	p := startDispatcher(t, store, identityTags(), spotSet("i-0m1"))
	h, _ := noticeServer(p, trusting{})
	at := time.Now().Add(2 * time.Minute).UTC().Truncate(time.Second)
	if code := postNotice(h, "i-0m1", instanceAction(at)); code != http.StatusNoContent {
		t.Fatalf("notice: %d, want 204", code)
	}
	var line string
	for _, l := range strings.Split(buf.String(), "\n") {
		if strings.Contains(l, `msg="spot notice"`) {
			line = l
		}
	}
	if line == "" || !strings.Contains(line, "action=") || !strings.Contains(line, at.Format(time.RFC3339)) || !strings.Contains(line, "terminate") {
		t.Fatalf("the notice's log line %q, want its instance-action (terminate at %s)", line, at.Format(time.RFC3339))
	}
}
