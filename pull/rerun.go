package main

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Rerunner runs again, once, each job lost with its machine: a runner seen taking a job whose machine EC2 stopped
// listing before the job was reported complete (AWS took the spot machine back mid-job). GitHub re-runs a job only
// once its whole run has completed, so a lost job waits here until then. The list is in memory: a restart of the
// listener forgets the jobs it held, which then stay failed for a person to re-run.
type Rerunner struct {
	mu   sync.Mutex
	rest *REST
	lost map[string]lostJob // by runner name, so a job is run again at most once
}

type lostJob struct {
	JobRef
	at   time.Time
	done bool
}

func NewRerunner(rest *REST) *Rerunner { return &Rerunner{rest: rest, lost: map[string]lostJob{}} }

func (r *Rerunner) Add(j JobRef, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.lost[j.Runner]; !ok {
		r.lost[j.Runner] = lostJob{JobRef: j, at: now}
		slog.Warn("job lost with its machine", "owner", j.Owner, "repo", j.Repo, "run", j.RunID, "job", j.Name, "runner", j.Runner)
	}
}

func (r *Rerunner) Run(ctx context.Context) {
	for tick := time.NewTicker(time.Minute); ; {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		r.mu.Lock()
		var todo []lostJob
		for name, j := range r.lost {
			switch {
			case j.done && time.Since(j.at) > 24*time.Hour, time.Since(j.at) > 8*time.Hour:
				delete(r.lost, name)
			case !j.done:
				todo = append(todo, j)
			}
		}
		r.mu.Unlock()
		for _, j := range todo {
			if r.settle(ctx, j) {
				r.mu.Lock()
				j.done = true
				r.lost[j.Runner] = j
				r.mu.Unlock()
			}
		}
	}
}

// settle re-runs a lost job once its run has completed, if it did not succeed. It says whether the job is settled.
func (r *Rerunner) settle(ctx context.Context, j lostJob) bool {
	completed, err := r.rest.RunCompleted(ctx, j.Owner, j.Repo, j.RunID)
	if err != nil || !completed {
		return false
	}
	id, conclusion, err := r.rest.JobOf(ctx, j.Owner, j.Repo, j.RunID, j.Runner)
	if err != nil {
		return false
	}
	if id == 0 || conclusion == "success" || conclusion == "skipped" {
		slog.Info("lost job needs no re-run", "run", j.RunID, "job", j.Name, "conclusion", conclusion)
		return true
	}
	err = r.rest.RerunJob(ctx, j.Owner, j.Repo, id)
	slog.Warn("re-ran lost job", "owner", j.Owner, "repo", j.Repo, "run", j.RunID, "job", j.Name, "job_id", id, "conclusion", conclusion, "err", errString(err))
	return err == nil
}
