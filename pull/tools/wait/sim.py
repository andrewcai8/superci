"""Trace-driven model of one superci set's queue wait and idle cost under a pool policy.

The listener keeps supply = jobs waiting + pool target (desired = assigned + waiting + pool). Each arriving job is seen
`seen` s after GitHub creates it and orders one machine then; a pool change orders or retires the difference. A
machine is ready (its runner registered) `lead` s after its order, drawn from the canary's measured fleet-start to
registered times. A job starts `start` s after it meets a ready machine (GitHub's assignment and the runner's pickup,
2 s measured, warm or cold). Machines are fungible and taken in ready order: one job per machine.

Idle cost is the time a ready machine waits for its job, plus the time a retired one waited, at the size's price.
"""
import bisect, heapq, json, math, random

SEEN, START = 1.5, 2.0


def quantile(xs, p):
    xs = sorted(xs)
    return xs[min(len(xs) - 1, int(p * len(xs)))] if xs else float("nan")


class Policy:
    """pool(t, recent, span) -> target idle runners. recent: the times the listener saw jobs over its window (sorted);
    span: the window's length so far, in seconds."""

    def __init__(self, fn, window=600.0):
        self.fn, self.window = fn, window


def simulate(arrivals, policy, leads, rng, prelaunch=None, retire_hold=20.0, retire_step=10.0):
    """arrivals: sorted creation times. prelaunch: optional {time: n} of extra machines ordered ahead (predictive).
    Returns (waits, idle machine-seconds, machines launched)."""
    arrivals = sorted(arrivals)
    ready = []  # heap of ready times of machines not yet taken (in flight or idle)
    waits, idle, launched = [], 0.0, 0
    t0 = arrivals[0] - 900 if arrivals else 0
    seen_times = []
    events = [(a + SEEN, 1, a) for a in arrivals]
    if prelaunch:
        events += [(t, 0, n) for t, n in prelaunch.items()]
    t = t0
    while t < arrivals[0]:
        events.append((t, 2, None))
        t += retire_step
    t = arrivals[0]
    while t < arrivals[-1] + 60:
        events.append((t, 2, None))
        t += retire_step
    events.sort()
    want = 0
    surplus_since = None

    def lead():
        return rng.choice(leads)

    for when, kind, x in events:
        if kind == 0:
            for _ in range(x):
                heapq.heappush(ready, when + lead())
                launched += 1
            continue
        if kind == 1:
            seen_times.append(when)
            if ready:
                r = heapq.heappop(ready)
            else:
                r = when + lead()
                launched += 1
            start = max(x + START, r + START)
            if r < x:
                idle += x - r
            waits.append(start - x)
            for _ in range(want - len(ready)):
                heapq.heappush(ready, when + lead())
                launched += 1
            continue
        cut = when - policy.window
        lo = bisect.bisect_left(seen_times, cut)
        span = min(policy.window, max(60.0, when - t0))
        want = policy.fn(when, seen_times[lo:], span)
        have = len(ready)
        if have < want:
            for _ in range(want - have):
                heapq.heappush(ready, when + lead())
                launched += 1
            surplus_since = None
        elif have > want:
            surplus_since = surplus_since if surplus_since is not None else when
            if when - surplus_since >= retire_hold:
                idle_now = sorted(r for r in ready if r <= when)
                drop = min(have - want, len(idle_now))
                for r in idle_now[:drop]:
                    ready.remove(r)
                    idle += when - r
                heapq.heapify(ready)
        else:
            surplus_since = None
    end = arrivals[-1] + 60
    idle += sum(end - r for r in ready if r < end)
    return waits, idle, launched


def summary(waits):
    return dict(n=len(waits), p50=quantile(waits, 0.5), p90=quantile(waits, 0.9), p99=quantile(waits, 0.99),
                warm=sum(1 for w in waits if w <= START + 0.01) / max(1, len(waits)))


def load_leads(path):
    return json.load(open(path))["machine_lead"]
