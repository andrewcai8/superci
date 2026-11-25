package main

import (
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/actions/scaleset"
)

// A Machine is one EC2 instance of a scale set. EC2 (its tags) says it exists and whether it was ever issued a runner;
// everything else here is derived from EC2, GitHub and what the machine asked for, and is rebuilt after a restart.
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
	Ordered   time.Time // when this process asked EC2 for it; zero for a machine adopted after a start
	Seen      time.Time // last listed by DescribeInstances
	Asked     time.Time // last asked for a runner (or adopted)
	FirstAsk  time.Time // first asked for a runner, by this process
	Asks      int       // asks for a runner, by this process
	// Issued says a runner was issued to it. A machine gets one runner, ever. The fence is its claim (Claims), made
	// before its runner is registered; EC2's tag superci-pull-issued, written before the config leaves, is a hint a
	// restart reads back.
	Issued   bool
	Claim    string  // the token of its grant's claim, kept while that grant may still go on; spent once it ends
	Tagged   bool    // EC2 has the issued tag (it listed it, or accepted our write)
	Issuing  bool    // its registration is in flight
	Runner   *Runner // its runner, once known
	Retire   bool    // to end once its idle runner is removed (fenced by GitHub): surplus, a second ask, past its age
	Checking bool    // GitHub is being asked whether its runner still exists
	Ending   bool    // told to end, or terminated: no longer supply
	Ended    time.Time
	Notice   bool // AWS gave notice that it takes this spot machine back
	Missed   int  // complete listings in a row that showed no runner of it while it was never issued one
}

// A Runner is one just-in-time registration, which takes one job.
type Runner struct {
	ID       int64
	Name     string
	Issued   time.Time
	Started  time.Time // when its job started (JobStarted); zero when unknown
	Busy     bool      // JobStarted seen, or GitHub lists it busy
	Done     bool      // JobCompleted seen
	Listed   bool      // in GitHub's last runner listing, or found by a check
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

// pollLease is how long the queue's statistics stay GitHub's word after the message session last answered: twice
// its long poll (50 s). Past it the session is lost, and nothing is launched or retired on what the statistics said.
const pollLease = 100 * time.Second

// State is everything the control loop knows about one scale set. One mutex: the listener goroutine, the launcher,
// the reconciler and the HTTP handlers each hold it only to read or write these fields, never across a network call.
type State struct {
	mu         sync.Mutex
	set        string
	started    time.Time
	stats      scaleset.RunnerScaleSetStatistic
	pool       int
	max        int
	capped     bool                 // the spend cap is reached: no launches, no pool
	known      bool                 // the message session has given its statistics since this process started
	emptySince time.Time            // when a retired set was first seen holding nothing (Drained)
	listings   map[string]time.Time // per configured region, when its last complete listing of machines began
	machines   map[string]*Machine
	gone       map[string]time.Time // machines issued (or being granted) that a listing dropped, so a re-adoption stays issued
	reserved   map[string]int       // per launch round, capacity asked of EC2 and not yet seen
	rounds     int
	boot       string

	lastActivity time.Time // last message that assigned or started a job
	lastStart    time.Time // last job started
	waitingSince time.Time // since when jobs have waited without a break
	staleWaiting int       // waiting jobs the statistics still count though idle runners have not taken them
	polled       time.Time // last answer of the message session, a message or an empty poll

	listedAt     time.Time            // last complete listing of the scale set's runners
	surplusSince time.Time            // since when the supply has exceeded the demand
	orphanSince  map[string]time.Time // stale runners, by name, since first listed as such

	arrivals    []time.Time          // jobs that arrived, for the pool's rate
	available   map[string]time.Time // when each waiting job was queued, by jobKey, for its queue wait
	launches    []time.Duration      // launch call to first ask, of the set's last machines
	held        []heldPool           // the bursts of the last hold, newest last
	carried     int                  // the burst the last listener saw, held for the first window after a start
	lost        []JobRef             // jobs that ended on a machine AWS was taking back, for a re-run
	waits       []float64            // queue waits (s) of jobs started since the metrics last took them
	odLaunched  []time.Time
	allLaunched []time.Time
}

func NewState(set string, pool, max int, regions []string, now time.Time) *State {
	s := &State{set: set, pool: pool, max: max, started: now, machines: map[string]*Machine{}, gone: map[string]time.Time{}, reserved: map[string]int{},
		boot: fmt.Sprintf("%x", now.UnixNano()), lastActivity: now, lastStart: now, polled: now, orphanSince: map[string]time.Time{},
		available: map[string]time.Time{}, listings: map[string]time.Time{}}
	for _, r := range regions {
		s.listings[r] = time.Time{}
	}
	return s
}

// ready says whether the set may decide anything: the queue is known and every region has been listed. Before then a
// region not yet listed reads as no machines, which would launch all its demand again and make every runner of it an
// orphan.
func (s *State) ready() bool {
	if !s.known {
		return false
	}
	for _, at := range s.listings {
		if at.IsZero() {
			return false
		}
	}
	return true
}

// leased says whether the message session answered within pollLease, so the statistics are current.
func (s *State) leased(now time.Time) bool { return now.Sub(s.polled) <= pollLease }

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
	_, idle := s.supplyCounts()
	return idle
}

