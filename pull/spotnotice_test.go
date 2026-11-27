package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
)

// A spot reclaim of a dispatch machine (Nov 27 19:07:52Z: a job handed to i-0378d35b7af5f5286, which AWS terminated at
// 19:08:14Z, was failed only at 19:11:20Z, when the window ran out). The machine's own notice, or EC2 listing it leaving
// for a spot interruption, loses its handed job at once, with the reclaim as proof for its re-run.

// spotSet is dispatchSet with spot machines, the only ones AWS reclaims.
func spotSet(ids ...string) *DispatchSet {
	st := dispatchSet(ids...)
	for _, m := range st.machines {
		m.Lifecycle = "spot"
	}
	return st
}

// noticeServer is the public server as main builds it, before any listing showed it a scale set's machine. describes
// counts the listings of EC2 a notice cost.
func noticeServer(p *dispatcher, v interface {
	Verify(body []byte) (InstanceDocument, error)
}) (h *Server, describes *int) {
	describes = new(int)
	return &Server{sets: map[string]*Set{}, verifier: v, refresh: func(context.Context, string) { *describes++ }, dispatch: p.d,
		dispatchMachine: func(id string) (string, *Machine) {
			if m := p.st.machine(id); m != nil {
				return p.st.spec.Name, m.Machine
			}
			return "", nil
		}}, describes
}

