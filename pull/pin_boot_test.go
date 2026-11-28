package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// pinStub is a pullcache that hydrates one checkout entry and, asked to pin, records how it was started (its arguments,
// its ids, its session and its memlock limits), prints the summary line and holds, as `pullcache pin` does.
const pinStub = `case "$1" in
  pin)
    echo "$@" > "$BOX/pin.args"
    echo "$$ $(ps -o sid= -p $$ | tr -d ' ') $(ps -o pgid= -p $$ | tr -d ' ')" > "$BOX/pin.ids"
    echo "$(ulimit -S -l) $(ulimit -H -l)" > "$BOX/pin.memlock"
    echo "$$" > "$BOX/pin.pid"
    echo "pinned files=53445 bytes=704643072 seconds=0.64 capped=no failed=0"
    exec sleep 300 ;;
esac
echo "hydrate: log line" >&2
echo "hydrated 9/9 entries 1775 MB"
echo "checkout $BOX/hydrated/v/git-seed/work"`

func hydratedMarks(b *prereadBox) string { return strings.Join(b.marks(), " ") }

// killPinner ends the stub pinner a test started: the one pid it wrote.
func killPinner(t *testing.T, b *prereadBox) {
	t.Cleanup(func() {
		if pid, err := strconv.Atoi(b.read("pin.pid")); err == nil && pid > 1 {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
}

func sessionOf(t *testing.T, pid int) int {
	t.Helper()
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		t.Fatal(err)
	}
	// pid (comm) state ppid pgrp session ...
	f := strings.Fields(string(raw[strings.LastIndex(string(raw), ")")+2:]))
	sid, _ := strconv.Atoi(f[3])
	return sid
}

func TestAMachineOf15GBOrMoreStartsThePinnerOnTheCheckoutAfterHydrationAndPostsPinDone(t *testing.T) {
	b := startPreread(t, `true`, "30083776512", pinStub)
	killPinner(t, b)
	b.wait(t, 20*time.Second)
	if got, want := b.read("pin.args"), "pin "+b.path("hydrated/v/git-seed/work"); got != want {
		t.Errorf("the pinner ran as %q, want %q: the work tree of the checkout entry", got, want)
	}
	if got, want := hydratedMarks(b), "hydrate.start= hydrate.done=9/9:1775MB pin.done=53445:672MB preread.start= preread.done=full rewarm.done=full warm.done=full"; got != want {
		t.Errorf("marks %q, want %q", got, want)
	}
	if _, err := os.Stat(filepath.Join(b.hr, ".warming")); err == nil {
		// startPreread's box makes the mark itself; hydrate() removes it only after the pin's.
		t.Error("the warm mark is still there after hydrate()")
	}
}

func TestThePinnerIsItsOwnSessionWithItsOwnMemlockAndOutlivesTheHooksKillOfTheHydration(t *testing.T) {
	// The hydration as boot.sh starts it: its own session, its leader's id in .pid; fio keeps it in the pre-read.
	b := prepPreread(t, `echo $$ > "$BOX/fio.pid"; exec sleep 300`, "30083776512", pinStub, "0", "0", "16000000", nil)
	// The soft memlock limit is low, as a login's is, so what the pinner holds is what boot raised it to.
	b.cmd = exec.Command("setsid", "bash", "-c", "ulimit -S -l 64\n"+b.cmd.Args[2])
	b.cmd.Env = b.env
	if err := b.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	killPinner(t, b)
	// The leader is reaped as init would: a zombie leader keeps its group alive.
	go func() { _ = b.cmd.Wait() }()
	t.Cleanup(func() { _ = syscall.Kill(-b.cmd.Process.Pid, syscall.SIGKILL) })
	waitFor(t, "the pinner and fio to start", func() bool { return b.read("pin.ids") != "" && b.read("fio.pid") != "" })
	ids := strings.Fields(b.read("pin.ids"))
	pinner, _ := strconv.Atoi(ids[0])
	if ids[1] != ids[0] || ids[2] != ids[0] {
		t.Fatalf("the pinner (pid %s) is in session %s, group %s: want its own session", ids[0], ids[1], ids[2])
	}
	hydration := b.cmd.Process.Pid
	if sessionOf(t, pinner) == sessionOf(t, hydration) {
		t.Fatal("the pinner shares the hydration's session")
	}
	if soft, hard, _ := strings.Cut(b.read("pin.memlock"), " "); soft == "64" || soft != hard {
		t.Errorf("the pinner's memlock limit is %q: want the soft limit raised to the hard one (from 64 kB)", b.read("pin.memlock"))
	}
	// The job-started hook ends the hydration's whole group.
	runHook(t, b.hr)
	waitFor(t, "the hook to end the hydration's group", func() bool { return !groupAlive(hydration) })
	if err := syscall.Kill(pinner, 0); err != nil {
		t.Fatalf("the hook's kill took the pinner with it: %v", err)
	}
}

// pinned runs the hydration on a machine of memKB kB with the given pullcache and says whether the pinner started.
func pinned(t *testing.T, memKB, pullcache string) (*prereadBox, bool) {
	t.Helper()
	b := startPrereadOn(t, `true`, "30083776512", pullcache, "0", "0", memKB)
	killPinner(t, b)
	b.wait(t, 20*time.Second)
	return b, b.read("pin.args") != ""
}

func TestAMachineBelow15GBPinsNothing(t *testing.T) {
	if _, ok := pinned(t, "15000000", pinStub); !ok {
		t.Fatal("control: a machine of exactly 15000000 kB must pin, or this test proves nothing")
	}
	for name, memKB := range map[string]string{"14999999 kB": "14999999", "a 4 GB machine": "4000000", "no meminfo number": "x"} {
		t.Run(name, func(t *testing.T) {
			b, ok := pinned(t, memKB, pinStub)
			if ok {
				t.Errorf("the pinner started on %s: %q", name, b.read("pin.args"))
			}
			if got := hydratedMarks(b); strings.Contains(got, "pin.") || !strings.Contains(got, "hydrate.done=9/9:1775MB preread.start=") {
				t.Errorf("marks %q: want no pin mark and the hydration as before", got)
			}
		})
	}
}

func TestAMachineWhoseHydrationNamedNoCheckoutPinsNothing(t *testing.T) {
	stub := func(checkout string) string {
		return `case "$1" in pin) echo "$@" > "$BOX/pin.args"; echo "pinned files=1 bytes=1 seconds=0.01 capped=no failed=0"; exec sleep 300 ;; esac
echo "hydrated 3/3 entries 100 MB"; ` + checkout
	}
	if _, ok := pinned(t, "16000000", stub(`echo "checkout $BOX/tree"`)); !ok {
		t.Fatal("control: the same machine with a checkout line must pin")
	}
	b, ok := pinned(t, "16000000", stub(`true`))
	if got := hydratedMarks(b); ok || strings.Contains(got, "pin.") {
		t.Errorf("a hydration with no checkout entry pinned (%v): marks %q", ok, got)
	}
}

func TestAPinnerThatFailsOrNeverAnswersCostsTheBootNothing(t *testing.T) {
	for name, c := range map[string]struct {
		pin, want string
		within    time.Duration
	}{
		"dies at once":      {`exit 1`, "pin.done=failed", 5 * time.Second},
		"crashes after one": {`echo boom >&2; exit 2`, "pin.done=failed", 5 * time.Second},
		"never prints":      {`exec sleep 300`, "pin.done=late", 8 * time.Second},
		// The lab's pins took 0.33 to 1.06 s (Nov 28): one that answers in 2 s is waited for, not marked late.
		"answers in 2 s":  {`sleep 2; echo "pinned files=100 bytes=3145728 seconds=2.00 capped=no failed=0"; exec sleep 300`, "pin.done=100:3MB", 5 * time.Second},
		"cannot lock all": {`echo "pinned files=100 bytes=3145728 seconds=0.20 capped=stopped failed=1"; exec sleep 300`, "pin.done=100:3MB:stopped:failed1", 5 * time.Second},
		"hit its cap":     {`echo "pinned files=9 bytes=2147483648 seconds=1.00 capped=yes failed=0"; exec sleep 300`, "pin.done=9:2048MB:capped", 5 * time.Second},
	} {
		t.Run(name, func(t *testing.T) {
			pull := `case "$1" in pin) echo $$ > "$BOX/pin.pid"; ` + c.pin + `;; esac
echo "hydrated 9/9 entries 1775 MB"; echo "checkout $BOX/tree"`
			b := startPreread(t, `true`, "30083776512", pull)
			killPinner(t, b)
			began := time.Now()
			b.wait(t, c.within+15*time.Second)
			got := hydratedMarks(b)
			if !strings.Contains(got, " "+c.want+" ") {
				t.Errorf("marks %q, want %s", got, c.want)
			}
			if !strings.Contains(got, "preread.done=full rewarm.done=full warm.done=full") {
				t.Errorf("a pinner that %s stopped the rest of the hydration: %q", name, got)
			}
			if _, err := os.Stat(filepath.Join(b.hr, ".pid")); !os.IsNotExist(err) {
				t.Errorf("the pid file is left: %v", err)
			}
			if c.want == "pin.done=late" {
				// Up to about 5 s, not longer, though the rest of the pre-read took its time too.
				if took := time.Since(began); took < 4500*time.Millisecond {
					t.Errorf("it did not wait 5 s for the pinner's summary: %v", took)
				}
			}
		})
	}
}

func TestThePinnerStartsAfterHydrationAndBeforeThePrereadAndTheWarmMarkGoes(t *testing.T) {
	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	got := bootData(cfg, realListener, realPin, "superci-4cpu")
	hydrate := strings.Index(got, "/usr/local/bin/pullcache hydrate ")
	pin := strings.Index(got, `pin "$tree"`)
	preread := strings.Index(got, `[ -n "${rs:-}" ] || preread`)
	warm := strings.Index(got, `rm -f "$HR/.pid" "$HR/.warming"`)
	if hydrate < 0 || pin < hydrate || preread < pin || warm < preread {
		t.Fatalf("order: pullcache hydrate %d, pin %d, preread %d, warm mark removed %d", hydrate, pin, preread, warm)
	}
	for _, want := range []string{"exec setsid /usr/local/bin/pullcache pin ", "ulimit -l unlimited", "-ge 15000000"} {
		if !strings.Contains(got, want) {
			t.Errorf("boot data lacks %q", want)
		}
	}
	// pin.done is a mark the listener takes, and a machine's own that the schema lists (events_test.go holds both lists equal).
	if !markNames["pin.done"] {
		t.Error("the listener drops pin.done")
	}
}

func TestTheSysctlFileRaisesTheMapCountThePinnerNeeds(t *testing.T) {
	src, err := os.ReadFile("boot.sh")
	if err != nil {
		t.Fatal(err)
	}
	line := regexp.MustCompile(`(?m)^printf '%s\\n' (.*) > /etc/sysctl\.d/99-superci\.conf$`).FindSubmatch(src)
	if line == nil {
		t.Fatal("boot.sh writes no /etc/sysctl.d/99-superci.conf")
	}
	out := filepath.Join(t.TempDir(), "99-superci.conf")
	cmd := exec.Command("bash", "-c", "printf '%s\\n' "+string(line[1])+" > "+out)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, b)
	}
	raw, _ := os.ReadFile(out)
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	for _, want := range []string{"vm.max_map_count=1048576", "vm.dirty_ratio=50", "net.ipv4.ip_local_reserved_ports=50000-59999"} {
		found := false
		for _, l := range lines {
			found = found || l == want
		}
		if !found {
			t.Errorf("the sysctl file %q lacks the line %s", lines, want)
		}
	}
}
