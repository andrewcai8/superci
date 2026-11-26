#!/usr/bin/env python3
"""What moving CI from Blacksmith to superci saves, per UTC day, netted.

  python3 pull/tools/cost/cost.py [--days 8] [--end 2026-11-26] [--json]

Reads only, with the ci-runners profile: it takes that profile's credentials itself, drops every other AWS variable of the
environment (a host shell may hold production keys), and stops unless the caller is account 243891825325.
"""
import argparse, datetime as dt, json, os, subprocess, sys

ACCOUNT = "243891825325"
REGION = "us-east-1"
LOG_GROUP = "/superci-pull/listener"
SIZES = (2, 4, 8, 16, 32)
BLACKSMITH_USD_PER_VCPU_MIN = 0.002  # x64 Linux list price: cost_usd / billing_minutes of `blacksmith usage`, 2026-11-10 to 26
DISCOUNT = 0.5  # YC discount on the invoice, through DISCOUNT_UNTIL; audit B read it, no script can
DISCOUNT_UNTIL = "2026-11-30"

JOBS_QUERY = (
    'filter msg="tl" and (ev="job.started" or ev="job.completed") '
    "| stats min(t_ms) as t by job, set, ev, eid "
    "| stats min(t) as s, max(t) as e, count_distinct(ev) as kinds by job, set | filter kinds=2 "
    "| stats count(*) as jobs, sum(e-s)/60000 as minutes by datefloor(fromMillis(s), 1d) as day, set"
)


def vcpu_of(set_name):
    """vCPUs of a scale set from its name: superci-<cpu>cpu or superci-<cpu>cpu-<k>. The listener has also logged
    superci-41cpu and superci-81cpu (the 4 and 8 vCPU sets, a set's index in the digits), so a number that is no size
    is read as a size followed by an index. None when no reading gives a size."""
    if not (set_name.startswith("superci-") and "cpu" in set_name):
        return None
    digits = set_name[len("superci-"):].split("cpu")[0]
    if not digits.isdigit():
        return None
    if int(digits) in SIZES:
        return int(digits)
    for n in sorted(SIZES, reverse=True):
        if digits.startswith(str(n)):
            return n
    return None


def invoice_factor(day, discount=DISCOUNT, until=DISCOUNT_UNTIL):
    return discount if day <= until else 1.0


def coverage(day, since):
    """"none" for a day that ended before the listener's first job event (`since`, ISO UTC), "partial" for the day it began, else "full"."""
    if since is None or day > since[:10]:
        return "full" if since else "none"
    return "partial" if day == since[:10] else "none"


def build_rows(days, ce, blacksmith, superci, since=None, price=BLACKSMITH_USD_PER_VCPU_MIN):
    """ce: {day: {service: usd}}; blacksmith: {day: list usd}; superci: {day: {set: (jobs, wall minutes)}}; since: when the
    listener's job events begin, None if it has none. Avoided Blacksmith spend is what the superci jobs' vCPU-minutes would
    have cost at Blacksmith's list price; a day before `since` has no job-minutes to price, so it has no avoided and no net."""
    rows = []
    for day in days:
        services = ce.get(day, {})
        cost = sum(services.values())
        jobs = minutes = vcpu_minutes = unmapped = 0.0
        for name, (j, m) in superci.get(day, {}).items():
            jobs += j
            minutes += m
            v = vcpu_of(name)
            if v is None:
                unmapped += m
            else:
                vcpu_minutes += v * m
        cov = coverage(day, since)
        avoided = vcpu_minutes * price if cov != "none" else None
        factor = invoice_factor(day)
        rows.append({
            "day": day,
            "superci_usd": cost,
            "top_services": sorted(services.items(), key=lambda kv: -kv[1])[:3],
            "blacksmith_list_usd": blacksmith.get(day),
            "jobs": int(jobs),
            "job_minutes": minutes,
            "vcpu_minutes": vcpu_minutes,
            "unmapped_minutes": unmapped,
            "job_events": cov,
            "avoided_list_usd": avoided,
            "net_list_usd": None if avoided is None else avoided - cost,
            "net_invoice_usd": None if avoided is None else avoided * factor - cost,
            "invoice_factor": factor,
            "superci_usd_per_job_minute": cost / minutes if minutes and cov == "full" else None,
        })
    return rows


