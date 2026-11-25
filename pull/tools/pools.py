#!/usr/bin/env python3
"""Prints the "pools" section of config.json from Blacksmith's hourly usage exports: for each runner size, the cap of
its hot pool at each hour of the day (UTC) is the jobs that hour averaged a second, times the pool rule's fill
seconds and factor, rounded up. Floors are given per size. A size's jobs come from blacksmith-<cpu>vcpu-ubuntu-2404.

  pools.py HOURLY_DIR [FILL_SECONDS] [FACTOR] > pools.json
"""
import collections
import glob
import json
import math
import sys

hourly, fill, factor = sys.argv[1], float(sys.argv[2] if len(sys.argv) > 2 else 45), float(sys.argv[3] if len(sys.argv) > 3 else 1.5)
floors = {2: 1, 4: 1, 8: 1, 16: 0, 32: 0}
jobs, hours = collections.defaultdict(collections.Counter), collections.Counter()
for f in glob.glob(hourly + "/*.json"):
    d = json.load(open(f))
    h = int(d["window"]["start"][11:13])
    hours[h] += 1
    for r in d["breakdowns"].get("runner_type", []):
        for cpu in floors:
            if r["runner_type"] == f"blacksmith-{cpu}vcpu-ubuntu-2404":
                jobs[cpu][h] += r["jobs"]
out = {}
for cpu, floor in floors.items():
    rate = [jobs[cpu][h] / hours[h] / 3600 if hours[h] else 0 for h in range(24)]
    out[f"superci-{cpu}cpu"] = {"floor": [floor] * 24, "cap": [max(floor, math.ceil(r * fill * factor)) for r in rate],
                                "_jobs_per_s": [round(r, 2) for r in rate]}
print(json.dumps(out, indent=2))
