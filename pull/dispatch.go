package main

import (
	"bufio"
	"context"
	"crypto/tls"
	_ "embed"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf16"
)

// agentScript is what a dispatch machine runs: it takes its one job on /work and runs the real Runner.Worker.
//
//go:embed agent.py
var agentScript []byte

// The identity plane's protocol (README, Identity plane). A machine of a dispatch set (POST /work) and a shim on an
// identity host (POST /shim) each prove who they are with the identity document EC2 signed, upgrade the connection to
// dispatchProtocol, and then speak frames: <u8 type><u32 little-endian length><payload>.
const dispatchProtocol = "superci-dispatch/1"

const (
	actionHeader = "X-Superci-Action" // on /notice: IMDS's spot/instance-action as the machine read it
	resumeHeader = "X-Superci-Resume" // <instance>.<token>: the pair a side rejoins after its connection broke
	runnerHeader = "X-Superci-Runner" // base64 of the identity's .runner, which names the set
	labelHeader  = "X-Superci-Label"  // the set, as the shim knows it: when given, it must be the identity's
)

type frameType byte

const (
	frameStdout frameType = 1 // machine to shim: the Worker's stdout
	frameStderr frameType = 2 // machine to shim: the Worker's stderr
	frameExit   frameType = 3 // machine to shim: the Worker's exit code (i32); it ends the job
	frameInfo   frameType = 4 // machine to shim: a JSON object of infoKeys
	framePipe   frameType = 5 // shim to machine: what the identity's listener wrote to the Worker's pipe
	frameJob    frameType = 6 // dispatcher to machine: the identity's .runner, the claim token and the job's names
	framePing   frameType = 7 // either way, every ping; a connection silent for silence ends
	framePaired frameType = 8 // dispatcher to shim: the machine and token the job is paired with
)

// frameMax is the largest payload a frame of each type carries. A larger one, or a type its sender may not send, ends
// the connection.
var frameMax = map[frameType]uint32{frameStdout: 1 << 20, frameStderr: 1 << 20, frameExit: 4, frameInfo: 4 << 10,
	framePipe: 1 << 20, frameJob: 64 << 10, framePing: 0, framePaired: 1 << 10}

var (
	fromMachine = []frameType{frameStdout, frameStderr, frameExit, frameInfo, framePing}
	fromShim    = []frameType{framePipe, framePing}
)

const (
	outBudget     = 4 << 20               // stdout and stderr one job relays; past it, frames are dropped and counted
	infoBudget    = 64                    // INFO frames one job relays
	jobMessageMax = 32 << 20              // the most of the listener's job message the dispatcher reads
	pipeHeld      = jobMessageMax + 1<<20 // PIPE bytes held while the job has no machine to write them to
	shimHeld      = 8 << 20               // what the machine sent that is held while the shim is away, frames at heldCost
	heldCost      = 64                    // a held frame's memory beyond its payload, so a flood of empty frames is bounded
	bootGrace     = 8 * time.Minute       // a launched machine that has not come to /work by then is stuck: warmCap, after a slow launch
	warmCap       = 300 * time.Second     // the uptime up to which a dispatch machine waits for its warm before it asks for a job (boot.sh)
	resumeWindow  = 3 * time.Minute       // how long a handed job waits for a side to rejoin after its connection broke
	drainFor      = 5 * time.Second       // how long an ended connection reads what its peer still sends
	hostTrust     = 5 * time.Minute       // how long an identity host's tags are trusted
	hostDistrust  = time.Minute           // how long a /shim caller that is no identity host is refused without asking EC2
	tagLookups    = 4                     // EC2 reads of /shim callers' tags at once; past them a caller is answered 503
	claimTries    = 3                     // asks of one machine's claim while S3's answer is unknown, with one token
	claimSpend    = 3                     // machines a job spends on unknown claims before each next one waits claimBackoff
	claimBackoff  = 30 * time.Second
)

// infoKeys are the keys an INFO frame may hold: the machine agent's times and its Worker's exit code.
var infoKeys = map[string]bool{"instance": true, "boot": true, "t_hdr": true, "t_spawn": true, "t_exit": true, "rc": true}

// readFrame reads one frame of a type in allowed, within its bound.
func readFrame(r io.Reader, allowed []frameType) (frameType, []byte, error) {
	var h [5]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return 0, nil, err
	}
	t, n := frameType(h[0]), binary.LittleEndian.Uint32(h[1:])
	if !slices.Contains(allowed, t) {
		return 0, nil, fmt.Errorf("frame of type %d", t)
	}
	if n > frameMax[t] || t == frameExit && n != 4 {
		return 0, nil, fmt.Errorf("frame of type %d holds %d bytes", t, n)
	}
	p := make([]byte, n)
	if _, err := io.ReadFull(r, p); err != nil {
		return 0, nil, err
	}
	return t, p, nil
}

func appendFrame(b []byte, t frameType, p []byte) []byte {
	b = append(b, byte(t))
	b = binary.LittleEndian.AppendUint32(b, uint32(len(p)))
	return append(b, p...)
}

// A wire is one upgraded connection: frames in and out, a PING every ping, and an end after silence.
type wire struct {
	conn     net.Conn
	in       *bufio.Reader
	silence  time.Duration
	mu       sync.Mutex // one frame written at a time
	dl       sync.Mutex // one change of the read deadline at a time
	draining atomic.Bool
	stopped  atomic.Bool // set when the job it carried was lost: no frame is written on it after
	done     chan struct{}
	once     sync.Once
	closed   func()
}

func newWire(conn net.Conn, in *bufio.Reader, ping, silence time.Duration, closed func()) *wire {
	w := &wire{conn: conn, in: in, silence: silence, done: make(chan struct{}), closed: closed}
	go func() {
		for tick := time.NewTicker(ping); ; {
			select {
			case <-w.done:
				tick.Stop()
				return
			case <-tick.C:
				if w.send(framePing, nil) != nil {
					return
				}
			}
		}
	}()
	return w
}

func (w *wire) send(t frameType, p []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.gone() || w.draining.Load() || w.stopped.Load() {
		return net.ErrClosed
	}
	_ = w.conn.SetWriteDeadline(time.Now().Add(w.silence))
	_, err := w.conn.Write(appendFrame(nil, t, p))
	if err != nil {
		w.close()
	}
	return err
}

