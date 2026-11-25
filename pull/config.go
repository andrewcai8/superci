package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strconv"
	"time"
)

//go:embed config.json
var builtinConfig []byte

// Config is everything an operator may change. The built-in file holds the defaults; the override file, when it
// exists, is decoded over them, so it names only what it changes (a "pools" or "sizes" entry replaces that size's
// whole entry). A field the listener does not know is an error, as is an hourly array of other than 24 values. The
// listener rereads the override file when it changes: pools, caps and limits apply at once; sizes, splits, types,
// disks, regions, the deployment and its repositories apply at the next start.
type Config struct {
	Account      string                `json:"account"`
	Org          string                `json:"org"`
	Installation int64                 `json:"installation"`
	AppParam     string                `json:"app_param"`
	Deployment   string                `json:"deployment"`   // tags its machines and names its runner group: a listener acts only on its own
	Repositories []string              `json:"repositories"` // the repositories its runner group serves, and whose lost jobs it re-runs
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
	Floor Hours `json:"floor"`
	Cap   Hours `json:"cap"`
}

// Hours is one value for each hour of the day (UTC): exactly 24.
type Hours [24]int

func (h *Hours) UnmarshalJSON(b []byte) error {
	var v []int
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	if len(v) != 24 {
		return fmt.Errorf("%d values, want one for each hour (24)", len(v))
	}
	copy(h[:], v)
	return nil
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
	PoolShare   float64 `json:"pool_share"`
	BlindRate   float64 `json:"blind_rate"`
	Concurrency int     `json:"concurrency"`
}

func LoadConfig(path string) (Config, error) {
	var c Config
	if err := decodeStrict(builtinConfig, &c); err != nil {
		return c, fmt.Errorf("built-in config: %w", err)
	}
	if path != "" {
		raw, err := os.ReadFile(path)
		switch {
		case errors.Is(err, os.ErrNotExist):
		case err != nil:
			return c, err
		default:
			if err := decodeStrict(raw, &c); err != nil {
				return c, fmt.Errorf("%s: %w", path, err)
			}
		}
	}
	return c, c.validate()
}

// decodeStrict decodes one JSON object over c, refusing a field Config does not have.
func decodeStrict(raw []byte, c *Config) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(c); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("more than one JSON value")
	}
	return nil
}

func (c Config) validate() error {
	if len(c.Sizes) == 0 || len(c.Regions) == 0 {
		return errors.New("config: no sizes or no regions")
	}
	for r, w := range c.Regions {
		if w <= 0 {
			return fmt.Errorf("config: region %s needs a weight above 0", r)
		}
	}
	for label, s := range c.Sizes {
		if !ownSetName.MatchString(label) || s.CPU <= 0 || s.Split < 1 || s.Max < 0 || len(s.Types) == 0 || s.DiskGB <= 0 {
			return fmt.Errorf("config: size %s needs a name superci-<N>cpu, cpu, split >= 1, max, disk_gb and types", label)
		}
	}
	for label := range c.Pools {
		if _, ok := c.Sizes[label]; !ok {
			return fmt.Errorf("config: pools names %s, which is not a size", label)
		}
	}
	if c.Deployment == "" || len(c.Repositories) == 0 {
		return errors.New("config: deployment and repositories are needed")
	}
	if c.DailyUSD < 0 || c.MonthlyUSD < 0 || c.OnDemand < 0 || c.OnDemand > 1 {
		return errors.New("config: caps must not be negative, and on_demand_share is in [0, 1]")
	}
	if r := c.Registration; r.Cap <= 0 || r.WindowS <= 0 || r.Margin < 0 || r.Margin >= 1 || r.PoolShare < 0 || r.PoolShare > 1 {
		return errors.New("config: registration needs cap, window_s, a margin in [0, 1) and a pool_share in [0, 1]")
	}
	if c.JobMinutes <= 0 || c.MaxAgeHours <= 0 {
		return errors.New("config: job_minutes and max_age_hours must be positive")
	}
	return nil
}

// job is the longest a job may run: its timeout and a margin.
func (c Config) job() time.Duration { return time.Duration(c.JobMinutes+c.MarginMin) * time.Minute }

// maxAge is the oldest a machine may be and still be granted a runner, or keep an idle one.
func (c Config) maxAge() time.Duration { return time.Duration(c.MaxAgeHours * float64(time.Hour)) }

// life is the longest a machine may live: the oldest a pool machine may be, and the longest job after it.
func (c Config) life() time.Duration { return c.maxAge() + c.job() }

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
