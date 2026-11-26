package main

import (
	"context"
	"log/slog"
	"math"
	"sort"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
)

// Namespace holds every metric the listener publishes, once a minute. The alarms in deploy/alarms.sh read them.
const Namespace = "superci-pull"

// Metrics is what one minute's publication reads.
type Metrics struct {
	Budget   BudgetSnapshot
	Spend    SpendSnapshot
	Sets     []Snapshot
	Waits    []float64            // queue waits of jobs started this minute, seconds
	SetWaits map[string][]float64 // the same, by set
	Orphans  int                  // orphaned machines and runners of every set, and machines of no configured set
	Regs     int                  // registrations GitHub made this minute
	Refused  int                  // registrations GitHub refused this minute
	Failed   int                  // registrations that failed otherwise this minute
	Short    int                  // machines asked of EC2 this minute that did not start (capacity, quotas)
	Stalled  int                  // sets whose jobs have waited 10 minutes with none started
	AtFloor  int                  // sets whose pool is at the hour's floor while more jobs wait than it has idle runners
	Denied   map[string]int       // registrations the budget turned away this minute, by reason
}

func publish(ctx context.Context, cw *cloudwatch.Client, m Metrics, now time.Time) {
	data := metricData(m, now)
	for i := 0; i < len(data); i += 500 {
		if _, err := cw.PutMetricData(ctx, &cloudwatch.PutMetricDataInput{Namespace: aws.String(Namespace), MetricData: data[i:min(len(data), i+500)]}); err != nil {
			slog.Error("metrics", "err", err.Error())
		}
	}
}

// metricData is the datapoints of one minute's publication.
func metricData(m Metrics, now time.Time) []cwtypes.MetricDatum {
	at := aws.Time(now)
	d := func(name string, v float64, unit cwtypes.StandardUnit, dims ...cwtypes.Dimension) cwtypes.MetricDatum {
		return cwtypes.MetricDatum{MetricName: aws.String(name), Value: aws.Float64(v), Unit: unit, Timestamp: at, Dimensions: dims}
	}
	data := []cwtypes.MetricDatum{
		d("Heartbeat", 1, cwtypes.StandardUnitCount),
		d("OurRegistrationRate", m.Budget.OursRate, cwtypes.StandardUnitCountSecond),
		d("SpareRegistrationRate", m.Budget.SpareRate, cwtypes.StandardUnitCountSecond),
		d("OrgRateKnown", b2f(m.Budget.Known), cwtypes.StandardUnitCount),
		d("Registrations", float64(m.Regs), cwtypes.StandardUnitCount),
		d("RegistrationRefusals", float64(m.Refused), cwtypes.StandardUnitCount),
		d("RegistrationErrors", float64(m.Failed), cwtypes.StandardUnitCount),
		d("LaunchShortfall", float64(m.Short), cwtypes.StandardUnitCount),
		d("Orphans", float64(m.Orphans), cwtypes.StandardUnitCount),
		d("SpendTodayUSD", m.Spend.DayUSD, cwtypes.StandardUnitNone),
		d("SpendMonthUSD", m.Spend.MonthUSD, cwtypes.StandardUnitNone),
		d("SpendPerHourUSD", m.Spend.PerHour, cwtypes.StandardUnitNone),
		d("SpendCapped", b2f(m.Spend.Capped), cwtypes.StandardUnitCount),
		d("SpendDailyCapUSD", m.Spend.DailyCap, cwtypes.StandardUnitNone),
		d("StalledSets", float64(m.Stalled), cwtypes.StandardUnitCount),
		d("PoolsAtFloor", float64(m.AtFloor), cwtypes.StandardUnitCount),
	}
	if m.Budget.Known {
		data = append(data, d("OrgRegistrationRate", m.Budget.OrgRate, cwtypes.StandardUnitCountSecond),
			d("OthersRegistrationRate", m.Budget.OthersRate, cwtypes.StandardUnitCountSecond),
			d("RegistrationWindowUse", m.Budget.WindowUse, cwtypes.StandardUnitCount))
	}
	machines, od := 0, 0
	var oldestJob, oldestMachine float64
	for _, s := range m.Sets {
		set := cwtypes.Dimension{Name: aws.String("Set"), Value: aws.String(s.Set)}
		data = append(data, d("Machines", float64(s.Machines), cwtypes.StandardUnitCount, set), d("IdleRunners", float64(s.Idle), cwtypes.StandardUnitCount, set),
			d("BusyRunners", float64(s.Busy), cwtypes.StandardUnitCount, set), d("Pool", float64(s.Pool), cwtypes.StandardUnitCount, set),
			d("JobsWaiting", float64(max(0, s.Stats.TotalAssignedJobs-s.Stats.TotalRunningJobs+s.Stats.TotalAvailableJobs)), cwtypes.StandardUnitCount, set))
		data = append(data, d("OldestWaitingJobAgeS", s.OldestJobS, cwtypes.StandardUnitSeconds, set),
			d("OldestUnregisteredMachineAgeS", s.OldestUnregisteredS, cwtypes.StandardUnitSeconds, set))
		machines += s.Machines
		od += s.OnDemand
		oldestJob, oldestMachine = max(oldestJob, s.OldestJobS), max(oldestMachine, s.OldestUnregisteredS)
	}
	// The alarms read these without a dimension: the longest waits of any set.
	data = append(data, d("OldestWaitingJobAgeS", oldestJob, cwtypes.StandardUnitSeconds),
		d("OldestUnregisteredMachineAgeS", oldestMachine, cwtypes.StandardUnitSeconds))
	for reason := DeniedWindow; reason < denialKinds; reason++ {
		data = append(data, d("BudgetDenied", float64(m.Denied[reason.String()]), cwtypes.StandardUnitCount,
			cwtypes.Dimension{Name: aws.String("Reason"), Value: aws.String(reason.String())}))
	}
	data = append(data, d("Machines", float64(machines), cwtypes.StandardUnitCount), d("OnDemandShare", float64(od)/math.Max(1, float64(machines)), cwtypes.StandardUnitNone))
	data = append(data, queueWaitData(m, at)...)
	return data
}

