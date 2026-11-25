package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeGitHub answers the REST calls a re-run makes: every run completed, each run's one job, and re-runs.
type fakeGitHub struct {
	mu         sync.Mutex
	conclusion map[int64]string // by run
	runner     map[int64]string // by run
	reruns     []int64          // job ids re-run
}

func (f *fakeGitHub) RoundTrip(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body, status := `{}`, http.StatusOK
	var run, job int64
	switch path := req.URL.Path; {
	case strings.HasSuffix(path, "/rerun"):
		fmt.Sscanf(path[strings.Index(path, "/jobs/")+len("/jobs/"):], "%d", &job)
		f.reruns = append(f.reruns, job)
		status = http.StatusCreated
	case strings.HasSuffix(path, "/jobs"):
		fmt.Sscanf(path[strings.Index(path, "/runs/")+len("/runs/"):], "%d", &run)
		body = fmt.Sprintf(`{"jobs":[{"id":%d,"runner_name":%q,"conclusion":%q}]}`, run*10, f.runner[run], f.conclusion[run])
	default:
		body = `{"status":"completed"}`
	}
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
}

// OPS-12: a job that failed by itself, or that a person cancelled, was re-run when its machine left before
// GitHub reported the job complete.
func TestOnlyAFailedJobASpotReclaimTookIsReRun(t *testing.T) {
	gh := &fakeGitHub{conclusion: map[int64]string{1: "failure", 2: "failure", 3: "cancelled", 4: "failure"},
		runner: map[int64]string{1: "set-i-a", 2: "set-i-b", 3: "set-i-c", 4: "set-i-d"}}
	rest := &REST{org: "o", http: &http.Client{Transport: gh}, token: "t", expires: time.Now().Add(time.Hour)}
	var asked []string
	r := NewRerunner(rest, func(_ context.Context, ids []string) (map[string]bool, error) {
		asked = append(asked, ids...)
		return map[string]bool{"i-a": true, "i-c": true}, nil
	})
	now := time.Now()
	r.Add(JobRef{Owner: "o", Repo: "r", RunID: 1, Name: "reclaimed, failed", Runner: "set-i-a"}, now, false)
	r.Add(JobRef{Owner: "o", Repo: "r", RunID: 2, Name: "failed by itself", Runner: "set-i-b"}, now, false)
	r.Add(JobRef{Owner: "o", Repo: "r", RunID: 3, Name: "reclaimed, cancelled", Runner: "set-i-c"}, now, false)
	r.Add(JobRef{Owner: "o", Repo: "r", RunID: 4, Name: "on a noticed machine", Runner: "set-i-d"}, now, true)
	r.pass(context.Background())
	if len(asked) != 3 {
		t.Fatalf("asked EC2 about %v: want the three machines without AWS's notice", asked)
	}
	if len(gh.reruns) != 2 || !(gh.reruns[0] == 10 && gh.reruns[1] == 40 || gh.reruns[0] == 40 && gh.reruns[1] == 10) {
		t.Fatalf("re-ran jobs %v: want the failed job of the reclaimed machine (10) and of the noticed one (40)", gh.reruns)
	}
	r.pass(context.Background())
	if len(gh.reruns) != 2 {
		t.Fatalf("re-ran again: %v", gh.reruns)
	}
}

func TestARunnerNamesItsMachine(t *testing.T) {
	for name, want := range map[string]string{"superci-4cpu-i-0abc": "i-0abc", "superci-4cpu-2-i-0abc-1f2e": "i-0abc", "other": ""} {
		if got := instanceOf(name); got != want {
			t.Fatalf("instanceOf(%q) = %q, want %q", name, got, want)
		}
	}
}
