#!/usr/bin/env python3
"""gate: decides whether a CI job may move from Blacksmith to superci, and whether the plane can carry the load.

  gate job verify-tools --since 2026-11-25T17:03:43Z --branch-glob 'ci/superci-load-pull-*' [--workflow lint-test-backend.yml]
  gate scale --from 2026-11-25T18:00:00Z --to 2026-11-25T19:00:00Z --trace trace.json
  gate replay-trace --hour 2026-11-25T14:00:00Z [--out trace.json]

`gate job` is gate 1 (speed parity, one job), `gate scale` is gate 2 (the whole plane over a replay window) and
`replay-trace` writes a real busy hour's arrivals for pull/bench/replay.sh to drive. The criteria and their thresholds are
the table in move-gate.md; each is a Row, and the tool prints one row per criterion and exits 0 only when every row passes.

Inputs are the GitHub REST API (cached), pull/tools/timeline for the listener's events and the unexplained time, and, read
only, CloudWatch, EC2-adjacent S3 listings and alarms of the ci-runners account (always `--profile`, never the environment).
"""
import argparse
import collections
import concurrent.futures
import dataclasses
import fnmatch
import hashlib
import json
import math
import os
import re
import sys
import time

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "timeline"))
import timeline as T  # noqa: E402

REPO = "Authentic-Intelligence/megpt-mono"
LIMITS_PATH = "scripts/verify/ci-limits.tsv"
CLAIMS_BUCKET = "superci-pull-claims-243891825325"
CW_REGION = "us-east-1"
NAMESPACE = "superci-pull"
PASS, FAIL, NOISE = "PASS", "FAIL", "NOISE"

# Gate 1 (move-gate.md).
MIN_RUNS = 5
P90_FACTOR = 1.1
QUEUE_P90_SLACK_S = 3.0
# The failures row counts runs, not legs (one broken PR fails many legs of one run together), and rolls back when the failed runs exceed what
# Blacksmith's own per-run failure rate would produce by chance in n runs, with FALSE_ROLLBACK_RATE the chance that a healthy superci rolls
# back anyway: the smallest k with P(X > k) <= FALSE_ROLLBACK_RATE for X ~ Binomial(superci runs, Blacksmith's run failure rate).
FALSE_ROLLBACK_RATE = 0.01
# One failed run never rolls a canary back alone, whatever the bound: Blacksmith's rate is an estimate from a finite window, and a rate of 0.0%
# from 40 clean runs is not a proof that a run cannot fail by chance (a rate of 0 with an exact bound of 0 would roll back on the first red).
MIN_ALLOWED_FAILED_RUNS = 1
MAX_UNEXPLAINED_S = 2.0
BASELINE_HOURS = 24
# Gate 2.
ORG_CAP = 1500
ORG_WINDOW_S = 300
REGISTRATION_SHARE = 0.8
QUEUE_P50_S = 6.0
QUEUE_P90_S = 10.0
ON_DEMAND_SHARE = 0.10
ORPHAN_WAIT_S = 600
SIZES = (2, 4, 8, 16, 32)
# A canary rolls back on harm to superci's own jobs, judged against Blacksmith over the same window, never on a cause every
# runner of the organization shares. GitHub's registration cap is the organization's: a job moved from Blacksmith takes one
# registration on either fleet, so a refusal (superci-pull-registration-refusals) pages and is counted, and moves nothing.
ROLLBACK_ROWS = ("wall p50", "wall p90", "queue wait p50", "queue wait p90", "failures (superci)")
# listener-down and spend-capped stop every set, so they send back every job listed. A wait or a stall is one set's: a canary
# reads the alarms of the sets its job ran on (SET_ROLLBACK_ALARMS, one `superci-pull-<alarm>-<size>` each) and no other set's, so a
# size with a cold machine, or one that is new, cannot send back a job that ran elsewhere. The listener-wide oldest-job-wait and
# jobs-stalled still page and roll nothing back.
ROLLBACK_ALARMS = ("superci-pull-listener-down", "superci-pull-spend-capped")
SET_ROLLBACK_ALARMS = ("superci-pull-oldest-job-wait", "superci-pull-jobs-stalled")
CANARY_P90_RUNS = 20  # a p90 of fewer runs is one job: one outlier must not send a job back
ROLLBACK, KEEP = 3, 0


# ---------------------------------------------------------------------------------------------------------------------
# The domain: an observation of a job, a criterion's row, and the spread that is the noise floor.
# ---------------------------------------------------------------------------------------------------------------------

@dataclasses.dataclass(frozen=True)
class Obs:
    """One finished run of a job: how long it waited and ran, on which commit, branch and fleet."""
    job_id: int
    run_id: int
    sha: str
    branch: str
    fleet: str
    ok: bool
    queue: "float | None"  # created to started; None for a rerun attempt, whose created_at is its first attempt's. A job cancelled
    #                        before it started has created to completed: at least its queue wait, a lower bound
    wall: "float | None"   # started to completed; None for a job cancelled before it started. For a cancelled job, a lower bound
    timed_out: bool = False
    attempt: int = 1
    cancelled: bool = False


@dataclasses.dataclass(frozen=True)
class Row:
    name: str
    value: str
    threshold: str
    status: str
    note: str = ""
    detail: tuple = ()  # lines printed under the row, for what a one-line note cannot hold (excluded test names)


def run_spread(obs, get):
    """The run-to-run noise of a metric: the IQR, across runs, of each run's median over its legs. A matrix job's legs differ
    systematically (leg 1 is not leg 3), and that is not noise; a one-leg job's run is its own median."""
    by = collections.defaultdict(list)
    for o in obs:
        by[o.run_id].append(get(o))
    return iqr([T.q(v, 50) for v in by.values()])


def iqr(values):
    return (T.q(values, 75) - T.q(values, 25)) if values else 0.0


def spread(values):
    """The sample's quantiles, interpolated, so the run-to-run noise is on the page next to every verdict."""
    names = (("min", 0), ("p25", 25), ("p50", 50), ("p75", 75), ("p90", 90), ("max", 100))
    return {"n": len(values), **{k: T.q(values, p) for k, p in names}}


def fmt_s(x):
    return "-" if x is None else f"{x:.1f} s"


def at_most(name, value, bound, threshold, spread_s=0.0, unit=fmt_s):
    """value <= bound passes; a pass whose margin is narrower than the sample's spread is NOISE: add runs, don't round."""
    if value is None or bound is None:
        return Row(name, unit(value), threshold, FAIL, "no data")
    if value > bound:
        return Row(name, unit(value), threshold, FAIL, f"over by {unit(value - bound)}")
    margin = bound - value
    if margin < spread_s:
        return Row(name, unit(value), threshold, NOISE, f"margin {unit(margin)} is inside the superci run-to-run spread (IQR of run medians {unit(spread_s)}): add runs")
    return Row(name, unit(value), threshold, PASS, f"margin {unit(margin)}")


OUTCOMES = ("success", "failure", "timed_out")


def is_obs(job, run):
    """An Obs from a REST job and its run, or None when the job has nothing to count: it never finished, or GitHub skipped it.
    Every attempt of a run is an observation, so a failure a rerun then passed still counts. A rerun attempt keeps its first
    attempt's created_at, so its queue wait is not real and is None; its failure and its wall time (started_at to completed_at)
    are real. A timed_out job is a failure.

    A cancelled job has no outcome, but it did spend time, and a plane that is slow and gets cancelled must not look fast. It is
    an observation of at least what it spent: a job that started contributes wall = started to completed (its queue wait is real);
    one cancelled in the queue contributes queue = created to completed and no wall. A job was cancelled in the queue when no
    runner took it (no runner_name): GitHub sets its started_at to its created_at, so started_at cannot tell. Both fleets get this rule, whatever
    cancelled the job (a newer push superseding the run is not distinguishable from any other cancel in the jobs listing, and
    the same rule on both fleets keeps the p50 and p90 comparison fair). A lower bound can only under-report, so a row that fails
    on lower bounds fails on the real times too; a cancelled job is never a failure and never counts as a run."""
    attempt = job.get("run_attempt") or 1
    cancelled = job.get("conclusion") == "cancelled"
    if not job.get("completed_at") or (job.get("conclusion") not in OUTCOMES and not cancelled) or (not job.get("started_at") and not cancelled):
        return None
    created, completed = T.ts(job["created_at"]), T.ts(job["completed_at"])
    queued_cancel = cancelled and not job.get("runner_name")
    started = T.ts(job["started_at"]) if job.get("started_at") and not queued_cancel else None
    if started is None:
        queue, wall = (max(0.0, completed - created) if attempt == 1 else None), None  # GitHub's second-granular stamps can put completed_at a second early
    else:
        queue, wall = (started - created if attempt == 1 else None), completed - started
    return Obs(job["id"], job["run_id"], job.get("head_sha") or run.get("head_sha") or "", job.get("head_branch") or run.get("head_branch") or "",
               fleet_or_label(job), job["conclusion"] == "success", queue, wall, job["conclusion"] == "timed_out", attempt, cancelled)


def fleet_or_label(job):
    """T.fleet_of, which reads a started job's runner; a job cancelled while queued has no runner yet, so its superci-<n>cpu label counts."""
    f = T.fleet_of(job)
    if f == "other" and any(re.fullmatch(r"superci-\d+cpu", l) for l in job.get("labels") or []):
        return "superci"
    return f


def cancelled_by_fleet(jobs):
    """{fleet: (runs, jobs)} of the jobs GitHub ended as cancelled. They are not observations (no outcome), so they are reported, not measured."""
    runs, n = collections.defaultdict(set), collections.Counter()
    for j in jobs:
        if j.get("conclusion") == "cancelled":
            f = fleet_or_label(j)
            runs[f].add(j["run_id"])
            n[f] += 1
    return {f: (len(runs[f]), n[f]) for f in n}


def is_superci(job, group):
    """A job on superci's runner group; one cancelled in the queue has no runner yet, so its superci-<n>cpu label places it."""
    if job.get("runner_name") or job.get("runner_group_name"):
        return T.fleet_of(job) == "superci" and job.get("runner_group_name") == group
    return fleet_or_label(job) == "superci"


def wall_sample(obs, ceiling):
    """The wall times a fleet's wall quantiles are drawn from: successful runs, each timed_out run raised to the job's
    ceiling (a job GitHub ended at its timeout ran at least that long, whatever the clock says), so p90 sees it, and each job
    cancelled after it started at the time it ran, a lower bound."""
    return ([o.wall for o in obs if o.ok] + [o.wall if ceiling is None else max(o.wall, ceiling) for o in obs if o.timed_out]
            + [o.wall for o in obs if o.cancelled and o.wall is not None])


def queue_sample(obs):
    return [o.queue for o in obs if o.queue is not None]


# ---------------------------------------------------------------------------------------------------------------------
# Gate 1: speed parity of one job.
# ---------------------------------------------------------------------------------------------------------------------

def excused(f, everything):
    """A failure that Blacksmith has on the same commit does not count against superci. Main's run of the commit is Blacksmith's
    observation (main runs on Blacksmith). A failure on superci never excuses another, main's included: that is the plane's own."""
    return any(o.job_id != f.job_id and not o.ok and not o.cancelled and o.sha == f.sha and o.fleet == "blacksmith" for o in everything)


