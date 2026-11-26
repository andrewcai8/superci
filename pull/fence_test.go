package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/actions/scaleset"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

// fakeRegistrar registers just-in-time runners as GitHub does: a name is refused while a runner of it exists, and an
// ephemeral runner is deleted when its job ends, which frees its name.
type fakeRegistrar struct {
	mu      sync.Mutex
	runners map[string]bool
	made    int
	fail    int
}

func (g *fakeRegistrar) GenerateJitRunnerConfig(_ context.Context, in *scaleset.RunnerScaleSetJitRunnerSetting, _ int) (*scaleset.RunnerScaleSetJitRunnerConfig, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.fail > 0 {
		g.fail--
		return nil, fmt.Errorf("401 Unauthorized: %w", scaleset.UnauthorizedError)
	}
	if g.runners[in.Name] {
		return nil, scaleset.RunnerExistsError
	}
	g.runners[in.Name] = true
	g.made++
	return &scaleset.RunnerScaleSetJitRunnerConfig{Runner: &scaleset.RunnerReference{ID: g.made, Name: in.Name}, EncodedJITConfig: "config-of-" + in.Name}, nil
}

func (g *fakeRegistrar) jobEnded(name string) { g.mu.Lock(); delete(g.runners, name); g.mu.Unlock() }

// fakeS3 keeps objects as S3 does for a create with If-None-Match: *: the first create wins, a later one is 412. It
// can fail a write before it lands, or lose the answer of one that landed.
type fakeS3 struct {
	mu      sync.Mutex
	objects map[string]string
	fail    int
	lose    int
}

func (f *fakeS3) PutObject(_ context.Context, in *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail > 0 {
		f.fail--
		return nil, errors.New("connection reset")
	}
	key := aws.ToString(in.Key)
	if _, ok := f.objects[key]; ok && aws.ToString(in.IfNoneMatch) == "*" {
		return nil, &smithy.GenericAPIError{Code: "PreconditionFailed", Message: "At least one of the pre-conditions you specified did not hold"}
	}
	body, _ := io.ReadAll(in.Body)
	f.objects[key] = string(body)
	if f.lose > 0 {
		f.lose--
		return nil, errors.New("timeout awaiting the answer")
	}
	return &s3.PutObjectOutput{}, nil
}

func (f *fakeS3) GetObject(_ context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body, ok := f.objects[aws.ToString(in.Key)]
	if !ok {
		return nil, &smithy.GenericAPIError{Code: "NoSuchKey"}
	}
	return &s3.GetObjectOutput{Body: io.NopCloser(strings.NewReader(body))}, nil
}

func (f *fakeS3) claims() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for k := range f.objects {
		if strings.HasPrefix(k, "issued/") {
			n++
		}
	}
	return n
}

// trusting takes the body as the instance id a signed document would name.
type trusting struct{}

func (trusting) Verify(body []byte) (InstanceDocument, error) {
	return InstanceDocument{InstanceID: string(body), Region: "us-east-1"}, nil
}

// listenerProcess is one listener process serving set "set": started an hour ago, its queue known, and its last
// listing of EC2 begun a minute ago, which shows machine i-0000 with the issued tag only when tagged says EC2 shows it.
func listenerProcess(gh *fakeRegistrar, store *fakeS3, tagged bool) *Server {
	now := time.Now()
	st := NewState("set", 0, 10, []string{"us-east-1"}, now.Add(-time.Hour))
	st.Observe(stats(1, 0), now)
	listed := machines(1, now.Add(-10*time.Minute))
	listed[0].Issued, listed[0].Tagged = tagged, tagged
	st.Seen("us-east-1", listed, now.Add(-time.Minute), now)
	return &Server{sets: map[string]*Set{"set": {spec: SetSpec{Name: "set"}, id: 1, state: st}}, budget: open(), verifier: trusting{}, client: gh,
		refresh: func(context.Context, string) {}, claim: (&Claims{store: store, bucket: "claims"}).Take,
		issue: func(context.Context, *Machine) error { return nil }, slots: make(chan struct{}, 4), maxAge: func() time.Duration { return 8 * time.Hour }}
}

