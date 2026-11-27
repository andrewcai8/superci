package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// A shim whose connection broke before PAIRED replays its job on a new one: the replay takes the queued job's place.
func TestAReplayedJobTakesItsQueuedPlaceAndIsNeverASecondJob(t *testing.T) {
	store := &fakeS3{objects: map[string]string{}}
	p := startDispatcher(t, store, identityTags(), dispatchSet("i-0m1", "i-0m2"))
	first := p.shim(t)
	msg := jobMessage(jobA, 7, "unit")
	first.sendJob(msg)
	waitUntil(t, "the job waits", func() bool { return p.st.Snapshot(time.Now()).JobsWaiting == 1 })
	replay := p.shim(t)
	replay.sendJob(msg)
	if !first.ended(2 * time.Second) {
		t.Fatal("the replaced connection was kept")
	}
	if n := p.st.Snapshot(time.Now()).JobsWaiting; n != 1 {
		t.Fatalf("%d jobs wait after the replay, want the one job", n)
	}
	m1 := p.machine(t, "i-0m1")
	m1.job()
	if got := m1.pipeBytes(len(msg)); string(got) != string(msg) {
		t.Fatal("the machine did not get the job")
	}
	m2 := p.machine(t, "i-0m2")
	if err := m2.quiet(300 * time.Millisecond); err != nil {
		t.Fatalf("a second machine was paired with the job: %v", err)
	}
	if inst, _ := replay.paired(); inst != "i-0m1" {
		t.Fatalf("the replay was paired with %s", inst)
	}
}

// An identity's runner is registered once and kept. No set's listing logic may take it for a machine's runner or an
// orphan, and nothing removes it.
func TestIdentityRunnersAreNeverAMachinesRunnerAnOrphanOrRemoved(t *testing.T) {
	now := time.Now()
	st := NewState("superci-4cpu", 0, 10, []string{"us-east-1"}, now.Add(-time.Hour))
	st.Observe(stats(0, 0), now)
	st.Seen("us-east-1", []*Machine{{ID: "i-0aaa", Set: "superci-4cpu", Region: "us-east-1", Launched: now.Add(-10 * time.Minute)}}, now.Add(-time.Minute), now)
	l := &Listener{sets: []*Set{{spec: SetSpec{Name: "superci-4cpu"}, state: st}}, dsets: []*DispatchSet{dispatchSet()}}
	listing := map[string]ListedRunner{
		"superci-id-4cpu-17":  {ID: 1, Name: "superci-id-4cpu-17"},
		"superci-id-4cpu-18":  {ID: 2, Name: "superci-id-4cpu-18", Busy: true},
		"superci-id-8cpu-3":   {ID: 3, Name: "superci-id-8cpu-3"},
		"superci-4cpu-i-0aaa": {ID: 4, Name: "superci-4cpu-i-0aaa"},
		"superci-4cpu-i-0bbb": {ID: 5, Name: "superci-4cpu-i-0bbb"},
		probePrefix + fmt.Sprint(now.Add(-time.Hour).UnixNano()): {ID: 6, Name: probePrefix + fmt.Sprint(now.Add(-time.Hour).UnixNano())},
	}
	gh := &registry{runners: map[int64]string{}}
	for _, r := range listing {
		gh.runners[r.ID] = r.Name
	}
	for range 3 {
		l.sweep(context.Background(), gh, listing, now)
	}
	var left []string
	for _, name := range gh.runners {
		left = append(left, name)
	}
	slices.Sort(left)
	if want := []string{"superci-4cpu-i-0aaa", "superci-id-4cpu-17", "superci-id-4cpu-18", "superci-id-8cpu-3"}; !slices.Equal(left, want) {
		t.Fatalf("left registered %v, want the machine's runner and every identity (the orphan and the probe removed)", left)
	}
	if id, name := st.RunnerRef(st.Machine("i-0aaa")); id != 4 || name != "superci-4cpu-i-0aaa" {
		t.Fatalf("the machine's runner is %d %s", id, name)
	}
	if n := st.Orphans(now.Add(time.Hour)); n != 1 {
		t.Fatalf("%d orphans, want the one stale runner of the set (no identity)", n)
	}
}

