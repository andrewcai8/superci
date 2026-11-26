# timeline

Where every second of a CI job goes, on superci and on Blacksmith. Python 3 standard library, `gh` and `aws` on the path
(`--profile ci-runners`, never another account).

```
timeline job <job-id>                                   one job, phase by phase
timeline compare --workflow lint-test-backend.yml --job verify-tools --since 2026-11-25T17:03:43Z [--until T]
                 [--baseline-since T --baseline-until T]    superci against Blacksmith, p50 and p90 with n
timeline critical-path --run <run-id>                   which job and phase set the run's wall time
timeline export --day 2026-11-25                        copy a day of the logs to S3 (CloudWatch keeps 30 days)
```

`--legacy` also rebuilds launches and registrations from CloudTrail and the old listener lines, for a window before the
listener wrote events. `--cache DIR` keeps GitHub's answers (a finished job never changes), so a rerun costs no calls.

## What it reads

| Source | Gives |
| --- | --- |
| Listener `tl` lines, group `/superci-pull/listener` ([schema](../../timeline.schema.json)) | job seen, launch, ask, token wait, registration, the machine's marks (boot, cache daemon, runner), decisions |
| EC2 state-change notifications, group `/superci-pull/ec2-state` in each region (`deploy/timeline.sh`) | pending, running, shutting-down, terminated |
| GitHub REST job | created, started, completed, every step, runner and group; the only source for Blacksmith |
| A job's log (`timeline job`) | cache restores of the step logs: key, hit or miss, bytes, seconds |

The log is the system of record. A JobTimeline is derived on each run and never stored; the lines are append-only and keyed
by the job's GUID (`job`, with its set; the runner request id is 0 in the messages this listener gets) and the machine's instance id (`inst`, which is the runner's name less its set).

## How the seconds are accounted

A job's wall time is GitHub's `created_at` to `completed_at`. Its phases are the intervals between consecutive milestones
(`MILESTONES` in `timeline.py`, in the order a job meets them), then its steps. A phase has one owner (github, listener,
ec2, machine, network, runner).

- **Every second lies in a named phase, or in an `unobserved` interval.** When a milestone the job's class expects has no
  source, the interval around it is named `unobserved: <a> to <b> (no <c>)` and counted in `unexplained`. For a job on a
  warm machine (the machine's milestones came before the job was queued) only the milestones after the queue are expected.
  A Blacksmith job expects the API's alone.
- **`overlap clamped`** is time that two sources disagree on by less than their resolution (GitHub's timestamps are whole
  seconds; a machine's clock may run ahead). It is reported, not hidden.
- **A machine clock is corrected on every post.** `offset_ms` is the listener's estimate of how far the machine's clock is
  behind its own: the smallest of the machine's last 8 posts' receipt less stamp, since a post's own delay is in each and
  the smallest holds the least. (Lines written before Nov 26 carry `skew_ms`, one post's receipt less stamp, delay
  included; the tool reads both.) A machine's clock is seconds off at boot until chrony steps it (1.7 s on the first
  live run, 36 ms by the end), so each mark moves by the offset of its own post, not by one offset for the machine. A
  job reports the largest offset, and `clock_corrected` when one passed 250 ms.
- **Warm or cold** is the machine's launch against the job's creation. With no launch event and a queue wait under 6 s the
  machine is `warm (inferred)`: a machine cannot launch and boot that fast.

## Events and where they are written

| Event | Written by |
| --- | --- |
| `job.record` | `scaler.Scale` (`main.go`), through `tlJobRecord` (`jobs.go`): one line per finished job, the gate's and the cost report's source |
| `job.assigned` (first sight; GitHub sends JobAssigned, never JobAvailable), `job.acquired`, `job.started`, `job.completed` | `scaler.Scale` (`main.go`), through `tlMessage` |
| `machine.launched`, `machine.ended` | `SetFleet.ask`, `Fleet.Terminate` (`fleet.go`) |
| `jit.ask`, `jit.granted` (token wait), `jit.claim` (the fence's claim: taken, refused, error), `jit.issued`, `jit.refused`, `machine.notice` | `Server.jit`, `Server.notice` (`server.go`) |
| `launch.decision`, `pool.target`, `machine.retired`, `listener.start` | the launcher and reconciler (`main.go`) |
| `boot.start`, `cache.start`, `cache.up`, `doc.got`, `jit.sent`, `jit.answered`, `runner.*`, `job.running`, `job.done`, `poweroff` | `boot.sh` on the machine, through a spool and `/mark` |
| `hydrate.start`, `hydrate.done` | pull-prehydrate, as marks (names are taken already) |
| `preread.start`, `preread.done` | the root volume's pre-read, after the hydration, in the same process (`boot.sh`) |
| `cache.restore`, `cache.save` | the cache daemon, appended to the same spool (`cache/internal/cache/marks.go`) |
| `ec2.state` | EventBridge, not the listener |

A new event is a line in the schema's `events`, an emit call, and a name in `markNames` if a machine posts it; the Go tests
fail when the code and the schema disagree. Fields are only added.

## Tests

`python3 -I test_timeline.py` (the derivation, clamping, gaps, the cache marks and log, the schema). The Go tests
(`events_test.go`, `cache/internal/cache/marks_test.go`) run with `deploy/test.sh`.
