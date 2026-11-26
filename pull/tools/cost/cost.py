#!/usr/bin/env python3
"""What the machines of the ci-runners account cost, derived from EC2's own records instead of the listener's
sampled estimate (spend.go), and how much of each machine's paid time was a job.

  cost.py fetch DIR [--since T] [--until T]        CloudTrail launches and terminations, launch-template disks,
                                                   spot price history, on-demand list prices, Cost Explorer daily
                                                   totals, the listener's register and ask lines and today's instances
  cost.py jobs DIR RUNS_QUERY...                   GitHub jobs of the runs a query under repos/R/actions/ lists (e.g.
                                                   'workflows/lint-test-backend.yml/runs?created=2026-11-25T17:00:00Z..2026-11-25T17:30:00Z'
                                                   or 'runs?branch=ci/superci-load-pull-1'), keeping those on superci runners
  cost.py report DIR [--json OUT]                  per-machine dollars, phases, per-job dollars, and the gap to
                                                   Cost Explorer by day

A machine's life is its RunInstances event to its TerminateInstances event (CloudTrail, second resolution), or to
`now` while EC2 still lists it running. A machine that powers itself off leaves no CloudTrail event: its end is its
job's completion, or the listener's "ask ... verdict end" line, plus the teardown measured on machines whose end EC2
recorded; the report counts each source and leaves out machines with none. Its compute is the spot price in force over that interval (the step function
of DescribeSpotPriceHistory for its type and zone) or the on-demand list price, per second with EC2's 60 s minimum.
Its gp3 volume (size, throughput and IOPS from the launch-template version it launched from) and its public IPv4
address are charged over the same interval. Data transfer, S3, Lambda and the listener's own services are not per
machine; the report shows them as account lines from Cost Explorer.

Phases of a superci-pull machine: boot (launch to the listener's register log line), wait (registered to its job's
start), job (GitHub's started_at to completed_at), teardown (job end to termination). A registered machine with no
job joined is pool time; a machine that never registered is boot only. AWS reads use --profile ci-runners (override
with AWS_PROFILE) and are read-only. GitHub reads use `gh api`.
"""
import argparse
import bisect
import collections
import concurrent.futures
import glob
import json
import os
import re
import statistics
import subprocess
import sys
import time
from datetime import datetime, timedelta, timezone

PROFILE = os.environ.get("AWS_PROFILE", "ci-runners")
ACCOUNT = "243891825325"  # ci-runners; this host's environment also holds production keys, which must never answer
REGIONS = ["us-east-1", "us-east-2"]
REGION_PREFIX = {"us-east-1": "", "us-east-2": "USE2-"}
GB_MONTH_HOURS = 720  # November; AWS prorates a GB-month over the hours of the month
GP3_GB_MONTH = 0.08
GP3_MBPS_MONTH = 0.04  # above the free 125 MB/s
GP3_IOPS_MONTH = 0.005  # above the free 3,000
IPV4_HOUR = 0.005
MIN_SECONDS = 60
UTC = timezone.utc


def ts(s):
    if s is None:
        return None
    if isinstance(s, (int, float)):
        return datetime.fromtimestamp(s / 1000 if s > 1e11 else s, UTC)
    whole, dot, rest = s.replace("Z", "+00:00").partition(".")
    if dot:  # Go writes nanoseconds; Python reads at most microseconds
        digits, tail = re.match(r"(\d+)(.*)", rest).groups()
        return datetime.fromisoformat(f"{whole}.{(digits + '000000')[:6]}{tail}")
    return datetime.fromisoformat(whole)


def iso(t):
    return t.astimezone(UTC).strftime("%Y-%m-%dT%H:%M:%SZ")


def aws(region, *args):
    cmd = ["aws", "--profile", PROFILE, "--region", region, "--output", "json", *args]
    env = dict(os.environ, AWS_RETRY_MODE="adaptive", AWS_MAX_ATTEMPTS="12", AWS_PAGER="")
    out = subprocess.run(cmd, check=True, capture_output=True, text=True, env=env).stdout
    return json.loads(out) if out.strip() else {}


def write_json(path, obj):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    tmp = path + ".tmp"
    with open(tmp, "w") as f:
        json.dump(obj, f)
    os.replace(tmp, path)




