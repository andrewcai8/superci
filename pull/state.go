package main

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/actions/scaleset"
)

// A Machine is one EC2 instance of the scale set. EC2 (its tags) says it exists; everything else here is derived from
// EC2, GitHub and what the machine asked for, and is rebuilt by the reconciler after a restart.
type Machine struct {
	ID        string
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
}

// State is everything the control loop knows. One mutex: the listener goroutine, the launcher, the reconciler and the
// HTTP handlers each hold it only to read or write these fields, never across a network call.
type State struct {
	mu       sync.Mutex
	set      string
	stats    scaleset.RunnerScaleSetStatistic
	pool     int
	max      int
	machines map[string]*Machine
	reserved map[string]int // per launch round, capacity asked of EC2 and not yet seen
	rounds   int
	boot     string

	// Registrations. GitHub refuses past 1,500 runners configured in any 5 minutes, counting every registrant of the
	// organization, so the listener keeps its own budget under that, may pace itself further (rate, burst), and backs
	// off when GitHub refuses anyway.
	regs           []time.Time // registrations GitHub made, oldest first (sliding window)
	pending        int         // registrations asked of GitHub and not yet answered
	budget         int         // registrations allowed per window
	window         time.Duration
	rate, burst    float64 // pacing: tokens a second and most saved up; rate 0 is unpaced
	tokens         float64
	refilled       time.Time
	throttledUntil time.Time
	throttleWait   time.Duration

	lastActivity time.Time // last message that assigned or started a job
	staleWaiting int       // waiting jobs the statistics still count though idle runners have not taken them

	listedAt     time.Time // last complete listing of the scale set's runners
	surplusSince time.Time // since when the supply has exceeded the demand

	totalRegs    int
	regErrors    int
	refused      int
	maxRegWindow int
	odLaunched   []time.Time
	allLaunched  []time.Time
}

func NewState(set string, pool, max, budget int) *State {
	now := time.Now()
	return &State{set: set, pool: pool, max: max, budget: budget, window: 5 * time.Minute, machines: map[string]*Machine{},
		reserved: map[string]int{}, boot: fmt.Sprintf("%x", now.UnixNano()), lastActivity: now}
}

// Pace limits registrations to rate a second, with up to burst saved up.
func (s *State) Pace(rate, burst float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rate, s.burst, s.tokens = rate, burst, burst
}

