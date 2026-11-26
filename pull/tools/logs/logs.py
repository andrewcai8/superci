#!/usr/bin/env python3
"""superci logs: everything superci knows about one job, from the listener's own events.

  python3 -I pull/tools/logs/logs.py https://github.com/OWNER/REPO/actions/runs/RUN/job/JOB [--profile ci-runners] [--no-metrics]

Prints the job's record (queue wait, wall time, conclusion, the machine and its cost), then every listener and machine event of the
job and of the machine that ran it in time order (launch, boot marks, the runner's registration, the job, the end), the listener's other
log lines naming the machine, and the job's host metrics summary when the machine's sampler uploaded one.

It makes one GitHub call (the job's REST record, to turn the URL's numeric job id into the run and the machine; cached) and reads everything
else from CloudWatch Logs and S3 of the superci account, with `--profile ci-runners`, read only.
"""
import argparse
import json
import os
import re
import sys
import time

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "timeline"))
import timeline as T  # noqa: E402

URL = re.compile(r"github\.com/([\w.-]+/[\w.-]+)/actions/runs/(\d+)/job/(\d+)")
METRICS_BUCKET = "superci-pull-timeline-243891825325"
BEFORE_S, AFTER_S = 6 * 3600, 900  # a warm-pool machine is launched hours before the job it takes
INSTANCE = re.compile(r"i-[0-9a-f]{8,17}")
SHOWN_LAST = ("set", "inst", "eid", "boot", "v", "msg", "t", "t_ms", "time", "level", "ev", "offset_ms")


def parse_url(url):
    """(repo, run id, job id) of a GitHub job URL, or exit."""
    m = URL.search(url)
    if not m:
        sys.exit(f"not a job URL (https://github.com/OWNER/REPO/actions/runs/RUN/job/JOB): {url}")
    return m.group(1), int(m.group(2)), int(m.group(3))


def event_query(run, inst):
    """The Logs Insights filter for the job's own events (by run) and its machine's (by instance id: its boot marks name no run)."""
    return 'msg = "tl" and (run = %d%s)' % (run, f' or inst = "{inst}"' if inst else "")


def at(e):
    """An event's time on the listener's clock: a machine's marks carry the offset their post measured."""
    return T.on_listener_clock(e) if e.get("offset_ms") is not None or e.get("skew_ms") is not None else e["t"]


def fmt_fields(e):
    out = []
    for k, v in e.items():
        if k in SHOWN_LAST or v in ("", None, [], {}) or k.endswith("_time") and k != "finish_time":
            continue
        out.append(f"{k}={json.dumps(v) if isinstance(v, (list, dict)) else v}")
    return " ".join(out)


def render(job, events, lines, summary, queued):
    """The report as text. job: the REST job; events: the tl events in time order; lines: the listener's other log lines naming the machine."""
    out = [f"job {job['id']}  {job['name']}  run {job['run_id']}  {job.get('conclusion') or job.get('status')}",
           f"  runner {job.get('runner_name') or '-'}  queued {T.iso(queued)}"]
    inst = T.instance_of(job.get("runner_name"))
    rec = next((e for e in events if e["ev"] == "job.record" and e.get("run") == job["run_id"] and e.get("name") == job["name"] and (not inst or e.get("inst") == inst)), None)
    if rec:
        usd = f"  cost ${rec['usd']:.4f} (${rec.get('busy_usd', 0):.4f} while the job ran)" if rec.get("usd") is not None else ""
        times = "queue and wall unknown (a completion after a listener restart)" if rec.get("partial") else f"queue {rec.get('queue_ms', 0) / 1000:.1f} s  wall {rec.get('wall_ms', 0) / 1000:.1f} s"
        out.append(f"  record: {rec.get('conclusion')}  {times}  set {rec.get('set')}  {rec.get('type', '-')} {rec.get('lifecycle', '')} {rec.get('az', '')}"
                   f"  {'warm' if rec.get('warm') else 'cold'}{usd}")
    else:
        out.append("  record: none (the job has not finished, or finished before the listener wrote records)")
    out.append("")
    out.append("events (time from the job's creation; the machine's marks are corrected to the listener's clock):")
    for e in events:
        out.append(f"  {at(e) - queued:+9.1f} s  {T.iso(at(e))}  {e['ev']:<16} {fmt_fields(e)}")
    if not events:
        out.append("  none: no listener event names this run or machine in the window")
    if lines:
        out.append("")
        out.append("the listener's other log lines naming the machine:")
        out += [f"  {l}" for l in lines]
    out.append("")
    if summary:
        out.append("host metrics (sampled on the machine during the job):" if "unreadable" not in summary else "host metrics:")
        out += [f"  {k}: {v}" for k, v in summary.items()]
    else:
        out.append("host metrics: none uploaded for this job (the machine's sampler writes them at job end)")
    return "\n".join(out)


