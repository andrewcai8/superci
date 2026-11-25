#!/usr/bin/env python3
"""Lists orphans: machines of the scale sets that hold no registered runner (and are not new), and runners of the sets
whose machine is not running. Reads EC2 (both regions) with the ci-runners profile and GitHub's runner listing of the
deployment's runner group (superci-pull) through GH_APP_CMD, a command that prints a REST path's body as the App's
installation.

  GH_APP_CMD=ghapp orphans.py
"""
import json
import os
import re
import subprocess
from datetime import datetime, timezone

now = datetime.now(timezone.utc)
machines = {}
for region in ("us-east-1", "us-east-2"):
    out = json.loads(subprocess.run(["aws", "--profile", "ci-runners", "ec2", "describe-instances", "--region", region, "--filters",
                                     "Name=tag:purpose,Values=superci-pull", "Name=tag:superci-pull-deployment,Values=superci-pull",
                                     "Name=tag-key,Values=superci-pull-set",
                                     "Name=instance-state-name,Values=pending,running",
                                     "--query", "Reservations[].Instances[].{id:InstanceId,launch:LaunchTime,set:Tags[?Key=='superci-pull-set']|[0].Value}",
                                     "--output", "json"], check=True, capture_output=True, text=True).stdout)
    for m in out:
        machines[m["id"]] = m
def gh(path):
    return json.loads(subprocess.run([os.environ.get("GH_APP_CMD", "ghapp"), path], check=True, capture_output=True, text=True).stdout)


group = next(g["id"] for g in gh("/orgs/Authentic-Intelligence/actions/runner-groups?per_page=100")["runner_groups"] if g["name"] == "superci-pull")
runners, page = {}, 1
while True:
    body = gh(f"/orgs/Authentic-Intelligence/actions/runner-groups/{group}/runners?per_page=100&page={page}")
    for r in body["runners"]:
        m = re.match(r"^(superci-[0-9]+cpu(?:-[0-9]+)?)-(i-[0-9a-f]+)(?:-[0-9a-f]+)?$", r["name"])
        if m:
            runners[r["name"]] = (m.group(2), r["status"], r["busy"])
    if len(body["runners"]) < 100:
        break
    page += 1
held = {i for i, _, _ in runners.values()}
stale = [(n, s, b) for n, (i, s, b) in runners.items() if i not in machines]
bare = [m for i, m in machines.items() if i not in held and (now - datetime.fromisoformat(m["launch"].replace("Z", "+00:00"))).total_seconds() > 300]
print(f"{now.isoformat(timespec='seconds')}: {len(machines)} machines, {len(runners)} runners of the sets")
print(f"  runners whose machine is not running: {len(stale)} {stale[:5]}")
print(f"  machines older than 5 min holding no runner: {len(bare)} {[(m['id'], m['set']) for m in bare][:5]}")