// supplyCounts is live and idleRunners in one pass over the machines: every machine waiting for its runner asks every
// 3 s, and each ask needs both.
func (s *State) supplyCounts() (live, idle int) {
	for _, k := range s.reserved {
		live += k
	}
	for _, m := range s.machines {
		if supply(m) {
			live++
			if m.Runner != nil && !m.Runner.Busy {
				idle++
			}
		}
	}
	return live, idle
}

// supply says whether a machine counts toward the set's supply: not ending, not being taken back by AWS, and not
// retiring.
func supply(m *Machine) bool { return !m.Ending && !m.Notice && !m.Retire }

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

// live is the supply: machines that count (supply), and capacity asked for and not yet seen. A machine a listing shows
// before its launch call answers settles one unit of its round's reservation, so it is never counted twice.
func (s *State) live() int {
	live, _ := s.supplyCounts()
	return live
}

// Noticed takes in AWS's two minutes' notice for a spot machine: it is no longer supply, so another is launched at
// once. With no runner it ends now; with an idle runner it retires (the runner removed before the machine ends); a
// busy runner keeps its machine until its job ends or AWS takes it. It returns the job the machine runs, if known.
func (s *State) Noticed(id string) *JobRef {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.machines[id]
	if m == nil {
		return nil
	}
	m.Notice = true
	switch r := m.Runner; {
	case r == nil && !m.Issuing && !m.Issued:
		m.Ending = true
	case r != nil && !r.Busy:
		m.Retire = true
	case r != nil && r.Job != nil:
		j := *r.Job
		return &j
	}
	return nil
}

// Deficit reserves the capacity to launch now, under a new round's name. It launches no more than leaves limit of
// the set's machines waiting for their runner at once (a machine that waits is paid for and does nothing), and none
// while the spend cap holds or before the set is ready.
func (s *State) Deficit(now time.Time, limit int) (round string, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.capped || !s.ready() || !s.leased(now) {
		return "", 0
	}
	n = min(s.desired()-s.live(), limit-s.unregistered())
	if n <= 0 {
		return "", 0
	}
	s.rounds++
	round = fmt.Sprintf("%s-%d", s.boot, s.rounds)
	s.reserved[round] = n
	return round, n
}

// unregistered is the machines asked for or running that wait for their runner: never issued one, or one in flight.
func (s *State) unregistered() int {
	n := 0
	for _, k := range s.reserved {
		n += k
	}
	for _, m := range s.machines {
		if !m.Ending && !m.Issued && m.Runner == nil {
			n++
		}
	}
	return n
}

func (s *State) Unregistered() int { s.mu.Lock(); defer s.mu.Unlock(); return s.unregistered() }

