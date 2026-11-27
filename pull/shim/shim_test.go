package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// A fconn is one connection the fake dispatcher accepted: its upgrade request and the frames either way.
type fconn struct {
	n    int
	fake *fakeDispatcher
	req  *http.Request
	doc  []byte
	conn net.Conn
	br   *bufio.Reader
}

func (c *fconn) up() {
	io.WriteString(c.conn, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: superci-dispatch/1\r\n\r\n")
}

func (c *fconn) send(typ byte, p []byte) {
	writeFrame(c.conn, typ, p)
}

func (c *fconn) header(typ byte, n uint32) {
	var h [5]byte
	h[0] = typ
	binary.LittleEndian.PutUint32(h[1:], n)
	c.conn.Write(h[:])
}

func (c *fconn) frame() (byte, []byte, error) {
	var h [5]byte
	if _, err := io.ReadFull(c.br, h[:]); err != nil {
		return 0, nil, err
	}
	p := make([]byte, binary.LittleEndian.Uint32(h[1:]))
	_, err := io.ReadFull(c.br, p)
	return h[0], p, err
}

// pipeUntil reads frames until the PIPE bytes it has read hold want, and returns those bytes.
func (c *fconn) pipeUntil(t *testing.T, want string) string {
	t.Helper()
	var got []byte
	for !bytes.Contains(got, []byte(want)) {
		typ, p, err := c.frame()
		if err != nil {
			t.Errorf("conn %d: read before PIPE bytes %q arrived (got %q): %v", c.n, want, got, err)
			return string(got)
		}
		if typ == typePipe {
			got = append(got, p...)
		}
	}
	return string(got)
}

// closedWithin reads until the shim closes the connection or d passes, reporting whether it closed.
func (c *fconn) closedWithin(d time.Duration) bool {
	c.conn.SetReadDeadline(time.Now().Add(d))
	_, err := io.Copy(io.Discard, c.br)
	return err == nil
}

type fakeDispatcher struct {
	ln     net.Listener
	pin    string
	mu     sync.Mutex
	conns  int
	reqs   []*fconn
	handle func(c *fconn)
}

func newFake(t *testing.T, handle func(c *fconn)) *fakeDispatcher {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "fake"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}})
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeDispatcher{ln: ln, pin: "sha256//" + base64.StdEncoding.EncodeToString(sum[:]), handle: handle}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			raw, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer raw.Close()
				br := bufio.NewReader(raw)
				req, err := http.ReadRequest(br)
				if err != nil {
					return
				}
				doc, _ := io.ReadAll(io.LimitReader(req.Body, 1<<20))
				f.mu.Lock()
				c := &fconn{n: f.conns, fake: f, req: req, doc: doc, conn: raw, br: br}
				f.conns++
				f.reqs = append(f.reqs, c)
				f.mu.Unlock()
				f.handle(c)
				// Closing with the shim's PINGs unread makes TCP reset the connection, which discards the frames the shim has yet to read.
				raw.SetReadDeadline(time.Now().Add(2 * time.Second))
				io.Copy(io.Discard, br)
			}()
		}
	}()
	return f
}

func (f *fakeDispatcher) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.conns
}