func jitAsk(h *Server, id string) (int, string) {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/jit", strings.NewReader(id)))
	return rec.Code, rec.Body.String()
}

// E1: the grant's only cross-process fence was GitHub's unique runner name, which an ephemeral runner frees when its
// job ends. A job that keeps its machine and keeps asking then met a listener that did not know the machine was
// issued (a restart whose EC2 listing lags the tag, a listing that dropped the machine, a second listener) and got a
// second runner.
func TestAMachineGetsOneConfigEverAcrossRestartsForgettingAndASecondListener(t *testing.T) {
	gh, store := &fakeRegistrar{runners: map[string]bool{}}, &fakeS3{objects: map[string]string{}}
	configs := 0
	hostileAsks := func(who string, h *Server, n int) {
		t.Helper()
		for range n {
			code, body := jitAsk(h, "i-0000")
			switch {
			case code == http.StatusOK && body != "":
				configs++
			case code != http.StatusGone:
				t.Fatalf("%s: answered %d, want 410 to every ask after the machine's one config", who, code)
			}
		}
	}
	first := listenerProcess(gh, store, false)
	if code, body := jitAsk(first, "i-0000"); code != http.StatusOK || body != "config-of-set-i-0000" {
		t.Fatalf("first ask: %d %q, want its config", code, body)
	}
	configs++
	gh.jobEnded("set-i-0000")

	// The same process drops the machine from one listing, then a listing that lags the tag shows it again.
	st, now := first.sets["set"].state, time.Now()
	st.Seen("us-east-1", nil, now, now)
	st.Seen("us-east-1", machines(1, now.Add(-10*time.Minute)), now, now)
	hostileAsks("after a listing dropped it", first, 5)

	// A restart, then a second listener at once with it: neither listing shows the tag yet.
	restarted, second := listenerProcess(gh, store, false), listenerProcess(gh, store, false)
	hostileAsks("a restarted listener", restarted, 5)
	hostileAsks("a second listener", second, 5)

	if configs != 1 || gh.made != 1 || store.claims() != 1 {
		t.Fatalf("configs handed out %d, runners registered %d, claims %d: want one of each", configs, gh.made, store.claims())
	}
}

// E1: a claim is the grant's, not the process's. A process that forgot it issued a machine (its memory of the machine
// gone with a day-old tombstone) is refused like any other grant.
func TestAGrantThisProcessForgotCannotReuseItsClaim(t *testing.T) {
	gh, store := &fakeRegistrar{runners: map[string]bool{}}, &fakeS3{objects: map[string]string{}}
	h := listenerProcess(gh, store, false)
	if code, _ := jitAsk(h, "i-0000"); code != http.StatusOK {
		t.Fatalf("first ask: %d", code)
	}
	gh.jobEnded("set-i-0000")
	st, now := h.sets["set"].state, time.Now()
	st.Seen("us-east-1", nil, now, now)
	st.Seen("us-east-1", nil, now.Add(25*time.Hour), now.Add(25*time.Hour))
	st.Seen("us-east-1", machines(1, now.Add(-10*time.Minute)), now.Add(25*time.Hour), now.Add(25*time.Hour))
	if code, _ := jitAsk(h, "i-0000"); code != http.StatusGone || gh.made != 1 {
		t.Fatalf("asked again: %d with %d runners registered, want 410 and one", code, gh.made)
	}
}

