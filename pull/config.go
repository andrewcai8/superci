package main

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"time"
)

//go:embed config.json
var builtinConfig []byte

// Config is everything an operator may change. The built-in file holds the defaults; the override file, when it
// exists, is decoded over them, so it names only what it changes (a "pools" entry replaces that size's whole entry).
// The listener rereads the override file when it changes: pools, caps and limits apply at once; sizes, splits, types,
// disks, regions and cache variables apply at the next start.
type Config struct {
	Account      string                `json:"account"`
	Org          string                `json:"org"`
	Installation int64                 `json:"installation"`
	AppParam     string                `json:"app_param"`
	Regions      map[string]int        `json:"regions"`
	Sizes        map[string]SizeConfig `json:"sizes"`
	Pools        map[string]PoolConfig `json:"pools"`
	Pool         PoolRule              `json:"pool"`
	Registration RegConfig             `json:"registration"`
	DailyUSD     float64               `json:"daily_usd"`
	MonthlyUSD   float64               `json:"monthly_usd"`
	OnDemand     float64               `json:"on_demand_share"`
	JobMinutes   int                   `json:"job_minutes"`
	MarginMin    int                   `json:"margin_minutes"`
	MaxAgeHours  float64               `json:"max_age_hours"`
	Rerun        bool                  `json:"rerun_lost_jobs"`
	CacheEnv     map[string]string     `json:"cache_env"`
}

// SizeConfig is one runner size: the label jobs ask for is its key (superci-<cpu>cpu).
type SizeConfig struct {
	CPU    int      `json:"cpu"`
	Split  int      `json:"split"`
	Max    int      `json:"max"`
	DiskGB int      `json:"disk_gb"`
	Types  []string `json:"types"`
}

// PoolConfig bounds one size's hot pool by the hour of the day (UTC): never fewer idle runners than floor, never more
// than cap. Between them the pool follows the size's own arrivals (PoolRule).
type PoolConfig struct {
	Floor [24]int `json:"floor"`
	Cap   [24]int `json:"cap"`
}

// PoolRule sizes a pool from what arrives: enough idle runners to cover the arrivals of fill seconds (the time a new
// machine takes to bring a runner), times factor, over the last window. Lead is how far ahead the next hour's bounds
// apply, so a busy hour's pool is registered slowly before it starts.
type PoolRule struct {
	FillSeconds   float64 `json:"fill_seconds"`
	Factor        float64 `json:"factor"`
	WindowMinutes int     `json:"window_minutes"`
	LeadMinutes   int     `json:"lead_minutes"`
}

// RegConfig is GitHub's organization-wide cap on runner registrations and the share of it this listener leaves alone.
type RegConfig struct {
	Cap         int     `json:"cap"`
	WindowS     int     `json:"window_s"`
	Margin      float64 `json:"margin"`
	PoolRate    float64 `json:"pool_rate"`
	BlindRate   float64 `json:"blind_rate"`
	Concurrency int     `json:"concurrency"`
}

func LoadConfig(path string) (Config, error) {
	var c Config
	if err := json.Unmarshal(builtinConfig, &c); err != nil {
		return c, fmt.Errorf("built-in config: %w", err)
	}
	if path != "" {
		raw, err := os.ReadFile(path)
		switch {
		case errors.Is(err, os.ErrNotExist):
		case err != nil:
			return c, err
		default:
			if err := json.Unmarshal(raw, &c); err != nil {
				return c, fmt.Errorf("%s: %w", path, err)
			}
		}
	}
	return c, c.validate()
}

func (c Config) validate() error {
	if len(c.Sizes) == 0 || len(c.Regions) == 0 {
		return errors.New("config: no sizes or no regions")
	}
	for label, s := range c.Sizes {
		if s.CPU <= 0 || s.Split < 1 || s.Max < 0 || len(s.Types) == 0 {
			return fmt.Errorf("config: size %s needs cpu, split >= 1, max and types", label)
		}
	}
	if c.Registration.Cap <= 0 || c.Registration.WindowS <= 0 || c.Registration.Margin < 0 || c.Registration.Margin >= 1 {
		return errors.New("config: registration needs cap, window_s and a margin in [0, 1)")
	}
	if c.JobMinutes <= 0 || c.MaxAgeHours <= 0 {
		return errors.New("config: job_minutes and max_age_hours must be positive")
	}
	return nil
}

// A SetSpec is one runner scale set. A size with split K runs as K sets: the first is named after the label, the
// others <label>-2 … <label>-K, and every one of them carries the label, so a job asking for the label may go to any.
type SetSpec struct {
	Name   string
	Label  string
	Index  int // 0 for the first set of the size
	Split  int
	CPU    int
	Max    int
	DiskGB int
	Types  []string
}

func (c Config) Sets() []SetSpec {
	labels := make([]string, 0, len(c.Sizes))
	for l := range c.Sizes {
		labels = append(labels, l)
	}
	sort.Strings(labels)
	var out []SetSpec
	for _, l := range labels {
		s := c.Sizes[l]
		for k := 0; k < s.Split; k++ {
			name := l
			if k > 0 {
				name = l + "-" + strconv.Itoa(k+1)
			}
			out = append(out, SetSpec{Name: name, Label: l, Index: k, Split: s.Split, CPU: s.CPU,
				Max: int(math.Ceil(float64(s.Max) / float64(s.Split))), DiskGB: s.DiskGB, Types: s.Types})
		}
	}
	return out
}

// PoolBounds is one set's floor and cap at time t: the larger of this hour's and, within lead of the next hour, the
// next hour's, each divided among the size's sets.
func (c Config) PoolBounds(spec SetSpec, t time.Time) (floor, top int) {
	p, ok := c.Pools[spec.Label]
	if !ok {
		return 0, 0
	}
	hours := []int{t.UTC().Hour(), t.UTC().Add(time.Duration(c.Pool.LeadMinutes) * time.Minute).Hour()}
	for _, h := range hours {
		floor = max(floor, p.Floor[h])
		top = max(top, p.Cap[h])
	}
	share := func(n int) int { return int(math.Ceil(float64(n) / float64(spec.Split))) }
	return share(floor), max(share(floor), share(top))
}

// PoolTarget is the pool a set keeps given its recent arrival rate (jobs a second).
func (c Config) PoolTarget(spec SetSpec, t time.Time, rate float64) int {
	floor, top := c.PoolBounds(spec, t)
	want := int(math.Ceil(rate * c.Pool.FillSeconds * c.Pool.Factor))
	return min(top, max(floor, want))
}