def fetch_trail(d, since, until):
    """CloudTrail event history, one file per region, event name and UTC day; the two regions page in parallel
    (lookup-events allows 2 calls a second per region). A day that ended over 30 minutes ago is complete (CloudTrail
    delivers within about 15) and is not fetched again."""
    now = datetime.now(UTC)

    def region_days(region):
        for name in ("RunInstances", "TerminateInstances", "StopInstances", "StartInstances"):
            day = since.replace(hour=0, minute=0, second=0, microsecond=0)
            while day < until:
                end = day + timedelta(days=1)
                path = f"{d}/ct/{region}/{name}/{day:%Y-%m-%d}.json"
                want = min(end, until)
                done = os.path.exists(path + ".until") and ts(open(path + ".until").read().strip()) >= want
                if not (os.path.exists(path) and (end < now - timedelta(minutes=30) or done)):
                    events, token = [], None
                    while True:
                        args = ["cloudtrail", "lookup-events", "--lookup-attributes",
                                f"AttributeKey=EventName,AttributeValue={name}", "--start-time", iso(max(day, since)),
                                "--end-time", iso(min(end, until)), "--max-results", "50", "--no-paginate"]
                        if token:
                            args += ["--next-token", token]
                        page = aws(region, *args)
                        events += [json.loads(e["CloudTrailEvent"]) for e in page.get("Events", [])]
                        token = page.get("NextToken")
                        if not token:
                            break
                    write_json(path, events)
                    if want < now - timedelta(minutes=30):
                        open(path + ".until", "w").write(iso(want))
                    print(f"trail {region} {name} {day:%Y-%m-%d} {len(events)}", file=sys.stderr, flush=True)
                day = end

    with concurrent.futures.ThreadPoolExecutor(len(REGIONS)) as pool:
        list(pool.map(region_days, REGIONS))


def launches(d):
    """Every instance a RunInstances event started, with what EC2 answered about it."""
    out = {}
    for path in glob.glob(f"{d}/ct/*/RunInstances/*.json"):
        region = path.split("/")[-3]
        for e in json.load(open(path)):
            if e.get("errorCode"):
                continue
            req = e.get("requestParameters") or {}
            lt = req.get("launchTemplate") or {}
            for i in ((e.get("responseElements") or {}).get("instancesSet") or {}).get("items", []):
                tags = {t["key"]: t["value"] for t in (i.get("tagSet") or {}).get("items", [])}
                out[i["instanceId"]] = {
                    "id": i["instanceId"], "region": region, "type": i["instanceType"],
                    "az": i["placement"]["availabilityZone"], "lifecycle": i.get("instanceLifecycle") or "on-demand",
                    "start": iso(ts(i.get("launchTime") or e["eventTime"])), "event_time": e["eventTime"],
                    "tags": tags, "lt": [lt.get("launchTemplateId"), lt.get("version")],
                    "bdm": [b["ebs"] for b in (req.get("blockDeviceMapping") or {}).get("items", []) if b.get("ebs")],
                    "by": (e.get("userIdentity") or {}).get("arn", ""),
                }
    return out


def fetch_templates(d, ms):
    for region, lt, ver in {(m["region"], *m["lt"]) for m in ms.values() if m["lt"][0]}:
        path = f"{d}/lt/{region}/{lt}-{ver}.json"
        if os.path.exists(path):
            continue
        try:
            v = aws(region, "ec2", "describe-launch-template-versions", "--launch-template-id", lt, "--versions",
                    str(ver))["LaunchTemplateVersions"][0]["LaunchTemplateData"]
        except subprocess.CalledProcessError:
            v = {"missing": True}  # a deleted template: the report falls back to the set's configured disk
        write_json(path, v)


def fetch_prices(d, ms, since, until):
    """Spot price history for every (type, zone) a spot machine ran in, from a day before the window (the price in
    force at the window's start was set before it) to its end."""
    pairs = collections.defaultdict(set)
    for m in ms.values():
        if m["lifecycle"] == "spot":
            pairs[m["region"]].add((m["type"], m["az"]))
    for region, ps in pairs.items():
        rows = []
        types = sorted({t for t, _ in ps})
        for k in range(0, len(types), 20):
            token = None
            while True:
                args = ["ec2", "describe-spot-price-history", "--product-descriptions", "Linux/UNIX", "--start-time",
                        iso(since - timedelta(days=1)), "--end-time", iso(until), "--instance-types", *types[k:k + 20],
                        "--max-results", "1000", "--no-paginate"]
                if token:
                    args += ["--next-token", token]
                page = aws(region, *args)
                rows += [{"type": r["InstanceType"], "az": r["AvailabilityZone"], "t": r["Timestamp"],
                          "price": float(r["SpotPrice"])} for r in page.get("SpotPriceHistory", [])]
                token = page.get("NextToken")
                if not token:
                    break
        write_json(f"{d}/spot/{region}.json", rows)
        print(f"spot {region} {len(rows)} price points for {len(ps)} pairs", file=sys.stderr)


