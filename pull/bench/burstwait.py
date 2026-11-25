#!/usr/bin/env python3
"""Scores a burst.py replay: every job of BRANCH's runs, its queue wait (started_at - created_at, whole seconds, as
GitHub reports them) and each run's slowest job (the most a check of that push waits for a machine), over all jobs and
over those created WARMUP minutes or more after the first; with the listener's log, the pool and idle runners it held
when each job arrived (its last state line before).

  burstwait.py BRANCH WARMUP [LISTENER_LOG.jsonl]
"""
import bisect, json, subprocess, sys
from datetime import datetime


def ts(s):
    return datetime.fromisoformat(s.replace("Z", "+00:00")).timestamp()


def pct(xs, p):
    xs = sorted(xs)
    return xs[min(len(xs) - 1, int(p * len(xs)))] if xs else None


branch, warmup = sys.argv[1], float(sys.argv[2])
runs = json.loads(subprocess.run(["gh", "api", f"repos/Authentic-Intelligence/megpt-mono/actions/runs?branch={branch}&per_page=100"],
                                 capture_output=True, text=True, check=True).stdout)["workflow_runs"]
jobs = []
for r in runs:
    out = subprocess.run(["gh", "api", "--paginate", f"repos/Authentic-Intelligence/megpt-mono/actions/runs/{r['id']}/jobs?per_page=100",
                          "--jq", ".jobs[] | tojson"], capture_output=True, text=True, check=True).stdout
    jobs += [dict(json.loads(l), run=r["id"]) for l in out.splitlines()]
jobs = [j for j in jobs if j["started_at"] and j["runner_name"] and j["conclusion"] != "cancelled"]
t0 = min(ts(j["created_at"]) for j in jobs)
states = []
if len(sys.argv) > 3:
    for line in open(sys.argv[3]):
        e = json.loads(line)
        if e.get("msg") == "state":
            states.append((ts(e["time"][:26] + "Z" if "." in e["time"] else e["time"]), e["pool"], e["idle"], e.get("launch_s", 0)))
    states.sort()
for name, sel in (("all", jobs), (f"after {warmup:g} min", [j for j in jobs if ts(j["created_at"]) - t0 >= warmup * 60])):
    w = [ts(j["started_at"]) - ts(j["created_at"]) for j in sel]
    worst = {}
    for j, x in zip(sel, w):
        worst[j["run"]] = max(worst.get(j["run"], 0), x)
    warm = sum(1 for x in w if x <= 3) / max(1, len(w))
    print(f"{name}: jobs {len(w)} wait p50 {pct(w, .5)} p90 {pct(w, .9)} p99 {pct(w, .99)} max {max(w) if w else None}; "
          f"warm (<= 3 s) {warm:.0%}; runs {len(worst)} slowest-job p50 {pct(list(worst.values()), .5)} p90 {pct(list(worst.values()), .9)}")
if states:
    at = [s[0] for s in states]
    pools = [states[max(0, bisect.bisect_right(at, ts(j["created_at"])) - 1)] for j in jobs]
    print(f"pool at arrival p50 {pct([p[1] for p in pools], .5)} max {max(p[1] for p in pools)}; idle at arrival p50 {pct([p[2] for p in pools], .5)}; "
          f"launch time measured, last {states[-1][3]:.1f} s")
