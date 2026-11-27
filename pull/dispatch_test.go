package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf16"
)

const (
	idHost   = "i-0identity1"
	idRunner = `{"agentId":17,"agentName":"superci-id-4cpu-17","poolId":1,"poolName":"superci-pull","serverUrl":"https://pipelines.actions.githubusercontent.com/x","gitHubUrl":"https://github.com/acme","workFolder":"_work"}`
)

// fakeTags is EC2's tags of each instance.
type fakeTags map[string]map[string]string

func (f fakeTags) Tags(_ context.Context, _, instance string) (map[string]string, error) {
	return f[instance], nil
}

func identityTags() fakeTags {
	return fakeTags{idHost: {tagRole: "identity", tagDeployment: "superci-pull"}}
}

// dispatchSet is the set superci-4cpu, every region listed, holding the given machines (launched ten minutes ago).
func dispatchSet(ids ...string) *DispatchSet {
	now := time.Now()
	st := NewDispatchSet(SetSpec{Name: "superci-4cpu", Label: "superci-4cpu", Max: 100, Dispatch: true}, nil, []string{"us-east-1"}, now.Add(-time.Hour))
	var listed []*Machine
	for _, id := range ids {
		listed = append(listed, &Machine{ID: id, Set: "superci-4cpu", Region: "us-east-1", Launched: now.Add(-10 * time.Minute), Dispatch: true})
	}
	st.Seen("us-east-1", listed, now, now)
	return st
}

// dispatcher is one listener process's dispatcher on a TLS server of its own, sharing the claims in store with any
// other process of the test.
type dispatcher struct {
	d   *Dispatcher
	st  *DispatchSet
	srv *httptest.Server
}

func startDispatcher(t *testing.T, store *fakeS3, tags fakeTags, st *DispatchSet) *dispatcher {
	return startDispatcherPinging(t, store, tags, st, 200*time.Millisecond, 5*time.Second)
}

func startDispatcherPinging(t *testing.T, store *fakeS3, tags fakeTags, st *DispatchSet, ping, silence time.Duration) *dispatcher {
	t.Helper()
	d := NewDispatcher("superci-pull", []*DispatchSet{st}, trusting{}, &Claims{store: store, bucket: "claims"}, tags, func(context.Context, string) {})
	d.ping, d.silence = ping, silence
	srv := httptest.NewUnstartedServer(d)
	srv.EnableHTTP2 = false
	srv.StartTLS()
	t.Cleanup(func() { d.Close(); srv.Close() })
	return &dispatcher{d: d, st: st, srv: srv}
}

// restart is the process ending: every connection breaks, and nothing it knew is kept but the claims.
func (p *dispatcher) restart(t *testing.T, store *fakeS3, tags fakeTags, ids ...string) *dispatcher {
	p.d.Close()
	p.srv.Close()
	return startDispatcher(t, store, tags, dispatchSet(ids...))
}

// A peer is a machine or a shim, speaking frames over its upgraded connection.
type peer struct {
	t    *testing.T
	conn *tls.Conn
	r    *bufio.Reader
	code int
}

