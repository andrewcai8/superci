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
	"regexp"
	"slices"
	"sort"
	"strconv"
	"time"

	"github.com/actions/scaleset"
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
	Batch        BatchRule             `json:"batch"`
	DailyUSD     float64               `json:"daily_usd"`
	MonthlyUSD   float64               `json:"monthly_usd"`
	OnDemand     float64               `json:"on_demand_share"`
	JobMinutes   int                   `json:"job_minutes"`
	MarginMin    int                   `json:"margin_minutes"`
	MaxAgeHours  float64               `json:"max_age_hours"`
	Rerun        bool                  `json:"rerun_lost_jobs"`
	// Only, when set, keeps just the sizes it names (and their pools): a second deployment for a test serves one label of
	// its own, not every built-in size, which would carry the same labels as the production sets.
	Only []string `json:"only"`
	// Scope is where the sets register their runners: "organization" (the default) or "repository", which needs one
	// repository. GitHub counts its 1,500 registrations in 5 minutes per scope, and the organization's bucket is the one
	// every other runner of the organization fills.
	Scope string `json:"scope"`
}

// SizeConfig is one runner size: the label jobs ask for is its key (superci-<cpu>cpu).
type SizeConfig struct {
	CPU    int      `json:"cpu"`
	Split  int      `json:"split"`
	Max    int      `json:"max"`
	DiskGB int      `json:"disk_gb"`
	Types  []string `json:"types"`
	// OnDemandFirst is how many of the leading types are asked on demand, when their spot is short, before any slower type:
	// on_demand_share bounds only the on-demand machines of the others. 0 asks every type's spot first.
	OnDemandFirst int `json:"on_demand_first"`
}

// PoolConfig bounds one size's hot pool by the hour of the day (UTC): never fewer idle runners than floor, never more
// than cap. Between them the pool follows the bursts of the size's own jobs (PoolRule).
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

// PoolRule sizes a pool from the bursts a set's jobs come in (State.Burst): the pool that would have given a ready
// runner to the quantile of the jobs of the last window, given how long a launch takes to bring one (measured from the
// set's own launches; ready_seconds until there are enough), and no smaller than the largest of the last hold. Lead
// is how far ahead the next hour's bounds apply, so a busy hour's pool is registered slowly before it starts.
type PoolRule struct {
	Quantile      float64 `json:"quantile"`
	WindowMinutes int     `json:"window_minutes"`
	HoldMinutes   int     `json:"hold_minutes"`
	ReadySeconds  float64 `json:"ready_seconds"`
	LeadMinutes   int     `json:"lead_minutes"`
}

func (r PoolRule) window() time.Duration { return time.Duration(max(1, r.WindowMinutes)) * time.Minute }
func (r PoolRule) hold() time.Duration   { return time.Duration(r.HoldMinutes) * time.Minute }
func (r PoolRule) ready() time.Duration  { return time.Duration(r.ReadySeconds * float64(time.Second)) }

// RegConfig is GitHub's organization-wide cap on runner registrations and the share of it this listener leaves alone.
type RegConfig struct {
	Cap         int     `json:"cap"`
	WindowS     int     `json:"window_s"`
	PoolRate    float64 `json:"pool_rate"`
	PoolShare   float64 `json:"pool_share"`
	Concurrency int     `json:"concurrency"`
	// CeilingRate and CeilingBurst bound our own registrations whatever GitHub answers: a rate a second, and how many at
	// once. RecoverS is how long the admitted rate takes to climb from nothing back to the ceiling after GitHub refused.
	CeilingRate  float64 `json:"ceiling_rate"`
	CeilingBurst int     `json:"ceiling_burst"`
	RecoverS     int     `json:"recover_s"`
	// Reserve is the tokens of the saved-up burst only critical registrations (the required check's jobs) may take:
	// a batch registration needs them saved up besides its own. It is cut to the burst less one.
	Reserve int `json:"critical_reserve"`
}

// BatchRule names the jobs that may wait for registration budget: a job is batch when its event is one of Events, or
// its workflow ref (owner/repo/.github/workflows/x.yml@refs/...) or its display name matches one of the patterns.
// Every other job is critical, so a job no rule knows is never made to wait. A batch job still gets its runner: it
// takes what the critical jobs leave, and never the tokens the reserve holds.
type BatchRule struct {
	Events       []string `json:"events"`
	WorkflowRefs []string `json:"workflow_refs"`
	Jobs         []string `json:"jobs"`
	refs, jobs   []*regexp.Regexp
}

// compile checks the patterns and keeps them compiled.
func (r *BatchRule) compile() error {
	compile := func(kind string, ps []string) (out []*regexp.Regexp, err error) {
		for _, p := range ps {
			re, err := regexp.Compile(p)
			if err != nil {
				return nil, fmt.Errorf("config: batch %s pattern %q: %w", kind, p, err)
			}
			out = append(out, re)
		}
		return out, nil
	}
	var err error
	if r.refs, err = compile("workflow_refs", r.WorkflowRefs); err != nil {
		return err
	}
	r.jobs, err = compile("jobs", r.Jobs)
	return err
}

