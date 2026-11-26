package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/actions/scaleset"
)

var testNow = t0.Add(6 * time.Minute)

// scarce is a budget that has saved up one token and a fifth, with the others' rate known: spare for one registration.
// The reserve is two tokens.
func scarce(reserve int) *Budget {
	c := reg()
	c.Reserve = reserve
	b := NewBudget(c, testNow.Add(-2400*time.Millisecond))
	heads(b, 3.5, 6*time.Minute) // spare 0.5/s, so 2.4 s have saved 1.2 tokens
	return b
}

func TestWithSpareForOneTokenACriticalAskIsGrantedBeforeABatchAsk(t *testing.T) {
	b := scarce(2)
	if b.Take(testNow, ForBatch) {
		t.Fatal("a batch registration took the one token: the critical class's reserve is empty")
	}
	if !b.Take(testNow, ForJob) {
		t.Fatal("a critical registration was refused the one token the batch ask left")
	}
	if b.Take(testNow, ForBatch) || b.Take(testNow, ForJob) {
		t.Fatal("a second registration on one token")
	}
}

func TestABatchOrPoolAskGivesWayWhileACriticalAskWaits(t *testing.T) {
	for _, give := range []struct {
		name string
		p    Priority
	}{{"batch", ForBatch}, {"pool", ForPool}} {
		b := scarce(0)
		// A token arrives after the critical ask was turned away. The class that asks first after it must not take it.
		b.Take(testNow.Add(-2*time.Second), ForJob) // 0.2 tokens saved: turned away
		later := testNow.Add(3 * time.Second)       // 1.7 tokens saved: a token for either
		if b.Take(later, give.p) {
			t.Errorf("%s took the token a waiting critical ask was about to take", give.name)
		}
		if !b.Take(later, ForJob) {
			t.Errorf("%s: the critical ask found no token", give.name)
		}
		if !b.Take(later.Add(criticalHold), give.p) && give.p == ForBatch {
			t.Errorf("%s still gives way after the critical ask was served", give.name)
		}
	}
}

func TestACriticalAskNoLongerWaitingHoldsNothing(t *testing.T) {
	b := scarce(0)
	b.Take(testNow.Add(-2*time.Second), ForJob) // turned away
	if b.Take(testNow, ForBatch) {
		t.Fatal("batch took a token inside the hold")
	}
	if !b.Take(testNow.Add(criticalHold+time.Second), ForBatch) {
		t.Fatal("batch is still held after the critical ask stopped asking")
	}
}

func TestAServedCriticalAskReleasesTheHold(t *testing.T) {
	b := open()
	b.criticalWait = testNow.Add(-time.Second) // a critical ask was turned away a second ago
	if b.Take(testNow, ForBatch) {
		t.Fatal("batch took a token while a critical ask waited")
	}
	if !b.Take(testNow, ForJob) {
		t.Fatal("the critical ask was refused on a full bucket")
	}
	if !b.Take(testNow, ForBatch) {
		t.Fatal("batch still gives way after the critical ask was served")
	}
}

func TestBatchBorrowsTheTokensTheReserveLeavesAndNeverTheReserve(t *testing.T) {
	c := reg()
	c.Reserve = 2
	b := NewBudget(c, testNow.Add(-9*time.Second))
	heads(b, 3, 6*time.Minute) // spare 1/s: 9 tokens saved
	got := 0
	for b.Take(testNow, ForBatch) {
		got++
	}
	if got != 7 {
		t.Fatalf("batch took %d of 9 tokens, want 7: the other two are the critical reserve", got)
	}
	if !b.Take(testNow, ForJob) || !b.Take(testNow, ForJob) || b.Take(testNow, ForJob) {
		t.Fatal("the reserve is two critical registrations")
	}
}

func TestAReserveAboveTheBurstDoesNotShutBatchOutForEver(t *testing.T) {
	c := reg()
	c.Reserve = 100
	b := NewBudget(c, testNow.Add(-time.Hour))
	heads(b, 3.9, 6*time.Minute) // spare 0.1/s: a burst of 3 tokens, and a bucket an hour has filled
	if !b.Take(testNow, ForBatch) {
		t.Fatal("a reserve of 100 shuts batch out of a full bucket: it is cut to the burst less one")
	}
}

