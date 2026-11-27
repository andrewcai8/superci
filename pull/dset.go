package main

import (
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/actions/scaleset"
)

// A DispatchSet is a size the identity plane serves (README, Identity plane). It has no scale set and registers no
// runner: each machine waits on /work for the one job the dispatcher hands it, runs it and ends. Its demand is the jobs
// waiting for a machine and the jobs running; its pool follows the jobs that arrive at the dispatcher. Its lifecycle
// is not a runner's (no registration, no GitHub fence before an end), so it is a type of its own, not a mode of State.
type DispatchSet struct {
	spec     SetSpec
	noDemand BatchRule // the jobs that are no routed demand: they size no pool
	fleet    *SetFleet
	wake     chan struct{} // a job arrived: the launcher looks at the deficit now

	mu       sync.Mutex
	started  time.Time
	boot     string
	rounds   int
	pool     int
	max      int
	capped   bool
	listings map[string]time.Time // per region, when its last complete listing began
	machines map[string]*dmachine
	reserved map[string]int      // per launch round, capacity asked of EC2 and not yet seen
	idle     []*dmachine         // machines waiting on /work for a job, oldest first (the most likely warm)
	queue    []*session          // jobs waiting for a machine, oldest first
	pairs    map[string]*session // by machine: the job a claimed machine was paired with, until it is over
	byJob    map[string]*session // every job whose message was read, by job id, until it is over
	meter
	surplusSince time.Time
	odLaunched   []time.Time
	allLaunched  []time.Time
	counts       DispatchCounts // since the metrics last took them
	waits        []float64      // seconds from a job's arrival to its hand-off, since the metrics last took them
	jobs         []JobRecord    // records of the jobs over since the metrics last took them
	price        func(Machine) float64
	ended        map[string]time.Time // jobs over, by id, so a late resume in this process adopts none
	over         func(job string)     // marks a job over beside its claim, for a resume after a restart
}

// A dmachine is one machine of a dispatch set: EC2's facts, and where it is in its one job's life.
type dmachine struct {
	*Machine
	phase     machinePhase
	wire      *wire     // its /work connection, while one holds
	session   *session  // the job it was claimed for
	since     time.Time // when it last had no connection: launched, adopted after a start, or its connection lost
	firstWork time.Time // its first /work, which ends its launch's time
}

type machinePhase int

const (
	mBooting machinePhase = iota // no connection: starting, or lost it before it was paired
	mWaiting                     // on /work, in the idle queue
	mPairing                     // taken for a job; its claim is being made
	mClaimed                     // holds its claim: it runs its one job
	mEnding                      // spent, done or past a bound: it is terminated
)

// DispatchCounts is what a dispatch set did, for the metrics.
type DispatchCounts struct {
	Handoffs int // jobs handed to a machine
	Retries  int // pairings that ended before the handed point: the machine spent, the job paired again
	Resumes  int // connections that rejoined their job after a break
	Lost     int // handed jobs whose machine did not come back within the window, or that ran past their bounds
	Dropped  int // frames over a job's budgets, dropped
}

func NewDispatchSet(spec SetSpec, fleet *SetFleet, regions []string, now time.Time) *DispatchSet {
	st := &DispatchSet{spec: spec, fleet: fleet, wake: make(chan struct{}, 1), started: now, boot: fmt.Sprintf("%x", now.UnixNano()),
		max: spec.Max, listings: map[string]time.Time{}, machines: map[string]*dmachine{}, reserved: map[string]int{},
		pairs: map[string]*session{}, byJob: map[string]*session{}, ended: map[string]time.Time{}}
	for _, r := range regions {
		st.listings[r] = time.Time{}
	}
	return st
}

// ready says whether every region has been listed: before then a region's machines read as none, and every one of
// them would be launched again.
func (st *DispatchSet) ready() bool {
	for _, at := range st.listings {
		if at.IsZero() {
			return false
		}
	}
	return true
}

// desired is the machines the set's jobs ask for: those waiting for one, those running, and the pool (none while the
// spend cap holds), at most max.
func (st *DispatchSet) desired() int {
	pool := st.pool
	if st.capped {
		pool = 0
	}
	return max(0, min(st.max, len(st.queue)+len(st.pairs)+pool))
}

