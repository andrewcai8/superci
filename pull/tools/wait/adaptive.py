"""The burst-quantile pool over the modelled weekday: pool = the q-quantile, over jobs seen in the last W minutes, of
how many jobs the set saw in the lead seconds up to and including each, then the max of that over the last H minutes
(a pool shrinks slowly; a retired idle runner wastes a registration), at least the hour's floor. Compared with the
searched static pools, per size: wait p50/p90, warm share, idle $/day, and machines launched beyond jobs (wasted
registrations)."""
import bisect, collections, json, os, random, sys
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import day
from sim import Policy, simulate, quantile, load_leads, START



def burst_pool(q, lead, hold, floor=0):
    history = collections.deque()

    def fn(now, recent, span):
        counts = []
        for i, a in enumerate(recent):
            counts.append(i - bisect.bisect_left(recent, a - lead) + 1)
        want = max(floor, int(quantile(counts, q)) if counts else 0)
        history.append((now, want))
        while history and history[0][0] < now - hold:
            history.popleft()
        return max(w for _, w in history)
    return fn


def run(arr, fn, window, leads):
    return simulate(arr, Policy(fn, window=window), leads, random.Random(1))


if __name__ == "__main__":
    S = sys.argv[1]
    leads = load_leads(f"{S}/logs/coldpath.json")
    grid = [(q, lead, w, h) for q in (0.8, 0.85, 0.9) for lead in (20.0, 22.0) for w in (600.0, 1800.0) for h in (300.0, 900.0)]
    best = {}
    for s in day.SIZES:
        arr = json.load(open(f"{S}/demand/day-{s}.json"))
        for q, lead, w, h in grid:
            waits, idle, n = run(arr, burst_pool(q, lead, h), w, leads)
            by = day.per_hour(arr, waits)
            hours_ok = sum(1 for ws in by.values() if day.meets(ws))
            r = dict(size=s, q=q, lead=lead, window=w, hold=h, p50=round(quantile(waits, .5), 1), p90=round(quantile(waits, .9), 1),
                     warm=round(sum(1 for x in waits if x <= START + .01) / len(waits), 3), usd_day=round(idle / 3600 * day.PRICE[s], 1),
                     waste=n - len(waits), hours_ok=hours_ok, hours=len(by))
            if day.meets(waits) and (s not in best or r["usd_day"] < best[s]["usd_day"]):
                best[s] = r
            print(json.dumps(r))
    print("best per size:")
    for s, r in best.items():
        print(json.dumps(r))
    print("sum $/day", round(sum(r["usd_day"] for r in best.values()), 1))