// Launched ends a round's reservation: the machines EC2 started join the supply.
func (s *State) Launched(round string, got []*Machine, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.reserved, round)
	for _, m := range got {
		if _, ok := s.machines[m.ID]; !ok {
			m.Ordered = now
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
	Grant   Verdict = iota // register its one runner
	End                    // it gets no runner: power off (whether the listener ends it is decided by proof)
	Wait                   // ask again shortly: the budget is spent, or the set is not ready
	Unknown                // not one of ours
)

// Ask decides for a machine that asks for a runner. A machine gets one runner, ever, so no job inherits what another
// left on it and no job can take a second runner: once one is issued, every ask is answered End, whoever on the
// machine asks. The ask is not proof of anything except that the asker does not run the runner, so an idle runner is
// removed (fenced by GitHub) and its machine ends; a runner that took its job ends its machine when the job completes.
// A machine this process knows of no runner for is granted one once the set is ready, inside the registration budget
// (ahead of the pool's when a job waits); the grant goes on only if it wins the machine's claim (Claims).
func (s *State) Ask(id string, now time.Time, b *Budget, maxAge time.Duration) Verdict {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.machines[id]
	if m == nil {
		return Unknown
	}
	m.Asked = now
	if m.FirstAsk.IsZero() {
		m.FirstAsk = now
	}
	m.Asks++
	switch r := m.Runner; {
	case m.Issuing:
		return Wait
	case r != nil && r.Done:
		m.Ending = true
		return End
	case r != nil && !r.Busy:
		m.Retire = true
		return End
	case m.Issued || r != nil:
		return End
	case m.Ending || m.Notice:
		m.Ending = true
		return End
	case !s.ready():
		return Wait
	}
	live, idle := s.supplyCounts()
	switch {
	case now.Sub(m.Launched) > maxAge || s.leased(now) && live > s.desired():
		m.Ending = true
		return End
	}
	prio := ForPool
	if s.waitingJobs() > idle {
		prio = ForJob
	}
	if !b.Take(now, prio) {
		return Wait
	}
	m.Issuing = true
	return Grant
}

// Claim is the token of a granted machine's claim: the one its grant made, when it made one, or a new one.
func (s *State) Claim(m *Machine) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m.Claim == "" {
		m.Claim = newToken()
	}
	return m.Claim
}

// IssuedElsewhere settles a grant whose claim another grant holds (another process's, or one this process forgot):
// the machine was issued its runner, so it gets none, and its end waits for proof (GitHub's word on its runner).
func (s *State) IssuedElsewhere(m *Machine) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m.Issuing, m.Issued, m.Claim = false, true, ""
}

// Issue records the runner GitHub made for a machine once EC2 has its issued tag: the runner's config may leave now.
func (s *State) Issue(m *Machine, r *Runner) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m.Issuing, m.Issued, m.Tagged, m.Runner, m.Claim = false, true, true, r, ""
	if !m.Ordered.IsZero() {
		ready := m.FirstAsk
		if ready.IsZero() {
			ready = r.Issued
		}
		s.launches = append(s.launches, ready.Sub(m.Ordered))
		s.launches = s.launches[max(0, len(s.launches)-launchSamples):]
	}
	if m.Notice {
		m.Retire = true
	}
}

// Asking is when a machine first asked for its runner and how many times it has asked, for the register event.
func (s *State) Asking(m *Machine) (first time.Time, asks int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return m.FirstAsk, m.Asks
}

// Unissued settles a grant GitHub registered nothing for: the machine may ask again.
func (s *State) Unissued(m *Machine) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m.Issuing = false
}

// Withheld settles a registration whose config never left: its tag could not be written, or GitHub already holds a
// runner of the machine's name that was never handed out. The machine gets none, ever, so it ends, after the runner,
// when known, is removed.
func (s *State) Withheld(m *Machine, r *Runner) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m.Issuing, m.Issued, m.Claim = false, true, ""
	if r == nil {
		m.Ending = true
		return
	}
	m.Runner, m.Retire = r, true
}

// runnerName is the name of a machine's one runner: <set>-<instance id>.
func runnerName(set, instance string) string { return set + "-" + instance }

// machineOf finds the machine a runner is named after: <set>-<instance id>, or <set>-<instance id>-<suffix> as an
// earlier build named them.
func (s *State) machineOf(runner string) *Machine {
	rest, ok := strings.CutPrefix(runner, s.set+"-")
	if !ok || !strings.HasPrefix(rest, "i-") {
		return nil
	}
	id, _, _ := strings.Cut(rest[2:], "-")
	return s.machines["i-"+id]
}

// runnerOf is the runner of a machine a job message names, adopted when the machine's runner is not known yet (after a
// restart). Any runner named after a machine is its one runner, so the machine is issued.
func runnerOf(m *Machine, name string, id int64, now time.Time) *Runner {
	m.Issued = true
	if m.Runner == nil {
		m.Runner = &Runner{ID: id, Name: name, Issued: now, Listed: true}
	}
	return m.Runner
}

