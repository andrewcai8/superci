package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestTheBuiltInConfigHasEverySize(t *testing.T) {
	c, err := LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	for _, label := range []string{"superci-2cpu", "superci-4cpu", "superci-8cpu", "superci-16cpu", "superci-32cpu"} {
		s, ok := c.Sizes[label]
		// Two types at least, so a size outlives one type's spot running out. superci-8cpu has exactly two: r8a.2xlarge
		// runs 12 to 20% slower and took jobs ahead of m8a.2xlarge, whose spot gave 609 launches on Nov 26-27.
		if !ok || len(s.Types) < 2 {
			t.Fatalf("%s: %+v, want 2 or more types", label, s)
		}
		if _, ok := c.Pools[label]; !ok {
			t.Fatalf("%s has no pool bounds", label)
		}
	}
}

// unit-tests' heaviest leg peaks at 11.3 GB on Blacksmith's 16 GB 4 vCPU runner, and the kernel ended it at 6.2 GB on a
// c8a.xlarge (8 GB) in 5 of 5 runs: a 4 or 8 vCPU machine has 4 GiB a vCPU (m) or more (r), as Blacksmith's do.
func TestTheFourAndEightCPUSizesHaveBlacksmithsMemory(t *testing.T) {
	c, err := LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	for _, label := range []string{"superci-4cpu", "superci-8cpu"} {
		for _, ty := range c.Sizes[label].Types {
			if ty[0] != 'm' && ty[0] != 'r' {
				t.Errorf("%s lists %s, which has less than 4 GiB a vCPU", label, ty)
			}
		}
	}
}

func TestASplitSizeRunsAsSeveralSetsWithTheLabel(t *testing.T) {
	c := Config{Sizes: map[string]SizeConfig{"superci-4cpu": {CPU: 4, Split: 3, Max: 300, Types: []string{"m8a.xlarge"}}},
		Pools: map[string]PoolConfig{"superci-4cpu": {Floor: [24]int{10: 7}, Cap: [24]int{10: 100}}}}
	sets := c.Sets()
	if len(sets) != 3 || sets[0].Name != "superci-4cpu" || sets[1].Name != "superci-4cpu-2" || sets[2].Name != "superci-4cpu-3" {
		t.Fatalf("sets %+v", sets)
	}
	for _, s := range sets {
		if s.Label != "superci-4cpu" || s.Max != 100 {
			t.Fatalf("set %+v: want the size's label and a third of its max", s)
		}
	}
	if floor, top := c.PoolBounds(sets[1], time.Date(2026, 11, 25, 10, 30, 0, 0, time.UTC)); floor != 3 || top != 34 {
		t.Fatalf("bounds %d/%d: want a third of 7 and of 100, rounded up", floor, top)
	}
}

func TestThePoolFollowsBurstsWithinTheHoursBounds(t *testing.T) {
	c := Config{Pool: PoolRule{LeadMinutes: 15},
		Pools: map[string]PoolConfig{"l": {Floor: [24]int{3: 1, 4: 2}, Cap: [24]int{3: 50, 4: 120}}}}
	spec := SetSpec{Name: "l", Label: "l", Split: 1}
	at := func(h, m int) time.Time { return time.Date(2026, 11, 25, h, m, 0, 0, time.UTC) }
	for _, tc := range []struct {
		at          time.Time
		burst, want int
		why         string
	}{
		{at(3, 10), 0, 0, "no jobs: nothing is routed here, so no floor holds machines"},
		{at(3, 10), 1, 1, "one job: the floor (1) and the burst agree"},
		{at(3, 10), 30, 30, "the burst"},
		{at(3, 10), 500, 50, "the hour's cap"},
		{at(3, 50), 500, 120, "within the lead of a busier hour: its cap"},
		{at(3, 50), 0, 0, "within the lead of a busier hour, no jobs: still none"},
		{at(3, 50), 1, 2, "within the lead of a busier hour, a job: its floor"},
	} {
		if n := c.PoolTarget(spec, tc.at, tc.burst); n != tc.want {
			t.Fatalf("burst %d at %s: pool %d, want %d (%s)", tc.burst, tc.at.Format("15:04"), n, tc.want, tc.why)
		}
	}
}

func TestAPoolRuleWithoutAQuantileOrALaunchTimeIsRefused(t *testing.T) {
	for _, override := range []string{`{"pool": {"quantile": 0, "ready_seconds": 22}}`, `{"pool": {"quantile": 0.8, "ready_seconds": 0}}`, `{"pool": {"quantile": 1.5, "ready_seconds": 22}}`} {
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, []byte(override), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfig(path); err == nil {
			t.Fatalf("%s: accepted", override)
		}
	}
}