// postNotice posts a machine's notice as boot.sh does: its identity document, and what IMDS's spot/instance-action said.
func postNotice(h http.Handler, instance, action string) int {
	req := httptest.NewRequest(http.MethodPost, "/notice", strings.NewReader(instance))
	if action != "" {
		req.Header.Set("X-Superci-Action", action)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

// instanceAction is IMDS's spot/instance-action for a reclaim at at.
func instanceAction(at time.Time) string {
	return fmt.Sprintf(`{"action":"terminate","time":%q}`, at.UTC().Format(time.RFC3339))
}

type forged struct{}

func (forged) Verify([]byte) (InstanceDocument, error) {
	return InstanceDocument{}, errors.New("bad signature")
}

// noPipeAfter reads a machine's connection until the dispatcher ends it, and fails on any PIPE frame: none of the
// job's bytes reaches a machine AWS is taking back.
func noPipeAfter(t *testing.T, m *peer, what string) {
	t.Helper()
	for {
		typ, _, err := m.next(3 * time.Second)
		switch {
		case err != nil && isTimeout(err):
			t.Fatalf("%s: the connection was kept", what)
		case err != nil:
			return
		case typ == framePipe:
			t.Fatalf("%s: the machine got the job's bytes", what)
		}
	}
}

func TestAHandedJobWhoseMachinePostsItsSpotNoticeIsLostAtOnceAndReRunOnce(t *testing.T) {
	store := &fakeS3{objects: map[string]string{}}
	p := startDispatcher(t, store, identityTags(), spotSet("i-0m1", "i-0m2"))
	m1, s, _ := handOne(t, p, "i-0m1", jobA)
	m2 := p.machine(t, "i-0m2")
	waitUntil(t, "the second machine waits", func() bool { return phaseOf(p.st, "i-0m2") == mWaiting })
	h, describes := noticeServer(p, trusting{})
	noticed := time.Now()
	if code := postNotice(h, "i-0m1", instanceAction(noticed.Add(2*time.Minute))); code != http.StatusNoContent {
		t.Fatalf("notice: %d, want 204: a dispatch machine's notice is taken", code)
	}
	s.send(framePipe, []byte("written after the notice"))
	if code := exitCode(s.expect(frameExit)); code != 1 {
		t.Fatalf("the shim's exit %d, want 1 (no TaskResult: its listener fails the job)", code)
	}
	if d := time.Since(noticed); d > time.Second {
		t.Fatalf("the shim's 1 came %s after the notice, want within a second", d)
	}
	noPipeAfter(t, m1, "the noticed machine")
	if err := m2.quiet(300 * time.Millisecond); err != nil {
		t.Fatalf("the job went to another machine: %v", err)
	}
	if *describes != 0 {
		t.Fatalf("the notice of a known dispatch machine listed EC2 %d times, want none", *describes)
	}
	if code := postNotice(h, "i-0m1", instanceAction(noticed.Add(2*time.Minute))); code != http.StatusNoContent {
		t.Fatalf("the notice again: %d, want 204", code)
	}
	end, lost := p.st.Reconcile(time.Now(), bounds)
	if len(end) != 1 || end[0].ID != "i-0m1" {
		t.Fatalf("terminate %v, want the noticed machine, so the job's steps stop", end)
	}
	want := JobRef{Owner: "acme", Repo: "mono", RunID: 7, Name: "unit", Runner: "superci-id-4cpu-17", Instance: "i-0m1"}
	if len(lost) != 1 || named(lost[0]) != want {
		t.Fatalf("lost %+v, want %+v once", lost, want)
	}
	if _, later := p.st.Reconcile(time.Now().Add(4*time.Minute), bounds); len(later) != 0 {
		t.Fatalf("lost again at the window: %+v", later)
	}
	// Before AWS takes the machine EC2 proves no reclaim, and the run may complete first: the job carries its proof.
	if got := reRuns(t, append(lost, lost...)); len(got) != 1 {
		t.Fatalf("re-ran %v, want the job once", got)
	}
	if c := p.st.TakeCounts(); c.Lost != 1 || c.Handoffs != 1 {
		t.Fatalf("counts %+v, want one hand-off and one lost job", c)
	}
}

// A job a restarted dispatcher adopted is its machine's before the machine rejoins it: the machine's notice loses it
// at once, and the machine, which cannot rejoin after, ends.
func TestAnAdoptedJobWhoseMachinePostsItsSpotNoticeBeforeRejoiningIsLostAtOnce(t *testing.T) {
	store := &fakeS3{objects: map[string]string{}}
	p := startDispatcher(t, store, identityTags(), spotSet("i-0m1"))
	_, _, token := handOne(t, p, "i-0m1", jobA)
	p.d.Close()
	p.srv.Close()
	p = startDispatcher(t, store, identityTags(), spotSet("i-0m1"))
	s := p.shim(t, resumeHeader, "i-0m1."+token)
	if s.code != 101 {
		t.Fatalf("resume: %d", s.code)
	}
	h, _ := noticeServer(p, trusting{})
	if code := postNotice(h, "i-0m1", instanceAction(time.Now().Add(2*time.Minute))); code != http.StatusNoContent {
		t.Fatalf("notice: %d, want 204", code)
	}
	if code := exitCode(s.expect(frameExit)); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if m := p.machine(t, "i-0m1", resumeHeader, "i-0m1."+token); m.code != http.StatusGone {
		t.Fatalf("the noticed machine's resume: %d, want 410", m.code)
	}
	if _, lost := p.st.Reconcile(time.Now(), bounds); len(lost) != 1 || len(reRuns(t, lost)) != 1 {
		t.Fatalf("lost %+v: want the job, re-run once", lost)
	}
}

// fakeEC2 answers DescribeInstances with the instances whose state the request's instance-state-name filter asks for.
type fakeEC2 struct {
	instances [][3]string // id, state, state reason
}

func (f *fakeEC2) RoundTrip(req *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(req.Body)
	form, _ := url.ParseQuery(string(body))
	var states []string
	for k, v := range form {
		if strings.HasSuffix(k, ".Name") && v[0] == "instance-state-name" {
			for kk, vv := range form {
				if strings.HasPrefix(kk, strings.TrimSuffix(k, "Name")+"Value.") {
					states = append(states, vv[0])
				}
			}
		}
	}
	var b strings.Builder
	b.WriteString(`<DescribeInstancesResponse xmlns="http://ec2.amazonaws.com/doc/2016-11-15/"><requestId>r</requestId><reservationSet><item><reservationId>r-1</reservationId><instancesSet>`)
	for _, in := range f.instances {
		if !slices.Contains(states, in[1]) {
			continue
		}
		fmt.Fprintf(&b, `<item><instanceId>%s</instanceId><instanceState><code>0</code><name>%s</name></instanceState>`, in[0], in[1])
		if in[2] != "" {
			fmt.Fprintf(&b, `<stateReason><code>%s</code><message>%s</message></stateReason>`, in[2], in[2])
		}
		fmt.Fprintf(&b, `<instanceLifecycle>spot</instanceLifecycle><launchTime>%s</launchTime><tagSet>`, time.Now().Add(-10*time.Minute).UTC().Format(time.RFC3339))
		b.WriteString(`<item><key>superci-pull-set</key><value>superci-4cpu</value></item><item><key>superci-pull-mode</key><value>dispatch</value></item></tagSet></item>`)
	}
	b.WriteString(`</instancesSet></item></reservationSet></DescribeInstancesResponse>`)
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/xml"}}, Body: io.NopCloser(strings.NewReader(b.String())), Request: req}, nil
}

// A machine whose notice did not reach the listener: the listing of EC2 the reconciler makes anyway shows it shutting
// down. One that leaves for a spot interruption loses its job at once; one that leaves for its own reason is left to
// the window.
func TestAHandedJobWhoseMachineEC2ListsShuttingDownForASpotInterruptionIsLostAtOnce(t *testing.T) {
	store := &fakeS3{objects: map[string]string{}}
	p := startDispatcher(t, store, identityTags(), spotSet("i-0m1", "i-0m2", "i-0m3"))
	_, s1, _ := handOne(t, p, "i-0m1", jobA)
	_, s2, _ := handOne(t, p, "i-0m2", jobB)
	cloud := &fakeEC2{instances: [][3]string{{"i-0m1", "shutting-down", "Server.SpotInstanceTermination"},
		{"i-0m2", "shutting-down", "Client.InstanceInitiatedShutdown"}, {"i-0m3", "running", ""}}}
	client := ec2.New(ec2.Options{Region: "us-east-1", Credentials: aws.AnonymousCredentials{}, HTTPClient: &http.Client{Transport: cloud}, RetryMaxAttempts: 1})
	l := &Listener{fleet: &Fleet{regions: []*Region{{Name: "us-east-1", ec2: client}}, deployment: "superci-pull"}, dsets: []*DispatchSet{p.st},
		strays: map[string]*Machine{}}
	listed := time.Now()
	l.refresher()(context.Background(), "us-east-1")
	if code := exitCode(s1.expect(frameExit)); code != 1 {
		t.Fatalf("the reclaimed machine's shim exit %d, want 1", code)
	}
	if d := time.Since(listed); d > time.Second {
		t.Fatalf("the shim's 1 came %s after the listing, want within a second", d)
	}
	if err := s2.quiet(300 * time.Millisecond); err != nil {
		t.Fatalf("the job of a machine leaving for its own reason: %v", err)
	}
	_, lost := p.st.Reconcile(time.Now(), bounds)
	if len(lost) != 1 || lost[0].Instance != "i-0m1" {
		t.Fatalf("lost %+v, want the job of the reclaimed machine alone", lost)
	}
	if got := reRuns(t, lost); len(got) != 1 {
		t.Fatalf("re-ran %v, want the job once", got)
	}
}

// Before the handed point a reclaimed machine is spent and the job is paired again; once PAIRED has gone, the job is
// lost at its handed point, and in both cases no machine AWS is taking back gets its bytes.
func TestASpotNoticeBeforeTheHandedPointPairsTheJobAgainOrLosesItAtTheHandedPoint(t *testing.T) {
	t.Run("waiting", func(t *testing.T) {
		store := &fakeS3{objects: map[string]string{}}
		p := startDispatcher(t, store, identityTags(), spotSet("i-0m1", "i-0m2"))
		h, _ := noticeServer(p, trusting{})
		m1 := p.machine(t, "i-0m1")
		waitUntil(t, "the first machine waits", func() bool { return phaseOf(p.st, "i-0m1") == mWaiting })
		if code := postNotice(h, "i-0m1", instanceAction(time.Now().Add(2*time.Minute))); code != http.StatusNoContent {
			t.Fatalf("notice: %d, want 204", code)
		}
		if phaseOf(p.st, "i-0m1") != mEnding {
			t.Fatal("a waiting machine AWS is taking back still waits for a job")
		}
		noPipeAfter(t, m1, "the noticed machine")
		m2 := p.machine(t, "i-0m2")
		waitUntil(t, "the second machine waits", func() bool { return phaseOf(p.st, "i-0m2") == mWaiting })
		s := p.shim(t)
		msg := jobMessage(jobA, 7, "unit")
		s.sendJob(msg)
		m2.job()
		if inst, _ := s.paired(); inst != "i-0m2" {
			t.Fatalf("PAIRED with %s, want i-0m2", inst)
		}
		m2.pipeBytes(len(msg))
	})
	for _, at := range []string{"the machine's claim", "the job's claim"} {
		t.Run("at "+at, func(t *testing.T) {
			store := &fakeS3{objects: map[string]string{}}
			hooked := &hookedClaims{Claims: &Claims{store: store, bucket: "claims"}}
			p := startDispatcher(t, store, identityTags(), spotSet("i-0m1", "i-0m2"))
			p.d.claims = hooked
			h, _ := noticeServer(p, trusting{})
			hookOn := "i-0m1"
			if at == "the job's claim" {
				hookOn = jobFence(jobA)
			}
			var once sync.Once
			noticed := make(chan int, 1)
			hooked.before = func(name string) {
				if name == hookOn {
					once.Do(func() { noticed <- postNotice(h, "i-0m1", instanceAction(time.Now().Add(2*time.Minute))) })
				}
			}
			m1 := p.machine(t, "i-0m1")
			waitUntil(t, "the first machine waits", func() bool { return phaseOf(p.st, "i-0m1") == mWaiting })
			m2 := p.machine(t, "i-0m2")
			waitUntil(t, "the second machine waits", func() bool { return phaseOf(p.st, "i-0m2") == mWaiting })
			s := p.shim(t)
			msg := jobMessage(jobA, 7, "unit")
			s.sendJob(msg)
			if code := <-noticed; code != http.StatusNoContent {
				t.Fatalf("notice: %d, want 204", code)
			}
			if at == "the machine's claim" {
				if inst, _ := s.paired(); inst != "i-0m2" {
					t.Fatalf("PAIRED with %s, want i-0m2: before PAIRED the job pairs again", inst)
				}
				m2.job()
				m2.pipeBytes(len(msg))
				noPipeAfter(t, m1, "the noticed machine")
				return
			}
			if inst, _ := s.paired(); inst != "i-0m1" {
				t.Fatalf("PAIRED with %s, want i-0m1", inst)
			}
			if code := exitCode(s.expect(frameExit)); code != 1 {
				t.Fatalf("the shim's exit %d, want 1: PAIRED had gone, so the job is lost at its handed point", code)
			}
			noPipeAfter(t, m1, "the noticed machine")
			if err := m2.quiet(300 * time.Millisecond); err != nil {
				t.Fatalf("the job went to another machine: %v", err)
			}
			_, lost := p.st.Reconcile(time.Now(), bounds)
			if got := reRuns(t, lost); len(lost) != 1 || len(got) != 1 {
				t.Fatalf("lost %+v, re-ran %v: want the job lost and re-run once", lost, got)
			}
		})
	}
}

// Only a current notice from the job's own machine, with a document EC2 signed, loses the job.
func TestANoticeFromAnotherInstanceOrStaleOrUnsignedChangesNothing(t *testing.T) {
	store := &fakeS3{objects: map[string]string{}}
	p := startDispatcher(t, store, identityTags(), spotSet("i-0m1", "i-0m2"))
	_, s, _ := handOne(t, p, "i-0m1", jobA)
	h, _ := noticeServer(p, trusting{})
	unsigned, _ := noticeServer(p, forged{})
	now := time.Now()
	for _, c := range []struct {
		what     string
		h        http.Handler
		instance string
		action   string
		want     int
	}{
		{"another dispatch machine's", h, "i-0m2", instanceAction(now.Add(2 * time.Minute)), http.StatusNoContent},
		{"an instance of no set's", h, "i-0other", instanceAction(now.Add(2 * time.Minute)), http.StatusForbidden},
		{"an unsigned document's", unsigned, "i-0m1", instanceAction(now.Add(2 * time.Minute)), http.StatusForbidden},
		{"a replayed stale", h, "i-0m1", instanceAction(now.Add(-10 * time.Minute)), http.StatusBadRequest},
		{"one for an hour ahead", h, "i-0m1", instanceAction(now.Add(time.Hour)), http.StatusBadRequest},
		{"one with no instance-action", h, "i-0m1", "", http.StatusBadRequest},
		{"one of another action", h, "i-0m1", fmt.Sprintf(`{"action":"reboot","time":%q}`, now.Add(2*time.Minute).UTC().Format(time.RFC3339)), http.StatusBadRequest},
	} {
		if code := postNotice(c.h, c.instance, c.action); code != c.want {
			t.Errorf("%s notice: %d, want %d", c.what, code, c.want)
		}
		if err := s.quiet(100 * time.Millisecond); err != nil {
			t.Fatalf("after %s notice the job's shim: %v", c.what, err)
		}
	}
	if _, lost := p.st.Reconcile(time.Now(), bounds); len(lost) != 0 {
		t.Fatalf("lost %+v, want none", lost)
	}
	if code := postNotice(h, "i-0m1", instanceAction(time.Now().Add(2*time.Minute))); code != http.StatusNoContent {
		t.Fatalf("the machine's own current notice: %d, want 204", code)
	}
	if code := exitCode(s.expect(frameExit)); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
}

// With no notice and no listing of a reclaim, a machine away loses its job at the window, as before, and the job is
// re-run only on EC2's proof.
func TestAMachineAwayWithNoNoticeIsLostAtTheWindowAndReRunOnlyOnProof(t *testing.T) {
	store := &fakeS3{objects: map[string]string{}}
	p := startDispatcher(t, store, identityTags(), spotSet("i-0m1"))
	m1, s, _ := handOne(t, p, "i-0m1", jobA)
	m1.conn.Close()
	waitUntil(t, "the machine is away", func() bool { st := p.st; st.mu.Lock(); defer st.mu.Unlock(); return st.machines["i-0m1"].wire == nil })
	if _, lost := p.st.Reconcile(time.Now().Add(time.Minute), bounds); len(lost) != 0 {
		t.Fatalf("lost within the window: %+v", lost)
	}
	if err := s.quiet(200 * time.Millisecond); err != nil {
		t.Fatalf("within the window: %v", err)
	}
	_, lost := p.st.Reconcile(time.Now().Add(4*time.Minute), bounds)
	if code := exitCode(s.expect(frameExit)); code != 1 || len(lost) != 1 {
		t.Fatalf("exit %d, lost %+v: want 1 and the job", code, lost)
	}
	if got := reRuns(t, lost); len(got) != 0 {
		t.Fatalf("re-ran %v with no proof of a reclaim", got)
	}
}

// boot.sh's watch of IMDS posts AWS's notice with what IMDS said, and posts it again until the listener answers.
func TestBootPostsTheSpotNoticeWithItsInstanceActionUntilTheListenerAnswers(t *testing.T) {
	got := renderBoot(t, bootScript, "dispatch")
	i := strings.Index(got, "( while sleep 5; do")
	j := strings.Index(got[max(i, 0):], "done ) &")
	if i < 0 || j < 0 {
		t.Fatal("no notice watch in boot.sh")
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	action := `{"action":"terminate","time":"2026-11-27T19:08:14Z"}`
	script := `log=` + log + `; n=0
T=imds; DOC=doc; L=https://203.0.113.1:8443; PIN=sha256//pin
printf '%s\n' 503 000 204 410 > ` + dir + `/statuses
sleep() { n=$((n + 1)); [ $n -le 8 ]; }
curl() { case " $* " in *" -w "*) echo 200 ;; *) echo '` + action + `' ;; esac; }
call() { echo "call $*" >> $log; head -1 ` + dir + `/statuses; sed -i 1d ` + dir + `/statuses; }
` + got[i:i+j] + `done )
`
	if out, err := exec.Command("bash", "-c", script).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	raw, _ := os.ReadFile(log)
	posts := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(posts) != 3 {
		t.Fatalf("posts %q: want three, the third answered 204", posts)
	}
	for _, p := range posts {
		if p != "call notice /dev/null -H X-Superci-Action: "+action {
			t.Fatalf("post %q: want the document to /notice with IMDS's instance-action", p)
		}
	}
}

// stuckConn is the connection of a machine that does not read: each of its first block writes waits until the
// connection closes (and fails) or for stuck (and goes out), as a write waits on its deadline. It keeps what went out.
type stuckConn struct {
	net.Conn
	stuck   time.Duration
	entered chan struct{} // a write is waiting
	closed  chan struct{}
	once    sync.Once
	mu      sync.Mutex
	block   int
	wrote   []string // the payloads of the PIPE frames that went out
}

func newStuckConn(block int, stuck time.Duration) *stuckConn {
	return &stuckConn{stuck: stuck, block: block, entered: make(chan struct{}, 8), closed: make(chan struct{})}
}

func (c *stuckConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	wait := c.block > 0
	if wait {
		c.block--
	}
	c.mu.Unlock()
	if wait {
		c.entered <- struct{}{}
		select {
		case <-c.closed:
			return 0, net.ErrClosed
		case <-time.After(c.stuck):
		}
	}
	if len(p) > 5 && frameType(p[0]) == framePipe {
		c.mu.Lock()
		c.wrote = append(c.wrote, string(p[5:]))
		c.mu.Unlock()
	}
	return len(p), nil
}

func (c *stuckConn) pipes() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.wrote)
}

func (c *stuckConn) Close() error                     { c.once.Do(func() { close(c.closed) }); return nil }
func (c *stuckConn) SetWriteDeadline(time.Time) error { return nil }
func (c *stuckConn) SetReadDeadline(time.Time) error  { return nil }

// handedOver is job-1 of spot machine i-0m1 at its handed point, over conn, with held its bytes held for the machine,
// as hand leaves it just before attachMachine.
func handedOver(conn net.Conn, held ...string) (*DispatchSet, *wire, *session) {
	st := spotSet("i-0m1")
	m := st.machine("i-0m1")
	mw := newWire(conn, nil, time.Hour, time.Hour, nil)
	st.mu.Lock()
	m.wire, m.phase = mw, mWaiting
	st.mu.Unlock()
	s := &session{set: st, id: "job-1", agent: "superci-id-4cpu-17", head: jobHead{ID: "job-1", Run: 7, Name: "unit", Repo: "o/r"},
		arrived: time.Now(), settled: make(chan struct{})}
	for _, h := range held {
		s.toMach, s.held = append(s.toMach, []byte(h)), s.held+len(h)
	}
	st.byJob["job-1"] = s
	st.claimed(s, m, mw, "tok.job-1")
	st.handed(s, time.Now())
	return st, mw, s
}

// heldExit says whether exit code 1 is held for the job's shim.
func heldExit(s *session) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.toShim) > 0 && s.toShim[len(s.toShim)-1].t == frameExit && exitCode(s.toShim[len(s.toShim)-1].p) == 1
}