def fetch_ondemand(d, ms):
    path = f"{d}/ondemand.json"
    have = json.load(open(path)) if os.path.exists(path) else {}
    for region, typ in sorted({(m["region"], m["type"]) for m in ms.values()}):
        key = f"{region}/{typ}"
        if key in have:
            continue
        page = aws("us-east-1", "pricing", "get-products", "--service-code", "AmazonEC2", "--filters",
                   f"Type=TERM_MATCH,Field=instanceType,Value={typ}", f"Type=TERM_MATCH,Field=regionCode,Value={region}",
                   "Type=TERM_MATCH,Field=operatingSystem,Value=Linux", "Type=TERM_MATCH,Field=tenancy,Value=Shared",
                   "Type=TERM_MATCH,Field=preInstalledSw,Value=NA", "Type=TERM_MATCH,Field=capacitystatus,Value=Used")
        for p in page.get("PriceList", []):
            p = json.loads(p)
            for term in p["terms"].get("OnDemand", {}).values():
                for dim in term["priceDimensions"].values():
                    have[key] = {"usd_hour": float(dim["pricePerUnit"]["USD"]),
                                 "vcpu": int(p["product"]["attributes"].get("vcpu", 0))}
    write_json(path, have)


def fetch_ce(d, since, until):
    start, end = since.date(), (until + timedelta(days=1)).date()
    rows, token = [], None
    while True:
        args = ["ce", "get-cost-and-usage", "--time-period", f"Start={start},End={end}", "--granularity", "DAILY",
                "--metrics", "UnblendedCost", "UsageQuantity", "--group-by", "Type=DIMENSION,Key=USAGE_TYPE"]
        if token:
            args += ["--next-page-token", token]
        page = aws("us-east-1", *args)
        rows += page.get("ResultsByTime", [])
        token = page.get("NextPageToken")
        if not token:
            break
    write_json(f"{d}/ce.json", rows)


def fetch_listener(d, since, until):
    """The listener's register lines (a machine's runner registered) and ask lines with verdict end (a machine told to
    power off holding no runner, which leaves no CloudTrail event)."""
    events, token = [], None
    while True:
        args = ["logs", "filter-log-events", "--log-group-name", "/superci-pull/listener", "--start-time",
                str(int(since.timestamp() * 1000)), "--end-time", str(int(until.timestamp() * 1000)),
                "--filter-pattern", '{ $.msg = "register" || $.msg = "ask" }', "--no-paginate"]
        if token:
            args += ["--next-token", token]
        page = aws("us-east-1", *args)
        for e in page.get("events", []):
            try:
                events.append(json.loads(e["message"]))
            except json.JSONDecodeError:
                pass
        token = page.get("nextToken")
        if not token:
            break
    write_json(f"{d}/listener.json", events)
    print(f"listener lines {len(events)}", file=sys.stderr)


def fetch_live(d):
    """Instances EC2 lists now: the running ones end at `now`, and those that ended in the last hour or so carry
    their end in StateTransitionReason when no TerminateInstances event names them (a shutdown from inside)."""
    out = {}
    for region in REGIONS:
        for r in aws(region, "ec2", "describe-instances").get("Reservations", []):
            for i in r["Instances"]:
                out[i["InstanceId"]] = {"state": i["State"]["Name"], "reason": i.get("StateTransitionReason", ""),
                                        "type": i["InstanceType"], "az": i["Placement"]["AvailabilityZone"],
                                        "lifecycle": i.get("InstanceLifecycle") or "on-demand",
                                        "start": i["LaunchTime"], "region": region,
                                        "tags": {t["Key"]: t["Value"] for t in i.get("Tags", [])}}
    write_json(f"{d}/live.json", {"at": iso(datetime.now(UTC)), "instances": out})