// live is the machines that are or will be supply: every machine not ending, and the capacity asked of EC2.
func (st *DispatchSet) live() int {
	n := 0
	for _, k := range st.reserved {
		n += k
	}
	for _, m := range st.machines {
		if m.phase != mEnding {
			n++
		}
	}
	return n
}

// Deficit reserves the machines to launch now, under a new round's name.
func (st *DispatchSet) Deficit(now time.Time) (round string, n int) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.capped || !st.ready() {
		return "", 0
	}
	if n = st.desired() - st.live(); n <= 0 {
		return "", 0
	}
	st.rounds++
	round = fmt.Sprintf("%s-%d", st.boot, st.rounds)
	st.reserved[round] = n
	return round, n
}

// Launched ends a round's reservation: the machines EC2 started join the set.
func (st *DispatchSet) Launched(round string, got []*Machine, now time.Time) {
	st.mu.Lock()
	defer st.mu.Unlock()
	delete(st.reserved, round)
	for _, m := range got {
		if _, ok := st.machines[m.ID]; !ok {
			m.Ordered, m.Dispatch = now, true
			st.machines[m.ID] = &dmachine{Machine: m, since: now}
		}
		st.allLaunched = append(st.allLaunched, now)
		if m.Lifecycle == "on-demand" {
			st.odLaunched = append(st.odLaunched, now)
		}
	}
}

// OnDemandRoom is how many on-demand machines may start now without the on-demand share of the last half hour's
// launches passing frac.
func (st *DispatchSet) OnDemandRoom(frac float64, more int, now time.Time) int {
	st.mu.Lock()
	defer st.mu.Unlock()
	cut := now.Add(-30 * time.Minute)
	st.allLaunched, st.odLaunched = since(st.allLaunched, cut), since(st.odLaunched, cut)
	return max(0, int(frac*float64(len(st.allLaunched)+more))-len(st.odLaunched))
}

// Seen merges one complete listing of a region's machines of the set, begun at began. A machine it does not list is
// forgotten once it has no connection: a connection that holds says more than a listing that missed it.
func (st *DispatchSet) Seen(region string, listed []*Machine, began, now time.Time) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.listings[region] = began
	in := map[string]bool{}
	for _, l := range listed {
		in[l.ID] = true
		m := st.machines[l.ID]
		if m == nil {
			st.machines[l.ID] = &dmachine{Machine: l, since: now}
			if st.reserved[l.Round] > 0 {
				st.reserved[l.Round]--
			}
			continue
		}
		m.IP, m.Type, m.AZ, m.Lifecycle, m.Seen = l.IP, l.Type, l.AZ, l.Lifecycle, now
		if l.Template != "" {
			m.Template = l.Template
		}
		if m.Launched.IsZero() {
			m.Launched = l.Launched
		}
	}
	for id, m := range st.machines {
		if m.Region == region && !in[id] && now.Sub(m.Launched) > 90*time.Second && m.wire == nil && m.session == nil {
			delete(st.machines, id)
		}
	}
}

func (st *DispatchSet) machine(id string) *dmachine {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.machines[id]
}

// mayWait says whether a machine may wait for a job: it was never taken for one.
func (st *DispatchSet) mayWait(m *dmachine) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return m.phase == mBooting || m.phase == mWaiting
}

// waiting puts a machine on mw in the idle queue, or gives a machine already there its new connection in its place.
func (st *DispatchSet) waiting(m *dmachine, mw *wire, now time.Time) bool {
	st.mu.Lock()
	if m.phase != mBooting && m.phase != mWaiting {
		st.mu.Unlock()
		return false
	}
	old := m.wire
	m.wire = mw
	if m.phase == mBooting {
		m.phase = mWaiting
		st.idle = append(st.idle, m)
	}
	if m.firstWork.IsZero() {
		m.firstWork = now
		if !m.Ordered.IsZero() {
			st.sample(now.Sub(m.Ordered))
		}
	}
	st.mu.Unlock()
	if old != nil && old != mw {
		old.close()
	}
	return true
}

// mayResume says whether a machine may rejoin a job: it is not ending.
func (st *DispatchSet) mayResume(m *dmachine) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return m.phase != mEnding
}