// A write to a machine that does not read holds the job's writer for up to the silence. A reclaim, whether a notice
// (its HTTP answer) or the listing of EC2 (the refresher, which holds the region's lock), waits on no such write:
// the job is lost at once, and no byte the shim sends after reaches the machine.
func TestAReclaimDoesNotWaitOnAMachineWriteThatBlocks(t *testing.T) {
	for _, path := range []string{"notice", "EC2"} {
		t.Run(path, func(t *testing.T) {
			conn := newStuckConn(1, 2*time.Second)
			st, mw, s := handedOver(conn)
			defer mw.close()
			s.attachMachine(mw)
			s.goLive()
			before := make(chan struct{})
			go func() { _ = s.pipe([]byte("before")); close(before) }()
			<-conn.entered
			start := time.Now()
			if path == "notice" {
				d := NewDispatcher("superci-pull", []*DispatchSet{st}, trusting{}, &Claims{store: &fakeS3{objects: map[string]string{}}, bucket: "claims"},
					identityTags(), func(context.Context, string) {})
				defer d.Close()
				if known, err := d.Notice(InstanceDocument{InstanceID: "i-0m1", Region: "us-east-1"}, instanceAction(start.Add(2*time.Minute)), start); !known || err != nil {
					t.Fatalf("notice: known %v, err %v", known, err)
				}
			} else {
				st.Leaving([]*Machine{{ID: "i-0m1", State: "shutting-down", Reclaimed: true}})
			}
			if d := time.Since(start); d > 200*time.Millisecond {
				t.Fatalf("the reclaim waited %s on the machine's write, want none", d)
			}
			if _, lost := st.Reconcile(time.Now(), bounds); len(lost) != 1 {
				t.Fatalf("lost %+v, want the job at once", lost)
			}
			for deadline := start.Add(time.Second); !heldExit(s); time.Sleep(5 * time.Millisecond) {
				if time.Now().After(deadline) {
					t.Fatal("no exit code 1 for the shim within a second of the reclaim")
				}
			}
			after := make(chan struct{})
			go func() { _ = s.pipe([]byte("after")); close(after) }()
			<-before
			<-after
			if got := conn.pipes(); slices.Contains(got, "after") {
				t.Fatalf("the machine got %q: a byte sent after the loss", got)
			}
		})
	}
}

