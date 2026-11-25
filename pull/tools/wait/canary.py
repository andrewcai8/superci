"""Join the canary's superci jobs (GitHub) with listener logs and EC2 records: per job, warm or cold, and the cold path."""
import json, sys, statistics as st
from datetime import datetime

S = sys.argv[1]

def t(s):
    s = s.replace("Z", "").replace("+00:00", "")
    if "." in s:
        a, b = s.split(".")
        s = a + "." + b[:6]
    return datetime.fromisoformat(s + "+00:00").timestamp()

ev = [json.loads(m) for m in json.load(open(f"{S}/logs/canary.json")) if m.startswith("{")]
for e in ev:
    e["t"] = t(e["time"])
reg = {e["machine"]: e for e in ev if e["msg"] == "register"}
fleet_by_round = {}
for e in ev:
    if e["msg"] == "fleet":
        fleet_by_round.setdefault(e["round"], []).append(e)
launched = {e["round"]: e for e in ev if e["msg"] == "launched"}
msgs = [e for e in ev if e["msg"] == "message" and e["assigned"] > 0]
states = [e for e in ev if e["msg"] == "state"]

ec2 = {}
for r in ("us-east-1", "us-east-2"):
    for i in json.load(open(f"{S}/logs/ec2-{r}.json")):
        i["tags"] = {x["Key"]: x["Value"] for x in (i["tags"] or [])}
        i["region"] = r
        ec2[i["id"]] = i

jobs = [json.loads(l) for l in open(f"{S}/logs/ltb-jobs.jsonl")]
jobs = [j for j in jobs if (j["runner_name"] or "").startswith("superci-") and j["started_at"]]
rows = []
for j in sorted(jobs, key=lambda j: j["created_at"]):
    c, s = t(j["created_at"]), t(j["started_at"])
    inst = "i-" + j["runner_name"].split("-i-")[1]
    m, r = ec2.get(inst), reg.get(inst)
    row = dict(job=j["name"][:28], run=j["run_id"], wait=s - c, inst=inst, label=j["labels"][0])
    if m:
        L = t(m["launch"])
        row["launch_minus_created"] = L - c
        rnd = m["tags"].get("superci-pull-round")
        row["round"] = rnd
        fl = fleet_by_round.get(rnd, [])
        if fl:
            fstart = min(e["t"] - e["ms"] / 1000 for e in fl)
            fend = max(e["t"] for e in fl)
            row["fleet_start_minus_created"] = fstart - c
            row["fleet_ms"] = fend - fstart
        row["type"] = m["type"]
        row["region"] = m["region"]
    if r:
        ask = r["t"] - r["ms"] / 1000
        row["reg_ms"] = r["ms"]
        row["registered_minus_created"] = r["t"] - c
        if m:
            row["boot"] = ask - t(m["launch"])
        row["start_after_reg"] = s - r["t"]
    row["warm"] = bool(r) and r["t"] < c
    prev = [x for x in states if x["set"] == j["labels"][0] and x["t"] <= c]
    if prev:
        row["idle_at_arrival"] = prev[-1]["idle"]
        row["pool_at_arrival"] = prev[-1]["pool"]
    nxt = [x for x in msgs if x["set"] == j["labels"][0] and x["t"] >= c - 1]
    if nxt:
        row["msg_minus_created"] = nxt[0]["t"] - c
    rows.append(row)

json.dump(rows, open(f"{S}/logs/canary-rows.json", "w"), indent=1)
leads = sorted(r["registered_minus_created"] - r["fleet_start_minus_created"] for r in rows
               if "registered_minus_created" in r and "fleet_start_minus_created" in r)
json.dump({"machine_lead": leads}, open(f"{S}/logs/coldpath.json", "w"))
def q(xs, p):
    xs = sorted(xs)
    return xs[min(len(xs) - 1, int(p * len(xs)))] if xs else float("nan")
W = [r for r in rows if r["warm"]]
C = [r for r in rows if not r["warm"]]
print(f"jobs {len(rows)} warm {len(W)} cold {len(C)}")
print("wait all p50 %.0f p90 %.0f" % (q([r['wait'] for r in rows], .5), q([r['wait'] for r in rows], .9)))
print("wait warm p50 %.0f p90 %.0f" % (q([r['wait'] for r in W], .5), q([r['wait'] for r in W], .9)))
print("wait cold p50 %.0f p90 %.0f" % (q([r['wait'] for r in C], .5), q([r['wait'] for r in C], .9)))
for k in ("msg_minus_created", "fleet_start_minus_created", "fleet_ms", "launch_minus_created", "boot", "reg_ms", "registered_minus_created", "start_after_reg", "idle_at_arrival", "pool_at_arrival"):
    v = [r[k] for r in C if k in r]
    if v:
        print(f"cold {k:28s} n={len(v):3d} p10 {q(v,.1):6.1f} p50 {q(v,.5):6.1f} p90 {q(v,.9):6.1f}")
from collections import Counter
print(Counter(r["label"] for r in rows), Counter(r.get("region") for r in C))