func TestJobsAreCriticalUnlessTheBatchRuleNamesThem(t *testing.T) {
	c, err := LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Registration.Reserve < 1 {
		t.Errorf("the built-in config holds no reserve for the critical class: %d", c.Registration.Reserve)
	}
	job := func(event, ref, name string) scaleset.JobMessageBase {
		return scaleset.JobMessageBase{EventName: event, JobWorkflowRef: ref, JobDisplayName: name}
	}
	for _, tc := range []struct {
		j     scaleset.JobMessageBase
		batch bool
	}{
		{job("pull_request", "o/r/.github/workflows/ci.yml@refs/pull/9/merge", "test-go"), false},
		{job("push", "o/r/.github/workflows/ci.yml@refs/heads/main", "test-go"), false},
		{job("schedule", "o/r/.github/workflows/warm.yml@refs/heads/main", "bun (harness, superci)"), true},
		{job("workflow_dispatch", "o/r/.github/workflows/load.yml@refs/heads/ci/superci-load-microvm-v2", "load"), true},
		{job("", "", ""), false},
	} {
		if got := c.Batch.Matches(tc.j); got != tc.batch {
			t.Errorf("%+v: batch %v, want %v", tc.j, got, tc.batch)
		}
	}
	r := BatchRule{WorkflowRefs: []string{`/warm\.yml@`}, Jobs: []string{`^git-seed`}}
	if err := r.compile(); err != nil {
		t.Fatal(err)
	}
	if !r.Matches(job("push", "o/r/.github/workflows/warm.yml@refs/heads/main", "x")) || !r.Matches(job("push", "", "git-seed (superci)")) || r.Matches(job("push", "", "test-go")) {
		t.Error("workflow_refs and jobs patterns do not pick out their jobs")
	}
}

func TestAnOverrideWithABadBatchPatternOrANegativeReserveIsRefused(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"pattern": `{"batch": {"jobs": ["("]}}`,
		"reserve": `{"registration": {"cap": 1500, "window_s": 300, "margin": 0.2, "pool_share": 0.9, "critical_reserve": -1}}`,
	} {
		p := filepath.Join(dir, name+".json")
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfig(p); err == nil {
			t.Errorf("%s: an override the listener cannot honour was accepted", name)
		}
	}
}

// waitingJobs puts jobs of one event each in the set's queue, in order.
func waitingJobs(s *State, at time.Time, events ...string) {
	var msgs []*scaleset.JobAvailable
	for i, e := range events {
		msgs = append(msgs, &scaleset.JobAvailable{JobMessageBase: scaleset.JobMessageBase{JobID: string(rune('a' + i)), EventName: e, QueueTime: at}})
	}
	s.Observe(&scaleset.RunnerScaleSetMessage{Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: len(events)}, JobAvailableMessages: msgs}, at)
}

func TestMachinesAreClassedByTheJobsTheyWouldTakeCriticalFirst(t *testing.T) {
	s := newState(0, 100)
	s.SetBatch(BatchRule{Events: []string{"schedule"}})
	waitingJobs(s, testNow, "schedule", "pull_request", "schedule", "pull_request", "schedule")
	launch(s, machines(7, testNow), testNow)
	var got []Priority
	for i := 0; i < 7; i++ {
		got = append(got, s.class(s.Machine(machines(7, testNow)[i].ID), 0))
	}
	want := []Priority{ForJob, ForJob, ForBatch, ForBatch, ForBatch, ForPool, ForPool}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("machines in launch order are classed %v, want %v", got, want)
		}
	}
	// One idle runner takes a critical job, so only one of the machines still serves a critical job.
	if c := []Priority{s.class(s.Machine("i-0000"), 1), s.class(s.Machine("i-0001"), 1), s.class(s.Machine("i-0002"), 1)}; c[0] != ForJob || c[1] != ForBatch || c[2] != ForBatch {
		t.Fatalf("with one idle runner: %v", c)
	}
	// With no batch job in the queue every job is critical, as before the classes.
	plain := newState(0, 100)
	plain.SetBatch(BatchRule{Events: []string{"schedule"}})
	waitingJobs(plain, testNow, "pull_request", "pull_request", "push")
	launch(plain, machines(4, testNow), testNow)
	for i := 0; i < 4; i++ {
		if c := plain.class(plain.Machine(machines(4, testNow)[i].ID), 0); c != ForJob {
			t.Fatalf("machine %d of a queue with no batch job: %v, want ForJob", i, c)
		}
	}
}