// A job lost while its held bytes go out to its machine sends no more of them: the frame being written may finish,
// and no other starts, even on a connection the machine no longer holds, which no end of the machine closes.
func TestAJobLostWhileItsHeldBytesGoOutSendsNoMoreOfThem(t *testing.T) {
	conn := newStuckConn(1, time.Second)
	st, mw, s := handedOver(conn, "held 1", "held 2")
	defer mw.close()
	st.mu.Lock()
	st.machines["i-0m1"].wire = nil
	st.mu.Unlock()
	flushed := make(chan struct{})
	go func() { s.attachMachine(mw); close(flushed) }()
	<-conn.entered
	start := time.Now()
	st.Leaving([]*Machine{{ID: "i-0m1", State: "shutting-down", Reclaimed: true}})
	if d := time.Since(start); d > 200*time.Millisecond {
		t.Fatalf("the reclaim waited %s on the machine's write, want none", d)
	}
	<-flushed
	s.attachMachine(mw) // a late attach, as hand's after a reclaim at the handed point
	if got := conn.pipes(); !slices.Equal(got, []string{"held 1"}) {
		t.Fatalf("the machine got %q, want only the frame being written at the loss", got)
	}
}

// A job whose exit code came (held for a shim that is away) is over on its machine: a reclaim after keeps that code for
// the shim and loses nothing.
func TestAReclaimAfterTheExitCodeCameKeepsIt(t *testing.T) {
	store := &fakeS3{objects: map[string]string{}}
	p := startDispatcher(t, store, identityTags(), spotSet("i-0m1"))
	m, s, token := handOne(t, p, "i-0m1", jobA)
	s.conn.Close()
	waitUntil(t, "the shim is away", func() bool { s := sessionOn(p.st, "i-0m1"); s.mu.Lock(); defer s.mu.Unlock(); return s.shim == nil })
	m.send(frameExit, i32(100))
	waitUntil(t, "the machine ends", func() bool { return phaseOf(p.st, "i-0m1") == mEnding })
	h, _ := noticeServer(p, trusting{})
	if code := postNotice(h, "i-0m1", instanceAction(time.Now().Add(2*time.Minute))); code != http.StatusNoContent {
		t.Fatalf("notice: %d, want 204", code)
	}
	back := p.shim(t, resumeHeader, "i-0m1."+token)
	if code := exitCode(back.expect(frameExit)); code != 100 {
		t.Fatalf("the shim's exit %d, want the Worker's 100", code)
	}
	if _, lost := p.st.Reconcile(time.Now(), bounds); len(lost) != 0 {
		t.Fatalf("lost %+v, want none: the job was over", lost)
	}
}