// A dispatch set launches for the jobs waiting for a machine, the jobs running and its pool, less what it has.
func TestADispatchSetLaunchesForItsWaitingAndRunningJobsAndItsPool(t *testing.T) {
	now := time.Now()
	st := NewDispatchSet(SetSpec{Name: "superci-4cpu", Max: 10, Dispatch: true}, nil, []string{"us-east-1"}, now.Add(-time.Hour))
	if _, n := st.Deficit(now); n != 0 {
		t.Fatalf("launched %d before every region was listed", n)
	}
	st.Seen("us-east-1", nil, now, now)
	st.SetPool(2)
	if _, n := st.Deficit(now); n != 2 {
		t.Fatalf("launched %d with no job, want the pool of 2", n)
	}
	for i := range 3 {
		st.enqueue(&session{set: st, arrived: now}, jobHead{ID: fmt.Sprintf("job-%d", i)}, now)
	}
	st.pairs["i-0run"] = &session{phase: running}
	if _, n := st.Deficit(now); n != 4 {
		t.Fatalf("launched %d more, want 3 waiting + 1 running + 2 pool - 2 asked", n)
	}
	st.SetCapped(true)
	if _, n := st.Deficit(now); n != 0 {
		t.Fatalf("launched %d under the spend cap", n)
	}
	st.SetCapped(false)
	st.SetMax(6)
	if _, n := st.Deficit(now); n != 0 {
		t.Fatalf("launched %d past the set's most machines", n)
	}
}

// A dispatch machine ends when its job ends, when it has had no connection past the window, past its ages, and as
// surplus; an ending machine is terminated, with no fence (it holds no runner).
func TestADispatchMachineEndsWhenItsJobEndsOrItHasNoConnectionOrPastItsAge(t *testing.T) {
	now := time.Now()
	st := NewDispatchSet(SetSpec{Name: "superci-4cpu", Max: 10, Dispatch: true}, nil, []string{"us-east-1"}, now.Add(-time.Hour))
	at := func(id string, launched time.Duration) *Machine {
		return &Machine{ID: id, Set: "superci-4cpu", Region: "us-east-1", Launched: now.Add(-launched), Dispatch: true}
	}
	st.Seen("us-east-1", []*Machine{at("i-0quiet", 10*time.Minute), at("i-0new", time.Minute), at("i-0old", 9*time.Hour),
		at("i-0idle1", 20*time.Minute), at("i-0idle2", 15*time.Minute), at("i-0idle3", 10*time.Minute)}, now.Add(-4*time.Minute), now.Add(-4*time.Minute))
	for _, id := range []string{"i-0old", "i-0idle1", "i-0idle2", "i-0idle3"} {
		conn, _ := net.Pipe()
		st.waiting(st.machines[id], &wire{conn: conn, done: make(chan struct{})}, now)
	}
	st.SetPool(1)
	b := dispatchBounds{maxAge: 8 * time.Hour, life: 15 * time.Hour, job: 6 * time.Hour, window: 3 * time.Minute}
	ended := func(at time.Time) []string {
		end, _ := st.Reconcile(at, b)
		var ids []string
		for _, m := range end {
			ids = append(ids, m.ID)
		}
		slices.Sort(ids)
		return ids
	}
	if got, want := ended(now), []string{"i-0old", "i-0quiet"}; !slices.Equal(got, want) {
		t.Fatalf("ended %v, want %v: one with no connection past the window, one that waited past its age", got, want)
	}
	if got := ended(now.Add(10 * time.Second)); len(got) != 0 {
		t.Fatalf("surplus ended %v before 20 s", got)
	}
	if got, want := ended(now.Add(30*time.Second)), []string{"i-0idle2", "i-0idle3", "i-0new"}; !slices.Equal(got, want) {
		t.Fatalf("surplus ended %v, want %v: the newest three of four with no job, for a pool of 1", got, want)
	}
	if got := ended(now.Add(40 * time.Second)); len(got) != 0 {
		t.Fatalf("terminated %v again within a minute", got)
	}
	if got, want := ended(now.Add(2*time.Minute)), []string{"i-0idle2", "i-0idle3", "i-0new", "i-0old", "i-0quiet"}; !slices.Equal(got, want) {
		t.Fatalf("a minute on terminated %v, want %v again while EC2 lists them", got, want)
	}
}

