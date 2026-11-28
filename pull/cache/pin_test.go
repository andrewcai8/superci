package main

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestMain lets the tests below run `pullcache pin` as a process: the test binary is the command when asked to be.
func TestMain(m *testing.M) {
	if os.Getenv("PULLCACHE_TEST_PIN") == "1" {
		if err := pinDir(os.Args[1:]); err != nil {
			os.Stderr.WriteString("pullcache: " + err.Error() + "\n")
			os.Exit(1)
		}
		return
	}
	os.Exit(m.Run())
}

func startPin(t *testing.T, args ...string) (*exec.Cmd, *bufio.Scanner) {
	t.Helper()
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(), "PULLCACHE_TEST_PIN=1")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return cmd, bufio.NewScanner(out)
}

func TestPinPrintsOneSummaryLineThenHoldsUntilTermAndExitsCleanly(t *testing.T) {
	dir := t.TempDir()
	for name, n := range map[string]int{"a": 100, "src/b": 200, "node_modules/c": 999} {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(strings.Repeat("x", n)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cmd, lines := startPin(t, dir)
	if !lines.Scan() {
		t.Fatalf("no summary line: %v", lines.Err())
	}
	line := lines.Text()
	if !regexp.MustCompile(`^pinned files=\d+ bytes=\d+ seconds=\d+\.\d\d capped=(no|yes|stopped) failed=\d+`).MatchString(line) {
		t.Fatalf("summary %q", line)
	}
	if strings.Contains(line, "capped=no") && !strings.Contains(line, "files=2 bytes=300 ") {
		t.Fatalf("want the two files outside node_modules: %q", line)
	}
	// It holds: still alive after the line.
	time.Sleep(100 * time.Millisecond)
	if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("the pinner ended on its own: %v", err)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("SIGTERM ends it with status 0: %v", err)
	}
}

func TestPinAtItsCapSaysSoOnTheLine(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"a", "b", "c"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("xxxx"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	_, lines := startPin(t, "-max-files", "1", dir)
	if !lines.Scan() {
		t.Fatalf("no summary line: %v", lines.Err())
	}
	if got := lines.Text(); !strings.Contains(got, "files=1 bytes=4 ") || !strings.Contains(got, "capped=yes") {
		t.Fatalf("summary %q", got)
	}
}

func TestPinOfAMissingDirectoryFailsWithAMessageAndNoHold(t *testing.T) {
	cmd := exec.Command(os.Args[0], filepath.Join(t.TempDir(), "nope"))
	cmd.Env = append(os.Environ(), "PULLCACHE_TEST_PIN=1")
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "pullcache: ") {
		t.Fatalf("want a failure that says why: %v %q", err, out)
	}
}
