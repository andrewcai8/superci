"""Idle cost of the cheapest static per-hour pool that meets the bar, as the cold path's lead shrinks by delta seconds."""
import json, sys
sys.path.insert(0, __import__("os").path.dirname(__import__("os").path.abspath(__file__)))
import day
from sim import load_leads, simulate, Policy, quantile
import random

S = sys.argv[1]
base = load_leads(f"{S}/logs/coldpath.json")
for delta in (0, 4, 8, 10, 11, 12, 14):
    leads = [max(1.0, l - delta) for l in base]
    tot, rows = 0, []
    for s in day.SIZES:
        arr = json.load(open(f"{S}/demand/day-{s}.json"))
        pools = day.static_search(arr, leads, 1)
        w, idle, n = day.run(arr, lambda h, r: pools[h], leads, 1)
        usd = idle / 3600 * day.PRICE[s]
        tot += usd
        rows.append(f"{s}:{max(pools)}/{usd:.0f}")
    print(f"lead p50 {quantile(leads, .5):5.1f}s  cold wait ~{quantile(leads, .5) + 3.5:4.1f}s  idle ${tot:6.0f}/day  ${tot*30:7.0f}/30d  (size:max pool/$day) {' '.join(rows)}")