def check_account():
    got = aws("us-east-1", "sts", "get-caller-identity").get("Account")
    if got != ACCOUNT:
        sys.exit(f"refusing: profile {PROFILE} answers as account {got}, not ci-runners {ACCOUNT}")


def cmd_fetch(a):
    check_account()
    since, until = ts(a.since), ts(a.until) if a.until else datetime.now(UTC)
    fetch_trail(a.dir, since, until)
    ms = launches(a.dir)
    print(f"launches {len(ms)}", file=sys.stderr)
    fetch_live(a.dir)
    live = json.load(open(f"{a.dir}/live.json"))["instances"]
    for iid, i in live.items():  # launched before the window, still running: its type and zone come from describe
        if iid not in ms:
            ms[iid] = {"id": iid, **i, "lt": [None, None]}
    fetch_templates(a.dir, ms)
    fetch_prices(a.dir, ms, since, until)
    fetch_ondemand(a.dir, ms)
    fetch_ce(a.dir, since, until)
    fetch_listener(a.dir, since, until)




def gh(path):
    for attempt in range(4):  # the jobs API answers a 502 now and then under load
        r = subprocess.run(["gh", "api", "-H", "Accept: application/vnd.github+json", path], capture_output=True, text=True)
        if r.returncode == 0:
            return json.loads(r.stdout)
        time.sleep(2 ** attempt)
    raise RuntimeError(f"gh api {path}: {r.stderr.strip()[:200]}")


def cmd_jobs(a):
    repo = a.repo
    os.makedirs(f"{a.dir}/gh", exist_ok=True)
    for q in a.queries:
        page = 1
        while True:
            sep = "&" if "?" in q else "?"
            runs = gh(f"repos/{repo}/actions/{q}{sep}per_page=100&page={page}")["workflow_runs"]
            for run in runs:
                path = f"{a.dir}/gh/{run['id']}.json"
                if os.path.exists(path) or run["status"] != "completed":
                    continue
                jobs, p = [], 1
                while True:
                    chunk = gh(f"repos/{repo}/actions/runs/{run['id']}/attempts/{run['run_attempt']}/jobs?per_page=100&page={p}")["jobs"]
                    jobs += chunk
                    if len(chunk) < 100:
                        break
                    p += 1
                keep = [{k: j.get(k) for k in ("id", "name", "runner_name", "labels", "created_at", "started_at",
                                                 "completed_at", "conclusion", "run_id")}
                        for j in jobs if (j.get("runner_name") or "").startswith("superci-")]
                write_json(path, {"run": run["id"], "workflow": run["name"], "jobs": keep})
                print(f"run {run['id']} {run['name']}: {len(keep)} of {len(jobs)} jobs on superci", file=sys.stderr)
            if len(runs) < 100:
                break
            page += 1




class Prices:
    """The spot price in force for a (type, zone) at any time, from its change points."""

    def __init__(self, rows):
        self.points = collections.defaultdict(list)
        for r in rows:
            self.points[(r["type"], r["az"])].append((ts(r["t"]), r["price"]))
        for v in self.points.values():
            v.sort()

    def cost(self, typ, az, start, end):
        pts = self.points.get((typ, az))
        if not pts:
            return None
        times = [p[0] for p in pts]
        i = max(0, bisect.bisect_right(times, start) - 1)
        total, t = 0.0, start
        while t < end:
            seg_end = min(end, times[i + 1]) if i + 1 < len(pts) else end
            total += pts[i][1] * max(0.0, (seg_end - t).total_seconds()) / 3600
            t, i = max(t, seg_end), i + 1
        return total