// E1: two listeners granting one machine at the same moment: exactly one config leaves, however the asks interleave.
func TestTwoListenersAskedAtOnceHandOutOneConfig(t *testing.T) {
	for round := range 20 {
		gh, store := &fakeRegistrar{runners: map[string]bool{}}, &fakeS3{objects: map[string]string{}}
		a, b := listenerProcess(gh, store, false), listenerProcess(gh, store, false)
		var mu sync.Mutex
		var wg sync.WaitGroup
		configs := 0
		for i := range 16 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				h := a
				if i%2 == 1 {
					h = b
				}
				if code, _ := jitAsk(h, "i-0000"); code == http.StatusOK {
					mu.Lock()
					configs++
					mu.Unlock()
					gh.jobEnded("set-i-0000")
				}
			}()
		}
		wg.Wait()
		if configs != 1 || store.claims() != 1 {
			t.Fatalf("round %d: %d configs, %d claims, want 1 and 1", round, configs, store.claims())
		}
	}
}

// E1: the claim is idempotent for the grant that made it: a lost answer to the create, or a failed registration,
// leaves the machine to ask again and get its one runner; a claim that cannot be made hands out nothing.
func TestAGrantGoesOnAfterALostClaimAnswerOrAFailedRegistrationAndNothingLeavesWithoutItsClaim(t *testing.T) {
	gh, store := &fakeRegistrar{runners: map[string]bool{}, fail: 1}, &fakeS3{objects: map[string]string{}, fail: 1, lose: 1}
	h := listenerProcess(gh, store, false)
	if code, _ := jitAsk(h, "i-0000"); code != http.StatusTooManyRequests || gh.made != 0 {
		t.Fatalf("S3 failed the create: %d with %d runners registered, want 429 and none", code, gh.made)
	}
	if code, _ := jitAsk(h, "i-0000"); code != http.StatusTooManyRequests || gh.made != 0 {
		t.Fatalf("the create's answer was lost: %d with %d runners registered, want 429 and none", code, gh.made)
	}
	if code, _ := jitAsk(h, "i-0000"); code != http.StatusServiceUnavailable || gh.made != 0 {
		t.Fatalf("GitHub failed: %d with %d runners registered, want 503 and none", code, gh.made)
	}
	if code, body := jitAsk(h, "i-0000"); code != http.StatusOK || body == "" {
		t.Fatalf("asked again: %d, want the config its own claim allows", code)
	}
	if code, _ := jitAsk(h, "i-0000"); code != http.StatusGone || gh.made != 1 || store.claims() != 1 {
		t.Fatalf("after its config: %d, %d runners, %d claims; want 410, 1, 1", code, gh.made, store.claims())
	}
}

// E1: a machine whose claim another listener holds ends only on GitHub's word about its runner, not on the refusal:
// the other listener's runner may be running its job.
func TestAMachineClaimedElsewhereIsCheckedWithGitHubNotEnded(t *testing.T) {
	gh, store := &fakeRegistrar{runners: map[string]bool{}}, &fakeS3{objects: map[string]string{"issued/i-0000": "another grant"}}
	h := listenerProcess(gh, store, false)
	if code, _ := jitAsk(h, "i-0000"); code != http.StatusGone || gh.made != 0 {
		t.Fatalf("claimed elsewhere: %d, %d runners registered; want 410 and none", code, gh.made)
	}
	st, now := h.sets["set"].state, time.Now()
	st.Listed(map[string]ListedRunner{}, now)
	if end := st.ToEnd(now); len(end) != 0 {
		t.Fatalf("ending %v on a refusal: want it kept until GitHub says its runner is gone", ids(end))
	}
	if _, unlisted := st.Stuck(now.Add(10*time.Minute), 5*time.Minute, 3*time.Minute); len(unlisted) != 1 {
		t.Fatalf("to check %v: want the machine checked with GitHub", ids(unlisted))
	}
}

