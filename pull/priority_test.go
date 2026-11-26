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

// scarce is a budget that has saved up one token and a fifth: its rate is 0.5/s and 2.4 s have passed. The reserve
// is the given number of tokens.
func scarce(reserve int) *Budget {
	c := reg()
	c.Reserve, c.CeilingRate, c.CeilingBurst = reserve, 0.5, 3
	return NewBudget(c, testNow.Add(-2400*time.Millisecond))
}

// filled is a budget whose bucket holds n tokens at testNow, at 1/s.
func filled(n, reserve int) *Budget {
	c := reg()
	c.Reserve, c.CeilingRate, c.CeilingBurst = reserve, 1, n
	return NewBudget(c, testNow.Add(-time.Duration(n)*time.Second))
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

func TestABatchAskGivesWayWhileACriticalMachineWaits(t *testing.T) {
	b := scarce(0)
	critical := Asker{ID: "i-0001", Launched: testNow.Add(time.Minute), P: ForJob}
	batch := Asker{ID: "i-0000", Launched: testNow, P: ForBatch}
	if ok, why := b.Try(testNow.Add(-2*time.Second), critical); ok || why != DeniedTokens { // 0.2 tokens saved
		t.Fatalf("critical ask on 0.2 tokens: %v %v", ok, why)
	}
	// A token arrives. The batch machine launched earlier, but the critical machine that waits comes first.
	later := testNow.Add(600 * time.Millisecond) // 1.5 tokens
	if ok, why := b.Try(later, batch); ok || why != DeniedQueued {
		t.Fatalf("batch took the token a waiting critical machine is owed: %v %v", ok, why)
	}
	if ok, _ := b.Try(later, critical); !ok {
		t.Fatal("the critical machine found no token")
	}
	if ok, _ := b.Try(later.Add(2*time.Second), batch); !ok { // 1.5 tokens, nothing ahead
		t.Fatal("batch still gives way after the critical machine was served")
	}
}

func TestAMachineThatStoppedAskingHoldsNothing(t *testing.T) {
	c := reg()
	c.CeilingRate, c.CeilingBurst = 0.5, 1 // one token at most, so a place in the queue decides who takes it
	b := NewBudget(c, testNow.Add(-2400*time.Millisecond))
	b.Try(testNow.Add(-2*time.Second), Asker{ID: "i-0001", Launched: testNow, P: ForJob}) // turned away
	if ok, why := b.Try(testNow, Asker{P: ForBatch}); ok || why != DeniedQueued {
		t.Fatalf("batch took a token while a critical machine waited: %v %v", ok, why)
	}
	if !b.Take(testNow.Add(queueHold+time.Second), ForBatch) {
		t.Fatal("batch is still held after the critical machine stopped asking")
	}
}

func TestBatchBorrowsTheTokensTheReserveLeavesAndNeverTheReserve(t *testing.T) {
	b := filled(9, 2)
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
	b := filled(3, 100)
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
		"reserve": `{"registration": {"cap": 1500, "window_s": 300, "pool_share": 0.9, "ceiling_rate": 2, "ceiling_burst": 50, "recover_s": 120, "critical_reserve": -1}}`,
		"ceiling": `{"registration": {"cap": 1500, "window_s": 300, "pool_share": 0.9, "ceiling_rate": 0, "ceiling_burst": 50, "recover_s": 120, "critical_reserve": 2}}`,
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
	b := filled(9, 2)
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
	b := filled(9, 2)
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

func TestALeaseComesAfterTheMachinesOfItsClassThatWait(t *testing.T) {
	b := scarce(0)
	b.Try(testNow.Add(-2*time.Second), Asker{ID: "i-0000", Launched: testNow, P: ForBatch}) // turned away: it waits
	if l := b.Lease(testNow, 5, time.Minute); l.Granted != 0 {
		t.Errorf("a lease took the one token a waiting batch machine is owed: %+v", l)
	}
	if ok, _ := b.Try(testNow, Asker{ID: "i-0000", Launched: testNow, P: ForBatch}); !ok {
		t.Error("the waiting batch machine found no token")
	}
}

func TestALeaseNeverTakesAWaitingMachinesTokenNorAsksRightAfterARefusal(t *testing.T) {
	b := scarce(0)
	b.Try(testNow.Add(-2*time.Second), Asker{ID: "i-0000", Launched: testNow, P: ForJob}) // turned away: it waits
	if l := b.Lease(testNow, 5, time.Minute); l.Granted != 0 || l.ID != "" {
		t.Errorf("a lease took the one token a waiting critical machine is owed: %+v", l)
	}
	refused := filled(9, 0)
	refused.Done(testNow, Refused, 0)
	if l := refused.Lease(testNow.Add(time.Second), 5, time.Minute); l.Granted != 0 {
		t.Errorf("a lease granted a second after GitHub refused a registration: %+v", l)
	}
}

func TestTheAdminEndpointLeasesAndSettles(t *testing.T) {
	c := reg()
	c.Reserve, c.CeilingBurst = 2, 9
	b := NewBudget(c, time.Now().Add(-9*time.Second))
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