func fakeIMDS(t *testing.T, doc string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/latest/api/token" && r.Header.Get("X-aws-ec2-metadata-token-ttl-seconds") != "":
			io.WriteString(w, "tok-1")
		case r.Method == http.MethodGet && r.URL.Path == "/latest/dynamic/instance-identity/rsa2048" && r.Header.Get("X-aws-ec2-metadata-token") == "tok-1":
			io.WriteString(w, doc)
		default:
			http.Error(w, "no", http.StatusUnauthorized)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

const identityDoc = "SIGNED-IDENTITY-DOCUMENT"

type rig struct {
	fake    *fakeDispatcher
	s       *shim
	pipe    *os.File // the listener's end of the pipe it hands the Worker
	readEnd *os.File
	stdout  *syncBuf
	stderr  *syncBuf
}

func newRig(t *testing.T, label string, handle func(c *fconn)) *rig {
	t.Helper()
	f := newFake(t, handle)
	imds := fakeIMDS(t, identityDoc)
	d, err := newDialer(config{Listener: "https://" + f.ln.Addr().String(), Pin: f.pin, Label: label}, []byte(`{"agentName":"id-1"}`), imds.URL)
	if err != nil {
		t.Fatal(err)
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close(); w.Close() })
	g := &rig{fake: f, pipe: w, stdout: &syncBuf{}, stderr: &syncBuf{}}
	g.s = &shim{dial: d.dial, ob: newOutbox(maxBuffered), stdout: g.stdout, stderr: g.stderr,
		pingEvery: 40 * time.Millisecond, silence: 5 * time.Second, window: 800 * time.Millisecond, retry: 10 * time.Millisecond}
	g.readEnd = r
	return g
}

func (g *rig) run(t *testing.T) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	code := make(chan int, 1)
	go func() { code <- g.s.run(ctx, g.readEnd) }()
	select {
	case c := <-code:
		return c
	case <-time.After(25 * time.Second):
		t.Fatal("the shim did not exit")
		return -1
	}
}

func exitFrame(code int32) []byte {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], uint32(code))
	return b[:]
}

func paired(instance, token string) []byte {
	return []byte(`{"instance":"` + instance + `","token":"` + token + `"}`)
}

func TestTheWorkersExitCodeIsTheShimsAndTheRequestCarriesWhatTheDispatcherNeeds(t *testing.T) {
	g := newRig(t, "superci-4cpu", func(c *fconn) {
		c.up()
		c.send(typePaired, paired("i-0abc", "tok"))
		c.pipeUntil(t, "job-message")
		c.send(typeStdout, []byte("out-bytes"))
		c.send(typeStderr, []byte("err-bytes"))
		c.send(typeInfo, []byte(`{"k":1}`))
		c.send(typeExit, exitFrame(100))
	})
	g.pipe.WriteString("job-message")
	if code := g.run(t); code != 100 {
		t.Fatalf("the shim exited %d, want the Worker's 100", code)
	}
	if g.stdout.String() != "out-bytes" || g.stderr.String() != "err-bytes" {
		t.Errorf("stdout %q stderr %q", g.stdout.String(), g.stderr.String())
	}
	req := g.fake.reqs[0]
	if req.req.Method != "POST" || req.req.URL.Path != "/shim" {
		t.Errorf("request %s %s", req.req.Method, req.req.URL.Path)
	}
	if !strings.EqualFold(req.req.Header.Get("Upgrade"), "superci-dispatch/1") || !strings.Contains(strings.ToLower(req.req.Header.Get("Connection")), "upgrade") {
		t.Errorf("not an upgrade: %v", req.req.Header)
	}
	if string(req.doc) != identityDoc {
		t.Errorf("body %q, want the identity document", req.doc)
	}
	if got, _ := base64.StdEncoding.DecodeString(req.req.Header.Get("X-Superci-Runner")); string(got) != `{"agentName":"id-1"}` {
		t.Errorf("runner header decodes to %q", got)
	}
	if req.req.Header.Get("X-Superci-Label") != "superci-4cpu" || req.req.Header.Get("X-Superci-Resume") != "" {
		t.Errorf("label %q resume %q", req.req.Header.Get("X-Superci-Label"), req.req.Header.Get("X-Superci-Resume"))
	}
}

func TestExitCodesArriveAsTheyAre(t *testing.T) {
	for _, want := range []int32{0, 1, 100, 101, 102, 103, 104, 255} {
		g := newRig(t, "", func(c *fconn) { c.up(); c.send(typeExit, exitFrame(want)) })
		if got := g.run(t); got != int(want) {
			t.Errorf("EXIT %d: the shim exited %d", want, got)
		}
	}
}

