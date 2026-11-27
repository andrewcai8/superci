#!/usr/bin/env python3
"""timeline: where every second of a CI job goes, on superci and on Blacksmith.

  timeline job <job-id> --repo OWNER/REPO
  timeline compare --workflow lint-test-backend.yml --job verify-tools --since 2026-11-25T17:03:43Z [--until ...]
  timeline critical-path --run <run-id> --repo OWNER/REPO

A JobTimeline is derived, never stored. Its sources are append-only logs and the GitHub API:
  - the listener's `tl` events (pull/timeline.schema.json) in CloudWatch group /superci-pull/listener, keyed by the job's
    runner request id and the machine's instance id, including the marks boot.sh posts from the machine;
  - EC2's state-change notifications (group /superci-pull/ec2-state in each region);
  - for a window before those existed, `legacy` rebuilds machine.launched and jit.issued from CloudTrail's CreateFleet
    and the listener's old `fleet` and `register` lines;
  - GitHub's REST job (created, started, completed, steps) for both fleets, and a job's log for its cache restores.

The phases are the intervals between consecutive milestones of MILESTONES. A milestone the job's class expects and no
source gave leaves an `unobserved` interval, and that interval is what `unexplained` counts: every other second of the
job's wall time lies in a named phase.
"""
import argparse
import bisect
import collections
import concurrent.futures
import datetime as dt
import hashlib
import json
import os
import re
import subprocess
import sys
import time
import urllib.parse

LISTENER_GROUP = "/superci-pull/listener"
EC2_GROUP = "/superci-pull/ec2-state"
SUPERCI_RUNNER_GROUP = "superci-pull"
SCHEMA_VERSION = 1


# ---------------------------------------------------------------------------------------------------------------------
# The domain: milestones in the order a job meets them, and the phase each interval between two is named for.
# ---------------------------------------------------------------------------------------------------------------------

# (milestone, name of the phase that ends at it, who owns that phase)
MILESTONES = [
    ("job.queued", "", ""),
    ("listener.seen", "github to listener", "github"),
    ("launch.requested", "listener to launch call", "listener"),
    ("ec2.pending", "CreateFleet call", "ec2"),
    ("ec2.running", "ec2 pending to running", "ec2"),
    ("os.up", "ec2 running to kernel start", "ec2"),
    ("boot.start", "kernel and cloud-init to boot.sh", "machine"),
    ("cache.start", "boot.sh setup", "machine"),
    ("cache.up", "cache daemon up", "machine"),
    ("doc.got", "identity document", "machine"),
    ("jit.sent", "document to first ask", "machine"),
    ("jit.ask", "ask to listener", "network"),
    ("jit.granted", "registration token wait (budget, readiness, 3 s polling)", "listener"),
    ("jit.claimed", "identity claim (S3 conditional write)", "listener"),
    ("jit.issued", "GitHub registration and issued tag", "listener"),
    ("jit.answered", "config to machine", "network"),
    ("runner.start", "machine to run.sh", "machine"),
    ("runner.online", "runner connects", "runner"),
    ("runner.assigned", "runner online to job assigned (GitHub)", "github"),
    ("job.running", "assigned to running", "runner"),
    ("job.started", "running to job started (GitHub)", "runner"),
]
ORDER = {m: i for i, (m, _, _) in enumerate(MILESTONES)}
PHASE = {m: (p, o) for m, p, o in MILESTONES}
# A job on Blacksmith shows GitHub's API and nothing else.
API_ONLY = ("job.queued", "job.started")
WARM_WAIT_S = 6
CLOCK_FLAG_S = 0.25
PHASE_API_QUEUE = "queue wait (api only)"
# The marks boot.sh posts, in the schema, are milestones under the same name.
POST_JOB = ["listener.completed", "machine.ended", "ec2.shutting-down", "poweroff", "ec2.terminated"]


def ts(s):
    """RFC 3339 or EventBridge time to epoch seconds."""
    if s is None or s == "":
        return None
    if isinstance(s, (int, float)):
        return float(s)
    s = s.replace("Z", "+00:00")
    m = re.match(r"(.*\.\d{6})\d+(.*)", s)  # Go writes nanoseconds; fromisoformat takes microseconds
    if m:
        s = m.group(1) + m.group(2)
    return dt.datetime.fromisoformat(s).timestamp()