// recv is the next frame other than a PING. On a connection being ended it reads and drops every frame, and returns
// only the error that ends it.
func (w *wire) recv(allowed []frameType) (frameType, []byte, error) {
	for {
		w.dl.Lock()
		if !w.draining.Load() {
			_ = w.conn.SetReadDeadline(time.Now().Add(w.silence))
		}
		w.dl.Unlock()
		t, p, err := readFrame(w.in, allowed)
		if err != nil {
			w.close()
			return 0, nil, err
		}
		if t != framePing && !w.draining.Load() {
			return t, p, nil
		}
	}
}

// end closes the connection after the frames sent on it: TLS close_notify and a FIN follow them, and the connection's
// reader goes on reading and dropping what the peer still sends until the peer closes or drainFor passes. A socket
// closed with bytes unread sends a reset, which discards what is still on its way to the peer, such as the EXIT just
// relayed to a shim that is PINGing.
func (w *wire) end() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.dl.Lock()
	defer w.dl.Unlock()
	if w.gone() || w.draining.Swap(true) {
		return
	}
	if tc, ok := w.conn.(*tls.Conn); ok {
		_ = tc.CloseWrite()
		if tcp, ok := tc.NetConn().(interface{ CloseWrite() error }); ok {
			_ = tcp.CloseWrite()
		}
	}
	_ = w.conn.SetReadDeadline(time.Now().Add(drainFor))
}

func (w *wire) close() {
	w.once.Do(func() {
		close(w.done)
		_ = w.conn.Close()
		if w.closed != nil {
			w.closed()
		}
	})
}

func (w *wire) gone() bool {
	select {
	case <-w.done:
		return true
	default:
		return false
	}
}

// jobHead is what the dispatcher reads of a job message: its id and names, never a secret.
type jobHead struct {
	ID          string
	Run         int64
	Job         string // the github context's job: its key in the workflow
	Name        string // its display name
	Repo        string // owner/name
	WorkflowRef string // owner/repo/.github/workflows/<file>@<ref>, which the gate selects a job's records by
	Event       string // the run's trigger
}

var jobIDForm = regexp.MustCompile(`^[0-9a-f-]{1,64}$`)

// parseJobRequest reads the listener's first message to the Worker from the PIPE payloads so far, size bytes in all:
// <i32 type 1, NewJobRequest><i32 length><UTF-16LE JSON>. done is false while more bytes are needed.
func parseJobRequest(payloads [][]byte, size int) (head jobHead, done bool, err error) {
	if size < 8 {
		return head, false, nil
	}
	first := leading(payloads, 8)
	typ, n := int32(binary.LittleEndian.Uint32(first)), int32(binary.LittleEndian.Uint32(first[4:]))
	switch {
	case typ != 1:
		return head, false, fmt.Errorf("the first message is of type %d, not a job request", typ)
	case n < 0 || n > jobMessageMax || n%2 != 0:
		return head, false, fmt.Errorf("a job request of %d bytes", n)
	case size < 8+int(n):
		return head, false, nil
	}
	raw := leading(payloads, 8+int(n))[8:]
	units := make([]uint16, n/2)
	for i := range units {
		units[i] = binary.LittleEndian.Uint16(raw[2*i:])
	}
	var msg struct {
		JobID          string                     `json:"jobId"`
		JobDisplayName string                     `json:"jobDisplayName"`
		ContextData    map[string]json.RawMessage `json:"contextData"`
	}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(string(utf16.Decode(units)), "\uFEFF")), &msg); err != nil {
		return head, false, fmt.Errorf("job request: %w", err)
	}
	id := strings.ToLower(msg.JobID)
	if !jobIDForm.MatchString(id) {
		return head, false, fmt.Errorf("job request with job id %q", msg.JobID)
	}
	gh := contextDict(msg.ContextData["github"])
	run, _ := strconv.ParseInt(gh["run_id"], 10, 64)
	name := msg.JobDisplayName
	if len(name) > 1<<10 {
		name = name[:1<<10]
	}
	return jobHead{ID: id, Run: run, Job: gh["job"], Name: name, Repo: gh["repository"], WorkflowRef: clip(gh["workflow_ref"], 1<<10),
		Event: clip(gh["event_name"], 64)}, true, nil
}

func clip(s string, n int) string { return s[:min(len(s), n)] }

// leading is the first n bytes of payloads, which hold at least n.
func leading(payloads [][]byte, n int) []byte {
	out := make([]byte, 0, n)
	for _, p := range payloads {
		out = append(out, p[:min(len(p), n-len(out))]...)
		if len(out) == n {
			break
		}
	}
	return out
}

// contextDict reads a dictionary of a job's context data, as the service sends one ({"t":2,"d":[{"k","v"}]}) or as a
// plain object, keeping its string and number values.
func contextDict(raw json.RawMessage) map[string]string {
	out := map[string]string{}
	var dict struct {
		D []struct {
			K string          `json:"k"`
			V json.RawMessage `json:"v"`
		} `json:"d"`
	}
	if json.Unmarshal(raw, &dict) == nil && len(dict.D) > 0 {
		for _, e := range dict.D {
			out[e.K] = contextString(e.V)
		}
		return out
	}
	var plain map[string]json.RawMessage
	if json.Unmarshal(raw, &plain) == nil {
		for k, v := range plain {
			out[k] = contextString(v)
		}
	}
	return out
}

func contextString(v json.RawMessage) string {
	var s string
	if json.Unmarshal(v, &s) == nil {
		return s
	}
	var n struct {
		N *float64 `json:"n"`
	}
	if json.Unmarshal(v, &n) == nil && n.N != nil {
		return strconv.FormatFloat(*n.N, 'f', -1, 64)
	}
	return ""
}

// identityName is an identity's agent name, superci-id-<size>-<n>, which serves the set superci-<size>.
var identityName = regexp.MustCompile(`^superci-id-([0-9]+cpu)-[0-9]+$`)

// credentialKeys are the keys of an identity's .credentials and .credentials_rsaparams. A .runner holding one is not
// a .runner, and it never leaves for a machine.
var credentialKeys = []string{"scheme", "data", "clientid", "authorizationurl", "requirefipscryptography", "d", "dp", "dq",
	"exponent", "inverseq", "modulus", "p", "q"}

// runnerIdentity reads the .runner a shim sent: its text, its agent name and the set that name serves.
func runnerIdentity(header string) (runner, agent, set string, err error) {
	raw, err := base64.StdEncoding.DecodeString(header)
	if err != nil || len(raw) == 0 || len(raw) > 8<<10 {
		return "", "", "", errors.New("no .runner of at most 8 KiB in base64")
	}
	text := strings.TrimPrefix(string(raw), "\uFEFF")
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(text), &fields); err != nil {
		return "", "", "", fmt.Errorf(".runner: %w", err)
	}
	for k, v := range fields {
		if slices.Contains(credentialKeys, strings.ToLower(k)) {
			return "", "", "", fmt.Errorf(".runner holds the credential field %q", k)
		}
		if strings.EqualFold(k, "agentName") {
			_ = json.Unmarshal(v, &agent)
		}
	}
	m := identityName.FindStringSubmatch(agent)
	if m == nil {
		return "", "", "", fmt.Errorf("agent %q is not an identity (superci-id-<size>-<n>)", agent)
	}
	return text, agent, "superci-" + m[1], nil
}