func TestPingsGoOutWhileTheJobRuns(t *testing.T) {
	seen := make(chan struct{})
	g := newRig(t, "", func(c *fconn) {
		c.up()
		for {
			typ, _, err := c.frame()
			if err != nil {
				return
			}
			if typ == typePing {
				close(seen)
				c.send(typeExit, exitFrame(0))
				return
			}
		}
	})
	if g.run(t) != 0 {
		t.Fatal("no clean exit")
	}
	select {
	case <-seen:
	default:
		t.Fatal("the shim sent no PING")
	}
}

func TestAfterABreakTheShimResumesWithItsMachineAndSendsOnlyWhatWasNotSent(t *testing.T) {
	redialing, buffered := make(chan struct{}), make(chan struct{})
	g := newRig(t, "", func(c *fconn) {
		switch c.n {
		case 0:
			c.up()
			c.send(typePaired, paired("i-9", "secret-token"))
			c.pipeUntil(t, "job")
			c.send(typeStdout, []byte("a"))
			c.conn.Close()
		default:
			close(redialing)
			<-buffered
			c.up()
			got := c.pipeUntil(t, "later")
			if got != "later" {
				t.Errorf("after the resume the machine was sent %q, want only the bytes read while disconnected", got)
			}
			c.send(typeStdout, []byte("b"))
			c.send(typeExit, exitFrame(101))
		}
	})
	g.pipe.WriteString("job")
	go func() {
		<-redialing
		g.pipe.WriteString("later")
		for {
			g.s.ob.mu.Lock()
			n := g.s.ob.bytes
			g.s.ob.mu.Unlock()
			if n > 0 {
				break
			}
			time.Sleep(time.Millisecond)
		}
		close(buffered)
	}()
	if code := g.run(t); code != 101 {
		t.Fatalf("exit %d, want 101", code)
	}
	if got := g.fake.reqs[1].req.Header.Get("X-Superci-Resume"); got != "i-9.secret-token" {
		t.Errorf("resume header %q", got)
	}
	if g.stdout.String() != "ab" {
		t.Errorf("stdout %q, want output from both connections", g.stdout.String())
	}
}

func TestBeforeAMachineHasTheJobAFreshConnectionSendsTheJobMessageAgain(t *testing.T) {
	g := newRig(t, "", func(c *fconn) {
		c.up()
		got := c.pipeUntil(t, "whole-job-message")
		if c.n == 0 {
			c.conn.Close()
			return
		}
		if got != "whole-job-message" {
			t.Errorf("the fresh connection got %q, want the job message from its start", got)
		}
		if c.req.Header.Get("X-Superci-Resume") != "" {
			t.Errorf("a resume header %q before any PAIRED", c.req.Header.Get("X-Superci-Resume"))
		}
		c.send(typeExit, exitFrame(100))
	})
	g.pipe.WriteString("whole-job-message")
	if code := g.run(t); code != 100 {
		t.Fatalf("exit %d", code)
	}
	if g.fake.count() != 2 {
		t.Errorf("%d connections, want 2", g.fake.count())
	}
}

func TestALostMachineEndsTheShimWithOne(t *testing.T) {
	g := newRig(t, "", func(c *fconn) {
		if c.n == 0 {
			c.up()
			c.send(typePaired, paired("i-lost", "t"))
			c.conn.Close()
			return
		}
		io.WriteString(c.conn, "HTTP/1.1 410 Gone\r\nContent-Length: 0\r\n\r\n")
	})
	g.pipe.WriteString("job")
	start := time.Now()
	if code := g.run(t); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if time.Since(start) >= g.s.window {
		t.Errorf("a 410 took the whole resume window: %v", time.Since(start))
	}
	if g.fake.count() != 2 {
		t.Errorf("%d connections: the shim asked again after 410", g.fake.count())
	}
}