def run_failure_allowance(n, p, alpha=FALSE_ROLLBACK_RATE):
    """The most failed runs out of n a healthy superci shows without rolling back: the smallest k with P(X > k) <= alpha for
    X ~ Binomial(n, p), p Blacksmith's per-run failure rate, summed exactly with math.comb, never below MIN_ALLOWED_FAILED_RUNS (nor above
    n). p = 0 gives the floor (the exact bound, 0, would send a job back on one red run against a clean baseline); p = 1 gives n (every
    run is expected to fail, nothing can exceed it)."""
    if n <= 0:
        return 0
    p = min(max(p, 0.0), 1.0)
    pmf = [math.comb(n, i) * p ** i * (1.0 - p) ** (n - i) for i in range(n + 1)]
    tail = 0.0  # P(X > k), summed from the top so the small terms are not lost
    for k in range(n, -1, -1):
        if tail + pmf[k] > alpha:
            return min(max(k, MIN_ALLOWED_FAILED_RUNS), n)
        tail += pmf[k]
    return min(MIN_ALLOWED_FAILED_RUNS, n)


def evaluate_job(S, B, ceiling, unexplained, min_runs=MIN_RUNS, tests=None):
    """Gate 1's rows from the superci runs S and Blacksmith's B of one job, the job's ceiling in seconds (None when it has no
    row in ci-limits.tsv), the unexplained seconds of each superci job and `tests` (Tests, or None for a job with no test-name
    source: the failures row then counts a failed job as one failure). Pure."""
    rows = []
    runs = len({o.run_id for o in S if not o.cancelled})
    rows.append(Row("n (superci runs)", str(runs), f">= {min_runs}", PASS if runs >= min_runs else FAIL, "counts runs, not shards"))
    sw, bw = wall_sample(S, ceiling), wall_sample(B, ceiling)
    sq, bq = queue_sample(S), queue_sample(B)
    if not sw or not bw:
        why = "no successful, timed-out or cancelled-after-start superci jobs" if not sw else "no Blacksmith runs in the baseline window"
        for name, th in (("wall p50", "<= Blacksmith p50"), ("wall p90", f"<= min(Blacksmith p90 x {P90_FACTOR}, ceiling)"),
                         ("queue wait p50", "<= Blacksmith p50"), ("queue wait p90", f"<= Blacksmith p90 + {QUEUE_P90_SLACK_S:g} s")):
            rows.append(Row(name, "-", th, FAIL, why))
    else:
        p90_cap = P90_FACTOR * T.q(bw, 90)
        th90 = f"min(B p90 {T.q(bw, 90):.1f} x {P90_FACTOR} = {p90_cap:.1f}, ceiling {'none' if ceiling is None else f'{ceiling:g}'})"
        bound90 = p90_cap if ceiling is None else min(p90_cap, ceiling)
        wall_noise = run_spread([o for o in S if o.ok], lambda o: o.wall)
        queue_noise = run_spread([o for o in S if o.queue is not None and not o.cancelled], lambda o: o.queue)
        rows.append(at_most("wall p50", T.q(sw, 50), T.q(bw, 50), f"<= B p50 {T.q(bw, 50):.1f} s", wall_noise))
        rows.append(at_most("wall p90", T.q(sw, 90), bound90, f"<= {th90}", wall_noise))
        rows.append(at_most("queue wait p50", T.q(sq, 50), T.q(bq, 50), f"<= B p50 {T.q(bq, 50):.1f} s", queue_noise))
        rows.append(at_most("queue wait p90", T.q(sq, 90), T.q(bq, 90) + QUEUE_P90_SLACK_S, f"<= B p90 {T.q(bq, 90):.1f} + {QUEUE_P90_SLACK_S:g} s", queue_noise))
    everything = list(S) + list(B)
    S_done, B_done = [o for o in S if not o.cancelled], [o for o in B if not o.cancelled]
    counted = [o for o in S_done if not o.ok and not excused(o, everything)]
    skipped = sum(1 for o in S_done if not o.ok) - len(counted)
    notes, detail = ([f"{skipped} more also failed on Blacksmith at the same commit"] if skipped else []), ()
    b_failed = [o for o in B_done if not o.ok]
    b_counted = b_failed
    if tests is not None:
        counted, more, detail = judge_by_tests(counted, tests)
        notes += more
        # Blacksmith's side leaves out only the red cluster superci's failures were excused by: a failed main job whose every failing test is among
        # the tests that excused a superci failure here (so a failure is not counted on both sides). Scattered Blacksmith failures on other tests, one
        # on another branch, one not read, one naming no test and one with a hard reason still count toward the allowance.
        b_counted = [o for o in b_failed if not _cluster_failure(tests.b_reads.get(o.job_id), tests.excused)]
    s_runs, b_runs = {o.run_id for o in S_done}, {o.run_id for o in B_done}
    failed_runs = collections.Counter(o.run_id for o in counted)  # run -> its counted failed legs
    b_failed_runs, b_counted_runs = {o.run_id for o in b_failed}, {o.run_id for o in b_counted}
    b_rate = len(b_counted_runs) / len(b_runs) if b_runs else 0.0
    allowed = run_failure_allowance(len(s_runs), b_rate)
    legs_of = ", ".join(f"{r} ({c})" for r, c in failed_runs.most_common(10)) + (" ..." if len(failed_runs) > 10 else "")
    rows.append(Row("failures (superci)", f"{len(failed_runs)} of {len(s_runs)} ({len(counted)} legs)",
                    f"<= {allowed} at a {100 * FALSE_ROLLBACK_RATE:g}% false-rollback bound (Blacksmith {len(b_counted_runs)}/{len(b_runs)} runs = {100 * b_rate:.1f}%)",
                    PASS if len(failed_runs) <= allowed else FAIL, "; ".join(notes), tuple(detail) + (
                        (f"failed superci runs (counted legs): {legs_of}",) if failed_runs else ()) + (
                        (f"Blacksmith failure rate: {100 * len(b_failed_runs) / len(b_runs) if b_runs else 0.0:.1f}% ({len(b_failed_runs)} of {len(b_runs)} runs) before, {100 * b_rate:.1f}% ({len(b_counted_runs)} of {len(b_runs)} runs) "
                         f"after excluding {len(b_failed) - len(b_counted)} failed main job{'' if len(b_failed) - len(b_counted) == 1 else 's'} whose every failing test excused a superci failure; the bound uses the rate after",) if tests is not None else ())))
    p50u = T.q(unexplained, 50)
    rows.append(Row("unexplained p50 (timeline)", fmt_s(p50u), f"< {MAX_UNEXPLAINED_S:g} s", PASS if p50u is not None and p50u < MAX_UNEXPLAINED_S else FAIL,
                    "" if unexplained else "no superci timelines"))
    return rows


def _cluster_failure(read, excused):
    """Whether a Blacksmith main job's failure is only tests that excused a superci failure: read, test-level names only, no hard reason."""
    return bool(read and read.tests and not read.coarse and not read.hard and read.tests <= excused)


def set_alarm(alarm, superci_set):
    """The per-set alarm of a set: superci-pull-oldest-job-wait for superci-4cpu is superci-pull-oldest-job-wait-4cpu."""
    return f"{alarm}-{superci_set.removeprefix('superci-')}"


def watched(alarms, sets):
    """[(set, alarm, per_set)] the canary watches for `sets`, one entry per set and wait or stall alarm. A set's own alarm is
    watched when `alarms` (alarm_states' answer) has it; one it does not have (alarms.sh not yet run, StalledSets not yet published,
    a split set such as superci-4cpu-2 that alarms.sh makes none for) falls back to the listener-wide alarm, and so does a job with
    no set known (set None): the gate is never blind, only less partitioned where it cannot be."""
    return [(s, own if s and own in alarms else base, bool(s and own in alarms))
            for s in sorted(set(sets)) or [None] for base in SET_ROLLBACK_ALARMS for own in [set_alarm(base, s) if s else base]]


def rollback_alarms(alarms, sets):
    """The alarms a canary that ran on `sets` rolls back on: the global ROLLBACK_ALARMS and each set's watched alarms."""
    return list(dict.fromkeys(list(ROLLBACK_ALARMS) + [a for _, a, _ in watched(alarms, sets)]))


def judged_on(alarms, sets):
    """One line saying which alarm each set is judged on, e.g.
    `superci-4cpu by superci-pull-oldest-job-wait-4cpu, superci-pull-jobs-stalled-4cpu; superci-4cpu-2 by superci-pull-oldest-job-wait (no per-set alarm), ...`."""
    by = {}
    for s, a, own in watched(alarms, sets):
        by.setdefault(s or "no set known", []).append(a if own else f"{a} (no per-set alarm)")
    return "; ".join(f"{s} by {', '.join(a)}" for s, a in by.items())


def canary_verdict(rows, alarms, sets=()):
    """The reasons a live canary must go back to Blacksmith, from gate 1's rows, the alarm states and `sets`, the superci sets the
    job ran on (job_sets); empty keeps it. A p50 row counts once n passes and a p90 row once there are CANARY_P90_RUNS runs, each
    only as FAIL (NOISE asks for runs, not a rollback), and never when Blacksmith had no runs to compare with; the failures row
    counts from the first run. Of the alarms only rollback_alarms(alarms, sets) count: each is a superci job not served, and the
    job's sets alone are the ones that serve it, bar the two that stop every set. A set with no alarm of its own in `alarms`, and
    a job with no set, are judged on the listener-wide wait and stall alarms (watched)."""
    by = {r.name: r for r in rows}
    n = by.get("n (superci runs)")
    runs = int(n.value) if n and n.value.isdigit() else 0
    need = {"wall p50": MIN_RUNS, "queue wait p50": MIN_RUNS, "wall p90": CANARY_P90_RUNS, "queue wait p90": CANARY_P90_RUNS, "failures (superci)": 0}
    harmed = [r for r in (by.get(k) for k in ROLLBACK_ROWS) if r and r.status == FAIL and runs >= need[r.name] and not r.note.startswith("no Blacksmith runs")]
    return ([f"{r.name}: {r.value} vs {r.threshold}" for r in harmed]
            + [f"{a} in ALARM" for a in rollback_alarms(alarms, sets) if alarms.get(a) == "ALARM"])


def job_sets(S_jobs, B_jobs):
    """The superci sets a job runs on, for the canary's alarms: the `set` of each of its superci records or, for a job read from
    GitHub, the superci label of its runner. A job with no superci run yet is judged on the sets of the label it ran under on
    Blacksmith (blacksmith-4vcpu-ubuntu-2404 is superci-4cpu), read with Blacksmith's listing and no further call; one with
    neither has no set, and only the alarms that stop every set can send it back."""
    ran = {j.get("set") or superci_label(j.get("labels")) for j in S_jobs} - {None}
    return sorted(ran or {superci_label(j.get("labels")) for j in B_jobs} - {None})


def parse_limits(text):
    """{(workflow, job): seconds} from ci-limits.tsv."""
    out = {}
    for line in text.splitlines():
        if not line or line.startswith("#"):
            continue
        parts = line.split("\t")
        if len(parts) >= 3:
            try:
                out[(parts[0], parts[1])] = float(parts[2])
            except ValueError:
                pass
    return out


def matches(job_name, wanted):
    """The job itself, or one of its matrix legs (`name (leg)`)."""
    return job_name == wanted or job_name.startswith(wanted + " (")


# ---------------------------------------------------------------------------------------------------------------------
# Failing tests and window floors: a test red on Blacksmith's main too is not superci's regression.
# ---------------------------------------------------------------------------------------------------------------------

@dataclasses.dataclass(frozen=True)
class Floor:
    """The earliest a parity window of a job may start: a megpt-mono commit and its committer time on main."""
    commit: str
    at: str
    why: str


# job name -> Floor. A window that starts before it is clamped to it (both superci's --since and Blacksmith's baseline start), and a
# Blacksmith window that ends before it is refused: runs from before the floor ran under a cost neither runner owns, so they are no parity sample.
WINDOW_FLOORS = {
    "integration": Floor("4e75d940d01c2ec79ef887558b1d050ed2eef4e5", "2026-11-27T20:33:52Z",
                         "megpt-mono #36944 ended the 5-25 minute resets between integration targets"),
    "unit-tests": Floor("bdfa0fed349ceecee9b2cd1522a0cc5f442a9d9f", "2026-11-28T05:52:41Z",
                        "megpt-mono #36789, the last of ci-speed's Nov 28 flake fixes (with #36883 0c49174 and #36887 df260e4)"),
}


