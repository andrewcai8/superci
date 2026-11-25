"""Upper bound on predictive launch: every job that arrives at least the lead after its push's first job is served by
a machine launched exactly in time (warm, no idle, no pool). The rest go through the searched static pool. Prints
the predictable share per size and the idle $/day with and without it."""
import json, random, sys
sys.path.insert(0, __import__("os").path.dirname(__import__("os").path.abspath(__file__)))
import day
from sim import load_leads

S = sys.argv[1]
leads = load_leads(f"{S}/logs/coldpath.json")
shapes, _ = day.batches(f"{S}/demand/win15.jsonl")
rates = day.hourly(sys.argv[2] + "/*.json", "2026-11-23")
tot0 = tot1 = 0
for s in day.SIZES:
    late = sum(1 for b in shapes[s] for o in b if o >= 21) / sum(len(b) for b in shapes[s])
    early = [[o for o in b if o < 21] for b in shapes[s]]
    early = [b for b in early if b]
    scale = [r * (1 - late) for r in rates[s]]
    arr = day.trace(early, scale, random.Random(s))
    pools = day.static_search(arr, leads, 1)
    w, idle, n = day.run(arr, lambda h, r: pools[h], leads, 1)
    base = json.load(open(f"{S}/demand/policies.json"))
    b0 = [r for r in base if r["size"] == s and r["policy"].startswith("static")][0]["usd_day"]
    usd = idle / 3600 * day.PRICE[s]
    tot0 += b0; tot1 += usd
    print(f"{s:2d}cpu predictable {late:5.1%}  static pool ${b0:6.1f}/day -> ${usd:6.1f}/day with perfect prediction")
print(f"total ${tot0:.0f} -> ${tot1:.0f}/day")