// rejoined gives a machine that rejoined its job its new connection.
func (st *DispatchSet) rejoined(m *dmachine, mw *wire, s *session) bool {
	st.mu.Lock()
	if m.phase == mEnding {
		st.mu.Unlock()
		return false
	}
	old := m.wire
	m.wire, m.session, m.phase = mw, s, mClaimed
	st.idle = slices.DeleteFunc(st.idle, func(o *dmachine) bool { return o == m })
	st.counts.Resumes++
	s.mu.Lock()
	s.from = mw
	s.mu.Unlock()
	st.mu.Unlock()
	if old != nil && old != mw {
		old.close()
	}
	return true
}

// sessionOf is the job whose frames a machine's connection carries, if any.
func (st *DispatchSet) sessionOf(m *dmachine, mw *wire) *session {
	st.mu.Lock()
	defer st.mu.Unlock()
	if m.wire != mw {
		return nil
	}
	return m.session
}

// machineClosed settles a machine's connection that ended. A machine that waited goes back to booting, and may come
// again; a job's machine is away until it rejoins.
func (st *DispatchSet) machineClosed(m *dmachine, mw *wire) {
	st.mu.Lock()
	if m.wire != mw {
		st.mu.Unlock()
		return
	}
	m.wire = nil
	s := m.session
	if m.phase == mWaiting {
		m.phase, m.since = mBooting, time.Now()
		st.idle = slices.DeleteFunc(st.idle, func(o *dmachine) bool { return o == m })
	}
	st.mu.Unlock()
	if s != nil {
		s.machineAway(mw)
	}
}

// endMachine ends a machine: it is never offered a job again, and is terminated.
func (st *DispatchSet) endMachine(m *dmachine, why string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.end(m, why)
}

func (st *DispatchSet) end(m *dmachine, why string) {
	if m.phase == mEnding {
		return
	}
	m.phase, m.Ending, m.session = mEnding, true, nil
	st.idle = slices.DeleteFunc(st.idle, func(o *dmachine) bool { return o == m })
	if m.wire != nil {
		go m.wire.end()
		m.wire = nil
	}
	slog.Info("dispatch machine ends", "set", st.spec.Name, "machine", m.ID, "why", why)
}

// enqueue puts a job whose message was read in the queue. A shim whose connection broke before PAIRED replays its
// job on a new one, so a job is known by its id: a replay takes a queued job's place, waits for a pairing in flight to
// settle (wait), or is sent to the pair that has the job (its <instance>.<token>), and is never a second job.
func (st *DispatchSet) enqueue(s *session, head jobHead, now time.Time) (pair string, wait chan struct{}) {
	st.mu.Lock()
	defer st.mu.Unlock()
	s.id, s.head = head.ID, head
	switch h := st.byJob[head.ID]; {
	case h == nil:
		s.phase = queued
		st.queue = append(st.queue, s)
		if !st.noDemand.Matches(scaleset.JobMessageBase{EventName: head.Event, JobWorkflowRef: head.WorkflowRef, JobDisplayName: head.Name}) { // superci's own maintenance runs size no pool
			st.arrive(now)
		}
	case h.phase == running:
		return h.key(), nil
	case h.phase == pairing:
		return "", h.settled
	default:
		s.phase, s.unknown, s.after = queued, h.unknown, h.after
		if i := slices.Index(st.queue, h); i >= 0 {
			st.queue[i] = s
		} else {
			st.queue = append(st.queue, s)
		}
		h.phase = over
		go h.closeAll()
	}
	st.byJob[head.ID] = s
	select {
	case st.wake <- struct{}{}:
	default:
	}
	return "", nil
}

// shimClosed settles a shim's connection that ended. A job not yet handed is dropped (its shim starts it again); a
// handed one is away until the shim rejoins.
func (st *DispatchSet) shimClosed(s *session, sw *wire) {
	s.shimAway(sw)
	st.mu.Lock()
	defer st.mu.Unlock()
	if s.phase == reading || s.phase == queued {
		s.phase = over
		st.queue = slices.DeleteFunc(st.queue, func(o *session) bool { return o == s })
		st.forget(s)
	}
}

// forget drops a job that is over from the jobs known by id.
func (st *DispatchSet) forget(s *session) {
	if st.byJob[s.id] == s {
		delete(st.byJob, s.id)
	}
}