def clamp_window(job, since, b_lo, b_hi, floors=None):
    """(since, b_lo, lines): the parity window of `job` held to its floor, and a line for each clamp. A Blacksmith window that ends at
    or before the floor holds nothing of the job after it, so it is refused (SystemExit), never read as empty."""
    f = (WINDOW_FLOORS if floors is None else floors).get(job)
    if f is None:
        return since, b_lo, []
    at, name = T.ts(f.at), f"window floor ({job}): {f.commit[:7]} at {f.at}, {f.why}"
    if b_hi <= at:
        raise SystemExit(f"refusing: Blacksmith's window for {job} ends at {T.iso(b_hi)}, before {name}: no run in it is a parity sample")
    lines = [f"{what} {T.iso(t)} clamped to {f.at}" for what, t in (("--since", since), ("Blacksmith baseline start", b_lo)) if t < at]
    return max(since, at), max(b_lo, at), [f"{name}; " + "; ".join(lines)] if lines else [f"{name}; the window starts after it, nothing clamped"]


PARSER_VERSION = 3  # bump with a parser change or a change to what a read stores: the cache keeps what was read by job and version
MAX_TEST_READS = 150
_STAMP = re.compile(r"^\d{4}-\d\d-\d\dT[\d:.]+Z ")
_ANSI = re.compile(r"\x1b\[[0-9;]*[A-Za-z]")
_FAIL = re.compile(r"^\s*\(fail\) (.*?)(?: \[[\d.]+ ?m?s\])?$")
_SUMMARY = re.compile(r"^\s*(\d+) (?:fail|errors?)$")  # bun's ` 1 fail` and ` 2 errors` lines, one of each at the end of a file's run
_UNHANDLED = re.compile(r"^# Unhandled error between tests")
_CUT = re.compile(r"(The operation was canceled|has exceeded the maximum execution time|received a shutdown signal|lost communication with the server)")
_RAN = re.compile(r"^Ran \d+ tests? across ")  # bun's last line of a run that finished
_PROCESS_EXIT = re.compile(r"##\[error\]Process completed with exit code (\d+)")  # GitHub's own line when a step's command ends non-zero; a test's output that says the same is not it
BUN_FAILED_EXIT = 1  # bun test's exit code when tests failed; any other code of a file or target is a crash, a kill or a harness failure


def log_lines(text):
    """The lines of a job log without GitHub's timestamps and bun's colours."""
    return [_ANSI.sub("", _STAMP.sub("", raw, count=1)).rstrip() for raw in text.splitlines()]


@dataclasses.dataclass(frozen=True)
class Parsed:
    """What a log says failed. tests: test-level names (`<file> :: <suite > test>`). coarse: files or targets that failed with no `(fail)`
    line to name a test. hard: reasons the job failed in a way its named tests do not explain (a file stopped at its time limit or killed,
    a shard cut short, a runner crash, an error between tests, bun counting more failures than the log names): such a job counts."""
    tests: frozenset = frozenset()
    coarse: frozenset = frozenset()
    hard: tuple = ()


class _Run:
    """One file's (or target's) bun run as the log shows it: its `(fail)` names, the failures and errors bun's summary counts."""

    def __init__(self):
        self.fails, self.reported, self.unhandled, self.ran, self.exit = [], 0, False, False, None


class _Runs:
    """The runs of a log in the order it shows them, one per header: a file or target that runs twice has two, each with its own `ran` and
    `exit`. Lines of bun's output belong to the current run; the runner's verdict on a name (`exited N`) belongs to the latest run of it."""

    def __init__(self):
        self.items, self.cur = [], None

    def start(self, name):
        self.cur = _Run()
        self.items.append((name, self.cur))

    def end(self):
        self.cur = None

    def here(self):
        if self.cur is None:
            self.start(None)
        return self.cur

    def latest(self, name):
        for n, r in reversed(self.items):
            if n == name:
                return r
        self.items.append((name, _Run()))
        return self.items[-1][1]


def _runs_parsed(runs, coarse, hard):
    """(tests, coarse, hard) from the runs: each `(fail)` name, a summary that counts more than the log names, an error between tests, a run
    that died after its failures. A job is hard when any one of its runs is."""
    tests = set()
    for name, r in runs.items:
        tests |= {f"{name} :: {f}" if name else f for f in r.fails}
        if r.reported > len(r.fails):
            hard.append(f"{name or 'a test file'}: bun's summary counts {r.reported} failures and errors, the log names {len(r.fails)} tests")
        if r.unhandled:
            hard.append(f"{name or 'a test file'}: an error between tests")
        if r.fails and r.exit not in (None, 0) and not r.ran:
            hard.append(f"{name or 'a test file'} exited {r.exit} with no bun summary: the run died after its failures")
    return frozenset(tests), frozenset(coarse), tuple(dict.fromkeys(hard))


def _exit_of(runs, coarse, hard, name, code):
    """A file or target that exited non-zero: coarse when no test is named for it, hard when the code is not bun's failed-tests code."""
    runs.latest(name).exit = int(code)
    if int(code) != BUN_FAILED_EXIT:
        hard.append(f"{name} exited {code}, not bun's {BUN_FAILED_EXIT} for failed tests")
    if not runs.latest(name).fails:
        coarse.add(name)


def _runner_line(line, hard):
    """GitHub's `Process completed with exit code N` for a step: any N other than bun's 1 is a kill or a crash (137 is the OOM killer). Whether it
    was one is decided here; True when the line was one."""
    m = _PROCESS_EXIT.match(line)
    if m and int(m.group(1)) != BUN_FAILED_EXIT:
        hard.append(f"the step's process exited {m.group(1)}")
    return bool(m)


def parse_unit_log(text):
    """The failures of a unit-tests job from its log. scripts/verify/test-files.ts runs one `bun test` per file and replays its output
    under `== bun test --isolate <file>`; bun prints `(fail) <suite > test> [<ms>]` per failed test and a summary of ` N fail` and
    ` N error`. Its own lines name a file that did not finish: `test-files: <file> was stopped at <limit>` and
    `terminated by <signal>` are hard, `exited N` is coarse (no test named) and hard when N is not bun's 1. A job cut by timeout-minutes
    or a cancel shows GitHub's own line, which is hard too."""
    runs, coarse, hard = _Runs(), set(), []
    for line in log_lines(text):
        if m := re.match(r"== bun test --isolate (.+)$", line):
            runs.start(m.group(1))
        elif m := _FAIL.match(line):
            runs.here().fails.append(m.group(1))
        elif m := _SUMMARY.match(line):
            runs.here().reported += int(m.group(1))
        elif _RAN.match(line):
            runs.here().ran = True
        elif _UNHANDLED.match(line):
            runs.here().unhandled = True
        elif m := re.match(r"test-files: (.+?) exited (\d+)$", line):
            _exit_of(runs, coarse, hard, m.group(1), m.group(2))
        elif m := re.match(r"test-files: (.+?) was stopped at (.*)$", line):
            coarse.add(m.group(1))
            hard.append(f"{m.group(1)} was stopped at {m.group(2)}")
        elif m := re.match(r"test-files: (.+?) terminated by (\S+); stopping$", line):
            coarse.add(m.group(1))
            hard.append(f"{m.group(1)} was terminated by {m.group(2)}")
        elif _runner_line(line, hard):
            pass
        elif _CUT.search(line):
            hard.append(f"the job was cut short ({line.strip()[:120]})")
    return Parsed(*_runs_parsed(runs, coarse, hard))


def parse_integration_log(text):
    """The failures of an integration shard from its log. testRunner.ts prints each target's output between
    `[integration] ── target N/M (...): <target>` and `── end target`. A target that failed with no `(fail)` line in it is coarse, and hard when
    its exit code is not bun's 1; one killed by its watchdog, one left unfinished when the shard was cut short, and a crashed runner
    (`Integration runner crashed`) are hard."""
    runs, coarse, hard, listing = _Runs(), set(), [], False
    for line in log_lines(text):
        if listing and (m := re.match(r" - (.+?)(?: \(exit code (\d+)\))?$", line)):
            if listing == "cut":
                coarse.add(m.group(1))
                hard.append(f"{m.group(1)} did not finish: the shard was cut short")
            elif m.group(2):
                _exit_of(runs, coarse, hard, m.group(1), m.group(2))
            continue
        listing = False
        if m := re.match(r"\[integration\] ── target \S+ \(.*\): (.+)$", line):
            runs.start(m.group(1))
        elif line.startswith("[integration] ── end target"):
            runs.end()
        elif m := _FAIL.match(line):
            runs.here().fails.append(m.group(1))
        elif m := _SUMMARY.match(line):
            runs.here().reported += int(m.group(1))
        elif _RAN.match(line):
            runs.here().ran = True
        elif _UNHANDLED.match(line):
            runs.here().unhandled = True
        elif m := re.match(r"\[integration\] Target failed with exit code (\d+): (.+)$", line):
            _exit_of(runs, coarse, hard, m.group(2), m.group(1))
        elif m := re.match(r"\[integration\] Target hung past its watchdog and was killed: (.+)$", line):
            coarse.add(m.group(1))
            hard.append(f"{m.group(1)} hung past its watchdog and was killed")
        elif re.match(r"\[integration\] \d+ of \d+ target\(s\) did not finish before \w+:$", line):
            listing = "cut"
        elif re.match(r"\[integration\] \d+ integration target\(s\) failed:$", line):
            listing = "failed"
        elif m := re.match(r"\[integration\] Integration runner crashed: (.*)$", line):
            hard.append(f"the integration runner crashed: {m.group(1)[:120]}")
        elif _runner_line(line, hard):
            pass
        elif _CUT.search(line):
            hard.append(f"the job was cut short ({line.strip()[:120]})")
    return Parsed(*_runs_parsed(runs, coarse, hard))


@dataclasses.dataclass(frozen=True)
class NameSource:
    """Where a job's failing test names come from: the step that runs the tests and the parser of its log."""
    step: str
    parse: object
    what: str


# job name -> NameSource. A job with no entry keeps the job-level rule. Both jobs print their failures in the job log, which the App's
# Actions: read permission already covers (the timeline reads logs on it): one call per failed job. check-run annotations would be
# smaller but need Checks: read, which the App is not documented to hold; unit-tests has no annotation naming a test either.
NAME_SOURCES = {
    "unit-tests": NameSource("Run tests", parse_unit_log, "bun's `(fail)` lines under each `== bun test` header of the job log"),
    "integration": NameSource("Run integration tests", parse_integration_log, "bun's `(fail)` lines and the target lines of the job log"),
}


@dataclasses.dataclass(frozen=True)
class Read:
    """The failures of one failed job. names (tests and coarse) empty means none was named: `outside` when the job failed in a step
    other than the tests' (so it names no test and says which step), else `why` it could not be read. tests, coarse and hard are Parsed's,
    hard also holding a cancelled step or a failed step other than the tests'."""
    tests: frozenset = frozenset()
    why: str = ""
    outside: bool = False
    coarse: frozenset = frozenset()
    hard: tuple = ()

    @property
    def names(self):
        return self.tests | self.coarse