// AWS reclaims spot machines only: an on-demand machine's notice is refused and changes nothing.
func TestAnOnDemandMachinesNoticeIsRefusedAndChangesNothing(t *testing.T) {
	store := &fakeS3{objects: map[string]string{}}
	st := dispatchSet("i-0m1", "i-0m2")
	for _, m := range st.machines {
		m.Lifecycle = "on-demand"
	}
	p := startDispatcher(t, store, identityTags(), st)
	_, s, _ := handOne(t, p, "i-0m1", jobA)
	p.machine(t, "i-0m2")
	waitUntil(t, "the second machine waits", func() bool { return phaseOf(p.st, "i-0m2") == mWaiting })
	h, _ := noticeServer(p, trusting{})
	for _, id := range []string{"i-0m1", "i-0m2"} {
		if code := postNotice(h, id, instanceAction(time.Now().Add(2*time.Minute))); code != http.StatusBadRequest {
			t.Fatalf("%s's notice: %d, want 400", id, code)
		}
	}
	if err := s.quiet(300 * time.Millisecond); err != nil {
		t.Fatalf("the job's shim: %v", err)
	}
	if phaseOf(p.st, "i-0m1") != mClaimed || phaseOf(p.st, "i-0m2") != mWaiting {
		t.Fatal("an on-demand machine's notice changed its machine")
	}
	if _, lost := p.st.Reconcile(time.Now(), bounds); len(lost) != 0 {
		t.Fatalf("lost %+v, want none", lost)
	}
}