// E1: a listing that drops a machine for a moment does not make this process forget it was issued a runner.
func TestAMachineAListingDroppedStaysIssued(t *testing.T) {
	s := newState(0, 10)
	s.Observe(stats(1, 0), t0)
	launch(s, machines(1, t0), t0)
	ask(s, "i-0000", t0)
	give(s, "i-0000", &Runner{ID: 5, Name: "set-i-0000", Issued: t0})
	s.Seen("us-east-1", nil, t0.Add(5*time.Minute), t0.Add(5*time.Minute))
	s.Seen("us-east-1", machines(1, t0), t0.Add(6*time.Minute), t0.Add(6*time.Minute))
	s.Observe(stats(1, 0), t0.Add(6*time.Minute))
	if v := ask(s, "i-0000", t0.Add(6*time.Minute)); v != End {
		t.Fatalf("re-adopted with no tag after a listing dropped it: %v, want End", v)
	}
}

// E6: GitHub's statistics are a lease that lapses with the message session: past it, nothing is launched, no idle
// runner is retired as surplus and no machine is told to end as surplus.
func TestALostSessionLaunchesRetiresAndEndsNothingOnOldStatistics(t *testing.T) {
	s := newState(0, 10)
	s.Observe(stats(3, 0), t0)
	if _, n := s.Deficit(t0, 100); n != 3 {
		t.Fatalf("with the session answering: launched %d, want 3", n)
	}
	s = newState(0, 10)
	s.Observe(stats(0, 0), t0)
	s.Seen("us-east-1", machines(3, t0.Add(-time.Hour)), t0, t0)
	give(s, "i-0001", &Runner{ID: 5, Name: "set-i-0001", Issued: t0.Add(-time.Hour)})
	s.Observe(stats(5, 0), t0.Add(time.Hour))
	lost := t0.Add(time.Hour + 2*time.Minute)
	if _, n := s.Deficit(lost, 100); n != 0 {
		t.Fatalf("session silent 2 min: launched %d on old statistics", n)
	}
	s.Observe(stats(0, 0), lost)
	later := lost.Add(2 * time.Minute)
	if n := s.Surplus(later, 0, 0, 100) + s.Surplus(later.Add(time.Second), 0, 0, 100); n != 0 {
		t.Fatalf("session silent 2 min: retired %d as surplus", n)
	}
	if v := ask(s, "i-0000", later); v == End {
		t.Fatal("session silent 2 min: a machine told to end as surplus")
	}
	s.Observe(stats(0, 0), later)
	if n := s.Surplus(later, 0, 0, 100) + s.Surplus(later.Add(time.Second), 0, 0, 100); n != 1 {
		t.Fatalf("session back: retired %d, want the idle runner", n)
	}
}

// E7: after a start, EC2's tags may not show yet that a machine was issued a runner. Shed ends only once the set is
// ready, and only a machine a runner listing of this process showed no runner of.
func TestShedWaitsForReadinessAndARunnerListing(t *testing.T) {
	s := newState(0, 10)
	s.Seen("us-east-1", machines(2, t0), t0, t0)
	s.Listed(map[string]ListedRunner{}, t0.Add(10*time.Minute))
	if got := s.Shed(t0.Add(10*time.Minute), 5); len(got) != 0 {
		t.Fatalf("shed %v before the queue was known", ids(got))
	}
	s = newState(0, 10)
	s.Seen("us-east-1", machines(2, t0), t0, t0)
	s.Observe(stats(2, 0), t0.Add(10*time.Minute))
	if got := s.Shed(t0.Add(10*time.Minute), 5); len(got) != 0 {
		t.Fatalf("shed %v before any runner listing", ids(got))
	}
	s.Listed(map[string]ListedRunner{"set-i-0001": {ID: 9, Name: "set-i-0001", Busy: true}}, t0.Add(10*time.Minute))
	if got := s.Shed(t0.Add(10*time.Minute), 5); len(got) != 1 || got[0].ID != "i-0000" {
		t.Fatalf("shed %v: want only the machine the listing showed no runner of", ids(got))
	}
}