func TestAStartedBatchJobNoLongerMakesAWaitingCriticalJobsMachineABatchOne(t *testing.T) {
	b := scarce(2)
	s := newState(0, 100)
	s.SetBatch(BatchRule{Events: []string{"schedule"}})
	waitingJobs(s, testNow, "schedule", "pull_request")
	launch(s, machines(1, testNow), testNow)
	if c := s.class(s.Machine("i-0000"), 0); c != ForJob {
		t.Fatalf("with the cron job's machine not yet there, the pull request's machine is %v", c)
	}
	s.Observe(&scaleset.RunnerScaleSetMessage{
		Statistics:         &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 2, TotalRunningJobs: 1},
		JobStartedMessages: []*scaleset.JobStarted{{JobMessageBase: scaleset.JobMessageBase{JobID: "a", EventName: "schedule"}, RunnerName: "elsewhere"}},
	}, testNow)
	if c := s.class(s.Machine("i-0000"), 0); c != ForJob {
		t.Fatalf("the cron job started, so one job waits and it is the pull request's: its machine is %v, want ForJob", c)
	}
	if v := s.Ask("i-0000", testNow, b, 8*time.Hour); v != Grant {
		t.Fatalf("the pull request's machine got %v, want Grant: it may use the critical reserve", v)
	}
}

func TestAConfiguredSetKnowsTheConfigsBatchJobsFromItsStart(t *testing.T) {
	c, err := LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	s := setState(SetSpec{Name: "superci-4cpu", Max: 100}, c, []string{"us-east-1"}, t0.Add(-2*time.Hour))
	s.listings["us-east-1"] = t0.Add(-time.Hour)
	waitingJobs(s, testNow, "schedule", "pull_request")
	launch(s, machines(2, testNow), testNow)
	if a, b := s.class(s.Machine("i-0000"), 0), s.class(s.Machine("i-0001"), 0); a != ForJob || b != ForBatch {
		t.Fatalf("a cron job and a pull request's, with the built-in config: %v and %v, want critical then batch", a, b)
	}
}

func TestAMachineForABatchJobWaitsWhileTheMachineForACriticalJobIsGranted(t *testing.T) {
	b := scarce(2)
	s := newState(0, 100)
	s.SetBatch(BatchRule{Events: []string{"schedule"}})
	waitingJobs(s, testNow, "schedule", "pull_request")
	launch(s, machines(2, testNow), testNow)
	maxAge := 8 * time.Hour
	if v := s.Ask("i-0001", testNow, b, maxAge); v != Wait {
		t.Fatalf("the machine of the batch job, asking first, got %v, want Wait: one token is the critical job's", v)
	}
	if v := s.Ask("i-0000", testNow, b, maxAge); v != Grant {
		t.Fatalf("the machine of the critical job got %v, want Grant", v)
	}
	if v := s.Ask("i-0001", testNow, b, maxAge); v != Wait {
		t.Fatalf("the batch job's machine, asking again on no token, got %v", v)
	}
}