// jobFence is the claim of a handed job: issued/job-<job id>, holding a jobClaim.
func jobFence(job string) string { return "job-" + job }

// overMark names the mark beside a job's claim that the job is over: a shim that rejoins after the job ended, in this
// process or after a restart, is refused rather than adopted into a session whose machine is gone.
const overMark = ".over"

// A jobClaim is what a handed job's claim holds: the pair it was handed to, <instance>.<token>, and on a second line
// what a dispatcher that adopts the job after a restart needs to have it re-run if its machine is lost. A machine's
// claim holds the same, without the handed time, so the job's names outlive a process that died before the job's
// claim. A stranded job's claim has no pair: Lost names the pair the job was PAIRED with and never handed to.
type jobClaim struct {
	pair    string
	Lost    string `json:"lost,omitempty"`
	Run     int64  `json:"run,omitempty"`
	Name    string `json:"name,omitempty"`
	Repo    string `json:"repo,omitempty"`
	Agent   string `json:"agent,omitempty"`
	Ref     string `json:"ref,omitempty"`
	Event   string `json:"event,omitempty"`
	Arrived int64  `json:"arrived,omitempty"` // unix ms: when the identity's shim brought the job
	Handed  int64  `json:"handed,omitempty"`  // unix ms: the handed point, when this claim was taken
}

func (c jobClaim) String() string {
	b, _ := json.Marshal(c)
	return c.pair + "\n" + string(b)
}

func parseJobClaim(holder string) jobClaim {
	pair, ref, _ := strings.Cut(holder, "\n")
	var c jobClaim
	_ = json.Unmarshal([]byte(ref), &c)
	c.pair = pair
	return c
}

// tagReader reads an instance's tags from EC2 (Fleet.Tags).
type tagReader interface {
	Tags(ctx context.Context, region, instance string) (map[string]string, error)
}

// claimStore is the claims bucket (Claims).
type claimStore interface {
	Take(ctx context.Context, name, token string) error
	Holder(ctx context.Context, name string) (string, error)
}

var errNotIdentity = errors.New("not an identity host of this deployment")

// Dispatcher hands each job an identity's listener acquired to one fresh machine of the identity's set, and relays
// between them until the Worker's exit code reaches the shim (README, Identity plane).
type Dispatcher struct {
	deployment string
	sets       map[string]*DispatchSet
	verifier   interface {
		Verify(body []byte) (InstanceDocument, error)
	}
	claims  claimStore
	tags    tagReader
	refresh func(ctx context.Context, region string)
	ping    time.Duration
	silence time.Duration
	window  time.Duration // how long a job's claim is asked with its pair before the job is stranded: the shim's resume window

	life context.Context // ends with Close: a stranding waits on S3 no longer
	stop context.CancelFunc

	handing  sync.WaitGroup // hand-offs in flight
	readers  sync.WaitGroup // connections' readers
	stopping atomic.Bool    // no new hand-off starts

	lookups chan struct{} // EC2 reads of /shim callers' tags in flight

	mu    sync.Mutex
	hosts map[string]hostCheck // /shim callers whose tags were read
	wires map[*wire]bool       // every open connection
}

// hostCheck is what an instance's tags said, identity host or not, and until when that reading holds.
type hostCheck struct {
	ok    bool
	until time.Time
}

func NewDispatcher(deployment string, sets []*DispatchSet, verifier interface {
	Verify(body []byte) (InstanceDocument, error)
}, claims claimStore, tags tagReader, refresh func(context.Context, string)) *Dispatcher {
	d := &Dispatcher{deployment: deployment, sets: map[string]*DispatchSet{}, verifier: verifier, claims: claims, tags: tags,
		refresh: refresh, ping: 20 * time.Second, silence: 60 * time.Second, window: resumeWindow, lookups: make(chan struct{}, tagLookups),
		hosts: map[string]hostCheck{}, wires: map[*wire]bool{}}
	d.life, d.stop = context.WithCancel(context.Background())
	for _, s := range sets {
		d.sets[s.spec.Name] = s
		s.over = func(job string) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := claims.Take(ctx, jobFence(job)+overMark, "over"); err != nil && !errors.Is(err, ErrClaimed) {
				slog.Warn("dispatch over mark", "job", job, "err", err.Error())
			}
		}
	}
	return d
}

func (d *Dispatcher) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 16<<10))
	if err != nil {
		http.Error(w, "POST the instance's signed identity document", http.StatusBadRequest)
		return
	}
	doc, err := d.verifier.Verify(body)
	if err != nil {
		slog.Warn("dispatch refused", "path", r.URL.Path, "remote", r.RemoteAddr, "err", err.Error())
		http.Error(w, "unknown instance", http.StatusForbidden)
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 15*time.Second)
	defer cancel()
	switch r.URL.Path {
	case "/work":
		d.work(ctx, w, r, doc)
	case "/shim":
		d.shim(ctx, w, r, doc)
	case "/agent":
		if _, m := d.machineOf(ctx, doc); m == nil {
			http.Error(w, "unknown machine", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "text/x-python")
		_, _ = w.Write(agentScript)
	default:
		http.NotFound(w, r)
	}
}

// The time an instance-action names is when AWS takes the machine, two minutes after its notice. A notice whose time
// is past by more than noticeLate, or ahead by more than noticeAhead, is no notice AWS gives now: a stale one replayed,
// or none at all.
const (
	noticeLate  = time.Minute
	noticeAhead = 3 * time.Minute
)

// Notice takes in a dispatch machine's word, from the request's verified identity document, that AWS takes it back:
// action is IMDS's spot/instance-action, {"action": "terminate", "time": "2026-11-27T19:08:14Z"}. It says whether the
// instance is a machine of a dispatch set, and refuses a notice that is not current or comes from an on-demand
// machine, which AWS does not reclaim. The reclaim acts on the
// document's instance alone (DispatchSet.reclaim).
func (d *Dispatcher) Notice(doc InstanceDocument, action string, now time.Time) (known bool, err error) {
	for _, st := range d.sets {
		known, spot := st.spotMachine(doc.InstanceID)
		if !known {
			continue
		}
		if !spot {
			return true, fmt.Errorf("%s is no spot machine: AWS reclaims none", doc.InstanceID)
		}
		if err := currentNotice(action, now); err != nil {
			return true, err
		}
		st.reclaim(doc.InstanceID, "spot notice")
		return true, nil
	}
	return false, nil
}

