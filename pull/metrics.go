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
	Budget        BudgetSnapshot
	Spend         SpendSnapshot
	Sets          []Snapshot
	OnDemandFirst map[string]bool      // onDemandFirstSets: the sets whose size asks its leading types on demand first, by name
	Waits         []float64            // queue waits of jobs started this minute, seconds
	SetWaits      map[string][]float64 // the same, by set
	Orphans       int                  // orphaned machines and runners of every set, and machines of no configured set
	Regs          int                  // registrations GitHub made this minute
	Refused       int                  // registrations GitHub refused this minute
	Failed        int                  // registrations that failed otherwise this minute
	Short         int                  // machines asked of EC2 this minute that did not start (capacity, quotas)
	Stalled       int                  // sets whose jobs have waited 10 minutes with none started
	AtFloor       int                  // sets whose pool is at the hour's floor while more jobs wait than it has idle runners
	Denied        map[string]int       // registrations the budget turned away this minute, by reason
	Jobs          []JobRecord          // jobs that finished this minute, every set
	// Dispatch is the sets the identity plane serves, and DispatchCounts what each did this minute.
	Dispatch       []DispatchSnapshot
	DispatchCounts map[string]DispatchCounts
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
		d("AdmitRate", m.Budget.Rate, cwtypes.StandardUnitCountSecond),
		d("RegistrationQueue", float64(m.Budget.Queued), cwtypes.StandardUnitCount),
		d("Registrations", float64(m.Regs), cwtypes.StandardUnitCount),
		d("RegistrationRefusals", float64(m.Refused), cwtypes.StandardUnitCount),
		d("RegistrationErrors", float64(m.Failed), cwtypes.StandardUnitCount),
		d("LaunchShortfall", float64(m.Short), cwtypes.StandardUnitCount),
		d("Orphans", float64(m.Orphans), cwtypes.StandardUnitCount),
		d("SpendTodayUSD", m.Spend.DayUSD, cwtypes.StandardUnitNone),
		d("SpendMonthUSD", m.Spend.MonthUSD, cwtypes.StandardUnitNone),
		d("SpendPerHourUSD", m.Spend.PerHour, cwtypes.StandardUnitNone),
		d("SpendBusyPerHourUSD", m.Spend.BusyPerHour, cwtypes.StandardUnitNone),
		d("SpendIdlePerHourUSD", m.Spend.IdlePerHour, cwtypes.StandardUnitNone),
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
	machines, od, fastMachines, fastOD := 0, 0, 0, 0
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
		if m.OnDemandFirst[s.Set] {
			fastMachines, fastOD = fastMachines+s.Machines, fastOD+s.OnDemand
		}
		oldestJob, oldestMachine = max(oldestJob, s.OldestJobS), max(oldestMachine, s.OldestUnregisteredS)
	}
	for _, s := range m.Dispatch {
		set := cwtypes.Dimension{Name: aws.String("Set"), Value: aws.String(s.Set)}
		c := m.DispatchCounts[s.Set]
		data = append(data, d("Machines", float64(s.Machines), cwtypes.StandardUnitCount, set), d("Pool", float64(s.Pool), cwtypes.StandardUnitCount, set),
			d("JobsWaiting", float64(s.JobsWaiting), cwtypes.StandardUnitCount, set), d("OldestWaitingJobAgeS", s.OldestJobS, cwtypes.StandardUnitSeconds, set),
			d("DispatchIdleMachines", float64(s.Idle), cwtypes.StandardUnitCount, set), d("DispatchRunning", float64(s.Running), cwtypes.StandardUnitCount, set),
			d("DispatchHandoffs", float64(c.Handoffs), cwtypes.StandardUnitCount, set), d("DispatchRetries", float64(c.Retries), cwtypes.StandardUnitCount, set),
			d("DispatchResumes", float64(c.Resumes), cwtypes.StandardUnitCount, set), d("DispatchLostJobs", float64(c.Lost), cwtypes.StandardUnitCount, set),
			d("DispatchDroppedFrames", float64(c.Dropped), cwtypes.StandardUnitCount, set))
		machines += s.Machines
		od += s.OnDemand
		if m.OnDemandFirst[s.Set] {
			fastMachines, fastOD = fastMachines+s.Machines, fastOD+s.OnDemand
		}
		oldestJob = max(oldestJob, s.OldestJobS)
	}
	// The alarms read these without a dimension: the longest waits of any set.
	data = append(data, d("OldestWaitingJobAgeS", oldestJob, cwtypes.StandardUnitSeconds),
		d("OldestUnregisteredMachineAgeS", oldestMachine, cwtypes.StandardUnitSeconds))
	for reason := DeniedTokens; reason < denialKinds; reason++ {
		data = append(data, d("BudgetDenied", float64(m.Denied[reason.String()]), cwtypes.StandardUnitCount,
			cwtypes.Dimension{Name: aws.String("Reason"), Value: aws.String(reason.String())}))
	}
	// OnDemandFirstMachines and OnDemandFirstShare are over onDemandFirstSets (superci-4cpu): the share the alarm reads, which
	// the spot-only sets' machines (2 vCPU, 600 at most) do not dilute.
	data = append(data, d("Machines", float64(machines), cwtypes.StandardUnitCount), d("OnDemandShare", float64(od)/math.Max(1, float64(machines)), cwtypes.StandardUnitNone),
		d("OnDemandFirstMachines", float64(fastMachines), cwtypes.StandardUnitCount), d("OnDemandFirstShare", float64(fastOD)/math.Max(1, float64(fastMachines)), cwtypes.StandardUnitNone))
	data = append(data, queueWaitData(m, at)...)
	data = append(data, jobData(m.Jobs, at)...)
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

