package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/actions/scaleset"
)

func hoursJSON(vals ...int) string {
	out := make([]string, 24)
	for i := range out {
		out[i] = fmt.Sprint(vals[i%len(vals)])
	}
	return "[" + strings.Join(out, ",") + "]"
}

func TestAPoolWhoseFloorIsAboveItsCapOrBelowZeroIsRefused(t *testing.T) {
	dir := t.TempDir()
	load := func(floor, top string) error {
		p := filepath.Join(dir, "o.json")
		body := fmt.Sprintf(`{"pools": {"superci-4cpu": {"floor": %s, "cap": %s}}}`, floor, top)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := LoadConfig(p)
		return err
	}
	if err := load(hoursJSON(2), hoursJSON(2)); err != nil {
		t.Errorf("a floor equal to its cap is fine: %v", err)
	}
	if err := load(hoursJSON(0, 0, 5), hoursJSON(10)); err != nil {
		t.Errorf("a floor under its cap is fine: %v", err)
	}
	if err := load(hoursJSON(1, 1, 11), hoursJSON(10)); err == nil || !strings.Contains(err.Error(), "hour 2") {
		t.Errorf("a floor of 11 over a cap of 10 at hour 2 was accepted, or the error names no hour: %v", err)
	}
	if err := load(hoursJSON(-1), hoursJSON(10)); err == nil {
		t.Error("a negative floor was accepted")
	}
	if err := load(hoursJSON(0), hoursJSON(-1)); err == nil {
		t.Error("a negative cap was accepted")
	}
}

func TestTheListenerReadsAliveThroughATwoMinutePassAndDeadPastIt(t *testing.T) {
	for _, tc := range []struct {
		idle time.Duration
		dead bool
	}{{30 * time.Second, false}, {90 * time.Second, false}, {130 * time.Second, true}} {
		l := &Listener{reconciled: time.Now().Add(-tc.idle)}
		if err := l.healthy(); (err != nil) != tc.dead {
			t.Errorf("reconciler idle %s: healthy says %v, want dead %v", tc.idle, err, tc.dead)
		}
	}
}

func TestAGrantWhoseHandlerNeverSettlesIsTakenBackByOverdueAfterAMinute(t *testing.T) {
	s := newState(0, 10)
	s.Observe(stats(1, 0), t0)
	launch(s, machines(1, t0), t0)
	b := open()
	if v := s.Ask("i-0000", t0, b, 8*time.Hour); v != Grant {
		t.Fatalf("first ask: %v", v)
	}
	if v := s.Ask("i-0000", t0.Add(3*time.Second), b, 8*time.Hour); v != Wait {
		t.Fatalf("an ask while the grant is in flight: %v, want Wait", v)
	}
	s.Overdue(t0.Add(issuingMax-time.Second), 6*time.Hour, 8*time.Hour, 9*time.Hour)
	if v := s.Ask("i-0000", t0.Add(issuingMax-time.Second), b, 8*time.Hour); v != Wait {
		t.Fatalf("a grant inside its minute: %v, want it left alone", v)
	}
	s.Claim(s.Machine("i-0000"))
	s.Overdue(t0.Add(issuingMax+time.Second), 6*time.Hour, 8*time.Hour, 9*time.Hour)
	if m := s.Machine("i-0000"); m.Issuing || m.Claim != "" {
		t.Fatalf("a grant unsettled for over a minute is still in flight: %+v", m)
	}
	if v := s.Ask("i-0000", t0.Add(issuingMax+4*time.Second), b, 8*time.Hour); v != Grant {
		t.Fatalf("after the deadline the machine asks again and gets %v, want Grant", v)
	}
}

func TestARunnerWhoseRemovalKeepsFailingIsTriedWithBackoffAndThenLeftToOverdue(t *testing.T) {
	s := newState(0, 10)
	s.Observe(stats(0, 0), t0)
	launch(s, machines(1, t0), t0)
	give(s, "i-0000", &Runner{ID: 5, Name: "set-i-0000", Issued: t0})
	s.Noticed("i-0000")
	now := t0
	var tried []time.Duration
	for step := 0; step < 4000 && now.Sub(t0) < 2*time.Hour; step++ {
		for _, m := range s.ToFence(now, 10) {
			tried = append(tried, now.Sub(t0))
			s.Fenced(m, NotSure, now)
		}
		now = now.Add(10 * time.Second)
	}
	if len(tried) != fenceTries {
		t.Fatalf("%d removals tried over two hours of failures: %v, want %d", len(tried), tried, fenceTries)
	}
	for i := 1; i < len(tried); i++ {
		gap := tried[i] - tried[i-1]
		want := min(fenceBackoffMax, fenceBackoff<<(i-1))
		if gap < want || gap > want+10*time.Second {
			t.Errorf("gap before try %d is %s, want %s (tries at %v)", i+1, gap, want, tried)
		}
	}
	if m := s.Machine("i-0000"); m.Ending || !m.Retire {
		t.Errorf("a machine left to Overdue is still retiring and not ended yet: %+v", m)
	}
	if got := s.Overdue(t0.Add(10*time.Hour), 6*time.Hour, 8*time.Hour, 9*time.Hour); len(got) != 1 {
		t.Errorf("Overdue ends the machine at its life: %v", ids(got))
	}
}