func TestAnIdentityHostTheDispatcherRefusesGetsOne(t *testing.T) {
	g := newRig(t, "", func(c *fconn) {
		io.WriteString(c.conn, "HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\n\r\n")
	})
	g.s.window = 10 * time.Second
	start := time.Now()
	if code := g.run(t); code != 1 {
		t.Fatalf("exit %d", code)
	}
	if time.Since(start) > 5*time.Second || g.fake.count() != 1 {
		t.Errorf("a 403 was retried: %d connections over %v", g.fake.count(), time.Since(start))
	}
}

func TestThreeMinutesOfFailedResumeEndTheShimWithOne(t *testing.T) {
	g := newRig(t, "", func(c *fconn) {
		c.up()
		c.send(typePaired, paired("i-gone", "t"))
		c.pipeUntil(t, "job")
		c.send(typeStdout, []byte("x"))
		c.conn.Close()
		c.fake.ln.Close()
	})
	g.pipe.WriteString("job")
	start := time.Now()
	code := g.run(t)
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if took := time.Since(start); took < g.s.window || took > 10*time.Second {
		t.Errorf("gave up after %v, want just after the resume window %v", took, g.s.window)
	}
	if !strings.Contains(g.stderr.String(), "lost") {
		t.Errorf("stderr %q says nothing for the job's log", g.stderr.String())
	}
}

func TestAWrongPinConnectsToNothing(t *testing.T) {
	reached := make(chan struct{}, 1)
	g := newRig(t, "", func(c *fconn) { reached <- struct{}{}; c.up(); c.send(typeExit, exitFrame(0)) })
	other := sha256.Sum256([]byte("another key"))
	d, err := newDialer(config{Listener: "https://" + g.fake.ln.Addr().String(), Pin: "sha256//" + base64.StdEncoding.EncodeToString(other[:])}, []byte("{}"), fakeIMDS(t, identityDoc).URL)
	if err != nil {
		t.Fatal(err)
	}
	g.s.dial = d.dial
	if code := g.run(t); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	select {
	case <-reached:
		t.Fatal("a listener whose key is not the pinned one received the identity document")
	default:
	}
}

func TestAFrameOutsideItsBoundEndsTheConnectionUnread(t *testing.T) {
	bad := map[string]func(c *fconn){
		"stdout over 1 MiB": func(c *fconn) { c.header(typeStdout, maxOutFrame+1) },
		"stderr over 1 MiB": func(c *fconn) { c.header(typeStderr, 1<<31) },
		"info over 4 KiB":   func(c *fconn) { c.header(typeInfo, maxInfoFrame+1) },
		"a 65th info frame": func(c *fconn) {
			for i := 0; i <= maxInfoFrames; i++ {
				c.send(typeInfo, []byte(`{}`))
			}
		},
		"exit of 3 bytes":      func(c *fconn) { c.send(typeExit, []byte{1, 0, 0}) },
		"exit of 5 bytes":      func(c *fconn) { c.send(typeExit, []byte{1, 0, 0, 0, 0}) },
		"unknown type":         func(c *fconn) { c.header(99, 0) },
		"a PIPE from the side": func(c *fconn) { c.send(typePipe, []byte("x")) },
		"a JOB":                func(c *fconn) { c.header(6, 0) },
		"a ping with a body":   func(c *fconn) { c.send(typePing, []byte("x")) },
		"paired over 1 KiB":    func(c *fconn) { c.header(typePaired, maxPairedLen+1) },
		"paired not JSON":      func(c *fconn) { c.send(typePaired, []byte("not json")) },
		"paired without token": func(c *fconn) { c.send(typePaired, []byte(`{"instance":"i-1"}`)) },
	}
	for name, frame := range bad {
		t.Run(name, func(t *testing.T) {
			closed := make(chan bool, 1)
			g := newRig(t, "", func(c *fconn) {
				c.up()
				if c.n == 0 {
					frame(c)
					closed <- c.closedWithin(3 * time.Second)
					return
				}
				c.send(typeStdout, []byte("fine"))
				c.send(typeExit, exitFrame(100))
			})
			if code := g.run(t); code != 100 {
				t.Fatalf("exit %d, want 100 from the second connection", code)
			}
			if !<-closed {
				t.Error("the shim kept the connection open past a frame outside its bound")
			}
			if g.stdout.String() != "fine" {
				t.Errorf("stdout %q: bytes of the refused frame reached the job's log", g.stdout.String())
			}
		})
	}
}

