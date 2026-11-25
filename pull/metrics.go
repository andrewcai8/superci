package main

import (
	"context"
	"log/slog"
	"math"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
)

// Namespace holds every metric the listener publishes, once a minute. The alarms in deploy/alarms.sh read them.
const Namespace = "superci-pull"

// Metrics is what one minute's publication reads.
type Metrics struct {
	Budget  BudgetSnapshot
	Spend   SpendSnapshot
	Sets    []Snapshot
	Waits   []float64 // queue waits of jobs started this minute, seconds
	Orphans int       // orphaned machines and runners of every set, and machines of no configured set
	Regs    int       // registrations GitHub made this minute
	Refused int       // registrations GitHub refused this minute
	Failed  int       // registrations that failed otherwise this minute
}

func publish(ctx context.Context, cw *cloudwatch.Client, m Metrics, now time.Time) {
	at := aws.Time(now)
	d := func(name string, v float64, unit cwtypes.StandardUnit, dims ...cwtypes.Dimension) cwtypes.MetricDatum {
		return cwtypes.MetricDatum{MetricName: aws.String(name), Value: aws.Float64(v), Unit: unit, Timestamp: at, Dimensions: dims}
	}
	data := []cwtypes.MetricDatum{
		d("Heartbeat", 1, cwtypes.StandardUnitCount),
		d("OrgRegistrationRate", m.Budget.OrgRate, cwtypes.StandardUnitCountSecond),
		d("OthersRegistrationRate", m.Budget.OthersRate, cwtypes.StandardUnitCountSecond),
		d("OurRegistrationRate", m.Budget.OursRate, cwtypes.StandardUnitCountSecond),
		d("SpareRegistrationRate", m.Budget.SpareRate, cwtypes.StandardUnitCountSecond),
		d("RegistrationWindowUse", m.Budget.WindowUse, cwtypes.StandardUnitCount),
		d("OrgRateKnown", b2f(m.Budget.Known), cwtypes.StandardUnitCount),
		d("Registrations", float64(m.Regs), cwtypes.StandardUnitCount),
		d("RegistrationRefusals", float64(m.Refused), cwtypes.StandardUnitCount),
		d("RegistrationErrors", float64(m.Failed), cwtypes.StandardUnitCount),
		d("Orphans", float64(m.Orphans), cwtypes.StandardUnitCount),
		d("SpendTodayUSD", m.Spend.DayUSD, cwtypes.StandardUnitNone),
		d("SpendMonthUSD", m.Spend.MonthUSD, cwtypes.StandardUnitNone),
		d("SpendPerHourUSD", m.Spend.PerHour, cwtypes.StandardUnitNone),
		d("SpendCapped", b2f(m.Spend.Capped), cwtypes.StandardUnitCount),
	}
	machines, od := 0, 0
	for _, s := range m.Sets {
		set := cwtypes.Dimension{Name: aws.String("Set"), Value: aws.String(s.Set)}
		data = append(data, d("Machines", float64(s.Machines), cwtypes.StandardUnitCount, set), d("IdleRunners", float64(s.Idle), cwtypes.StandardUnitCount, set),
			d("BusyRunners", float64(s.Busy), cwtypes.StandardUnitCount, set), d("Pool", float64(s.Pool), cwtypes.StandardUnitCount, set),
			d("JobsWaiting", float64(max(0, s.Stats.TotalAssignedJobs-s.Stats.TotalRunningJobs+s.Stats.TotalAvailableJobs)), cwtypes.StandardUnitCount, set))
		machines += s.Machines
		od += s.OnDemand
	}
	data = append(data, d("Machines", float64(machines), cwtypes.StandardUnitCount), d("OnDemandShare", float64(od)/math.Max(1, float64(machines)), cwtypes.StandardUnitNone))
	if len(m.Waits) > 0 {
		counts := map[float64]float64{}
		for _, w := range m.Waits {
			counts[math.Round(w)]++
		}
		values, weights := make([]float64, 0, len(counts)), make([]float64, 0, len(counts))
		for v, c := range counts {
			values, weights = append(values, v), append(weights, c)
		}
		for i := 0; i < len(values); i += 150 {
			j := min(len(values), i+150)
			data = append(data, cwtypes.MetricDatum{MetricName: aws.String("QueueWait"), Values: values[i:j], Counts: weights[i:j],
				Unit: cwtypes.StandardUnitSeconds, Timestamp: at})
		}
	}
	for i := 0; i < len(data); i += 500 {
		if _, err := cw.PutMetricData(ctx, &cloudwatch.PutMetricDataInput{Namespace: aws.String(Namespace), MetricData: data[i:min(len(data), i+500)]}); err != nil {
			slog.Error("metrics", "err", err.Error())
		}
	}
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