// Observe takes in one answer of the message session: its statistics, the jobs that arrived, and which runners
// started and finished jobs (with how long each started job waited in the queue). A job arrives with its first
// JobAvailable or JobAssigned: GitHub assigns these sets' jobs directly, with no JobAvailable. A machine whose job
// completed ends: the listener ends it, rather than trust what runs on it to.
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
	for _, j := range msg.JobAvailableMessages {
		s.arrived(j.JobMessageBase, now)
	}
	for _, j := range msg.JobAssignedMessages {
		s.arrived(j.JobMessageBase, now)
	}
	for _, j := range msg.JobAssignedMessages {
		arrive(j.JobMessageBase)
	}
	for _, j := range msg.JobStartedMessages {
		queued, ok := s.available[jobKey(j.JobMessageBase)]
		if !j.QueueTime.IsZero() {
			queued, ok = j.QueueTime, true
		}
		if ok {
			s.waits = append(s.waits, max(0, now.Sub(queued).Seconds()))
		}
		delete(s.available, jobKey(j.JobMessageBase))
		s.lastStart = now
		if m := s.machineOf(j.RunnerName); m != nil {
			if r := runnerOf(m, j.RunnerName, int64(j.RunnerID), now); r.Name == j.RunnerName {
				r.Busy, m.Retire = true, false
				if r.Started.IsZero() {
					r.Started = now
				}
				r.Job = &JobRef{Owner: j.OwnerName, Repo: j.RepositoryName, RunID: j.WorkflowRunID, Name: j.JobDisplayName, Runner: j.RunnerName}
			}
		}
	}
	for _, j := range msg.JobCompletedMessages {
		delete(s.available, jobKey(j.JobMessageBase))
		if m := s.machineOf(j.RunnerName); m != nil {
			r := runnerOf(m, j.RunnerName, int64(j.RunnerID), now)
			r.Busy, r.Done = true, true
			m.Ending = true
			if m.Notice && r.Job != nil {
				s.lost = append(s.lost, *r.Job)
			}
		}
	}
	if s.waitingJobs() > 0 {
		if s.waitingSince.IsZero() {
			s.waitingSince = now
		}
	} else {
		s.waitingSince = time.Time{}
	}
}

// arrived takes in a job the first time a message names it: one arrival, dated when GitHub assigned it to the set
// (so a backlog redelivered after a restart keeps its own times), and queued since GitHub queued it or, as a
// JobAssigned and a JobStarted carry no queue time, since that assignment.
func (s *State) arrived(j scaleset.JobMessageBase, now time.Time) {
	key := jobKey(j)
	if _, ok := s.available[key]; ok && key != "" {
		return
	}
	at := now
	if t := j.ScaleSetAssignTime; !t.IsZero() && t.Before(now) {
		at = t
	}
	s.arrivals = append(s.arrivals, at)
	for i := len(s.arrivals) - 1; i > 0 && s.arrivals[i].Before(s.arrivals[i-1]); i-- {
		s.arrivals[i], s.arrivals[i-1] = s.arrivals[i-1], s.arrivals[i]
	}
	queued := at
	if !j.QueueTime.IsZero() && j.QueueTime.Before(now) {
		queued = j.QueueTime
	}
	if key != "" {
		s.available[key] = queued
	}
}

// jobKey names a job across its messages: its job id. A JobAssigned carries no runner request id (0 in every one of
// them on Nov 25 2026), so keyed by that, every job after the first read as one already seen. A message without a job
// id falls back to the runner request id; with neither, the job has no key ("") and counts without being matched.
func jobKey(j scaleset.JobMessageBase) string {
	switch {
	case j.JobID != "":
		return j.JobID
	case j.RunnerRequestID != 0:
		return "request:" + strconv.FormatInt(j.RunnerRequestID, 10)
	}
	return ""
}

// Stalled says whether jobs have waited for the set for over d with none started meanwhile.
func (s *State) Stalled(now time.Time, d time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.waitingJobs() <= 0 || s.waitingSince.IsZero() {
		return false
	}
	return now.Sub(s.waitingSince) > d && now.Sub(s.lastStart) > d
}

