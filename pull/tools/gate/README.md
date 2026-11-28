# gate

Decides, from measurements anyone can rerun, whether a CI job may move from Blacksmith to superci and whether the plane can
carry the load. Python 3 standard library, `gh`, and `aws --profile ci-runners` (reads only). It reuses
[`timeline`](../timeline/README.md) for GitHub and CloudWatch access, interpolated quantiles and the unexplained time.

```
gate job <job> --branch-glob 'ci/superci-load-pull-*' [--workflow lint-test-backend.yml] [--since T]
         [--baseline-hours 24] [--baseline-until T] [--max-runs 600] [--limits ci-limits.tsv]      gate 1, one job
         [--split 10|50|90]                                                                        ... a split key: one window, no baseline
gate scale --from T --to T --trace trace.json [--workflow superci-load.yml] [--branch-glob 'ci/superci-load-pull-*']
                                                                                               gate 2, a replay window
gate replay-trace --hour T [--hours 1] [--bucket 60] [--out trace.json]                         a real hour's arrivals
```

GitHub is read on the superci App's installation budget through a token the listener mints
([how, the refusal and the one personal-token exception](../../README.md#the-tools-read-github-on-the-apps-budget)):
with no App token `gate job` and `gate replay-trace` exit 2 before reading anything, unless `--personal-token` (before the
subcommand) spends the shared personal budget on purpose. `canary.sh` takes `SUPERCI_PERSONAL_TOKEN=1` for the same. `ci-limits.tsv` is read on the App's
`contents` token (a refusal says to pass `--limits FILE`).
Every call on an App token (the gate's and `canary.sh`'s) also sets `GH_SHIM_KEEP_TOKEN=1`, so a host `gh` wrapper that routes reads through another App keeps superci's token; the personal-token paths do not set it.

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
| failures | failed **runs**, not legs (a run fails when one of its non-cancelled counted legs does, so one broken PR failing 15 legs is one run), at most the smallest k with P(X > k) <= 1% (`FALSE_ROLLBACK_RATE`) for X ~ Binomial(superci runs, Blacksmith's per-run failure rate), summed exactly and never below 1 (a clean baseline does not make one red run evidence; see [the bound](#the-failures-row-counts-runs-against-a-binomial-bound)); a failure the same commit also has on Blacksmith (main runs there) is not counted. A superci failure never excuses another, main's included. For `unit-tests` and `integration`, a failure whose every failing test is red on Blacksmith's main too is excluded and listed ([below](#failing-tests-and-window-floors)) |
| unexplained p50 | under 2 s per `timeline`'s derivation |

**The noise floor.** Every quantile is interpolated, and above the rows the tool prints each fleet's min, p25, p50, p75, p90,
max and IQR. A speed row that passes by less than the superci sample's IQR is `NOISE`, not `PASS`: the spread is wider than
the gap, so add runs. Nothing is rounded toward either verdict.

## Failing tests and window floors

**Failing tests (`unit-tests`, `integration`).** A test red on both runners is not a superci regression, so these two jobs'
failures row reads each failed job's failing tests (`NAME_SOURCES` in `gate.py`; any other job keeps the job-level rule and the
output says so: `failing tests: no test-name source for <job>`).

- **Source: the job's log**, one `GET actions/jobs/<id>/logs` per failed job, on Actions: read, which the App holds (the timeline
  already reads logs on it). unit-tests: bun's `(fail) <suite > test> [ms]` lines under each `== bun test --isolate <file>` header
  that `scripts/verify/test-files.ts` replays, names `<file> :: <suite > test>`; a file that failed with no such line (load error,
  time limit, signal) is named by its file. integration: the same lines inside each `[integration] ── target N/M (...): <target>`
  block; a target killed by its watchdog, or left unfinished when the shard was cut short, is named by itself. Check-run
  annotations would be smaller, but need Checks: read, which the App is not documented to hold, and unit-tests has none that names a test.
- **Cost.** One call per failed job, kept in `--cache` (names, not logs; a finished job never changes), plus one jobs listing per
  failed superci run in `events` mode to find a record's REST job (a record has only a GUID), and one per failed Blacksmith main
  run not already in the baseline sample. A job whose steps show the tests' step succeeded (`Run tests`, `Run integration tests`)
  names no test and costs no call. `--max-test-reads` (150) bounds the logs one check reads, superci's first. A canary's second and later
  checks cost only the jobs that failed since: Blacksmith's baseline ends at `SINCE`, so its reads happen once.
- **Rule.** Blacksmith's side is every failed job of this name (any matrix leg) in every main run of the baseline window that
  ended red, read from the run listing the gate already holds, not only the sample the rates use. A superci failure counts when at
  least one of its failing tests is in no failed Blacksmith main job of that window (a test never seen failing there passed on every
  main run; the failing-test source cannot say where a test ran, so absence is a pass). A failure whose tests are all red there is
  excluded and printed under the failures row: `excluded (red on Blacksmith main too): <tests> in <superci run ids>`, then the
  Blacksmith runs. Counted failures are printed too (`counted, green on Blacksmith main`). Fail closed: a failure whose log cannot be read,
  names no test, or whose job record matches no REST job counts and says why; a failure in a step other than the tests' counts and says
  which. A Blacksmith main failure that cannot be read draws no exclusion and is reported.
  An exclusion that rests on one Blacksmith main failure of that test in the window is tagged `(1 Blacksmith failure: flaky?)`.
  **A failure the named tests do not explain counts.** A failure is excluded only when its test-level names explain it. It counts, and says
  why, when its job timed out; when a file was `stopped at` its limit or `terminated by` a signal, a file or target exited with a code other than
  bun's 1, a shard was cut short (`did not finish before SIGTERM`, a cancelled step, GitHub's own cut-off line), a target hung past its
  watchdog or the runner crashed; when bun's summary (` N fail`, ` N error`) counts more than the log names, or an error came between tests;
  when a file or target has failures, exited non-zero and printed no `Ran N tests across` line (the run died after its first failures, e.g. `process.exit(1)`);
  when a step's process ended with an exit code other than 1 (`Process completed with exit code 137`, the OOM killer), the runner lost communication with the server, or a step
  of a finished job has no conclusion; when a step other than the tests' failed; and when it is named only by a file or target (no `(fail)` line), since a coarse name
  compares nothing. A log that is not valid UTF-8 is unreadable and counts.
  **Which Blacksmith runs make main red.** Ordinary main runs only: a `workflow_dispatch` run (a census, a hoist record) and an integration
  census leg (`integration (N, v3)`) are left out, as are failed jobs naming no test-level name.
  **The allowance leaves out only the double-counted cluster.** A failed Blacksmith main job leaves the rate's numerator only when every test
  it failed is among the tests that excused a superci failure in this window; the denominator stays every Blacksmith run (runs, not legs: a Blacksmith run is failed when one of its counted legs is). Scattered
  Blacksmith failures on other tests, one on another branch, one not read, one naming no test and one with a hard reason still count. Both rates are
  printed under the failures row, before and after; the allowance uses the rate after.

