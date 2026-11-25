"""A weekday of superci arrivals per size, and each pool policy's queue wait and idle cost over it.

Arrivals: pushes arrive as a Poisson process per hour; each brings one push's jobs of the size with their measured
offsets (jobs of one head branch whose run started within 10 s of each other, from the Nov 24 02:00-02:15Z peak, all
workflows). The hour's rate is Blacksmith's jobs of that size in that hour of Nov 23 (a Monday, the last full weekday
of the export). Waits and idle come from sim.simulate.

  day.py DATA HOURLY [mode] [CONFIG]   also writes DATA/demand/day-<size>.json, the weekday the other tools read
"""
import collections, glob, json, math, random, sys
from datetime import datetime

sys.path.insert(0, __import__("os").path.dirname(__import__("os").path.abspath(__file__)))
from sim import Policy, simulate, quantile, load_leads, START
from demand import load

SIZES = (2, 4, 8, 16, 32)
PRICE = {2: 0.037, 4: 0.074, 8: 0.143, 16: 0.261, 32: 0.498}  # idle machine-hour: spot p25 + gp3 + IPv4
SLO = (5.0, 10.0)


def batches(path):
    jobs = load(path)
    runs = collections.defaultdict(list)
    for j in jobs:
        runs[j["run"]].append(j)
    branch = {}
    for l in open(path):
        r = json.loads(l)
        branch[(r["run_id"], r["run_attempt"])] = r["head_branch"]
    first = sorted((min(j["c"] for j in js), k) for k, js in runs.items())
    pushes, open_ = [], {}
    for c, k in first:
        b = branch.get(k)
        p = open_.get(b)
        if p is None or c - p["c"] > 10:
            p = {"c": c, "jobs": []}
            pushes.append(p)
            open_[b] = p
        p["jobs"] += runs[k]
    out = {s: [] for s in SIZES}
    for p in pushes:
        for s in SIZES:
            offs = sorted(j["c"] - p["c"] for j in p["jobs"] if j["size"] == s)
            if offs:
                out[s].append(offs)
    return out, len(pushes)


def hourly(path_glob, day):
    rate = {s: [0] * 24 for s in SIZES}
    for f in glob.glob(path_glob):
        d = json.load(open(f))
        if d["window"]["start"][:10] != day:
            continue
        h = int(d["window"]["start"][11:13])
        for r in d["breakdowns"].get("runner_type", []):
            for s in SIZES:
                if r["runner_type"] in (f"blacksmith-{s}vcpu-ubuntu-2404", f"blacksmith-{s}vcpu-ubuntu-2404;scheduling=flex"):
                    rate[s][h] += r["jobs"]
    return rate


def trace(shapes, jobs_per_hour, rng, hours=range(24)):
    mean = sum(len(b) for b in shapes) / len(shapes)
    out = []
    for h in hours:
        lam = jobs_per_hour[h] / mean / 3600
        t = h * 3600.0
        while lam > 0:
            t += rng.expovariate(lam)
            if t >= (h + 1) * 3600:
                break
            out += [t + o for o in rng.choice(shapes)]
    return sorted(out)


def run(arr, pool_of_hour, leads, seed):
    return simulate(arr, Policy(lambda now, recent, span: pool_of_hour(int(now // 3600) % 24, len(recent) / span)), leads, random.Random(seed))


def per_hour(arr, waits):
    by = collections.defaultdict(list)
    for a, w in zip(sorted(arr), waits):
        by[int(a // 3600) % 24].append(w)
    return by


def meets(ws):
    return quantile(ws, 0.5) <= SLO[0] and quantile(ws, 0.9) <= SLO[1]


def static_search(arr, leads, seed, hi=400):
    """The smallest pool per hour that meets the SLO in that hour, hour by hour (a pool's effect is local in time)."""
    pools = [0] * 24
    for h in range(24):
        sub = [a for a in arr if h * 3600 - 300 <= a < (h + 1) * 3600]
        if not [a for a in sub if a >= h * 3600]:
            continue
        lo_, hi_ = 0, hi
        while lo_ < hi_:
            mid = (lo_ + hi_) // 2
            w, _, _ = run(sub, lambda hh, r: mid, leads, seed)
            ws = [x for a, x in zip(sub, w) if a >= h * 3600]
            if meets(ws):
                hi_ = mid
            else:
                lo_ = mid + 1
        pools[h] = lo_
    return pools


def report(name, arr, waits, idle, size, launched):
    ok = meets(waits)
    return dict(policy=name, size=size, n=len(waits), p50=round(quantile(waits, 0.5), 1), p90=round(quantile(waits, 0.9), 1),
                warm=round(sum(1 for w in waits if w <= START + 0.01) / len(waits), 3), idle_h=round(idle / 3600, 1),
                usd_day=round(idle / 3600 * PRICE[size], 2), launched=launched, meets=ok)


if __name__ == "__main__":
    S, HOURLY = sys.argv[1], sys.argv[2]
    shapes, npush = batches(f"{S}/demand/win15.jsonl")
    rates = hourly(HOURLY + "/*.json", "2026-11-23")
    leads = load_leads(f"{S}/logs/coldpath.json")
    mode = sys.argv[3] if len(sys.argv) > 3 else "all"
    print(f"pushes {npush}; jobs per push by size:", {s: round(sum(map(len, b)) / max(1, len(b)), 1) for s, b in shapes.items()},
          file=sys.stderr)
    results = []
    for s in SIZES:
        rng = random.Random(s)
        arr = trace(shapes[s], rates[s], rng)
        json.dump(arr, open(f"{S}/demand/day-{s}.json", "w"))
        caps = json.load(open(sys.argv[4]))["pools"][f"superci-{s}cpu"] if len(sys.argv) > 4 else None
        w, idle, n = run(arr, lambda h, r: 1 if s <= 8 else 0, leads, 1)
        results.append(report("today: floor only (arrivals uncounted)", arr, w, idle, s, n))
        if caps:
            w, idle, n = run(arr, lambda h, r: min(caps["cap"][h], max(caps["floor"][h], math.ceil(r * 45 * 1.5))), leads, 1)
            results.append(report("rule fixed: rate x 45 x 1.5, today's caps", arr, w, idle, s, n))
            w, idle, n = run(arr, lambda h, r: max(caps["floor"][h], math.ceil(r * 45 * 1.5)), leads, 1)
            results.append(report("rule fixed: rate x 45 x 1.5, no cap", arr, w, idle, s, n))
        pools = static_search(arr, leads, 1)
        w, idle, n = run(arr, lambda h, r: pools[h], leads, 1)
        results.append(dict(report("static per hour, searched", arr, w, idle, s, n), pools=pools))
        print(json.dumps(results[-1]), file=sys.stderr)
    json.dump(results, open(f"{S}/demand/policies.json", "w"), indent=1)
    for r in results:
        print({k: v for k, v in r.items() if k != "pools"})