// TakeLost hands over the jobs that completed on a machine AWS gave notice it takes back (whether each failed is
// GitHub's to say when the re-run is decided).
func (s *State) TakeLost() []JobRef {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.lost
	s.lost = nil
	return l
}

// launchSamples is how many of the set's last launches its launch time is measured over.
const launchSamples = 64

// heldPool is a burst the set saw, kept for the hold.
type heldPool struct {
	at time.Time
	n  int
}

// launchTime is how long a launch takes to bring a machine that can take a runner: the 90th percentile of the set's
// last launches, from the launch call to the machine's first ask (its boot), or def until it has eight. The wait for
// the registration budget is left out: it is a rate the pool cannot buy ahead, and counting it fed back (a longer
// launch time sized a larger pool, whose registrations lengthened the wait; 244 s measured on a dev set).
func (s *State) launchTime(def time.Duration) time.Duration {
	if len(s.launches) < 8 {
		return def
	}
	xs := slices.Clone(s.launches)
	slices.Sort(xs)
	return xs[len(xs)*9/10]
}

// Burst is the pool the set's recent jobs ask for. A job finds a ready runner when fewer jobs than the pool arrived in
// the launch time up to and including it: each job takes a runner and launches the next, which is ready one launch
// time later. Burst is that count at the quantile q of the jobs seen in the last window, and no less than the largest
// burst of the last hold, so the pool shrinks only once bursts have stayed smaller for a while (a pool runner retired
// is a registration and a boot spent for nothing). Without arrivals it is 0, and the hour's floor holds.
func (s *State) Burst(now time.Time, r PoolRule) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.arrivals = since(s.arrivals, now.Add(-r.window()))
	for id, t := range s.available {
		if now.Sub(t) > 7*time.Hour {
			delete(s.available, id)
		}
	}
	launch := s.launchTime(r.ready())
	counts := make([]int, len(s.arrivals))
	lo := 0
	for i, a := range s.arrivals {
		for lo < i && !s.arrivals[lo].After(a.Add(-launch)) {
			lo++
		}
		counts[i] = i - lo + 1
	}
	n := 0
	if len(counts) > 0 {
		slices.Sort(counts)
		n = counts[min(len(counts)-1, int(r.Quantile*float64(len(counts))))]
	}
	s.held = append(s.held, heldPool{now, n})
	i := 0
	for i < len(s.held) && now.Sub(s.held[i].at) > r.hold() {
		i++
	}
	s.held = s.held[i:]
	for _, h := range s.held {
		n = max(n, h.n)
	}
	if now.Sub(s.started) < r.window() {
		n = max(n, s.carried)
	}
	return n
}

// Carried is what a set's pool keeps across a restart: the burst it was sized for and its launch times.
type Carried struct {
	Burst    int     `json:"burst"`
	Launches []int64 `json:"launches_ms"`
}

// Carry is the set's pool as the next listener should start from it.
func (s *State) Carry() Carried {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := Carried{Launches: make([]int64, len(s.launches))}
	for i, d := range s.launches {
		c.Launches[i] = d.Milliseconds()
	}
	for _, h := range s.held {
		c.Burst = max(c.Burst, h.n)
	}
	return c
}

// Resume starts the set from what the last listener carried: its launch times, and its burst for the first window,
// so a restart does not drop the pool to the floor while the set sees its jobs again.
func (s *State) Resume(c Carried) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.carried = c.Burst
	s.launches = s.launches[:0]
	for _, ms := range c.Launches[max(0, len(c.Launches)-launchSamples):] {
		s.launches = append(s.launches, time.Duration(ms)*time.Millisecond)
	}
}

// TakeWaits hands over the queue waits recorded since the last call.
func (s *State) TakeWaits() []float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	w := s.waits
	s.waits = nil
	return w
}

