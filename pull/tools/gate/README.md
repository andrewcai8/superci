# gate

Decides, from measurements anyone can rerun, whether a CI job may move from Blacksmith to superci and whether the plane can
carry the load. Python 3 standard library, `gh`, and `aws --profile ci-runners` (reads only). It reuses
[`timeline`](../timeline/README.md) for GitHub and CloudWatch access, interpolated quantiles and the unexplained time.

```
gate job <job> --branch-glob 'ci/superci-load-pull-*' [--workflow lint-test-backend.yml] [--since T]
         [--baseline-hours 24] [--baseline-until T] [--max-runs 600] [--limits ci-limits.tsv]      gate 1, one job
gate scale --from T --to T --trace trace.json [--workflow superci-load.yml] [--branch-glob 'ci/superci-load-pull-*']
                                                                                               gate 2, a replay window
gate replay-trace --hour T [--hours 1] [--bucket 60] [--out trace.json]                         a real hour's arrivals
```

GitHub is read on the superci App's installation budget through a token the listener mints
([how, the refusal and the two personal-token exceptions](../../README.md#the-tools-read-github-on-the-apps-budget)):
with no App token `gate job` and `gate replay-trace` exit 2 before reading anything, unless `--personal-token` (before the
subcommand) spends the shared personal budget on purpose. `canary.sh` takes `SUPERCI_PERSONAL_TOKEN=1` for the same.

`--cache DIR` (before the subcommand) keeps GitHub's answers; a finished job never changes, so a rerun costs no calls.
Exit 0 only when every criterion passes. 1 is FAIL, or INCONCLUSIVE when every miss is a pass inside the noise.

## Where superci's jobs come from: `--superci-source auto|events|github`

By default (`auto`) `gate job` and `gate scale` read superci's jobs from the listener's `job.record` events (one per finished job: queue
wait, wall time, conclusion, workflow and branch, machine; [timeline](../timeline/README.md#events-and-where-they-are-written)),
through CloudWatch Logs Insights, so superci's side costs no GitHub call, whenever the window starts a minute or more after the listener's first
record; before that `auto` reads GitHub's REST API, so a window from before the records is never read as empty. A record is turned into the REST-shaped job the rest of the
tool reads, so every row below is decided by one code path. What differs from reading GitHub:

- `gate job` still reads Blacksmith's side from GitHub: one listing of the baseline window and the jobs of a spread sample of at most
  100 runs (`--max-runs`; 300 with `--superci-source github`), cached by `--cache`, so a canary's repeat checks of the same baseline cost
  nothing. A superci failure is excused by a Blacksmith failure of the same commit, and a record holds no commit: when Blacksmith has
  a failure in the sample, the head of each failed superci run is read (one call per failed run, cached).
- `gate scale` reads no GitHub call for jobs the scale sets ran.
- A job the identity plane ran (a record with `dispatch`) waited for a free identity at GitHub before its identity's shim brought it to the
  dispatcher, and the record's `queue_time` is when the shim did: a size short of identities would read as fast. Both commands read such a
  job's `created_at` from GitHub instead: one jobs listing per run (all attempts, cached), the job matched by name, by the runner the
  record names and by the completion nearest the record's finish. The row `dispatched wait from GitHub` fails when a record finds no job.
  A rerun attempt keeps its first attempt's `created_at`, so it counts for failures and wall time, not queue wait, as on GitHub's path.
  The timeline's unexplained row still starts where the dispatcher saw the job.
- The branch is the record's `workflow_ref`: `refs/heads/<branch>` for a push (the load branches, main, the merge queue). A pull request's
  ref is `refs/pull/<n>/merge`, which matches only a glob that fits it.
- The message has no attempt number, so every record counts as a first attempt (a dispatched one takes its attempt from its REST job): a rerun's queue wait is the one GitHub sent for it. It has no
  `timed_out` either: a failure that ran to within 5 s of the job's ceiling is read as one and counts at the ceiling, as GitHub's would. Without
  a ceiling row it is a failure with the wall time it ran.
- A job cancelled before any runner took it has a partial record (no start) with no runner: it is kept as a queue-wait lower bound, as on GitHub's
  path. Any other partial record (a completion after a listener restart, start unknown) is left out.
- A job in a reusable workflow carries the called workflow's file in `workflow_ref`, so `--workflow` must name that file, not the caller's, and the
  job's name is the message's, which may lack the `caller / ` prefix REST names carry. Neither is checked against live messages yet.
- Unexplained time is accounted to the moment the job's runner took it (a record has no steps); the row is about the plane's time.
- A window from before the first record (the listener writes them from the release that carried them) has none: `auto` reads GitHub there.
  Force one with `--superci-source events` or `github`.

## gate 1: `gate job`

A job is superci's by its own runner group (`superci-pull`), created from `--since` (default the last 24 h). `--branch-glob` is
required and picks only which runs from `--since` are read whole (`main` for a canary on main, `ci/superci-load-pull-*` for
load branches): without it every run would be read whole and `--max-runs` would bound nothing. Blacksmith's are the same job's
runs on Blacksmith over the `--baseline-hours` ending now, read from at most `--max-runs` (300) runs, an evenly spaced sample,
the same ones each rerun. `<job>` is the job's name, or a matrix job's name for all its legs.

**What counts.** Every attempt of a run is read (`filter=all`), so a failure that a rerun then passed still counts. A job's
outcome is `success`, `failure` or `timed_out`, and the last two are failures. A rerun attempt keeps its first attempt's
`created_at`, so only attempts after the first lose their queue wait; their failure and wall time are counted. A `timed_out`
job's wall time is raised to the job's ceiling, so wall p90 sees it.

**Cancelled jobs are lower bounds, on both fleets.** A cancelled job has no outcome, but it spent time, and a slow plane that
gets cancelled must not look fast. A job that started contributes a wall sample of started to completed (and its real queue
wait); a job cancelled in the queue contributes a queue-wait sample of created to completed (superci's is found by its
`superci-<n>cpu` label, since it has no runner yet). Whatever cancelled it counts, a newer push superseding the run
included: the jobs listing cannot tell them apart, and the same rule on Blacksmith keeps the p50 and p90 comparison fair. A
lower bound only under-reports, so a row that fails on them fails on the real times too. A cancelled job is never a run for
`n`, never a failure and never excuses one. The per-fleet cancelled counts (runs and jobs) are still printed above the rows.

| Row | Passes when |
| --- | --- |
| n | at least 5 superci runs (runs, not shards) |
| wall p50 | superci p50 of started to completed, over successful runs, timed-out ones at the ceiling and cancelled ones at the time they ran, at or below Blacksmith's |
| wall p90 | at or below the smaller of Blacksmith's p90 x 1.1 and the job's ceiling in `scripts/verify/ci-limits.tsv` on origin/main |
| queue wait p50, p90 | created to started (created to completed for a job cancelled in the queue): p50 at or below Blacksmith's, p90 at or below Blacksmith's plus 3 s |
| failures | at most Blacksmith's failure rate x superci observations + 1; a failure the same commit also has on Blacksmith (main runs there) is not counted. A superci failure never excuses another, main's included |
| unexplained p50 | under 2 s per `timeline`'s derivation |

**The noise floor.** Every quantile is interpolated, and above the rows the tool prints each fleet's min, p25, p50, p75, p90,
max and IQR. A speed row that passes by less than the superci sample's IQR is `NOISE`, not `PASS`: the spread is wider than
the gap, so add runs. Nothing is rounded toward either verdict.

## A live canary: `gate job --canary` and `canary.sh`

`gate job <job> --canary` prints the rows, then `canary <job>: KEEP` (exit 0) or `ROLL BACK (reasons)` (exit 3). A canary rolls
back on harm to superci's own jobs, judged against Blacksmith over the same window, and on nothing else:

- a FAIL of wall p50, wall p90, queue wait p50, queue wait p90 or failures. A p50 row counts once n passes (5 runs) and a
  p90 row at 20 runs, since a p90 of six runs is one job; the failures row counts from the first run. NOISE asks for runs
  and a row with no Blacksmith runs to compare has no verdict: neither rolls anything back;
- `superci-pull-listener-down` or `superci-pull-spend-capped` in ALARM: each stops every set, so it sends back every job listed;
- `superci-pull-oldest-job-wait-<size>` or `superci-pull-jobs-stalled-<size>` in ALARM, for a set the job ran on: a wait or a
  stall is one set's, so a canary reads the alarms of its own sets alone. The sets are the `set` of the job's superci records in
  the window (the superci-`<n>`cpu label of a job read from GitHub); with no superci record yet, the set of the label the job
  ran under on Blacksmith (`blacksmith-4vcpu-ubuntu-2404` is `superci-4cpu`), read from Blacksmith's listing with no further call;
  with neither, only the two above. A set with no alarm of its own (alarms.sh not yet run, `StalledSets` not yet published, a split
  set such as `superci-4cpu-2`), and a job with no set known, are judged on the listener-wide `superci-pull-oldest-job-wait` and
  `-jobs-stalled`, so the gate is never blind, only less partitioned where it cannot be; the line `judged on:` before the canary
  line says which alarm each set was judged on. Another set's alarm never sends the job back, so a new or unrelated size, or a cold machine
  of superci-8cpu, cannot make a superci-4cpu canary fragile (Nov 27 17:43Z: test-go's canary was rolled back on the
  listener-wide `oldest-job-wait`, which 8cpu's `net-tools` had tripped, with every test-go row passing). The reason names the
  alarm and so the set (`superci-pull-oldest-job-wait-4cpu in ALARM`). The listener-wide `oldest-job-wait` and `jobs-stalled`
  alarms still page, and the canary line lists them as `paging, no rollback`.

`superci-pull-registration-refusals` is not on the list. GitHub's cap of 1,500 registrations in 5 minutes is the
organization's, and a job moved from Blacksmith takes one registration on either fleet, so a refusal says the organization
is at the cap, not that superci harmed a job. It pages and is counted. If the refusals made superci's jobs wait, the
queue-wait rows and the per-set `superci-pull-oldest-job-wait-<size>` see it. In canary 4b's minutes over the cap (Nov 26 16:11-16:20Z, the
organization at 5.03-5.30/s) superci's test-go waited 1-2 s and Blacksmith's 4 vCPU jobs p50 7 s, p90 19 s.

`canary.sh SINCE UNTIL JOB...` runs the check for each job SUPERCI_JOBS still lists, every 15 minutes, until UNTIL, with
Blacksmith's side the 24 h before SINCE (`--baseline-until`), so a job that left Blacksmith still has a baseline. It
removes a job that gets ROLL BACK from SUPERCI_JOBS, and leaves the other jobs listed. `WORKFLOW` names the jobs' workflow
file (lint-test-backend.yml by default). A check that crashes exits 1 and rolls nothing back: read its output file.

## gate 2: `gate scale`

Reads the replay's runs (every run in the window whose workflow file is `--workflow` and whose branch matches `--branch-glob`; a load workflow that exists only on its branch has no name GitHub resolves), the listener's timeline events and CloudWatch, and lists the claims
bucket read-only.

| Row | Source | Passes when |
| --- | --- | --- |
| completion | the trace's job count (`--trace` is required, so a push that never created a run is a missing job), the jobs GitHub ran and `job lost with its machine` lines | completed equals the trace's count, none stranded, none lost |
| queue wait p50, p90 | replay jobs on `superci-pull` | at most 6 s and 10 s |
| org registrations per 5 min | `RegistrationWindowUse` (ours and others) and `OrgRateKnown` | peak at most 1,200 (80% of 1,500), with the rate known throughout and a datapoint for every minute of the replay window (a gap fails the row and names its first minute) |
| one runner per machine | `issued/` objects, `jit.issued` and `jit.claim` events | claimed set equals registered set, no machine registered or claimed twice. A refused duplicate grant is the fence working and is reported |
| orphans 10 min after | `Orphans` from `to`+10 min | 0 |
| on-demand share | `machine.launched` lifecycle and step | at most 10%, not counting a size's `on_demand_first` types (step `fast-*`), which on_demand_share does not bound |
| alarms at the end | `superci-pull-*` alarm history at `to` | none in ALARM |
| QueueWait published | `QueueWait` sample count | above 0 |

A source with no data fails its row: the gate never passes on missing evidence. Run the orphan row 10 minutes after the replay.

## `gate replay-trace` and the bench

`gate replay-trace --hour 2026-11-25T14:00:00Z` lists every workflow run created in the hour (GitHub caps a query at 1,000, so
the window is halved as needed), reads their jobs, and writes a trace of the Linux jobs: creation offset, runner label as a
`superci-<n>cpu` label (Blacksmith's `blacksmith-<n>vcpu-ubuntu-<v>` for the sizes superci has), and run time. Other runner
kinds and jobs that never ran are counted under `dropped`.

```
replay.sh BRANCH trace.json        one push per bucket at its real offset; each job sleeps the run time it really had
DRY_RUN=1 replay.sh BRANCH trace.json   prints the schedule and each push's workflow size, pushes nothing
```

`replay.sh`'s existing mode (`BRANCH WAVES INTERVAL BURST_AT WORKFLOW`) is unchanged. Pass the same trace to
`gate scale --trace`: "replayed" is the trace's job count.

## First exec: `spawn.py`

A job's first exec of a binary reads it from the machine's volume. On Nov 26 a superci pool machine, idle for minutes,
started `node` in 274-403 ms and `docker` in 1.7-3.8 s, where Blacksmith took 41 and 813 ms. Four runs of a test that
spawns child processes timed out at bun's 10 s, all on superci legs. Gate 1's wall rows did not catch it. The spawn row does:

```
spawn.py <superci run id> <blacksmith run id>      two dispatches of spawn-probe.yml on a load branch, one per fleet
spawn.py --logs <superci log> <blacksmith log>    the same from saved `gh run view --log` output
```

The probe prints its runner, then each measure as milliseconds and the command's exit code. A failed or absent command is no
sample. The bound is Blacksmith's median x 1.25 + 20 ms:

- **First exec** (node, python, docker's CLI): superci's slowest probe job, since one cold machine in five is the failure.
- **Throughput** (500 fork-execs of `true`, 50 runs of `node -e 0`): superci's median.
- A gated measure with fewer than 5 samples on either fleet is MISSING, which fails. The probe runs 6 jobs.
- bun, git, docker's first daemon call and a cold read of 400 large files are printed, not gated.
- Exit 0 on PASS, 1 on FAIL, 2 when the first run is not superci's or the second not Blacksmith's.

## Tests

`python3 -I test_gate.py`: a PASS and a FAIL for every criterion of both gates, the canary's rollback rows and alarms (canary 4b's refusals keep it), the noise rule, the exclusion of a failure
Blacksmith shares (and not one superci or main-on-superci shares), reruns, timeouts and cancellations, the required flags, minute coverage of the registration datapoints, the alarm history, the trace extraction and the bench's per-bucket workflow, and the recorded
canary fixtures in `fixtures/`. `python3 -I test_spawn.py`: a warmed fleet passes, one cold machine in five fails, the bound's edge,
failed commands, the 5-sample minimum per measure and fleet, shown rows, the echoed script with ANSI and CRLF, a retried
attempt, and `main`'s exit codes.
