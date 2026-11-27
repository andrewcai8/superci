package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// writtenOnMachines are trees a job or the machine's own boot writes: a read set recorded on a lab machine (tools/readset)
// holds only paths of the image, checked against a volume of its snapshot, so none of these can be in it.
var writtenOnMachines = []string{"/home/runner/_work/", "/home/runner/hydrated/", "/home/runner/.", "/tmp/", "/run/", "/proc/",
	"/sys/", "/dev/", "/root/", "/var/lib/cloud/", "/var/log/"}

func TestEachReadSetIsSortedAbsoluteImagePathsAndNamesASize(t *testing.T) {
	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	lists, _ := filepath.Glob("readsets/*.list")
	if len(lists) == 0 {
		t.Fatal("no read set in readsets/")
	}
	for _, f := range lists {
		label := strings.TrimSuffix(filepath.Base(f), ".list")
		if _, ok := cfg.Sizes[label]; !ok {
			t.Errorf("%s: %s is no size of the config, so no machine would read it", f, label)
		}
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		paths := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
		if !slices.IsSorted(paths) || len(slices.Compact(slices.Clone(paths))) != len(paths) {
			t.Errorf("%s: paths are not sorted and unique (tools/readset build writes them so)", f)
		}
		for _, p := range paths {
			if !filepath.IsAbs(p) || filepath.Clean(p) != p {
				t.Errorf("%s: %q is not a clean absolute path", f, p)
			}
			for _, w := range writtenOnMachines {
				if strings.HasPrefix(p, w) {
					t.Errorf("%s: %q is under %s, which jobs or boot write, not the image", f, p, w)
				}
			}
		}
	}
}