func TestARemovalThatWorksOrFindsAJobForgetsEarlierFailures(t *testing.T) {
	s := newState(0, 10)
	s.Observe(stats(0, 0), t0)
	launch(s, machines(1, t0), t0)
	give(s, "i-0000", &Runner{ID: 5, Name: "set-i-0000", Issued: t0})
	s.Noticed("i-0000")
	m := s.ToFence(t0, 10)[0]
	for i := 0; i < fenceTries-1; i++ {
		s.Fenced(m, NotSure, t0)
		m.Runner.Removing = true
	}
	s.Fenced(m, HeldJob, t0)
	if m.FenceFails != 0 || !m.FenceAfter.IsZero() {
		t.Fatalf("a refusal for a job left %d failures and a wait to %v", m.FenceFails, m.FenceAfter)
	}
}

func TestQueueWaitIsPublishedAlsoForEachSet(t *testing.T) {
	m := Metrics{Waits: []float64{1, 1, 300}, SetWaits: map[string][]float64{"superci-4cpu": {300}, "superci-2cpu": {1, 1}, "superci-8cpu": nil}}
	var all, perSet = 0, map[string][]float64{}
	for _, d := range queueWaitData(m, nil) {
		if *d.MetricName != "QueueWait" {
			t.Fatalf("%s", *d.MetricName)
		}
		if len(d.Dimensions) == 0 {
			all += len(d.Values)
			continue
		}
		if *d.Dimensions[0].Name != "Set" {
			t.Fatalf("dimension %s", *d.Dimensions[0].Name)
		}
		set := *d.Dimensions[0].Value
		for i, v := range d.Values {
			for c := 0; c < int(d.Counts[i]); c++ {
				perSet[set] = append(perSet[set], v)
			}
		}
	}
	if all != 2 {
		t.Errorf("the dimensionless QueueWait (what the p90 alarm reads) holds %d values, want its 2 distinct ones", all)
	}
	if len(perSet["superci-4cpu"]) != 1 || perSet["superci-4cpu"][0] != 300 || len(perSet["superci-2cpu"]) != 2 {
		t.Errorf("per set: %v", perSet)
	}
	if _, ok := perSet["superci-8cpu"]; ok {
		t.Error("a set with no job started publishes no wait")
	}
}

func TestMetricsGiveEachSetItsOwnWaits(t *testing.T) {
	a, b := NewState("superci-2cpu", 0, 10, nil, t0), NewState("superci-4cpu", 0, 10, nil, t0)
	started := func(s *State, job string) {
		s.Observe(&scaleset.RunnerScaleSetMessage{JobStartedMessages: []*scaleset.JobStarted{{JobMessageBase: scaleset.JobMessageBase{JobID: job, QueueTime: t0.Add(-30 * time.Second)}}}}, t0)
	}
	started(a, "j1")
	started(b, "j2")
	started(b, "j3")
	l := &Listener{sets: []*Set{{spec: SetSpec{Name: "superci-2cpu"}, state: a}, {spec: SetSpec{Name: "superci-4cpu"}, state: b}},
		budget: NewBudget(reg(), t0), spend: NewSpend(0, 0, nil, t0), strays: map[string]*Machine{}}
	m := l.metrics()
	if len(m.Waits) != 3 || len(m.SetWaits["superci-2cpu"]) != 1 || len(m.SetWaits["superci-4cpu"]) != 2 {
		t.Fatalf("waits %v by set %v", m.Waits, m.SetWaits)
	}
}