// next takes the oldest waiting job that is not backing off and the oldest waiting machine for a pairing.
func (st *DispatchSet) next(now time.Time) (*session, *dmachine, *wire) {
	st.mu.Lock()
	defer st.mu.Unlock()
	i := slices.IndexFunc(st.queue, func(s *session) bool { return !now.Before(s.after) })
	if i < 0 || len(st.idle) == 0 {
		return nil, nil, nil
	}
	s, m := st.queue[i], st.idle[0]
	st.queue, st.idle = slices.Delete(st.queue, i, i+1), st.idle[1:]
	s.phase, s.settled = pairing, make(chan struct{})
	m.phase = mPairing
	return s, m, m.wire
}

// claimed records that a machine's claim is the job's: frames of the machine's connection mw are now the job's.
func (st *DispatchSet) claimed(s *session, m *dmachine, mw *wire, token string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	m.phase, m.session = mClaimed, s
	s.machine, s.token = m.ID, token
	st.pairs[m.ID] = s
	s.mu.Lock()
	s.from = mw
	s.mu.Unlock()
}

// retry settles a pairing that failed before the handed point: the machine is spent, and the job goes back to the
// head of the queue while its shim holds. A job that has now spent claimSpend machines on unknown claims is not paired
// again for the wait it returns.
func (st *DispatchSet) retry(s *session, m *dmachine, unknown bool, now time.Time) (wait time.Duration) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.counts.Retries++
	st.end(m, "spent before the handed point")
	if st.pairs[m.ID] == s {
		delete(st.pairs, m.ID)
	}
	s.machine, s.token = "", ""
	close(s.settled)
	s.settled = nil
	s.mu.Lock()
	alive := s.shim != nil && !s.shim.gone()
	s.from, s.toShim, s.shimCost, s.out, s.infos, s.exited = nil, nil, 0, 0, 0, false
	s.mu.Unlock()
	if !alive {
		s.phase = over
		st.forget(s)
		return 0
	}
	if unknown {
		if s.unknown++; s.unknown >= claimSpend {
			s.after, wait = now.Add(claimBackoff), claimBackoff
		}
	}
	s.phase = queued
	st.queue = append([]*session{s}, st.queue...)
	return wait
}

// abandon settles a pairing that failed before the handed point with no shim to go on with: the machine is spent and
// the job dropped, for its shim to start again or rejoin the pair its PAIRED names.
func (st *DispatchSet) abandon(s *session, m *dmachine) {
	st.mu.Lock()
	st.counts.Retries++
	st.end(m, "spent before the handed point")
	if st.pairs[m.ID] == s {
		delete(st.pairs, m.ID)
	}
	close(s.settled)
	s.settled, s.phase = nil, over
	st.forget(s)
	st.mu.Unlock()
	s.closeAll()
}

// handed records the handed point: the job's claim names its pair.
func (st *DispatchSet) handed(s *session, now time.Time) {
	st.mu.Lock()
	defer st.mu.Unlock()
	s.phase, s.handed = running, now
	st.counts.Handoffs++
	st.waits = append(st.waits, now.Sub(s.arrived).Seconds())
	close(s.settled)
	s.settled = nil
}

// pairOf is the job a machine was paired with in this process, its token, and while the pairing is still going on a
// channel closed once it settles.
func (st *DispatchSet) pairOf(inst string) (*session, string, chan struct{}) {
	st.mu.Lock()
	defer st.mu.Unlock()
	s := st.pairs[inst]
	if s == nil {
		return nil, "", nil
	}
	return s, s.token, s.settled
}

// adopt is the session of a job a previous process handed, rejoined after a restart: its claims name the pair, the
// job's claim names the job for a re-run, and both sides are away until they come.
func (st *DispatchSet) adopt(inst, token, job string, c jobClaim, now time.Time) *session {
	st.mu.Lock()
	defer st.mu.Unlock()
	if s := st.pairs[inst]; s != nil {
		return s
	}
	if _, gone := st.ended[job]; gone {
		return nil
	}
	s := &session{set: st, id: job, head: jobHead{ID: job, Run: c.Run, Name: c.Name, Repo: c.Repo, WorkflowRef: c.Ref, Event: c.Event}, agent: c.Agent, machine: inst,
		token: token, phase: running, handed: now, arrived: now, live: true, away: [2]time.Time{now, now}, adopted: true}
	// A claim that kept the job's times gives them back; one that did not (an earlier build's) leaves its record
	// partial, rather than a queue of 0 and a wall from the restart.
	if c.Handed > 0 {
		s.handed, s.recHanded = time.UnixMilli(c.Handed), true
	}
	if c.Arrived > 0 {
		s.arrived = time.UnixMilli(c.Arrived)
	}
	st.pairs[inst], st.byJob[job] = s, s
	return s
}