// currentNotice checks an instance-action: an action AWS takes on a spot machine, at a time it gives notice of now.
func currentNotice(raw string, now time.Time) error {
	var a struct {
		Action string    `json:"action"`
		Time   time.Time `json:"time"`
	}
	if err := json.Unmarshal([]byte(raw), &a); err != nil {
		return fmt.Errorf("instance-action %q: %w", clip(raw, 256), err)
	}
	switch {
	case a.Action != "terminate" && a.Action != "stop" && a.Action != "hibernate":
		return fmt.Errorf("instance-action %q", clip(a.Action, 64))
	case a.Time.Before(now.Add(-noticeLate)), a.Time.After(now.Add(noticeAhead)):
		return fmt.Errorf("instance-action at %s, not current at %s", a.Time.UTC().Format(time.RFC3339), now.UTC().Format(time.RFC3339))
	}
	return nil
}

// machineOf is the dispatch set and machine an identity document names, listing EC2 again if the instance is newer
// than the last listing.
func (d *Dispatcher) machineOf(ctx context.Context, doc InstanceDocument) (*DispatchSet, *dmachine) {
	for try := 0; try < 3; try++ {
		for _, s := range d.sets {
			if m := s.machine(doc.InstanceID); m != nil {
				return s, m
			}
		}
		d.refresh(ctx, doc.Region)
	}
	return nil, nil
}

// work serves a machine. A machine never paired waits for a job; a machine that holds a claim is answered 410 unless
// it rejoins the job it was handed.
func (d *Dispatcher) work(ctx context.Context, w http.ResponseWriter, r *http.Request, doc InstanceDocument) {
	st, m := d.machineOf(ctx, doc)
	if m == nil {
		http.Error(w, "unknown machine", http.StatusForbidden)
		return
	}
	if !upgradable(r) {
		http.Error(w, "upgrade to "+dispatchProtocol+" over HTTP/1.1", http.StatusUpgradeRequired)
		return
	}
	if resume := r.Header.Get(resumeHeader); resume != "" {
		inst, token, _ := strings.Cut(resume, ".")
		if inst != doc.InstanceID || !st.mayResume(m) {
			w.WriteHeader(http.StatusGone)
			return
		}
		s, code, _ := d.resume(ctx, st, inst, token, false)
		if code != 0 {
			w.WriteHeader(code)
			return
		}
		mw, err := d.upgrade(w)
		if err != nil {
			return
		}
		if !st.rejoined(m, mw, s) {
			mw.close()
			return
		}
		s.attachMachine(mw)
		slog.Info("dispatch resumed", "set", st.spec.Name, "side", "machine", "machine", inst, "job", s.id)
		d.spawn(func() { d.machineReader(st, m, mw) })
		return
	}
	if !st.mayWait(m) {
		w.WriteHeader(http.StatusGone)
		return
	}
	switch holder, err := d.claims.Holder(ctx, m.ID); {
	case err != nil:
		slog.Error("dispatch claim read", "set", st.spec.Name, "machine", m.ID, "err", err.Error())
		w.Header().Set("Retry-After", "3")
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	case holder != "":
		w.WriteHeader(http.StatusGone)
		return
	}
	mw, err := d.upgrade(w)
	if err != nil {
		return
	}
	if !st.waiting(m, mw, time.Now()) {
		mw.close()
		return
	}
	d.spawn(func() { d.machineReader(st, m, mw) })
	d.match(st)
}

// shim serves an identity host's shim: a new job, or a job rejoining the machine it was handed to.
func (d *Dispatcher) shim(ctx context.Context, w http.ResponseWriter, r *http.Request, doc InstanceDocument) {
	if err := d.identityHost(ctx, doc); err != nil {
		slog.Warn("shim refused", "instance", doc.InstanceID, "err", err.Error())
		if errors.Is(err, errNotIdentity) {
			http.Error(w, "not an identity host", http.StatusForbidden)
		} else {
			w.Header().Set("Retry-After", "3")
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		return
	}
	runner, agent, set, err := runnerIdentity(r.Header.Get(runnerHeader))
	st := d.sets[set]
	if err == nil && st == nil {
		err = fmt.Errorf("no dispatch set %s", set)
	}
	if err != nil {
		slog.Warn("shim refused", "instance", doc.InstanceID, "err", err.Error())
		http.Error(w, "not an identity of a dispatch set", http.StatusForbidden)
		return
	}
	if l := r.Header.Get(labelHeader); l != "" && l != set {
		http.Error(w, fmt.Sprintf("identity %s serves %s, not %s", agent, set, l), http.StatusBadRequest)
		return
	}
	if !upgradable(r) {
		http.Error(w, "upgrade to "+dispatchProtocol+" over HTTP/1.1", http.StatusUpgradeRequired)
		return
	}
	if resume := r.Header.Get(resumeHeader); resume != "" {
		inst, token, _ := strings.Cut(resume, ".")
		s, code, elsewhere := d.resume(ctx, st, inst, token, true)
		if code != 0 && elsewhere == nil {
			w.WriteHeader(code)
			return
		}
		sw, err := d.upgrade(w)
		if err != nil {
			return
		}
		if elsewhere != nil {
			_ = sw.send(framePaired, elsewhere)
			sw.end()
			d.spawn(func() { _, _, _ = sw.recv(fromShim) })
			return
		}
		if s.attachShim(sw) {
			st.finish(s, exitRelayed)
		}
		st.count(func(c *DispatchCounts) { c.Resumes++ })
		slog.Info("dispatch resumed", "set", st.spec.Name, "side", "shim", "machine", inst, "job", s.id)
		d.spawn(func() { d.shimReader(st, s, sw) })
		return
	}
	sw, err := d.upgrade(w)
	if err != nil {
		return
	}
	s := &session{set: st, agent: agent, runner: runner, shim: sw, arrived: time.Now()}
	d.spawn(func() { d.shimReader(st, s, sw) })
}

// spawn runs a connection's reader, which Close waits for. Once Close has begun no reader starts: its connection is
// among those Close ends.
func (d *Dispatcher) spawn(read func()) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopping.Load() {
		return
	}
	d.readers.Add(1)
	go func() {
		defer d.readers.Done()
		read()
	}()
}