// Seen merges one complete DescribeInstances listing of a region, begun at began: machines it lists are live (a new
// one, met after a restart or launched by a call whose answer was lost, is adopted, with its issued tag); machines it
// does not list, launched long enough ago for the listing to show them, are gone. A gone machine that was issued a
// runner, or was being granted one, stays issued for a day, should a later listing show it again. It returns the
// jobs lost with them: a machine gone while its runner held a job not reported complete (it died mid-job, before
// GitHub noticed).
func (s *State) Seen(region string, listed []*Machine, began, now time.Time) (lost []JobRef) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listings[region] = began
	in := map[string]bool{}
	for _, l := range listed {
		in[l.ID] = true
		m := s.machines[l.ID]
		if m == nil {
			l.Asked = now
			if _, ok := s.gone[l.ID]; ok {
				l.Issued = true
			}
			s.machines[l.ID] = l
			if s.reserved[l.Round] > 0 {
				s.reserved[l.Round]--
			}
			continue
		}
		m.IP, m.Type, m.AZ, m.Lifecycle, m.Seen = l.IP, l.Type, l.AZ, l.Lifecycle, now
		m.Issued, m.Tagged, m.Notice = m.Issued || l.Issued, m.Tagged || l.Tagged, m.Notice || l.Notice
		if m.Launched.IsZero() {
			m.Launched = l.Launched
		}
	}
	for id, m := range s.machines {
		if m.Region == region && !in[id] && now.Sub(m.Launched) > 90*time.Second {
			if r := m.Runner; r != nil && r.Busy && !r.Done && r.Job != nil {
				lost = append(lost, *r.Job)
			}
			if m.Issued || m.Issuing {
				s.gone[id] = now
			}
			delete(s.machines, id)
		}
	}
	for id, at := range s.gone {
		if now.Sub(at) > 24*time.Hour {
			delete(s.gone, id)
		}
	}
	return lost
}

// Listed merges GitHub's complete listing of the scale set's runners. A machine adopted after a restart learns its
// runner here (and a machine with a runner is issued, whatever its tags say). It returns the listed runners that
// belong to no live machine, or are not its runner: stale registrations, to be removed (GitHub refuses to remove one
// that holds a job). Until the set is ready it returns none: a runner of a region not yet listed is no orphan.
func (s *State) Listed(runners map[string]ListedRunner, now time.Time) (orphans []ListedRunner) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listedAt = now
	for _, m := range s.machines {
		if m.Runner != nil {
			m.Runner.Listed = false
		}
	}
	held := map[*Machine]bool{}
	for name, r := range runners {
		m := s.machineOf(name)
		switch {
		case m == nil || m.Ending:
			orphans = append(orphans, r)
		case m.Runner == nil:
			m.Runner = &Runner{ID: r.ID, Name: name, Issued: now, Busy: r.Busy, Listed: true}
			m.Issued = true
			held[m] = true
		case m.Runner.Name == name:
			m.Runner.Listed = true
			m.Runner.Busy = m.Runner.Busy || r.Busy
			if m.Runner.ID == 0 {
				m.Runner.ID = r.ID
			}
			held[m] = true
		default:
			orphans = append(orphans, r)
		}
	}
	for _, m := range s.machines {
		switch {
		case held[m] || m.Runner != nil || m.Issued:
			m.Missed = 0
		case now.Sub(m.Launched) > time.Minute:
			m.Missed++
		}
	}
	if !s.ready() {
		return nil
	}
	stale := map[string]bool{}
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

// Stuck finds machines to end, and machines whose runner to check with GitHub first. A machine never issued a runner
// that has not asked lately, of which three complete listings in a row showed none, ends. An issued machine whose
// runner a listing made well after it was issued did not show (or, after a restart, whose runner no listing of this
// process showed) is returned to be checked (Checked): its end needs GitHub's word that the runner is gone. Asking
// keeps none of them, since anything on a machine can ask.
func (s *State) Stuck(now time.Time, grace, quiet time.Duration) (ended, unlisted []*Machine) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ready() {
		return nil, nil
	}
	for _, m := range s.machines {
		if m.Ending || m.Issuing || m.Checking || now.Sub(m.Launched) < grace {
			continue
		}
		switch r := m.Runner; {
		case !m.Issued && r == nil:
			if m.Missed >= 3 && now.Sub(m.Asked) >= quiet {
				m.Ending = true
				ended = append(ended, m)
			}
		case r != nil && r.Done:
			m.Ending = true
			ended = append(ended, m)
		case r == nil:
			if s.listedAt.After(s.started) {
				m.Checking = true
				unlisted = append(unlisted, m)
			}
		case !r.Listed && s.listedAt.Sub(r.Issued) > quiet:
			m.Checking = true
			unlisted = append(unlisted, m)
		}
	}
	return ended, unlisted
}