def load(d):
    ms = launches(d)
    live_doc = json.load(open(f"{d}/live.json"))
    live, live_at = live_doc["instances"], ts(live_doc["at"])
    for iid, i in live.items():
        if iid not in ms:
            ms[iid] = {"id": iid, **i, "lt": [None, None], "by": "before window"}
    ends = {}
    for path in glob.glob(f"{d}/ct/*/TerminateInstances/*.json") + glob.glob(f"{d}/ct/*/StopInstances/*.json"):
        for e in json.load(open(path)):
            if e.get("errorCode"):
                continue
            for i in ((e.get("requestParameters") or {}).get("instancesSet") or {}).get("items", []):
                t = ts(e["eventTime"])
                if i["instanceId"] not in ends or t < ends[i["instanceId"]][0]:
                    ends[i["instanceId"]] = (t, "terminate-event")
    for m in ms.values():
        m["start_t"] = ts(m["start"])
        if m["id"] in ends:
            m["end_t"], m["end_src"] = ends[m["id"]]
        elif live.get(m["id"], {}).get("state") in ("running", "pending"):
            m["end_t"], m["end_src"] = live_at, "running-now"
        elif m["id"] in live and (r := re.search(r"\((\d{4}-\d\d-\d\d \d\d:\d\d:\d\d) GMT\)", live[m["id"]]["reason"])):
            m["end_t"], m["end_src"] = datetime.fromisoformat(r.group(1) + "+00:00"), "describe-reason"
        else:
            m["end_t"], m["end_src"] = None, "unknown"
    return ms


def disk(d, m, sets):
    for e in m.get("bdm") or []:  # a launch without a template names its disk in the request
        return e.get("volumeSize", 8), e.get("throughput", 125), e.get("iops", 3000), "request"
    lt = m["lt"]
    path = f"{d}/lt/{m['region']}/{lt[0]}-{lt[1]}.json"
    if lt[0] and os.path.exists(path):
        for b in json.load(open(path)).get("BlockDeviceMappings", []):
            e = b.get("Ebs") or {}
            if e:
                return e.get("VolumeSize", 8), e.get("Throughput", 125), e.get("Iops", 3000), "template"
    s = sets.get(m["tags"].get("superci-pull-set"))
    if s:
        return s, 250, 3000, "set config"
    return 8, 125, 3000, "default 8 GB"


def price_machines(d, ms, sets):
    spot = Prices([r for p in glob.glob(f"{d}/spot/*.json") for r in json.load(open(p))])
    od = json.load(open(f"{d}/ondemand.json")) if os.path.exists(f"{d}/ondemand.json") else {}
    for m in ms.values():
        if not m["end_t"]:
            m["usd"] = None
            continue
        secs = max(MIN_SECONDS, (m["end_t"] - m["start_t"]).total_seconds())
        end = m["start_t"] + timedelta(seconds=secs)
        listed = od.get(f"{m['region']}/{m['type']}", {})
        m["vcpu"] = listed.get("vcpu") or 0
        compute, src = None, "spot history"
        if m["lifecycle"] == "spot":
            compute = spot.cost(m["type"], m["az"], m["start_t"], end)
        if compute is None:
            compute, src = listed.get("usd_hour", 0) * secs / 3600, "on-demand list"
            if m["lifecycle"] == "spot":
                src = "spot, no history: on-demand list"
        gb, mbps, iops, dsrc = disk(d, m, sets)
        hours = secs / 3600
        ebs = (gb * GP3_GB_MONTH + max(0, mbps - 125) * GP3_MBPS_MONTH + max(0, iops - 3000) * GP3_IOPS_MONTH) * hours / GB_MONTH_HOURS
        m.update(seconds=secs, compute_usd=compute, ebs_usd=ebs, ipv4_usd=IPV4_HOUR * hours, usd=compute + ebs + IPV4_HOUR * hours,
                 price_src=src, disk_src=dsrc, disk_gb=gb)


def kind(m):
    tags = m["tags"]
    if tags.get("purpose") == "superci-pull" or "superci-pull-set" in tags:
        return "pull:" + tags.get("superci-pull-deployment", "?")
    if "aws:autoscaling:groupName" in tags:
        return "asg:" + tags["aws:autoscaling:groupName"]
    for k in tags:
        if k.startswith("superci"):
            return "push:" + k
    return "other:" + (m.get("by", "").split("/")[-2] if "/" in m.get("by", "") else "?")