// Only the set whose jobs stalled is marked stalled for its own alarm; the listener-wide count still counts it.
func TestMetricsMarkOnlyTheStalledSetInStalledBy(t *testing.T) {
	a, b := newState(0, 10), newState(0, 10)
	a.Observe(stats(2, 0), time.Now().Add(-11*time.Minute))
	l := &Listener{sets: []*Set{{spec: SetSpec{Name: "superci-4cpu"}, state: a}, {spec: SetSpec{Name: "superci-8cpu"}, state: b}},
		budget: NewBudget(reg(), t0), spend: NewSpend(0, 0, nil, t0), strays: map[string]*Machine{}}
	m := l.metrics()
	if m.Stalled != 1 || !m.StalledBy["superci-4cpu"] || m.StalledBy["superci-8cpu"] {
		t.Errorf("stalled %d by set %v, want only superci-4cpu", m.Stalled, m.StalledBy)
	}
}

// sendFn is boot.sh's send function and mark spool, run in bash against a curl that answers as the listener does: 413 to
// a marks header over 32 KiB, else 204.
func runSend(t *testing.T, lines []string, rounds int, curlCode string) (posts []string, left []string) {
	t.Helper()
	src, err := os.ReadFile("boot.sh")
	if err != nil {
		t.Fatal(err)
	}
	fn := regexp.MustCompile(`(?s)\nsend\(\) \{\n.*?\n\}\n`).Find(src)
	if fn == nil {
		t.Fatal("boot.sh has no send function")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "spool"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "spool.sent"), []byte("0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	script := `SP=$1/spool; PIN=x; L=http://listener; DOC=doc
curl() { local h=; while [ $# -gt 0 ]; do case $1 in -H) case $2 in X-Superci-Marks:*) h=${2#X-Superci-Marks: };; esac; shift;; esac; shift; done
  printf '%s\n' "$h" >> $SP.posts
  if [ "${#h}" -gt 32768 ] || [ -n "$FORCE" ]; then printf %s "${FORCE:-413}"; else printf 204; fi; }
` + string(fn) + `
for _ in $(seq $2); do send; done
`
	cmd := exec.Command("bash", "-c", script, "_", dir, fmt.Sprint(rounds))
	cmd.Env = append(os.Environ(), "FORCE="+curlCode)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "spool.posts"))
	posts = strings.Fields(string(raw))
	took, _ := os.ReadFile(filepath.Join(dir, "spool.sent"))
	var n int
	fmt.Sscan(string(took), &n)
	return posts, lines[min(n, len(lines)):]
}

func TestAFatMarkBatchIsCutAndAnOverlongLineSkippedNotResentForEver(t *testing.T) {
	fat := "cache.restore=1795631985.000000=1=" + strings.Repeat("a", 5000)
	huge := "cache.save=1795631985.000000=1=" + strings.Repeat("b", 40000)
	lines := []string{"boot.start=1795631980.000000=0"}
	for i := 0; i < 8; i++ {
		lines = append(lines, fat) // 8 lines of 5 KB: 40 KB, over the listener's 32 KiB
	}
	lines = append(lines, huge, "poweroff=1795631999.000000=9")
	posts, left := runSend(t, lines, 20, "")
	if len(left) != 0 {
		t.Fatalf("after 20 sends %d lines are still unsent, the first %.40q: one fat batch holds every mark behind it", len(left), left[0])
	}
	delivered := map[string]int{}
	for _, p := range posts {
		if len(p) > 32768 {
			t.Errorf("a post carried a marks header of %d bytes, over the listener's 32 KiB", len(p))
		}
		for _, l := range strings.Split(p, ",") {
			delivered[strings.SplitN(l, "=", 2)[0]]++
		}
	}
	if delivered["cache.restore"] != 8 || delivered["boot.start"] != 1 || delivered["poweroff"] != 1 || delivered["cache.save"] != 0 {
		t.Errorf("delivered %v: want every line but the one over the limit (cache.save), each once", delivered)
	}
}

func TestA413TheSenderDidNotForeseeDropsOneLineAndTheRestGoOn(t *testing.T) {
	lines := []string{"cache.restore=1795631985.000000=1=x", "cache.save=1795631986.000000=1=y", "poweroff=1795631999.000000=9"}
	posts, left := runSend(t, lines, 1, "413")
	if len(posts) != 1 || len(left) != 2 || left[0] != lines[1] {
		t.Fatalf("one 413 for a batch of three: posted %d times, left %q: want the first line dropped, the others kept", len(posts), left)
	}
	_, left = runSend(t, lines, 3, "413")
	if len(left) != 0 {
		t.Fatalf("a listener that refuses every post leaves %q: the spool must drain", left)
	}
	_, left = runSend(t, lines, 2, "500")
	if len(left) != 3 {
		t.Fatalf("a server error leaves the lines to be sent again: left %q", left)
	}
}
