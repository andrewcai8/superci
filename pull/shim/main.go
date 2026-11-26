// Command superci-shim is installed as an identity's bin/Runner.Worker. The identity's listener starts it with
// `Runner.Worker spawnclient <inHandle> <outHandle>` once it has acquired a job, and writes the job message into the
// first handle. The shim hands the job to a fresh machine through the dispatcher in the superci listener (pull/): it
// relays the listener's pipe bytes out and the machine's stdout, stderr and exit code back, and exits with the code the
// real Worker exited with. It holds no credential of the identity and keeps no job message once the dispatcher has
// paired the job with a machine.
package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Frame types and bounds of the dispatch protocol (D-SPEC.md, "Wire protocol"). A frame is <u8 type><u32 LE length><payload>.
const (
	typeStdout = 1
	typeStderr = 2
	typeExit   = 3
	typeInfo   = 4
	typePipe   = 5
	typePing   = 7
	typePaired = 8

	maxOutFrame   = 1 << 20 // one STDOUT or STDERR frame
	outBudget     = 4 << 20 // STDOUT and STDERR together, per job
	maxInfoFrame  = 4 << 10
	maxInfoFrames = 64
	maxPairedLen  = 1 << 10
	pipeChunk     = 64 << 10 // what one PIPE frame carries, well under the 1 MiB bound
	maxBuffered   = 32 << 20 // the dispatcher parses at most this much of the job message
	maxDocument   = 16 << 10
)

const (
	pingEvery    = 20 * time.Second
	silenceLimit = 60 * time.Second
	resumeWindow = 3 * time.Minute
	retryEvery   = time.Second
)

const envFile = "/etc/superci-shim.env"

type config struct {
	Listener string // https URL of the listener
	Pin      string // curl's --pinnedpubkey form: sha256//<base64>, several joined by ;
	Label    string // the set the identity serves, for the dispatcher; optional
}

// loadConfig reads KEY=VALUE lines (# comments, optional quotes) for LISTENER, PIN and LABEL.
func loadConfig(path string) (config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return config{}, err
	}
	var c config
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		switch strings.TrimSpace(k) {
		case "LISTENER":
			c.Listener = v
		case "PIN":
			c.Pin = v
		case "LABEL":
			c.Label = v
		}
	}
	if c.Listener == "" || c.Pin == "" {
		return config{}, fmt.Errorf("%s must set LISTENER and PIN", path)
	}
	return c, nil
}

// parsePins decodes curl's pinnedpubkey value: sha256//<base64> entries joined by ;.
func parsePins(s string) ([][]byte, error) {
	var pins [][]byte
	for _, p := range strings.Split(s, ";") {
		b64, ok := strings.CutPrefix(strings.TrimSpace(p), "sha256//")
		if !ok {
			return nil, errors.New("PIN is not sha256//<base64>")
		}
		sum, err := base64.StdEncoding.DecodeString(b64)
		if err != nil || len(sum) != sha256.Size {
			return nil, errors.New("PIN is not a base64 SHA-256")
		}
		pins = append(pins, sum)
	}
	return pins, nil
}

// A dialer opens one connection to the dispatcher: TLS to the pinned key, then an HTTP upgrade to the frame protocol.
type dialer struct {
	host    string // host:port
	pins    [][]byte
	label   string
	runner  []byte // the identity's .runner text, no credential
	imds    string
	timeout time.Duration
	http    *http.Client
}

func newDialer(c config, runner []byte, imds string) (*dialer, error) {
	u, err := url.Parse(c.Listener)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("LISTENER %q is not an https URL", c.Listener)
	}
	host := u.Host
	if u.Port() == "" {
		host = net.JoinHostPort(u.Hostname(), "443")
	}
	pins, err := parsePins(c.Pin)
	if err != nil {
		return nil, err
	}
	return &dialer{host: host, pins: pins, label: c.Label, runner: runner, imds: imds, timeout: 15 * time.Second,
		http: &http.Client{Timeout: 5 * time.Second}}, nil
}

// identityDocument is the instance identity document EC2 signed for this host, from IMDSv2.
func (d *dialer) identityDocument(ctx context.Context) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, d.imds+"/latest/api/token", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-aws-ec2-metadata-token-ttl-seconds", "600")
	resp, err := d.http.Do(req)
	if err != nil {
		return nil, err
	}
	tok, err := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
	resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("imds token: status %d", resp.StatusCode)
	}
	req, err = http.NewRequestWithContext(ctx, http.MethodGet, d.imds+"/latest/dynamic/instance-identity/rsa2048", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-aws-ec2-metadata-token", string(tok))
	resp, err = d.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	doc, err := io.ReadAll(io.LimitReader(resp.Body, maxDocument+1))
	if err != nil || resp.StatusCode != http.StatusOK || len(doc) == 0 || len(doc) > maxDocument {
		return nil, fmt.Errorf("imds document: status %d, %d bytes", resp.StatusCode, len(doc))
	}
	return doc, nil
}