func (p *dispatcher) dial(t *testing.T, path, instance string, headers ...string) *peer {
	t.Helper()
	conn, err := tls.Dial("tcp", p.srv.Listener.Addr().String(), &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	var req strings.Builder
	fmt.Fprintf(&req, "POST %s HTTP/1.1\r\nHost: listener\r\nConnection: Upgrade\r\nUpgrade: %s\r\nContent-Length: %d\r\n", path, dispatchProtocol, len(instance))
	for i := 0; i+1 < len(headers); i += 2 {
		fmt.Fprintf(&req, "%s: %s\r\n", headers[i], headers[i+1])
	}
	req.WriteString("\r\n" + instance)
	if _, err := conn.Write([]byte(req.String())); err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(conn)
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(r, nil)
	if err != nil {
		t.Fatalf("%s %s: %v", path, instance, err)
	}
	_ = conn.SetReadDeadline(time.Time{})
	if resp.StatusCode != http.StatusSwitchingProtocols {
		conn.Close()
	}
	return &peer{t: t, conn: conn, r: r, code: resp.StatusCode}
}

func (p *dispatcher) machine(t *testing.T, id string, headers ...string) *peer {
	return p.dial(t, "/work", id, headers...)
}

func (p *dispatcher) shim(t *testing.T, headers ...string) *peer {
	return p.dial(t, "/shim", idHost, append([]string{runnerHeader, base64.StdEncoding.EncodeToString([]byte(idRunner))}, headers...)...)
}

func (p *peer) send(t frameType, payload []byte) {
	p.t.Helper()
	if _, err := p.conn.Write(appendFrame(nil, t, payload)); err != nil {
		p.t.Fatalf("send %d: %v", t, err)
	}
}

// next is the next frame other than a PING, or an error when none comes within d or the connection ends.
func (p *peer) next(d time.Duration) (frameType, []byte, error) {
	_ = p.conn.SetReadDeadline(time.Now().Add(d))
	for {
		var h [5]byte
		if _, err := ioReadFull(p.r, h[:]); err != nil {
			return 0, nil, err
		}
		body := make([]byte, binary.LittleEndian.Uint32(h[1:]))
		if _, err := ioReadFull(p.r, body); err != nil {
			return 0, nil, err
		}
		if frameType(h[0]) != framePing {
			return frameType(h[0]), body, nil
		}
	}
}

// expect is the next frame, which must be of type want.
func (p *peer) expect(want frameType) []byte {
	p.t.Helper()
	got, body, err := p.next(5 * time.Second)
	if err != nil {
		p.t.Fatalf("waiting for a frame of type %d: %v", want, err)
	}
	if got != want {
		p.t.Fatalf("got a frame of type %d (%q), want %d", got, body, want)
	}
	return body
}

// quiet says the peer gets no frame other than a PING within d, nor loses its connection.
func (p *peer) quiet(d time.Duration) error {
	if t, body, err := p.next(d); err == nil {
		return fmt.Errorf("got a frame of type %d (%q)", t, body)
	} else if !isTimeout(err) {
		return fmt.Errorf("connection ended: %v", err)
	}
	return nil
}

// ended says the dispatcher ended the peer's connection within d.
func (p *peer) ended(d time.Duration) bool {
	for deadline := time.Now().Add(d); time.Now().Before(deadline); {
		if _, _, err := p.next(time.Until(deadline)); err != nil {
			return !isTimeout(err)
		}
	}
	return false
}

func isTimeout(err error) bool {
	ne, ok := err.(interface{ Timeout() bool })
	return ok && ne.Timeout()
}

// jobMessage is the listener's NewJobRequest as it writes it to the Worker's pipe, with a secret the dispatcher must
// not keep.
func jobMessage(id string, run int64, name string) []byte {
	js := fmt.Sprintf(`{"messageType":"PipelineAgentJobRequest","jobId":%q,"jobDisplayName":%q,"contextData":{"github":{"t":2,"d":[{"k":"run_id","v":"%d"},{"k":"job","v":"unit"},{"k":"repository","v":"acme/mono"},{"k":"workflow_ref","v":"acme/mono/.github/workflows/ci.yml@refs/heads/main"},{"k":"event_name","v":"push"}]}},"variables":{"system.accessToken":{"value":"job-secret","isSecret":true}}}`, id, name, run)
	units := utf16.Encode([]rune(js))
	b := binary.LittleEndian.AppendUint32(nil, 1)
	b = binary.LittleEndian.AppendUint32(b, uint32(2*len(units)))
	for _, u := range units {
		b = binary.LittleEndian.AppendUint16(b, u)
	}
	return b
}

// sendJob writes a job message as the shim does, in two PIPE frames.
func (p *peer) sendJob(msg []byte) {
	p.send(framePipe, msg[:7])
	p.send(framePipe, msg[7:])
}

// pipeBytes reads PIPE frames until n bytes have come.
func (p *peer) pipeBytes(n int) []byte {
	p.t.Helper()
	var got []byte
	for len(got) < n {
		got = append(got, p.expect(framePipe)...)
	}
	return got
}

type jobFrameOf struct {
	Runner string `json:"runner"`
	Token  string `json:"token"`
	Job    struct {
		Run  int64  `json:"run"`
		Job  string `json:"job"`
		Name string `json:"name"`
		Repo string `json:"repo"`
	} `json:"job"`
}

func (p *peer) job() jobFrameOf {
	p.t.Helper()
	var j jobFrameOf
	if err := json.Unmarshal(p.expect(frameJob), &j); err != nil {
		p.t.Fatal(err)
	}
	return j
}

func (p *peer) paired() (instance, token string) {
	p.t.Helper()
	var v struct{ Instance, Token string }
	if err := json.Unmarshal(p.expect(framePaired), &v); err != nil {
		p.t.Fatal(err)
	}
	return v.Instance, v.Token
}

func exitCode(b []byte) int32 { return int32(binary.LittleEndian.Uint32(b)) }

func i32(v int32) []byte { return binary.LittleEndian.AppendUint32(nil, uint32(v)) }

func waitUntil(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !ok(); {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting until %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func idleMachines(st *DispatchSet) int { st.mu.Lock(); defer st.mu.Unlock(); return len(st.idle) }

func phaseOf(st *DispatchSet, id string) machinePhase {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.machines[id].phase
}

func TestAJobRunsOnOneMachineAndItsExitCodeReachesTheShim(t *testing.T) {
	store := &fakeS3{objects: map[string]string{}}
	p := startDispatcher(t, store, identityTags(), dispatchSet("i-0m1"))
	m := p.machine(t, "i-0m1")
	waitUntil(t, "the machine waits", func() bool { return idleMachines(p.st) == 1 })
	s := p.shim(t)
	if m.code != 101 || s.code != 101 {
		t.Fatalf("machine %d, shim %d, want 101 both", m.code, s.code)
	}
	msg := jobMessage("5f0c3b52-9b8d-4c62-8e31-1a2b3c4d5e6f", 4242, "unit (1/4)")
	s.sendJob(msg)
	j := m.job()
	if j.Runner != idRunner || j.Token == "" || j.Job.Run != 4242 || j.Job.Job != "unit" || j.Job.Name != "unit (1/4)" || j.Job.Repo != "acme/mono" {
		t.Fatalf("JOB %+v: want the identity's .runner, a token and the job's run, key, name and repository", j)
	}
	if inst, token := s.paired(); inst != "i-0m1" || token != j.Token {
		t.Fatalf("PAIRED %s %s, want the machine and the token its JOB carries", inst, token)
	}
	if got := m.pipeBytes(len(msg)); !bytes.Equal(got, msg) {
		t.Fatal("the machine's PIPE bytes are not the listener's job message")
	}
	m.send(frameStdout, []byte("ran"))
	m.send(frameInfo, []byte(`{"t_spawn":1.5}`))
	m.send(frameExit, i32(100))
	if got := s.expect(frameStdout); string(got) != "ran" {
		t.Fatalf("stdout %q", got)
	}
	s.expect(frameInfo)
	if code := exitCode(s.expect(frameExit)); code != 100 {
		t.Fatalf("exit %d, want the Worker's 100", code)
	}
	if !m.ended(2 * time.Second) {
		t.Fatal("the machine's connection was kept after its EXIT reached the shim: the agent takes a close for delivery")
	}
	waitUntil(t, "the machine ends", func() bool { return phaseOf(p.st, "i-0m1") == mEnding })
	end, _ := p.st.Reconcile(time.Now(), dispatchBounds{maxAge: time.Hour, life: 9 * time.Hour, job: 6 * time.Hour, window: 3 * time.Minute})
	if len(end) != 1 || end[0].ID != "i-0m1" {
		t.Fatalf("terminate %v, want the machine whose job ended", end)
	}
	if c := p.st.TakeCounts(); c.Handoffs != 1 || c.Retries != 0 || c.Lost != 0 {
		t.Fatalf("counts %+v, want one hand-off", c)
	}
}

func ioReadFull(r *bufio.Reader, b []byte) (int, error) {
	n := 0
	for n < len(b) {
		k, err := r.Read(b[n:])
		n += k
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

const (
	jobA = "5f0c3b52-9b8d-4c62-8e31-1a2b3c4d5e6f"
	jobB = "77aa0000-1111-4222-8333-444455556666"
)

var bounds = dispatchBounds{maxAge: 8 * time.Hour, life: 15 * time.Hour, job: 390 * time.Minute, window: 3 * time.Minute}

// handOne pairs a waiting machine with a new job and reads the job's PIPE bytes on the machine: the job is past its
// handed point.
func handOne(t *testing.T, p *dispatcher, machine, job string) (m, s *peer, token string) {
	t.Helper()
	m = p.machine(t, machine)
	waitUntil(t, machine+" waits", func() bool { return phaseOf(p.st, machine) == mWaiting })
	s = p.shim(t)
	msg := jobMessage(job, 7, "unit")
	s.sendJob(msg)
	token = m.job().Token
	s.paired()
	m.pipeBytes(len(msg))
	return m, s, token
}

// The handed point, and what may happen around it, is the spec's "a job written to two machines": no interleaving of
// pairing, failure and restart may deliver PIPE bytes to two machines.
func TestNoInterleavingOfPairingFailureAndRestartWritesAJobToTwoMachines(t *testing.T) {
	t.Run("a shim that starts its job again after a restart is sent to the machine that has it", func(t *testing.T) {
		store := &fakeS3{objects: map[string]string{}}
		p := startDispatcher(t, store, identityTags(), dispatchSet("i-0m1", "i-0m2"))
		m1, _, token := handOne(t, p, "i-0m1", jobA)
		// The shim's PAIRED was lost with the process: it knows no pair, so it starts the job again.
		p = p.restart(t, store, identityTags(), "i-0m1", "i-0m2")
		m2 := p.machine(t, "i-0m2")
		waitUntil(t, "the second machine waits", func() bool { return phaseOf(p.st, "i-0m2") == mWaiting })
		again := p.shim(t)
		msg := jobMessage(jobA, 7, "unit")
		again.sendJob(msg)
		var last string
		for {
			typ, body, err := again.next(5 * time.Second)
			if err != nil {
				break
			}
			if typ == framePaired {
				var v struct{ Instance, Token string }
				_ = json.Unmarshal(body, &v)
				last = v.Instance + "." + v.Token
			}
		}
		if last != "i-0m1."+token {
			t.Fatalf("the shim was last paired with %q, want the machine that has the job (i-0m1.%s)", last, token)
		}
		for {
			typ, _, err := m2.next(500 * time.Millisecond)
			if err != nil {
				break
			}
			if typ == framePipe {
				t.Fatal("a second machine got the job's PIPE bytes")
			}
		}
		m1.conn.Close()
		m1 = p.machine(t, "i-0m1", resumeHeader, "i-0m1."+token)
		s := p.shim(t, resumeHeader, "i-0m1."+token)
		if m1.code != 101 || s.code != 101 {
			t.Fatalf("resume: machine %d, shim %d, want 101 both", m1.code, s.code)
		}
		m1.send(frameExit, i32(100))
		if code := exitCode(s.expect(frameExit)); code != 100 {
			t.Fatalf("exit %d", code)
		}
	})
	t.Run("a shim that starts its job again in the same process spends no machine", func(t *testing.T) {
		store := &fakeS3{objects: map[string]string{}}
		p := startDispatcher(t, store, identityTags(), dispatchSet("i-0m1", "i-0m2"))
		_, _, token := handOne(t, p, "i-0m1", jobA)
		m2 := p.machine(t, "i-0m2")
		waitUntil(t, "the second machine waits", func() bool { return phaseOf(p.st, "i-0m2") == mWaiting })
		again := p.shim(t)
		again.sendJob(jobMessage(jobA, 7, "unit"))
		if inst, tok := again.paired(); inst != "i-0m1" || tok != token {
			t.Fatalf("PAIRED %s.%s, want the pair that has the job", inst, tok)
		}
		if err := m2.quiet(300 * time.Millisecond); err != nil {
			t.Fatalf("the waiting machine was offered the job again: %v", err)
		}
		if phaseOf(p.st, "i-0m2") != mWaiting {
			t.Fatal("a machine was spent on a job already handed")
		}
	})
}

// Handed-point retry: a pairing that fails before the first PIPE byte spends its machine, and the job runs on the next.
func TestAMachineThatFailsBeforeTheFirstPipeByteIsSpentAndTheJobRunsOnTheNext(t *testing.T) {
	for _, c := range []struct {
		name   string
		break_ func(store *fakeS3, p *dispatcher, m *peer)
	}{
		{"its claim failed on every try", func(store *fakeS3, _ *dispatcher, _ *peer) {
			store.mu.Lock()
			store.fail = claimTries
			store.mu.Unlock()
		}},
		{"its connection ended while it was claimed", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			store := &fakeS3{objects: map[string]string{}}
			hooked := &hookedClaims{Claims: &Claims{store: store, bucket: "claims"}}
			p := startDispatcher(t, store, identityTags(), dispatchSet("i-0m1", "i-0m2"))
			p.d.claims = hooked
			m1 := p.machine(t, "i-0m1")
			waitUntil(t, "the first machine waits", func() bool { return phaseOf(p.st, "i-0m1") == mWaiting })
			m2 := p.machine(t, "i-0m2")
			waitUntil(t, "the second machine waits", func() bool { return phaseOf(p.st, "i-0m2") == mWaiting })
			if c.break_ != nil {
				c.break_(store, p, m1)
			} else {
				hooked.before = func(name string) {
					if name == "i-0m1" {
						m1.conn.Close()
						waitUntil(t, "the dispatcher sees the first machine gone", func() bool { st := p.st; st.mu.Lock(); defer st.mu.Unlock(); return st.machines["i-0m1"].wire == nil })
					}
				}
			}
			s := p.shim(t)
			msg := jobMessage(jobA, 7, "unit")
			s.sendJob(msg)
			if j := m2.job(); j.Token == "" {
				t.Fatal("no token")
			}
			if got := m2.pipeBytes(len(msg)); !bytes.Equal(got, msg) {
				t.Fatal("the next machine did not get the job")
			}
			for {
				typ, _, err := m1.next(300 * time.Millisecond)
				if err != nil {
					break
				}
				if typ == framePipe {
					t.Fatal("the failed machine got PIPE bytes")
				}
			}
			if ph := phaseOf(p.st, "i-0m1"); ph != mEnding {
				t.Fatalf("the failed machine is in phase %d, want it spent (ending)", ph)
			}
			if c := p.st.TakeCounts(); c.Retries != 1 || c.Handoffs != 1 {
				t.Fatalf("counts %+v, want one retry and one hand-off", c)
			}
		})
	}
}

// hookedClaims calls before ahead of each claim it takes, and after once it is taken.
type hookedClaims struct {
	*Claims
	before func(name string)
	after  func(name string)
}

func (h *hookedClaims) Take(ctx context.Context, name, token string) error {
	if h.before != nil {
		h.before(name)
	}
	err := h.Claims.Take(ctx, name, token)
	if h.after != nil {
		h.after(name)
	}
	return err
}

// Machine death after the handed point: the job is never sent anywhere else; once the window passes the shim exits 1
// and the machine ends.
func TestAMachineLostAfterTheHandedPointEndsItsShimWithOneAndTheJobGoesNowhereElse(t *testing.T) {
	store := &fakeS3{objects: map[string]string{}}
	p := startDispatcher(t, store, identityTags(), dispatchSet("i-0m1", "i-0m2"))
	p.st.SetPool(1)
	m1, s, _ := handOne(t, p, "i-0m1", jobA)
	m1.conn.Close()
	m2 := p.machine(t, "i-0m2")
	waitUntil(t, "the first machine is away", func() bool { st := p.st; st.mu.Lock(); defer st.mu.Unlock(); return st.machines["i-0m1"].wire == nil })
	p.st.Reconcile(time.Now().Add(time.Minute), bounds)
	if err := s.quiet(300 * time.Millisecond); err != nil {
		t.Fatalf("within the window: %v", err)
	}
	end, _ := p.st.Reconcile(time.Now().Add(4*time.Minute), bounds)
	if code := exitCode(s.expect(frameExit)); code != 1 {
		t.Fatalf("the shim's exit %d, want 1 (no TaskResult: its listener fails the job)", code)
	}
	if err := m2.quiet(300 * time.Millisecond); err != nil {
		t.Fatalf("the job went to another machine: %v", err)
	}
	if len(end) != 1 || end[0].ID != "i-0m1" {
		t.Fatalf("terminate %v, want the lost machine", end)
	}
	if c := p.st.TakeCounts(); c.Lost != 1 {
		t.Fatalf("counts %+v, want one lost job", c)
	}
}

// Resume: the dispatcher restarts mid-job, both sides rejoin, and the exit code reaches the shim.
func TestADispatcherRestartMidJobResumesBothSidesAndTheExitCodeReachesTheShim(t *testing.T) {
	store := &fakeS3{objects: map[string]string{}}
	p := startDispatcher(t, store, identityTags(), dispatchSet("i-0m1"))
	_, _, token := handOne(t, p, "i-0m1", jobA)
	p = p.restart(t, store, identityTags(), "i-0m1")
	m := p.machine(t, "i-0m1", resumeHeader, "i-0m1."+token)
	s := p.shim(t, resumeHeader, "i-0m1."+token)
	if m.code != 101 || s.code != 101 {
		t.Fatalf("resume: machine %d, shim %d, want 101 both", m.code, s.code)
	}
	s.send(framePipe, []byte("cancel"))
	if got := m.pipeBytes(6); string(got) != "cancel" {
		t.Fatalf("PIPE after the resume %q", got)
	}
	m.send(frameStdout, []byte("after"))
	m.send(frameExit, i32(102))
	if got := s.expect(frameStdout); string(got) != "after" {
		t.Fatalf("stdout %q", got)
	}
	if code := exitCode(s.expect(frameExit)); code != 102 {
		t.Fatalf("exit %d, want 102", code)
	}
	if c := p.st.TakeCounts(); c.Resumes != 2 {
		t.Fatalf("counts %+v, want both sides resumed", c)
	}
}

// The agent sends its EXIT again on a resumed connection when the last one did not close cleanly: after a restart the
// job's exit code may already have reached its shim, which never comes back, and the job ends as done.
func TestAnExitSentAgainAfterARestartIsHarmless(t *testing.T) {
	store := &fakeS3{objects: map[string]string{}}
	p := startDispatcher(t, store, identityTags(), dispatchSet("i-0m1"))
	m, s, token := handOne(t, p, "i-0m1", jobA)
	m.send(frameExit, i32(100))
	if code := exitCode(s.expect(frameExit)); code != 100 {
		t.Fatalf("exit %d", code)
	}
	p = p.restart(t, store, identityTags(), "i-0m1")
	m = p.machine(t, "i-0m1", resumeHeader, "i-0m1."+token)
	if m.code != 101 {
		t.Fatalf("resume: %d", m.code)
	}
	m.send(frameExit, i32(100))
	m.send(frameExit, i32(100))
	waitUntil(t, "the dispatcher holds the exit code", func() bool {
		st := p.st
		st.mu.Lock()
		j := st.pairs["i-0m1"]
		st.mu.Unlock()
		if j == nil {
			return false
		}
		j.mu.Lock()
		defer j.mu.Unlock()
		return j.exited
	})
	end, _ := p.st.Reconcile(time.Now().Add(4*time.Minute), bounds)
	if len(end) != 1 || end[0].ID != "i-0m1" {
		t.Fatalf("terminate %v, want the machine", end)
	}
	if c := p.st.TakeCounts(); c.Lost != 0 {
		t.Fatalf("counts %+v: a job whose exit code came is not lost", c)
	}
}

// A second /work from a claimed machine gets 410, in the process that claimed it and after a restart, and so does a
// resume whose token is not the machine's claim.
func TestASecondWorkFromAClaimedMachineAndAResumeWithAnotherTokenGet410(t *testing.T) {
	store := &fakeS3{objects: map[string]string{}}
	p := startDispatcher(t, store, identityTags(), dispatchSet("i-0m1"))
	_, _, token := handOne(t, p, "i-0m1", jobA)
	wrong := strings.Repeat("0", 32) + "." + jobA
	if code := p.machine(t, "i-0m1").code; code != http.StatusGone {
		t.Fatalf("a second /work: %d, want 410", code)
	}
	if code := p.machine(t, "i-0m1", resumeHeader, "i-0m1."+wrong).code; code != http.StatusGone {
		t.Fatalf("a resume with another token: %d, want 410", code)
	}
	p = p.restart(t, store, identityTags(), "i-0m1")
	if code := p.machine(t, "i-0m1").code; code != http.StatusGone {
		t.Fatalf("a second /work after a restart: %d, want 410", code)
	}
	if code := p.machine(t, "i-0m1", resumeHeader, "i-0m1."+wrong).code; code != http.StatusGone {
		t.Fatalf("a resume with another token after a restart: %d, want 410", code)
	}
	if code := p.machine(t, "i-0m1", resumeHeader, "i-0m1."+token).code; code != 101 {
		t.Fatalf("a resume with its token after a restart: %d, want 101", code)
	}
}

// /shim from an instance without the identity role tag, or of another deployment, gets 403.
func TestAShimFromAnInstanceThatIsNoIdentityHostIsRefused(t *testing.T) {
	store := &fakeS3{objects: map[string]string{}}
	tags := fakeTags{idHost: {tagRole: "identity", tagDeployment: "superci-pull"}, "i-0other": {tagDeployment: "superci-pull"},
		"i-0dev": {tagRole: "identity", tagDeployment: "superci-pull-dev"}}
	p := startDispatcher(t, store, tags, dispatchSet())
	runner := base64.StdEncoding.EncodeToString([]byte(idRunner))
	for _, c := range []struct {
		host    string
		headers []string
		want    int
	}{
		{"i-0other", nil, http.StatusForbidden},
		{"i-0dev", nil, http.StatusForbidden},
		{"i-0none", nil, http.StatusForbidden},
		{idHost, []string{labelHeader, "superci-8cpu"}, http.StatusBadRequest},
		{idHost, nil, 101},
	} {
		if got := p.dial(t, "/shim", c.host, append([]string{runnerHeader, runner}, c.headers...)...).code; got != c.want {
			t.Errorf("/shim from %s %v: %d, want %d", c.host, c.headers, got, c.want)
		}
	}
	for _, agent := range []string{"superci-4cpu-i-0abc", "superci-id-4cpu", "superci-id-8cpu-1"} {
		other := base64.StdEncoding.EncodeToString([]byte(`{"agentName":"` + agent + `"}`))
		if got := p.dial(t, "/shim", idHost, runnerHeader, other).code; got != http.StatusForbidden {
			t.Errorf("/shim for agent %s: %d, want 403 (no identity of a dispatch set)", agent, got)
		}
	}
}

// Frame bounds: an oversize INFO frame or an unknown type ends the machine's connection, and stdout past its budget
// is dropped and counted, so the shim gets no more than the bounds.
func TestFramesPastTheirBoundsEndTheConnectionOrAreDropped(t *testing.T) {
	t.Run("stdout past its budget", func(t *testing.T) {
		store := &fakeS3{objects: map[string]string{}}
		p := startDispatcher(t, store, identityTags(), dispatchSet("i-0m1"))
		m, s, _ := handOne(t, p, "i-0m1", jobA)
		chunk := bytes.Repeat([]byte("x"), 1<<20)
		for range 5 {
			m.send(frameStdout, chunk)
		}
		m.send(frameExit, i32(100))
		got := 0
		for {
			typ, body, err := s.next(5 * time.Second)
			if err != nil {
				t.Fatal(err)
			}
			if typ == frameExit {
				break
			}
			got += len(body)
		}
		if got != outBudget {
			t.Fatalf("the shim got %d bytes of stdout, want the budget of %d", got, outBudget)
		}
		if c := p.st.TakeCounts(); c.Dropped != 1 {
			t.Fatalf("counts %+v, want the one frame past the budget dropped", c)
		}
	})
	t.Run("info", func(t *testing.T) {
		store := &fakeS3{objects: map[string]string{}}
		p := startDispatcher(t, store, identityTags(), dispatchSet("i-0m1"))
		m, s, _ := handOne(t, p, "i-0m1", jobA)
		m.send(frameInfo, []byte(`{"t_hdr":1,"token":"x"}`))
		for range infoBudget + 1 {
			m.send(frameInfo, []byte(`{"t_hdr":1}`))
		}
		m.send(frameStdout, []byte("end"))
		for range infoBudget {
			if got := s.expect(frameInfo); string(got) != `{"t_hdr":1}` {
				t.Fatalf("the shim got INFO %s, want only frames of known keys", got)
			}
		}
		if body := s.expect(frameStdout); string(body) != "end" {
			t.Fatalf("after %d INFO frames %q, want no more of them", infoBudget, body)
		}
		if c := p.st.TakeCounts(); c.Dropped != 2 {
			t.Fatalf("counts %+v, want the unknown key and the frame past 64 dropped", c)
		}
		big := []byte(`{"t_hdr":"` + strings.Repeat("y", 4<<10) + `"}`)
		m.send(frameInfo, big)
		if !m.ended(2 * time.Second) {
			t.Fatal("an INFO frame over 4 KiB did not end the machine's connection")
		}
		if err := s.quiet(300 * time.Millisecond); err != nil {
			t.Fatalf("the oversize INFO frame reached the shim: %v", err)
		}
	})
	t.Run("a type the sender may not send", func(t *testing.T) {
		store := &fakeS3{objects: map[string]string{}}
		p := startDispatcher(t, store, identityTags(), dispatchSet("i-0m1"))
		m, s, _ := handOne(t, p, "i-0m1", jobA)
		m.send(9, nil)
		if !m.ended(2 * time.Second) {
			t.Fatal("an unknown type did not end the machine's connection")
		}
		s.send(frameStdout, []byte("not the shim's to send"))
		if !s.ended(2 * time.Second) {
			t.Fatal("STDOUT from the shim did not end its connection")
		}
		other := startDispatcher(t, store, identityTags(), dispatchSet())
		big := other.shim(t)
		_, _ = big.conn.Write(appendFrame(nil, framePipe, make([]byte, 1<<20+1)))
		if !big.ended(2 * time.Second) {
			t.Fatal("a PIPE frame over 1 MiB did not end the shim's connection")
		}
	})
}

// Credentials: what a machine gets is the identity's .runner, its token and the job's names; a .runner that holds a
// field of the identity's credentials is refused before any machine gets anything.
func TestAShimThatSendsCredentialsIsRefusedAndNoMachineGetsThem(t *testing.T) {
	store := &fakeS3{objects: map[string]string{}}
	p := startDispatcher(t, store, identityTags(), dispatchSet("i-0m1"))
	m := p.machine(t, "i-0m1")
	waitUntil(t, "the machine waits", func() bool { return idleMachines(p.st) == 1 })
	for _, bad := range []string{
		`{"scheme":"OAuth","data":{"clientId":"c0ffee","authorizationUrl":"https://token"}}`,
		`{"agentName":"superci-id-4cpu-17","modulus":"AQAB","d":"secret"}`,
		`{"agentName":"superci-id-4cpu-17","data":{"clientId":"c0ffee"}}`,
	} {
		if got := p.dial(t, "/shim", idHost, runnerHeader, base64.StdEncoding.EncodeToString([]byte(bad))).code; got != http.StatusForbidden {
			t.Errorf("a .runner %s: %d, want 403", bad, got)
		}
	}
	if err := m.quiet(300 * time.Millisecond); err != nil {
		t.Fatalf("the machine got something of a refused shim: %v", err)
	}
	s := p.shim(t)
	s.sendJob(jobMessage(jobA, 7, "unit"))
	raw := m.expect(frameJob)
	var keys map[string]json.RawMessage
	_ = json.Unmarshal(raw, &keys)
	if len(keys) != 3 || keys["runner"] == nil || keys["token"] == nil || keys["job"] == nil {
		t.Fatalf("JOB %s, want only runner, token and job", raw)
	}
	if strings.Contains(string(raw), "job-secret") {
		t.Fatal("the JOB frame holds the job message's secret")
	}
}

// N shims and M machines pairing at once: each job runs on exactly one machine and each machine takes at most one.
func TestShimsAndMachinesPairingAtOnceGiveEachJobOneMachineAndEachMachineOneJob(t *testing.T) {
	const n = 24
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("i-0m%02d", i)
	}
	store := &fakeS3{objects: map[string]string{}}
	p := startDispatcher(t, store, identityTags(), dispatchSet(ids...))
	var mu sync.Mutex
	jobsOf := map[string][]string{} // machine -> the jobs whose bytes it got
	ranOn := map[string]string{}    // job -> the machine its shim heard from
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(2)
		go func() {
			defer wg.Done()
			m := p.machine(t, ids[i])
			if m.code != 101 {
				t.Errorf("machine %s: %d", ids[i], m.code)
				return
			}
			for {
				typ, body, err := m.next(10 * time.Second)
				if err != nil {
					return
				}
				// Each job's bytes end with a 3-byte frame naming it.
				if typ == framePipe && len(body) == 3 {
					mu.Lock()
					jobsOf[ids[i]] = append(jobsOf[ids[i]], string(body))
					mu.Unlock()
					m.send(frameStdout, []byte(ids[i]))
					m.send(frameExit, i32(100))
				}
			}
		}()
		go func() {
			defer wg.Done()
			s := p.shim(t)
			job := fmt.Sprintf("00000000-0000-4000-8000-%012d", i)
			msg := jobMessage(job, int64(i+1), "unit")
			s.send(framePipe, msg)
			s.send(framePipe, []byte(fmt.Sprintf("%03d", i)))
			for {
				typ, body, err := s.next(10 * time.Second)
				if err != nil {
					t.Errorf("job %d: %v", i, err)
					return
				}
				switch typ {
				case frameStdout:
					mu.Lock()
					ranOn[fmt.Sprintf("%03d", i)] = string(body)
					mu.Unlock()
				case frameExit:
					return
				}
			}
		}()
	}
	wg.Wait()
	if len(ranOn) != n {
		t.Fatalf("%d of %d jobs ran", len(ranOn), n)
	}
	used := map[string]bool{}
	for job, machine := range ranOn {
		if used[machine] {
			t.Errorf("machine %s ran two jobs", machine)
		}
		used[machine] = true
		if got := jobsOf[machine]; len(got) != 1 || got[0] != job {
			t.Errorf("machine %s got the bytes of jobs %v, want only %s", machine, got, job)
		}
	}
}

// A shim still PINGing while its job's EXIT is relayed gets the EXIT and a clean end: the dispatcher reads what the
// shim still sends rather than close with it unread, which would reset the connection and discard the EXIT on its way.
func TestAShimThatKeepsPingingWhileItsExitIsRelayedStillGetsIt(t *testing.T) {
	store := &fakeS3{objects: map[string]string{}}
	p := startDispatcher(t, store, identityTags(), dispatchSet("i-0m1"))
	m, s, _ := handOne(t, p, "i-0m1", jobA)
	stop, failed := make(chan struct{}), make(chan error, 1)
	go func() {
		for tick := time.NewTicker(time.Millisecond); ; {
			select {
			case <-stop:
				tick.Stop()
				close(failed)
				return
			case <-tick.C:
				if _, err := s.conn.Write(appendFrame(nil, framePing, nil)); err != nil {
					failed <- err
					return
				}
			}
		}
	}()
	m.send(frameExit, i32(100))
	if code := exitCode(s.expect(frameExit)); code != 100 {
		t.Fatalf("exit %d", code)
	}
	time.AfterFunc(300*time.Millisecond, func() { close(stop) })
	if err := <-failed; err != nil {
		t.Fatalf("the shim's PINGs after the EXIT failed (%v): the dispatcher reset the connection", err)
	}
	if _, _, err := s.next(2 * time.Second); err != io.EOF {
		t.Fatalf("after the EXIT the shim read %v, want a clean end (close_notify)", err)
	}
}

// PING liveness: the dispatcher PINGs every connection, and ends one that has been silent for the silence.
func TestASilentConnectionEndsAndOneThatPingsHolds(t *testing.T) {
	store := &fakeS3{objects: map[string]string{}}
	p := startDispatcherPinging(t, store, identityTags(), dispatchSet("i-0silent", "i-0alive"), 50*time.Millisecond, 400*time.Millisecond)
	silent := p.machine(t, "i-0silent")
	alive := p.machine(t, "i-0alive")
	waitUntil(t, "both wait", func() bool { return idleMachines(p.st) == 2 })
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for tick := time.NewTicker(100 * time.Millisecond); ; {
			select {
			case <-stop:
				return
			case <-tick.C:
				if _, err := alive.conn.Write(appendFrame(nil, framePing, nil)); err != nil {
					return
				}
			}
		}
	}()
	if typ, _, err := silent.next(time.Second); err == nil {
		t.Fatalf("got a frame of type %d, want only PINGs", typ)
	} else if isTimeout(err) {
		t.Fatal("the silent machine's connection was kept past the silence")
	}
	waitUntil(t, "the silent machine leaves the queue", func() bool { return idleMachines(p.st) == 1 })
	if phaseOf(p.st, "i-0alive") != mWaiting {
		t.Fatal("a machine that PINGs was dropped")
	}
	if _, _, err := alive.next(200 * time.Millisecond); err == nil || !isTimeout(err) {
		t.Fatalf("the machine that PINGs: %v, want its connection held, with only PINGs on it", err)
	}
}

// sessionOn is the job paired with a machine, once there is one.
func sessionOn(st *DispatchSet, machine string) *session {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.pairs[machine]
}

// heldForShim is what a job holds for its shim: its frames, the EXITs among them, and their cost against shimHeld.
func heldForShim(s *session) (frames, exits, cost int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, f := range s.toShim {
		frames++
		cost += len(f.p) + heldCost
		if f.t == frameExit {
			exits++
		}
	}
	return frames, exits, cost
}

// A machine's job code can rejoin with its own resume token while its shim is away, after a restart, and send frames
// in a loop: what the job holds for its shim stays within shimHeld, with one EXIT, and the shim that rejoins gets
// that one EXIT.
func TestAFloodOfFramesWhileTheShimIsAwayLeavesTheJobBounded(t *testing.T) {
	st := dispatchSet("i-0m1")
	s := st.adopt("i-0m1", "t."+jobA, jobA, jobClaim{}, time.Now())
	mw := &wire{done: make(chan struct{})}
	s.mu.Lock()
	s.from = mw
	s.mu.Unlock()
	for i := range 200_000 {
		s.fromMachine(mw, []frameType{frameStdout, frameStderr}[i%2], []byte("x"))
	}
	for range 200_000 {
		s.fromMachine(mw, frameStdout, nil)
	}
	for range 100_000 {
		s.fromMachine(mw, frameExit, i32(1))
	}
	frames, exits, cost := heldForShim(s)
	if exits != 1 {
		t.Fatalf("the job holds %d EXITs for its shim, want one", exits)
	}
	if cost > shimHeld+heldCost+4 {
		t.Fatalf("the job holds %d frames costing %d bytes for its shim, past the bound of %d and its EXIT", frames, cost, shimHeld)
	}
	if c := st.TakeCounts(); c.Dropped == 0 {
		t.Fatal("frames past the bound were dropped uncounted")
	}
	c1, c2 := net.Pipe()
	defer c2.Close()
	go s.attachShim(newWire(c1, bufio.NewReader(c1), time.Hour, time.Minute, nil))
	shim := &peer{t: t, r: bufio.NewReader(c2)}
	got := 0
	for {
		_ = c2.SetReadDeadline(time.Now().Add(5 * time.Second))
		var h [5]byte
		if _, err := ioReadFull(shim.r, h[:]); err != nil {
			t.Fatal(err)
		}
		body := make([]byte, binary.LittleEndian.Uint32(h[1:]))
		if _, err := ioReadFull(shim.r, body); err != nil {
			t.Fatal(err)
		}
		if frameType(h[0]) == frameExit {
			if exitCode(body) != 1 {
				t.Fatalf("exit %d", exitCode(body))
			}
			break
		}
		got += len(body) + heldCost
	}
	if got > shimHeld {
		t.Fatalf("the shim that rejoined got %d bytes at heldCost a frame, past the bound of %d", got, shimHeld)
	}
}

// A PIPE frame whose send found the machine's connection gone is held for the machine's next connection: the
// machine that rejoins gets every byte of the job, in order.
func TestAPipeFrameNeverWrittenGoesToTheMachineThatRejoins(t *testing.T) {
	t.Run("the machine's connection ended between JOB and the flush", func(t *testing.T) {
		store := &fakeS3{objects: map[string]string{}}
		hooked := &hookedClaims{Claims: &Claims{store: store, bucket: "claims"}}
		p := startDispatcher(t, store, identityTags(), dispatchSet("i-0m1"))
		p.d.claims = hooked
		m := p.machine(t, "i-0m1")
		waitUntil(t, "the machine waits", func() bool { return phaseOf(p.st, "i-0m1") == mWaiting })
		hooked.before = func(name string) {
			if name == jobFence(jobA) {
				m.conn.Close()
				waitUntil(t, "the dispatcher sees the machine gone", func() bool { st := p.st; st.mu.Lock(); defer st.mu.Unlock(); return st.machines["i-0m1"].wire == nil })
			}
		}
		s := p.shim(t)
		msg := jobMessage(jobA, 7, "unit")
		s.sendJob(msg)
		inst, token := s.paired()
		waitUntil(t, "the job is handed and its machine away", func() bool {
			j := sessionOn(p.st, "i-0m1")
			if j == nil {
				return false
			}
			j.mu.Lock()
			defer j.mu.Unlock()
			return !j.away[0].IsZero()
		})
		again := p.machine(t, inst, resumeHeader, inst+"."+token)
		if again.code != 101 {
			t.Fatalf("resume: %d", again.code)
		}
		if got := again.pipeBytes(len(msg)); !bytes.Equal(got, msg) {
			t.Fatalf("the machine that rejoined got %d bytes that are not the job message's %d", len(got), len(msg))
		}
	})
	t.Run("the machine's connection ended under a PIPE frame from the shim", func(t *testing.T) {
		st := dispatchSet("i-0m1")
		s := st.adopt("i-0m1", "t."+jobA, jobA, jobClaim{}, time.Now())
		dead := &wire{done: make(chan struct{})}
		close(dead.done)
		s.mu.Lock()
		s.mach, s.from = dead, dead
		s.mu.Unlock()
		if err := s.pipe([]byte("cancel")); err != nil {
			t.Fatal(err)
		}
		c1, c2 := net.Pipe()
		defer c2.Close()
		live := newWire(c1, bufio.NewReader(c1), time.Hour, time.Minute, nil)
		s.mu.Lock()
		s.from = live
		s.mu.Unlock()
		go s.attachMachine(live)
		machine := &peer{t: t, conn: nil, r: bufio.NewReader(c2)}
		_ = c2.SetReadDeadline(time.Now().Add(5 * time.Second))
		var h [5]byte
		if _, err := ioReadFull(machine.r, h[:]); err != nil {
			t.Fatalf("the machine that rejoined got nothing: %v", err)
		}
		body := make([]byte, binary.LittleEndian.Uint32(h[1:]))
		if _, err := ioReadFull(machine.r, body); err != nil || frameType(h[0]) != framePipe || string(body) != "cancel" {
			t.Fatalf("the machine that rejoined got a frame of type %d %q (%v), want the PIPE frame whose send failed", h[0], body, err)
		}
	})
}

// A listener that crashes after the job's claim and before the job's bytes reach the machine leaves a machine with
// JOB and no job message. Its agent sends EXIT 1 when no first message comes, and that reaches the shim through the
// session the next dispatcher adopts: the job fails once and runs nowhere else.
func TestAMachineThatLostItsJobMessageToACrashFailsTheJobThroughTheNextDispatcher(t *testing.T) {
	store := &fakeS3{objects: map[string]string{}}
	hooked := &hookedClaims{Claims: &Claims{store: store, bucket: "claims"}}
	p := startDispatcher(t, store, identityTags(), dispatchSet("i-0m1"))
	p.d.claims = hooked
	dead := make(chan struct{})
	t.Cleanup(func() { close(dead) })
	hooked.after = func(name string) {
		if name == jobFence(jobA) {
			<-dead
		}
	}
	m := p.machine(t, "i-0m1")
	waitUntil(t, "the machine waits", func() bool { return phaseOf(p.st, "i-0m1") == mWaiting })
	s := p.shim(t)
	s.sendJob(jobMessage(jobA, 7, "unit"))
	token := m.job().Token
	s.paired()
	waitUntil(t, "the job's claim is taken", func() bool {
		store.mu.Lock()
		defer store.mu.Unlock()
		return store.objects["issued/"+jobFence(jobA)] != ""
	})
	p.d.mu.Lock()
	for w := range p.d.wires {
		go w.close()
	}
	p.d.mu.Unlock()
	next := startDispatcher(t, store, identityTags(), dispatchSet("i-0m1"))
	m = next.machine(t, "i-0m1", resumeHeader, "i-0m1."+token)
	if m.code != 101 {
		t.Fatalf("resume: %d", m.code)
	}
	if err := m.quiet(300 * time.Millisecond); err != nil {
		t.Fatalf("the machine that rejoined: %v", err)
	}
	m.send(frameExit, i32(1))
	s = next.shim(t, resumeHeader, "i-0m1."+token)
	if s.code != 101 {
		t.Fatalf("resume: %d", s.code)
	}
	if code := exitCode(s.expect(frameExit)); code != 1 {
		t.Fatalf("exit %d, want the agent's 1", code)
	}
	waitUntil(t, "the machine ends", func() bool { return phaseOf(next.st, "i-0m1") == mEnding })
}

// countingTags is EC2's tags, counting each read and holding each one while gate is open.
type countingTags struct {
	fakeTags
	mu       sync.Mutex
	reads    map[string]int
	inflight int
	peak     int
	gate     chan struct{}
}

func (c *countingTags) Tags(ctx context.Context, region, instance string) (map[string]string, error) {
	c.mu.Lock()
	c.reads[instance]++
	c.inflight++
	c.peak = max(c.peak, c.inflight)
	gate := c.gate
	c.mu.Unlock()
	if gate != nil {
		<-gate
	}
	c.mu.Lock()
	c.inflight--
	c.mu.Unlock()
	return c.fakeTags.Tags(ctx, region, instance)
}

func (c *countingTags) readsOf(instance string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reads[instance]
}

// Any instance of the account can call /shim, job code included: one that is no identity host costs one read of its
// tags a minute however often it calls, and the reads at once are bounded.
func TestShimCallersCostBoundedReadsOfTheirTags(t *testing.T) {
	store := &fakeS3{objects: map[string]string{}}
	p := startDispatcher(t, store, identityTags(), dispatchSet())
	tags := &countingTags{fakeTags: identityTags(), reads: map[string]int{}}
	p.d.tags = tags
	runner := base64.StdEncoding.EncodeToString([]byte(idRunner))
	for range 20 {
		if got := p.dial(t, "/shim", "i-0job", runnerHeader, runner).code; got != http.StatusForbidden {
			t.Fatalf("/shim from a job's machine: %d, want 403", got)
		}
	}
	if n := tags.readsOf("i-0job"); n != 1 {
		t.Fatalf("20 calls from one instance that is no identity host read its tags %d times, want once", n)
	}
	gate := make(chan struct{})
	tags.mu.Lock()
	tags.gate = gate
	tags.mu.Unlock()
	var wg sync.WaitGroup
	var mu sync.Mutex
	busy := 0
	for i := range 3 * tagLookups {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := p.d.identityHost(context.Background(), InstanceDocument{InstanceID: fmt.Sprintf("i-0caller%02d", i), Region: "us-east-1"})
			if err != nil && !errors.Is(err, errNotIdentity) {
				mu.Lock()
				busy++
				mu.Unlock()
			}
		}()
	}
	waitUntil(t, "the callers past the bound are turned away", func() bool { mu.Lock(); defer mu.Unlock(); return busy == 2*tagLookups })
	if got := p.dial(t, "/shim", "i-0other", runnerHeader, runner).code; got != http.StatusServiceUnavailable {
		t.Fatalf("/shim while the reads are all in flight: %d, want 503", got)
	}
	close(gate)
	wg.Wait()
	if tags.peak > tagLookups {
		t.Fatalf("%d reads of tags ran at once, want at most %d", tags.peak, tagLookups)
	}
}

