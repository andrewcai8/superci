#!/usr/bin/env python3
"""gate spawn: does a job's first exec run as fast on superci as on Blacksmith?

A probe job (spawn-probe.yml, dispatched on a load branch once per fleet) prints its runner, then one line per measure:
  runner=blacksmith-4vcpu-ubuntu-2404-abc uptime=3.87
  SPAWN node_first=41:0 python_first=8:0 docker_exec=12:0 bun_first=30:0 git_first=7:0
  SPAWN docker_daemon=813:0 true500=113:0 node50=537:0 coldread400=1517:0
Each measure is milliseconds and the command's exit code. A measure whose command failed, or whose binary is absent, is not
a sample.

- First exec (node, python, docker's CLI): every probe job is held to Blacksmith's median x 1.25 + 20 ms. The slowest job
  decides, because one cold machine in three is the failure, not the median.
- Throughput (500 fork-execs of `true`, 50 runs of `node -e 0`): superci's median against the same bound.
- A gated measure needs MIN_JOBS samples on each fleet, or it is MISSING and fails.
- bun, git, docker's first daemon call and a cold read of 400 large files are printed, not gated.

  spawn.py <superci run id> <blacksmith run id> [--repo owner/name]     reads the runs' logs through gh
  spawn.py --logs <superci log file> <blacksmith log file>               the same from saved `gh run view --log` output
"""
import argparse
import re
import statistics
import subprocess
import sys

FIRST = ("node_first", "python_first", "docker_exec")
THROUGHPUT = ("true500", "node50")
SHOWN = ("bun_first", "git_first", "docker_daemon", "coldread400")
MIN_JOBS = 5
ANSI = re.compile(r"\x1b\[[0-9;]*m")


def parse(text):
    """Per probe job, {measure: ms} of the commands that succeeded, and its runner. A `gh run view --log` line is
    job, step and then a timestamp and the output: the probe's echoed script is output too, and its measures have no
    digits after the =, so it parses to nothing. A measure seen again starts a new attempt of that job."""
    jobs, current = [], {}
    for raw in text.splitlines():
        parts = ANSI.sub("", raw.rstrip("\r")).split("\t", 2)
        if len(parts) < 3:
            continue
        job, out = parts[0], parts[2].split(" ", 1)[-1]
        j = current.get(job)
        if out.startswith("runner="):
            j = current[job] = {"job": job, "runner": out.split()[0][len("runner="):], "ms": {}}
            jobs.append(j)
        elif out.startswith("SPAWN ") and j is not None:
            for k, ms, rc in re.findall(r"\b(\w+)=(\d+):(\d+)\b", out):
                if k in j["ms"]:
                    j = current[job] = {"job": job, "runner": j["runner"], "ms": {}}
                    jobs.append(j)
                if rc == "0":
                    j["ms"][k] = int(ms)
    return jobs


def verdict(superci, blacksmith):
    rows, ok = [], True
    for k in FIRST + THROUGHPUT + SHOWN:
        s = [j["ms"][k] for j in superci if k in j["ms"]]
        b = [j["ms"][k] for j in blacksmith if k in j["ms"]]
        gated = k not in SHOWN
        if len(s) < MIN_JOBS or len(b) < MIN_JOBS:
            state = "MISSING" if gated else "-"
            ok = ok and not gated
            rows.append((k, None, None, None, f"{state} ({len(s)} and {len(b)} samples)"))
            continue
        bm = statistics.median(b)
        bound = bm * 1.25 + 20
        got = max(s) if k in FIRST else statistics.median(s)
        state = ("PASS" if got <= bound else "FAIL") if gated else "-"
        ok = ok and state != "FAIL"
        rows.append((k, got, bm, bound, state))
    return ok, rows


def fleet(jobs):
    names = {j["runner"].split("-", 1)[0] for j in jobs}
    return names.pop() if len(names) == 1 else "mixed"


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
    if fleet(s) != "superci" or fleet(b) != "blacksmith":
        print(f"refusing: the first run's runners are {fleet(s)} and the second's {fleet(b)}; want superci then blacksmith")
        return 2
    ok, rows = verdict(s, b)
    print(f"probe jobs: superci {len(s)}, blacksmith {len(b)} (each gated measure needs {MIN_JOBS} a fleet)")
    print(f"{'measure':<14}{'superci':>10}{'blacksmith p50':>16}{'bound':>9}  verdict (first exec: superci's slowest job; else its median)")
    for k, got, bm, bound, state in rows:
        f = lambda x: "-" if x is None else f"{x:g}"
        print(f"{k:<14}{f(got):>10}{f(bm):>16}{f(bound):>9}  {state}")
    print("spawn: PASS" if ok else "spawn: FAIL")
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