def aws_env(profile):
    out = subprocess.run(["aws", "--profile", profile, "configure", "export-credentials", "--format", "process"],
                         capture_output=True, text=True)
    if out.returncode:
        sys.exit(f"aws {profile} credentials: {out.stderr.strip()[:200]}")
    c = json.loads(out.stdout)
    env = {k: v for k, v in os.environ.items() if not k.startswith("AWS_")}
    env.update(AWS_ACCESS_KEY_ID=c["AccessKeyId"], AWS_SECRET_ACCESS_KEY=c["SecretAccessKey"], AWS_DEFAULT_REGION=REGION)
    if c.get("SessionToken"):
        env["AWS_SESSION_TOKEN"] = c["SessionToken"]
    who = subprocess.run(["aws", "sts", "get-caller-identity", "--query", "Account", "--output", "text"], env=env, capture_output=True, text=True)
    if who.stdout.strip() != ACCOUNT:
        sys.exit(f"refusing: the credentials are account {who.stdout.strip() or '?'}, not {ACCOUNT}")
    return env


def aws(env, *args):
    out = subprocess.run(["aws", *args, "--output", "json"], env=env, capture_output=True, text=True)
    if out.returncode:
        sys.exit(f"aws {args[0]} {args[1]}: {out.stderr.strip()[:300]}")
    return json.loads(out.stdout)


def read_ce(env, start, end):
    d = aws(env, "ce", "get-cost-and-usage", "--time-period", f"Start={start},End={end}", "--granularity", "DAILY",
            "--metrics", "UnblendedCost", "--group-by", "Type=DIMENSION,Key=SERVICE")
    return {r["TimePeriod"]["Start"]: {g["Keys"][0]: float(g["Metrics"]["UnblendedCost"]["Amount"]) for g in r["Groups"]}
            for r in d["ResultsByTime"]}


def read_blacksmith(start, end):
    out = subprocess.run(["blacksmith", "usage", "--start-time", f"{start}T00:00:00Z", "--end-time", f"{end}T00:00:00Z",
                          "--breakdown-by", "day", "--format", "json"], capture_output=True, text=True)
    try:
        d = json.loads(out.stdout[out.stdout.index("{"):])
    except ValueError:
        sys.exit(f"blacksmith usage: {(out.stderr or out.stdout).strip()[:300]}")
    return {r["date"]: r["cost_usd"] for r in d["daily"]}


FIRST_QUERY = 'filter msg="tl" and ev="job.completed" | stats min(t_ms) as first'


def run_query(env, t0, t1, query):
    import time
    qid = aws(env, "logs", "start-query", "--region", REGION, "--log-group-name", LOG_GROUP, "--start-time", str(t0),
              "--end-time", str(t1), "--query-string", query)["queryId"]
    for _ in range(60):
        r = aws(env, "logs", "get-query-results", "--region", REGION, "--query-id", qid)
        if r["status"] in ("Complete", "Failed", "Cancelled", "Timeout"):
            break
        time.sleep(2)
    if r["status"] != "Complete":
        sys.exit(f"logs query {r['status']}")
    return [{c["field"]: c["value"] for c in row} for row in r["results"]]


