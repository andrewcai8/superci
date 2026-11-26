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
import json
import math
import os
import re
import subprocess
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
EXTRA_FAILURES = 1
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
ROLLBACK_ALARMS = ("superci-pull-oldest-job-wait", "superci-pull-jobs-stalled", "superci-pull-listener-down", "superci-pull-spend-capped")
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


def evaluate_job(S, B, ceiling, unexplained, min_runs=MIN_RUNS):
    """Gate 1's rows from the superci runs S and Blacksmith's B of one job, the job's ceiling in seconds (None when it has no
    row in ci-limits.tsv) and the unexplained seconds of each superci job. Pure."""
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
    b_rate = sum(1 for o in B_done if not o.ok) / len(B_done) if B_done else 0.0
    allowed = b_rate * len(S_done) + EXTRA_FAILURES
    skipped = sum(1 for o in S_done if not o.ok) - len(counted)
    rows.append(Row("failures (superci)", f"{len(counted)} of {len(S_done)}", f"<= B rate {100 * b_rate:.1f}% x {len(S_done)} + {EXTRA_FAILURES} = {allowed:.1f}",
                    PASS if len(counted) <= allowed else FAIL, f"{skipped} more also failed on Blacksmith at the same commit" if skipped else ""))
    p50u = T.q(unexplained, 50)
    rows.append(Row("unexplained p50 (timeline)", fmt_s(p50u), f"< {MAX_UNEXPLAINED_S:g} s", PASS if p50u is not None and p50u < MAX_UNEXPLAINED_S else FAIL,
                    "" if unexplained else "no superci timelines"))
    return rows


def canary_verdict(rows, alarms):
    """The reasons a live canary must go back to Blacksmith, from gate 1's rows and the alarm states; empty keeps it. A p50 row
    counts once n passes and a p90 row once there are CANARY_P90_RUNS runs, each only as FAIL (NOISE asks for runs, not a
    rollback), and never when Blacksmith had no runs to compare with; the failures row counts from the first run. Of the
    alarms only ROLLBACK_ALARMS count: each is a superci job not served, so it sends back every job listed."""
    by = {r.name: r for r in rows}
    n = by.get("n (superci runs)")
    runs = int(n.value) if n and n.value.isdigit() else 0
    need = {"wall p50": MIN_RUNS, "queue wait p50": MIN_RUNS, "wall p90": CANARY_P90_RUNS, "queue wait p90": CANARY_P90_RUNS, "failures (superci)": 0}
    harmed = [r for r in (by.get(k) for k in ROLLBACK_ROWS) if r and r.status == FAIL and runs >= need[r.name] and not r.note.startswith("no Blacksmith runs")]
    return ([f"{r.name}: {r.value} vs {r.threshold}" for r in harmed]
            + [f"{a} in ALARM" for a in ROLLBACK_ALARMS if alarms.get(a) == "ALARM"])


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
    twice = sorted(i for i, n in s.issued.items() if n > 1) + sorted(i for i, n in s.claim_taken.items() if n > 1 and s.issued.get(i, 0) <= 1)
    unregistered, unclaimed = sorted(s.claims - set(s.issued)), sorted(set(s.issued) - s.claims)
    one = not twice and not unregistered and not unclaimed and bool(s.claims)
    rows.append(Row("one runner per machine", f"{len(s.claims)} claims, {len(s.issued)} registrations, {len(twice)} claimed twice",
                    "claims = registrations, 0 twice", PASS if one else FAIL,
                    f"claimed not registered {unregistered[:3]}, registered not claimed {unclaimed[:3]}, twice {twice[:3]}; fence refused {s.claim_refused}" if not one else
                    f"the fence refused {s.claim_refused} duplicate grant(s)" if s.claim_refused else ""))
    if s.orphans:
        peak = max(v for _, v in s.orphans)
        rows.append(Row("orphans 10 min after", f"max {peak:.0f}", "0", PASS if peak == 0 else FAIL))
    else:
        rows.append(Row("orphans 10 min after", "-", "0", FAIL, f"no Orphans datapoint at {T.iso(s.orphans_due)}+: run it after then"))
    od = sum(1 for l in s.launched if l not in ("spot", "fast"))
    fast = sum(1 for l in s.launched if l == "fast")
    share = od / len(s.launched) if s.launched else None
    rows.append(Row("on-demand share", "-" if share is None else f"{100 * share:.1f}% ({od} of {len(s.launched)})", f"<= {100 * ON_DEMAND_SHARE:.0f}%",
                    PASS if share is not None and share <= ON_DEMAND_SHARE else FAIL,
                    "no machine.launched events" if not s.launched else f"{fast} of an on_demand_first type, outside the share" if fast else ""))
    firing = sorted(n for n, st in s.alarms.items() if st == "ALARM")
    rows.append(Row("alarms at the end", f"{len(firing)} in ALARM of {len(s.alarms)}", "0 in ALARM", PASS if not firing and s.alarms else FAIL,
                    f"firing: {firing}" if firing else "" if s.alarms else "no alarms listed"))
    pub = s.queuewait_samples or 0
    rows.append(Row("QueueWait published", f"{pub:.0f} samples", "> 0", PASS if pub > 0 else FAIL, f"{len(s.waits)} jobs started in the replay"))
    return rows


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
    p = subprocess.run(["gh", "api", "-H", "Accept: application/vnd.github.raw", f"repos/{REPO}/contents/{path}?ref={ref}"], capture_output=True, text=True)
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


