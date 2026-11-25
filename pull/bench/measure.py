#!/usr/bin/env python3
"""Measures one load-test run against the pass bar, from GitHub (the run's jobs), EC2 (the set's instances, which
EC2 lists for about an hour after they end) and the listener's log.

  measure.py RUN_ID [LISTENER_LOG.jsonl]

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


def pct(xs, p):
    xs = sorted(xs)
    return xs[min(len(xs) - 1, max(0, round(p / 100 * len(xs) + 0.5) - 1))] if xs else None


run = sys.argv[1]
jobs = []
for page in range(1, 30):
    got = json.loads(sh("gh", "api", f"repos/Authentic-Intelligence/megpt-mono/actions/runs/{run}/jobs?per_page=100&page={page}&filter=latest"))
    jobs += got["jobs"]
    if len(got["jobs"]) < 100:
        break

conclusions = Counter(j["conclusion"] or j["status"] for j in jobs)
waits = [(ts(j["started_at"]) - ts(j["created_at"])).total_seconds() for j in jobs if j["started_at"] and j["runner_name"]]
created = min(ts(j["created_at"]) for j in jobs)
runners = Counter(j["runner_name"] for j in jobs if j["runner_name"])
print(f"run {run}: {len(jobs)} jobs, {dict(conclusions)}")
print(f"queue wait s: p50 {pct(waits, 50)}  p90 {pct(waits, 90)}  p99 {pct(waits, 99)}  max {max(waits) if waits else None}  mean {statistics.mean(waits) if waits else 0:.1f}")
print(f"jobs per runner name: max {max(runners.values()) if runners else 0} (a JIT runner takes one job)")
starts = Counter(int((ts(j["started_at"]) - created).total_seconds() // 10) * 10 for j in jobs if j["started_at"])
print("jobs started, by 10 s after the first job was created:", dict(sorted(starts.items())))
last_done = max(ts(j["completed_at"]) for j in jobs if j["completed_at"])
print(f"first created {created.isoformat()}  last completed {last_done.isoformat()}")

machines = {}
for region in ("us-east-1", "us-east-2"):
    out = json.loads(sh("aws", "--profile", "ci-runners", "ec2", "describe-instances", "--region", region,
                        "--filters", "Name=tag:purpose,Values=superci-pull", "Name=tag:superci-pull-set,Values=superci-pull-4cpu",
                        "--query", "Reservations[].Instances[].{id:InstanceId,life:InstanceLifecycle,type:InstanceType,az:Placement.AvailabilityZone,state:State.Name,launch:LaunchTime,reason:StateTransitionReason}",
                        "--output", "json"))
    for m in out:
        launched = ts(m["launch"])
        if created - timedelta(minutes=10) <= launched <= last_done:
            machines[m["id"]] = m
life = Counter(m["life"] or "on-demand" for m in machines.values())
od = life.get("on-demand", 0)
print(f"machines launched for this run: {len(machines)}  {dict(life)}  on-demand {100 * od / max(1, len(machines)):.1f}%")
print("  by zone:", dict(Counter(m["az"] for m in machines.values())))
print("  by type:", dict(Counter(m["type"] for m in machines.values()).most_common(8)))
print("  still running:", sum(1 for m in machines.values() if m["state"] in ("pending", "running")))

# Cost: each machine's seconds (a minute at least, as EC2 bills Linux) at its pool's spot price when it started, or an
# on-demand estimate, plus its gp3 volume (30 GB, 250 MB/s).
EBS_PER_HOUR = (30 * 0.08 + 125 * 0.04) / 730
prices = {}
for (region, az, kind) in {(m["az"][:-1], m["az"], m["type"]) for m in machines.values()}:
    out = json.loads(sh("aws", "--profile", "ci-runners", "ec2", "describe-spot-price-history", "--region", region,
                        "--availability-zone", az, "--instance-types", kind, "--product-descriptions", "Linux/UNIX",
                        "--start-time", created.isoformat(), "--end-time", created.isoformat(), "--output", "json"))
    prices[(az, kind)] = float(out["SpotPriceHistory"][0]["SpotPrice"]) if out["SpotPriceHistory"] else 0.0
cost, seconds = 0.0, 0.0
for m in machines.values():
    end = datetime.now(created.tzinfo)
    if "(" in (m["reason"] or ""):
        end = datetime.strptime(m["reason"].split("(")[1].split(")")[0], "%Y-%m-%d %H:%M:%S %Z").replace(tzinfo=created.tzinfo)
    secs = max(60.0, (end - ts(m["launch"])).total_seconds())
    rate = prices[(m["az"], m["type"])] if (m["life"] or "on-demand") == "spot" else 0.20
    seconds += secs
    cost += secs / 3600 * (rate + EBS_PER_HOUR)
print(f"machine time {seconds / 3600:.1f} h, cost ${cost:.2f} (spot price at the run's start per pool, on-demand at $0.20/h, gp3 included)")

if len(sys.argv) > 2:
    regs, errors, notices, stuck, fleets = [], 0, 0, 0, Counter()
    for line in open(sys.argv[2]):
        try:
            e = json.loads(line)
        except ValueError:
            continue
        t = ts(e.get("time", "").replace("Z", "+00:00")[:26] + "+00:00") if e.get("time") else None
        if e.get("msg") == "register":
            if e.get("err"):
                errors += 1
            else:
                regs.append(t)
        elif e.get("msg") == "spot notice":
            notices += 1
        elif e.get("msg") == "stuck":
            stuck += 1
        elif e.get("msg") == "fleet":
            fleets[(e["region"], e["step"])] += e["got"]
    regs.sort()
    window, j = 0, 0
    for i, t in enumerate(regs):
        while regs[j] < t - timedelta(minutes=5):
            j += 1
        window = max(window, i - j + 1)
    print(f"registrations: {len(regs)} ok, {errors} refused; most in any 5 min: {window} (GitHub's cap 1,500)")
    print(f"spot notices: {notices}; stuck machines ended: {stuck}; fleet launches by (region, step): {dict(fleets)}")
