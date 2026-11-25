#!/usr/bin/env python3
"""Measures a load test against its pass bar: every run of BRANCH created in [SINCE, UNTIL] in megpt-mono, the
machines EC2 lists for the sets (it lists ended ones for about an hour), and the listener's log.

  measure.py BRANCH SINCE UNTIL [LISTENER_LOG.jsonl]      (times as 2026-11-25T10:00:00Z)

Queue wait is a job's started_at minus its created_at, as GitHub reports them (whole seconds).
"""
import json
import statistics
import subprocess
import sys
from collections import Counter
from datetime import datetime, timedelta


def sh(*args):
    return subprocess.run(args, check=True, capture_output=True, text=True).stdout


def ts(s):
    return datetime.fromisoformat(s.replace("Z", "+00:00")) if s else None


def logtime(s):
    """A Go log time (nanoseconds, trailing zeros trimmed) to a datetime."""
    whole, _, frac = s.rstrip("Z").partition(".")
    return datetime.fromisoformat(f"{whole}.{(frac + '000000')[:6]}+00:00")


def pct(xs, p):
    xs = sorted(xs)
    return xs[min(len(xs) - 1, max(0, round(p / 100 * len(xs) + 0.5) - 1))] if xs else None


branch, since, until = sys.argv[1], ts(sys.argv[2]), ts(sys.argv[3])
runs = []
for page in range(1, 20):
    got = json.loads(sh("gh", "api", f"repos/Authentic-Intelligence/megpt-mono/actions/runs?branch={branch}&per_page=100&page={page}"))
    runs += [r for r in got["workflow_runs"] if since <= ts(r["created_at"]) <= until]
    if len(got["workflow_runs"]) < 100:
        break
jobs = []
for r in runs:
    for page in range(1, 10):
        got = json.loads(sh("gh", "api", f"repos/Authentic-Intelligence/megpt-mono/actions/runs/{r['id']}/jobs?per_page=100&page={page}&filter=latest"))
        jobs += got["jobs"]
        if len(got["jobs"]) < 100:
            break
ran = [j for j in jobs if j["conclusion"] != "skipped"]
conclusions = Counter(j["conclusion"] or j["status"] for j in ran)
waits = [(ts(j["started_at"]) - ts(j["created_at"])).total_seconds() for j in ran if j["started_at"] and j["runner_name"]]
print(f"{len(runs)} runs ({min(r['id'] for r in runs)}..{max(r['id'] for r in runs)}), {len(ran)} jobs: {dict(conclusions)}")
print(f"queue wait s: p50 {pct(waits, 50)}  p90 {pct(waits, 90)}  p99 {pct(waits, 99)}  max {max(waits) if waits else None}  mean {statistics.mean(waits) if waits else 0:.1f}")
by_label = {}
for j in ran:
    if j["started_at"] and j["runner_name"]:
        by_label.setdefault(j["labels"][0] if j["labels"] else "?", []).append((ts(j["started_at"]) - ts(j["created_at"])).total_seconds())
for label, w in sorted(by_label.items()):
    print(f"  {label:16} {len(w):5} jobs  p50 {pct(w, 50)}  p90 {pct(w, 90)}  max {max(w)}")
burst = [j for j in ran if j["name"].startswith("burst")]
if burst:
    w = [(ts(j["started_at"]) - ts(j["created_at"])).total_seconds() for j in burst if j["started_at"]]
    print(f"  burst: {len(burst)} jobs  p50 {pct(w, 50)}  p90 {pct(w, 90)}  max {max(w) if w else None}")
print(f"jobs per runner name: max {max(Counter(j['runner_name'] for j in ran if j['runner_name']).values())} (a JIT runner takes one job)")
first = min(ts(j["created_at"]) for j in ran)
last = max(ts(j["completed_at"]) for j in ran if j["completed_at"])
print(f"first job created {first.isoformat()}  last completed {last.isoformat()}")

machines = {}
for region in ("us-east-1", "us-east-2"):
    out = json.loads(sh("aws", "--profile", "ci-runners", "ec2", "describe-instances", "--region", region,
                        "--filters", "Name=tag:purpose,Values=superci-pull", "Name=tag-key,Values=superci-pull-set",
                        "--query", "Reservations[].Instances[].{id:InstanceId,life:InstanceLifecycle,type:InstanceType,az:Placement.AvailabilityZone,state:State.Name,launch:LaunchTime}",
                        "--output", "json"))
    for m in out:
        if first - timedelta(minutes=10) <= ts(m["launch"]) <= last:
            machines[m["id"]] = m
life = Counter(m["life"] or "on-demand" for m in machines.values())
print(f"machines launched in the test: {len(machines)} {dict(life)}  on-demand {100 * life.get('on-demand', 0) / max(1, len(machines)):.1f}%")
print("  by zone:", dict(Counter(m["az"] for m in machines.values())))
print("  types:", len(Counter(m["type"] for m in machines.values())), dict(Counter(m["type"] for m in machines.values()).most_common(6)))

if len(sys.argv) > 4:
    made, refused, failed, notices, stuck, lost, rates = 0, 0, 0, 0, 0, 0, []
    for line in open(sys.argv[4]):
        try:
            e = json.loads(line)
        except ValueError:
            continue
        t = logtime(e["time"]) if e.get("time") else None
        if not t or not (first - timedelta(minutes=30) <= t <= last + timedelta(minutes=15)):
            continue
        m = e.get("msg")
        if m == "register":
            if e.get("refused"):
                refused += 1
            elif e.get("err"):
                failed += 1
            elif first <= t <= last:
                made += 1
        elif m == "spot notice":
            notices += 1
        elif m == "stuck":
            stuck += 1
        elif m == "job lost with its machine":
            lost += 1
        elif m == "budget" and e.get("known"):
            rates.append((t, e["org_rate"], e["others_rate"], e["ours_rate"], e["spare_rate"]))
    print(f"registrations in the test: {made} made, {refused} refused, {failed} failed otherwise; spot notices {notices}; stuck ended {stuck}; jobs lost {lost}")
    before = [r for r in rates if r[0] < first]
    during = [r for r in rates if first <= r[0] <= last]
    for name, rs in (("before", before), ("during", during)):
        if rs:
            print(f"org registration rate {name}: org {statistics.mean(r[1] for r in rs):.2f}/s (max {max(r[1] for r in rs):.2f}), "
                  f"others {statistics.mean(r[2] for r in rs):.2f}/s, ours {statistics.mean(r[3] for r in rs):.2f}/s, spare {statistics.mean(r[4] for r in rs):.2f}/s")
