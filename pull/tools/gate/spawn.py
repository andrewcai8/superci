#!/usr/bin/env python3
"""gate spawn: does a job's first exec run as fast on superci as on Blacksmith?

A probe job (spawn-probe.yml, dispatched on a load branch once per fleet) prints one line per measure:
  SPAWN node_first=41 node_second=13 git_first=7 python_first=8 docker_first=813 bash_first=1
  SPAWN true500=113
  SPAWN node50=537
  SPAWN coldread400=1517
Each measure is milliseconds. A row passes when superci's median is at most Blacksmith's x 1.25 + 5 ms, over at least 3
probe jobs a fleet. coldread400 (400 large files outside the binaries jobs start) is printed and not gated.

  spawn.py <superci run id> <blacksmith run id> [--repo owner/name]     reads the runs' logs through gh (two calls a run)
  spawn.py --logs <superci log file> <blacksmith log file>               the same from saved `gh run view --log` output
"""
import argparse
import re
import statistics
import subprocess
import sys

GATED = ("node_first", "git_first", "python_first", "docker_first", "true500", "node50")
SHOWN = ("node_second", "bash_first", "coldread400")
MIN_JOBS = 3


def parse(text):
    """Per probe job (by the log's job column), {measure: ms}. The probe's own script, echoed in the log, has no digits after
    a measure's =, so it parses to nothing."""
    jobs = {}
    for line in text.splitlines():
        if "SPAWN " not in line:
            continue
        job = line.split("\t", 1)[0]
        for k, v in re.findall(r"\b(\w+)=(\d+)\b", line.split("SPAWN ", 1)[1]):
            jobs.setdefault(job, {})[k] = int(v)
    return list(jobs.values())


def verdict(superci, blacksmith):
    rows, ok = [], len(superci) >= MIN_JOBS and len(blacksmith) >= MIN_JOBS
    for k in GATED + SHOWN:
        s = [j[k] for j in superci if k in j]
        b = [j[k] for j in blacksmith if k in j]
        if not s or not b:
            rows.append((k, None, None, None, "MISSING" if k in GATED else "-"))
            ok = ok and k not in GATED
            continue
        sm, bm = statistics.median(s), statistics.median(b)
        bound = bm * 1.25 + 5
        state = ("PASS" if sm <= bound else "FAIL") if k in GATED else "-"
        ok = ok and state != "FAIL"
        rows.append((k, sm, bm, bound, state))
    return ok, rows


def logs(run, repo):
    return subprocess.run(["gh", "run", "view", str(run), "-R", repo, "--log"], capture_output=True, text=True, check=True).stdout


def main(argv=None):
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("superci")
    p.add_argument("blacksmith")
    p.add_argument("--repo", default="Authentic-Intelligence/megpt-mono")
    p.add_argument("--logs", action="store_true", help="the two arguments are saved log files")
    a = p.parse_args(argv)
    read = (lambda f: open(f).read()) if a.logs else (lambda r: logs(r, a.repo))
    s, b = parse(read(a.superci)), parse(read(a.blacksmith))
    ok, rows = verdict(s, b)
    print(f"probe jobs: superci {len(s)}, blacksmith {len(b)} (need {MIN_JOBS} each)")
    print(f"{'measure':<14}{'superci p50':>12}{'blacksmith p50':>16}{'bound':>9}  verdict")
    for k, sm, bm, bound, state in rows:
        f = lambda x: "-" if x is None else f"{x:g}"
        print(f"{k:<14}{f(sm):>12}{f(bm):>16}{f(bound):>9}  {state}")
    print("spawn: PASS" if ok else "spawn: FAIL")
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