// finish ends a job and its machine. A job whose machine was lost gives its shim exit code 1 first. The writes run on
// their own: a peer that does not read holds a write for up to its silence, and the reconciler is not to wait on it.
func (st *DispatchSet) finish(s *session, why jobEnd) {
	st.mu.Lock()
	if s.phase == over {
		st.mu.Unlock()
		return
	}
	s.phase = over
	if st.pairs[s.machine] == s {
		delete(st.pairs, s.machine)
	}
	st.forget(s)
	lost := why == machineLost
	reason := "its job ended"
	if lost {
		reason = "its job was lost"
		st.counts.Lost++
	}
	var mach *Machine
	if m := st.machines[s.machine]; m != nil {
		mach = m.Machine
		if m.session == s {
			st.end(m, reason)
		}
	}
	rec := st.record(s, why, mach, time.Now())
	st.jobs = append(st.jobs, rec)
	if len(st.jobs) > maxJobBuffer { // a listener run with metrics off takes none
		st.jobs = st.jobs[len(st.jobs)-maxJobBuffer:]
	}
	st.ended[s.id] = time.Now()
	over := st.over
	st.mu.Unlock()
	tlJobRecord(rec, time.Now())
	go func() {
		if over != nil {
			over(s.id)
		}
		if why == machineLost {
			s.lose()
		}
		s.closeAll()
	}()
	slog.Info("dispatch job over", "set", st.spec.Name, "job", s.id, "machine", s.machine, "lost", lost)
}

// record is a dispatched job's one record, made when it is over (finish runs once a session). It has no completed
// message: the job's names come from its job message, its result from its Worker's exit code (or a failure when its
// machine was lost, which the identity's listener reports for it), and its machine is the dispatch machine. Queued is
// when the identity's shim brought the job and Started when the machine took it, so queue_ms is the wait for a machine;
// GitHub's own queue before an identity took the job is not in it (identities idle at once take a job).
func (st *DispatchSet) record(s *session, why jobEnd, m *Machine, now time.Time) JobRecord {
	s.mu.Lock()
	exited, code, exitAt := s.exited, s.code, s.exitAt
	s.mu.Unlock()
	// A lost machine's job, and one whose exit code never reached its shim, are failures: the identity's listener fails
	// them at GitHub, whatever the Worker said.
	result := "Failed"
	if exited && why == exitRelayed {
		result = dispatchResult(code)
	}
	if exited && !exitAt.IsZero() {
		now = exitAt
	}
	started := s.handed
	if s.adopted && !s.recHanded {
		started = time.Time{}
	}
	owner, repo, _ := strings.Cut(s.head.Repo, "/")
	j := &scaleset.JobCompleted{Result: result, RunnerName: s.agent, JobMessageBase: scaleset.JobMessageBase{JobID: s.id, WorkflowRunID: s.head.Run,
		JobDisplayName: s.head.Name, OwnerName: owner, RepositoryName: repo, JobWorkflowRef: s.head.WorkflowRef, EventName: s.head.Event,
		RequestLabels: []string{st.spec.Label}, QueueTime: s.arrived, RunnerAssignTime: started, FinishTime: now}}
	r := newJobRecord(st.spec.Name, j, m, st.price, began{})
	r.Dispatched = true
	r.Inst = s.machine
	return r
}

// dispatchResult is the result a Worker's exit code stands for (TaskResult plus 100); any other code is the Worker's
// crash, which the identity's listener fails the job for.
func dispatchResult(code int32) string {
	switch code {
	case 100, 101:
		return "Succeeded"
	case 103:
		return "Canceled"
	case 105:
		return "Abandoned"
	default:
		return "Failed"
	}
}

// SetPricer sets how this set's job records price a machine (the spend's estimate).
func (st *DispatchSet) SetPricer(f func(Machine) float64) {
	st.mu.Lock()
	st.price = f
	st.mu.Unlock()
}

// TakeJobs hands over the records of the jobs over since the last call, for the minute's metrics.
func (st *DispatchSet) TakeJobs() []JobRecord {
	st.mu.Lock()
	defer st.mu.Unlock()
	j := st.jobs
	st.jobs = nil
	return j
}

