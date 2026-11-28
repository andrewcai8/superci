// Package pin locks a hydrated checkout in the page cache (`pullcache pin DIR`, started by boot.sh's hydrate()).
//
// Memory pressure in a job pushes a checkout out of the page cache: the files were written once at boot and not read
// since, so they sit on the inactive list and go first. A scan of 10,791 small files then read gp3 one 4 KiB block at a
// time, 4.8 to 5.2 s against a 5 s timeout, where from memory it takes 0.34 s (lab superci-lab=cold, Nov 28). Each file
// is mapped and mlocked, and the process holds the maps until it is killed or the machine powers off, so the kernel
// cannot evict them. A rename (the job's first step moves the tree into its workspace) keeps the inodes and their pages.
package pin

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const (
	// DefaultMaxBytes is the file data pinned at most. The checkout is 0.7 GB; a tree past this is not a checkout a job
	// scans, and what it locks is memory no test can use.
	DefaultMaxBytes = 2 << 30
	// DefaultMaxFiles is the files pinned at most. Each is one map, and the machine's vm.max_map_count (boot.sh sets
	// 1,048,576) is what bounds the maps of a process.
	DefaultMaxFiles = 400000
)

// A Locker maps the open file of the given size and locks the map in memory. It returns what undoes both, and an error
// when it could do neither (the error of the call that failed). Tests replace it.
type Locker func(f *os.File, size int64) (unlock func(), err error)

// Options are what Run pins and how.
type Options struct {
	MaxBytes int64
	MaxFiles int
	// Lock is Mlock when nil.
	Lock Locker
}

// A Result is what Run did.
type Result struct {
	Files   int
	Bytes   int64
	Seconds float64
	// Capped is that a cap stopped the locking, and Stopped what else did: the first lock the machine refused
	// (RLIMIT_MEMLOCK, no memory to lock).
	Capped  bool
	Stopped bool
	// Failed is the files that could not be locked, and FirstError the first of them.
	Failed     int
	FirstError error
	unlocks    []func()
}

// Release unlocks everything Run locked (the process normally holds it until it dies, so only tests call this).
func (r *Result) Release() {
	for _, u := range r.unlocks {
		u()
	}
	r.unlocks = nil
}

// Line is the one line `pullcache pin` prints: files, bytes, seconds, and whether a cap stopped it (or the machine
// refused a lock, which is a failure and is said as one). boot.sh reads the fields by name.
func (r Result) Line() string {
	state := "no"
	switch {
	case r.Capped:
		state = "yes"
	case r.Stopped:
		state = "stopped"
	}
	s := fmt.Sprintf("pinned files=%d bytes=%d seconds=%.2f capped=%s failed=%d", r.Files, r.Bytes, r.Seconds, state, r.Failed)
	if r.FirstError != nil {
		s += fmt.Sprintf(" first_error=%q", r.FirstError.Error())
	}
	return s
}

// errCap ends the walk at a cap.
var errCap = errors.New("cap")

// errStop ends it at a lock the machine refused: the next would be refused too.
var errStop = errors.New("stop")

// Run reads and locks every regular file under root except what is under a node_modules directory and what is in a
// .git directory but its index (the work tree is what jobs scan; the index is what git status reads first). A file that
// vanishes or is not regular by the time it is opened is skipped. It stops at the first cap. A lock the machine refuses
// is counted and the first reason kept: those that say the limits of memory are out stop the run, since every file after
// would fail the same way. It never returns an error for a file; only an unusable root is one.
func Run(root string, o Options) (Result, error) {
	var r Result
	began := time.Now()
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		return r, err
	}
	if st, err := os.Stat(root); err != nil || !st.IsDir() {
		return r, fmt.Errorf("%s is not a directory", root)
	}
	if o.MaxBytes <= 0 {
		o.MaxBytes = DefaultMaxBytes
	}
	if o.MaxFiles <= 0 {
		o.MaxFiles = DefaultMaxFiles
	}
	lock := o.Lock
	if lock == nil {
		lock = mlock
	}
	one := func(path string) error {
		f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				r.fail(err)
			}
			return nil
		}
		defer f.Close()
		st, err := f.Stat()
		if err != nil || !st.Mode().IsRegular() || st.Size() == 0 {
			return nil
		}
		if r.Files >= o.MaxFiles || r.Bytes+st.Size() > o.MaxBytes {
			r.Capped = true
			return errCap
		}
		unlock, err := lock(f, st.Size())
		if err != nil {
			r.fail(err)
			if errors.Is(err, syscall.ENOMEM) || errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EAGAIN) {
				r.Stopped = true
				return errStop
			}
			return nil
		}
		r.unlocks = append(r.unlocks, unlock)
		r.Files++
		r.Bytes += st.Size()
		return nil
	}
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		switch {
		case d.IsDir() && d.Name() == "node_modules":
			return fs.SkipDir
		case d.Name() == ".git" && path != root:
			if d.IsDir() {
				if err := one(filepath.Join(path, "index")); err != nil {
					return err
				}
				return fs.SkipDir
			}
			return nil
		case d.Type().IsRegular():
			return one(path)
		}
		return nil
	})
	if err != nil && err != errCap && err != errStop {
		return r, err
	}
	r.Seconds = time.Since(began).Seconds()
	return r, nil
}

func (r *Result) fail(err error) {
	r.Failed++
	if r.FirstError == nil {
		r.FirstError = err
	}
}
