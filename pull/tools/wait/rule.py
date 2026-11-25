"""The shipped rule with hydrated machines: the canary's launch times plus the 19 s hydration adds at the median (38 s
against 19, prehydrate n=24), the pool's launch time their p90, over the modelled weekday: wait and idle per size."""
import json, os, random, sys
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import day, sim
from adaptive import burst_pool
from sim import quantile, START, Policy, simulate

S = sys.argv[1]
cfg = json.load(open(sys.argv[2]))
shift = float(sys.argv[3]) if len(sys.argv) > 3 else 19.0
leads = [l + shift for l in sim.load_leads(f"{S}/logs/coldpath.json")]
ready = quantile(leads, 0.9)
r = cfg["pool"]
allw, tot = [], 0
for s in day.SIZES:
    pool = cfg["pools"][f"superci-{s}cpu"]
    arr = json.load(open(f"{S}/demand/day-{s}.json"))
    f0 = burst_pool(r["quantile"], ready, r["hold_minutes"] * 60)
    fn = lambda now, recent, span, f0=f0, pool=pool: min(pool["cap"][int(now // 3600) % 24], max(pool["floor"][int(now // 3600) % 24], f0(now, recent, span)))
    w, idle, n = simulate(arr, Policy(fn, window=r["window_minutes"] * 60), leads, random.Random(1))
    allw += w
    usd = idle / 3600 * day.PRICE[s]
    tot += usd
    print(f"{s:2d}cpu p50 {quantile(w,.5):4.1f} p90 {quantile(w,.9):5.1f} warm {sum(1 for x in w if x <= START+.01)/len(w):5.1%} idle ${usd:6.1f}/day")
print(f"leads median {quantile(leads,.5):.1f} p90 {ready:.1f}; all p50 {quantile(allw,.5):.1f} p90 {quantile(allw,.9):.1f}; idle ${tot:.0f}/day, ${tot*30:.0f}/30d at Nov 23's volume")
