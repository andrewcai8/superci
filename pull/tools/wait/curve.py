"""The wait-versus-idle-dollars curve of the burst rule over the modelled weekday, per quantile: per job (wait p50/p90)
and per push (the push's slowest job of the size, the most a required check can wait for a machine), with plain
(SHIFT 0) or hydrated (SHIFT 19) launch times. Pushes come from day.trace; each push's jobs are kept together.

  curve.py DATA HOURLY SHIFT
"""
import json, os, random, sys
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import day, sim
from adaptive import burst_pool
from sim import quantile, Policy, simulate

S, HOURLY, shift = sys.argv[1], sys.argv[2], float(sys.argv[3])
shapes, _ = day.batches(f"{S}/demand/win15.jsonl")
rates = day.hourly(HOURLY + "/*.json", "2026-11-23")
leads = [l + shift for l in sim.load_leads(f"{S}/logs/coldpath.json")]
ready = quantile(leads, 0.9)


def trace(shapes, jobs_per_hour, rng):
    mean = sum(len(b) for b in shapes) / len(shapes)
    out = []
    for h in range(24):
        lam = jobs_per_hour[h] / mean / 3600
        t, k = h * 3600.0, 0
        while lam > 0:
            t += rng.expovariate(lam)
            if t >= (h + 1) * 3600:
                break
            k += 1
            out += [(t + o, (h, k)) for o in rng.choice(shapes)]
    return sorted(out)


print(f"launch median {quantile(leads, .5):.0f} s p90 {ready:.0f} s")
print("q     idle$/day  job p50 p90   push-max p50 p90   pushes all-warm")
for q in (0.8, 0.85, 0.88, 0.9, 0.93, 0.95, 0.97):
    tot, jw, pw, warm = 0, [], [], 0
    for s in day.SIZES:
        tr = trace(shapes[s], rates[s], random.Random(s))
        arr = [a for a, _ in tr]
        w, idle, _ = simulate(arr, Policy(burst_pool(q, ready, 300), window=1800), leads, random.Random(1))
        tot += idle / 3600 * day.PRICE[s]
        jw += w
        worst = {}
        for (a, p), x in zip(tr, w):
            worst[(s, p)] = max(worst.get((s, p), 0), x)
        pw += list(worst.values())
        warm += sum(1 for x in worst.values() if x <= 2.01)
    print(f"{q:.2f}  {tot:9.0f}  {quantile(jw,.5):7.1f} {quantile(jw,.9):4.1f}   {quantile(pw,.5):12.1f} {quantile(pw,.9):4.1f}   {warm/len(pw):6.0%}")