def read_jobs(env, start, end):
    t0 = int(dt.datetime.fromisoformat(start).replace(tzinfo=dt.timezone.utc).timestamp())
    t1 = int(dt.datetime.fromisoformat(end).replace(tzinfo=dt.timezone.utc).timestamp())
    jobs = {}
    for f in run_query(env, t0, t1, JOBS_QUERY):
        day = f["day"][:10]
        try:
            dt.date.fromisoformat(day)
        except ValueError:
            sys.exit(f"logs query gave a day key that is not a date: {f['day']!r}")
        jobs.setdefault(day, {})[f["set"]] = (int(f["jobs"]), float(f["minutes"]))
    first = run_query(env, t1 - 30 * 86400, t1, FIRST_QUERY)
    since = dt.datetime.fromtimestamp(int(first[0]["first"]) / 1000, dt.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ") if first and first[0].get("first") else None
    return jobs, since


def fmt(v, spec):
    return "-" if v is None else format(v, spec)


def render(rows, today):
    na = lambda v, spec: "n/a" if v is None else format(v, spec)
    head = f"{'day (UTC)':<11}{'superci':>9}{'blacksmith':>11}{'jobs':>7}{'job-min':>9}{'vCPU-min':>10}{'avoided':>9}{'net(list)':>10}{'net(inv)':>10}{'$/job-min':>10}  top services"
    lines = [head]
    for r in rows:
        mark = "*" if r["day"] == today else "~" if r["job_events"] == "partial" else " "
        top = ", ".join(f"{k.replace('Amazon ', '').replace(' - Compute', '')} {v:.0f}" for k, v in r["top_services"])
        lines.append(f"{r['day']:<10}{mark}{r['superci_usd']:>9.2f}{fmt(r['blacksmith_list_usd'], '11.0f')}{r['jobs']:>7}{r['job_minutes']:>9.0f}"
                     f"{r['vcpu_minutes']:>10.0f}{na(r['avoided_list_usd'], '9.2f'):>9}{na(r['net_list_usd'], '10.2f'):>10}{na(r['net_invoice_usd'], '10.2f'):>10}"
                     f"{na(r['superci_usd_per_job_minute'], '10.4f'):>10}  {top}")
    t = lambda k: sum(r[k] for r in rows)
    lines.append(f"{'total':<11}{t('superci_usd'):>9.2f}{(format(t('blacksmith_list_usd'), '.0f') if all(r['blacksmith_list_usd'] is not None for r in rows) else '-'):>11}"
                 f"{t('jobs'):>7}{t('job_minutes'):>9.0f}{t('vcpu_minutes'):>10.0f}{sum(r['avoided_list_usd'] or 0 for r in rows):>9.2f}{sum(r['net_list_usd'] or 0 for r in rows):>10.2f}{sum(r['net_invoice_usd'] or 0 for r in rows):>10.2f}")
    unmapped = t("unmapped_minutes")
    lines += ["", "* today, partial: Cost Explorer lags and the day is not over.",
              "~ the listener's job events begin during this day, so job-min and avoided cover only its later hours while superci covers all 24; net is not a day's net. n/a: the day is before the first job event, so its job-minutes are unreadable here.",
              "total's avoided and net add the days with job events, the `~` day among them (its superci is the whole day); its superci and blacksmith add every day, blacksmith `-` if a day has no row.",
              "superci = Cost Explorer UnblendedCost, ci-runners, every record type. blacksmith = list USD of the whole installation (`blacksmith usage`), after the move.",
              f"avoided = superci vCPU-minutes x {BLACKSMITH_USD_PER_VCPU_MIN} USD, Blacksmith's x64 Linux list price per billing minute. net = avoided - superci.",
              f"net(inv) counts avoided at {DISCOUNT:.0%} through {DISCOUNT_UNTIL}, the YC discount on the invoice (audit B), then at list.",
              "job-min = wall minutes from job.started to job.completed in the listener log; a job counts on its start day. $/job-min is fully loaded: idle pool, boot, disks, IPs."]
    if unmapped:
        lines.append(f"{unmapped:.1f} job-minutes ran on a set whose name gives no vCPU count and are not in avoided.")
    return "\n".join(lines)


def main(argv=None):
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--days", type=int, default=8, help="how many UTC days, ending at --end")
    p.add_argument("--end", default=dt.datetime.now(dt.timezone.utc).strftime("%Y-%m-%d"), help="last day, UTC (default today)")
    p.add_argument("--profile", default="ci-runners")
    p.add_argument("--json", action="store_true")
    a = p.parse_args(argv)
    last = dt.date.fromisoformat(a.end)
    days = [(last - dt.timedelta(days=i)).isoformat() for i in range(a.days - 1, -1, -1)]
    stop = (last + dt.timedelta(days=1)).isoformat()
    env = aws_env(a.profile)
    jobs, since = read_jobs(env, days[0], stop)
    rows = build_rows(days, read_ce(env, days[0], stop), read_blacksmith(days[0], stop), jobs, since)
    print(json.dumps(rows, indent=1) if a.json else render(rows, dt.datetime.now(dt.timezone.utc).strftime("%Y-%m-%d")))


if __name__ == "__main__":
    main()
