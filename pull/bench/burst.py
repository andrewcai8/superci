#!/usr/bin/env python3
"""Replays real pushes on a set: pushes arrive as a Poisson process at RATE a minute for MINUTES, each one run of a
matrix of as many legs as a real push brought jobs of the size (drawn from PUSHES, pushes-4cpu.json), on LABEL. Each
push is a commit whose tree holds only the load workflow, so no other workflow of megpt-mono runs; the first push
creates BRANCH, each later one is its child. Each leg sleeps 60-90 s. Run inside any checkout of megpt-mono; it
writes git objects and pushes, nothing else. Prints each push's time and legs.

  burst.py BRANCH LABEL RATE MINUTES [PUSHES] [SEED]
"""
import json, os, random, subprocess, sys, time

branch, label, rate, minutes = sys.argv[1], sys.argv[2], float(sys.argv[3]), float(sys.argv[4])
pushes = json.load(open(sys.argv[5] if len(sys.argv) > 5 else os.path.join(os.path.dirname(__file__), "pushes-4cpu.json")))["jobs_per_push"]
rng = random.Random(int(sys.argv[6]) if len(sys.argv) > 6 else 1)


def git(*args, stdin=None, env=None):
    return subprocess.run(["git", *args], input=stdin, capture_output=True, text=True, check=True, env=env).stdout.strip()


def workflow(legs):
    return "\n".join([
        "name: superci-load",
        "# Load test for superci-pull: one push is one run of a matrix as large as a real push's jobs of the size.",
        "on:",
        "  push:",
        f"    branches: [{branch}]",
        "permissions: {}",
        "jobs:",
        "  burst:",
        f"    runs-on: {label}",
        "    timeout-minutes: 10",
        "    strategy:",
        "      fail-fast: false",
        "      matrix:",
        f"        i: [{', '.join(str(i) for i in range(1, legs + 1))}]",
        "    steps:",
        "      - run: echo \"runner=$RUNNER_NAME i=${{ matrix.i }}\"; sleep $((60 + RANDOM % 31))",
        "",
    ])


def commit(legs, parent, message):
    blob = git("hash-object", "-w", "--stdin", stdin=workflow(legs))
    wf = git("mktree", stdin=f"100644 blob {blob}\tsuperci-load.yml\n")
    gh = git("mktree", stdin=f"040000 tree {wf}\tworkflows\n")
    root = git("mktree", stdin=f"040000 tree {gh}\t.github\n")
    return git("commit-tree", root, *(["-p", parent] if parent else []), "-m", message)


start, head, t, k = time.time(), None, 0.0, 0
while True:
    t += rng.expovariate(rate / 60)
    if t > minutes * 60:
        break
    legs = rng.choice(pushes)
    k += 1
    c = commit(legs, head, f"superci-load: push {k}, {legs} legs")
    time.sleep(max(0.0, start + t - time.time()))
    subprocess.run(["git", "push", "-q", "origin", f"{c}:refs/heads/{branch}"], check=True, capture_output=True)
    head = c
    print(f"{time.strftime('%H:%M:%S', time.gmtime())} push {k} legs {legs}", flush=True)