// exitHeld ends the machine of a handed job whose exit code came while its shim was away: the job is over on the
// machine, and its exit code waits in the session for the shim to rejoin.
func (st *DispatchSet) exitHeld(s *session) {
	st.mu.Lock()
	defer st.mu.Unlock()
	s.mu.Lock()
	exited := s.exited
	s.mu.Unlock()
	if m := st.machines[s.machine]; m != nil && m.session == s && s.phase == running && exited {
		st.end(m, "its exit code is held for its shim")
	}
}

func (st *DispatchSet) count(f func(*DispatchCounts)) {
	st.mu.Lock()
	f(&st.counts)
	st.mu.Unlock()
}

// dispatchBounds are the ages past which a dispatch machine ends: maxAge waiting for a job, life in all, job for its
// job from its hand-off; warmup is how long after a start no surplus ends, while the pool is measured.
type dispatchBounds struct {
	maxAge, life, job, warmup, window time.Duration
}

// Reconcile ends what has outlived its bounds and returns the machines to terminate now and the jobs lost. A handed job
// whose machine has been away past the window, or that ran past its bound, is lost: its shim gets exit code 1. A shim
// away loses nothing: the machine renews its job itself, so the job runs on to its exit code, which is held for the
// shim until the window after the shim left. A machine waiting past its age, one that has had no connection past the
// window, any past its life, and surplus waiting machines end; an ending machine is terminated once, then a minute on
// if EC2 still lists it.
func (st *DispatchSet) Reconcile(now time.Time, b dispatchBounds) (end []*Machine, lost []JobRef) {
	st.mu.Lock()
	for job, at := range st.ended {
		if now.Sub(at) > b.job+b.window {
			delete(st.ended, job)
		}
	}
	st.mu.Unlock()
	for s, why := range st.expired(now, b) {
		if why == machineLost {
			if j, ok := s.jobRef(); ok {
				lost = append(lost, j)
			}
		}
		st.finish(s, why)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if !st.ready() {
		return nil, lost
	}
	for _, m := range st.machines {
		age := now.Sub(m.Launched)
		switch {
		case m.phase == mEnding, m.session != nil:
		case age > b.life:
			st.end(m, "past its life")
		case m.phase == mWaiting && age > b.maxAge:
			st.end(m, "waited past its age")
		case m.phase == mBooting && age > bootGrace && now.Sub(m.since) > b.window:
			st.end(m, "no connection")
		}
	}
	st.surplus(now, b.warmup)
	for _, m := range st.machines {
		if m.phase == mEnding && now.Sub(m.Ended) > time.Minute {
			m.Ended = now
			end = append(end, m.Machine)
		}
	}
	return end, lost
}

// jobEnd is why a handed job ends.
type jobEnd int

const (
	exitRelayed       jobEnd = iota // its exit code reached its shim
	machineLost                     // its machine was away past the window, or it ran past its bound: its shim gets 1
	shimLeftAfterExit               // its shim was away past the window with the exit code held for it
)

// expired is the handed jobs to end now, and why.
func (st *DispatchSet) expired(now time.Time, b dispatchBounds) map[*session]jobEnd {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := map[*session]jobEnd{}
	for inst, s := range st.pairs {
		if s.phase != running {
			continue
		}
		s.mu.Lock()
		machineAway, shimAway, exited := s.away[0], s.away[1], s.exited
		s.mu.Unlock()
		m := st.machines[inst]
		switch {
		case exited:
			if !shimAway.IsZero() && now.Sub(shimAway) > b.window {
				out[s] = shimLeftAfterExit
			}
		case !machineAway.IsZero() && now.Sub(machineAway) > b.window, now.Sub(s.handed) > b.job,
			m != nil && now.Sub(m.Launched) > b.life:
			out[s] = machineLost
		}
	}
	return out
}

// surplus ends the newest machines with no job, booting or waiting, once the set has had more than its jobs and pool
// ask for for 20 s, and not in the warmup after a start (a machine that rejoins its job after a start reads as booting
// until it does). The oldest are kept: a machine that has waited is warm. A machine with no job holds no runner, so
// its end needs no fence.
func (st *DispatchSet) surplus(now time.Time, warmup time.Duration) {
	excess := st.live() - st.desired()
	if excess <= 0 || now.Sub(st.started) < warmup {
		st.surplusSince = time.Time{}
		return
	}
	if st.surplusSince.IsZero() {
		st.surplusSince = now
	}
	if now.Sub(st.surplusSince) < 20*time.Second {
		return
	}
	var free []*dmachine
	for _, m := range st.machines {
		if m.phase == mBooting || m.phase == mWaiting {
			free = append(free, m)
		}
	}
	slices.SortFunc(free, func(a, b *dmachine) int { return b.Launched.Compare(a.Launched) })
	for _, m := range free[:min(len(free), excess, 100)] {
		st.end(m, "surplus")
	}
}

// jobRef is a lost job as the re-run path finds it: its run, its name and the identity that took it.
func (s *session) jobRef() (JobRef, bool) {
	owner, repo, ok := strings.Cut(s.head.Repo, "/")
	if !ok || s.head.Run == 0 {
		return JobRef{}, false
	}
	return JobRef{Owner: owner, Repo: repo, RunID: s.head.Run, Name: s.head.Name, Runner: s.agent, Instance: s.machine}, true
}

func (st *DispatchSet) SetPool(n int)           { st.mu.Lock(); st.pool = n; st.mu.Unlock() }
func (st *DispatchSet) SetMax(n int)            { st.mu.Lock(); st.max = n; st.mu.Unlock() }
func (st *DispatchSet) SetNoDemand(r BatchRule) { st.mu.Lock(); st.noDemand = r; st.mu.Unlock() }
func (st *DispatchSet) SetCapped(on bool)       { st.mu.Lock(); st.capped = on; st.mu.Unlock() }

// Carry and Resume keep the set's pool across a restart as State's do (carry.go).
func (st *DispatchSet) Carry(now time.Time, r PoolRule) Carried {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.carry(now, st.started, r)
}

func (st *DispatchSet) Resume(c Carried) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.resume(c)
}