// desired is the supply GitHub's queue asks for: every job assigned to the scale set or waiting to be acquired by it,
// less waiting jobs found stale, plus the hot pool, at most max.
func (s *State) desired() int {
	return min(s.max, s.stats.TotalAssignedJobs-s.staleWaiting+s.stats.TotalAvailableJobs+s.pool)
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

// live is the supply: machines not ending, and capacity asked for and not yet seen. A machine a listing shows before
// its launch call answers settles one unit of its round's reservation, so it is never counted twice.
func (s *State) live() int {
	n := 0
	for _, k := range s.reserved {
		n += k
	}
	for _, m := range s.machines {
		if !m.Ending {
			n++
		}
	}
	return n
}

// Deficit reserves the capacity to launch now, under a new round's name. While registrations are paced, or GitHub
// lately refused one, it launches no more machines than can be registered in about a minute and a half: a machine
// that waits for its runner is paid for and does nothing.
func (s *State) Deficit(now time.Time) (round string, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n = s.desired() - s.live()
	limit := s.max
	if s.rate > 0 {
		limit = max(25, int(s.rate*90))
	}
	if now.Before(s.throttledUntil.Add(time.Minute)) {
		limit = min(limit, 50)
	}
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
	Wait                   // the registration budget is spent: ask again shortly
	Unknown                // not one of ours
)

// Ask decides for a machine that asks for a runner (it holds none: its last one, if any, took its job and ended). It
// grants while the supply is at most what the queue asks for, and keeps the registration budget: at most budget
// registrations in any window.
func (s *State) Ask(id string, now time.Time) Verdict {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.machines[id]
	if m == nil {
		return Unknown
	}
	m.Asked = now
	m.Runner = nil
	if m.Ending {
		return End
	}
	if s.live() > s.desired() {
		m.Ending = true
		return End
	}
	if now.Before(s.throttledUntil) {
		return Wait
	}
	s.regs = since(s.regs, now.Add(-s.window))
	if len(s.regs)+s.pending >= s.budget {
		return Wait
	}
	if s.rate > 0 {
		s.tokens = min(s.burst, s.tokens+now.Sub(s.refilled).Seconds()*s.rate)
		s.refilled = now
		if s.tokens < 1 {
			return Wait
		}
		s.tokens--
	}
	s.pending++
	return Grant
}

// Outcome is what GitHub answered to a registration.
type Outcome int

const (
	Made    Outcome = iota
	Refused         // GitHub's limit on runners configured in 5 minutes
	Failed
)

// Registered records what GitHub answered: the runner it made for a machine, or a refusal, after which no one asks
// GitHub for a while (5 s, doubling to a minute while it keeps refusing).
func (s *State) Registered(id string, r *Runner, outcome Outcome, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending--
	switch outcome {
	case Made:
		s.regs = append(since(s.regs, now.Add(-s.window)), now)
		s.totalRegs++
		s.maxRegWindow = max(s.maxRegWindow, len(s.regs))
		s.throttleWait = 0
		if m := s.machines[id]; m != nil {
			m.Runner = r
		}
	case Refused:
		s.refused++
		s.throttleWait = min(time.Minute, max(5*time.Second, 2*s.throttleWait))
		s.throttledUntil = now.Add(s.throttleWait)
	case Failed:
		s.regErrors++
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
	if !ok {
		return nil
	}
	if i := strings.LastIndexByte(rest, '-'); i > 0 {
		return s.machines[rest[:i]]
	}
	return nil
}

// Observe takes in one message of the queue: its statistics, and which runners started and finished jobs.
func (s *State) Observe(msg *scaleset.RunnerScaleSetMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if msg.Statistics != nil {
		s.stats = *msg.Statistics
	}
	if len(msg.JobAvailableMessages)+len(msg.JobAssignedMessages)+len(msg.JobStartedMessages) > 0 {
		s.lastActivity = time.Now()
		s.staleWaiting = 0
	}
	for _, j := range msg.JobStartedMessages {
		if m := s.machineOf(j.RunnerName); m != nil && m.Runner != nil && m.Runner.Name == j.RunnerName {
			m.Runner.Busy = true
		}
	}
	for _, j := range msg.JobCompletedMessages {
		if m := s.machineOf(j.RunnerName); m != nil && m.Runner != nil && m.Runner.Name == j.RunnerName {
			m.Runner.Busy, m.Runner.Done = true, true
		}
	}
}

// Seen merges one DescribeInstances listing of a region: machines it lists are live (a new one, met after a restart
// or launched by a call whose answer was lost, is adopted); machines it does not list, launched long enough ago for
// the listing to show them, are gone.
func (s *State) Seen(region string, listed []*Machine, now time.Time) {
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
			delete(s.machines, id)
		}
	}
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
	return orphans
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
	if excess <= 0 {
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

func (s *State) SetPool(n int) { s.mu.Lock(); s.pool = n; s.mu.Unlock() }
func (s *State) SetMax(n int)  { s.mu.Lock(); s.max = n; s.mu.Unlock() }

// Snapshot is the state as the admin endpoint and the logs report it.
type Snapshot struct {
	Stats        scaleset.RunnerScaleSetStatistic `json:"stats"`
	Pool         int                              `json:"pool"`
	Max          int                              `json:"max"`
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
	RegsWindow   int                              `json:"regs_window"`
	MaxRegWindow int                              `json:"max_regs_window"`
	TotalRegs    int                              `json:"total_regs"`
	RegErrors    int                              `json:"reg_errors"`
	Refused      int                              `json:"refused"`
	Pending      int                              `json:"pending"`
	StaleWaiting int                              `json:"stale_waiting"`
}

func (s *State) Snapshot(now time.Time) Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.regs = since(s.regs, now.Add(-s.window))
	inflight := 0
	for _, k := range s.reserved {
		inflight += k
	}
	out := Snapshot{Stats: s.stats, Pool: s.pool, Max: s.max, Desired: s.desired(), Live: s.live(), Inflight: inflight,
		Machines: len(s.machines), RegsWindow: len(s.regs), MaxRegWindow: s.maxRegWindow, TotalRegs: s.totalRegs, RegErrors: s.regErrors,
		Refused: s.refused, Pending: s.pending, StaleWaiting: s.staleWaiting}
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