// A size moving between a scale set and the identity plane keeps machines of both for a while: each set takes only
// its own kind, by the dispatch tag.
func TestScaleSetAndDispatchMachinesOfOneSizeAreNeverMixed(t *testing.T) {
	tag := func(k, v string) ec2types.Tag { return ec2types.Tag{Key: aws.String(k), Value: aws.String(v)} }
	listed := func(id string, tags ...ec2types.Tag) *Machine {
		return listedMachine(ec2types.Instance{InstanceId: aws.String(id), Tags: append(tags, tag("superci-pull-set", "superci-4cpu"))}, "us-east-1")
	}
	jit, disp := listed("i-0jit"), listed("i-0disp", tag(tagMode, "dispatch"))
	other := listedMachine(ec2types.Instance{InstanceId: aws.String("i-0other"), Tags: []ec2types.Tag{tag("superci-pull-set", "superci-8cpu"), tag(tagMode, "dispatch")}}, "us-east-1")
	draining := &Set{spec: SetSpec{Name: "superci-4cpu"}, drain: true}
	d := dispatchSet()
	scaled, dispatched, strays := sortMachines([]*Machine{jit, disp, other}, []*Set{draining}, []*DispatchSet{d})
	if len(scaled[draining]) != 1 || scaled[draining][0] != jit {
		t.Fatalf("the draining scale set got %v, want only its runner's machine", scaled[draining])
	}
	if len(dispatched[d]) != 1 || dispatched[d][0] != disp {
		t.Fatalf("the dispatch set got %v, want only the machine tagged dispatch", dispatched[d])
	}
	if len(strays) != 1 || strays[0] != other {
		t.Fatalf("strays %v, want the dispatch machine of no dispatch set", strays)
	}
	_, _, strays = sortMachines([]*Machine{jit, disp}, nil, []*DispatchSet{d})
	if len(strays) != 1 || strays[0] != jit {
		t.Fatalf("with no scale set left, strays %v, want the scale set's machine (not the dispatch set's)", strays)
	}
}

// A dispatch size is one set, with no split: an identity names its size.
func TestADispatchSizeIsOneSet(t *testing.T) {
	path := t.TempDir() + "/override.json"
	write := func(body string) error {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := LoadConfig(path)
		return err
	}
	size := `{"sizes": {"superci-4cpu": {"cpu": 4, "split": %d, "max": 10, "disk_gb": 60, "types": ["m8a.xlarge"], "dispatch": true}}}`
	if err := write(fmt.Sprintf(size, 2)); err == nil || !strings.Contains(err.Error(), "split 1") {
		t.Fatalf("a split dispatch size: %v, want it refused", err)
	}
	if err := write(fmt.Sprintf(size, 1)); err != nil {
		t.Fatal(err)
	}
	cfg, _ := LoadConfig(path)
	for _, s := range cfg.Sets() {
		if s.Dispatch != (s.Name == "superci-4cpu") {
			t.Fatalf("set %s dispatch %v", s.Name, s.Dispatch)
		}
	}
}

// /agent hands the machine agent to a machine of a dispatch set, and to nothing else.
func TestTheAgentScriptGoesOnlyToADispatchMachine(t *testing.T) {
	d := NewDispatcher("superci-pull", []*DispatchSet{dispatchSet("i-0m1")}, trusting{}, &Claims{store: &fakeS3{objects: map[string]string{}}, bucket: "c"},
		identityTags(), func(context.Context, string) {})
	srv := &Server{dispatch: d}
	for id, want := range map[string]int{"i-0m1": http.StatusOK, "i-0nope": http.StatusForbidden, idHost: http.StatusForbidden} {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/agent", strings.NewReader(id)))
		if rec.Code != want {
			t.Errorf("/agent for %s: %d, want %d", id, rec.Code, want)
		}
		if want == http.StatusOK && rec.Body.String() != string(agentScript) {
			t.Errorf("/agent for %s did not answer agent.py", id)
		}
	}
}

// identityJobs answers a re-run's REST calls for runs whose jobs an identity took: every run completed, its jobs with
// their names, runner and conclusion, and the re-runs.
type identityJobs struct {
	mu     sync.Mutex
	jobs   string // the jobs of every run, as the jobs listing answers them
	reruns []string
}

func (f *identityJobs) RoundTrip(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body, status := `{"status":"completed"}`, http.StatusOK
	switch path := req.URL.Path; {
	case strings.HasSuffix(path, "/rerun"):
		f.reruns = append(f.reruns, path)
		status = http.StatusCreated
	case strings.HasSuffix(path, "/jobs"):
		body = f.jobs
	}
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
}