func TestOutputPastItsBudgetIsDroppedAndTheRestStillArrives(t *testing.T) {
	g := newRig(t, "", func(c *fconn) {
		c.up()
		chunk := bytes.Repeat([]byte("x"), maxOutFrame)
		for i := 0; i < 5; i++ {
			c.send(typeStdout, chunk)
		}
		c.send(typeStderr, []byte("e"))
		c.send(typeExit, exitFrame(100))
	})
	if code := g.run(t); code != 100 {
		t.Fatalf("exit %d", code)
	}
	if got := len(g.stdout.String()) + len(g.stderr.String()); got != outBudget {
		t.Errorf("%d bytes of output reached the listener, want the %d budget", got, outBudget)
	}
}

func TestInfoFramesAtTheirBoundAreTaken(t *testing.T) {
	g := newRig(t, "", func(c *fconn) {
		c.up()
		for i := 0; i < maxInfoFrames; i++ {
			c.send(typeInfo, bytes.Repeat([]byte("i"), maxInfoFrame))
		}
		c.send(typeExit, exitFrame(7))
	})
	if code := g.run(t); code != 7 {
		t.Fatalf("exit %d, want 7: a frame at its bound ended the connection", code)
	}
}

func TestSilenceFromTheDispatcherBreaksTheConnection(t *testing.T) {
	g := newRig(t, "", func(c *fconn) {
		c.up()
		if c.n == 0 {
			io.Copy(io.Discard, c.br)
			return
		}
		c.send(typeExit, exitFrame(100))
	})
	g.s.silence = 150 * time.Millisecond
	if code := g.run(t); code != 100 {
		t.Fatalf("exit %d", code)
	}
	if g.fake.count() != 2 {
		t.Errorf("%d connections, want the silent one dropped and a new one", g.fake.count())
	}
}

func TestWhatTheListenerWritesIsBoundedInMemory(t *testing.T) {
	ob := newOutbox(1 << 20)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	go ob.pump(r)
	go func() { w.Write(bytes.Repeat([]byte("z"), 8<<20)) }()
	deadline := time.Now().Add(5 * time.Second)
	var held int
	for time.Now().Before(deadline) {
		ob.mu.Lock()
		held = ob.bytes
		ob.mu.Unlock()
		if held >= 1<<20 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if held < 1<<20 {
		t.Fatalf("held only %d bytes", held)
	}
	time.Sleep(50 * time.Millisecond)
	ob.mu.Lock()
	over := ob.bytes - 1<<20
	ob.mu.Unlock()
	if over > pipeChunk {
		t.Errorf("held %d bytes past its 1 MiB limit with nothing taking them", over)
	}
}

func TestAJobMessageIsNotKeptOnceAMachineHasIt(t *testing.T) {
	ob := newOutbox(1 << 20)
	r, w, _ := os.Pipe()
	defer r.Close()
	defer w.Close()
	go ob.pump(r)
	w.WriteString("secret-job")
	for {
		if c, ok := ob.pop(); ok {
			if string(c) != "secret-job" {
				t.Fatalf("popped %q", c)
			}
			break
		}
		time.Sleep(time.Millisecond)
	}
	ob.release()
	ob.mu.Lock()
	defer ob.mu.Unlock()
	if len(ob.hist) != 0 || ob.bytes != 0 {
		t.Errorf("after PAIRED the shim still holds %d chunks (%d bytes) of what it sent", len(ob.hist), ob.bytes)
	}
}

func TestConfigAndPins(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "env")
	os.WriteFile(path, []byte("# the listener\nLISTENER=\"https://203.0.113.1:8443\"\nPIN=sha256//"+base64.StdEncoding.EncodeToString(make([]byte, 32))+"\nLABEL='superci-8cpu'\nOTHER=x\n"), 0o600)
	c, err := loadConfig(path)
	if err != nil || c.Listener != "https://203.0.113.1:8443" || c.Label != "superci-8cpu" || !strings.HasPrefix(c.Pin, "sha256//") {
		t.Fatalf("%+v %v", c, err)
	}
	if _, err := parsePins(c.Pin + ";" + c.Pin); err != nil {
		t.Error(err)
	}
	for _, bad := range []string{"", "sha1//AAAA", "sha256//not-base64!", "sha256//AAAA"} {
		if _, err := parsePins(bad); err == nil {
			t.Errorf("pin %q accepted", bad)
		}
	}
	os.WriteFile(path, []byte("LISTENER=https://x\n"), 0o600)
	if _, err := loadConfig(path); err == nil {
		t.Error("a config without a PIN loaded")
	}
}