// jobData is the minute's view of the jobs that finished, derived from their job.record events (the same JobRecords the
// timeline writes, so a count here and a count of the log agree): JobsCompleted by result, JobWall of the jobs that
// succeeded, and JobCostUSD with the part of it spent outside the job. Each is published for every set together (what the
// alarms and the totals read) and for each set under the Set dimension, and JobsCompleted by Result under both. A
// partial record (its start unknown) counts as completed and as cost, and adds no wall time.
func jobData(jobs []JobRecord, at *time.Time) []cwtypes.MetricDatum {
	if len(jobs) == 0 {
		return nil
	}
	type key struct{ set, result string }
	count := map[key]float64{}
	wall := map[string][]float64{}
	cost, idle := map[string]float64{}, map[string]float64{}
	sets := map[string]bool{}
	for _, j := range jobs {
		sets[j.Set] = true
		for _, set := range []string{"", j.Set} {
			count[key{set, j.Conclusion()}]++
			cost[set] += j.USD
			idle[set] += j.IdleUSD()
			if j.Conclusion() == "success" && !j.Partial() {
				wall[set] = append(wall[set], float64(j.WallMS())/1000)
			}
		}
	}
	dims := func(set string, more ...cwtypes.Dimension) []cwtypes.Dimension {
		var d []cwtypes.Dimension
		if set != "" {
			d = append(d, cwtypes.Dimension{Name: aws.String("Set"), Value: aws.String(set)})
		}
		return append(d, more...)
	}
	names := []string{""}
	for set := range sets {
		names = append(names, set)
	}
	sort.Strings(names)
	var data []cwtypes.MetricDatum
	for _, set := range names {
		for _, result := range []string{"success", "failure", "cancelled"} {
			data = append(data, cwtypes.MetricDatum{MetricName: aws.String("JobsCompleted"), Value: aws.Float64(count[key{set, result}]), Unit: cwtypes.StandardUnitCount,
				Timestamp: at, Dimensions: dims(set, cwtypes.Dimension{Name: aws.String("Result"), Value: aws.String(result)})})
		}
		data = append(data, cwtypes.MetricDatum{MetricName: aws.String("JobCostUSD"), Value: aws.Float64(cost[set]), Unit: cwtypes.StandardUnitNone, Timestamp: at, Dimensions: dims(set)},
			cwtypes.MetricDatum{MetricName: aws.String("JobIdleCostUSD"), Value: aws.Float64(idle[set]), Unit: cwtypes.StandardUnitNone, Timestamp: at, Dimensions: dims(set)})
		for _, d := range waitDatum(wall[set], at, dims(set)) {
			d.MetricName = aws.String("JobWall")
			data = append(data, d)
		}
	}
	return data
}

// onDemandFirstSets marks each set whose size asks its leading types on demand before a slower type's spot (on_demand_first):
// the on-demand-share alarm reads those sets alone. A spot_first size asks no type on demand outside on_demand_share, so it
// is not one of them.
func onDemandFirstSets(specs []SetSpec) map[string]bool {
	out := map[string]bool{}
	for _, s := range specs {
		out[s.Name] = s.OnDemandFirst > 0
	}
	return out
}
