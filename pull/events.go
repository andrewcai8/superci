package main

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/actions/scaleset"
)

// The job timeline's event log. Each fact about a job or a machine is one JSON line written where it happens (the
// listener's own log, which the CloudWatch agent ships; a machine's marks arrive at /mark and are written here), keyed
// by the job's GUID and the machine's instance id. Lines are never changed or counted; every table the
// timeline tool prints is derived from them. timeline.schema.json names every event and its fields.

// tlVersion is the schema version every line carries.
const tlVersion = 1

// tlBoot names this listener process; every line carries it, so a table can be cut by process generation. eid is
// unique to a line (boot and a count): the log ships at least once, so a reader keeps one line of each eid.
var (
	tlBoot = fmt.Sprintf("%x", time.Now().UnixNano())
	tlSeq  atomic.Uint64
)

// tl writes one event. t is when the fact happened; kv are the event's fields as slog pairs.
func tl(ev string, t time.Time, kv ...any) {
	slog.Info("tl", append([]any{"v", tlVersion, "ev", ev, "t", t.UTC().Format(time.RFC3339Nano), "t_ms", t.UnixMilli(),
		"boot", tlBoot, "eid", fmt.Sprintf("%s.%d", tlBoot, tlSeq.Add(1))}, kv...)...)
}

// tlStart writes that a listener process began.
func tlStart(deployment string, now time.Time) {
	build := ""
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			if s.Key == "vcs.revision" {
				build = s.Value
			}
		}
	}
	tl("listener.start", now, "pid", os.Getpid(), "build", build, "deployment", deployment)
}

// tlDecision writes why a launch round asked for n machines: the set's snapshot, the limit the registration budget
// left across the sets, and the spare rate it came from.
func tlDecision(snap Snapshot, round string, n, limit int, spare float64, now time.Time) {
	tl("launch.decision", now, "set", snap.Set, "round", round, "n", n, "limit", limit, "spare_rate", spare, "desired", snap.Desired, "live", snap.Live,
		"inflight", snap.Inflight, "booting", snap.Booting, "idle", snap.Idle, "busy", snap.Busy, "pool", snap.Pool,
		"assigned", snap.Stats.TotalAssignedJobs, "available", snap.Stats.TotalAvailableJobs)
}

// tlPoolTarget writes a set's pool and what it holds now, once each state log. extra carries what a pool rule adds
// (its burst, quantile, effective launch time, floor, cap and choice).
func tlPoolTarget(snap Snapshot, now time.Time, extra ...any) {
	tl("pool.target", now, append([]any{"set", snap.Set, "pool", snap.Pool, "desired", snap.Desired, "live", snap.Live, "idle", snap.Idle,
		"booting", snap.Booting, "busy", snap.Busy}, extra...)...)
}

// tlRetired writes why the listener is ending a machine: stuck, overdue, retire (its idle runner removed), shed.
func tlRetired(set string, m *Machine, why string, now time.Time) {
	tl("machine.retired", now, "set", set, "inst", m.ID, "why", why, "age_ms", now.Sub(m.Launched).Milliseconds())
}

// tlMessage writes what one message of a set's session says about its jobs.
func tlMessage(set string, msg *scaleset.RunnerScaleSetMessage, now time.Time) {
	if msg == nil {
		return
	}
	job := func(b scaleset.JobMessageBase) []any {
		return []any{"set", set, "rr", b.RunnerRequestID, "run", b.WorkflowRunID, "job", b.JobID, "name", b.JobDisplayName, "repo", b.OwnerName + "/" + b.RepositoryName}
	}
	for _, j := range msg.JobAvailableMessages {
		tl("job.available", now, append(job(j.JobMessageBase), "labels", j.RequestLabels, "queue_time", tlTime(j.QueueTime))...)
	}
	for _, j := range msg.JobAssignedMessages {
		tl("job.assigned", now, append(job(j.JobMessageBase), "labels", j.RequestLabels, "queue_time", tlTime(j.QueueTime), "assign_time", tlTime(j.ScaleSetAssignTime))...)
	}
	for _, j := range msg.JobStartedMessages {
		tl("job.started", now, append(job(j.JobMessageBase), "runner", j.RunnerName, "runner_id", j.RunnerID, "inst", tlInstance(j.RunnerName),
			"queue_time", tlTime(j.QueueTime), "assign_time", tlTime(j.ScaleSetAssignTime), "runner_assign_time", tlTime(j.RunnerAssignTime),
			"queue_ms", now.Sub(j.QueueTime).Milliseconds())...)
	}
	for _, j := range msg.JobCompletedMessages {
		tl("job.completed", now, append(job(j.JobMessageBase), "runner", j.RunnerName, "runner_id", j.RunnerID, "inst", tlInstance(j.RunnerName),
			"result", j.Result, "finish_time", tlTime(j.FinishTime))...)
	}
}