func TestTheRunnerFileIsBesideBinAndLosesItsByteOrderMark(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "bin"), 0o755)
	os.WriteFile(filepath.Join(root, ".runner"), []byte("\ufeff{\"agentId\":1}"), 0o600)
	os.WriteFile(filepath.Join(root, ".credentials"), []byte("secret"), 0o600)
	p, err := runnerFile(filepath.Join(root, "bin", "Runner.Worker"))
	if err != nil || p != filepath.Join(root, ".runner") {
		t.Fatalf("%q %v", p, err)
	}
	got, err := readRunner(p)
	if err != nil || string(got) != `{"agentId":1}` {
		t.Errorf("%q %v", got, err)
	}
}

func TestWhatAMachineHasTakenIsNoLongerHeldSoALargeJobMessageFlows(t *testing.T) {
	const total = 1 << 20
	g := newRig(t, "", func(c *fconn) {
		c.up()
		c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		c.pipeUntil(t, "first")
		c.send(typePaired, paired("i-1", "t"))
		c.pipeUntil(t, "last")
		c.send(typeExit, exitFrame(100))
	})
	g.s.ob = newOutbox(256 << 10)
	go func() {
		g.pipe.WriteString("first")
		g.pipe.Write(bytes.Repeat([]byte("p"), total))
		g.pipe.WriteString("last")
	}()
	if code := g.run(t); code != 100 {
		t.Fatalf("exit %d", code)
	}
	g.s.ob.mu.Lock()
	defer g.s.ob.mu.Unlock()
	if len(g.s.ob.hist) != 0 {
		t.Errorf("%d chunks of what a machine took are still held", len(g.s.ob.hist))
	}
}

func TestTheIdentityDocumentIsFetchedWithoutAProxy(t *testing.T) {
	d, err := newDialer(config{Listener: "https://127.0.0.1:1", Pin: "sha256//" + base64.StdEncoding.EncodeToString(make([]byte, 32))}, nil, "http://169.254.169.254")
	if err != nil {
		t.Fatal(err)
	}
	tr, ok := d.http.Transport.(*http.Transport)
	if !ok || tr.Proxy != nil {
		t.Errorf("the IMDS client's transport %#v can use a proxy from the environment", d.http.Transport)
	}
}

func TestMainSurvivesAClosedStderr(t *testing.T) {
	if os.Getenv("SHIM_MAIN_HELPER") == "1" {
		os.Args = []string{"Runner.Worker"}
		main()
		return
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
	defer w.Close()
	cmd := exec.Command(os.Args[0], "-test.run=^TestMainSurvivesAClosedStderr$")
	cmd.Env = append(os.Environ(), "SHIM_MAIN_HELPER=1")
	cmd.Stderr = w
	var ee *exec.ExitError
	if err := cmd.Run(); !errors.As(err, &ee) || ee.ExitCode() != 1 {
		t.Fatalf("the shim with a closed stderr ended with %v, want its own exit 1 and not SIGPIPE", err)
	}
}