def join_jobs(d, ms):
    """A superci runner's name carries its instance id: superci-4cpu-i-0123..., or superci-2cpu-i-0123...-d453 in the
    load test's sets."""
    jobs = {}
    for path in glob.glob(f"{d}/gh/*.json"):
        doc = json.load(open(path))
        for j in doc["jobs"]:
            m = re.search(r"-(i-[0-9a-f]{8,17})(?:-|$)", j["runner_name"] or "")
            if m:
                jobs[m.group(1)] = dict(j, workflow=doc["workflow"])
    regs, asks = {}, {}
    if os.path.exists(f"{d}/listener.json"):
        for r in json.load(open(f"{d}/listener.json")):
            if r.get("machine") and r.get("msg") == "register" and r.get("level") == "INFO":
                regs.setdefault(r["machine"], ts(r["time"]))
            if r.get("machine") and r.get("msg") == "ask" and r.get("verdict") == "end":
                asks.setdefault(r["machine"], ts(r["time"]))
    for m in ms.values():
        m["registered_t"] = regs.get(m["id"])
        m["job"] = jobs.get(m["id"])
    # A machine that powers itself off leaves no TerminateInstances event: its end is its job's completion plus the
    # teardown measured on machines whose end EC2 recorded, or its ask for a runner told to end plus that teardown.
    teardown = [(m["end_t"] - ts(m["job"]["completed_at"])).total_seconds() for m in ms.values()
                if m["end_src"] == "terminate-event" and m["job"] and m["job"].get("completed_at")]
    tear = timedelta(seconds=max(0.0, statistics.median(teardown)) if teardown else 5)
    for m in ms.values():
        if m["end_t"]:
            continue
        if m["job"] and m["job"].get("completed_at"):
            m["end_t"], m["end_src"] = ts(m["job"]["completed_at"]) + tear, "job end + teardown p50"
        elif m["id"] in asks:
            m["end_t"], m["end_src"] = asks[m["id"]] + tear, "ask end + teardown p50"
    return jobs, tear.total_seconds(), len(teardown)


def phases(m, j):
    """Seconds of a machine's paid life in each phase. The boundaries clamp to the paid interval, so a clock skew of a
    second between EC2, the listener and GitHub cannot make a phase negative."""
    if not m.get("end_t"):
        return {}
    s, e = m["start_t"], m["end_t"]
    clamp = lambda t: min(max(t, s), e) if t else None
    reg = clamp(m.get("registered_t"))
    js, je = (clamp(ts(j["started_at"])), clamp(ts(j["completed_at"]))) if j and j.get("started_at") else (None, None)
    sec = lambda a, b: max(0.0, (b - a).total_seconds())
    if js:
        boot_end = reg or js
        return {"boot": sec(s, boot_end), "wait": sec(boot_end, js), "job": sec(js, je), "teardown": sec(je, e)}
    if reg:
        return {"boot": sec(s, reg), "pool": sec(reg, e)}
    return {"never registered": sec(s, e)}


def ce_lines(d):
    out = collections.defaultdict(dict)
    if not os.path.exists(f"{d}/ce.json"):
        return out
    for r in json.load(open(f"{d}/ce.json")):
        for g in r.get("Groups", []):
            out[r["TimePeriod"]["Start"]][g["Keys"][0]] = (float(g["Metrics"]["UnblendedCost"]["Amount"]),
                                                           float(g["Metrics"]["UsageQuantity"]["Amount"]))
    return out


def family(usage_type):
    u = re.sub(r"^(USE1|USE2|USW2|US)-", "", usage_type)
    if u.startswith("SpotUsage"):
        return "spot"
    if u.startswith("BoxUsage"):
        return "on-demand"
    if u.startswith("EBS:VolumeUsage") or u.startswith("EBS:VolumeP-"):
        return "ebs"
    if "PublicIPv4" in u:
        return "ipv4"
    return "other: " + u.split(":")[0]


def split_by_day(m):
    """A machine's dollars split over the UTC days its life spans, pro rata by seconds (CE's DAILY grain)."""
    out, t, e = {}, m["start_t"], m["start_t"] + timedelta(seconds=m["seconds"])
    while t < e:
        nxt = min(e, (t + timedelta(days=1)).replace(hour=0, minute=0, second=0, microsecond=0))
        out[t.strftime("%Y-%m-%d")] = (nxt - t).total_seconds() / m["seconds"]
        t = nxt
    return out


def pct(xs, q):
    xs = sorted(xs)
    return xs[min(len(xs) - 1, int(q * len(xs)))] if xs else 0