// A dispatcherRefused is an answer other than 101 to the upgrade.
type dispatcherRefused struct{ status int }

func (e dispatcherRefused) Error() string { return fmt.Sprintf("dispatcher answered %d", e.status) }

// fatal reports an answer that retrying cannot change: the identity host is not allowed (403), or the machine is gone (410).
func (e dispatcherRefused) fatal() bool {
	return e.status == http.StatusForbidden || e.status == http.StatusGone
}

// dial connects, proves the host with its identity document, and returns the connection with the reader that holds any
// frame bytes read past the 101. resume is "<instance>.<token>" once a machine has the job.
func (d *dialer) dial(ctx context.Context, resume string) (net.Conn, *bufio.Reader, error) {
	doc, err := d.identityDocument(ctx)
	if err != nil {
		return nil, nil, err
	}
	nd := net.Dialer{Timeout: d.timeout}
	raw, err := nd.DialContext(ctx, "tcp", d.host)
	if err != nil {
		return nil, nil, err
	}
	conn := tls.Client(raw, &tls.Config{
		MinVersion: tls.VersionTLS12,
		// The listener's certificate is self-issued; what is checked is its key, as boot.sh's curl --pinnedpubkey does.
		InsecureSkipVerify: true,
		VerifyPeerCertificate: func(certs [][]byte, _ [][]*x509.Certificate) error {
			if len(certs) == 0 {
				return errors.New("no certificate")
			}
			cert, err := x509.ParseCertificate(certs[0])
			if err != nil {
				return err
			}
			sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
			for _, p := range d.pins {
				if string(p) == string(sum[:]) {
					return nil
				}
			}
			return errors.New("listener key does not match its pin")
		},
	})
	conn.SetDeadline(time.Now().Add(d.timeout))
	if err := conn.HandshakeContext(ctx); err != nil {
		conn.Close()
		return nil, nil, err
	}
	hdr := "POST /shim HTTP/1.1\r\nHost: " + d.host + "\r\nConnection: Upgrade\r\nUpgrade: superci-dispatch/1\r\n" +
		"Content-Type: text/plain\r\nContent-Length: " + strconv.Itoa(len(doc)) + "\r\n" +
		"X-Superci-Runner: " + base64.StdEncoding.EncodeToString(d.runner) + "\r\n"
	if d.label != "" {
		hdr += "X-Superci-Label: " + d.label + "\r\n"
	}
	if resume != "" {
		hdr += "X-Superci-Resume: " + resume + "\r\n"
	}
	if _, err := io.WriteString(conn, hdr+"\r\n"); err != nil {
		conn.Close()
		return nil, nil, err
	}
	if _, err := conn.Write(doc); err != nil {
		conn.Close()
		return nil, nil, err
	}
	br := bufio.NewReaderSize(conn, 64<<10)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodPost})
	if err != nil {
		conn.Close()
		return nil, nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		resp.Body.Close()
		conn.Close()
		return nil, nil, dispatcherRefused{resp.StatusCode}
	}
	conn.SetDeadline(time.Time{})
	return conn, br, nil
}

// An outbox holds the bytes read from the listener's pipe until a connection has sent them. Memory is bounded: past
// maxBuffered the pipe reader waits, so the listener's write waits in its own pipe.
//
// Before a machine has the job (no PAIRED yet) a fresh connection must start over, so what was sent stays in hist and
// replays. PAIRED drops it: from then on only unsent bytes are kept.
type outbox struct {
	mu     sync.Mutex
	chunks [][]byte
	hist   [][]byte
	paired bool
	bytes  int // chunks and hist together
	limit  int
	notify chan struct{}
	room   chan struct{}
}

func newOutbox(limit int) *outbox {
	return &outbox{limit: limit, notify: make(chan struct{}, 1), room: make(chan struct{}, 1)}
}

func poke(c chan struct{}) {
	select {
	case c <- struct{}{}:
	default:
	}
}

// pump reads the listener's pipe until it closes.
func (o *outbox) pump(in io.Reader) {
	buf := make([]byte, pipeChunk)
	for {
		n, err := in.Read(buf)
		if n > 0 {
			chunk := append([]byte(nil), buf[:n]...)
			for {
				o.mu.Lock()
				if o.bytes+n <= o.limit || o.bytes == 0 {
					o.chunks = append(o.chunks, chunk)
					o.bytes += n
					o.mu.Unlock()
					break
				}
				o.mu.Unlock()
				<-o.room
			}
			poke(o.notify)
		}
		if err != nil {
			return
		}
	}
}