// Check is GitHub's answer about a runner a listing did not show.
type Check int

const (
	Present Check = iota // GitHub has it: the listing missed it
	Absent               // GitHub has no runner of the machine: it holds no job and gets none
	Unsure               // the check failed: ask again on a later pass
)

// Checked settles a check: absent, the machine ends; present, its runner is kept (and learned, when found by name).
func (s *State) Checked(m *Machine, c Check, found *Runner) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m.Checking = false
	switch c {
	case Absent:
		m.Ending = true
	case Present:
		if m.Runner == nil && found != nil {
			m.Runner = found
		}
		if m.Runner != nil {
			m.Runner.Listed = true
		}
	}
}

// Overdue ends what has outlived its bounds, whatever runs on the machine (a job on it may stop the machine's own
// timers): a job running past jobMax since it started, a machine never issued a runner or holding an idle one past
// idleMax (the idle runner removed first), and any machine past lifeMax.
func (s *State) Overdue(now time.Time, jobMax, idleMax, lifeMax time.Duration) (ended []*Machine) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ready() {
		return nil
	}
	for _, m := range s.machines {
		if m.Ending || m.Issuing {
			continue
		}
		age := now.Sub(m.Launched)
		switch r := m.Runner; {
		case age > lifeMax, r != nil && r.Busy && !r.Started.IsZero() && now.Sub(r.Started) > jobMax,
			r == nil && !m.Issued && age > idleMax:
			m.Ending = true
			ended = append(ended, m)
		case r != nil && !r.Busy && age > idleMax:
			m.Retire = true
		}
	}
	return ended
}

// Surplus retires idle runners once the supply has exceeded what the queue asks for for a while (the queue's
// statistics lag its messages by a moment), and not in the first warmup after a start, while the arrival rate (and so
// the pool) is still being measured. It returns how many it retired; each is removed (ToFence) before its machine ends.
func (s *State) Surplus(now time.Time, hold, warmup time.Duration, limit int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	excess := s.live() - s.desired()
	if excess <= 0 || !s.ready() || !s.leased(now) || now.Sub(s.started) < warmup {
		s.surplusSince = time.Time{}
		return 0
	}
	if s.surplusSince.IsZero() {
		s.surplusSince = now
	}
	if now.Sub(s.surplusSince) < hold {
		return 0
	}
	n := 0
	for _, m := range s.machines {
		if n >= min(excess, limit) {
			break
		}
		r := m.Runner
		if supply(m) && r != nil && !r.Busy && !r.Removing && r.ID != 0 && now.Sub(r.Issued) > hold {
			m.Retire = true
			n++
		}
	}
	return n
}

// ToFence is the retiring machines whose idle runner to remove now, at most limit: GitHub refuses to remove a runner
// that took a job meanwhile, which fences the machine's end.
func (s *State) ToFence(limit int) []*Machine {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Machine
	for _, m := range s.machines {
		if len(out) >= limit {
			break
		}
		r := m.Runner
		if m.Retire && !m.Ending && r != nil && !r.Busy && !r.Removing && r.ID != 0 {
			r.Removing = true
			out = append(out, m)
		}
	}
	return out
}

// Removal is GitHub's answer to removing a runner.
type Removal int

const (
	Removed Removal = iota // removed, or already gone
	HeldJob                // refused: it holds a job
	NotSure                // failed otherwise (a timeout, an outage): try again later
)

// Fenced settles the removal of a retiring machine's runner: removed, the machine ends; refused because it holds a
// job, the runner is busy and the machine serves that job; failed otherwise, it is tried again.
func (s *State) Fenced(m *Machine, out Removal) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := m.Runner; r != nil {
		r.Removing = false
		if out == HeldJob {
			r.Busy, m.Retire = true, false
		}
	}
	if out == Removed {
		m.Ending = true
	}
}