// tlAcquired writes that AcquireJobs answered for these runner requests.
func tlAcquired(set string, got []int64, now time.Time) {
	for _, rr := range got {
		tl("job.acquired", now, "set", set, "rr", rr)
	}
}

// tlLaunched writes one machine's launch: the CreateFleet call began at started and EC2 has just answered.
func tlLaunched(set, round, step string, started time.Time, ms []*Machine) {
	now := time.Now()
	for _, m := range ms {
		tl("machine.launched", now, "set", set, "inst", m.ID, "region", m.Region, "az", m.AZ, "type", m.Type, "lifecycle", m.Lifecycle,
			"round", round, "step", step, "requested", tlTime(started), "answer_ms", now.Sub(started).Milliseconds())
	}
}

// tlEnded writes that the listener asked EC2 to terminate these machines.
func tlEnded(region, why string, ids []string, now time.Time) {
	for _, id := range ids {
		tl("machine.ended", now, "inst", id, "region", region, "why", why)
		firstAsks.Lock()
		delete(firstAsks.seen, id)
		firstAsks.Unlock()
		offsets.Lock()
		delete(offsets.last, id)
		offsets.Unlock()
	}
}

// tlInstance is the instance id a runner is named after: <set>-<instance id>.
func tlInstance(runner string) string {
	if i := strings.LastIndex(runner, "-i-"); i >= 0 {
		return runner[i+1:]
	}
	return ""
}

func tlTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// askState is a machine's waits for its runner: when it first asked, how many asks it made, and when its wait last
// showed as a jit.wait.
type askState struct {
	first    time.Time
	polls    int
	lastWait time.Time
}

// waitEvery is how often a machine that is still waiting for its runner writes a jit.wait.
const waitEvery = 30 * time.Second

// firstAsks holds the machines whose first ask was written, so a machine waiting for the registration budget (it asks
// every 3 s) leaves one jit.ask, and one jit.granted that says how long it waited.
var firstAsks = struct {
	sync.Mutex
	seen map[string]*askState
}{seen: map[string]*askState{}}

var verdictNames = map[Verdict]string{Grant: "grant", End: "end", Wait: "wait", Unknown: "unknown"}

// tlAsk writes a machine's ask: the first of its asks as jit.ask, the verdict that ends its waiting as jit.granted
// (with how long and how many asks it took) or jit.refused (end), and, while it is still told to wait, a jit.wait every
// waitEvery with why it waits (State.AskWhy). The wait is written while it lasts: jit.granted comes at its end, which
// is too late to show a stall.
func tlAsk(set, inst string, v Verdict, why string, now time.Time) {
	firstAsks.Lock()
	defer firstAsks.Unlock()
	st := firstAsks.seen[inst]
	if st == nil {
		st = &askState{first: now}
		firstAsks.seen[inst] = st
		tl("jit.ask", now, "set", set, "inst", inst, "verdict", verdictNames[v])
	}
	st.polls++
	switch v {
	case Wait:
		since := st.first
		if st.lastWait.After(since) {
			since = st.lastWait
		}
		if now.Sub(since) >= waitEvery {
			st.lastWait = now
			tl("jit.wait", now, "set", set, "inst", inst, "why", why, "waited_ms", now.Sub(st.first).Milliseconds(), "polls", st.polls)
		}
	case Grant:
		tl("jit.granted", now, "set", set, "inst", inst, "waited_ms", now.Sub(st.first).Milliseconds(), "polls", st.polls)
		delete(firstAsks.seen, inst)
	case End:
		tl("jit.refused", now, "set", set, "inst", inst, "why", "end", "register_ms", 0, "waited_ms", now.Sub(st.first).Milliseconds(), "polls", st.polls)
		delete(firstAsks.seen, inst)
	}
}