def jobs_of(gh, runs, workers=12, attempts="latest"):
    """{run id: its jobs} for a list of runs. A run GitHub will not list (a 5xx that outlasts the retries) is left out and
    recorded in gh.skipped; an exhausted rate limit stops the command, since every later call fails the same way."""
    skipped = getattr(gh, "skipped", None)
    if skipped is None:
        skipped = gh.skipped = []

    def one(r):
        try:
            return gh.run_jobs(r["id"], attempts)
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
    bad = [r for r in rows if r.status != PASS]
    if not bad:
        print(f"\n{verdict_name}: PASS")
        return 0
    noisy = all(r.status == NOISE for r in bad)
    print(f"\n{verdict_name}: {'INCONCLUSIVE, add runs' if noisy else 'FAIL'} ({', '.join(r.name for r in bad)})")
    return 1


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
    limits = parse_limits(open(a.limits).read() if a.limits else gh_raw(LIMITS_PATH))
    ceiling = limits.get((a.workflow, a.job))
    listed = runs_of(gh, a.workflow, min(since, b_lo), now)
    # The branch only picks which runs are read whole, to bound GitHub calls; which jobs are superci's is decided by the job's own fleet and group below.
    s_runs = [r for r in listed if T.ts(r["created_at"]) >= since and fnmatch.fnmatch(r.get("head_branch") or "", a.branch_glob)]
    b_runs = [r for r in listed if b_lo <= T.ts(r["created_at"]) <= b_hi and r["id"] not in {x["id"] for x in s_runs}]
    b_runs = sample(b_runs, a.max_runs)
    runs = s_runs + b_runs
    by_run = {r["id"]: r for r in runs}
    jobs = [(j, by_run[rid]) for rid, js in jobs_of(gh, runs, attempts="all").items() for j in js if matches(j["name"], a.job)]
    jobs_all = [j for j, r in jobs]
    S_jobs = [j for j, r in jobs if is_superci(j, a.superci_group) and T.ts(j["created_at"]) >= since]
    B_jobs = [j for j, r in jobs if fleet_or_label(j) == "blacksmith" and b_lo <= T.ts(j["created_at"]) <= b_hi]
    runs_by_job = {j["id"]: r for j, r in jobs}
    S = [o for o in (is_obs(j, runs_by_job[j["id"]]) for j in S_jobs) if o]
    B = [o for o in (is_obs(j, runs_by_job[j["id"]]) for j in B_jobs) if o]
    unexplained = []
    finished = [j for j in S_jobs if j.get("completed_at") and (j.get("run_attempt") or 1) == 1 and j.get("conclusion") in OUTCOMES]  # a rerun's created_at is its first attempt's: no timeline
    if finished:
        ev = T.gather(a.profile, finished, before=3 * 3600, legacy=a.legacy)
        unexplained = [t.unexplained for t in (T.build(j, ev) for j in finished) if t.wall is not None]
    print(f"gate job {a.job}  workflow {a.workflow}  ({len(listed)} runs listed, jobs read from {len(s_runs)} superci-window and {len(b_runs)} baseline runs, {gh.calls} GitHub calls)")
    print(f"  superci: runner group {a.superci_group}, runs read whole from branches {a.branch_glob}, created {T.iso(since)} to {T.iso(now)}")
    print(f"  blacksmith: created {T.iso(b_lo)} to {T.iso(b_hi)}; ceiling " + (f"{ceiling:g} s ({LIMITS_PATH})" if ceiling is not None else f"none (no {a.workflow} / {a.job} row)"))
    print(f"  rerun attempts: {sum(1 for j in S_jobs + B_jobs if (j.get('run_attempt') or 1) > 1)} counted for failures and wall time, not for queue wait (a rerun keeps its first attempt's created_at, so its queue wait is not real)")
    cancelled = cancelled_by_fleet([j for j in jobs_all if (fleet_or_label(j) == "superci" and T.ts(j["created_at"]) >= since) or (fleet_or_label(j) == "blacksmith" and b_lo <= T.ts(j["created_at"]) <= b_hi)])
    print("  cancelled (no outcome; counted in wall and queue wait as lower bounds, never as runs or failures): " + ", ".join(f"{f} {cancelled.get(f, (0, 0))[0]} run{'' if cancelled.get(f, (0, 0))[0] == 1 else 's'} ({cancelled.get(f, (0, 0))[1]} job{'' if cancelled.get(f, (0, 0))[1] == 1 else 's'})" for f in ("superci", "blacksmith")))
    print("  quantiles are interpolated (wall over successful runs and timed-out ones at the ceiling, queue wait over every first attempt):")
    show_spread("superci", S, ceiling)
    show_spread("blacksmith", B, ceiling)
    print()
    rows = evaluate_job(S, B, ceiling, unexplained) + unread(gh, a.allow_unread)
    code = print_rows("criteria", rows, f"gate 1 for {a.job}")
    if not a.canary:
        return code
    alarms = alarm_states(a.profile, now, now)
    reasons = canary_verdict(rows, alarms)
    paging = sorted(n for n, st in alarms.items() if st == "ALARM" and n not in ROLLBACK_ALARMS)
    print(f"\ncanary {a.job}: " + (f"ROLL BACK ({'; '.join(reasons)})" if reasons else "KEEP") + (f"   [paging, no rollback: {paging}]" if paging else ""))
    return ROLLBACK if reasons else KEEP