// Burst is the pool the set's recent arrivals at the dispatcher ask for (State.Burst's rule).
func (st *DispatchSet) Burst(now time.Time, r PoolRule) int {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.burst(now, st.started, r)
}

// Running is a copy of the set's machines, for the spend.
func (st *DispatchSet) Running() []Machine {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := make([]Machine, 0, len(st.machines))
	for _, m := range st.machines {
		out = append(out, *m.Machine)
	}
	return out
}

// TakeCounts hands over what the set did since the last call, and TakeWaits its jobs' waits for a machine.
func (st *DispatchSet) TakeCounts() DispatchCounts {
	st.mu.Lock()
	defer st.mu.Unlock()
	c := st.counts
	st.counts = DispatchCounts{}
	return c
}

func (st *DispatchSet) TakeWaits() []float64 {
	st.mu.Lock()
	defer st.mu.Unlock()
	w := st.waits
	st.waits = nil
	return w
}

// DispatchSnapshot is a dispatch set as the admin endpoint and the metrics report it.
type DispatchSnapshot struct {
	Set         string  `json:"set"`
	Ready       bool    `json:"ready"`
	Pool        int     `json:"pool"`
	Max         int     `json:"max"`
	Capped      bool    `json:"capped"`
	Desired     int     `json:"desired"`
	Live        int     `json:"live"`
	Inflight    int     `json:"inflight"`
	Machines    int     `json:"machines"`
	Booting     int     `json:"booting"`
	Idle        int     `json:"idle"`
	Running     int     `json:"running"`
	Ending      int     `json:"ending"`
	JobsWaiting int     `json:"jobs_waiting"`
	OldestJobS  float64 `json:"oldest_job_s"`
	LaunchS     float64 `json:"launch_s"`
}

func (st *DispatchSet) Snapshot(now time.Time) DispatchSnapshot {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := DispatchSnapshot{Set: st.spec.Name, Ready: st.ready(), Pool: st.pool, Max: st.max, Capped: st.capped, Desired: st.desired(),
		Live: st.live(), Machines: len(st.machines), Idle: len(st.idle), JobsWaiting: len(st.queue), Running: len(st.pairs),
		LaunchS: st.launchTime(0).Seconds()}
	for _, k := range st.reserved {
		out.Inflight += k
	}
	for _, m := range st.machines {
		switch m.phase {
		case mBooting:
			out.Booting++
		case mEnding:
			out.Ending++
		}
	}
	if len(st.queue) > 0 {
		out.OldestJobS = now.Sub(st.queue[0].arrived).Seconds()
	}
	return out
}