// tlIssued writes that a machine's runner was registered and its config left; started is when its ask arrived.
func tlIssued(set, inst, runner string, runnerID int, started, now time.Time) {
	tl("jit.issued", now, "set", set, "inst", inst, "runner", runner, "runner_id", runnerID, "register_ms", now.Sub(started).Milliseconds())
}

// tlClaim writes the claim a grant takes before it registers a runner (the fence): taken, refused (another grant holds
// it) or error (the claim store could not be read or written); claim_ms is how long the claim took.
func tlClaim(set, inst string, err error, started, now time.Time) {
	result := "taken"
	switch {
	case errors.Is(err, ErrClaimed):
		result = "refused"
	case err != nil:
		result = "error"
	}
	tl("jit.claim", now, "set", set, "inst", inst, "result", result, "claim_ms", now.Sub(started).Milliseconds())
}

// tlRefused writes that no config left for a machine's ask: exists, refused (GitHub's cap), failed or withheld.
func tlRefused(set, inst, why string, started, now time.Time) {
	tl("jit.refused", now, "set", set, "inst", inst, "why", why, "register_ms", now.Sub(started).Milliseconds())
}

// tlNotice writes that AWS gave notice it takes a spot machine back.
func tlNotice(set string, m *Machine, now time.Time) {
	tl("machine.notice", now, "set", set, "inst", m.ID, "region", m.Region, "az", m.AZ, "type", m.Type)
}

// A machine's marks. boot.sh stamps the moments the listener cannot see (it reads the machine's clock) and posts them
// with the identity document, the way it asks for its runner, so no machine needs a credential for it. Only these names
// are taken, so a job that reads the document cannot write other events; each machine gets at most markLimit.
var markNames = map[string]bool{"boot.start": true, "cache.start": true, "cache.up": true, "doc.got": true, "jit.sent": true, "jit.answered": true,
	"runner.start": true, "runner.online": true, "job.running": true, "job.done": true, "runner.exit": true, "poweroff": true,
	"job.got": true, "worker.start": true, "worker.exit": true, "hydrate.start": true, "hydrate.done": true, "readset.start": true, "readset.done": true, "preread.start": true, "preread.done": true, "rewarm.done": true, "warm.done": true, "warm.ready": true, "cache.restore": true, "cache.save": true}

const markLimit = 256

// A mark's value holds at most markValMax bytes (a cache key, url-escaped, fits) and a request's marks header at most
// markHeaderMax, so a job, which is root on its machine, can make the listener write at most markLimit short lines.
const (
	markValMax    = 256
	markHeaderMax = 32 << 10
)

var markCounts = struct {
	sync.Mutex
	n map[string]int
}{n: map[string]int{}}

// mark takes in a machine's marks: header X-Superci-Marks is a comma list of name=epoch[=dur_ms[=val]] (epoch in seconds
// from the machine's clock, dur_ms since the machine's previous mark), X-Superci-Sent the epoch the request left. A mark whose clock is over an hour off
// the listener's is dropped.
func (h *Server) mark(w http.ResponseWriter, r *http.Request) {
	recv := time.Now()
	set, m, err := h.marker(r)
	if err != nil {
		http.Error(w, "unknown machine", http.StatusForbidden)
		return
	}
	marks := r.Header.Get("X-Superci-Marks")
	if len(marks) > markHeaderMax {
		http.Error(w, "marks too long", http.StatusRequestEntityTooLarge)
		return
	}
	h.markEvents(set, m, recv, marks, r.Header.Get("X-Superci-Sent"))
	w.WriteHeader(http.StatusNoContent)
}

