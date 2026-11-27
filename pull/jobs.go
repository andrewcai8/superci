package main

import (
	"strings"
	"time"

	"github.com/actions/scaleset"
)

// JobRecord is the one durable record of a finished job: what GitHub's JobCompleted says of it joined, at the moment it
// completes, with the machine that ran it and what that machine cost. It is written once as the job.record event and
// is the only thing the gate, the cost report, the dashboard's job panels and `superci logs` read about a job, so no
// view needs GitHub's REST API for the jobs superci ran.
type JobRecord struct {
	Set         string
	Job         string // GitHub's job GUID
	Run         int64
	Name        string
	Repo        string
	WorkflowRef string // owner/repo/.github/workflows/<file>@refs/heads/<branch>
	Event       string // the workflow's trigger (push, pull_request, merge_group, schedule)
	Labels      []string
	Runner      string
	Inst        string
	Result      string // the message's own word: Succeeded, Failed, Canceled, Abandoned
	Queued      time.Time
	Started     time.Time // the runner's assignment, GitHub's started_at to within a second
	Finished    time.Time
	Machine     *Machine // a copy; nil when the listener holds no machine of the runner (after a restart, once it is gone)
	USD         float64  // the machine's estimated cost from its launch to the job's finish
	BusyUSD     float64  // the share of it spent while the job ran
	Dispatched  bool     // run by the identity plane: Queued is when an identity took the job, Started when its machine did
}

// Partial says the record lacks the job's start: its completion came after a listener restart, with no start time in the
// message and no started message kept. Its queue wait, wall time and busy share are unknown, not zero.
func (r JobRecord) Partial() bool { return r.Started.IsZero() }

// Conclusion is the job's outcome in GitHub's REST words, so the gate reads superci's and Blacksmith's alike: success,
// failure or cancelled. A message does not say timed_out, which GitHub's REST does; such a job is a failure here.
func (r JobRecord) Conclusion() string {
	switch strings.ToLower(r.Result) {
	case "succeeded":
		return "success"
	case "canceled", "cancelled", "abandoned":
		return "cancelled"
	default:
		return "failure"
	}
}

// QueueMS is the wait from GitHub queueing the job to a runner taking it; 0 when either time is missing.
func (r JobRecord) QueueMS() int64 { return spanMS(r.Queued, r.Started) }

// WallMS is the job's run, from its runner's assignment to its finish.
func (r JobRecord) WallMS() int64 { return spanMS(r.Started, r.Finished) }

// IdleUSD is the machine's cost outside the job: its boot, its time idle in the pool and its shutdown.
func (r JobRecord) IdleUSD() float64 {
	if r.Partial() {
		return 0
	}
	return max(0, r.USD-r.BusyUSD)
}

func spanMS(from, to time.Time) int64 {
	if from.IsZero() || to.IsZero() {
		return 0
	}
	return max(0, to.Sub(from).Milliseconds())
}

// newJobRecord joins a completed job with its machine (which may be nil) and prices the machine's life at price. b is what
// the job's started message said, for the times a completed message leaves out.
func newJobRecord(set string, j *scaleset.JobCompleted, m *Machine, price func(Machine) float64, b began) JobRecord {
	r := JobRecord{Set: set, Job: j.JobID, Run: j.WorkflowRunID, Name: j.JobDisplayName, Repo: j.OwnerName + "/" + j.RepositoryName,
		WorkflowRef: j.JobWorkflowRef, Event: j.EventName, Labels: j.RequestLabels, Runner: j.RunnerName, Inst: tlInstance(j.RunnerName), Result: j.Result,
		Queued: firstTime(j.QueueTime, b.queued), Started: firstTime(j.RunnerAssignTime, b.started), Finished: j.FinishTime}
	if m == nil {
		return r
	}
	c := *m
	r.Machine = &c
	if price != nil && !m.Launched.IsZero() && !r.Finished.IsZero() {
		perHour := price(c)
		r.USD = perHour * max(0, r.Finished.Sub(m.Launched).Hours())
		r.BusyUSD = min(r.USD, perHour*time.Duration(r.WallMS()*int64(time.Millisecond)).Hours())
	}
	return r
}

// tlJobRecord writes the job's record: t is when the job finished by GitHub's clock.
func tlJobRecord(r JobRecord, now time.Time) {
	kv := []any{"set", r.Set, "run", r.Run, "job", r.Job, "name", r.Name, "repo", r.Repo, "workflow_ref", r.WorkflowRef, "event_name", r.Event,
		"labels", r.Labels, "runner", r.Runner, "inst", r.Inst, "result", r.Result, "conclusion", r.Conclusion(),
		"queue_time", tlTime(r.Queued), "start_time", tlTime(r.Started), "finish_time", tlTime(r.Finished),
		"partial", r.Partial()}
	if r.Dispatched {
		kv = append(kv, "dispatch", true)
	}
	if !r.Partial() {
		kv = append(kv, "queue_ms", r.QueueMS(), "wall_ms", r.WallMS())
	}
	if m := r.Machine; m != nil {
		kv = append(kv, "region", m.Region, "az", m.AZ, "type", m.Type, "lifecycle", m.Lifecycle, "launched", tlTime(m.Launched),
			"warm", !m.Launched.IsZero() && m.Launched.Before(r.Queued), "usd", r.USD)
		if !r.Partial() {
			kv = append(kv, "busy_usd", r.BusyUSD)
		}
	}
	at := r.Finished
	if at.IsZero() {
		at = now
	}
	tl("job.record", at, kv...)
}