// pop returns the next chunk to send, if there is one.
func (o *outbox) pop() ([]byte, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.chunks) == 0 {
		return nil, false
	}
	chunk := o.chunks[0]
	o.chunks = o.chunks[1:]
	if o.paired {
		o.bytes -= len(chunk)
		poke(o.room)
	} else {
		o.hist = append(o.hist, chunk)
	}
	return chunk, true
}

// rewind queues everything sent before PAIRED again, for a fresh connection.
func (o *outbox) rewind() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.chunks = append(o.hist, o.chunks...)
	o.hist = nil
}

// release drops the history: a machine has the job.
func (o *outbox) release() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.paired {
		return
	}
	o.paired = true
	for _, c := range o.hist {
		o.bytes -= len(c)
	}
	o.hist = nil
	poke(o.room)
}

type shim struct {
	dial   func(ctx context.Context, resume string) (net.Conn, *bufio.Reader, error)
	ob     *outbox
	stdout io.Writer
	stderr io.Writer

	pingEvery, silence, window, retry time.Duration

	instance, token string
	out             int // STDOUT and STDERR bytes taken so far, across connections
	info            int
}

func (s *shim) logf(format string, a ...any) {
	fmt.Fprintf(s.stderr, "superci-shim: "+format+"\n", a...)
}

type pairedMsg struct {
	Instance string `json:"instance"`
	Token    string `json:"token"`
}

// run relays one job and returns the exit code of the process: the Worker's code once the machine reports it, 1 once
// the machine is lost.
func (s *shim) run(ctx context.Context, in io.Reader) int {
	go s.ob.pump(in)
	outage := time.Now()
	for {
		resume := ""
		if s.instance != "" {
			resume = s.instance + "." + s.token
		} else {
			s.ob.rewind()
		}
		conn, br, err := s.dial(ctx, resume)
		if err == nil {
			code, exited, progressed := s.serve(ctx, conn, br)
			if exited {
				return code
			}
			if progressed {
				outage = time.Now()
			}
		} else {
			var refused dispatcherRefused
			if errors.As(err, &refused) && refused.fatal() {
				s.logf("%v: the job's machine is not available", err)
				return 1
			}
		}
		if time.Since(outage) > s.window {
			s.logf("no dispatcher for %v: the job is lost", s.window)
			return 1
		}
		select {
		case <-time.After(s.retry):
		case <-ctx.Done():
			return 1
		}
	}
}

// lenient drops a write error: the listener closing its end of the Worker's output must not end the job.
type lenient struct{ w io.Writer }

func (l lenient) Write(p []byte) (int, error) {
	l.w.Write(p)
	return len(p), nil
}

func writeFrame(w io.Writer, typ byte, p []byte) error {
	buf := make([]byte, 5+len(p))
	buf[0] = typ
	binary.LittleEndian.PutUint32(buf[1:], uint32(len(p)))
	copy(buf[5:], p)
	_, err := w.Write(buf)
	return err
}