def iso(t):
    return dt.datetime.fromtimestamp(t, dt.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def instance_of(runner):
    """The instance id a runner is named after (<set>-<instance id>)."""
    if runner and "-i-" in runner:
        return "i-" + runner.rsplit("-i-", 1)[1]
    return None


def fleet_of(job):
    if job.get("runner_group_name") == SUPERCI_RUNNER_GROUP or (job.get("runner_name") or "").startswith("superci-"):
        return "superci"
    if "blacksmith" in (job.get("runner_name") or "") or any("blacksmith" in l for l in job.get("labels", [])):
        return "blacksmith"
    return "other"


class Timeline:
    """One job: its milestones (epoch seconds, first observation of each), phases and attributes."""

    def __init__(self, job):
        self.job = job
        self.id = job["id"]
        self.run = job["run_id"]
        self.name = job["name"]
        self.fleet = fleet_of(job)
        self.label = (job.get("labels") or [""])[0]
        self.runner = job.get("runner_name")
        self.inst = instance_of(self.runner) if self.fleet == "superci" else None
        self.marks = {}
        self.attrs = {"fleet": self.fleet, "label": self.label}
        self.steps = []
        self.phases = []  # (label, owner, seconds, start, end)
        self.after = []  # (name, t) after the job completed
        self.cache = []
        self.wall = None
        self.unexplained = 0.0
        self.overlap = 0.0
        self.gaps = []
        self.pre = []  # (name, t) machine milestones that happened before the job was queued


def first(events, name, **match):
    out = [e for e in events if e["ev"] == name and all(e.get(k) == v for k, v in match.items())]
    return min(out, key=lambda e: e["t"]) if out else None


MACHINE_MARKS = ("boot.start", "cache.start", "cache.up", "doc.got", "jit.sent", "jit.answered", "runner.start", "runner.online", "job.running")


def offset_ms(e):
    """The machine clock offset an event carries: offset_ms, the smallest of the machine's last few posts' recv less sent
    (a post's own delay is in it, so the smallest is the nearest to the offset), or skew_ms, which lines written before
    the offset estimate hold: one post's recv less sent."""
    k = e.get("offset_ms")
    return e.get("skew_ms") if k is None else k


def on_listener_clock(e):
    """A machine mark on the listener's clock. offset_ms is the listener's clock less the machine's at the post that carried
    the mark: a machine's clock is seconds off at boot until chrony steps it (1.7 s on the first live run, 36 ms by the
    end), so each post's own estimate moves its marks, not one offset for the machine."""
    return e["t"] + (offset_ms(e) or 0) / 1000


def machine_milestones(tl, events, created):
    """Fill tl.marks and tl.attrs of a superci job from the events of its machine and of its run."""
    m, inst = tl.marks, tl.inst
    mine = [e for e in events if e.get("inst") == inst]
    js = first([e for e in mine if e.get("run") == tl.run], "job.started")
    if js:
        tl.attrs["set"] = js.get("set")
        if js.get("runner_assign_time"):
            m["runner.assigned"] = ts(js["runner_assign_time"])
        seen = [e for name in ("job.assigned", "job.available") for e in [first(events, name, job=js.get("job"), set=js.get("set"))] if e]
        if seen:  # GitHub sends this listener JobAssigned, never JobAvailable (0 of 7,023 messages on Nov 25)
            m["listener.seen"] = min(e["t"] for e in seen)
    ln = first(mine, "machine.launched")
    if ln:
        m["launch.requested"] = ts(ln["requested"]) if ln.get("requested") else ln["t"]
        for k in ("region", "az", "type", "lifecycle", "round", "answer_ms"):
            tl.attrs[k] = ln.get(k)
        if created is not None:
            tl.attrs["pool"] = "cold" if m["launch.requested"] >= created else "warm"
    for state in ("pending", "running", "shutting-down", "terminated"):
        e = first(mine, "ec2.state", state=state)
        if e:
            m["ec2." + state] = e["t"]
    for name in MACHINE_MARKS + ("poweroff", "jit.ask", "jit.granted", "jit.issued", "machine.ended"):
        e = first(mine, name)
        if e:
            m[name] = on_listener_clock(e)
    c = first(mine, "jit.claim")
    if c:
        m["jit.claimed"] = c["t"]
        tl.attrs["claim"] = c.get("result")
    bs = first(mine, "boot.start")
    if bs and bs.get("val"):
        try:
            m["os.up"] = on_listener_clock(bs) - float(bs["val"])
        except ValueError:
            pass
    done = first([e for e in mine if e.get("run") == tl.run], "job.completed")
    if done:
        m["listener.completed"] = done["t"]
    for key, name, field in (("ask_verdict", "jit.ask", "verdict"), ("register_ms", "jit.issued", "register_ms"), ("end_why", "machine.ended", "why")):
        e = first(mine, name)
        if e:
            tl.attrs[key] = e.get(field)
    skews = [offset_ms(e) for e in mine if offset_ms(e) is not None]
    tl.attrs["clock_offset_ms"] = max(skews, key=abs) if skews else None
    tl.attrs["clock_corrected"] = any(abs(k) > CLOCK_FLAG_S * 1000 for k in skews) or None
    tl.cache = [cache_mark(e) for e in sorted(mine, key=lambda e: e["t"]) if e["ev"] in ("cache.restore", "cache.save")]


def build(job, events):
    """The Timeline of one REST job from the events that name its machine or its run. Pure.

    Chain milestones are taken in MILESTONES order; one the machine had passed before the job was queued is the
    machine's warm time (tl.pre) and the milestones before it are not the job's to explain. Between two milestones the
    chain expects, one that no source gave makes an unobserved interval, which counts as unexplained."""
    tl = Timeline(job)
    created, started, completed = ts(job.get("created_at")), ts(job.get("started_at")), ts(job.get("completed_at"))
    m = tl.marks
    m["job.queued"] = created
    if started is not None:
        m["job.started"] = started
    if tl.fleet == "superci":
        machine_milestones(tl, events, created)
    # a listener from before the claim (pull-fence) wrote no jit.claim, and its jobs do not owe the milestone
    claims = any(e["ev"] == "jit.claim" for e in events)
    chain = [x for x, _, _ in MILESTONES if claims or x != "jit.claimed"] if tl.fleet == "superci" else list(API_ONLY)
    pos = {x: i for i, x in enumerate(chain)}
    known_pre = [x for x in chain if x not in ("job.queued", "listener.seen") and m.get(x) is not None and created is not None and m[x] < created - 1]
    cut = max((ORDER[x] for x in known_pre), default=-1)
    if tl.fleet == "superci" and "pool" not in tl.attrs and started is not None and created is not None and started - created < WARM_WAIT_S:
        # No source knows the machine's launch, and a runner that took a job this fast was idle: a machine needs more
        # than WARM_WAIT_S to launch and boot (the pool's own fill time is 45 s). Inferred, and marked so.
        tl.attrs["pool"] = "warm (inferred)"
        cut = max(cut, ORDER["runner.online"])
    tl.pre = sorted(((x, m[x]) for x in known_pre), key=lambda p: p[1])
    seq = [(x, m[x], PHASE[x] if tl.fleet == "superci" else (PHASE_API_QUEUE, "github")) for x in chain
           if m.get(x) is not None and (x in ("job.queued", "listener.seen") or ORDER[x] > cut)]
    tl.steps = [(s["name"], ts(s["started_at"]), ts(s["completed_at"])) for s in job.get("steps", []) if s.get("started_at") and s.get("completed_at")]
    for i, (n, a, b) in enumerate(tl.steps):
        seq.append((f"step{i}.start", a, ("between steps" if i else "job started to first step", "runner")))
        seq.append((f"step{i}.end", b, (n, "runner")))
    if completed is not None:
        seq.append(("job.completed", completed, ("last step to job completed", "runner")))
    cur = prev = None
    for x, t, (label, owner) in seq:
        if cur is None:
            cur, prev = t, x
            continue
        if t < cur:
            tl.overlap += cur - t
            t = cur
        if prev in pos and x in pos:
            missing = [y for y in chain[pos[prev] + 1:pos[x]] if (y == "listener.seen" or ORDER[y] > cut) and m.get(y) is None]
            if missing:
                lost = missing[0] if len(missing) == 1 else f"{missing[0]} .. {missing[-1]}"
                label, owner = f"unobserved: {prev} to {x} (no {lost})", "?"
                tl.gaps.append((prev, x, missing, t - cur))
                tl.unexplained += t - cur
        tl.phases.append((label, owner, t - cur, cur, t, x))
        cur, prev = t, x
    if created is not None and completed is not None and cur is not None:
        tl.wall = completed - created
        tl.unexplained += tl.wall - (cur - created)  # time after the last milestone the sources reach
    tl.after = sorted(((x, m[x]) for x in POST_JOB if m.get(x) is not None and completed is not None), key=lambda p: p[1])
    return tl


# ---------------------------------------------------------------------------------------------------------------------
# Sources
# ---------------------------------------------------------------------------------------------------------------------

class Cache:
    """A directory of JSON by request key, so a rerun of a window costs no API calls."""

    def __init__(self, path):
        self.path = path
        os.makedirs(path, exist_ok=True)

    def get(self, key, fetch, keep=True, fresh=False, worth=None):
        """The cached value of key, or fetch()'s. fresh skips the read and replaces the entry; worth(v), when given, decides whether a fetched value is stored at all."""
        f = os.path.join(self.path, hashlib.sha1(key.encode()).hexdigest() + ".json")
        if keep and not fresh and os.path.exists(f):
            with open(f) as fh:
                return json.load(fh)
        v = fetch()
        if keep and (worth is None or worth(v)):
            with open(f + ".tmp", "w") as fh:
                json.dump(v, fh)
            os.replace(f + ".tmp", f)
        return v


class GitHub:
    def __init__(self, repo, cache):
        self.repo, self.cache, self.calls = repo, cache, 0

    def api(self, path, keep=True, raw=False):
        def fetch():
            for attempt in range(4):
                self.calls += 1
                p = subprocess.run(["gh", "api", f"repos/{self.repo}/{path}"], capture_output=True, text=True)
                if not p.returncode:
                    return p.stdout if raw else json.loads(p.stdout)
                if "HTTP 4" in p.stderr and "HTTP 429" not in p.stderr:
                    break  # a refusal that a retry will not change
                time.sleep(2 ** attempt)
            raise SystemExit(f"gh api {path}: {p.stderr.strip()[:300]}")

        return self.cache.get("gh:" + self.repo + path, fetch, keep)

    def job(self, job_id):
        return self.api(f"actions/jobs/{job_id}")

    def run_jobs(self, run_id, attempts="latest", fresh=False):
        """The jobs of a run: those of its latest attempt, or (attempts="all") of every attempt. The listing is cached only once every job in it
        has completed: a run in flight, or a rerun's attempt not yet started, would otherwise stay as the first read saw it. fresh reads GitHub
        again and replaces the entry."""
        def fetch():
            out, page = [], 1
            while True:
                d = self.api(f"actions/runs/{run_id}/jobs?per_page=100&filter={attempts}&page={page}", keep=False)
                out += d["jobs"]
                if len(out) >= d["total_count"] or not d["jobs"]:
                    return out
                page += 1

        return self.cache.get(f"gh:{self.repo}actions/runs/{run_id}/jobs:{attempts}", fetch, fresh=fresh, worth=lambda jobs: all(j.get("completed_at") for j in jobs))

    def runs(self, workflow, since, until):
        out, page = [], 1
        past = until < time.time() - 600
        while True:
            d = self.api(f"actions/workflows/{workflow}/runs?created={iso(since)}..{iso(until)}&per_page=100&page={page}", keep=past)
            out += d["workflow_runs"]
            if len(d["workflow_runs"]) < 100:
                return out
            page += 1

    def log(self, job_id):
        return self.api(f"actions/jobs/{job_id}/logs", raw=True)


SUPERCI_ACCOUNT = "243891825325"
_checked = set()


def aws(profile, *args):
    """The AWS CLI under --profile, which wins over any AWS_ACCESS_KEY_ID in the environment; the first call of a profile
    proves it is the superci account, and any other is refused."""
    if profile not in _checked:
        who = subprocess.run(["aws", "--profile", profile, "sts", "get-caller-identity", "--query", "Account", "--output", "text"], capture_output=True, text=True)
        if who.stdout.strip() != SUPERCI_ACCOUNT:
            raise SystemExit(f"refusing: profile {profile} is account {who.stdout.strip() or who.stderr.strip()[:200]}, not {SUPERCI_ACCOUNT}")
        _checked.add(profile)
    p = subprocess.run(["aws", "--profile", profile, *args], capture_output=True, text=True)
    if p.returncode:
        raise SystemExit(f"aws {' '.join(args[:3])}: {p.stderr.strip()[:400]}")
    return json.loads(p.stdout) if p.stdout.strip() else {}


def insights(profile, region, group, query, start, end, limit=10000):
    """Rows (dicts) of a Logs Insights query; a window that fills the row limit is halved."""
    q = aws(profile, "logs", "start-query", "--region", region, "--log-group-name", group, "--start-time", str(int(start)),
            "--end-time", str(int(end)), "--query-string", f"{query} | limit {limit}")["queryId"]
    while True:
        time.sleep(1)
        r = aws(profile, "logs", "get-query-results", "--region", region, "--query-id", q)
        if r["status"] in ("Complete", "Failed", "Cancelled", "Timeout"):
            break
    if r["status"] != "Complete":
        raise SystemExit(f"insights {r['status']}: {query}")
    rows = [{f["field"]: f["value"] for f in row} for row in r["results"]]
    if len(rows) >= limit and end - start > 60:
        mid = (start + end) / 2
        return insights(profile, region, group, query, start, mid, limit) + insights(profile, region, group, query, mid, end, limit)
    return rows


def listener_events(profile, start, end, where='msg = "tl"', region="us-east-1"):
    rows = insights(profile, region, LISTENER_GROUP, f"fields @message | filter {where} | sort @timestamp asc", start, end)
    out, seen = [], set()
    for r in rows:
        try:
            e = json.loads(r["@message"])
        except ValueError:
            continue
        if e.get("msg") != "tl" or e.get("v") != SCHEMA_VERSION or e.get("eid") in seen:
            continue  # the log ships at least once: one line of each eid
        seen.add(e.get("eid"))
        e["t"] = ts(e["t"])
        out.append(e)
    return out


def ec2_events(profile, regions, ids, start, end):
    out = []
    for region in regions:
        for i in range(0, len(ids), 50):
            pattern = "|".join(ids[i:i + 50])  # the field name has a dash Insights will not take, so match the line
            try:
                rows = insights(profile, region, EC2_GROUP, f"fields @message | filter @message like /({pattern})/", start, end)
            except SystemExit:
                return out  # the group does not exist in this window: the notifications were not on yet
            for r in rows:
                d = json.loads(r["@message"])
                out.append({"ev": "ec2.state", "t": ts(d["time"]), "inst": d["detail"]["instance-id"], "state": d["detail"]["state"]})
    return out


def legacy_events(profile, start, end, regions=("us-east-1", "us-east-2")):
    """machine.launched and jit.issued rebuilt from before the listener wrote tl events: CloudTrail's CreateFleet (the
    instances), the listener's `fleet` line with the same ClientToken (when the call began and ended) and its
    `register` line (when each runner was issued)."""
    out = []
    fleets = {}
    for r in insights(profile, "us-east-1", LISTENER_GROUP, 'fields @message | filter msg = "fleet"', start - 60, end + 60):
        e = json.loads(r["@message"])
        fleets[e.get("token")] = e
    for r in insights(profile, "us-east-1", LISTENER_GROUP, 'fields @message | filter msg = "register"', start - 60, end + 3600):
        e = json.loads(r["@message"])
        out.append({"ev": "jit.issued", "t": ts(e["time"]), "inst": e["machine"], "set": e["set"], "runner": e["runner"], "register_ms": e["ms"]})
    for region in regions:
        d = aws(profile, "cloudtrail", "lookup-events", "--region", region, "--lookup-attributes", "AttributeKey=EventName,AttributeValue=CreateFleet",
                "--start-time", iso(start - 60), "--end-time", iso(end + 60))  # the CLI follows the pages
        for ev in d.get("Events", []):
            ce = json.loads(ev["CloudTrailEvent"])
            req = (ce.get("requestParameters") or {}).get("CreateFleetRequest", {})
            items = ((ce.get("responseElements") or {}).get("CreateFleetResponse") or {}).get("fleetInstanceSet", {}).get("item", [])
            if isinstance(items, dict):
                items = [items]
            f = fleets.get(req.get("ClientToken"), {})
            end_t = ts(f["time"]) if f.get("time") else ts(ce["eventTime"])
            req_t = end_t - f["ms"] / 1000 if f.get("ms") is not None else end_t
            for it in items:
                ids = it.get("instanceIds", {}).get("item", [])
                for i in ([ids] if isinstance(ids, str) else ids):
                    out.append({"ev": "machine.launched", "t": end_t, "inst": i, "set": f.get("set"), "region": region, "az": it.get("availabilityZone"),
                                "type": it.get("instanceType"), "lifecycle": it.get("lifecycle"), "round": f.get("round"),
                                "requested": req_t, "answer_ms": f.get("ms")})
    return out


def cache_mark(e):
    """A cache.restore or cache.save mark of the daemon: val is <hit|miss|saved>:<bytes>:<ms>:<key>, the key percent-encoded."""
    parts = (e.get("val") or "").split(":", 3)
    kind, nbytes, ms, key = parts if len(parts) == 4 else ("?", "0", "0", e.get("val") or "")
    return {"key": urllib.parse.unquote(key), "hit": kind in ("hit", "saved"), "op": e["ev"].split(".")[1], "bytes": int(nbytes or 0), "seconds": int(ms or 0) / 1000}


def cache_restores(log):
    """Each cache restore in a job log: the key, hit or miss, bytes, and the seconds from the size line to the end line."""
    out, size = [], None
    line_t = re.compile(r"^(\d{4}-\d\d-\d\dT[\d:.]+Z) (.*)$")
    for raw in log.splitlines():
        m = line_t.match(raw.lstrip("﻿"))
        if not m:
            continue
        t, text = ts(m.group(1)), m.group(2)
        s = re.search(r"Cache Size: ~\d+ MB \((\d+) B\)", text)
        if s:
            size = (int(s.group(1)), t)
        r = re.search(r"Cache restored from key: (.*)", text)
        if r:
            out.append({"key": r.group(1).strip(), "hit": True, "bytes": size[0] if size else None, "seconds": round(t - size[1], 1) if size else None})
            size = None
        n = re.search(r"Cache not found for input keys?: (.*)", text)
        if n:
            out.append({"key": n.group(1).strip(), "hit": False, "bytes": 0, "seconds": None})
    return out


# ---------------------------------------------------------------------------------------------------------------------
# Output
# ---------------------------------------------------------------------------------------------------------------------

def q(values, p):
    v = sorted(values)
    if not v:
        return None
    k = (len(v) - 1) * p / 100
    lo = int(k)
    hi = min(lo + 1, len(v) - 1)
    return v[lo] + (v[hi] - v[lo]) * (k - lo)


def f1(x):
    return "   -" if x is None else f"{x:6.1f}"


def show_job(tl):
    j = tl.job
    print(f"job {tl.id}  {tl.name}  run {tl.run}  {tl.fleet}  label {tl.label}  runner {tl.runner}")
    a = tl.attrs
    bits = [f"{k}={a[k]}" for k in ("region", "az", "type", "lifecycle", "pool", "ask_verdict", "claim", "register_ms", "clock_offset_ms", "clock_corrected", "end_why") if a.get(k) is not None]
    print("  " + "  ".join(bits) if bits else "  (no machine attributes: api only)")
    if tl.wall is None:
        print("  job has not completed")
        return
    t0 = tl.marks["job.queued"]
    print(f"  wall {tl.wall:.1f} s: created {iso(t0)} to completed {iso(t0 + tl.wall)}")
    if tl.pre:
        print("  before the job was queued (the machine was warm): " + ", ".join(f"{n} {t - t0:+.1f}" for n, t in tl.pre))
    print(f"  {'at':>7} {'sec':>6} {'%':>5}  phase")
    between = [p for p in tl.phases if p[0] == "between steps"]
    for label, owner, sec, a_, b_, _ in tl.phases:
        if label != "between steps":
            print(f"  {b_ - t0:7.1f} {sec:6.1f} {100 * sec / tl.wall:5.1f}  {label[:100]}" + (f"  [{owner}]" if owner and not label.startswith("unobserved") else ""))
    if between:
        sec = sum(p[2] for p in between)
        print(f"  {'':>7} {sec:6.1f} {100 * sec / tl.wall:5.1f}  between steps, {len(between)} gaps  [runner]")
    print(f"  unexplained {tl.unexplained:.1f} s ({100 * tl.unexplained / tl.wall:.1f}%); overlap clamped {tl.overlap:.1f} s (clock skew and 1 s timestamps)")
    for n, t in tl.after:
        print(f"  after the job: {n} {t - t0 - tl.wall:+.1f} s")
    for c in tl.cache:
        print(f"  cache {c.get('op', 'restore')} {'hit ' if c['hit'] else 'miss'} {c['seconds'] if c['seconds'] is not None else '-'} s {c['bytes'] or 0:>12} B  {c['key'][:90]}")


def gather(profile, jobs, before, after=900, regions=("us-east-1", "us-east-2"), legacy=False, legacy_before=1800):
    """The events for a set of jobs: the listener's tl lines from `before` seconds ahead of the first job, EC2's states of
    their machines, and, where the listener wrote no tl lines or legacy is asked, the rebuilt ones."""
    sup = [j for j in jobs if fleet_of(j) == "superci" and j.get("completed_at")]
    if not sup:
        return []
    first_created = min(ts(j["created_at"]) for j in sup)
    hi = max(ts(j["completed_at"]) for j in sup) + after
    ev = listener_events(profile, first_created - before, hi)
    if legacy or not any(e["ev"] == "machine.launched" for e in ev):
        ev += legacy_events(profile, first_created - legacy_before, hi, regions)
    ids = sorted({instance_of(j["runner_name"]) for j in sup if instance_of(j.get("runner_name"))})
    ev += ec2_events(profile, regions, ids, first_created - before, hi + 3600)
    return ev


# ---------------------------------------------------------------------------------------------------------------------
# Commands
# ---------------------------------------------------------------------------------------------------------------------

def cmd_job(a):
    gh = GitHub(a.repo, Cache(a.cache))
    job = gh.job(a.job_id)
    ev = gather(a.profile, [job], before=6 * 3600, regions=(a.region,) if a.region else ("us-east-1", "us-east-2"), legacy=a.legacy)
    tl = build(job, ev)
    if not a.no_logs and job.get("completed_at"):
        tl.cache = cache_restores(gh.log(job["id"]))
    show_job(tl)


def safe(fetch, what):
    """A run GitHub will not list jobs for (a 5xx that outlasts the retries) is skipped and said so, not fatal."""
    try:
        return fetch()
    except SystemExit as e:
        print(f"skipped run {what}: {e}", file=sys.stderr)
        return []


def name_matches(job, pattern):
    return re.search(pattern, job["name"], re.I) is not None


def cmd_compare(a):
    gh = GitHub(a.repo, Cache(a.cache))
    since, until = ts(a.since), ts(a.until) if a.until else time.time()

    def window(lo, hi):
        runs = gh.runs(a.workflow, lo, hi)
        with concurrent.futures.ThreadPoolExecutor(6) as pool:
            per_run = list(pool.map(lambda r: safe(lambda: gh.run_jobs(r["id"]), r["id"]), runs))
        out = [j for jobs_ in per_run for j in jobs_
               if name_matches(j, a.job) and j.get("completed_at") and j["conclusion"] in ("success", "failure")]
        return runs, out

    runs, jobs = window(since, until)
    if a.baseline_since:  # Blacksmith's side comes from its own window, when the jobs moved to superci in this one
        b_runs, b_jobs = window(ts(a.baseline_since), ts(a.baseline_until))
        runs = runs + b_runs
        jobs = [j for j in jobs if fleet_of(j) == "superci"] + [j for j in b_jobs if fleet_of(j) == "blacksmith"]
    jobs = [j for j in jobs if fleet_of(j) != "superci" or j.get("runner_group_name") == a.superci_group]
    ev = gather(a.profile, jobs, before=3 * 3600, legacy=a.legacy)
    tls = [build(j, ev) for j in jobs]
    tls = [t for t in tls if t.wall is not None]
    print(f"workflow {a.workflow}  job /{a.job}/  created {iso(since)} to {iso(until)}  ({len(runs)} runs, {gh.calls} GitHub calls)")
    by = collections.defaultdict(list)
    for t in tls:
        by[t.fleet].append(t)
    for f, l in sorted(by.items()):
        cls = collections.Counter(t.attrs.get("pool", "-") for t in l)
        print(f"  {f}: {len(l)} jobs  pool/cold: {dict(cls)}  label: {dict(collections.Counter(t.label for t in l))}")
    S, B = by.get("superci", []), by.get("blacksmith", [])
    rows = []

    def add(name, sget, bget, indent=0):
        sv = [x for x in map(sget, S) if x is not None]
        bv = [x for x in map(bget, B) if x is not None] if bget else []
        rows.append((indent, name, sv, bv))

    qw = lambda t: t.marks["job.started"] - t.marks["job.queued"] if "job.started" in t.marks else None
    add("queue wait (created to started)", qw, qw)
    for cls in ("cold", "warm"):
        add(f"queue wait, {cls} machine", lambda t, cls=cls: qw(t) if str(t.attrs.get("pool", "")).startswith(cls) else None, None, 1)
    # sub-phases of the queue wait, superci only: the phases that end at a chain milestone; unobserved intervals are one row
    key = lambda label: label.split(" (no ")[0]  # an unobserved interval is one row by its two ends, whatever it lacks
    labels = []
    for t in S:
        for label, owner, sec, x, y, end in t.phases:
            if end in ORDER and key(label) not in labels:
                labels.append(key(label))
    for label in labels:
        def part(t, label=label):
            v = [sec for l, o, sec, x, y, end in t.phases if end in ORDER and key(l) == label]
            return sum(v) if v else None
        add(label, part, None, 1)
    run = lambda t: t.wall - qw(t) if qw(t) is not None else None
    add("run (started to completed)", run, run)
    stepnames = []
    for t in tls:
        for n, x, y in t.steps:
            if n not in stepnames:
                stepnames.append(n)
    for n in stepnames:
        g = lambda t, n=n: next((y - x for m, x, y in t.steps if m == n), None)
        add("step: " + n[:48], g, g, 1)
    add("wall (created to completed)", lambda t: t.wall, lambda t: t.wall)
    add("unexplained (unobserved intervals and the tail after the last milestone)", lambda t: t.unexplained, lambda t: t.unexplained)
    add("overlap clamped (sources disagreeing within their resolution)", lambda t: t.overlap, lambda t: t.overlap)
    print(f"\n{'':54} {'superci':^26} {'blacksmith':^26}")
    print(f"{'seconds':54} {'p50':>7} {'p90':>7} {'n':>5}   {'p50':>7} {'p90':>7} {'n':>5}")
    hidden = 0
    for indent, name, sv, bv in rows:
        if not sv and not bv:
            continue
        if name.startswith("step: ") and max(q(sv, 90) or 0, q(bv, 90) or 0) < a.min_step:
            hidden += 1
            continue
        print(f"{'  ' * indent + name:54} {f1(q(sv, 50)):>7} {f1(q(sv, 90)):>7} {len(sv):>5}   {f1(q(bv, 50)):>7} {f1(q(bv, 90)):>7} {len(bv):>5}")
    if hidden:
        print(f"  ({hidden} steps under {a.min_step:g} s at p90 on both fleets not shown; --min-step 0 shows them)")


def cmd_critical_path(a):
    gh = GitHub(a.repo, Cache(a.cache))
    jobs = [j for j in gh.run_jobs(a.run) if j.get("completed_at")]
    if not jobs:
        raise SystemExit("no completed jobs in the run")
    ev = gather(a.profile, jobs, before=3 * 3600, legacy=a.legacy)
    tls = {j["id"]: build(j, ev) for j in jobs}
    created = {j["id"]: ts(j["created_at"]) for j in jobs}
    done = {j["id"]: ts(j["completed_at"]) for j in jobs}
    run = gh.api(f"actions/runs/{a.run}")
    t0 = ts(run["created_at"])
    last = max(jobs, key=lambda j: done[j["id"]])
    path, cur = [last], last
    while True:  # a job's predecessor is the job that completed just before it was created (the needs graph is not in the REST API)
        prior = [j for j in jobs if j["id"] != cur["id"] and done[j["id"]] <= created[cur["id"]] + 1.5 and created[cur["id"]] - done[j["id"]] < 30]
        if not prior:
            break
        cur = max(prior, key=lambda j: done[j["id"]])
        path.append(cur)
    path.reverse()
    wall = done[last["id"]] - t0
    print(f"run {a.run}  {run.get('name')}  event {run.get('event')}  wall {wall:.1f} s (run created {iso(t0)}); path inferred from job created/completed times [I]")
    worst = (None, 0)
    for j in path:
        t = tls[j["id"]]
        print(f"\n  job {t.name} ({t.fleet}, {t.label}) created +{created[j['id']] - t0:.1f}s completed +{done[j['id']] - t0:.1f}s wall {t.wall:.1f}s unexplained {t.unexplained:.1f}s")
        for label, owner, sec, x, y, _ in sorted(t.phases, key=lambda p: -p[2])[:5]:
            print(f"      {sec:6.1f} s  {label[:100]}")
            if sec > worst[1]:
                worst = ((t.name, label), sec)
    if worst[0]:
        print(f"\n  the longest single phase on the path: {worst[1]:.1f} s, {worst[0][1][:100]} of job {worst[0][0][:40]} ({100 * worst[1] / wall:.0f}% of the run)")


def cmd_export(a):
    """Copy one UTC day of the listener's log and of EC2's state changes to the export bucket (an export task each)."""
    day = dt.datetime.strptime(a.day, "%Y-%m-%d").replace(tzinfo=dt.timezone.utc)
    lo, hi = int(day.timestamp() * 1000), int((day + dt.timedelta(days=1)).timestamp() * 1000) - 1
    acct = aws(a.profile, "sts", "get-caller-identity")["Account"]
    bucket = f"superci-pull-timeline-{acct}"
    for region, group in (("us-east-1", LISTENER_GROUP), ("us-east-1", EC2_GROUP), ("us-east-2", EC2_GROUP)):
        t = aws(a.profile, "logs", "create-export-task", "--region", region, "--log-group-name", group, "--from", str(lo), "--to", str(hi),
                "--destination", bucket, "--destination-prefix", f"timeline/{region}{group}/{a.day}")
        print(f"export task {t['taskId']}: {region} {group} {a.day} to s3://{bucket}/timeline/{region}{group}/{a.day}")


def main(argv=None):
    p = argparse.ArgumentParser(prog="timeline", description=__doc__.split("\n")[0])
    p.add_argument("--profile", default=os.environ.get("AWS_PROFILE", "ci-runners"), help="AWS profile of the superci account")
    p.add_argument("--repo", default="Authentic-Intelligence/megpt-mono")
    p.add_argument("--cache", default=os.path.expanduser("~/.cache/superci-timeline"), help="directory of cached GitHub answers")
    p.add_argument("--legacy", action="store_true", help="also rebuild launches and registrations from CloudTrail and the old listener lines")
    sub = p.add_subparsers(dest="cmd", required=True)
    j = sub.add_parser("job", help="one job's phase-by-phase breakdown")
    j.add_argument("job_id")
    j.add_argument("--region", help="the machine's region, when known")
    j.add_argument("--no-logs", action="store_true", help="skip the job log (cache restores)")
    j.set_defaults(fn=cmd_job)
    c = sub.add_parser("compare", help="superci against Blacksmith, p50 and p90 for each phase and step")
    c.add_argument("--workflow", required=True)
    c.add_argument("--job", required=True, help="regex on the job's name")
    c.add_argument("--since", required=True)
    c.add_argument("--until")
    c.add_argument("--baseline-since", help="take Blacksmith's jobs from this window instead (when the job moved to superci in --since..--until)")
    c.add_argument("--baseline-until")
    c.add_argument("--min-step", type=float, default=1.0, help="hide steps whose p90 is under this many seconds on both fleets")
    c.add_argument("--superci-group", default=SUPERCI_RUNNER_GROUP, help="count a superci job only from this runner group")
    c.set_defaults(fn=cmd_compare)
    k = sub.add_parser("critical-path", help="which job and phase set a run's wall time")
    k.add_argument("--run", required=True)
    k.set_defaults(fn=cmd_critical_path)
    e = sub.add_parser("export", help="copy a day of the logs to S3, past CloudWatch's 30 days")
    e.add_argument("--day", required=True, help="UTC day, YYYY-MM-DD")
    e.set_defaults(fn=cmd_export)
    a = p.parse_args(argv)
    a.fn(a)


if __name__ == "__main__":
    main()