// identityHost checks that an instance is an identity host of this deployment, by its EC2 tags. Any instance of the
// account can call /shim, job code included, and each EC2 read spends the account's Describe budget: so a host that
// passed is not asked about for hostTrust, one that failed for hostDistrust, and at most tagLookups reads run at once.
func (d *Dispatcher) identityHost(ctx context.Context, doc InstanceDocument) error {
	now := time.Now()
	d.mu.Lock()
	h, known := d.hosts[doc.InstanceID]
	d.mu.Unlock()
	switch {
	case known && now.Before(h.until) && h.ok:
		return nil
	case known && now.Before(h.until):
		return errNotIdentity
	}
	select {
	case d.lookups <- struct{}{}:
		defer func() { <-d.lookups }()
	default:
		return fmt.Errorf("tags of %s: %d reads of tags already in flight", doc.InstanceID, tagLookups)
	}
	tags, err := d.tags.Tags(ctx, doc.Region, doc.InstanceID)
	if err != nil {
		return fmt.Errorf("tags of %s: %w", doc.InstanceID, err)
	}
	ok := tags[tagRole] == "identity" && tags[tagDeployment] == d.deployment
	d.mu.Lock()
	if len(d.hosts) > 4096 {
		for id, c := range d.hosts {
			if now.After(c.until) {
				delete(d.hosts, id)
			}
		}
	}
	if ok {
		d.hosts[doc.InstanceID] = hostCheck{ok: true, until: now.Add(hostTrust)}
	} else {
		d.hosts[doc.InstanceID] = hostCheck{until: now.Add(hostDistrust)}
	}
	d.mu.Unlock()
	if !ok {
		return errNotIdentity
	}
	return nil
}

// resumeRedirect is resume's answer to a shim whose job was handed to another pair: that pair's PAIRED.
const resumeRedirect = -1

// resume finds the job a side rejoins after its connection broke. In this process its session says. After a restart
// the job's claim does: it must name this pair, <instance>.<token>, and it was taken only once the machine's claim held
// that token, so it proves the token too. A job whose claim names another pair sends the shim there. A job never handed
// is gone (410): a shim drops its job's bytes once PAIRED comes, so it cannot start the job again. A job with no claim
// at all was PAIRED by a process that ended before its claim, and is stranded for its re-run (strand).
func (d *Dispatcher) resume(ctx context.Context, st *DispatchSet, inst, token string, shim bool) (*session, int, []byte) {
	_, job, ok := strings.Cut(token, ".")
	if !ok || !jobIDForm.MatchString(job) {
		return nil, http.StatusGone, nil
	}
	for {
		s, held, settled := st.pairOf(inst)
		if s == nil {
			break
		}
		if held != token {
			return nil, http.StatusGone, nil
		}
		if settled == nil {
			return s, 0, nil
		}
		select {
		case <-settled:
		case <-ctx.Done():
			return nil, http.StatusServiceUnavailable, nil
		}
	}
	key := inst + "." + token
	claim, err := d.jobClaimOf(ctx, job)
	if err == nil && claim == (jobClaim{}) {
		claim, err = d.strandPaired(ctx, st, key, job)
	}
	fence := claim.pair
	switch {
	case err != nil:
		return nil, http.StatusServiceUnavailable, nil
	case fence == key:
		if over, err := d.claims.Holder(ctx, jobFence(job)+overMark); err != nil {
			return nil, http.StatusServiceUnavailable, nil
		} else if over != "" {
			return nil, http.StatusGone, nil
		}
		if s := st.adopt(inst, token, job, claim, time.Now()); s != nil {
			return s, 0, nil
		}
		return nil, http.StatusGone, nil
	case fence == "" || !shim:
		return nil, http.StatusGone, nil
	}
	return nil, resumeRedirect, pairedOf(fence)
}

// jobClaimOf is the claim a job's claim holds; none, the zero jobClaim.
func (d *Dispatcher) jobClaimOf(ctx context.Context, job string) (jobClaim, error) {
	holder, err := d.claims.Holder(ctx, jobFence(job))
	return parseJobClaim(holder), err
}

// strandPaired strands a job with no claim that a side rejoins as the pair key. The machine's claim was taken before
// PAIRED, with the job's names: it must name key, which proves the token, and gives the job's re-run its names. A
// machine's claim that names another pair, or an earlier build's that holds only a token, strands nothing.
func (d *Dispatcher) strandPaired(ctx context.Context, st *DispatchSet, key, job string) (jobClaim, error) {
	inst, _, _ := strings.Cut(key, ".")
	holder, err := d.claims.Holder(ctx, inst)
	if err != nil {
		return jobClaim{}, err
	}
	c := parseJobClaim(holder)
	if c.pair != key {
		return jobClaim{}, nil
	}
	return d.strand(ctx, st, c, job)
}

// strand settles a job PAIRED with c's pair whose claim S3 has not shown taken. It takes the job's claim with no pair,
// after which no pair can be handed the job, and reports the job for its re-run: no machine got its bytes. Every
// strand of one pair writes the same claim, so a strand whose answer was lost, or one from each side, finds it its
// own. It returns the claim S3 holds: that one, or the pair's own when its claim landed first, which handed the job.
func (d *Dispatcher) strand(ctx context.Context, st *DispatchSet, c jobClaim, job string) (jobClaim, error) {
	lost := c
	lost.pair, lost.Lost, lost.Handed = "", c.pair, 0
	switch err := d.take(ctx, jobFence(job), lost.String(), 0); {
	case errors.Is(err, ErrClaimed):
		return d.jobClaimOf(ctx, job)
	case err != nil:
		return jobClaim{}, err
	}
	inst, _, _ := strings.Cut(c.pair, ".")
	st.strand(job, lost, inst)
	return lost, nil
}

// pairedOf is the PAIRED frame of a pair, <instance>.<token>.
func pairedOf(key string) []byte {
	inst, token, _ := strings.Cut(key, ".")
	b, _ := json.Marshal(map[string]string{"instance": inst, "token": token})
	return b
}

func upgradable(r *http.Request) bool {
	if r.ProtoMajor != 1 || r.ProtoMinor < 1 || r.Header.Get("Upgrade") != dispatchProtocol {
		return false
	}
	for _, v := range r.Header.Values("Connection") {
		for _, tok := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(tok), "upgrade") {
				return true
			}
		}
	}
	return false
}

