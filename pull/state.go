package main

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/actions/scaleset"
)

// A Machine is one EC2 instance of a scale set. EC2 (its tags) says it exists; everything else here is derived from
// EC2, GitHub and what the machine asked for, and is rebuilt by the reconciler after a restart.
type Machine struct {
	ID        string
	Set       string // its superci-pull-set tag
	Region    string
	IP        string
	Type      string
	AZ        string
	Lifecycle string // "spot" or "on-demand"
	Round     string // the launch round that started it (its superci-pull-round tag)
	Launched  time.Time
	Seen      time.Time // last listed by DescribeInstances
	Asked     time.Time // last asked for a runner (or adopted)
	Runner    *Runner   // the runner registered on it, while one is
	Ending    bool      // told to end, or terminated: no longer supply
	Ended     time.Time // when TerminateInstances was last asked for it
	Notice    bool      // AWS gave notice that it takes this spot machine back
}

// A Runner is one just-in-time registration, which takes one job.
type Runner struct {
	ID       int64
	Name     string
	Issued   time.Time
	Busy     bool // JobStarted seen, or GitHub lists it busy
	Done     bool // JobCompleted seen: the machine will ask again
	Listed   bool // in GitHub's last runner listing
	Removing bool
	Job      *JobRef // the job it took, from JobStarted
}

// JobRef names a job well enough to find it again in GitHub's REST API.
type JobRef struct {
	Owner, Repo string
	RunID       int64
	Name        string
	Runner      string
}

// State is everything the control loop knows about one scale set. One mutex: the listener goroutine, the launcher,
// the reconciler and the HTTP handlers each hold it only to read or write these fields, never across a network call.
type State struct {
	mu       sync.Mutex
	set      string
	stats    scaleset.RunnerScaleSetStatistic
	pool     int
	max      int
	capped   bool // the spend cap is reached: no launches, no pool
	known    bool // the message session has given its statistics since this process started
	machines map[string]*Machine
	reserved map[string]int // per launch round, capacity asked of EC2 and not yet seen
	rounds   int
	boot     string

	lastActivity time.Time // last message that assigned or started a job
	staleWaiting int       // waiting jobs the statistics still count though idle runners have not taken them
	polled       time.Time // last answer of the message session, a message or an empty poll

	listedAt     time.Time            // last complete listing of the scale set's runners
	surplusSince time.Time            // since when the supply has exceeded the demand
	orphanSince  map[string]time.Time // stale runners, by name, since first listed as such

	arrivals    []time.Time // jobs that became available, for the pool's rate
	waits       []float64   // queue waits (s) of jobs started since the metrics last took them
	odLaunched  []time.Time
	allLaunched []time.Time
}

func NewState(set string, pool, max int) *State {
	now := time.Now()
	return &State{set: set, pool: pool, max: max, machines: map[string]*Machine{}, reserved: map[string]int{},
		boot: fmt.Sprintf("%x", now.UnixNano()), lastActivity: now, polled: now, orphanSince: map[string]time.Time{}}
}

// desired is the supply GitHub's queue asks for: every job assigned to the scale set or waiting to be acquired by it,
// less waiting jobs found stale, plus the hot pool (none while capped), at most max.
func (s *State) desired() int {
	pool := s.pool
	if s.capped {
		pool = 0
	}
	return max(0, min(s.max, s.stats.TotalAssignedJobs-s.staleWaiting+s.stats.TotalAvailableJobs+pool))
}

// waitingJobs is the jobs the queue holds for the set that no runner has started.
func (s *State) waitingJobs() int {
	return s.stats.TotalAssignedJobs - s.stats.TotalRunningJobs - s.staleWaiting + s.stats.TotalAvailableJobs
}

func (s *State) idleRunners() int {
	n := 0
	for _, m := range s.machines {
		if !m.Ending && m.Runner != nil && !m.Runner.Busy {
			n++
		}
	}
	return n
}

// MarkStale finds jobs the statistics count as waiting that no idle runner has taken for a while. GitHub's counts
// can keep a job a reassignment left behind; supply kept for it would never be used.
func (s *State) MarkStale(now time.Time, after time.Duration) (stale int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	waiting := s.stats.TotalAssignedJobs - s.stats.TotalRunningJobs
	s.staleWaiting = 0
	if waiting > 0 && s.stats.TotalIdleRunners >= waiting && now.Sub(s.lastActivity) > after {
		s.staleWaiting = waiting
	}
	return s.staleWaiting
}