@dataclasses.dataclass
class Tests:
    """Everything the failures row reads about test names. reads: Obs.job_id -> Read, for each superci failure the job-level rule would
    count. red: test name -> the Blacksmith main runs it failed in. main_runs/main_failed: Blacksmith main runs of the window that ended
    red and the failed jobs read in them. main_unread: (run id, why) of a failed Blacksmith main job whose log could not be read.
    red_jobs: test name -> how many failed Blacksmith main jobs it failed in. b_reads: Blacksmith job id -> Read, for each failed main job
    read. excused: the tests that excused a superci failure (set by judge_by_tests); the Blacksmith failure rate leaves out a main job
    only when every test it failed is among them."""
    source: str
    reads: dict
    red: dict
    main_runs: int = 0
    main_failed: int = 0
    main_unread: list = dataclasses.field(default_factory=list)
    logs: int = 0
    red_jobs: dict = dataclasses.field(default_factory=dict)
    b_reads: dict = dataclasses.field(default_factory=dict)
    excused: frozenset = frozenset()


class OverBudget(Exception):
    pass


def step_reasons(steps, test_step):
    """Why a job's steps say it failed in more than a red test: a cancelled step (timeout-minutes or a cancel cut it), a step with no
    conclusion on a finished job (its runner was lost) or a failed step other than the tests'."""
    out = []
    for s in steps:
        if s.get("conclusion") is None:
            out.append(f"step '{s.get('name')}' has no conclusion on a finished job: the runner was lost")
        elif s.get("conclusion") == "cancelled":
            out.append(f"step '{s.get('name')}' was cancelled: the job was cut short")
        elif s.get("conclusion") == "failure" and s.get("name") != test_step:
            out.append(f"step '{s.get('name')}' failed")
    return tuple(out)


class NameReader:
    """What a failed job's log says failed, one log call each, kept in the cache like the run listings (only a finished job is read, so
    only a finished job is kept). A job whose steps are known and whose test step succeeded names no test and costs no call."""

    def __init__(self, gh, cache, source, limit=MAX_TEST_READS):
        self.gh, self.cache, self.source, self.limit, self.logs = gh, cache, source, limit, 0

    def read(self, job):
        steps = job.get("steps") or []
        tested = next((s for s in steps if s.get("name") == self.source.step), None)
        if tested is not None and tested.get("conclusion") in ("success", "skipped"):
            bad = next((s.get("name") for s in steps if s.get("conclusion") in ("failure", "cancelled")), None)
            return Read(frozenset(), f"failed outside the '{self.source.step}' step" + (f", in '{bad}'" if bad else ""), outside=True)
        if not job.get("completed_at"):
            return Read(why="the job had not finished")

        def fetch():
            if self.logs >= self.limit:
                raise OverBudget
            self.logs += 1
            p = self.source.parse(self.gh.api(f"actions/jobs/{job['id']}/logs", keep=False, raw=True))
            return {"tests": sorted(p.tests), "coarse": sorted(p.coarse), "hard": list(p.hard),
                    "why": "" if p.tests or p.coarse or p.hard else f"the log of {job.get('name')} names no failing test"}

        try:
            got = self.cache.get(f"tests:v{PARSER_VERSION}:{self.gh.repo}:{job['id']}", fetch)
        except OverBudget:
            return Read(why=f"not read: the {self.limit} log reads of --max-test-reads are used up")
        except UnicodeDecodeError as e:
            return Read(why=f"log not read (it is not valid UTF-8: {str(e)[:100]})")
        except SystemExit as e:
            if "budget is used up" in str(e) or "rate limit" in str(e).lower():
                raise SystemExit(f"stopping: {e}")
            return Read(why=f"log not read ({str(e)[:160]})")
        return Read(frozenset(got["tests"]), got["why"], coarse=frozenset(got["coarse"]), hard=tuple(got["hard"]) + step_reasons(steps, self.source.step))


def census_leg(name):
    """A leg of integration's V3 census (`integration (3, v3)`), which runs every shard under another owner policy: not an ordinary run."""
    return bool(re.search(r", v3\)$", name))


def main_red_runs(listed, job_window):
    """The Blacksmith main runs of the window that ended red, from the run listing the gate already holds (no call). A run started by
    hand (workflow_dispatch: a census, a hoist record) is not an ordinary push or schedule, so it is left out."""
    lo, hi = job_window
    return [r for r in listed if r.get("head_branch") == "main" and r.get("event") != "workflow_dispatch" and lo <= (T.ts(r.get("created_at")) or 0) <= hi
            and r.get("conclusion") in ("failure", "timed_out", "cancelled")]


def gather_tests(gh, cache, source, name, candidates, s_jobs, events, listed, window, limit=MAX_TEST_READS):
    """Tests for the failures row. `candidates` are the superci failures the job-level rule would count, `s_jobs` the REST-shaped jobs
    of S by id. Superci's side comes first in the read budget. A record has no GitHub job id, so each failed record is matched to its
    REST job in its run's listing (one call a run, cached, as with_github_created does), which also gives the job's steps.
    Blacksmith's side is every failed `name` job of the ordinary main runs listed in `window` that ended red (not only the sample the
    rates are drawn from; no census leg, no manual run), read once and then cached. Only test-level names are red: a file or target
    named for failing with no test is not."""
    src, reader, reads = NAME_SOURCES[name], NameReader(gh, cache, NAME_SOURCES[name], limit), {}
    listing = jobs_of(gh, [{"id": r} for r in sorted({s_jobs[o.job_id]["run_id"] for o in candidates})], attempts="all") if events and candidates else {}
    for o in candidates:
        j = s_jobs[o.job_id]
        if events:
            j = rest_job_of({"name": j.get("name"), "runner": j.get("runner_name"), "finish_time": j.get("completed_at")}, listing.get(j["run_id"], []))
            if j is None:
                reads[o.job_id] = Read(why="no job of its run in GitHub's listing matches the record (name, runner, finish), so its log cannot be found")
                continue
        reads[o.job_id] = reader.read(j)
    runs = main_red_runs(listed, window)
    by_run = jobs_of(gh, runs, attempts="all") if runs else {}
    t = Tests(source, reads, {}, main_runs=len(runs))
    red, red_jobs = collections.defaultdict(set), collections.Counter()
    for r in runs:
        for j in by_run.get(r["id"], []):
            if (not matches(j.get("name") or "", name) or census_leg(j.get("name") or "") or j.get("conclusion") not in ("failure", "timed_out")
                    or fleet_or_label(j) != "blacksmith"):
                continue
            t.main_failed += 1
            got = t.b_reads[j["id"]] = reader.read(j)
            for n in got.tests:
                red[n].add(r["id"])
                red_jobs[n] += 1
            if not got.names and not got.outside:
                t.main_unread.append((r["id"], got.why or "; ".join(got.hard) or "no reason given"))
    t.red, t.red_jobs, t.logs = {n: sorted(ids) for n, ids in red.items()}, dict(red_jobs), reader.logs
    return t


def judge_by_tests(counted, tests, show=20):
    """(counted, notes, detail): the superci failures the job-level rule counted, less those excused, and the lines that say what was
    excluded and why each other failure counted. A failure is excused only when it is explained by test-level names alone, each red on
    Blacksmith's main in the window: it counts when a failing test is in no failed Blacksmith main job of the window, when its job timed out,
    when it has a reason beyond its named tests (a file stopped or killed, a shard cut short, a runner crash, an error between tests, a
    summary counting more failures than the log names, a cancelled or failed non-test step), when it is named by a file or target alone,
    and when its tests cannot be read (fail closed). Nothing is dropped without a line. Sets tests.excused."""
    kept, gone, fresh = [], [], {}
    unread, outside, beyond, coarse = (collections.defaultdict(list) for _ in range(4))
    for o in counted:
        r = tests.reads.get(o.job_id) or Read(why="no read was made")
        if o.timed_out:
            kept.append(o)
            beyond["the job timed out"].append(o.run_id)
            continue
        if r.hard:
            kept.append(o)
            for why in r.hard:
                beyond[why].append(o.run_id)
            continue
        if not r.names:
            kept.append(o)
            (outside if r.outside else unread)[r.why].append(o.run_id)
            continue
        if r.coarse:
            kept.append(o)
            coarse[f"named only by its file or target ({', '.join(sorted(r.coarse))[:160]}): no test-level name to compare"].append(o.run_id)
            continue
        new = sorted(n for n in r.tests if n not in tests.red)
        if new:
            kept.append(o)
            fresh.setdefault(o.run_id, set()).update(new)
        else:
            gone.append(o)
    detail, notes = [], []
    tests.excused = frozenset(n for o in gone for n in tests.reads[o.job_id].tests)
    if gone:
        names = sorted(tests.excused)
        runs = sorted({o.run_id for o in gone})
        red_runs = sorted({i for n in names for i in tests.red[n]})
        once = " (1 Blacksmith failure: flaky?)"  # an exclusion that rests on one failure of the test on main
        detail.append(f"excluded (red on Blacksmith main too): {', '.join(n + (once if tests.red_jobs.get(n) == 1 else '') for n in names)} in {', '.join(map(str, runs))}")
        detail.append(f"  red on Blacksmith main in runs {', '.join(map(str, red_runs))}")
        notes.append(f"{len(gone)} more failed only on tests red on Blacksmith main too (excluded, listed below)")
    if fresh:
        names = sorted({n for ns in fresh.values() for n in ns})
        detail.append(f"counted, green on Blacksmith main: {', '.join(names[:show])}" + (f" and {len(names) - show} more" if len(names) > show else "") + f" in {', '.join(map(str, sorted(fresh)))}")
    for label, group in (("failed beyond its named tests (fail closed)", beyond), ("named by file or target only (fail closed)", coarse),
                         ("failing tests unreadable (fail closed)", unread), ("no test failed", outside)):
        for why, runs in sorted(group.items()):
            detail.append(f"counted, {label}: run {', '.join(map(str, sorted(set(runs))))}: {why}")
    if tests.main_unread:
        detail.append(f"Blacksmith main: {len(tests.main_unread)} of {tests.main_failed} failed jobs could not be read ({tests.main_unread[0][1]}); no exclusion is drawn from them")
    return kept, notes, detail


# ---------------------------------------------------------------------------------------------------------------------
# The listener's job records: what superci's side of every comparison is read from, with no GitHub call (but for a dispatched job's created_at).
# ---------------------------------------------------------------------------------------------------------------------

RECORDS_WHERE = 'msg = "tl" and ev = "job.record"'
TIMEOUT_SLACK_S = 5.0  # a failed job that ran to within this of the job's ceiling timed out
EVENTS_BASELINE_RUNS = 100  # Blacksmith's side still comes from GitHub: a spread sample of this many runs, cached, since a finished job never changes


def job_records(profile, lo, hi):
    """The listener's job.record events written in [lo, hi], one per job: GitHub redelivers a completion until the listener
    acknowledges it, so a job can have two lines and the first is kept. A partial record (a completion after a listener restart, its start unknown) is left out, but a job cancelled before any runner took it (partial, cancelled, no runner) is kept: it is a queue-wait lower bound, which a slow plane must not hide."""
    seen, out = set(), []
    for e in T.listener_events(profile, lo, hi, where=RECORDS_WHERE):
        if e.get("partial") and not (e.get("conclusion") == "cancelled" and not e.get("runner")):
            continue  # a completion after a listener restart: its start is unknown, so it has no queue wait or wall time to count.
            #           A job cancelled before any runner took it has no start either, and is kept: its queue wait is a lower bound
        if e.get("job") and e["job"] not in seen:
            seen.add(e["job"])
            out.append(e)
    return out


def first_record(profile, now):
    """When the listener wrote its first job.record in the last 30 days (epoch seconds), or None when it wrote none."""
    rows = T.insights(profile, CW_REGION, T.LISTENER_GROUP, f'filter {RECORDS_WHERE} | stats min(t_ms) as first', now - 30 * 86400, now)
    return int(rows[0]["first"]) / 1000 if rows and rows[0].get("first") else None


def resolve_source(a, since, now):
    """The source superci's jobs are read from. `auto` (the default) reads the listener's records when the window starts after the first
    one and GitHub otherwise, so a window from before the records is never read as empty. Said on the first line of the output."""
    if a.superci_source != "auto":
        return a.superci_source
    first = first_record(a.profile, now)
    return "events" if first is not None and since >= first + 60 else "github"