def cmd_report(a):
    d = a.dir
    cfg_path = os.path.join(os.path.dirname(__file__), "..", "..", "config.json")
    sets = {k: v["disk_gb"] for k, v in json.load(open(cfg_path))["sizes"].items()} if os.path.exists(cfg_path) else {}
    ms = load(d)
    jobs, tear, ntear = join_jobs(d, ms)
    price_machines(d, ms, sets)
    for m in ms.values():
        m["phases"] = phases(m, m["job"])
    priced = [m for m in ms.values() if m.get("usd") is not None]
    unknown = [m for m in ms.values() if m.get("usd") is None]
    print(f"# Machine cost from EC2 records ({d})\n")
    print(f"{len(ms)} instances launched or listed; {len(priced)} priced; {len(unknown)} with no end found "
          f"(no TerminateInstances event and no longer listed: they shut down from inside, or ended before EC2's "
          f"listing; their cost is left out and counted here).\n")
    print("Ends from: " + ", ".join(f"{k} {v}" for k, v in collections.Counter(m["end_src"] for m in ms.values()).most_common())
          + f". Teardown p50 {tear:.0f} s (job completion to TerminateInstances, n={ntear}).\n")
    srcs = collections.Counter(m["price_src"] for m in priced)
    print("Compute priced by: " + ", ".join(f"{k} {v}" for k, v in srcs.most_common()) + "\n")

    print("## By kind\n")
    print("| kind | machines | machine-hours | vCPU-hours | compute $ | EBS $ | IPv4 $ | total $ | $ per vCPU-hour all-in |")
    print("|---|---:|---:|---:|---:|---:|---:|---:|---:|")
    by = collections.defaultdict(list)
    for m in priced:
        by[kind(m)].append(m)
    for k, xs in sorted(by.items(), key=lambda kv: -sum(m["usd"] for m in kv[1])):
        h = sum(m["seconds"] for m in xs) / 3600
        vh = sum(m["seconds"] * m["vcpu"] for m in xs) / 3600
        tot = sum(m["usd"] for m in xs)
        print(f"| {k} | {len(xs)} | {h:.1f} | {vh:.1f} | {sum(m['compute_usd'] for m in xs):.2f} | "
              f"{sum(m['ebs_usd'] for m in xs):.2f} | {sum(m['ipv4_usd'] for m in xs):.2f} | {tot:.2f} | "
              f"{tot / vh if vh else 0:.4f} |")

    pull = [m for m in priced if kind(m).startswith("pull:")]
    print("\n## superci-pull machines: where the paid seconds went\n")
    print("| set | machines | with a job | phase seconds: boot / wait / job / teardown / pool / never registered | "
          "paid ÷ job (machines with a job) | $ per job, machine all-in, p50 / mean |")
    print("|---|---:|---:|---|---:|---|")
    per_set = collections.defaultdict(list)
    for m in pull:
        per_set[m["tags"].get("superci-pull-set", "?")].append(m)
    for s, xs in sorted(per_set.items()):
        ph = collections.Counter()
        for m in xs:
            ph.update(m["phases"])
        withjob = [m for m in xs if m["phases"].get("job")]
        paid = sum(m["seconds"] for m in withjob)
        jobsec = sum(m["phases"]["job"] for m in withjob)
        usd = [m["usd"] for m in withjob]
        cells = " / ".join(f"{ph.get(k, 0):.0f}" for k in ("boot", "wait", "job", "teardown", "pool", "never registered"))
        print(f"| {s} | {len(xs)} | {len(withjob)} | {cells} | {paid / jobsec if jobsec else 0:.2f} | "
              f"{pct(usd, .5):.4f} / {sum(usd) / len(usd) if usd else 0:.4f} |")
    withjob = [m for m in pull if m["phases"].get("job")]
    if withjob:
        print("\nOne job's machine, p50 seconds: " + ", ".join(
            f"{k} {pct([m['phases'].get(k, 0) for m in withjob], .5):.0f}" for k in ("boot", "wait", "job", "teardown")) +
              f"; paid life p50 {pct([m['seconds'] for m in withjob], .5):.0f} s, n={len(withjob)}.")
        tot_paid = sum(m["usd"] for m in pull)
        job_share = sum(m["usd"] * m["phases"]["job"] / m["seconds"] for m in withjob) / tot_paid if tot_paid else 0
        print(f"Of all superci-pull dollars (${tot_paid:.2f}), {100 * job_share:.1f}% paid for seconds a job ran.")
        rates = [m["compute_usd"] / (m["seconds"] / 3600) / m["vcpu"] for m in withjob if m["vcpu"]]
        print(f"Compute $ per vCPU-hour over these machines: p50 {pct(rates, .5):.4f}, p10 {pct(rates, .1):.4f}, "
              f"p90 {pct(rates, .9):.4f}; types {collections.Counter(m['type'] for m in withjob).most_common(6)}.")

    print("\n## Machines that ran a job, by workflow and by how the job found its machine\n")
    print("A job found a pool machine when its machine waited 10 s or more registered before the job started; else "
          "the machine was launched for it. Overhead is paid seconds beyond the job.\n")
    print("| workflow | found | n | p50 s: boot / wait / job / teardown | overhead s p50 / mean | paid ÷ job | "
          "$ per job p50 | all-in $ per vCPU-hour p50 |")
    print("|---|---|---:|---|---|---:|---:|---:|")
    groups = collections.defaultdict(list)
    for m in withjob:
        groups[((m["job"] or {}).get("workflow", "?")[:40], "pool" if m["phases"]["wait"] >= 10 else "launched")].append(m)
    for (wf, how), xs in sorted(groups.items()):
        over = [m["seconds"] - m["phases"]["job"] for m in xs]
        ph = " / ".join(f"{pct([m['phases'][k] for m in xs], .5):.0f}" for k in ("boot", "wait", "job", "teardown"))
        rate = [m["usd"] / (m["seconds"] * m["vcpu"] / 3600) for m in xs if m["vcpu"]]
        print(f"| {wf} | {how} | {len(xs)} | {ph} | {pct(over, .5):.0f} / {sum(over) / len(over):.0f} | "
              f"{sum(m['seconds'] for m in xs) / sum(m['phases']['job'] for m in xs):.2f} | "
              f"{pct([m['usd'] for m in xs], .5):.4f} | {pct(rate, .5):.4f} |")

    print("\n## Derived against Cost Explorer, by UTC day (CE is daily and posts with a lag of up to a day)\n")
    ce = ce_lines(d)
    derived = collections.defaultdict(collections.Counter)
    for m in priced:
        for day, f in split_by_day(m).items():
            fam = "spot" if m["lifecycle"] == "spot" and m["price_src"] == "spot history" else (
                "spot" if m["lifecycle"] == "spot" else "on-demand")
            derived[day][fam] += m["compute_usd"] * f
            derived[day]["ebs"] += m["ebs_usd"] * f
            derived[day]["ipv4"] += m["ipv4_usd"] * f
    print("| day | family | derived $ | Cost Explorer $ | gap $ | gap % |")
    print("|---|---|---:|---:|---:|---:|")
    for day in sorted(set(derived) | set(ce)):
        fams = collections.Counter()
        for u, (usd, _) in ce.get(day, {}).items():
            fams[family(u)] += usd
        for fam in sorted(set(fams) | set(derived[day])):
            dv, cv = derived[day].get(fam, 0), fams.get(fam, 0)
            if dv < 0.01 and cv < 0.01:
                continue
            gap = dv - cv
            print(f"| {day} | {fam} | {dv:.2f} | {cv:.2f} | {gap:+.2f} | {100 * gap / cv if cv else float('nan'):+.0f}% |")

    if a.json:
        rows = [{k: (iso(v) if isinstance(v, datetime) else v) for k, v in m.items() if k not in ("job",)} |
                {"kind": kind(m), "job": (m["job"] or {}).get("name"), "job_id": (m["job"] or {}).get("id"),
                 "workflow": (m["job"] or {}).get("workflow")} for m in ms.values()]
        write_json(a.json, rows)


def main():
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = p.add_subparsers(dest="cmd", required=True)
    f = sub.add_parser("fetch")
    f.add_argument("dir")
    f.add_argument("--since", default="2026-11-23T00:00:00Z")
    f.add_argument("--until")
    j = sub.add_parser("jobs")
    j.add_argument("dir")
    j.add_argument("queries", nargs="+")
    j.add_argument("--repo", default="Authentic-Intelligence/megpt-mono")
    r = sub.add_parser("report")
    r.add_argument("dir")
    r.add_argument("--json")
    a = p.parse_args()
    {"fetch": cmd_fetch, "jobs": cmd_jobs, "report": cmd_report}[a.cmd](a)


if __name__ == "__main__":
    main()
