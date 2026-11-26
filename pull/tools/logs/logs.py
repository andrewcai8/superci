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
BEFORE_S, AFTER_S = 3 * 3600, 900
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
    rec = next((e for e in events if e["ev"] == "job.record" and (e.get("run") == job["run_id"]) and e.get("name") == job["name"]), None)
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
        out.append(f"  {e['t'] - queued:+9.1f} s  {T.iso(e['t'])}  {e['ev']:<16} {fmt_fields(e)}")
    if not events:
        out.append("  none: no listener event names this run or machine in the window")
    if lines:
        out.append("")
        out.append("the listener's other log lines naming the machine:")
        out += [f"  {l}" for l in lines]
    out.append("")
    if summary:
        out.append("host metrics (sampled on the machine during the job):")
        out += [f"  {k}: {v}" for k, v in summary.items()]
    else:
        out.append("host metrics: none uploaded for this job (the machine's sampler writes them at job end)")
    return "\n".join(out)


def read_summary(profile, job_id):
    """The job's host metrics summary from S3, or None."""
    try:
        return T.aws(profile, "s3", "cp", f"s3://{METRICS_BUCKET}/jobs/{job_id}/summary.json", "-") or None
    except SystemExit:
        return None


def machine_lines(profile, inst, lo, hi):
    """The listener's log lines that are not events and name the machine (a job lost with its machine, a spot notice, an error)."""
    rows = T.insights(profile, "us-east-1", T.LISTENER_GROUP,
                      f'fields @timestamp, @message | filter @message like "{inst}" and msg != "tl" | sort @timestamp asc', lo, hi, limit=50)
    return [f"{r['@timestamp']}  {r['@message'][:400]}" for r in rows]


def main(argv=None):
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("url")
    p.add_argument("--profile", default=os.environ.get("AWS_PROFILE", "ci-runners"))
    p.add_argument("--cache", default=os.path.expanduser("~/.cache/superci-timeline"))
    p.add_argument("--no-metrics", action="store_true", help="skip the S3 read of the host metrics summary")
    a = p.parse_args(argv)
    repo, run, job_id = parse_url(a.url)
    job = T.GitHub(repo, T.Cache(a.cache)).job(job_id)
    run = job.get("run_id") or run
    inst = T.instance_of(job.get("runner_name"))
    queued = T.ts(job["created_at"])
    lo = queued - BEFORE_S
    hi = (T.ts(job.get("completed_at")) or time.time()) + AFTER_S
    events = sorted(T.listener_events(a.profile, lo, hi, where=event_query(run, inst)), key=lambda e: e["t"])
    # a machine's events before its job came to it are its own (warm pool); a job's own events are the run's, of its job only
    events = [e for e in events if e.get("inst") == inst and inst or e.get("run") == run and e.get("name") in (None, job["name"])]
    lines = machine_lines(a.profile, inst, lo, hi) if inst else []
    summary = None if a.no_metrics else read_summary(a.profile, job_id)
    print(render(job, events, lines, summary, queued))
    return 0


if __name__ == "__main__":
    sys.exit(main())