// live is the supply: machines not ending and not being taken back by AWS, and capacity asked for and not yet seen.
// A machine a listing shows before its launch call answers settles one unit of its round's reservation, so it is
// never counted twice.
func (s *State) live() int {
	n := 0
	for _, k := range s.reserved {
		n += k
	}
	for _, m := range s.machines {
		if !m.Ending && !m.Notice {
			n++
		}
	}
	return n
}

// Noticed takes in AWS's two minutes' notice for a spot machine: it is no longer supply, so another is launched at
// once. With no runner it ends now; with an idle runner it returns that runner, to be removed (fenced) before the
// machine ends; a busy runner keeps its machine until its job ends or AWS takes it.
func (s *State) Noticed(id string) (m *Machine, idle *Runner) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m = s.machines[id]
	if m == nil {
		return nil, nil
	}
	m.Notice = true
	switch r := m.Runner; {
	case r == nil:
		m.Ending = true
	case !r.Busy && !r.Removing && r.ID != 0:
		r.Removing = true
		return m, r
	}
	return m, nil
}

// Deficit reserves the capacity to launch now, under a new round's name. It launches no more machines than about
// limit can be waiting for their runner at once (a machine that waits is paid for and does nothing), and none while
// the spend cap holds.
func (s *State) Deficit(now time.Time, limit int) (round string, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.capped || !s.known {
		return "", 0
	}
	n = s.desired() - s.live()
	unregistered := 0
	for _, k := range s.reserved {
		unregistered += k
	}
	for _, m := range s.machines {
		if !m.Ending && m.Runner == nil {
			unregistered++
		}
	}
	n = min(n, limit-unregistered)
	if n <= 0 {
		return "", 0
	}
	s.rounds++
	round = fmt.Sprintf("%s-%d", s.boot, s.rounds)
	s.reserved[round] = n
	return round, n
}

// Launched ends a round's reservation: the machines EC2 started join the supply.
func (s *State) Launched(round string, got []*Machine, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.reserved, round)
	for _, m := range got {
		if _, ok := s.machines[m.ID]; !ok {
			s.machines[m.ID] = m
		}
		s.allLaunched = append(s.allLaunched, now)
		if m.Lifecycle == "on-demand" {
			s.odLaunched = append(s.odLaunched, now)
		}
	}
}

// OnDemandRoom is how many on-demand machines may start now without the on-demand share of the last half hour's
// launches passing frac.
func (s *State) OnDemandRoom(frac float64, more int, now time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	cut := now.Add(-30 * time.Minute)
	s.allLaunched = since(s.allLaunched, cut)
	s.odLaunched = since(s.odLaunched, cut)
	total := len(s.allLaunched) + more
	return max(0, int(frac*float64(total))-len(s.odLaunched))
}

func since(ts []time.Time, cut time.Time) []time.Time {
	i := 0
	for i < len(ts) && ts[i].Before(cut) {
		i++
	}
	return ts[i:]
}

// Verdict is the answer to a machine asking for a runner.
type Verdict int

const (
	Grant   Verdict = iota // register a runner for it
	End                    // it is not needed: it holds no runner, so it may end
	Wait                   // ask again shortly: the budget is spent, or its last runner may still hold a job
	Replace                // its last runner never started: remove that runner (fenced) before it gets another
	Unknown                // not one of ours
)