// serve runs one connection until it breaks or the machine reports its exit. progressed is whether any frame arrived.
func (s *shim) serve(ctx context.Context, conn net.Conn, br *bufio.Reader) (code int, exited, progressed bool) {
	stop := make(chan struct{})
	var wmu sync.Mutex
	send := func(typ byte, p []byte) error {
		wmu.Lock()
		defer wmu.Unlock()
		conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
		return writeFrame(conn, typ, p)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		tick := time.NewTicker(s.pingEvery)
		defer tick.Stop()
		for {
			select {
			case <-tick.C:
				if send(typePing, nil) != nil {
					conn.Close()
					return
				}
				continue
			case <-stop:
				return
			default:
			}
			if c, ok := s.ob.pop(); ok {
				if send(typePipe, c) != nil {
					conn.Close()
					return
				}
				continue
			}
			select {
			case <-s.ob.notify:
			case <-tick.C:
				if send(typePing, nil) != nil {
					conn.Close()
					return
				}
			case <-stop:
				return
			}
		}
	}()
	defer func() {
		close(stop)
		conn.Close()
		wg.Wait()
	}()
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-stop:
		}
	}()

	var hdr [5]byte
	for {
		conn.SetReadDeadline(time.Now().Add(s.silence))
		if _, err := io.ReadFull(br, hdr[:]); err != nil {
			return 0, false, progressed
		}
		typ, n := hdr[0], binary.LittleEndian.Uint32(hdr[1:])
		progressed = true
		switch typ {
		case typeStdout, typeStderr:
			if n > maxOutFrame {
				s.logf("refused a %d-byte frame of type %d", n, typ)
				return 0, false, progressed
			}
			w := lenient{s.stdout}
			if typ == typeStderr {
				w = lenient{s.stderr}
			}
			if s.out+int(n) > outBudget {
				if _, err := io.CopyN(io.Discard, br, int64(n)); err != nil {
					return 0, false, progressed
				}
				continue
			}
			s.out += int(n)
			if _, err := io.CopyN(w, br, int64(n)); err != nil {
				return 0, false, progressed
			}
		case typeExit:
			if n != 4 {
				s.logf("refused an EXIT frame of %d bytes", n)
				return 0, false, progressed
			}
			var b [4]byte
			if _, err := io.ReadFull(br, b[:]); err != nil {
				return 0, false, progressed
			}
			return int(int32(binary.LittleEndian.Uint32(b[:]))), true, progressed
		case typeInfo:
			if n > maxInfoFrame {
				s.logf("refused a %d-byte INFO frame", n)
				return 0, false, progressed
			}
			if _, err := io.CopyN(io.Discard, br, int64(n)); err != nil {
				return 0, false, progressed
			}
			s.info++
		case typePing:
			if n != 0 {
				return 0, false, progressed
			}
		case typePaired:
			if n > maxPairedLen {
				s.logf("refused a %d-byte PAIRED frame", n)
				return 0, false, progressed
			}
			raw := make([]byte, n)
			if _, err := io.ReadFull(br, raw); err != nil {
				return 0, false, progressed
			}
			var p pairedMsg
			if json.Unmarshal(raw, &p) != nil || p.Instance == "" || p.Token == "" || !headerSafe(p.Instance) || !headerSafe(p.Token) {
				s.logf("refused a PAIRED frame it cannot read")
				return 0, false, progressed
			}
			s.instance, s.token = p.Instance, p.Token
			s.ob.release()
		default:
			s.logf("refused a frame of type %d", typ)
			return 0, false, progressed
		}
	}
}

// headerSafe is whether s may go into a request header as it is: visible ASCII only.
func headerSafe(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] <= ' ' || s[i] > '~' {
			return false
		}
	}
	return true
}

// runnerFile finds the identity's .runner beside the shim's bin/ directory. The listener starts the Worker by its path
// under bin/, so argv[0] names it even when the file is a link.
func runnerFile(argv0 string) (string, error) {
	p := argv0
	if !strings.Contains(p, "/") {
		exe, err := os.Executable()
		if err != nil {
			return "", err
		}
		p = exe
	}
	p, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(filepath.Dir(p)), ".runner"), nil
}

// readRunner reads the .runner file without its byte order mark, which the runner writes and the dispatcher's JSON parser rejects.
func readRunner(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return []byte(strings.TrimPrefix(string(raw), "\ufeff")), nil
}

func main() {
	if len(os.Args) != 4 || !strings.EqualFold(os.Args[1], "spawnclient") {
		fmt.Fprintln(os.Stderr, "superci-shim: usage: Runner.Worker spawnclient <inHandle> <outHandle>")
		os.Exit(1)
	}
	inFd, err1 := strconv.Atoi(os.Args[2])
	outFd, err2 := strconv.Atoi(os.Args[3])
	if err1 != nil || err2 != nil || inFd < 0 || outFd < 0 {
		fmt.Fprintln(os.Stderr, "superci-shim: the handles are not file descriptors")
		os.Exit(1)
	}
	in := os.NewFile(uintptr(inFd), "in")
	out := os.NewFile(uintptr(outFd), "out")
	defer out.Close()

	fail := func(err error) {
		fmt.Fprintln(os.Stderr, "superci-shim:", err)
		os.Exit(1)
	}
	cfg, err := loadConfig(envFile)
	if err != nil {
		fail(err)
	}
	path, err := runnerFile(os.Args[0])
	if err != nil {
		fail(err)
	}
	runner, err := readRunner(path)
	if err != nil {
		fail(err)
	}
	d, err := newDialer(cfg, runner, "http://169.254.169.254")
	if err != nil {
		fail(err)
	}
	s := &shim{dial: d.dial, ob: newOutbox(maxBuffered), stdout: os.Stdout, stderr: os.Stderr,
		pingEvery: pingEvery, silence: silenceLimit, window: resumeWindow, retry: retryEvery}
	os.Exit(s.run(context.Background(), in) & 0xff)
}