def record_branch(e):
    """The branch (or other ref) of a record's workflow_ref, `owner/repo/.github/workflows/<file>@refs/heads/<branch>`. A pull
    request's ref is refs/pull/<n>/merge: the message names no head branch, so such a record matches only a glob that fits it."""
    return (e.get("workflow_ref") or "").partition("@")[2].removeprefix("refs/heads/")


def record_workflow(e):
    return os.path.basename((e.get("workflow_ref") or "").partition("@")[0])


def synth_job(e, group=T.SUPERCI_RUNNER_GROUP):
    """A record as the REST job the rest of the gate reads, so one code path decides every row. A job no runner took has no runner
    and no started_at, which is how a job cancelled in the queue is told from one cancelled while running; its label (the set's
    name when the message carried none) places it on superci. A record has no steps."""
    started = bool(e.get("wall_ms")) and e.get("start_time")
    return {"id": int(hashlib.sha1(e["job"].encode()).hexdigest()[:12], 16), "run_id": e.get("run") or 0, "name": e.get("name") or "",
            "head_branch": record_branch(e), "created_at": e.get("queue_time") or e.get("start_time") or e.get("finish_time"),
            "started_at": e["start_time"] if started else None, "completed_at": e.get("finish_time"), "conclusion": e.get("conclusion"),
            "run_attempt": 1, "runner_name": e.get("runner") if started else None,
            "runner_group_name": group if started else None, "labels": list(e.get("labels") or []) or [e.get("set") or ""], "set": e.get("set"), "steps": []}


def timeline_job(j):
    """The job as `timeline` accounts it: up to the moment its runner took it. A record has no steps to explain the job's own
    run, which the gate's unexplained row is not about (the row is the plane's time). A dispatched job's timeline starts where
    the dispatcher saw it: the wait before that, for a free identity, is the queue wait rows' and the starvation alarm's."""
    return dict(j, completed_at=j["started_at"], created_at=j.get("arrived_at") or j["created_at"])


MATCH_S = 120.0  # a record's finish and GitHub's completed_at of the same job are seconds apart; another attempt's are not


def rest_job_of(e, jobs):
    """The REST job a dispatched record is, among the jobs of its run (every attempt): the one of its name that its identity
    ran and that completed nearest its finish, within MATCH_S. None when no job fits, or when a record with no finish fits two."""
    mine = [g for g in jobs if g.get("name") == (e.get("name") or "") and g.get("created_at") and (not e.get("runner") or g.get("runner_name") == e["runner"])]
    done = T.ts(e.get("finish_time"))
    if done is None:
        return mine[0] if len(mine) == 1 else None
    near = [g for g in mine if g.get("completed_at") and abs(T.ts(g["completed_at"]) - done) <= MATCH_S]
    return min(near, key=lambda g: abs(T.ts(g["completed_at"]) - done)) if near else None


def with_github_created(gh, recs, group=T.SUPERCI_RUNNER_GROUP):
    """(jobs, dispatched, unmatched): synth_job of each record, a dispatched record's created_at being GitHub's own.

    A dispatched job waits for a free identity at GitHub, before its identity's shim brings it to the dispatcher, and the
    record's queue_time is when the shim did. A size short of identities would read as fast, so the wait is measured from the
    REST job's created_at, found by one jobs listing per run (all attempts, cached: a finished run never changes). A rerun
    attempt keeps its first attempt's created_at, so it counts for failures and wall time and not for queue wait, as on GitHub's
    path. `unmatched` are the dispatched records no REST job was found for: they keep the shim's arrival, a lower bound, and a
    row fails on them."""
    runs = sorted({e["run"] for e in recs if e.get("dispatch") and e.get("run")})
    listed = jobs_of(gh, [{"id": r} for r in runs], attempts="all") if runs else {}
    found = lambda e: rest_job_of(e, listed.get(e.get("run"), []))
    # A cached listing can be older than the job: read while its run was in flight, or before a rerun's attempt began. A record it
    # does not match has its run listed again, once and uncached, which also replaces the entry.
    stale = sorted({e["run"] for e in recs if e.get("dispatch") and e.get("run") and found(e) is None})
    if stale:
        again = jobs_of(gh, [{"id": r} for r in stale], attempts="all", fresh=True)
        listed.update({r: again[r] or listed.get(r, []) for r in stale})
    jobs, dispatched, unmatched = [], 0, []
    for e in recs:
        j = synth_job(e, group)
        if e.get("dispatch"):
            dispatched += 1
            g = found(e)
            if g is None:
                unmatched.append(e)
            else:
                j = dict(j, created_at=g["created_at"], arrived_at=j["created_at"], run_attempt=g.get("run_attempt") or 1)
        jobs.append(j)
    return jobs, dispatched, unmatched


def dispatched_row(dispatched, unmatched):
    """A row that fails when a dispatched record's wait could not be measured from GitHub's created_at: it would read from the
    shim's arrival and hide the wait for an identity. No row when no record was dispatched."""
    if not dispatched:
        return []
    miss = len(unmatched)
    return [Row("dispatched wait from GitHub", f"{dispatched - miss} of {dispatched}", "all read from GitHub's created_at", PASS if not miss else FAIL,
                "" if not miss else f"no REST job found for {[e.get('job') for e in unmatched[:3]]}: their wait reads from the shim's arrival, a lower bound that hides a wait for an identity")]


# ---------------------------------------------------------------------------------------------------------------------
# Gate 2: scale of the plane over a replay window.
# ---------------------------------------------------------------------------------------------------------------------

@dataclasses.dataclass
class Scale:
    """Everything gate 2 reads, already fetched. None for a source that gave nothing."""
    replayed: int
    completed: int
    stranded: list            # (job name, conclusion or status) of replayed jobs that did not complete
    lost: int                 # "job lost with its machine" lines in the window
    waits: list               # queue wait seconds of the replayed jobs
    window_use: list          # (epoch, registrations in the last 5 min, ours and others) a minute
    org_known: list           # OrgRateKnown samples
    claims: set               # instance ids with an issued/ object made in the window
    issued: collections.Counter    # instance id -> jit.issued events
    claim_taken: collections.Counter
    claim_refused: int
    launched: list            # lifecycle of each machine.launched in the window, "fast" for a size's on_demand_first types
    orphans: list             # (epoch, Orphans) from 10 min after the replay
    orphans_due: float
    lo: float                 # the replay window
    hi: float
    alarms: dict              # alarm name -> state at the end of the replay
    queuewait_samples: float  # QueueWait datapoints published in the window
    waited_asks: int = 0
    max_wait_s: float = 0.0
    dispatched: int = 0           # replayed jobs the identity plane ran, whose waits are read from GitHub's created_at
    dispatched_unmatched: list = dataclasses.field(default_factory=list)
    # instance id -> the dispatched job.records of the window's machines (handed in it). A dispatch size registers no runner:
    # its machine's claim is taken when the dispatcher pairs it with its one job, so a dispatched record stands where
    # jit.issued does
    dispatched_machines: collections.Counter = dataclasses.field(default_factory=collections.Counter)


def handed_machines(records, lo, hi):
    """instance id -> the job.records of dispatched jobs handed to it in [lo, hi], every set's, as the claims are: the handed
    time (start_time) places a record, not its write at the job's end. Pure."""
    return collections.Counter(e["inst"] for e in records if e.get("dispatch") and e.get("inst") and lo <= (T.ts(e.get("start_time")) or 0) <= hi)