// Ask decides for a machine that asks for a runner. A machine runs one job: once a runner of it has taken a job, it
// gets no other (it ends), so no job inherits what the one before it left on the machine. It gets another runner
// only for one that never started (GitHub no longer lists it, or removed it on our request), and while its runner
// holds a job an ask (by anything on it that can read its identity) waits. A granted ask takes a registration from
// the budget, ahead of the pool's when a job waits. Until the
// message session has given the queue's statistics (after a restart, while a dead listener's session lapses), every
// machine is told to wait: nothing is known to end it for, or to register for.
func (s *State) Ask(id string, now time.Time, b *Budget, maxAge time.Duration) (Verdict, *Runner) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.machines[id]
	if m == nil {
		return Unknown, nil
	}
	m.Asked = now
	if r := m.Runner; r != nil {
		gone := !r.Listed && s.listedAt.Sub(r.Issued) > time.Minute
		switch {
		case r.Done || (r.Busy && gone):
			m.Ending = true
			return End, nil
		case r.Busy || r.Removing:
			return Wait, nil
		case !gone:
			r.Removing = true
			return Replace, r
		}
	}
	m.Runner = nil
	if !s.known && !m.Ending && !m.Notice {
		return Wait, nil
	}
	if m.Ending || m.Notice || now.Sub(m.Launched) > maxAge || s.live() > s.desired() {
		m.Ending = true
		return End, nil
	}
	prio := ForPool
	if s.waitingJobs() > s.idleRunners() {
		prio = ForJob
	}
	if !b.Take(now, prio) {
		return Wait, nil
	}
	return Grant, nil
}

// Replaced settles the removal of a machine's runner that never started: removed, the machine holds none; refused,
// it took a job meanwhile and is busy.
func (s *State) Replaced(m *Machine, r *Runner, removed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r.Removing = false
	if !removed {
		r.Busy = true
		return
	}
	if m.Runner == r {
		m.Runner = nil
	}
}

// Registered records the runner GitHub made for a machine.
func (s *State) Registered(id string, r *Runner) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m := s.machines[id]; m != nil {
		m.Runner = r
	}
}

// PreviousDone says whether the machine's last runner has been reported complete, so that the queue's statistics
// already count its job as gone when the machine's next ask is decided.
func (s *State) PreviousDone(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.machines[id]
	return m == nil || m.Runner == nil || m.Runner.Done || !m.Runner.Busy
}

// machineOf finds the machine a runner is named after: <set>-<instance id>-<suffix>.
func (s *State) machineOf(runner string) *Machine {
	rest, ok := strings.CutPrefix(runner, s.set+"-")
	if !ok || !strings.HasPrefix(rest, "i-") {
		return nil
	}
	if i := strings.LastIndexByte(rest, '-'); i > 0 {
		return s.machines[rest[:i]]
	}
	return nil
}

// Observe takes in one answer of the message session: its statistics, the jobs that arrived, and which runners
// started and finished jobs (with how long each started job waited in the queue).
func (s *State) Observe(msg *scaleset.RunnerScaleSetMessage, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.polled = now
	if msg == nil {
		return
	}
	if msg.Statistics != nil {
		s.stats = *msg.Statistics
		s.known = true
	}
	if len(msg.JobAvailableMessages)+len(msg.JobAssignedMessages)+len(msg.JobStartedMessages) > 0 {
		s.lastActivity = now
		s.staleWaiting = 0
	}
	for range msg.JobAvailableMessages {
		s.arrivals = append(s.arrivals, now)
	}
	for _, j := range msg.JobStartedMessages {
		if !j.QueueTime.IsZero() {
			started := now
			if !j.RunnerAssignTime.IsZero() && j.RunnerAssignTime.After(j.QueueTime) {
				started = j.RunnerAssignTime
			}
			s.waits = append(s.waits, max(0, started.Sub(j.QueueTime).Seconds()))
		}
		if m := s.machineOf(j.RunnerName); m != nil && m.Runner != nil && m.Runner.Name == j.RunnerName {
			m.Runner.Busy = true
			m.Runner.Job = &JobRef{Owner: j.OwnerName, Repo: j.RepositoryName, RunID: j.WorkflowRunID, Name: j.JobDisplayName, Runner: j.RunnerName}
		}
	}
	for _, j := range msg.JobCompletedMessages {
		if m := s.machineOf(j.RunnerName); m != nil && m.Runner != nil && m.Runner.Name == j.RunnerName {
			m.Runner.Busy, m.Runner.Done = true, true
		}
	}
}

// ArrivalRate is the jobs a second that became available over the last window.
func (s *State) ArrivalRate(now time.Time, window time.Duration) float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.arrivals = since(s.arrivals, now.Add(-window))
	return float64(len(s.arrivals)) / window.Seconds()
}

