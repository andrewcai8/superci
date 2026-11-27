package main

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

// Spend is what the machines have cost today and this month (UTC), accrued every reconcile from each running
// machine's price: its pool's spot price, or for on-demand (and a spot pool not yet priced) an estimate per vCPU,
// plus its gp3 volume and its public IPv4 address. Data transfer out is not counted. It is an estimate made to cap the
// bill, not the bill. It survives a restart in an SSM
// parameter. At either cap the listener launches nothing and keeps no pool, so the bill stops growing once the
// running jobs end.
type Spend struct {
	mu          sync.Mutex
	DailyCap    float64 `json:"-"`
	MonthlyCap  float64 `json:"-"`
	Day         string  `json:"day"`
	Month       string  `json:"month"`
	DayUSD      float64 `json:"day_usd"`
	MonthUSD    float64 `json:"month_usd"`
	PerHour     float64 `json:"per_hour"`
	BusyPerHour float64 `json:"-"` // of PerHour, the machines whose runner holds a job
	IdlePerHour float64 `json:"-"` // the rest: booting, waiting for a runner or for a job
	last        time.Time
	prices      map[string]float64 // zone/type -> spot $/h
	sets        map[string]SetSpec
}

const (
	onDemandPerVCPUHour = 0.155            // m8azn.3xlarge's list price ($1.24) over the 8 vCPUs of the set it serves, the most of the types used
	gp3PerGBHour        = 0.08 / 730       // storage
	gp3ExtraPerHour     = 125 * 0.04 / 730 // the defaults' extra: 250 MiB/s is 125 above the free 125, and 3,000 IOPS are free
	gp3PerMBpsHour      = 0.04 / 730       // a MiB/s above 125
	gp3PerIOPSHour      = 0.005 / 730      // an IOPS above 3,000
	ipv4PerHour         = 0.005            // every machine has a public IPv4 address (the default subnets give one)
)

func NewSpend(daily, monthly float64, sets []SetSpec, now time.Time) *Spend {
	s := &Spend{DailyCap: daily, MonthlyCap: monthly, prices: map[string]float64{}, sets: map[string]SetSpec{}, last: now,
		Day: now.UTC().Format("2006-01-02"), Month: now.UTC().Format("2006-01")}
	for _, spec := range sets {
		s.sets[spec.Name] = spec
	}
	return s
}

func (s *Spend) SetCaps(daily, monthly float64) {
	s.mu.Lock()
	s.DailyCap, s.MonthlyCap = daily, monthly
	s.mu.Unlock()
}

func (s *Spend) SetPrices(p map[string]float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range p {
		s.prices[k] = v
	}
}

// volumeExtraPerHour is what a machine's root volume costs an hour beyond its storage: the throughput over 125 MiB/s and the IOPS over 3,000.
func volumeExtraPerHour(spec SetSpec) float64 {
	iops, mbps := spec.RootVolume()
	return float64(mbps-125)*gp3PerMBpsHour + float64(max(0, iops-3000))*gp3PerIOPSHour
}

// price is a machine's cost an hour.
func (s *Spend) price(m Machine) float64 {
	spec := s.sets[m.Set]
	od := onDemandPerVCPUHour * float64(max(1, spec.CPU))
	p := od
	if m.Lifecycle == "spot" {
		if v, ok := s.prices[m.AZ+"/"+m.Type]; ok && v > 0 {
			p = v
		}
	}
	return p + float64(spec.DiskGB)*gp3PerGBHour + volumeExtraPerHour(spec) + ipv4PerHour
}

// Price is a machine's estimated cost an hour, for a record of the job it ran.
func (s *Spend) Price(m Machine) float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.price(m)
}

// Accrue adds the running machines' cost since the last accrual (at most five minutes of it, so a pause of the
// listener is not charged at today's fleet).
func (s *Spend) Accrue(now time.Time, ms []Machine) {
	s.mu.Lock()
	defer s.mu.Unlock()
	day, month := now.UTC().Format("2006-01-02"), now.UTC().Format("2006-01")
	if month != s.Month {
		s.Month, s.MonthUSD = month, 0
	}
	if day != s.Day {
		s.Day, s.DayUSD = day, 0
	}
	hours := min(5*time.Minute, max(0, now.Sub(s.last))).Hours()
	s.last = now
	perHour, busy := 0.0, 0.0
	for _, m := range ms {
		p := s.price(m)
		perHour += p
		if m.Runner != nil && m.Runner.Busy {
			busy += p
		}
	}
	s.PerHour, s.BusyPerHour, s.IdlePerHour = perHour, busy, perHour-busy
	s.DayUSD += perHour * hours
	s.MonthUSD += perHour * hours
}

// Over says whether a cap is reached.
func (s *Spend) Over() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return (s.DailyCap > 0 && s.DayUSD >= s.DailyCap) || (s.MonthlyCap > 0 && s.MonthUSD >= s.MonthlyCap)
}

type SpendSnapshot struct {
	Day         string  `json:"day"`
	DayUSD      float64 `json:"day_usd"`
	MonthUSD    float64 `json:"month_usd"`
	PerHour     float64 `json:"per_hour"`
	BusyPerHour float64 `json:"busy_per_hour"`
	IdlePerHour float64 `json:"idle_per_hour"`
	DailyCap    float64 `json:"daily_cap"`
	MonthlyCap  float64 `json:"monthly_cap"`
	Capped      bool    `json:"capped"`
}

func (s *Spend) Snapshot() SpendSnapshot {
	over := s.Over()
	s.mu.Lock()
	defer s.mu.Unlock()
	return SpendSnapshot{Day: s.Day, DayUSD: s.DayUSD, MonthUSD: s.MonthUSD, PerHour: s.PerHour, BusyPerHour: s.BusyPerHour, IdlePerHour: s.IdlePerHour, DailyCap: s.DailyCap,
		MonthlyCap: s.MonthlyCap, Capped: over}
}

// Load takes the totals a previous listener saved, if they are of this day and month.
func (s *Spend) Load(ctx context.Context, client *ssm.Client, name string) error {
	out, err := client.GetParameter(ctx, &ssm.GetParameterInput{Name: aws.String(name)})
	var missing *ssmtypes.ParameterNotFound
	if errors.As(err, &missing) {
		return nil
	}
	if err != nil {
		return err
	}
	var saved struct {
		Day, Month       string
		DayUSD, MonthUSD float64
	}
	if err := json.Unmarshal([]byte(aws.ToString(out.Parameter.Value)), &saved); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if saved.Month == s.Month {
		s.MonthUSD = saved.MonthUSD
	}
	if saved.Day == s.Day {
		s.DayUSD = saved.DayUSD
	}
	return nil
}

func (s *Spend) Save(ctx context.Context, client *ssm.Client, name string) error {
	s.mu.Lock()
	value, _ := json.Marshal(struct {
		Day, Month       string
		DayUSD, MonthUSD float64
	}{s.Day, s.Month, s.DayUSD, s.MonthUSD})
	s.mu.Unlock()
	_, err := client.PutParameter(ctx, &ssm.PutParameterInput{Name: aws.String(name), Type: ssmtypes.ParameterTypeString,
		Value: aws.String(string(value)), Overwrite: aws.Bool(true)})
	return err
}