// faultyClaims is an S3 that answers no machine's claim: every one is unknown. The jobs' own claims are taken.
type faultyClaims struct {
	*Claims
	mu    sync.Mutex
	takes map[string][]string // by machine, the tokens its claim was asked with
}

func (f *faultyClaims) Take(ctx context.Context, name, token string) error {
	if strings.HasPrefix(name, "job-") {
		return f.Claims.Take(ctx, name, token)
	}
	f.mu.Lock()
	f.takes[name] = append(f.takes[name], token)
	f.mu.Unlock()
	return errors.New("SlowDown: please reduce your request rate")
}

func spent(st *DispatchSet) int {
	st.mu.Lock()
	defer st.mu.Unlock()
	n := 0
	for _, m := range st.machines {
		if m.phase == mEnding {
			n++
		}
	}
	return n
}

// A machine's claim whose answer is unknown is asked again with its token before the machine is spent, and a lasting
// S3 fault spends at most claimSpend machines on a job before it waits.
func TestALastingS3FaultDoesNotChurnMachines(t *testing.T) {
	t.Run("a fault shorter than the tries spends no machine", func(t *testing.T) {
		store := &fakeS3{objects: map[string]string{}}
		p := startDispatcher(t, store, identityTags(), dispatchSet("i-0m1"))
		store.mu.Lock()
		store.fail = claimTries - 1
		store.mu.Unlock()
		handOne(t, p, "i-0m1", jobA)
		if c := p.st.TakeCounts(); c.Retries != 0 || c.Handoffs != 1 {
			t.Fatalf("counts %+v, want the job on its first machine", c)
		}
	})
	t.Run("a lasting fault", func(t *testing.T) {
		ids := make([]string, 10)
		for i := range ids {
			ids[i] = fmt.Sprintf("i-0m%02d", i)
		}
		store := &fakeS3{objects: map[string]string{}}
		p := startDispatcher(t, store, identityTags(), dispatchSet(ids...))
		faulty := &faultyClaims{Claims: &Claims{store: store, bucket: "claims"}, takes: map[string][]string{}}
		p.d.claims = faulty
		var machines []*peer
		for _, id := range ids {
			machines = append(machines, p.machine(t, id))
		}
		waitUntil(t, "the machines wait", func() bool { return idleMachines(p.st) == len(ids) })
		s := p.shim(t)
		s.sendJob(jobMessage(jobA, 7, "unit"))
		waitUntil(t, "the job spends its machines", func() bool { return spent(p.st) == claimSpend })
		for deadline := time.Now().Add(1500 * time.Millisecond); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			if n := spent(p.st); n > claimSpend {
				t.Fatalf("a lasting S3 fault spent %d machines on one job, want %d and then a wait", n, claimSpend)
			}
		}
		faulty.mu.Lock()
		defer faulty.mu.Unlock()
		for id, tokens := range faulty.takes {
			if len(tokens) != claimTries || tokens[0] != tokens[len(tokens)-1] {
				t.Errorf("machine %s's claim was asked %d times with tokens %v, want %d times with one token", id, len(tokens), tokens, claimTries)
			}
		}
		if snap := p.st.Snapshot(time.Now()); snap.JobsWaiting != 1 || snap.Idle != len(ids)-claimSpend {
			t.Fatalf("%d jobs waiting and %d machines idle, want the job queued and the rest of the machines kept", snap.JobsWaiting, snap.Idle)
		}
		if err := machines[len(machines)-1].quiet(100 * time.Millisecond); err != nil {
			t.Fatalf("a machine past the spent ones: %v", err)
		}
	})
}