// TakeWaits hands over the queue waits recorded since the last call.
func (s *State) TakeWaits() []float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	w := s.waits
	s.waits = nil
	return w
}

// Seen merges one DescribeInstances listing of a region: machines it lists are live (a new one, met after a restart
// or launched by a call whose answer was lost, is adopted); machines it does not list, launched long enough ago for
// the listing to show them, are gone. It returns the jobs lost with them: a machine gone while its runner held a job
// not reported complete (AWS took it back mid-job).
func (s *State) Seen(region string, listed []*Machine, now time.Time) (lost []JobRef) {
	s.mu.Lock()
	defer s.mu.Unlock()
	in := map[string]bool{}
	for _, l := range listed {
		in[l.ID] = true
		m := s.machines[l.ID]
		if m == nil {
			l.Asked = now
			s.machines[l.ID] = l
			if s.reserved[l.Round] > 0 {
				s.reserved[l.Round]--
			}
			continue
		}
		m.IP, m.Type, m.AZ, m.Lifecycle, m.Seen = l.IP, l.Type, l.AZ, l.Lifecycle, now
		if m.Launched.IsZero() {
			m.Launched = l.Launched
		}
	}
	for id, m := range s.machines {
		if m.Region == region && !in[id] && now.Sub(m.Launched) > 90*time.Second {
			if r := m.Runner; r != nil && r.Busy && !r.Done && r.Job != nil {
				lost = append(lost, *r.Job)
			}
			delete(s.machines, id)
		}
	}
	return lost
}

// Listed merges GitHub's complete listing of the scale set's runners. A machine adopted after a restart learns its
// runner here. It returns the listed runners that belong to no live machine, or are not its current runner: stale
// registrations, to be removed (GitHub refuses to remove one that holds a job).
func (s *State) Listed(runners map[string]ListedRunner, now time.Time) (orphans []ListedRunner) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listedAt = now
	for _, m := range s.machines {
		if m.Runner != nil {
			m.Runner.Listed = false
		}
	}
	stale := map[string]bool{}
	for name, r := range runners {
		m := s.machineOf(name)
		switch {
		case m == nil || m.Ending:
			orphans = append(orphans, r)
		case m.Runner == nil:
			m.Runner = &Runner{ID: r.ID, Name: name, Issued: now, Busy: r.Busy, Listed: true}
		case m.Runner.Name == name:
			m.Runner.Listed = true
			m.Runner.Busy = m.Runner.Busy || r.Busy
		default:
			orphans = append(orphans, r)
		}
	}
	for _, o := range orphans {
		stale[o.Name] = true
		if _, ok := s.orphanSince[o.Name]; !ok {
			s.orphanSince[o.Name] = now
		}
	}
	for name := range s.orphanSince {
		if !stale[name] {
			delete(s.orphanSince, name)
		}
	}
	return orphans
}

// Orphans counts what should not be there: runners stale for over two minutes (removal keeps failing), and machines
// told to end over three minutes ago that EC2 still lists running.
func (s *State) Orphans(now time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, t := range s.orphanSince {
		if now.Sub(t) > 2*time.Minute {
			n++
		}
	}
	for _, m := range s.machines {
		if m.Ending && !m.Ended.IsZero() && now.Sub(m.Ended) > 3*time.Minute && m.Seen.After(m.Ended.Add(2*time.Minute)) {
			n++
		}
	}
	return n
}

// Stuck is the machines to end without a fence, because they hold no runner that could hold a job: older than the
// boot grace, they have not asked for a runner lately, and they have no runner, or one that a complete listing of
// GitHub's, made well after it was issued, no longer shows. A runner seen taking a job and not seen finishing it
// always keeps its machine.
func (s *State) Stuck(now time.Time, grace, quiet time.Duration) []*Machine {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Machine
	for _, m := range s.machines {
		if m.Ending || now.Sub(m.Launched) < grace || now.Sub(m.Asked) < quiet {
			continue
		}
		r := m.Runner
		if r == nil || (!(r.Busy && !r.Done) && !r.Listed && s.listedAt.Sub(r.Issued) > quiet) {
			m.Ending = true
			out = append(out, m)
		}
	}
	return out
}