// upgrade answers 101 and takes the connection over.
func (d *Dispatcher) upgrade(w http.ResponseWriter) (*wire, error) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "this connection cannot be upgraded", http.StatusInternalServerError)
		return nil, errors.New("not hijackable")
	}
	conn, rw, err := hj.Hijack()
	if err != nil {
		return nil, err
	}
	if _, err := conn.Write([]byte("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: " + dispatchProtocol + "\r\n\r\n")); err != nil {
		_ = conn.Close()
		return nil, err
	}
	var wr *wire
	wr = newWire(conn, rw.Reader, d.ping, d.silence, func() {
		d.mu.Lock()
		delete(d.wires, wr)
		d.mu.Unlock()
	})
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopping.Load() {
		go wr.close()
		return nil, errors.New("the dispatcher is stopping")
	}
	d.wires[wr] = true
	return wr, nil
}

// Close ends every connection, as a stop of the process does, and each side rejoins the next dispatcher. It first
// starts no hand-off and lets those in flight reach their handed point (or not) for up to 5 s: a stop between a
// PAIRED and the job's claim would lose the job, since the shim has dropped its bytes.
func (d *Dispatcher) Close() {
	d.mu.Lock()
	d.stopping.Store(true)
	d.mu.Unlock()
	drained := make(chan struct{})
	go func() { d.handing.Wait(); close(drained) }()
	select {
	case <-drained:
	case <-time.After(5 * time.Second):
	}
	d.stop()
	d.mu.Lock()
	ws := make([]*wire, 0, len(d.wires))
	for w := range d.wires {
		ws = append(ws, w)
	}
	d.mu.Unlock()
	for _, w := range ws {
		w.close()
	}
	read := make(chan struct{})
	go func() { d.readers.Wait(); close(read) }()
	select {
	case <-read:
	case <-time.After(5 * time.Second):
	}
}

// machineReader relays a machine's frames to the job it was paired with. A machine waiting for its job sends only
// PINGs.
func (d *Dispatcher) machineReader(st *DispatchSet, m *dmachine, mw *wire) {
	for {
		t, p, err := mw.recv(fromMachine)
		if err != nil {
			break
		}
		s := st.sessionOf(m, mw)
		if s == nil {
			slog.Warn("dispatch frame from a machine with no job", "set", st.spec.Name, "machine", m.ID, "type", t)
			mw.close()
			break
		}
		if s.fromMachine(mw, t, p) {
			st.finish(s, exitRelayed)
		} else if t == frameExit {
			st.exitHeld(s)
		}
	}
	st.machineClosed(m, mw)
}

// shimReader holds a shim's PIPE bytes for its machine, and queues the job once its message has been read.
func (d *Dispatcher) shimReader(st *DispatchSet, s *session, sw *wire) {
	queued := s.id != ""
	for {
		_, p, err := sw.recv(fromShim)
		if err != nil {
			break
		}
		if err := s.pipe(p); err != nil {
			slog.Warn("shim refused", "set", st.spec.Name, "job", s.id, "err", err.Error())
			sw.close()
			break
		}
		if queued {
			continue
		}
		head, done, err := s.parse()
		if err != nil {
			slog.Warn("shim refused", "set", st.spec.Name, "agent", s.agent, "err", err.Error())
			sw.close()
			break
		}
		if !done {
			continue
		}
		queued = true
		key, wait := st.enqueue(s, head, time.Now())
		for ; wait != nil; key, wait = st.enqueue(s, head, time.Now()) {
			<-wait
		}
		if key != "" {
			slog.Warn("dispatch job already handed", "set", st.spec.Name, "job", head.ID, "pair", key)
			_ = sw.send(framePaired, pairedOf(key))
			sw.end()
			continue
		}
		d.match(st)
	}
	st.shimClosed(s, sw)
}

// match pairs the set's waiting jobs with its waiting machines, oldest first, each pair on its own.
func (d *Dispatcher) match(st *DispatchSet) {
	for !d.stopping.Load() {
		s, m, mw := st.next(time.Now())
		if s == nil {
			return
		}
		d.handing.Add(1)
		go func() {
			defer d.handing.Done()
			d.hand(st, s, m, mw)
		}()
	}
}

// hand gives a job to a machine. The machine's claim is taken first, so it never takes a second job; then JOB goes to
// the machine and PAIRED to the shim. The job's own claim is the handed point: once it names this pair, no other pair
// can take it, and the first PIPE byte follows. Before it any failure spends the machine and puts the job back at the
// head of its queue; after it the job is never sent anywhere else.
func (d *Dispatcher) hand(st *DispatchSet, s *session, m *dmachine, mw *wire) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	token := newToken() + "." + s.id
	claim := jobClaim{pair: m.ID + "." + token, Run: s.head.Run, Name: s.head.Name, Repo: s.head.Repo, Agent: s.agent, Ref: s.head.WorkflowRef, Event: s.head.Event,
		Arrived: s.arrived.UnixMilli()}
	if err := d.take(ctx, m.ID, claim.String(), claimTries); err != nil {
		slog.Warn("dispatch claim", "set", st.spec.Name, "machine", m.ID, "job", s.id, "elsewhere", errors.Is(err, ErrClaimed), "err", err.Error())
		d.retry(st, s, m, !errors.Is(err, ErrClaimed))
		return
	}
	st.claimed(s, m, mw, token)
	if mw.send(frameJob, s.jobFrame(token)) != nil {
		d.retry(st, s, m, false)
		return
	}
	// A machine AWS is taking back is spent while the job can still be paired again: until PAIRED, the shim holds its
	// bytes. A reclaim that comes after this check loses the job at its handed point.
	if st.isReclaimed(m) {
		d.retry(st, s, m, false)
		return
	}
	sw := s.shimWire()
	if sw == nil || sw.send(framePaired, pairedOf(m.ID+"."+token)) != nil {
		st.abandon(s, m)
		return
	}
	claim.Handed = time.Now().UnixMilli()
	fctx, fcancel := context.WithTimeout(context.Background(), d.window)
	defer fcancel()
	// The job's claim is asked again while S3's answer is unknown, for as long as a shim would wait to rejoin: a claim
	// that may have landed names this pair, nothing else may be sent until that is known, and the shim has PAIRED, so
	// it cannot start the job again. Past that the shim may have given up on the job, so it is stranded instead, for as
	// long as S3 takes to answer; the first of the two claims to land decides.
	held, err := claim, d.take(fctx, jobFence(s.id), claim.String(), 0)
	switch {
	case errors.Is(err, ErrClaimed):
		held, err = d.jobClaimOf(fctx, s.id)
	case err != nil:
		held, err = d.strand(d.life, st, claim, s.id)
	}
	if err != nil {
		slog.Error("dispatch job claim", "set", st.spec.Name, "job", s.id, "machine", m.ID, "err", err.Error())
		st.abandon(s, m)
		return
	}
	if held.pair != claim.pair {
		slog.Warn("dispatch job not handed to its pair", "set", st.spec.Name, "job", s.id, "pair", held.pair, "stranded", held.Lost)
		if held.pair != "" {
			_ = sw.send(framePaired, pairedOf(held.pair))
		}
		st.abandon(s, m)
		return
	}
	if st.handed(s, time.Now()) {
		st.loseReclaimed(s)
	} else {
		s.attachMachine(mw)
	}
	if s.goLive() {
		st.finish(s, exitRelayed)
	}
	slog.Info("dispatch handed", "set", st.spec.Name, "job", s.id, "machine", m.ID, "agent", s.agent, "run", s.head.Run, "name", s.head.Name)
}