func TestALaneLeasesRegistrationsAtBatchPriorityAndSettlesThemAsOurs(t *testing.T) {
	c := reg()
	c.Reserve = 2
	b := NewBudget(c, testNow.Add(-9*time.Second))
	heads(b, 3, 6*time.Minute) // 9 tokens
	l := b.Lease(testNow, 20, time.Minute)
	if l.Granted != 7 || l.ID == "" {
		t.Fatalf("lease of 20 on 9 tokens with a reserve of 2: %+v, want 7", l)
	}
	if got := b.Snapshot(testNow); got.Pending != 7 || got.Leased != 7 {
		t.Fatalf("a lease is registrations in flight: %+v", got)
	}
	if !b.Take(testNow, ForJob) || !b.Take(testNow, ForJob) || b.Take(testNow, ForJob) {
		t.Fatal("the lane took the critical reserve")
	}
	before := b.Snapshot(testNow)
	if !b.Settle(testNow.Add(10*time.Second), l.ID, 5) {
		t.Fatal("a held lease was not settled")
	}
	after := b.Snapshot(testNow.Add(10 * time.Second))
	if after.Made != before.Made+5 || after.Leased != 0 || after.Pending != before.Pending-7 {
		t.Fatalf("settled with 5 made: before %+v after %+v", before, after)
	}
	if b.Settle(testNow, l.ID, 5) {
		t.Fatal("a lease settled twice")
	}
}

func TestALeaseThatIsNeverSettledIsReleasedWhenItExpires(t *testing.T) {
	c := reg()
	c.Reserve = 2
	b := NewBudget(c, testNow.Add(-9*time.Second))
	heads(b, 3, 6*time.Minute)
	l := b.Lease(testNow, 5, 30*time.Second)
	if l.Granted != 5 {
		t.Fatalf("%+v", l)
	}
	b.Take(testNow.Add(29*time.Second), ForJob)
	if got := b.Snapshot(testNow.Add(29 * time.Second)).Leased; got != 5 {
		t.Fatalf("a lease inside its time: %d held", got)
	}
	b.Take(testNow.Add(31*time.Second), ForJob)
	if got := b.Snapshot(testNow.Add(31 * time.Second)); got.Leased != 0 || got.Pending != 2 {
		t.Fatalf("past its time the lease holds nothing and only the two critical registrations are in flight: %+v", got)
	}
	if b.Settle(testNow.Add(32*time.Second), l.ID, 5) {
		t.Fatal("an expired lease settled")
	}
}

func TestALeaseIsRefusedWhileACriticalAskWaitsAndWhenTheCapLeavesNothing(t *testing.T) {
	b := scarce(0)
	b.Take(testNow.Add(-2*time.Second), ForJob) // turned away: a critical ask waits
	if l := b.Lease(testNow, 5, time.Minute); l.Granted != 0 || l.ID != "" {
		t.Errorf("a lease granted while a critical ask waits: %+v", l)
	}
	starved := NewBudget(reg(), testNow)
	heads(starved, 4.5, 6*time.Minute)
	if l := starved.Lease(testNow, 5, time.Minute); l.Granted != 0 {
		t.Errorf("a lease granted with others over the line: %+v", l)
	}
}

func TestTheAdminEndpointLeasesAndSettles(t *testing.T) {
	c := reg()
	c.Reserve = 2
	b := NewBudget(c, time.Now().Add(-9*time.Second))
	for s := 0; s <= 360; s += 15 { // others at 3/s, up to now
		b.Head(time.Now().Add(time.Duration(s-360)*time.Second), 7_000_000+int64(3*s))
	}
	h := Admin(func() Status { return Status{} }, func() error { return nil }, func(string, int, int) {}, b)
	post := func(target string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, target, nil))
		return w
	}
	w := post("/lease?n=3&ttl=60")
	var l Lease
	if err := json.Unmarshal(w.Body.Bytes(), &l); err != nil || w.Code != 200 || l.Granted != 3 || l.ID == "" {
		t.Fatalf("lease: %d %s", w.Code, w.Body.String())
	}
	if w := post("/lease?n=0&ttl=60"); w.Code != http.StatusBadRequest {
		t.Errorf("a lease of nothing: %d", w.Code)
	}
	if w := post("/lease/settle?id=" + l.ID + "&made=2"); w.Code != http.StatusNoContent {
		t.Errorf("settle: %d %s", w.Code, w.Body.String())
	}
	if w := post("/lease/settle?id=" + l.ID + "&made=2"); w.Code != http.StatusGone {
		t.Errorf("settling again: %d", w.Code)
	}
}