// Surplus is the idle runners to remove once the supply has exceeded what the queue asks for for a while (the
// queue's statistics lag its messages by a moment). Each is fenced by GitHub, which refuses to remove a runner given
// a job meanwhile, before its machine ends.
func (s *State) Surplus(now time.Time, hold time.Duration, limit int) []*Machine {
	s.mu.Lock()
	defer s.mu.Unlock()
	excess := s.live() - s.desired()
	if excess <= 0 || !s.known {
		s.surplusSince = time.Time{}
		return nil
	}
	if s.surplusSince.IsZero() {
		s.surplusSince = now
	}
	if now.Sub(s.surplusSince) < hold {
		return nil
	}
	var out []*Machine
	for _, m := range s.machines {
		if len(out) >= min(excess, limit) {
			break
		}
		r := m.Runner
		if !m.Ending && r != nil && !r.Busy && !r.Removing && r.ID != 0 && now.Sub(r.Issued) > hold {
			r.Removing = true
			out = append(out, m)
		}
	}
	return out
}

// Removed settles a fenced removal: removed, the machine ends; refused because it took a job, it is busy.
func (s *State) Removed(m *Machine, removed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m.Runner != nil {
		m.Runner.Removing = false
		if !removed {
			m.Runner.Busy = true
		}
	}
	if removed {
		m.Ending = true
	}
}

// ToEnd is the ending machines to terminate now: each once, then again a minute on if EC2 still lists it.
func (s *State) ToEnd(now time.Time) []*Machine {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Machine
	for _, m := range s.machines {
		if m.Ending && now.Sub(m.Ended) > time.Minute {
			m.Ended = now
			out = append(out, m)
		}
	}
	return out
}

func (s *State) Machine(id string) *Machine {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.machines[id]
}

// Running is a copy of the machines EC2 lists running, for the spend.
func (s *State) Running() []Machine {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Machine, 0, len(s.machines))
	for _, m := range s.machines {
		out = append(out, *m)
	}
	return out
}

func (s *State) SetPool(n int)     { s.mu.Lock(); s.pool = n; s.mu.Unlock() }
func (s *State) SetMax(n int)      { s.mu.Lock(); s.max = n; s.mu.Unlock() }
func (s *State) SetCapped(on bool) { s.mu.Lock(); s.capped = on; s.mu.Unlock() }
func (s *State) Polled() time.Time { s.mu.Lock(); defer s.mu.Unlock(); return s.polled }

// Snapshot is the state as the admin endpoint and the logs report it.
type Snapshot struct {
	Set          string                           `json:"set"`
	Stats        scaleset.RunnerScaleSetStatistic `json:"stats"`
	Pool         int                              `json:"pool"`
	Max          int                              `json:"max"`
	Capped       bool                             `json:"capped"`
	Desired      int                              `json:"desired"`
	Live         int                              `json:"live"`
	Inflight     int                              `json:"inflight"`
	Machines     int                              `json:"machines"`
	Ending       int                              `json:"ending"`
	Booting      int                              `json:"booting"`
	Idle         int                              `json:"idle"`
	Busy         int                              `json:"busy"`
	OnDemand     int                              `json:"on_demand"`
	Notices      int                              `json:"notices"`
	StaleWaiting int                              `json:"stale_waiting"`
	Orphans      int                              `json:"orphans"`
}

func (s *State) Snapshot(now time.Time) Snapshot {
	orphans := s.Orphans(now)
	s.mu.Lock()
	defer s.mu.Unlock()
	inflight := 0
	for _, k := range s.reserved {
		inflight += k
	}
	out := Snapshot{Set: s.set, Stats: s.stats, Pool: s.pool, Max: s.max, Capped: s.capped, Desired: s.desired(), Live: s.live(),
		Inflight: inflight, Machines: len(s.machines), StaleWaiting: s.staleWaiting, Orphans: orphans}
	for _, m := range s.machines {
		switch {
		case m.Ending:
			out.Ending++
		case m.Runner == nil:
			out.Booting++
		case m.Runner.Busy:
			out.Busy++
		default:
			out.Idle++
		}
		if m.Lifecycle == "on-demand" {
			out.OnDemand++
		}
		if m.Notice {
			out.Notices++
		}
	}
	return out
}