// Shed ends up to n machines never issued a runner that have waited for one over three minutes, the longest-waiting
// first: the registration budget cannot serve them soon, and a machine that waits is paid for. Only once the set is
// ready, and only a machine a runner listing of this process showed no runner of: after a start, EC2's tags may not
// show yet that a machine was issued one.
func (s *State) Shed(now time.Time, n int) []*Machine {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ready() {
		return nil
	}
	var waiting []*Machine
	for _, m := range s.machines {
		if !m.Ending && !m.Issued && !m.Issuing && m.Runner == nil && m.Missed >= 1 && now.Sub(m.Launched) > 3*time.Minute {
			waiting = append(waiting, m)
		}
	}
	sort.Slice(waiting, func(i, j int) bool { return waiting[i].Launched.Before(waiting[j].Launched) })
	waiting = waiting[:min(n, len(waiting))]
	for _, m := range waiting {
		m.Ending = true
	}
	return waiting
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

// Untagged is the issued machines EC2 has no issued tag for (a runner adopted from GitHub or a job message): the tag
// is written so a later start knows too.
func (s *State) Untagged() []*Machine {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Machine
	for _, m := range s.machines {
		if m.Issued && !m.Tagged && !m.Ending {
			m.Tagged = true
			out = append(out, m)
		}
	}
	return out
}

// TagFailed undoes Untagged's mark for a machine whose tag could not be written, so the next pass tries again.
func (s *State) TagFailed(m *Machine) { s.mu.Lock(); m.Tagged = false; s.mu.Unlock() }

// RunnerRef is the id (0 when unknown) and name of a machine's runner.
func (s *State) RunnerRef(m *Machine) (int64, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := m.Runner; r != nil {
		return r.ID, r.Name
	}
	return 0, runnerName(s.set, m.ID)
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

// Drained says whether a set being retired holds nothing more, as of now: it is ready, it has no machine and no
// launch in flight, and GitHub counts no runner and no job assigned or offered to it. That has held for a minute, so
// one EC2 listing that misses a busy machine, or a job assigned but not yet started, never deletes the set under it.
func (s *State) Drained(now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.stats
	if !s.ready() || len(s.machines) > 0 || len(s.reserved) > 0 || st.TotalRegisteredRunners > 0 || st.TotalAssignedJobs > 0 || st.TotalAvailableJobs > 0 {
		s.emptySince = time.Time{}
		return false
	}
	if s.emptySince.IsZero() {
		s.emptySince = now
	}
	return now.Sub(s.emptySince) >= drainedFor
}

// drainedFor is how long a retired set must hold nothing before it is deleted: two of drain's ticks.
const drainedFor = time.Minute

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
	Ready        bool                             `json:"ready"`
	Desired      int                              `json:"desired"`
	Live         int                              `json:"live"`
	Inflight     int                              `json:"inflight"`
	Machines     int                              `json:"machines"`
	Ending       int                              `json:"ending"`
	Retiring     int                              `json:"retiring"`
	Booting      int                              `json:"booting"`
	Idle         int                              `json:"idle"`
	Busy         int                              `json:"busy"`
	Issued       int                              `json:"issued"`
	OnDemand     int                              `json:"on_demand"`
	Notices      int                              `json:"notices"`
	StaleWaiting int                              `json:"stale_waiting"`
	Orphans      int                              `json:"orphans"`
	LaunchS      float64                          `json:"launch_s"` // the launch time the pool is sized for, measured
	Launches     int                              `json:"launches"` // the launches it is measured over
}

func (s *State) Snapshot(now time.Time) Snapshot {
	orphans := s.Orphans(now)
	s.mu.Lock()
	defer s.mu.Unlock()
	inflight := 0
	for _, k := range s.reserved {
		inflight += k
	}
	out := Snapshot{Set: s.set, Stats: s.stats, Pool: s.pool, Max: s.max, Capped: s.capped, Ready: s.ready(), Desired: s.desired(), Live: s.live(),
		Inflight: inflight, Machines: len(s.machines), StaleWaiting: s.staleWaiting, Orphans: orphans,
		LaunchS: s.launchTime(0).Seconds(), Launches: len(s.launches)}
	for _, m := range s.machines {
		switch {
		case m.Ending:
			out.Ending++
		case m.Retire:
			out.Retiring++
		case m.Runner == nil && !m.Issued:
			out.Booting++
		case m.Runner == nil || m.Runner.Busy:
			out.Busy++
		default:
			out.Idle++
		}
		if m.Issued {
			out.Issued++
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