func TestTheOverrideFileReplacesOnlyWhatItNames(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"daily_usd": 50, "pools": {"superci-8cpu": {"floor": [5,5,5,5,5,5,5,5,5,5,5,5,5,5,5,5,5,5,5,5,5,5,5,5], "cap": [9,9,9,9,9,9,9,9,9,9,9,9,9,9,9,9,9,9,9,9,9,9,9,9]}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.DailyUSD != 50 || c.Pools["superci-8cpu"].Floor[0] != 5 || c.Pools["superci-4cpu"].Cap[22] == 0 || len(c.Sizes) != 5 {
		t.Fatalf("override %+v", c)
	}
}

// OPS-9: a typo in the override file was accepted, and an hourly array of the wrong length read as zeros.
func TestAnOverrideWithATypoOrAShortHourlyArrayIsRefused(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"unknown field":   `{"daily_usdd": 50}`,
		"short array":     `{"pools": {"superci-8cpu": {"floor": [5,5,5], "cap": [9,9,9,9,9,9,9,9,9,9,9,9,9,9,9,9,9,9,9,9,9,9,9,9]}}}`,
		"long array":      `{"pools": {"superci-8cpu": {"floor": [5,5,5,5,5,5,5,5,5,5,5,5,5,5,5,5,5,5,5,5,5,5,5,5,5], "cap": [9,9,9,9,9,9,9,9,9,9,9,9,9,9,9,9,9,9,9,9,9,9,9,9]}}}`,
		"unknown size":    `{"pools": {"superci-6cpu": {"floor": [0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0], "cap": [1,1,1,1,1,1,1,1,1,1,1,1,1,1,1,1,1,1,1,1,1,1,1,1]}}}`,
		"two values":      `{"daily_usd": 50} {"daily_usd": 60}`,
		"no repositories": `{"repositories": []}`,
	} {
		path := filepath.Join(dir, "config.json")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfig(path); err == nil {
			t.Fatalf("%s: accepted %s", name, body)
		}
	}
}

func TestEachSetsMachinesReadTheirOwnLabelsManifest(t *testing.T) {
	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	for _, label := range []string{"superci-2cpu", "superci-4cpu"} {
		got := bootData(cfg, "https://203.0.113.1:8443", "sha256//pin", label)
		if left := regexp.MustCompile(`@[A-Z_]+@`).FindString(got); left != "" {
			t.Fatalf("%s: a placeholder is left: %s", label, left)
		}
		if !strings.Contains(got, "/hydrate/"+label+".json") {
			t.Fatalf("%s: the machine does not read its own label's manifest", label)
		}
	}
}

func TestOnlyKeepsTheSizesItNames(t *testing.T) {
	path := filepath.Join(t.TempDir(), "override.json")
	override := `{"deployment":"superci-pull-timeline","only":["superci-96cpu"],"sizes":{"superci-96cpu":{"cpu":2,"split":1,"max":10,"disk_gb":40,"types":["c7i.large"]}}}`
	if err := os.WriteFile(path, []byte(override), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Sizes) != 1 || c.Sizes["superci-96cpu"].CPU != 2 || len(c.Pools) != 0 {
		t.Fatalf("sizes %v pools %v, want only superci-96cpu", c.Sizes, c.Pools)
	}
}

// The ceiling is what we alone may register in one window: the burst at once and the rate over the window. It starts
// low (2 a second, 50 at once: 650 in 5 minutes) and is raised by hand, a step after each quiet day (README).
func TestTheCeilingStartsAt2And50(t *testing.T) {
	c, err := LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	if r := c.Registration; r.CeilingRate != 2 || r.CeilingBurst != 50 {
		t.Fatalf("the built-in ceiling is %v a second and %d at once, want 2 and 50", r.CeilingRate, r.CeilingBurst)
	}
}

func TestACeilingThatCouldPassTheCapInOneWindowIsRefused(t *testing.T) {
	dir := t.TempDir()
	for _, c := range []struct {
		rate  string
		burst int
		ok    bool
	}{
		{"2", 50, true},  // 650
		{"3", 100, true}, // 1,000
		{"4", 150, true}, // 1,350
		{"4", 300, true}, // 1,500
		{"4", 301, false},
		{"5", 150, false}, // 1,650
		{"5", 1, false},
		{"6", 50, false}, // 1,850
	} {
		path := filepath.Join(dir, "config.json")
		body := fmt.Sprintf(`{"registration": {"ceiling_rate": %s, "ceiling_burst": %d}}`, c.rate, c.burst)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := LoadConfig(path)
		if c.ok && err != nil {
			t.Fatalf("a ceiling of %s a second and %d at once refused: %v", c.rate, c.burst, err)
		}
		if !c.ok && (err == nil || !strings.Contains(err.Error(), "ceiling")) {
			t.Fatalf("a ceiling of %s a second and %d at once accepted (%v): over a 300 s window it passes the 1500 cap", c.rate, c.burst, err)
		}
	}
}
