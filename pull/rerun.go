package main

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// Rerunner runs again, once, each job a spot reclaim took: a job on a machine AWS gave notice it takes back, or on a
// machine EC2 says a spot interruption terminated (a dispatched job the dispatcher lost to either says so itself,
// JobRef.Reclaimed), and each identity's job the dispatcher stranded before any machine got it (JobRef.Unrun). It re-runs a job only on that proof, and only if GitHub concluded
// the job failed, so a job that failed by itself, or that a person cancelled, is left as it ended. GitHub re-runs a
// job only once its whole run has completed, so a lost job waits here until then. A start rebuilds the list from the
// machines EC2 lists with a notice tag (for about an hour after a reclaim).
type Rerunner struct {
	mu    sync.Mutex
	rest  *REST
	proof func(ctx context.Context, ids []string) (map[string]bool, error) // which machines a spot reclaim ended
	lost  map[string]lostJob                                               // by key, so a job is run again at most once
}

type lostJob struct {
	JobRef
	at     time.Time
	proven bool // a spot reclaim took its machine: AWS's notice, or EC2's state reason
	done   bool
}

func NewRerunner(rest *REST, proof func(context.Context, []string) (map[string]bool, error)) *Rerunner {
	return &Rerunner{rest: rest, proof: proof, lost: map[string]lostJob{}}
}

// key names a lost job: its runner, which takes one job, or for an identity's job (which takes many) its machine.
func (j JobRef) key() string {
	if j.Instance != "" {
		return j.Instance
	}
	return j.Runner
}

// machine is the instance a lost job ran on.
func (j JobRef) machine() string {
	if j.Instance != "" {
		return j.Instance
	}
	return instanceOf(j.Runner)
}

// Add records a job lost with its machine; proven when AWS's notice already says a spot reclaim took the machine (or
// EC2's state did, for a dispatched job), or when no machine ran it, so its failure is superci's own. A dispatched job
// lost to a notice is failed before AWS takes its machine, often before EC2 could prove the reclaim; its run may
// complete first, so it carries its proof.
func (r *Rerunner) Add(j JobRef, now time.Time, proven bool) {
	proven = proven || j.Unrun || j.Reclaimed
	r.mu.Lock()
	defer r.mu.Unlock()
	if old, ok := r.lost[j.key()]; ok {
		if proven && !old.proven {
			old.proven = true
			r.lost[j.key()] = old
		}
		return
	}
	r.lost[j.key()] = lostJob{JobRef: j, at: now, proven: proven}
	slog.Warn("job lost with its machine", "owner", j.Owner, "repo", j.Repo, "run", j.RunID, "job", j.Name, "runner", j.Runner, "proven", proven)
}

// Rebuild adds, after a start forgot the list, each job a spot notice found running: the listener tags a noticed
// machine with its job's run (superci-pull-noticed), so EC2 keeps the proof and the job for the hour it lists the
// machine after the reclaim.
func (r *Rerunner) Rebuild(jobs []JobRef, now time.Time) {
	for _, j := range jobs {
		r.Add(j, now, true)
	}
	slog.Info("rebuilt lost jobs", "noticed", len(jobs))
}

// instanceOf is the machine a runner is named after: <set>-<instance id>[-<suffix>].
func instanceOf(runner string) string {
	i := strings.Index(runner, "-i-")
	if i < 0 {
		return ""
	}
	id, _, _ := strings.Cut(runner[i+3:], "-")
	return "i-" + id
}

func (r *Rerunner) Run(ctx context.Context) {
	for tick := time.NewTicker(time.Minute); ; {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		r.pass(ctx)
	}
}

// pass proves or drops each unproven lost job, and re-runs the proven ones whose run has completed.
func (r *Rerunner) pass(ctx context.Context) {
	r.mu.Lock()
	var todo []lostJob
	var unproven []string
	for name, j := range r.lost {
		switch {
		case j.done && time.Since(j.at) > 24*time.Hour, time.Since(j.at) > 8*time.Hour:
			delete(r.lost, name)
		case !j.done:
			todo = append(todo, j)
			if !j.proven {
				unproven = append(unproven, j.machine())
			}
		}
	}
	r.mu.Unlock()
	reclaimed := map[string]bool{}
	if len(unproven) > 0 {
		var err error
		if reclaimed, err = r.proof(ctx, unproven); err != nil {
			slog.Error("spot reclaim proof", "err", err.Error())
			return
		}
	}
	for _, j := range todo {
		if !j.proven && !reclaimed[j.machine()] {
			slog.Info("lost job not re-run: no spot reclaim took its machine", "run", j.RunID, "job", j.Name, "runner", j.Runner)
			r.settled(j)
			continue
		}
		j.proven = true
		if r.settle(ctx, j) {
			r.settled(j)
		} else {
			r.mu.Lock()
			r.lost[j.key()] = j
			r.mu.Unlock()
		}
	}
}

func (r *Rerunner) settled(j lostJob) {
	r.mu.Lock()
	defer r.mu.Unlock()
	j.done = true
	r.lost[j.key()] = j
}

// settle re-runs a lost job once its run has completed, if GitHub concluded it failed. It says whether the job is
// settled.
func (r *Rerunner) settle(ctx context.Context, j lostJob) bool {
	completed, err := r.rest.RunCompleted(ctx, j.Owner, j.Repo, j.RunID)
	if err != nil || !completed {
		return false
	}
	name := ""
	if j.Instance != "" {
		name = j.Name
	}
	id, conclusion, err := r.rest.JobOf(ctx, j.Owner, j.Repo, j.RunID, j.Runner, name)
	if err != nil {
		return false
	}
	if id == 0 || conclusion != "failure" {
		slog.Info("lost job needs no re-run", "run", j.RunID, "job", j.Name, "conclusion", conclusion)
		return true
	}
	err = r.rest.RerunJob(ctx, j.Owner, j.Repo, id)
	slog.Warn("re-ran lost job", "owner", j.Owner, "repo", j.Repo, "run", j.RunID, "job", j.Name, "job_id", id, "conclusion", conclusion, "err", errString(err))
	return err == nil
}