// take takes a claim for token, asking again while S3's answer is unknown: tries times in all, or with tries 0 until
// ctx ends. Take is idempotent per token, so a claim that landed with its answer lost is found to be this token's.
func (d *Dispatcher) take(ctx context.Context, name, token string, tries int) error {
	for try, wait := 1, 100*time.Millisecond; ; try, wait = try+1, min(2*wait, 2*time.Second) {
		err := d.claims.Take(ctx, name, token)
		if err == nil || errors.Is(err, ErrClaimed) || ctx.Err() != nil || try == tries {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(wait):
		}
	}
}

// retry settles a pairing that failed before the handed point and pairs again. A job that has spent claimSpend
// machines on claims S3 never answered waits claimBackoff before each next one, so a lasting S3 fault does not churn
// through the set's machines.
func (d *Dispatcher) retry(st *DispatchSet, s *session, m *dmachine, unknown bool) {
	if wait := st.retry(s, m, unknown, time.Now()); wait > 0 {
		time.AfterFunc(wait, func() { d.match(st) })
	}
	d.match(st)
}

// A session is one job, from its shim's arrival to the Worker's exit code reaching the shim: waiting for a machine,
// being paired, then relayed between its shim and its one machine across breaks of either connection.
type session struct {
	set     *DispatchSet
	agent   string // the identity's agent name
	runner  string // its .runner
	arrived time.Time

	// Set before the job is queued, or when a resumed job is adopted, and fixed after.
	id   string
	head jobHead

	// The set's lock guards these.
	phase   sessionPhase
	machine string
	token   string
	settled chan struct{} // closed when its pairing ends, handed or not
	handed  time.Time
	unknown int       // machines spent on claims whose answer was unknown
	after   time.Time // not paired before then

	wm sync.Mutex // one writer of PIPE frames to the machine at a time, in order
	ws sync.Mutex // one writer of frames to the shim at a time, in order
	mu sync.Mutex
	// The session's own lock guards these.
	from      *wire // the machine connection whose frames are this job's
	mach      *wire // the machine connection PIPE bytes go to, once handed and while it holds
	shim      *wire // the shim's connection, while it holds
	live      bool  // handed: frames for the shim go out rather than wait
	toMach    [][]byte
	held      int
	toShim    []shimFrame
	shimCost  int // what toShim holds, at heldCost a frame beyond its payload
	out       int
	infos     int
	exited    bool
	code      int32        // the Worker's exit code, once exited
	exitAt    time.Time    // when it came
	adopted   bool         // a session a previous process handed, rejoined after a restart
	recHanded bool         // its claim kept the handed time
	away      [2]time.Time // since when the machine (0) and the shim (1) have been away, once handed
	shimDone  bool
}

type sessionPhase int

const (
	reading sessionPhase = iota // its message is being read
	queued                      // waiting for a machine
	pairing                     // a machine is being claimed for it
	running                     // handed to its machine
	over
)

type shimFrame struct {
	t frameType
	p []byte
}

func (s *session) key() string { return s.machine + "." + s.token }

func (s *session) shimWire() *wire {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.shim
}

// jobFrame is the JOB frame of the session for the machine its claim token is for.
func (s *session) jobFrame(token string) []byte {
	b, _ := json.Marshal(struct {
		Runner string `json:"runner"`
		Token  string `json:"token"`
		Job    struct {
			Run  int64  `json:"run"`
			Job  string `json:"job"`
			Name string `json:"name"`
			Repo string `json:"repo"`
		} `json:"job"`
	}{Runner: s.runner, Token: token, Job: struct {
		Run  int64  `json:"run"`
		Job  string `json:"job"`
		Name string `json:"name"`
		Repo string `json:"repo"`
	}{s.head.Run, s.head.Job, s.head.Name, s.head.Repo}})
	return b
}

// parse reads the job message from the PIPE bytes held so far.
func (s *session) parse() (jobHead, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return parseJobRequest(s.toMach, s.held)
}

// pipe takes PIPE bytes from the shim: written to the machine while it holds, held for it otherwise.
func (s *session) pipe(p []byte) error {
	s.wm.Lock()
	defer s.wm.Unlock()
	s.mu.Lock()
	m := s.mach
	if m == nil {
		if s.held+len(p) > pipeHeld {
			s.mu.Unlock()
			return fmt.Errorf("over %d PIPE bytes with no machine to take them", pipeHeld)
		}
		s.toMach = append(s.toMach, p)
		s.held += len(p)
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()
	if m.send(framePipe, p) != nil {
		s.mu.Lock()
		s.toMach = append(s.toMach, p)
		s.held += len(p)
		s.mu.Unlock()
		s.machineAway(m)
	}
	return nil
}

// attachMachine makes mw, the job's machine connection, the one PIPE bytes go to, after the bytes held for it. A frame
// whose send failed is held with the rest for the machine's next connection: a failed send put no whole frame on the
// wire, and the agent acts only on whole frames, so it is never written twice. A connection the machine has since
// replaced by rejoining is left alone.
func (s *session) attachMachine(mw *wire) {
	s.wm.Lock()
	defer s.wm.Unlock()
	s.mu.Lock()
	if s.from != mw {
		s.mu.Unlock()
		return
	}
	held := s.toMach
	s.toMach, s.held = nil, 0
	s.mu.Unlock()
	for i, p := range held {
		if mw.send(framePipe, p) != nil {
			s.mu.Lock()
			s.toMach = append(held[i:], s.toMach...)
			for _, q := range s.toMach {
				s.held += len(q)
			}
			if s.away[0].IsZero() {
				s.away[0] = time.Now()
			}
			s.mu.Unlock()
			return
		}
	}
	s.mu.Lock()
	// A connection that ended while its bytes went is away, not attached: machineAway may have run before mach
	// named it, and nothing else would mark it.
	if mw.gone() {
		if s.away[0].IsZero() {
			s.away[0] = time.Now()
		}
		s.mu.Unlock()
		return
	}
	old := s.mach
	s.mach, s.away[0] = mw, time.Time{}
	s.mu.Unlock()
	if old != nil && old != mw {
		old.close()
	}
}

// detach takes the job from its machine, and waits on no write: a write to a machine that does not read holds s.wm
// for up to the silence, and the caller (a notice, the listing of EC2) is not to wait on it. Its connections are
// stopped, so no frame starts on them after this; one already being written may finish. None of the machine's frames
// is the job's after this.
func (s *session) detach() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, w := range []*wire{s.mach, s.from} {
		if w != nil {
			w.stopped.Store(true)
		}
	}
	s.mach, s.from = nil, nil
}