// E8: one id EC2 refuses fails a whole CreateTags call. Splitting the batch tags every other id in a few calls; trying
// each id alone took one mutating call per id, enough for EC2 to throttle a burst into mass withholding.
func TestAFailedTagBatchIsSplitNotTriedIdByIdAlone(t *testing.T) {
	calls := 0
	tg := &Tagger{write: func(_ context.Context, _ *Region, ids []string, _ time.Time) error {
		calls++
		for _, id := range ids {
			if id == "i-bad" {
				return errors.New("InvalidInstanceID.NotFound")
			}
		}
		return nil
	}}
	part := make([]tagWaiter, 500)
	for i := range part {
		part[i] = tagWaiter{id: fmt.Sprintf("i-%04d", i), done: make(chan error, 1)}
	}
	part[137].id = "i-bad"
	tg.tag(context.Background(), &Region{Name: "us-east-1"}, part)
	for _, w := range part {
		if err := <-w.done; (err != nil) != (w.id == "i-bad") {
			t.Fatalf("%s: %v", w.id, err)
		}
	}
	if calls > 19 {
		t.Fatalf("%d calls to tag 500 machines with one bad id, want at most 19", calls)
	}
}

// E9: a set the config dropped is deleted only once it holds no machine, so no job it runs fails.
func TestADroppedSetIsDeletedOnlyOnceItHoldsNothing(t *testing.T) {
	s := NewState("set", 0, 0, []string{"us-east-1"}, t0.Add(-time.Hour))
	if s.Drained(t0) {
		t.Fatal("drained before its machines were listed")
	}
	s.Observe(stats(1, 0), t0)
	s.Seen("us-east-1", machines(1, t0.Add(-10*time.Minute)), t0, t0)
	s.Observe(started("set-i-0000", 9), t0)
	if s.Drained(t0) {
		t.Fatal("drained while its runner holds a job")
	}
	s.Observe(completed("set-i-0000"), t0.Add(time.Minute))
	if end := s.ToEnd(t0.Add(time.Minute)); len(end) != 1 {
		t.Fatalf("ending %v: its job completed", ids(end))
	}
	s.Seen("us-east-1", nil, t0.Add(2*time.Minute), t0.Add(2*time.Minute))
	s.Observe(stats(0, 0), t0.Add(2*time.Minute))
	if s.Drained(t0.Add(2 * time.Minute)) {
		t.Fatal("deleted on the first look at an empty set: one listing that missed a busy machine would fail its job")
	}
	if !s.Drained(t0.Add(3 * time.Minute)) {
		t.Fatal("not drained a minute after its last machine is gone")
	}
	booting := NewState("set", 0, 0, []string{"us-east-1"}, t0.Add(-time.Hour))
	booting.Seen("us-east-1", machines(1, t0), t0, t0)
	booting.Observe(stats(0, 0), t0)
	for _, at := range []time.Time{t0, t0.Add(2 * time.Minute)} {
		if booting.Drained(at) {
			t.Fatal("drained with a machine running that GitHub counts nothing of yet (it has not registered)")
		}
	}
}

func TestADroppedSetWithAJobAssignedButNotStartedIsNotDeleted(t *testing.T) {
	s := NewState("set", 0, 0, []string{"us-east-1"}, t0.Add(-time.Hour))
	s.Seen("us-east-1", nil, t0, t0)
	s.Observe(stats(1, 0), t0)
	for _, at := range []time.Time{t0, t0.Add(time.Minute), t0.Add(5 * time.Minute)} {
		if s.Drained(at) {
			t.Fatalf("deleted at %s with a job assigned to it and no runner yet: that job would fail", at.Sub(t0))
		}
	}
	s.Observe(stats(0, 0), t0.Add(6*time.Minute))
	if s.Drained(t0.Add(6*time.Minute)) || !s.Drained(t0.Add(7*time.Minute)) {
		t.Fatal("once GitHub counts nothing for a minute, the set goes")
	}
}