// The job's claim is asked again while S3's answer is unknown, whether the claim landed or not: the job is handed to
// its machine and no machine is spent.
func TestAJobClaimWhoseAnswerIsUnknownIsAskedAgain(t *testing.T) {
	for _, c := range []struct {
		name  string
		fault func(*fakeS3)
	}{
		{"not made", func(f *fakeS3) { f.fail = 1 }},
		{"made, with its answer lost", func(f *fakeS3) { f.lose = 1 }},
	} {
		t.Run(c.name, func(t *testing.T) {
			store := &fakeS3{objects: map[string]string{}}
			hooked := &hookedClaims{Claims: &Claims{store: store, bucket: "claims"}}
			p := startDispatcher(t, store, identityTags(), dispatchSet("i-0m1"))
			p.d.claims = hooked
			var once sync.Once
			hooked.before = func(name string) {
				if name == jobFence(jobA) {
					once.Do(func() {
						store.mu.Lock()
						c.fault(store)
						store.mu.Unlock()
					})
				}
			}
			handOne(t, p, "i-0m1", jobA)
			if counts := p.st.TakeCounts(); counts.Retries != 0 || counts.Handoffs != 1 {
				t.Fatalf("counts %+v, want the job handed to its machine", counts)
			}
		})
	}
}

// A stop lets a hand-off in flight reach its handed point and write the job's bytes before it ends the connections.
func TestAStopLetsAHandOffInFlightWriteTheJob(t *testing.T) {
	store := &fakeS3{objects: map[string]string{}}
	hooked := &hookedClaims{Claims: &Claims{store: store, bucket: "claims"}}
	p := startDispatcher(t, store, identityTags(), dispatchSet("i-0m1"))
	p.d.claims = hooked
	inFlight := make(chan struct{})
	hooked.before = func(name string) {
		if name == jobFence(jobA) {
			close(inFlight)
			time.Sleep(300 * time.Millisecond)
		}
	}
	m := p.machine(t, "i-0m1")
	waitUntil(t, "the machine waits", func() bool { return phaseOf(p.st, "i-0m1") == mWaiting })
	s := p.shim(t)
	msg := jobMessage(jobA, 7, "unit")
	s.sendJob(msg)
	m.job()
	s.paired()
	<-inFlight
	p.d.Close()
	if got := m.pipeBytes(len(msg)); !bytes.Equal(got, msg) {
		t.Fatal("the machine did not get the job message before the stop")
	}
}

