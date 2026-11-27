package main

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"
)

// deadlineConn records the read deadlines the server sets on a connection, so a test reads the timeout the server
// holds a client to without waiting for it to pass.
type deadlineConn struct {
	net.Conn
	mu        sync.Mutex
	readLimit []time.Time
}

func (c *deadlineConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	c.readLimit = append(c.readLimit, t)
	c.mu.Unlock()
	return c.Conn.SetReadDeadline(t)
}

func (c *deadlineConn) SetDeadline(t time.Time) error {
	c.mu.Lock()
	c.readLimit = append(c.readLimit, t)
	c.mu.Unlock()
	return c.Conn.SetDeadline(t)
}

// limits is every read deadline set so far, in order.
func (c *deadlineConn) limits() []time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Time(nil), c.readLimit...)
}

// limit is the read deadline now in force: the last one set, the zero time for none.
func (c *deadlineConn) limit() time.Time {
	ls := c.limits()
	if len(ls) == 0 {
		return time.Time{}
	}
	return ls[len(ls)-1]
}

type connKey struct{}

type deadlineListener struct {
	net.Listener
	conns chan *deadlineConn
}

func (l deadlineListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	dc := &deadlineConn{Conn: c}
	l.conns <- dc
	return dc, nil
}

// servePublic serves h on publicServer over a loopback listener and returns its address and the connections it accepts.
func servePublic(t *testing.T, h http.Handler) (string, chan *deadlineConn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dl := deadlineListener{Listener: ln, conns: make(chan *deadlineConn, 8)}
	srv := publicServer("", h)
	srv.ConnContext = func(ctx context.Context, c net.Conn) context.Context { return context.WithValue(ctx, connKey{}, c) }
	go func() { _ = srv.Serve(dl) }()
	t.Cleanup(func() { _ = srv.Close() })
	return ln.Addr().String(), dl.conns
}

func TestAClientThatTricklesItsBodyIsGivenThirtySecondsToSendTheRequest(t *testing.T) {
	seen := make(chan time.Time, 1)
	addr, _ := servePublic(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Context().Value(connKey{}).(*deadlineConn).limit()
	}))
	before := time.Now()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	fmt.Fprintf(c, "POST /jit HTTP/1.1\r\nHost: listener\r\nContent-Length: 16384\r\n\r\nx")
	limit := <-seen
	after := time.Now()
	if limit.Before(before.Add(30*time.Second)) || limit.After(after.Add(30*time.Second)) {
		t.Fatalf("read deadline of the request %v, want 30 s from its start (between %v and %v)", limit, before.Add(30*time.Second), after.Add(30*time.Second))
	}
}

func TestAnUpgradedDispatchConnectionIsNotCutByTheRequestsReadTimeout(t *testing.T) {
	store := &fakeS3{objects: map[string]string{}}
	st := dispatchSet("i-0m1")
	const silence = 5 * time.Second
	d := NewDispatcher("superci-pull", []*DispatchSet{st}, trusting{}, &Claims{store: store, bucket: "claims"}, identityTags(), func(context.Context, string) {})
	d.ping, d.silence = time.Hour, silence
	t.Cleanup(d.Close)
	addr, accepted := servePublic(t, d)
	before := time.Now()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	fmt.Fprintf(c, "POST /work HTTP/1.1\r\nHost: listener\r\nConnection: Upgrade\r\nUpgrade: %s\r\nContent-Length: 5\r\n\r\ni-0m1", dispatchProtocol)
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil || resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade: %v %v", resp, err)
	}
	conn := <-accepted
	limit := conn.limit()
	if !slices.ContainsFunc(conn.limits(), func(l time.Time) bool { return !l.Before(before.Add(30 * time.Second)) }) {
		t.Fatalf("read deadlines %v: the request was never held to 30 s, so the upgrade clearing it proves nothing", conn.limits())
	}
	if !limit.IsZero() && limit.After(time.Now().Add(silence)) {
		t.Fatalf("read deadline %v on the upgraded connection, want none or its wire's own (%v of silence), not the request's 30 s from %v", limit, silence, before)
	}
}
