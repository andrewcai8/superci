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

`--cache DIR` (before the subcommand) keeps GitHub's answers; a finished job never changes, so a rerun costs no calls.
Exit 0 only when every criterion passes. 1 is FAIL, or INCONCLUSIVE when every miss is a pass inside the noise.

## gate 1: `gate job`

A job is superci's by its own runner group (`superci-pull`), created from `--since` (default the last 24 h). `--branch-glob` is
required and picks only which runs from `--since` are read whole (`main` for a canary on main, `ci/superci-load-pull-*` for
load branches): without it every run would be read whole and `--max-runs` would bound nothing. Blacksmith's are the same job's
runs on Blacksmith over the `--baseline-hours` ending now, read from at most `--max-runs` (300) runs, an evenly spaced sample,
the same ones each rerun. `<job>` is the job's name, or a matrix job's name for all its legs.

**What counts.** Every attempt of a run is read (`filter=all`), so a failure that a rerun then passed still counts. A job's
outcome is `success`, `failure` or `timed_out`, and the last two are failures. A rerun attempt keeps its first attempt's
`created_at`, so only attempts after the first lose their queue wait; their failure and wall time are counted. A `timed_out`
job's wall time is raised to the job's ceiling, so wall p90 sees it. `cancelled` jobs have no outcome: they are left out of
every row and reported per fleet (runs and jobs) above the rows.

| Row | Passes when |
| --- | --- |
| n | at least 5 superci runs (runs, not shards) |
| wall p50 | superci p50 of started to completed, over successful runs and timed-out ones at the ceiling, at or below Blacksmith's |
| wall p90 | at or below the smaller of Blacksmith's p90 x 1.1 and the job's ceiling in `scripts/verify/ci-limits.tsv` on origin/main |
| queue wait p50, p90 | created to started: p50 at or below Blacksmith's, p90 at or below Blacksmith's plus 3 s |
| failures | at most Blacksmith's failure rate x superci observations + 1; a failure the same commit also has on Blacksmith (main runs there) is not counted. A superci failure never excuses another, main's included |
| unexplained p50 | under 2 s per `timeline`'s derivation |

**The noise floor.** Every quantile is interpolated, and above the rows the tool prints each fleet's min, p25, p50, p75, p90,
max and IQR. A speed row that passes by less than the superci sample's IQR is `NOISE`, not `PASS`: the spread is wider than
the gap, so add runs. Nothing is rounded toward either verdict.

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
| on-demand share | `machine.launched` lifecycle | at most 10% |
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

## Tests

`python3 -I test_gate.py`: a PASS and a FAIL for every criterion of both gates, the noise rule, the exclusion of a failure
Blacksmith shares (and not one superci or main-on-superci shares), reruns, timeouts and cancellations, the required flags, minute coverage of the registration datapoints, the alarm history, the trace extraction and the bench's per-bucket workflow, and the recorded
canary fixtures in `fixtures/`.