// The reconciler does not wait on a shim that does not read: the lost job's exit code 1 goes to it on its own.
func TestTheReconcilerDoesNotWaitOnAShimThatDoesNotRead(t *testing.T) {
	st := dispatchSet("i-0m1")
	now := time.Now()
	s := st.adopt("i-0m1", "t."+jobA, jobA, jobClaim{}, now)
	c1, c2 := net.Pipe()
	defer c2.Close()
	s.attachShim(newWire(c1, bufio.NewReader(c1), time.Hour, time.Minute, nil))
	done := make(chan struct{})
	go func() {
		st.Reconcile(now.Add(4*time.Minute), bounds)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the reconciler waited on a shim that does not read")
	}
	_ = c2.SetReadDeadline(time.Now().Add(5 * time.Second))
	var h [9]byte
	if _, err := io.ReadFull(c2, h[:]); err != nil || frameType(h[0]) != frameExit || exitCode(h[5:]) != 1 {
		t.Fatalf("the lost job's shim read %v (%v), want EXIT 1", h, err)
	}
}

// A job adopted after a restart keeps what its claim names of it, so a lost one is re-run.
func TestAJobLostAfterARestartIsReportedForItsReRun(t *testing.T) {
	store := &fakeS3{objects: map[string]string{}}
	p := startDispatcher(t, store, identityTags(), dispatchSet("i-0m1"))
	_, _, token := handOne(t, p, "i-0m1", jobA)
	p = p.restart(t, store, identityTags(), "i-0m1")
	s := p.shim(t, resumeHeader, "i-0m1."+token)
	if s.code != 101 {
		t.Fatalf("resume: %d", s.code)
	}
	_, lost := p.st.Reconcile(time.Now().Add(4*time.Minute), bounds)
	want := JobRef{Owner: "acme", Repo: "mono", RunID: 7, Name: "unit", Runner: "superci-id-4cpu-17", Instance: "i-0m1"}
	if len(lost) != 1 || lost[0] != want {
		t.Fatalf("lost %+v, want %+v", lost, want)
	}
	if code := exitCode(s.expect(frameExit)); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
}