### The failures row counts runs against a binomial bound

Nov 28 07:36Z: unit-tests rolled back on 22 failed legs of 491 against `B rate 1.9% x 491 + 1 = 10.2`; 15 of the legs were one PR's single run
and none was superci's. Legs of one run fail together, so the row counts runs: the value reads `<failed runs> of <runs> (<failed legs> legs)`
and the threshold `<= k at a 1% false-rollback bound (Blacksmith <failed>/<runs> runs = <rate>%)`, where k is the smallest value with
P(X > k) <= `FALSE_ROLLBACK_RATE` (1%) for X ~ Binomial(superci runs, Blacksmith's per-run rate), summed exactly (`run_failure_allowance`).
1% is the chance that a healthy superci rolls back on this row alone. The row fails when the failed runs exceed k, and a canary's `ROLL BACK`
line carries the value and the threshold, so the bound is in the log. The floor is 1: against a Blacksmith rate of 0 the exact bound is 0,
which would roll back on one red run, and a clean finite baseline is not proof that a run cannot fail by chance. The distinct runs are those
with at least one non-cancelled job; the per-leg exclusions (same commit red on Blacksmith, red on Blacksmith main) are unchanged.

**Window floors (`WINDOW_FLOORS`).** A job's parity window may not include runs from before its floor: `--since` and Blacksmith's
baseline start are clamped to it (the output says so), and a Blacksmith window that ends before it is refused. `integration` starts at
megpt-mono `4e75d94` (#36944, 2026-11-27T20:33:52Z, its committer time on main), which ended 5-25 minute resets between targets.
`unit-tests` starts at megpt-mono `bdfa0fe` (#36789, 2026-11-28T05:52:41Z), the last of ci-speed's Nov 28 flake fixes (with #36883
and #36887). The floor is by run creation time: a run on a branch cut before that commit and created after it is not caught.
A split's one window is clamped on `--since` alone, and a split canary has no baseline to float over a floor. A plain canary's Blacksmith window ends at its T0 (`canary.sh` passes `--baseline-until` = `--since`), so a canary of a floored job whose
T0 is at or before the floor gets no check on any tick (the gate's `refusing:` line, exit 1, logged as `NO CHECK`) and is never rolled
back by it: no Blacksmith run in its window is a parity sample. Route the job
again after its floor, so T0 is past it, with Blacksmith runs between the floor and T0 for the baseline.

## A split key: `gate job <job> --split N`

megpt-mono's `SUPERCI_JOBS` takes split keys, `"<job>@10"`, `"<job>@50"`, `"<job>@90"` (and `"@50"` for every job). A run goes to superci by the
last digit of `github.run_id` (`@50`: 0-4 to superci, 5-9 to Blacksmith; `@10`: 0; `@90`: 0-8), and every leg and every rerun of a run stays on
one side. So both halves run in the same hours on the same PRs, and `--split N` compares them **in that window** instead of superci-now against
Blacksmith's earlier hours, which had another mix of PRs (that is how the Nov 28 07:36Z rollback of `unit-tests` counted 6 broken PRs against superci).

- **One window.** `[--since, now]` (clamped to the job's [window floor](#failing-tests-and-window-floors); the output reports `--since` alone).
  Every run of the job's workflow created in it on `--branch-glob` (`*` for a canary) is read whole, so `--max-runs` is refused, as is
  `--baseline-until`: there is no earlier baseline, no baseline end to fix, and `--baseline-hours` is unused. A split canary's Blacksmith side is
  every Blacksmith run from T0 on, so a T0 before the floor no longer gets `NO CHECK`.
- **The halves are by run, from the fleet each leg ran on** (`fleet_or_label`, as everywhere in the gate). All legs of a run (and every attempt)
  share a fleet: a run on superci is in S, one on Blacksmith in B. With the listener's records (`events`) S is still read from them, but the jobs of every
  run of the window are read from GitHub too, to see where each run's other legs ran: a split's reads grow with all its runs, not a sample.
- **`routing faults (runs on both fleets)`.** A run with legs on both fleets (a leg on each, or a rerun on the other fleet) is in neither S nor B and
  is listed. **Report-only.** It means megpt-mono's routing (the run-id digit, or a per-leg rule) is broken, not that superci harmed a job, and a rollback of the
  job would not fix a routing bug; the row is FAIL and the canary line carries it: `canary unit-tests@50: KEEP   [split, no rollback: routing faults ...: 1 of 13
  runs (run 700)]`, which `canary.sh` logs each tick. It is not in `ROLLBACK_ROWS`.
- **`split ratio (superci share of runs)`.** Superci's runs over S and B runs against N%, an exact two-sided binomial test at `FALSE_ROLLBACK_RATE`
  (1%): FAIL when the split is further from N% than chance gives. **Report-only**, for the same reason; the failures row cannot see a skewed split.
  With no run in either half it is NOISE.
- **`wall (per shard)` replaces `wall p50` and `wall p90`.** Pooled percentiles of two halves move with each half's mix of legs: a 4cpu `unit-tests` leg takes
  about 100 s and an 8cpu leg about 55 s, and two halves finish different shares of each, so equal fleets read as unequal (Nov 28 09:24Z: `unit-tests@50`
  rolled back on a pooled p50 0.4 s over, while per shard superci was at 1.000x on 4cpu and 0.905x on 8cpu). So the wall is compared shard by shard. A shard is a
  leg's name with its fleet label left out (`unit-tests (11, superci-8cpu, 8)` and `unit-tests (11, blacksmith-8vcpu-ubuntu-2404, 8)` are `unit-tests (11, 8)`);
  for each shard with legs on both halves the ratio is superci's median wall over Blacksmith's (wall as `wall p50` counts it: successes, timed-out legs at the
  ceiling, cancelled-after-start lower bounds). The row prints the geometric mean of the ratios and FAILs only when a **one-sided run-permutation test**
  (`permutation_p`) says the mean of their logs is above `log(1 + WALL_MARGIN)` (`WALL_MARGIN` = 0.05: a slowdown under 5% is not worth a rollback) at
  `FALSE_ROLLBACK_RATE` (1%) divided by the sizes judged. The test permutes whole runs, not shards, because the legs of a run share the run's weight (a big PR is slow in
  every shard) and the shard ratios therefore move together: a signed-rank test over shards read them as independent samples, and on equal fleets with 10% of runs 1.3x
  slower it rolled back 2.7% (tail row) to 4.0% (both rows) of windows at 10 runs a half. Per size the window's runs of both halves are pooled (a run is all its legs of
  that size); superci's walls are divided by `1 + WALL_MARGIN`, so the null is "superci is exactly at the margin"; the statistic is the mean over the paired shards of the
  log of superci's median over Blacksmith's. `PERMUTATIONS` (2000) random reassignments of whole runs to the halves, each half keeping its run count, give the null
  distribution, and p = (1 + the assignments at least as slow as the observed) / (1 + draws). The draws are seeded from the window's run ids (and the row and size), so a
  window read again has the same p. Once enough assignments are as slow for p > alpha to be certain the draws stop (the verdict is the one all 2000 give, and the p printed
  is the estimate from the draws made, which the row's value prints as `p > alpha, stopped at N of 2000 draws`, never as a p); a FAIL always has every draw made and prints its p, and the two rows of 52 shards at 24 runs a half take under a second. It is NOISE (`not enough shards paired yet`)
  until `MIN_PAIRED_SHARDS` (8) shards are paired, so that no verdict hangs on one or two shards. It is also NOISE until each half has `MIN_SHARD_RUNS` (10) runs, and the run floor is per size: a size is judged (gets a p and can FAIL) only when each half has `MIN_SHARD_RUNS`
  distinct non-cancelled runs with at least one leg of that size among its paired shards, so a size short of runs reads `4cpu 0.979x over 24 (4 and 4 runs with 4cpu legs, under 10 a half: not judged)`.
  megpt-mono gives 4cpu legs to merge-queue copies and non-PR runs alone (a PR run has 28 legs, all 8cpu): a window of 12 runs a half with 4 queue copies a half has 8 runs of 4cpu, C(8, 4) = 70
  assignments and a least p of 0.014, over the 0.005 the size fails at, so 4cpu could only PASS. The judged sizes share the alpha (the Bonferroni divisor counts them alone); the row is NOISE while none is judged.
  The detail lines under the row give the shards paired and the worst 10 ratios. The row's rollback is `canary_verdict`'s from `MIN_SHARD_RUNS` runs. **`wall p90 (ceiling)`** stays as `wall p90` had it: superci's own p90 within the job's ceiling in `ci-limits.tsv` (no row without a ceiling),
  counted from 20 runs.
- **`wall tail (per shard)`** (a split only) is the tail check the pooled `wall p90` gave: a per-shard median does not see 15% of superci's legs on cold machines at 1.5x.
  For each shard with at least `MIN_TAIL_LEGS` (10) legs on both halves (a p90 of fewer is its one or two largest legs) the ratio is superci's p90 wall over Blacksmith's;
  per size, exactly as the median row (the same run-permutation test, on each shard's p90), the row FAILs when the mean of the logs of those ratios is above
  `log(1 + TAIL_MARGIN)` at `FALSE_ROLLBACK_RATE` divided by the sizes judged. `TAIL_MARGIN` is 0.10, the `P90_FACTOR` of the pooled rows: a shard's p90 of 10 to 20 legs is noisier than its median, and at 0.05 equal halves rolled
  back 1.7% of 300 seeds at 10 runs a half (0.10: none of 1800). It is NOISE until a size has `MIN_PAIRED_SHARDS` shards with enough legs and each half has `MIN_SHARD_RUNS`
  runs with legs of the size (the same per-size floor as the median row's), and counts toward a rollback from `MIN_SHARD_RUNS` runs. A cold tail of 15% at 1.5x on every shard rolls back in 93% of simulated windows at 24 runs a half, 98% at 30 and 40% at 12 (300 seeds each; the signed-rank rows: 84%, 92%, 40%),
  with the median row passing throughout. The shards of a window of 10 to 15 runs have few legs on one half, so the row is often NOISE early in a canary.
- **`ramp readiness`** (a split only, **report-only**, in no `ROLLBACK_ROWS`) says whether the job may go from its share to the whole: every machine size with a superci leg
  in the window is *ready* when `wall (per shard)` judged it (`MIN_PAIRED_SHARDS` shards, `MIN_SHARD_RUNS` runs a half that carry legs of that size) and did not FAIL it, and `wall tail (per shard)` did the same.
  PASS when every size is ready; else NOISE, and the note names each size and what it lacks (`not ready: 4cpu tail not judged (3 of 8 shards with 10 legs a half); 8cpu ready`, or `not ready: 4cpu median not judged (4 superci and 4 Blacksmith runs with 4cpu legs, under 10 a half), tail not judged (...); 8cpu ready`;
  a size a row FAILs is named too, though the rollback acts on that row). The value is each size's state (`4cpu: median PASS, tail not judged; 8cpu: median PASS, tail PASS`). The
  canary line ends with `[ramp: ready]` or `[ramp: not ready: ...]`. It reads what the two rows concluded for each size (`Row.sizes`) and tests nothing again.
- **Superci's wall is the listener's `wall_ms`**, never GitHub's started to completed: GitHub marks a dispatched job started when its identity accepts it, so that
  interval holds the wait for a machine (up to 24 s on a size short of identities). With `--superci-source events` (the canary's) the superci jobs are the records,
  whose start and finish are `wall_ms`; with `github` the window's records are read as well and matched to the GitHub legs (run, name, runner, finish within 2 min), and the output
  says when none was found and GitHub's wall stays.
- **Unchanged.** Queue wait stays pooled (it does not depend on the shard), with its p50/p90 rows and the Blacksmith half's size gating; so do the failures row with its per-run
  Fisher bound, `MIN_RUNS`, the same-commit excuse and `canary_verdict`. The bound's Blacksmith rate is now the B half's rate in the same window; early ticks have few B runs,
  so the floor of one failed run and `MIN_RUNS` carry more of the weight. Without `--split` nothing changes.
- **Red on Blacksmith main** (`gather_tests`) is read from the same window's B half: the red main runs are the ordinary main runs of `[--since, now]` that ran on
  Blacksmith (`only_runs`), so a routing fault's Blacksmith leg and a main run that ran on superci are not red-on-Blacksmith-main, and a main run from before
  `--since` is not read. In non-split mode `(baseline start, baseline end)` still bounds it, as before.

## A live canary: `gate job --canary` and `canary.sh`

`gate job <job> --canary` prints the rows, then `canary <job>: KEEP` (exit 0) or `ROLL BACK (reasons)` (exit 3) (`canary <job>@N` with `--split`). A canary rolls
back on harm to superci's own jobs, judged against Blacksmith over the same window, and on nothing else:

- a FAIL of wall p50, wall p90 (a split: wall (per shard), wall tail (per shard) and wall p90 (ceiling)), queue wait p50, queue wait p90 or failures. A p50 row counts once n passes (5 runs; a split's two per-shard rows at 10) and a
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

`canary.sh SINCE UNTIL JOB...` (a JOB is a plain job, or a split, `unit-tests@50`: see [a split key](#a-split-key-gate-job-job---split-n)) runs the check for each job SUPERCI_JOBS still lists, the first at once (right after the start probe, so a harm in the first minutes is seen then, not after a first EVERY sleep), then every 15 minutes, until UNTIL (it never sleeps past UNTIL: when the next tick would start at or after it, the watch ends), with
Blacksmith's side the 24 h before SINCE (`--baseline-until`), so a job that left Blacksmith still has a baseline. A split JOB
(`unit-tests@50`) calls the gate as `job unit-tests --split 50 --since SINCE` with **no `--baseline-until`**: the Blacksmith half is the same window's.
SUPERCI_JOBS is matched by **key**, the part of an entry before the `@`: a job is still listed while the variable holds its key at any percent, and
a rollback removes every entry of that key (the plain job and each percent) and no other key's: `["test-go","unit-tests@50"]` becomes `["test-go"]`. A job with an `@`
that is not `@10`, `@50` or `@90` is refused at the start (exit 2). It
removes a job that gets ROLL BACK from SUPERCI_JOBS, and leaves the other jobs listed. SUPERCI_JOBS is read and written on the App's
`variables` token, and a rollback that cannot get it prints `ROLLBACK FAILED` (or, with `SUPERCI_PERSONAL_TOKEN=1`, writes on the personal
token and says so). `canary.sh set '["test-go","unit-tests@50"]'` and `canary.sh unset` re-route by hand the same way, with no watch; `set` takes a job, or a job then
`@10`, `@50` or `@90`, and refuses any other entry (`unit-tests@33`, `unit-tests@`) with a line naming it, before any call. `WORKFLOW` names the jobs' workflow
file (lint-test-backend.yml by default). A check that crashes exits 1 and rolls nothing back: read its output file.

**Telling the operator.** A rollback is a line in a log unless something reads it (Nov 28 00:57:56Z: test-go was rolled back on its queue
wait p90 and nobody noticed for 59 minutes). Two switches make it reach a person:

- `ON_ROLLBACK=<command>` runs once after every rollback the watch makes, with one more argument, a one-line summary: the UTC time, the
  job, the reason line the gate printed, and SUPERCI_JOBS before and after. It also runs when the rollback's write fails (the job is
  still on superci, the line says so and names the cause), on each failed tick, and once when a job has had no check for 3 ticks running
  (no App token, SUPERCI_JOBS unreadable, under the 1,500-read floor, a gate that refused or crashed), with why. The command line runs
  under `bash -c` with the summary appended as its last argument, under `env -i` with only `PATH`, `HOME`, `LANG` and the `NOTIFY_*`
  variables (the watch's `GH_TOKEN`, `GITHUB_TOKEN` and AWS credentials never reach it). `NOTIFY_TIMEOUT` seconds (60) is a hard bound:
  TERM, then 5 s later KILL, to the notifier's whole process group, so a notifier that traps TERM or leaves a background child behind ends
  within `NOTIFY_TIMEOUT` + 6 s, and what it leaves running is killed. Its output goes to a file capped at 256 KiB and only the first 250
  bytes are read, scrubbed of GitHub tokens (`gh*_`, `github_pat_`), JWTs and AWS access key ids. Its failure is logged
  (`ON_ROLLBACK failed (exit N)`) and never stops the watch or undoes the rollback.
- `STOP_ON_ROLLBACK=1` exits 3 at the end of the tick that made a rollback, once ON_ROLLBACK has run, so a wrapper that runs the watch as a
  background task is woken. The tick's remaining jobs are checked first (and rolled back if the gate says so), so none goes unchecked in
  that tick; but the watch is then over, and **a wrapper must restart it** (with the same SINCE and UNTIL) for the jobs SUPERCI_JOBS still
  lists. A rollback that could not be written does not stop the watch (the next tick tries again). An unset SUPERCI_JOBS (someone else sent
  every job back), and a job another watch or a `set` had already taken out of the value this watch re-read, are not rollbacks of this
  watch: no notice, no exit 3. A watch that reaches UNTIL exits 0.
- An unset SUPERCI_JOBS (after a rollback, or `canary.sh unset`) is not a failure to check: each tick logs `SUPERCI_JOBS unset, nothing to
  check` and the no-check streak starts over, as it does for a job that is off the list. Only an unreadable variable (GitHub did not answer,
  no token), a gate that refused or crashed, a missing App token or the read floor count toward the 3 ticks.

## gate 2: `gate scale`

Reads the replay's runs (every run in the window whose workflow file is `--workflow` and whose branch matches `--branch-glob`; a load workflow that exists only on its branch has no name GitHub resolves), the listener's timeline events and CloudWatch, and lists the claims
bucket read-only.

| Row | Source | Passes when |
| --- | --- | --- |
| completion | the trace's job count (`--trace` is required, so a push that never created a run is a missing job), the jobs GitHub ran and `job lost with its machine` lines | completed equals the trace's count, none stranded, none lost |
| queue wait p50, p90 | replay jobs on `superci-pull` | at most 6 s and 10 s |
| org registrations per 5 min | `RegistrationWindowUse` (ours and others) and `OrgRateKnown` | peak at most 1,200 (80% of 1,500), with the rate known throughout and a datapoint for every minute of the replay window (a gap fails the row and names its first minute) |
| one runner per machine | `issued/` objects, `jit.issued` and `jit.claim` events, dispatched `job.record`s | claimed set equals the registered set and the dispatched machines (a dispatch size registers no runner: its machine is claimed when it is handed its one job), no machine registered, claimed or handed a job twice. A refused duplicate grant is the fence working and is reported |
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

Before its first push a trace replay checks the identities labelled per size: `identity.sh labels` (the online identities
GitHub lists under each label) against `workflow.py --need` (the most of a label's legs at once, each held for its sleep
and the 10 s of gate 2's queue-wait p90). A short label refuses the replay, which would otherwise measure the wait for an
identity, not the plane. Every push's tree is origin/main's at the start, so a fetch by another session during the replay
adds no file of main's to a push (megpt's pre-push hook checks what a push changes). A refused push stops the replay.

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
Blacksmith shares (and not one superci or main-on-superci shares), reruns, timeouts and cancellations, the required flags, minute coverage of the registration datapoints, the alarm history, the trace extraction and the bench's per-bucket workflow, the recorded
canary fixtures in `fixtures/`, and a split key's one window (halves by run and fleet, a routing-fault row, the ratio's exact test, red on Blacksmith main from the B half,
and `canary.sh`'s key matching and `set` validation). `python3 -I test_spawn.py`: a warmed fleet passes, one cold machine in five fails, the bound's edge,
failed commands, the 5-sample minimum per measure and fleet, shown rows, the echoed script with ANSI and CRLF, a retried
attempt, and `main`'s exit codes.