// An identity takes many jobs, one of each machine: a job lost with its machine is known by the machine, proved by it,
// and found in its run by its name.
func TestAnIdentitysJobLostToASpotReclaimIsReRunByItsMachineAndName(t *testing.T) {
	gh := &identityJobs{jobs: `{"jobs":[{"id":11,"name":"unit (1/2)","runner_name":"superci-id-4cpu-17","conclusion":"failure"},` +
		`{"id":12,"name":"unit (2/2)","runner_name":"superci-id-4cpu-17","conclusion":"failure"}]}`}
	rest := &REST{org: "o", http: &http.Client{Transport: gh}, token: "t", expires: time.Now().Add(time.Hour)}
	var asked []string
	r := NewRerunner(rest, func(_ context.Context, ids []string) (map[string]bool, error) {
		asked = append(asked, ids...)
		return map[string]bool{"i-0two": true}, nil
	})
	now := time.Now()
	r.Add(JobRef{Owner: "o", Repo: "r", RunID: 5, Name: "unit (1/2)", Runner: "superci-id-4cpu-17", Instance: "i-0one"}, now, false)
	r.Add(JobRef{Owner: "o", Repo: "r", RunID: 5, Name: "unit (2/2)", Runner: "superci-id-4cpu-17", Instance: "i-0two"}, now, false)
	r.pass(context.Background())
	slices.Sort(asked)
	if !slices.Equal(asked, []string{"i-0one", "i-0two"}) {
		t.Fatalf("asked EC2 about %v, want both machines", asked)
	}
	if len(gh.reruns) != 1 || !strings.HasSuffix(gh.reruns[0], "/jobs/12/rerun") {
		t.Fatalf("re-ran %v, want only job 12, the one a reclaim took", gh.reruns)
	}
}

// Each dispatch set publishes its waiting jobs, idle machines, hand-offs, retries, resumes and lost jobs.
func TestEachDispatchSetPublishesItsDispatchMetrics(t *testing.T) {
	m := Metrics{Dispatch: []DispatchSnapshot{{Set: "superci-4cpu", JobsWaiting: 3, Idle: 2, Machines: 7, OldestJobS: 40}},
		DispatchCounts: map[string]DispatchCounts{"superci-4cpu": {Handoffs: 9, Retries: 1, Resumes: 4, Lost: 2, Dropped: 5}}}
	got := map[string]float64{}
	var oldest float64
	for _, d := range metricData(m, time.Now()) {
		if len(d.Dimensions) == 1 && aws.ToString(d.Dimensions[0].Value) == "superci-4cpu" {
			got[aws.ToString(d.MetricName)] = aws.ToFloat64(d.Value)
		}
		if aws.ToString(d.MetricName) == "OldestWaitingJobAgeS" && len(d.Dimensions) == 0 {
			oldest = aws.ToFloat64(d.Value)
		}
	}
	want := map[string]float64{"JobsWaiting": 3, "DispatchIdleMachines": 2, "Machines": 7, "DispatchHandoffs": 9, "DispatchRetries": 1,
		"DispatchResumes": 4, "DispatchLostJobs": 2, "DispatchDroppedFrames": 5}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
	if oldest != 40 {
		t.Errorf("the alarm's OldestWaitingJobAgeS is %v, want the dispatch set's 40", oldest)
	}
}

// A machine of the dispatch mode left with no dispatch set, as when its size moved back to a scale set, may be running
// its job: it is not ended at 10 minutes as other strays are, only past a machine's life.
func TestADispatchStrayIsKeptWhileItMayRunItsJob(t *testing.T) {
	now := time.Now()
	l := &Listener{strays: map[string]*Machine{
		"i-0jit":  {ID: "i-0jit", Launched: now.Add(-30 * time.Minute)},
		"i-0disp": {ID: "i-0disp", Dispatch: true, Launched: now.Add(-30 * time.Minute)},
		"i-0old":  {ID: "i-0old", Dispatch: true, Launched: now.Add(-16 * time.Hour)},
	}}
	got := map[string]bool{}
	for _, m := range l.endStrays(now, 15*time.Hour) {
		got[m.ID] = true
	}
	if !got["i-0jit"] || got["i-0disp"] || !got["i-0old"] || len(got) != 2 {
		t.Fatalf("ended %v, want the scale set's stray and the dispatch stray past its life, not the one that may run its job", got)
	}
}