def collect_scale(a):
    now = time.time()
    lo, hi = T.ts(a.since), T.ts(a.until)
    gh = T.GitHub(a.repo, T.Cache(a.cache))
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
    due = hi + ORPHAN_WAIT_S
    return Scale(replayed=replayed, completed=sum(1 for j in jobs if j.get("conclusion") == "success"), stranded=stranded, lost=lost_n, waits=waits,
                 window_use=metric(a.profile, "RegistrationWindowUse", lo, hi + tail), org_known=[v for _, v in metric(a.profile, "OrgRateKnown", lo, hi + tail, "Minimum")],
                 claims=claim_objects(a.profile, lo - tail, hi + tail), issued=issued, claim_taken=taken, claim_refused=refused, launched=launched,
                 orphans=[p for p in metric(a.profile, "Orphans", due, due + 120) if p[0] <= now], orphans_due=due, lo=lo, hi=hi,
                 alarms=alarm_states(a.profile, hi, now),
                 queuewait_samples=sum(v for _, v in metric(a.profile, "QueueWait", lo, hi + tail, "SampleCount")),
                 waited_asks=len(grants), max_wait_s=max((e.get("waited_ms") or 0 for e in grants), default=0) / 1000), gh


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
    j.add_argument("--max-runs", type=int, default=300, help="read the baseline's jobs from at most this many runs, spread evenly (GitHub's rate limit is 5,000 an hour); the superci window is always read whole")
    j.add_argument("--limits", help="a ci-limits.tsv to read instead of origin/main's")
    j.add_argument("--superci-group", default=T.SUPERCI_RUNNER_GROUP)
    j.add_argument("--canary", action="store_true", help=f"judge a live canary: exit {ROLLBACK} to roll it back (a FAIL of {', '.join(ROLLBACK_ROWS)}, or one of {', '.join(ROLLBACK_ALARMS)} in ALARM), else {KEEP}")
    j.set_defaults(fn=cmd_job)
    s = sub.add_parser("scale", help="gate 2: the plane over a replay window")
    s.add_argument("--from", dest="since", required=True)
    s.add_argument("--to", dest="until", required=True)
    s.add_argument("--workflow", default="superci-load.yml", help="the replay's workflow file")
    s.add_argument("--branch-glob", default="ci/superci-load-pull-*")
    s.add_argument("--trace", required=True, help="the trace the replay drove: its job count is what was replayed, so a push that never created a run is a missing job")
    s.add_argument("--superci-group", default=T.SUPERCI_RUNNER_GROUP)
    s.set_defaults(fn=cmd_scale)
    r = sub.add_parser("replay-trace", help="write a real hour's arrivals for pull/bench/replay.sh")
    r.add_argument("--hour", required=True, help="start of the window, 2026-11-25T14:00:00Z")
    r.add_argument("--hours", type=int, default=1)
    r.add_argument("--bucket", type=int, default=60, help="seconds of arrivals one push of the replay carries")
    r.add_argument("--out")
    r.set_defaults(fn=cmd_replay_trace)
    return p


def main(argv=None):
    a = build_parser().parse_args(argv)
    return a.fn(a)


if __name__ == "__main__":
    sys.exit(main())
