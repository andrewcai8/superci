package pin

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

const rlimitMemlock = 8 // RLIMIT_MEMLOCK on Linux

func mlockedKB(t *testing.T) int {
	t.Helper()
	raw, err := os.ReadFile("/proc/self/status")
	if err != nil {
		t.Skipf("no /proc: %v", err)
	}
	for _, l := range strings.Split(string(raw), "\n") {
		if v, ok := strings.CutPrefix(l, "VmLck:"); ok {
			n, _ := strconv.Atoi(strings.Fields(v)[0])
			return n
		}
	}
	t.Skip("no VmLck in /proc/self/status")
	return 0
}

func memlockLimit() string {
	var l syscall.Rlimit
	if err := syscall.Getrlimit(rlimitMemlock, &l); err != nil {
		return err.Error()
	}
	return fmt.Sprintf("%d bytes", l.Cur)
}