// marker is the set and machine a /mark comes from. A dispatch machine is no scale set's: the dispatch sets are looked
// up with the scale sets, first and again after each listing of EC2, so a machine that posts before the listing that
// holds it is found by the listing it caused, which is the only one it costs. Before, every dispatch machine was refused
// (403), and boot.sh sent the batch again each second, each time a describe of the region.
func (h *Server) marker(r *http.Request) (string, *Machine, error) {
	doc, err := h.document(r)
	if err != nil {
		return "", nil, err
	}
	for try := 0; try < 3; try++ {
		if h.dispatchMachine != nil {
			if set, m := h.dispatchMachine(doc.InstanceID); m != nil {
				return set, m, nil
			}
		}
		if s, m := h.scaleMachine(doc.InstanceID); m != nil {
			return s.spec.Name, m, nil
		}
		h.refresh(r.Context(), doc.Region)
	}
	return "", nil, fmt.Errorf("instance %s is no machine of ours", doc.InstanceID)
}

// markEvents writes the marks of one request, received at recv.
func (h *Server) markEvents(set string, m *Machine, recv time.Time, marks, sentAt string) {
	offset := clockOffset(m.ID, recv, parseEpoch(sentAt))
	for _, kv := range strings.Split(marks, ",") {
		name, rest, _ := strings.Cut(kv, "=")
		at, rest, _ := strings.Cut(rest, "=")
		dur, val, _ := strings.Cut(rest, "=")
		t := parseEpoch(at)
		if len(val) > markValMax {
			val = val[:markValMax]
		}
		if !markNames[name] || t.IsZero() || t.Sub(recv).Abs() > time.Hour {
			continue
		}
		markCounts.Lock()
		over := markCounts.n[m.ID] >= markLimit
		if !over {
			markCounts.n[m.ID]++
		}
		if len(markCounts.n) > 100000 {
			markCounts.n = map[string]int{}
		}
		markCounts.Unlock()
		if over {
			break
		}
		fields := []any{"set", set, "inst", m.ID, "offset_ms", offset, "val", val}
		if ms, err := strconv.ParseInt(dur, 10, 64); err == nil {
			fields = append(fields, "dur_ms", ms)
		}
		tl(name, t, fields...)
	}
}

// offsetPosts is how many of a machine's last posts its clock offset is estimated from.
const offsetPosts = 8

// offsets holds each machine's last posts' receipt less stamp, in ms.
var offsets = struct {
	sync.Mutex
	last map[string][]int64
}{last: map[string][]int64{}}

// clockOffset is the estimate, in ms, of how far the machine's clock is behind the listener's: the smallest receipt
// less stamp of its last offsetPosts posts. One post's is the offset plus that post's delay (hundreds of ms on a busy
// listener), so one overstates it by a delay that varies; the smallest of several holds the least delay. Only the last
// few count, so a clock chrony steps during boot is followed, not remembered. 0 until a post carries its stamp.
func clockOffset(inst string, recv, sent time.Time) int64 {
	offsets.Lock()
	defer offsets.Unlock()
	if len(offsets.last) > 100000 {
		offsets.last = map[string][]int64{}
	}
	posts := offsets.last[inst]
	if !sent.IsZero() {
		posts = append(posts, recv.Sub(sent).Milliseconds())
		posts = posts[max(0, len(posts)-offsetPosts):]
		offsets.last[inst] = posts
	}
	if len(posts) == 0 {
		return 0
	}
	return slices.Min(posts)
}

func parseEpoch(s string) time.Time {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f <= 0 {
		return time.Time{}
	}
	return time.Unix(0, int64(f*1e9))
}

// tlRetiredAll is tlRetired for machines of any set.
func tlRetiredAll(ms []*Machine, why string, now time.Time) {
	for _, m := range ms {
		tlRetired(m.Set, m, why, now)
	}
}