// A shim that dies after the handed point (its identity's listener was killed) loses nothing: the machine renews the
// job itself, so it is kept while it holds its connection, however long the shim is away, and its EXIT ends it.
func TestAShimThatDiesAfterTheHandedPointLeavesTheJobRunningToItsExit(t *testing.T) {
	store := &fakeS3{objects: map[string]string{}}
	p := startDispatcher(t, store, identityTags(), dispatchSet("i-0m1"))
	m, s, _ := handOne(t, p, "i-0m1", jobA)
	s.conn.Close()
	waitUntil(t, "the dispatcher sees the shim gone", func() bool {
		j := sessionOn(p.st, "i-0m1")
		j.mu.Lock()
		defer j.mu.Unlock()
		return j.shim == nil
	})
	end, lost := p.st.Reconcile(time.Now().Add(10*time.Minute), bounds)
	if len(end) != 0 || len(lost) != 0 || phaseOf(p.st, "i-0m1") != mClaimed {
		t.Fatalf("with the shim away past the window: terminate %v, lost %v, machine in phase %d; want the job running on", end, lost, phaseOf(p.st, "i-0m1"))
	}
	m.send(frameStdout, []byte("still running"))
	m.send(frameExit, i32(0))
	if !m.ended(2 * time.Second) {
		t.Fatal("the machine's connection was kept after its EXIT, with no shim to take it")
	}
	waitUntil(t, "the machine ends", func() bool { return phaseOf(p.st, "i-0m1") == mEnding })
	end, _ = p.st.Reconcile(time.Now().Add(10*time.Minute), bounds)
	if len(end) != 1 || end[0].ID != "i-0m1" {
		t.Fatalf("terminate %v, want the machine whose job ended", end)
	}
	if c := p.st.TakeCounts(); c.Lost != 0 {
		t.Fatalf("counts %+v: a job that ran to its exit code is not lost", c)
	}
}

