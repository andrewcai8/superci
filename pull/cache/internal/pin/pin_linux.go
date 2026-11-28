package pin

import (
	"os"
	"syscall"
)

// mlock maps f read-only and shared, so the pages are the page cache's own, populated now, and locks them. A map made
// with MAP_LOCKED alone would not say that the lock failed (RLIMIT_MEMLOCK), so the lock is its own call.
func mlock(f *os.File, size int64) (func(), error) {
	b, err := syscall.Mmap(int(f.Fd()), 0, int(size), syscall.PROT_READ, syscall.MAP_SHARED|syscall.MAP_POPULATE)
	if err != nil {
		return nil, err
	}
	if err := syscall.Mlock(b); err != nil {
		_ = syscall.Munmap(b)
		return nil, err
	}
	return func() { _ = syscall.Munmap(b) }, nil
}
