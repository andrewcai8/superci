"""Replays the canary's 4cpu arrivals with the pool it had (1) and compares the model's waits with the measured ones."""
import json, random, sys
sys.path.insert(0, __import__("os").path.dirname(__import__("os").path.abspath(__file__)))
from datetime import datetime
from sim import Policy, simulate, summary, load_leads

S = sys.argv[1]
rows = [json.loads(l) for l in open(f"{S}/logs/ltb-jobs.jsonl")]
t = lambda s: datetime.fromisoformat(s.replace("Z", "+00:00")).timestamp()
arr = [t(j["created_at"]) for j in rows if (j["runner_name"] or "").startswith("superci-4cpu") and j["started_at"] and t(j["started_at"]) >= t(j["created_at"])]
meas = [t(j["started_at"]) - t(j["created_at"]) for j in rows if (j["runner_name"] or "").startswith("superci-4cpu") and j["started_at"] and t(j["started_at"]) >= t(j["created_at"])]
leads = load_leads(f"{S}/logs/coldpath.json")
print("measured", summary(meas))
for seed in range(3):
    w, idle, n = simulate(arr, Policy(lambda now, recent, span: 1), leads, random.Random(seed))
    print("model   ", summary(w), "idle h %.2f" % (idle / 3600))