// A dispatched job never passes through State.Observe, so the dispatcher writes its record: exactly one when it is over,
// with the job's names and workflow from its job message, its result from its Worker's exit code, and its machine.
func TestADispatchedJobLeavesExactlyOneRecord(t *testing.T) {
	store := &fakeS3{objects: map[string]string{}}
	p := startDispatcher(t, store, identityTags(), dispatchSet("i-0m1"))
	m := p.machine(t, "i-0m1")
	waitUntil(t, "the machine waits", func() bool { return idleMachines(p.st) == 1 })
	s := p.shim(t)
	msg := jobMessage("5f0c3b52-9b8d-4c62-8e31-1a2b3c4d5e6f", 4242, "unit (1/4)")
	s.sendJob(msg)
	m.job()
	s.paired()
	m.pipeBytes(len(msg))
	m.send(frameExit, i32(100))
	s.expect(frameExit)
	waitUntil(t, "the job is over", func() bool { return phaseOf(p.st, "i-0m1") == mEnding })
	bounds := dispatchBounds{maxAge: time.Hour, life: 9 * time.Hour, job: 6 * time.Hour, window: 3 * time.Minute}
	p.st.Reconcile(time.Now(), bounds)
	p.st.Reconcile(time.Now().Add(time.Hour), bounds)
	recs := p.st.TakeJobs()
	if len(recs) != 1 {
		t.Fatalf("%d records, want one for the one job", len(recs))
	}
	r := recs[0]
	if !r.Dispatched || r.Job != "5f0c3b52-9b8d-4c62-8e31-1a2b3c4d5e6f" || r.Run != 4242 || r.Name != "unit (1/4)" || r.Repo != "acme/mono" ||
		r.WorkflowRef != "acme/mono/.github/workflows/ci.yml@refs/heads/main" || r.Event != "push" || r.Runner != "superci-id-4cpu-17" ||
		r.Inst != "i-0m1" || r.Machine == nil || r.Conclusion() != "success" || r.Partial() || r.Started.Before(r.Queued) || r.Finished.Before(r.Started) {
		t.Fatalf("record %+v", r)
	}
	if again := p.st.TakeJobs(); len(again) != 0 {
		t.Fatalf("%d more records after the job was over", len(again))
	}
}