// machineAway marks the job's machine away once the connection its frames come on ends. That holds from its claim,
// not only once the job is live: a machine that leaves between the first PIPE byte and goLive is away like any other.
func (s *session) machineAway(mw *wire) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mach == mw {
		s.mach = nil
	}
	if s.from == mw && s.away[0].IsZero() {
		s.away[0] = time.Now()
	}
}

// fromMachine takes one frame of the machine's, within the job's budgets, and says whether it was the exit code and
// reached the shim.
func (s *session) fromMachine(mw *wire, t frameType, p []byte) bool {
	s.mu.Lock()
	if s.from != mw {
		s.mu.Unlock()
		return false
	}
	keep := true
	switch t {
	case frameStdout, frameStderr:
		if keep = s.out+len(p) <= outBudget; keep {
			s.out += len(p)
		}
	case frameInfo:
		if keep = s.infos < infoBudget && knownInfo(p); keep {
			s.infos++
		}
	case frameExit:
		if s.exited {
			s.mu.Unlock()
			return false
		}
		s.exited = true
		if len(p) == 4 {
			s.code = int32(binary.LittleEndian.Uint32(p))
		}
		s.exitAt = time.Now()
	}
	s.mu.Unlock()
	if !keep {
		s.set.count(func(c *DispatchCounts) { c.Dropped++ })
		return false
	}
	return s.toShimFrame(t, p) && t == frameExit
}

// knownInfo says whether an INFO payload is a JSON object of infoKeys only.
func knownInfo(p []byte) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal(p, &fields) != nil {
		return false
	}
	for k := range fields {
		if !infoKeys[k] {
			return false
		}
	}
	return true
}

// toShimFrame sends a frame to the shim, or holds it while the job is not handed or the shim is away. It says whether
// the shim got it. An exit code whose write fails is held for the shim's next connection.
func (s *session) toShimFrame(t frameType, p []byte) bool {
	s.ws.Lock()
	defer s.ws.Unlock()
	s.mu.Lock()
	sw := s.shim
	if sw == nil || !s.live {
		kept := s.holdForShim(shimFrame{t, p})
		s.mu.Unlock()
		if !kept {
			s.set.count(func(c *DispatchCounts) { c.Dropped++ })
		}
		return false
	}
	s.mu.Unlock()
	if sw.send(t, p) != nil {
		s.mu.Lock()
		if t == frameExit {
			s.holdForShim(shimFrame{t, p})
		}
		s.mu.Unlock()
		s.shimAway(sw)
		return false
	}
	return true
}

// holdForShim holds a frame for the shim within shimHeld, and says whether it did. The exit code is always held: a
// job holds one at most. The caller holds mu.
func (s *session) holdForShim(f shimFrame) bool {
	cost := len(f.p) + heldCost
	if f.t != frameExit && s.shimCost+cost > shimHeld {
		return false
	}
	s.toShim = append(s.toShim, f)
	s.shimCost += cost
	return true
}

// attachShim makes sw the shim's connection and sends it what was held. It says whether the exit code reached it.
func (s *session) attachShim(sw *wire) bool {
	s.ws.Lock()
	defer s.ws.Unlock()
	s.mu.Lock()
	old := s.shim
	s.shim, s.away[1] = sw, time.Time{}
	s.mu.Unlock()
	if old != nil && old != sw {
		old.close()
	}
	return s.sendHeld()
}

// goLive lets the machine's frames go to the shim once the job is handed, after what was held for it. It says whether
// the exit code reached the shim.
func (s *session) goLive() bool {
	s.ws.Lock()
	defer s.ws.Unlock()
	s.mu.Lock()
	s.live = true
	s.mu.Unlock()
	return s.sendHeld()
}

// sendHeld sends the shim, in order, what was held for it; the caller holds ws.
func (s *session) sendHeld() bool {
	s.mu.Lock()
	sw, held := s.shim, s.toShim
	if sw == nil || !s.live {
		s.mu.Unlock()
		return false
	}
	s.toShim, s.shimCost = nil, 0
	s.mu.Unlock()
	for i, f := range held {
		if sw.send(f.t, f.p) != nil {
			rest := held[i+1:]
			if f.t == frameExit {
				rest = held[i:]
			}
			s.mu.Lock()
			s.toShim = append(rest, s.toShim...)
			s.shimCost = 0
			for _, h := range s.toShim {
				s.shimCost += len(h.p) + heldCost
			}
			s.mu.Unlock()
			s.shimAway(sw)
			return false
		}
		if f.t == frameExit {
			return true
		}
	}
	return false
}

func (s *session) shimAway(sw *wire) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.shim != sw {
		return
	}
	s.shim = nil
	if s.live && s.away[1].IsZero() {
		s.away[1] = time.Now()
	}
}

// lose ends a handed job whose machine is lost: the shim gets exit code 1, which is no TaskResult, so the identity's
// listener fails the job.
//
// The 1 goes the way the Worker's own exit code would: to the shim now, or held for it, so a shim whose resume was
// answered while its job was being lost (adopted, not yet attached) still gets it when it attaches. A job whose exit
// code came keeps that code for its shim.
func (s *session) lose() {
	s.mu.Lock()
	if s.exited {
		s.mu.Unlock()
		return
	}
	s.exited = true
	s.mu.Unlock()
	s.toShimFrame(frameExit, binary.LittleEndian.AppendUint32(nil, 1))
}

// closeAll ends both connections of a job that is over, each after what was sent on it.
func (s *session) closeAll() {
	s.mu.Lock()
	ws := []*wire{s.mach, s.shim, s.from}
	s.mu.Unlock()
	for _, w := range ws {
		if w != nil {
			w.end()
		}
	}
}