def read_summary(profile, inst):
    """The machine's host metrics summary from S3 (a machine runs one job): a dict, None when none was uploaded, or {"unreadable": why}.
    The machine wrote it, and a job is root there, so only a JSON object is taken."""
    try:
        got = T.aws(profile, "s3", "cp", f"s3://{METRICS_BUCKET}/machines/{inst}/summary.json", "-")
    except SystemExit as e:
        why = str(e)
        return None if any(k in why for k in ("404", "NoSuchKey", "Not Found")) else {"unreadable": why[:200]}
    except ValueError:
        return {"unreadable": "not JSON"}
    return got if isinstance(got, dict) and got else None if not got else {"unreadable": "not a JSON object"}


def query(profile, text, lo, hi, limit):
    """Rows of one Logs Insights query, at most `limit`: a hard cap, never split (T.insights halves a window that fills its limit)."""
    q = T.aws(profile, "logs", "start-query", "--region", "us-east-1", "--log-group-name", T.LISTENER_GROUP, "--start-time", str(int(lo)), "--end-time", str(int(hi)),
              "--query-string", f"{text} | limit {limit}")["queryId"]
    for _ in range(120):
        time.sleep(1)
        r = T.aws(profile, "logs", "get-query-results", "--region", "us-east-1", "--query-id", q)
        if r["status"] in ("Complete", "Failed", "Cancelled", "Timeout"):
            break
    if r["status"] != "Complete":
        raise SystemExit(f"logs query {r['status']}")
    return [{f["field"]: f["value"] for f in row} for row in r["results"]]


def machine_lines(profile, inst, lo, hi):
    """The listener's log lines that are not events and name the machine (a job lost with its machine, a spot notice, an error): the first 50."""
    rows = query(profile, f'fields @timestamp, @message | filter @message like "{inst}" and msg != "tl" | sort @timestamp asc', lo, hi, 50)
    return [f"{r['@timestamp']}  {r['@message'][:400]}" for r in rows]


def select(events, run, name, inst):
    """The events that are this job's: its machine's (by instance id, the boot marks name no run) and, of the run's, those of this job's name that
    belong to this attempt. A re-run keeps the run and the name, so with a machine known only its machine's events and the latest assignment
    before its machine took the job count: the assignment names no machine."""
    if not inst:
        return [e for e in events if e.get("run") == run and e.get("name") == name]
    mine = [e for e in events if e.get("inst") == inst]
    took = min((e["t"] for e in mine if e["ev"] == "job.started"), default=None)
    assigned = [e for e in events if e.get("run") == run and e.get("name") == name and not e.get("inst") and e["ev"] in ("job.assigned", "job.available", "job.acquired")
                and (took is None or e["t"] <= took)]
    return mine + assigned[-1:]


def main(argv=None):
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("url")
    p.add_argument("--profile", default=os.environ.get("AWS_PROFILE", "ci-runners"))
    p.add_argument("--cache", default=os.path.expanduser("~/.cache/superci-timeline"))
    p.add_argument("--no-metrics", action="store_true", help="skip the S3 read of the host metrics summary")
    a = p.parse_args(argv)
    repo, run, job_id = parse_url(a.url)
    job = T.GitHub(repo, T.Cache(a.cache)).api(f"actions/jobs/{job_id}", keep=False)  # not cached: a job read while it waits would stay as it was
    run = job.get("run_id") or run
    inst = T.instance_of(job.get("runner_name"))
    if inst and not INSTANCE.fullmatch(inst):
        sys.exit(f"the job's runner {job.get('runner_name')!r} names no EC2 instance")
    queued = T.ts(job["created_at"])
    lo = queued - BEFORE_S
    hi = (T.ts(job.get("completed_at")) or time.time()) + AFTER_S
    events = T.listener_events(a.profile, lo, hi, where=event_query(run, inst))
    events = sorted(select(events, run, job["name"], inst), key=at)
    lines = machine_lines(a.profile, inst, lo, hi) if inst else []
    summary = None if a.no_metrics or not inst else read_summary(a.profile, inst)
    print(render(job, events, lines, summary, queued))
    return 0


if __name__ == "__main__":
    sys.exit(main())