// Matches says whether a job is batch.
func (r BatchRule) Matches(j scaleset.JobMessageBase) bool {
	if slices.Contains(r.Events, j.EventName) {
		return true
	}
	for _, re := range r.refs {
		if re.MatchString(j.JobWorkflowRef) {
			return true
		}
	}
	for _, re := range r.jobs {
		if re.MatchString(j.JobDisplayName) {
			return true
		}
	}
	return false
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
	c.keepOnly()
	if err := c.Batch.compile(); err != nil {
		return c, err
	}
	return c, c.validate()
}

// keepOnly drops the sizes and pools Only does not name.
func (c *Config) keepOnly() {
	if len(c.Only) == 0 {
		return
	}
	for l := range c.Sizes {
		if !slices.Contains(c.Only, l) {
			delete(c.Sizes, l)
			delete(c.Pools, l)
		}
	}
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
		if s.OnDemandFirst < 0 || s.OnDemandFirst > len(s.Types) {
			return fmt.Errorf("config: size %s: on_demand_first %d is not between 0 and its %d types", label, s.OnDemandFirst, len(s.Types))
		}
	}
	for label, p := range c.Pools {
		if _, ok := c.Sizes[label]; !ok {
			return fmt.Errorf("config: pools names %s, which is not a size", label)
		}
		for h := range p.Floor {
			if p.Floor[h] < 0 || p.Cap[h] < 0 || p.Floor[h] > p.Cap[h] {
				return fmt.Errorf("config: pools %s at hour %d: floor %d and cap %d need 0 <= floor <= cap (PoolBounds would let a floor above the cap win silently)", label, h, p.Floor[h], p.Cap[h])
			}
		}
	}
	if c.Deployment == "" || len(c.Repositories) == 0 {
		return errors.New("config: deployment and repositories are needed")
	}
	switch c.Scope {
	case "", "organization":
	case "repository":
		if len(c.Repositories) != 1 {
			return fmt.Errorf("config: scope repository needs exactly one repository, not %d", len(c.Repositories))
		}
	default:
		return fmt.Errorf("config: scope %q is neither organization nor repository", c.Scope)
	}
	if c.DailyUSD < 0 || c.MonthlyUSD < 0 || c.OnDemand < 0 || c.OnDemand > 1 {
		return errors.New("config: caps must not be negative, and on_demand_share is in [0, 1]")
	}
	if r := c.Registration; r.Cap <= 0 || r.WindowS <= 0 || r.CeilingRate <= 0 || r.CeilingBurst < 1 || r.RecoverS < 1 || r.PoolShare < 0 || r.PoolShare > 1 || r.Reserve < 0 {
		return errors.New("config: registration needs cap, window_s, a ceiling_rate above 0, a ceiling_burst and recover_s of 1 or more, a pool_share in [0, 1] and a critical_reserve of 0 or more")
	}
	if r := c.Registration; r.CeilingRate*float64(r.WindowS)+float64(r.CeilingBurst) > float64(r.Cap) {
		return fmt.Errorf("config: registration ceiling_rate %v x window_s %d + ceiling_burst %d = %.0f registrations in one window, over the cap of %d",
			r.CeilingRate, r.WindowS, r.CeilingBurst, r.CeilingRate*float64(r.WindowS)+float64(r.CeilingBurst), r.Cap)
	}
	if p := c.Pool; p.Quantile <= 0 || p.Quantile > 1 || p.ReadySeconds <= 0 || p.HoldMinutes < 0 {
		return errors.New("config: pool needs a quantile in (0, 1], ready_seconds above 0 and hold_minutes of 0 or more")
	}
	if c.JobMinutes <= 0 || c.MaxAgeHours <= 0 {
		return errors.New("config: job_minutes and max_age_hours must be positive")
	}
	return nil
}

// repository is the repository the sets register at, or "" when they register at the organization.
func (c Config) repository() string {
	if c.Scope == "repository" {
		return c.Repositories[0]
	}
	return ""
}

// scopeURL is the GitHub URL the scale sets live under.
func (c Config) scopeURL() string {
	if r := c.repository(); r != "" {
		return "https://github.com/" + c.Org + "/" + r
	}
	return "https://github.com/" + c.Org
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
	// OnDemandFirst is SizeConfig's.
	OnDemandFirst int
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
				Max: int(math.Ceil(float64(s.Max) / float64(s.Split))), DiskGB: s.DiskGB, Types: s.Types, OnDemandFirst: s.OnDemandFirst})
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

// PoolTarget is the pool a set keeps given the burst its recent jobs ask for (State.Burst), within the hour's bounds.
func (c Config) PoolTarget(spec SetSpec, t time.Time, burst int) int {
	floor, top := c.PoolBounds(spec, t)
	return min(top, max(floor, burst))
}