def evaluate_scale(s):
    """Gate 2's rows. Pure."""
    rows = []
    done = s.completed == s.replayed and not s.stranded and s.replayed > 0 and not s.lost
    never = max(0, s.replayed - s.completed - len(s.stranded))
    rows.append(Row("completion", f"{s.completed} of {s.replayed} replayed, {s.lost} lost with a machine", "all complete, none stranded, 0 lost", PASS if done else FAIL,
                    "" if done else f"{never} never ran, {len(s.stranded)} ran and did not succeed: {s.stranded[:5]}"))
    if s.waits:
        p50, p90 = T.q(s.waits, 50), T.q(s.waits, 90)
        rows.append(at_most("queue wait p50", p50, QUEUE_P50_S, f"<= {QUEUE_P50_S:g} s"))
        rows.append(at_most("queue wait p90", p90, QUEUE_P90_S, f"<= {QUEUE_P90_S:g} s"))
    else:
        rows += [Row("queue wait p50", "-", f"<= {QUEUE_P50_S:g} s", FAIL, "no started jobs"), Row("queue wait p90", "-", f"<= {QUEUE_P90_S:g} s", FAIL, "no started jobs")]
    limit = REGISTRATION_SHARE * ORG_CAP
    if s.window_use:
        t, peak = max(s.window_use, key=lambda p: p[1])
        known = bool(s.org_known) and min(s.org_known) >= 1
        minutes = range(int(s.lo // 60), math.ceil(s.hi / 60))
        seen = {int(e // 60) for e, _ in s.window_use}
        gaps = [m for m in minutes if m not in seen]
        ok = peak <= limit and known and not gaps
        notes = [] if known else ["the org rate was unknown for part of the window (blind rate only)"]
        if gaps:
            notes.append(f"no datapoint for {len(gaps)} of {len(minutes)} minutes of the replay window, the first at {T.iso(gaps[0] * 60)}: the peak may sit in the gap")
        rows.append(Row("org registrations per 5 min", f"max {peak:.0f} at {T.iso(t)}", f"<= {limit:.0f} (80% of {ORG_CAP}), a datapoint every minute", PASS if ok else FAIL, "; ".join(notes)))
    else:
        rows.append(Row("org registrations per 5 min", "-", f"<= {limit:.0f} (80% of {ORG_CAP})", FAIL, "no RegistrationWindowUse datapoints"))
    # A scale set's machine is claimed by its grant, which writes jit.claim, and must then register. The dispatcher claims a
    # machine when it pairs it with a job, and writes no jit.claim: a machine it spends before the hand-off (its JOB send
    # failed, AWS took it back) keeps a claim and runs nothing, which is the fence working. Neither may run a job unclaimed
    # or a second job.
    disp = s.dispatched_machines
    twice = (sorted(i for i, n in s.issued.items() if n > 1) + sorted(i for i, n in s.claim_taken.items() if n > 1 and s.issued.get(i, 0) <= 1)
             + sorted(i for i, n in disp.items() if n > 1))
    ran = set(s.issued) | set(disp)
    unregistered = sorted((s.claims & set(s.claim_taken)) - set(s.issued))
    spent = sorted(s.claims - set(s.claim_taken) - ran)
    unclaimed = sorted(ran - s.claims)
    one = not twice and not unregistered and not unclaimed and bool(s.claims)
    notes = [f"the fence refused {s.claim_refused} duplicate grant(s)"] if s.claim_refused else []
    if spent:
        notes.append(f"{len(spent)} dispatch machine(s) spent after their claim, with no job: {spent[:3]}")
    rows.append(Row("one runner per machine", f"{len(s.claims)} claims, {len(s.issued)} registrations, {len(disp)} dispatched, {len(twice)} claimed twice",
                    "a grant's claims = registrations, every registration and hand-off claimed, 0 twice", PASS if one else FAIL,
                    f"granted, not registered {unregistered[:3]}, registered or dispatched, not claimed {unclaimed[:3]}, twice {twice[:3]}; fence refused {s.claim_refused}" if not one else
                    "; ".join(notes)))
    if s.orphans:
        peak = max(v for _, v in s.orphans)
        rows.append(Row("orphans 10 min after", f"max {peak:.0f}", "0", PASS if peak == 0 else FAIL))
    else:
        rows.append(Row("orphans 10 min after", "-", "0", FAIL, f"no Orphans datapoint at {T.iso(s.orphans_due)}+: run it after then"))
    # the share is of the launches on_demand_share bounds: a fast-* launch (an on_demand size, or an on_demand_first type)
    # is outside it, in neither the count nor the whole
    od = sum(1 for l in s.launched if l not in ("spot", "fast"))
    fast = sum(1 for l in s.launched if l == "fast")
    bounded = len(s.launched) - fast
    share = od / bounded if bounded else (0.0 if s.launched else None)
    rows.append(Row("on-demand share", "-" if share is None else f"{100 * share:.1f}% ({od} of {bounded})", f"<= {100 * ON_DEMAND_SHARE:.0f}%",
                    PASS if share is not None and share <= ON_DEMAND_SHARE else FAIL,
                    "no machine.launched events" if not s.launched else f"{fast} of an on_demand size or on_demand_first type, outside the share" if fast else ""))
    firing = sorted(n for n, st in s.alarms.items() if st == "ALARM")
    rows.append(Row("alarms at the end", f"{len(firing)} in ALARM of {len(s.alarms)}", "0 in ALARM", PASS if not firing and s.alarms else FAIL,
                    f"firing: {firing}" if firing else "" if s.alarms else "no alarms listed"))
    pub = s.queuewait_samples or 0
    rows.append(Row("QueueWait published", f"{pub:.0f} samples", "> 0", PASS if pub > 0 else FAIL, f"{len(s.waits)} jobs started in the replay"))
    return rows + dispatched_row(s.dispatched, s.dispatched_unmatched)


# ---------------------------------------------------------------------------------------------------------------------
# Arrival traces: what replay.sh drives.
# ---------------------------------------------------------------------------------------------------------------------

LABEL = re.compile(r"^(?:blacksmith-(\d+)vcpu-ubuntu-\d+|superci-(\d+)cpu)$")


def superci_label(labels):
    """superci-<n>cpu for a job that asked for Blacksmith's or superci's Linux x64 runner of a size superci has, else None."""
    for l in labels or []:
        m = LABEL.match(l)
        if m and int(m.group(1) or m.group(2)) in SIZES:
            return f"superci-{m.group(1) or m.group(2)}cpu"
    return None


def build_trace(jobs, start, hours=1, bucket_s=60, source=None):
    """The arrival trace of REST jobs created from `start` for `hours`: each Linux job's creation offset, label and run
    time. Jobs that never ran (skipped, or cancelled before a runner took them) and other runner kinds are counted, not listed. Pure."""
    lo, hi = start, start + 3600 * hours
    legs, dropped = [], collections.Counter()
    for j in jobs:
        created = T.ts(j.get("created_at"))
        if created is None or not (lo <= created < hi):
            continue
        label = superci_label(j.get("labels"))
        if label is None:
            dropped["other runner: " + (",".join(j.get("labels") or []) or "none")] += 1
        elif not (j.get("started_at") and j.get("completed_at")) or j.get("conclusion") == "skipped":
            dropped[f"{label} job that did not run ({j.get('conclusion') or j.get('status')})"] += 1
        else:
            legs.append({"t": round(created - lo, 1), "label": label, "duration_s": round(max(1.0, T.ts(j["completed_at"]) - T.ts(j["started_at"])), 1),
                         "name": j["name"], "run": j["run_id"]})
    legs.sort(key=lambda l: l["t"])
    return {"version": 1, "start": T.iso(lo), "hours": hours, "bucket_s": bucket_s, "source": source or {},
            "dropped": dict(dropped), "jobs": legs}


# ---------------------------------------------------------------------------------------------------------------------
# Sources.
# ---------------------------------------------------------------------------------------------------------------------

def gh_raw(path, ref="main"):
    """A file of the repository at ref, read on the App's contents token (Contents: read, megpt-mono alone). With none the command refuses and says how to go without."""
    try:
        p = T.AUTH.gh(["api", "-H", "Accept: application/vnd.github.raw", f"repos/{REPO}/contents/{path}?ref={ref}"], scope="contents")
    except T.apptoken.AppTokenRefused as e:
        raise T.apptoken.AppTokenRefused(f"{e}\nTo make no such call, pass --limits FILE: a copy of {path} from main.") from None
    if p.returncode:
        raise SystemExit(f"gh api contents/{path}@{ref}: {p.stderr.strip()[:300]}")
    return p.stdout


def runs_of(gh, workflow, lo, hi):
    """Runs of a workflow (or of every workflow when None) created in [lo, hi]. GitHub lists at most 1,000 for a query, so a
    window that holds more is halved first."""
    past = hi < time.time() - 600
    base = f"actions/workflows/{workflow}/runs" if workflow else "actions/runs"
    window = f"created={T.iso(lo)}..{T.iso(hi)}"
    if gh.api(f"{base}?{window}&per_page=1", keep=past)["total_count"] > 1000 and hi - lo > 2:
        mid = (lo + hi) // 2
        runs = {r["id"]: r for r in runs_of(gh, workflow, lo, mid) + runs_of(gh, workflow, mid + 1, hi)}
        return sorted(runs.values(), key=lambda r: r["created_at"])
    out, page = [], 1
    while True:
        d = gh.api(f"{base}?{window}&per_page=100&page={page}", keep=past)
        out += d["workflow_runs"]
        if len(d["workflow_runs"]) < 100:
            return out
        page += 1


def sample(items, n):
    """At most n items spread evenly through the list, the same ones on every rerun (so the GitHub cache serves them)."""
    if len(items) <= n:
        return list(items)
    return [items[int(i * len(items) / n)] for i in range(n)]


def jobs_of(gh, runs, workers=12, attempts="latest", fresh=False):
    """{run id: its jobs} for a list of runs. A run GitHub will not list (a 5xx that outlasts the retries) is left out and
    recorded in gh.skipped; an exhausted rate limit stops the command, since every later call fails the same way."""
    skipped = getattr(gh, "skipped", None)
    if skipped is None:
        skipped = gh.skipped = []

    def one(r):
        try:
            return gh.run_jobs(r["id"], attempts, fresh=True) if fresh else gh.run_jobs(r["id"], attempts)
        except SystemExit as e:
            if "budget is used up" in str(e) or "rate limit" in str(e).lower():
                raise SystemExit(f"stopping: {e}")
            skipped.append(r["id"])
            return []

    with concurrent.futures.ThreadPoolExecutor(workers) as pool:
        got = list(pool.map(one, runs))
    return {r["id"]: j for r, j in zip(runs, got)}


def metric(profile, name, start, end, stat="Maximum", period=60, dimensions=()):
    """[(epoch, value)] of a superci-pull metric, in calls of at most 1,440 datapoints."""
    out, step = [], 1440 * period
    for lo in range(int(start), int(end), step):
        hi = min(int(end), lo + step)
        d = T.aws(profile, "cloudwatch", "get-metric-statistics", "--region", CW_REGION, "--namespace", NAMESPACE, "--metric-name", name,
                  "--start-time", T.iso(lo), "--end-time", T.iso(hi), "--period", str(period), "--statistics", stat, *dimensions)
        out += [(T.ts(p["Timestamp"]), p[stat]) for p in d.get("Datapoints", [])]
    return sorted(out)


def alarm_states(profile, at, now):
    """{alarm: state} of the superci-pull alarms as of `at`: the last StateUpdate not after it, else the current state."""
    d = T.aws(profile, "cloudwatch", "describe-alarms", "--region", CW_REGION, "--alarm-name-prefix", "superci-pull-")
    out = {}
    for a in d.get("MetricAlarms", []) + d.get("CompositeAlarms", []):
        name, state = a["AlarmName"], a["StateValue"]
        if at < now - 120:
            h = T.aws(profile, "cloudwatch", "describe-alarm-history", "--region", CW_REGION, "--alarm-name", name, "--history-item-type", "StateUpdate",
                      "--start-date", T.iso(at - 14 * 86400), "--end-date", T.iso(now))
            items = sorted(((T.ts(i["Timestamp"]), json.loads(i["HistoryData"])) for i in h.get("AlarmHistoryItems", [])), key=lambda p: p[0])
            before = [x for x in items if x[0] <= at]
            after = [x for x in items if x[0] > at]
            state = before[-1][1]["newState"]["stateValue"] if before else after[0][1]["oldState"]["stateValue"] if after else state
        out[name] = state
    return out


def claim_objects(profile, lo, hi):
    """Instance ids of the claims bucket's issued/i-* objects (a policy proof leaves others there) last modified in [lo, hi] (a read-only listing)."""
    d = T.aws(profile, "s3api", "list-objects-v2", "--region", CW_REGION, "--bucket", CLAIMS_BUCKET, "--prefix", "issued/",
              "--query", "Contents[].[Key,LastModified]", "--output", "json")
    return {k.split("/", 1)[1] for k, m in (d or []) if re.fullmatch(r"issued/i-[0-9a-f]+", k) and lo <= T.ts(m) <= hi}


# ---------------------------------------------------------------------------------------------------------------------
# Commands.
# ---------------------------------------------------------------------------------------------------------------------

def unread(gh, allowed=0):
    """A row that fails when GitHub would not list more than `allowed` runs' jobs: the other rows were drawn without them.
    A run that fails every time (GitHub's own 5xx) is named so it can be allowed."""
    skipped = getattr(gh, "skipped", [])
    if not skipped:
        return []
    return [Row("every run read", f"{len(skipped)} skipped", f"<= {allowed}", PASS if len(skipped) <= allowed else FAIL,
                f"GitHub would not list jobs of runs {sorted(skipped)[:5]}: rerun (the cache keeps what was read) or --allow-unread")]


def print_rows(title, rows, verdict_name):
    w = max(len(r.name) for r in rows)
    vw = max(len(r.value) for r in rows)
    print(title)
    for r in rows:
        print(f"  {r.status:5} {r.name:{w}}  {r.value:{vw}}  {r.threshold}" + (f"   [{r.note}]" if r.note else ""))
        for line in r.detail:
            print(f"        {line}")
    bad = [r for r in rows if r.status != PASS]
    if not bad:
        print(f"\n{verdict_name}: PASS")
        return 0
    noisy = all(r.status == NOISE for r in bad)
    print(f"\n{verdict_name}: {'INCONCLUSIVE, add runs' if noisy else 'FAIL'} ({', '.join(r.name for r in bad)})")
    return 1


# Blacksmith's x64 Linux list price, per vCPU-minute, billed by the started minute (tools/cost/cost.py measured it from
# `blacksmith usage`, Nov 10-26).
BLACKSMITH_USD_PER_VCPU_MIN = 0.002


def cost_lines(records, blacksmith_jobs):
    """Lines of AWS's spend per job of each superci size beside Blacksmith's for the same job on the same vCPUs. AWS's is the
    listener's job.record: busy_usd, the machine while it ran the job, and usd, the machine's whole life (one job a machine,
    so a pool machine's idle wait is in it, and at low traffic it is most of it). Blacksmith's is its billed minutes x vCPUs x
    list price, over the jobs it billed: one that ran (a cancelled job that ran is billed), not one skipped or cancelled before
    it started (0 s, no minute billed). A record from before busy_usd was written counts in the whole-life mean only. The ratio
    is of the busy cost. Information, not a row: the canary judges speed and failures, the operator judges this. Pure."""
    busy_by, life_by = {}, {}
    for e in records:
        if e.get("usd") is not None and e.get("set") and e.get("wall_ms"):
            cpu = e["set"].removeprefix("superci-").removesuffix("cpu")
            life_by.setdefault(cpu, []).append(float(e["usd"]))
            if e.get("busy_usd") is not None:
                busy_by.setdefault(cpu, []).append(float(e["busy_usd"]))
    bs_by = {}
    for j in blacksmith_jobs:
        m = next((re.match(r"blacksmith-(\d+)vcpu", l) for l in j.get("labels") or [] if re.match(r"blacksmith-(\d+)vcpu", l)), None)
        st, done = T.ts(j.get("started_at")), T.ts(j.get("completed_at"))
        if m and st and done and done > st and j.get("conclusion") != "skipped":
            bs_by.setdefault(m.group(1), []).append(math.ceil((done - st) / 60) * int(m.group(1)) * BLACKSMITH_USD_PER_VCPU_MIN)
    mean = lambda xs: sum(xs) / len(xs)
    lines = []
    for cpu in sorted(set(life_by) | set(bs_by), key=lambda c: int(c) if c.isdigit() else 0):
        busy, life, b = busy_by.get(cpu, []), life_by.get(cpu, []), bs_by.get(cpu, [])
        busy_s = f"busy ${mean(busy):.4f} a job (n={len(busy)})" if busy else "busy: no record with busy_usd"
        aws = f"AWS {busy_s}, whole life ${mean(life):.4f} (n={len(life)})" if life else "AWS: no job.record with a cost"
        bs = f"Blacksmith {cpu}vcpu ${mean(b):.4f} (n={len(b)})" if b else f"Blacksmith {cpu}vcpu: no billed job"
        ratio = ""
        if life and b and mean(b) > 0:
            ratios = ([f"busy {mean(busy) / mean(b):.2f}x"] if busy else []) + [f"whole life {mean(life) / mean(b):.2f}x"]
            ratio = f": AWS {', '.join(ratios)}"
        lines.append(f"cost superci-{cpu}cpu: {aws}, {bs}{ratio}")
    return lines


def window_cost_line(spend, hours, records):
    """The line that decides the move: superci's whole spend over a window (every machine-second, idle pools included, from
    the listener's SpendPerHourUSD) over the jobs it served in it (every set's job.records with a wall), beside what Blacksmith
    bills for the same jobs (each one's billed minutes x its size's vCPUs x list price). Pure."""
    served = [e for e in records if e.get("wall_ms") and e.get("set")]
    if spend is None or not served:
        return f"cost of the window: no {'spend metric' if spend is None else 'job served'} over its {hours:.2f} h"
    spend_usd, found, expected = spend
    def vcpus(set_name):
        m = re.match(r"superci-(\d+)cpu", set_name)
        return int(m.group(1)) if m else 0
    bs = sum(math.ceil(e["wall_ms"] / 60000) * vcpus(e["set"]) * BLACKSMITH_USD_PER_VCPU_MIN for e in served)
    ratio = f": AWS {spend_usd / bs:.2f}x" if bs > 0 else ""
    gaps = f" [{found} of {expected} five-minute spend points: the rest read as the mean]" if found < expected else ""
    return (f"cost of the window ({hours:.2f} h): superci ${spend_usd:.2f} for {len(served)} jobs, ${spend_usd / len(served):.4f} a job;"
            f" Blacksmith ${bs:.2f} for the same jobs, ${bs / len(served):.4f} a job{ratio}{gaps}")


def spend_over(points, lo, hi):
    """(USD, points found, points expected) over [lo, hi] from the listener's SpendPerHourUSD five-minute averages (it publishes
    a point a minute, so an average is the bucket's mean rate): the mean rate x the window's hours, so partial buckets at the
    edges count for the time they cover, not five minutes each. None with no point. Pure."""
    if not points:
        return None
    rate = sum(v for _, v in points) / len(points)
    return rate * (hi - lo) / 3600, len(points), max(1, math.ceil((hi - lo) / 300))


def window_spend(profile, lo, hi):
    """spend_over of the listener's SpendPerHourUSD over [lo, hi]."""
    return spend_over(metric(profile, "SpendPerHourUSD", lo, hi, "Average", 300), lo, hi)


def show_spread(label, obs, ceiling=None):
    walls = [o for o in obs if o.ok]
    queued = [o for o in obs if o.queue is not None and not o.cancelled]
    for what, get, of, v in (("wall", lambda o: o.wall, walls, wall_sample(obs, ceiling)), ("queue", lambda o: o.queue, queued, queue_sample(obs))):
        if not v:
            print(f"  {label:11} {what:5} n=0")
            continue
        s = spread(v)
        print(f"  {label:11} {what:5} n={s['n']:<4} min {s['min']:7.1f}  p25 {s['p25']:7.1f}  p50 {s['p50']:7.1f}  p75 {s['p75']:7.1f}  p90 {s['p90']:7.1f}  max {s['max']:7.1f}   leg IQR {iqr(v):.1f}  run-to-run IQR {run_spread(of, get):.1f}")


def cmd_job(a):
    now = time.time()
    gh = T.GitHub(a.repo, T.Cache(a.cache))
    since = T.ts(a.since) if a.since else now - 86400
    b_hi = T.ts(a.baseline_until) if a.baseline_until else now
    b_lo = b_hi - 3600 * a.baseline_hours
    since, b_lo, floor_lines = clamp_window(a.job, since, b_lo, b_hi)
    limits = parse_limits(open(a.limits).read() if a.limits else gh_raw(LIMITS_PATH))
    ceiling = limits.get((a.workflow, a.job))
    events = resolve_source(a, since, now) == "events"
    # With the listener's job records the superci window needs no GitHub call: only Blacksmith's side is listed and read.
    listed = runs_of(gh, a.workflow, b_lo if events else min(since, b_lo), now)
    # The branch only picks which runs are read whole, to bound GitHub calls; which jobs are superci's is decided by the job's own fleet and group below.
    s_runs = [] if events else [r for r in listed if T.ts(r["created_at"]) >= since and fnmatch.fnmatch(r.get("head_branch") or "", a.branch_glob)]
    b_runs = [r for r in listed if b_lo <= T.ts(r["created_at"]) <= b_hi and r["id"] not in {x["id"] for x in s_runs}]
    b_runs = sample(b_runs, a.max_runs or (EVENTS_BASELINE_RUNS if events else 300))
    runs = s_runs + b_runs
    by_run = {r["id"]: r for r in runs}
    jobs = [(j, by_run[rid]) for rid, js in jobs_of(gh, runs, attempts="all").items() for j in js if matches(j["name"], a.job)]
    dispatched, unmatched = 0, []
    if events:
        jobs = [(j, r) for j, r in jobs if not is_superci(j, a.superci_group)]  # a sampled baseline run can hold superci jobs too: they come from the records alone
        recs = [e for e in job_records(a.profile, since, now + 600) if record_workflow(e) == a.workflow and fnmatch.fnmatch(record_branch(e), a.branch_glob)
                and matches(e.get("name") or "", a.job) and (T.ts(e.get("queue_time") or e.get("finish_time")) or 0) >= since]
        records, dispatched, unmatched = with_github_created(gh, recs, a.superci_group)
        jobs += [(j, {"head_branch": j["head_branch"], "head_sha": ""}) for j in records]
    jobs_all = [j for j, r in jobs]
    S_jobs = [j for j, r in jobs if is_superci(j, a.superci_group) and T.ts(j["created_at"]) >= since]
    B_jobs = [j for j, r in jobs if fleet_or_label(j) == "blacksmith" and b_lo <= T.ts(j["created_at"]) <= b_hi]
    runs_by_job = {j["id"]: r for j, r in jobs}
    S = [o for o in (is_obs(j, runs_by_job[j["id"]]) for j in S_jobs) if o]
    B = [o for o in (is_obs(j, runs_by_job[j["id"]]) for j in B_jobs) if o]
    if events and any(not o.ok and not o.cancelled for o in B):
        # A superci failure is excused when Blacksmith failed the same commit; a record has no commit, so its run's head is read, for the failures alone.
        S = [o if o.ok or o.cancelled else dataclasses.replace(o, sha=gh.api(f"actions/runs/{o.run_id}").get("head_sha") or "") for o in S]
    if events and ceiling is not None:
        # a record cannot say timed_out; a failure that ran to the job's ceiling is one, and counts at the ceiling as GitHub's would
        S = [dataclasses.replace(o, timed_out=True) if not o.ok and not o.cancelled and o.wall is not None and o.wall >= ceiling - TIMEOUT_SLACK_S else o for o in S]
    source = NAME_SOURCES.get(a.job)
    tests = None
    if source is not None:
        candidates = [o for o in S if not o.ok and not o.cancelled and not excused(o, list(S) + list(B))]
        tests = gather_tests(gh, T.Cache(a.cache), source, a.job, candidates, {j["id"]: j for j in S_jobs}, events, listed, (b_lo, b_hi), a.max_test_reads)
    unexplained = []
    finished = [j for j in S_jobs if j.get("completed_at") and (j.get("run_attempt") or 1) == 1 and j.get("conclusion") in OUTCOMES]  # a rerun's created_at is its first attempt's: no timeline
    if events:
        finished = [timeline_job(j) for j in finished if j.get("started_at")]
    if finished:
        ev = T.gather(a.profile, finished, before=3 * 3600, legacy=a.legacy)
        unexplained = [t.unexplained for t in (T.build(j, ev) for j in finished) if t.wall is not None]
    print(f"gate job {a.job}  workflow {a.workflow}  ({len(listed)} runs listed, jobs read from {len(s_runs)} superci-window and {len(b_runs)} baseline runs, {gh.calls} GitHub calls)")
    print(f"  superci: " + (f"{len(S_jobs)} jobs from the listener's job records ({'no GitHub call' if not dispatched else f'GitHub read for the created_at of {dispatched} dispatched jobs'}), on branches {a.branch_glob}" if events
                            else f"runner group {a.superci_group}, runs read whole from branches {a.branch_glob}") + f", created {T.iso(since)} to {T.iso(now)}")
    print(f"  blacksmith: created {T.iso(b_lo)} to {T.iso(b_hi)}; ceiling " + (f"{ceiling:g} s ({LIMITS_PATH})" if ceiling is not None else f"none (no {a.workflow} / {a.job} row)"))
    for line in floor_lines:
        print(f"  {line}")
    if tests is None:
        print(f"  failing tests: no test-name source for {a.job} (only {', '.join(NAME_SOURCES)} have one): the failures row counts a superci failed job as one failure, as before")
    else:
        print(f"  failing tests: {source.what}, {tests.logs} log read{'' if tests.logs == 1 else 's'} from GitHub this check (the rest cached); a superci failure counts when a failing test is in no failed "
              f"Blacksmith main job of the baseline window ({tests.main_runs} main run{'' if tests.main_runs == 1 else 's'} ended red, {tests.main_failed} failed job{'' if tests.main_failed == 1 else 's'} read, {len(tests.red)} test{'' if len(tests.red) == 1 else 's'} red there)")
    print(f"  rerun attempts: {sum(1 for j in S_jobs + B_jobs if (j.get('run_attempt') or 1) > 1)} counted for failures and wall time, not for queue wait (a rerun keeps its first attempt's created_at, so its queue wait is not real)")
    cancelled = cancelled_by_fleet([j for j in jobs_all if (fleet_or_label(j) == "superci" and T.ts(j["created_at"]) >= since) or (fleet_or_label(j) == "blacksmith" and b_lo <= T.ts(j["created_at"]) <= b_hi)])
    print("  cancelled (no outcome; counted in wall and queue wait as lower bounds, never as runs or failures): " + ", ".join(f"{f} {cancelled.get(f, (0, 0))[0]} run{'' if cancelled.get(f, (0, 0))[0] == 1 else 's'} ({cancelled.get(f, (0, 0))[1]} job{'' if cancelled.get(f, (0, 0))[1] == 1 else 's'})" for f in ("superci", "blacksmith")))
    print("  quantiles are interpolated (wall over successful runs and timed-out ones at the ceiling, queue wait over every first attempt):")
    show_spread("superci", S, ceiling)
    show_spread("blacksmith", B, ceiling)
    print("  spend per job (not judged; AWS's from the listener's records, Blacksmith's at its list price):")
    for line in cost_lines(recs if events else [], B_jobs) or ["no superci records or Blacksmith jobs to price"]:
        print(f"  {line}")
    try:
        print("  " + window_cost_line(window_spend(a.profile, since, now), (now - since) / 3600, job_records(a.profile, since, now) if events else []))
    except Exception as e:  # the spend is information: a CloudWatch failure never stops a check
        print(f"  cost of the window: not read ({type(e).__name__})")
    print()
    rows = evaluate_job(S, B, ceiling, unexplained, tests=tests) + dispatched_row(dispatched, unmatched) + unread(gh, a.allow_unread)
    code = print_rows("criteria", rows, f"gate 1 for {a.job}")
    if not a.canary:
        return code
    alarms = alarm_states(a.profile, now, now)
    sets = job_sets(S_jobs, B_jobs)
    reasons = canary_verdict(rows, alarms, sets)
    paging = sorted(n for n, st in alarms.items() if st == "ALARM" and n not in rollback_alarms(alarms, sets))
    print(f"\njudged on: {judged_on(alarms, sets)}")
    print(f"canary {a.job}: " + (f"ROLL BACK ({'; '.join(reasons)})" if reasons else "KEEP") + (f"   [paging, no rollback: {paging}]" if paging else ""))
    return ROLLBACK if reasons else KEEP


def collect_scale(a):
    now = time.time()
    lo, hi = T.ts(a.since), T.ts(a.until)
    gh = T.GitHub(a.repo, T.Cache(a.cache))
    if resolve_source(a, lo - 60, now) == "events":
        # the replay's jobs are the listener's own records: no run is listed and no job is read from GitHub
        # a job queued up to 5 minutes after `hi` belongs to a run created before it, as on GitHub's path; records are written when jobs
        # finish, so the read runs 2 hours past `hi`, longer than any replayed job
        recs = [e for e in job_records(a.profile, lo - 60, hi + 7200) if record_workflow(e) == a.workflow and fnmatch.fnmatch(record_branch(e), a.branch_glob)
                and lo - 60 <= (T.ts(e.get("queue_time") or e.get("finish_time")) or 0) <= hi + 300]
        jobs, dispatched, unmatched = with_github_created(gh, recs, a.superci_group)
    else:
        dispatched, unmatched = 0, []
        # a workflow that lives only on the load branches has no file name GitHub resolves, so every run is listed and matched by path
        runs = [r for r in runs_of(gh, None, lo - 60, hi) if os.path.basename(r.get("path") or "") == a.workflow and fnmatch.fnmatch(r.get("head_branch") or "", a.branch_glob)]
        jobs = [j for js in jobs_of(gh, runs).values() for j in js if j.get("conclusion") != "skipped"]
    started = [j for j in jobs if j.get("started_at") and j.get("runner_group_name") == a.superci_group]
    waits = [T.ts(j["started_at"]) - T.ts(j["created_at"]) for j in started]
    stranded = [(j["name"], j.get("conclusion") or j.get("status")) for j in jobs if j.get("conclusion") != "success"]
    with open(a.trace) as fh:
        replayed = len(json.load(fh)["jobs"])
    tail = 300
    ev = T.listener_events(a.profile, lo - tail, hi + tail)
    lost = T.insights(a.profile, CW_REGION, T.LISTENER_GROUP, 'fields @message | filter msg = "job lost with its machine" | stats count(*) as n', lo - tail, hi + tail)
    lost_n = int(lost[0]["n"]) if lost else 0
    in_window = lambda e: lo - tail <= e["t"] <= hi + tail
    issued = collections.Counter(e["inst"] for e in ev if e["ev"] == "jit.issued" and in_window(e) and e.get("inst"))
    taken = collections.Counter(e["inst"] for e in ev if e["ev"] == "jit.claim" and e.get("result") == "taken" and in_window(e))
    refused = sum(1 for e in ev if e["ev"] == "jit.claim" and e.get("result") == "refused" and in_window(e))
    launched = ["fast" if (e.get("step") or "").startswith("fast-") else e.get("lifecycle") or "on-demand"
                for e in ev if e["ev"] == "machine.launched" and lo <= e["t"] <= hi]
    grants = [e for e in ev if e["ev"] == "jit.granted" and in_window(e)]
    # a record is written when its job ends, so the read runs 2 hours past the window
    handed = handed_machines(job_records(a.profile, lo - tail, hi + 7200), lo - tail, hi + tail)
    due = hi + ORPHAN_WAIT_S
    return Scale(replayed=replayed, completed=sum(1 for j in jobs if j.get("conclusion") == "success"), stranded=stranded, lost=lost_n, waits=waits,
                 window_use=metric(a.profile, "RegistrationWindowUse", lo, hi + tail), org_known=[v for _, v in metric(a.profile, "OrgRateKnown", lo, hi + tail, "Minimum")],
                 claims=claim_objects(a.profile, lo - tail, hi + tail), issued=issued, claim_taken=taken, claim_refused=refused, launched=launched,
                 orphans=[p for p in metric(a.profile, "Orphans", due, due + 120) if p[0] <= now], orphans_due=due, lo=lo, hi=hi,
                 alarms=alarm_states(a.profile, hi, now),
                 queuewait_samples=sum(v for _, v in metric(a.profile, "QueueWait", lo, hi + tail, "SampleCount")),
                 waited_asks=len(grants), max_wait_s=max((e.get("waited_ms") or 0 for e in grants), default=0) / 1000,
                 dispatched=dispatched, dispatched_unmatched=unmatched, dispatched_machines=handed), gh


def cmd_scale(a):
    s, gh = collect_scale(a)
    print(f"gate scale  {a.since} to {a.until}  workflow {a.workflow} branches {a.branch_glob}  ({gh.calls} GitHub calls)")
    print(f"  registration token waits (the budget guard, never hidden): {s.waited_asks} grants, longest {s.max_wait_s:.1f} s")
    print()
    return print_rows("criteria", evaluate_scale(s) + unread(gh, a.allow_unread), "gate 2")


def cmd_replay_trace(a):
    gh = T.GitHub(a.repo, T.Cache(a.cache))
    lo = T.ts(a.hour)
    runs = runs_of(gh, None, lo, lo + 3600 * a.hours)
    jobs = [j for js in jobs_of(gh, runs).values() for j in js]
    trace = build_trace(jobs, lo, a.hours, a.bucket, {"repo": a.repo, "runs": len(runs)})
    out = a.out or f"trace-{T.iso(lo)}.json"
    with open(out, "w") as fh:
        json.dump(trace, fh)
    per = collections.Counter(l["label"] for l in trace["jobs"])
    print(f"{out}: {len(trace['jobs'])} jobs from {len(runs)} runs created {T.iso(lo)} + {a.hours} h ({gh.calls} GitHub calls)")
    print(f"  by label: {dict(per)}")
    print(f"  not replayed: {trace['dropped']}")
    print(f"  drive it: pull/bench/replay.sh BRANCH {out}")
    return 0


def build_parser():
    p = argparse.ArgumentParser(prog="gate", description=__doc__.split("\n")[0])
    p.add_argument("--profile", default=os.environ.get("AWS_PROFILE", "ci-runners"), help="AWS profile of the superci account")
    p.add_argument("--repo", default=REPO)
    p.add_argument("--cache", default=os.path.expanduser("~/.cache/superci-timeline"), help="directory of cached GitHub answers")
    p.add_argument("--legacy", action="store_true", help="rebuild launches and registrations from CloudTrail for a window before the listener's tl events")
    p.add_argument("--personal-token", action="store_true", help="read GitHub on the shared personal token's budget when the superci App's token cannot be had (default: refuse)")
    p.add_argument("--allow-unread", type=int, default=0, help="runs whose jobs GitHub will not list (a persistent 5xx) that the verdict tolerates")
    sub = p.add_subparsers(dest="cmd", required=True)
    j = sub.add_parser("job", help="gate 1: speed parity of one job against Blacksmith")
    j.add_argument("job")
    j.add_argument("--workflow", default="lint-test-backend.yml")
    j.add_argument("--since", help="start of the superci window (default: the last 24 h)")
    j.add_argument("--branch-glob", required=True, help="read whole the runs since --since from branches matching this glob ('main' for a canary on main, 'ci/superci-load-pull-*' for load branches); "
                   "a job counts as superci's by its own runner group, so this bounds the GitHub calls and nothing else. Required: with no glob every run would be read whole and --max-runs would bound nothing")
    j.add_argument("--baseline-hours", type=float, default=BASELINE_HOURS, help="Blacksmith's window, ending now")
    j.add_argument("--baseline-until", help="end Blacksmith's window here instead of now")
    j.add_argument("--max-runs", type=int, default=None, help=f"read the baseline's jobs from at most this many runs, spread evenly (GitHub's rate limit is 5,000 an hour; default {EVENTS_BASELINE_RUNS} with the listener's job records, 300 reading superci's side from GitHub); the superci window is always read whole")
    j.add_argument("--superci-source", choices=("auto", "events", "github"), default="auto", help="where superci's jobs are read from: the listener's job.record events (no GitHub call for superci's side), "
                   "GitHub's REST API, or auto (default): events when the window starts after the first record, GitHub before")
    j.add_argument("--max-test-reads", type=int, default=MAX_TEST_READS, help="job logs one check may read to name failing tests (unit-tests, integration); a failure past it counts as unread, a Blacksmith main failure past it excludes nothing")
    j.add_argument("--limits", help="a ci-limits.tsv to read instead of origin/main's")
    j.add_argument("--superci-group", default=T.SUPERCI_RUNNER_GROUP)
    j.add_argument("--canary", action="store_true", help=f"judge a live canary: exit {ROLLBACK} to roll it back (a FAIL of {', '.join(ROLLBACK_ROWS)}, or {', '.join(ROLLBACK_ALARMS)}, or the wait or stall alarm of a set the job ran on, in ALARM), else {KEEP}")
    j.set_defaults(fn=cmd_job)
    s = sub.add_parser("scale", help="gate 2: the plane over a replay window")
    s.add_argument("--from", dest="since", required=True)
    s.add_argument("--to", dest="until", required=True)
    s.add_argument("--workflow", default="superci-load.yml", help="the replay's workflow file")
    s.add_argument("--branch-glob", default="ci/superci-load-pull-*")
    s.add_argument("--trace", required=True, help="the trace the replay drove: its job count is what was replayed, so a push that never created a run is a missing job")
    s.add_argument("--superci-group", default=T.SUPERCI_RUNNER_GROUP)
    s.add_argument("--superci-source", choices=("auto", "events", "github"), default="auto", help="where the replay's jobs are read from: the listener's job.record events (no GitHub call), GitHub's REST API, or auto (default): events when the window starts after the first record")
    s.set_defaults(fn=cmd_scale)
    r = sub.add_parser("replay-trace", help="write a real hour's arrivals for pull/bench/replay.sh")
    r.add_argument("--hour", required=True, help="start of the window, 2026-11-25T14:00:00Z")
    r.add_argument("--hours", type=int, default=1)
    r.add_argument("--bucket", type=int, default=60, help="seconds of arrivals one push of the replay carries")
    r.add_argument("--out")
    r.set_defaults(fn=cmd_replay_trace)
    return p


@T.apptoken.refuse_as_exit
def main(argv=None):
    a = build_parser().parse_args(argv)
    T.AUTH.personal = a.personal_token
    # `job` and `replay-trace` always read GitHub, so a missing App token stops them before any other work; `scale` reads it only
    # for a window the listener's records do not cover, and then refuses at its first call.
    if a.personal_token or a.cmd != "scale":
        T.AUTH.require()
    return a.fn(a)


if __name__ == "__main__":
    sys.exit(main())
