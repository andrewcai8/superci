"""Demand shape of megpt-mono's Linux Blacksmith jobs as superci would see them.

Data shape: arrivals[size] = sorted job creation times (s); bursts = jobs per (run, size, creation second).
Prints per size: jobs, mean rate, per-minute histogram quantiles, burst-size quantiles, lag of jobs after their run's
first job, and the Blacksmith queue wait (started - created) for comparison.
"""
import collections, json, re, sys
from datetime import datetime

def t(s):
    return datetime.fromisoformat(s.replace("Z", "+00:00")).timestamp()

SIZE = re.compile(r"^blacksmith-(\d+)vcpu-ubuntu-2404(;scheduling=flex)?$")

def load(path):
    jobs = []
    for line in open(path):
        j = json.loads(line)
        if not j["started_at"] or not (j["runner_name"] or "").startswith("blacksmith-"):
            continue
        m = SIZE.match((j["labels"] or [""])[0])
        if not m:
            continue
        jobs.append(dict(size=int(m.group(1)), run=(j["run_id"], j["run_attempt"]), c=t(j["created_at"]), s=t(j["started_at"]),
                         d=t(j["completed_at"]) - t(j["started_at"]) if j["completed_at"] else None, wf=j["workflow_name"], name=j["name"]))
    return jobs

def q(xs, p):
    xs = sorted(xs)
    return xs[min(len(xs) - 1, int(p * len(xs)))] if xs else 0

if __name__ == "__main__":
    jobs = load(sys.argv[1])
    t0, t1 = min(j["c"] for j in jobs), max(j["c"] for j in jobs)
    span = t1 - t0 + 1
    first = {}
    for j in jobs:
        first[j["run"]] = min(first.get(j["run"], 1e18), j["c"])
    print(f"jobs {len(jobs)} over {span/60:.1f} min")
    print("size  jobs  /s    perMin p50 p90 max | burst p50 p90 p99 max | per20s p50 p90 p99 | lag>=20s  | bs wait p50 p90")
    for size in sorted({j["size"] for j in jobs}):
        js = [j for j in jobs if j["size"] == size]
        pm = collections.Counter(int((j["c"] - t0) // 60) for j in js)
        pmv = [pm.get(k, 0) for k in range(int(span // 60))]
        b = collections.Counter((j["run"], j["c"]) for j in js)
        p20 = collections.Counter(int((j["c"] - t0) // 20) for j in js)
        p20v = [p20.get(k, 0) for k in range(int(span // 20))]
        lag = sum(1 for j in js if j["c"] - first[j["run"]] >= 20) / len(js)
        w = [j["s"] - j["c"] for j in js]
        print(f"{size:4d} {len(js):5d} {len(js)/span:5.2f}  {q(pmv,.5):6d} {q(pmv,.9):3d} {max(pmv):3d} | {q(list(b.values()),.5):5d} {q(list(b.values()),.9):3d} {q(list(b.values()),.99):3d} {max(b.values()):3d} | {q(p20v,.5):10d} {q(p20v,.9):3d} {q(p20v,.99):3d} | {lag:8.0%} | {q(w,.5):9.0f} {q(w,.9):3.0f}")
    wf = collections.Counter(j["wf"] for j in jobs)
    print("top workflows by jobs:", wf.most_common(8))