// queueWaitData is the minute's QueueWait: every set's waits together (what the alarm reads), and each set's own under
// the Set dimension, so one set's long waits do not hide in the p90 of the rest.
func queueWaitData(m Metrics, at *time.Time) []cwtypes.MetricDatum {
	data := waitDatum(m.Waits, at, nil)
	sets := make([]string, 0, len(m.SetWaits))
	for set := range m.SetWaits {
		sets = append(sets, set)
	}
	sort.Strings(sets)
	for _, set := range sets {
		data = append(data, waitDatum(m.SetWaits[set], at, []cwtypes.Dimension{{Name: aws.String("Set"), Value: aws.String(set)}})...)
	}
	return data
}

// waitDatum is waits as one QueueWait histogram (a value and its count), in as many datums as CloudWatch takes.
func waitDatum(waits []float64, at *time.Time, dims []cwtypes.Dimension) []cwtypes.MetricDatum {
	if len(waits) == 0 {
		return nil
	}
	counts := map[float64]float64{}
	for _, w := range waits {
		counts[math.Round(w)]++
	}
	values, weights := make([]float64, 0, len(counts)), make([]float64, 0, len(counts))
	for v, c := range counts {
		values, weights = append(values, v), append(weights, c)
	}
	var data []cwtypes.MetricDatum
	for i := 0; i < len(values); i += 150 {
		j := min(len(values), i+150)
		data = append(data, cwtypes.MetricDatum{MetricName: aws.String("QueueWait"), Values: values[i:j], Counts: weights[i:j],
			Unit: cwtypes.StandardUnitSeconds, Timestamp: at, Dimensions: dims})
	}
	return data
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