func TestALostDispatchedJobLeavesOneFailedRecord(t *testing.T) {
	for code, want := range map[int32]string{102: "failure", 103: "cancelled", 1: "failure"} {
		st := dispatchSet("i-0m1")
		s := &session{set: st, id: "job-1", agent: "superci-id-4cpu-17", machine: "i-0m1", arrived: time.Now().Add(-time.Minute), handed: time.Now().Add(-50 * time.Second),
			head: jobHead{ID: "job-1", Run: 7, Name: "n", Repo: "acme/mono"}, phase: running, exited: true, code: code}
		st.pairs["i-0m1"], st.byJob["job-1"] = s, s
		st.finish(s, exitRelayed)
		st.finish(s, exitRelayed)
		recs := st.TakeJobs()
		if len(recs) != 1 || recs[0].Conclusion() != want {
			t.Fatalf("exit %d: records %+v, want one %s", code, recs, want)
		}
	}
	st := dispatchSet("i-0m1")
	s := &session{set: st, id: "job-2", agent: "superci-id-4cpu-17", machine: "i-0m1", arrived: time.Now(), handed: time.Now(), phase: running, exited: true, code: 100}
	st.pairs["i-0m1"], st.byJob["job-2"] = s, s
	st.finish(s, machineLost)
	if recs := st.TakeJobs(); len(recs) != 1 || recs[0].Conclusion() != "failure" {
		t.Fatalf("a lost job: records %+v, want one failure", recs)
	}
}
