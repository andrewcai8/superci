#!/usr/bin/env python3
"""Prints the load-test workflow. Each push to BRANCH is one wave: for every size, a matrix of that many legs on
superci-<cpu>cpu, and CACHE legs on superci-2cpu that save and restore one key with actions/cache (the first wave saves
it, later waves of the same region restore it) and print their runtime token's issuer and key id, never the token. A
push whose commit message contains "burst" runs instead one matrix of BURST legs on BURST_LABEL. Each leg prints its
machine (type, vCPUs, memory) and sleeps 30-90 s. Push-triggered on one branch only; no secrets, no checkout.

  workflow.py BRANCH 2=9,4=27,8=9,16=1,32=1 [BURST] [BURST_LABEL] [CACHE] > .github/workflows/superci-load.yml

A real hour's arrivals (`gate replay-trace`) are driven bucket by bucket instead: the legs of one bucket, each sleeping the
run time its real job had (capped at MAX_SLEEP seconds), as matrices of at most 256 legs a label.

  workflow.py --trace TRACE.json BRANCH BUCKET [MAX_SLEEP] > .github/workflows/superci-load.yml
  workflow.py --schedule TRACE.json                    prints "<bucket> <offset seconds> <legs>" for each non-empty bucket
  workflow.py --need TRACE.json [MAX_SLEEP]            prints "<label> <identities>": the most legs of the label at once
  workflow.py --check-need TRACE.json MAX_SLEEP LABELS reads "<label> <idle identities>" lines (identity.sh labels) on
                                                       stdin, prints the need of each dispatch size LABELS names (comma
                                                       separated) against them, exits 1 if one is short; a scale set's
                                                       label has no identities and is not checked
"""
import json
import sys

MATRIX_LIMIT = 256
# A leg holds its identity from its arrival to its end. Its arrival is its bucket's push, which queues the bucket's legs at
# once. A plane that meets gate 2's queue-wait p90 (10 s) has a leg's machine start it within that, so a leg is counted
# for its sleep and 10 s more.
NEED_HOLD_S = 10


def trace_buckets(trace):
    """{bucket index: [leg]} of a trace file's jobs, by the push of the replay that carries each."""
    out = {}
    for leg in trace["jobs"]:
        out.setdefault(int(leg["t"] // trace["bucket_s"]), []).append(leg)
    return out


def trace_workflow(branch, legs, max_sleep=900):
    """The workflow of one bucket: for each runner label, its legs in matrices of MATRIX_LIMIT, each sleeping its real duration."""
    out = ["name: superci-load", f"# One bucket of a real hour's arrivals. Push-triggered on {branch} only; no secrets, no checkout.", "on:", "  push:",
           f"    branches: [{branch}]", "", "permissions: {}", "", "jobs:"]
    by_label = {}
    for leg in legs:
        by_label.setdefault(leg["label"], []).append(min(max_sleep, int(round(leg["duration_s"]))))
    for label, durations in sorted(by_label.items()):
        cpu = label.removeprefix("superci-").removesuffix("cpu")
        for n, i in enumerate(range(0, len(durations), MATRIX_LIMIT)):
            chunk = durations[i:i + MATRIX_LIMIT]
            out += [f"  cpu{cpu}_{n + 1}:", f"    runs-on: {label}", f"    timeout-minutes: {max(chunk) // 60 + 3}", "    strategy:", "      fail-fast: false", "      matrix:",
                    "        include: [" + ", ".join(f"{{i: {k + 1}, d: {d}}}" for k, d in enumerate(chunk)) + "]", "    steps:", "      - name: sleep",
                    f'        run: echo "runner=$RUNNER_NAME label={label} i=${{{{ matrix.i }}}}"; sleep ${{{{ matrix.d }}}}']
    return "\n".join(out)


def trace_need(trace, max_sleep=900, hold_s=NEED_HOLD_S):
    """{label: the most of its legs at once}, each leg from its bucket's push (its arrival: a push queues its bucket's legs
    together) to its (capped) sleep's end and hold_s more. A leg that ends when another arrives is not counted with it."""
    edges = {}
    for leg in trace["jobs"]:
        at = int(leg["t"] // trace["bucket_s"]) * trace["bucket_s"]
        end = at + min(max_sleep, int(round(leg["duration_s"]))) + hold_s
        edges.setdefault(leg["label"], []).extend([(at, 1), (end, -1)])
    out = {}
    for label, es in edges.items():
        n = peak = 0
        for _, d in sorted(es):  # at one instant an end (-1) sorts before an arrival (+1)
            n += d
            peak = max(peak, n)
        out[label] = peak
    return out


def check_need(need, have_lines, dispatch):
    """(lines to print, every dispatch size has its need) for the trace's need against "<label> <count>" lines. A label that is
    not a dispatch size is a scale set's, which registers a runner a machine and has no identities to count."""
    have = {}
    for line in have_lines:
        parts = line.split()
        if len(parts) == 2 and parts[1].isdigit():
            have[parts[0]] = int(parts[1])
    lines, ok = [], True
    for label, n in sorted(need.items()):
        if label not in dispatch:
            lines.append(f"identities labelled {label}: a scale set's label, no identities: not checked")
            continue
        got = have.get(label, 0)
        ok = ok and got >= n
        lines.append(f"identities labelled {label}: {got} idle, the trace's peak holds {n}: {'ok' if got >= n else 'SHORT'}")
    return lines, ok


if sys.argv[1] in ("--need", "--check-need"):
    need = trace_need(json.load(open(sys.argv[2])), int(sys.argv[3]) if len(sys.argv) > 3 else 900)
    if sys.argv[1] == "--need":
        for label, n in sorted(need.items()):
            print(label, n)
        sys.exit(0)
    lines, ok = check_need(need, sys.stdin.read().splitlines(), set(filter(None, (sys.argv[4] if len(sys.argv) > 4 else "").split(","))))
    print("\n".join(lines))
    sys.exit(0 if ok else 1)
if sys.argv[1] == "--schedule":
    trace = json.load(open(sys.argv[2]))
    for idx, legs in sorted(trace_buckets(trace).items()):
        print(idx, idx * trace["bucket_s"], len(legs))
    sys.exit(0)
if sys.argv[1] == "--trace":
    trace = json.load(open(sys.argv[2]))
    print(trace_workflow(sys.argv[3], trace_buckets(trace)[int(sys.argv[4])], int(sys.argv[5]) if len(sys.argv) > 5 else 900))
    sys.exit(0)

branch, wave = sys.argv[1], [tuple(map(int, kv.split("="))) for kv in sys.argv[2].split(",")]
burst = int(sys.argv[3]) if len(sys.argv) > 3 else 200
burst_label = sys.argv[4] if len(sys.argv) > 4 else "superci-4cpu"
cache = int(sys.argv[5]) if len(sys.argv) > 5 else 0


def job(name, label, legs, cond):
    return [
        f"  {name}:",
        f"    if: {cond}",
        f"    runs-on: {label}",
        "    timeout-minutes: 10",
        "    strategy:",
        "      fail-fast: false",
        "      matrix:",
        f"        i: [{', '.join(str(i) for i in range(1, legs + 1))}]",
        "    steps:",
        "      - name: sleep",
        "        run: |",
        "          T=$(curl -s -m 2 -X PUT -H 'X-aws-ec2-metadata-token-ttl-seconds: 60' http://169.254.169.254/latest/api/token)",
        "          type=$(curl -s -m 2 -H \"X-aws-ec2-metadata-token: $T\" http://169.254.169.254/latest/meta-data/instance-type)",
        f'          echo "runner=$RUNNER_NAME label={label} type=$type nproc=$(nproc) mem_gib=$(free -g | awk \'/Mem:/ {{print $2}}\') i=${{{{ matrix.i }}}}"',
        "          sleep $((30 + RANDOM % 61))",
    ]


out = [
    "name: superci-load",
    "# Load test for superci-pull: each push is one wave of jobs over every runner size, and a push whose message",
    "# contains \"burst\" is one matrix burst instead. Push-triggered on one branch only; no secrets, no checkout.",
    "on:",
    "  push:",
    f"    branches: [{branch}]",
    "",
    "permissions: {}",
    "",
    "jobs:",
]
wave_cond = "${{ !contains(github.event.head_commit.message, 'burst') }}"
for cpu, legs in wave:
    if legs:
        out += job(f"cpu{cpu}", f"superci-{cpu}cpu", legs, wave_cond)
if cache:
    out += [
        "  cache:",
        f"    if: {wave_cond}",
        "    runs-on: superci-2cpu",
        "    timeout-minutes: 10",
        "    strategy:",
        "      fail-fast: false",
        "      matrix:",
        f"        i: [{', '.join(str(i) for i in range(1, cache + 1))}]",
        "    steps:",
        "      - name: the files to cache",
        "        run: |",
        "          mkdir -p /tmp/superci-cache",
        '          echo "saved by run ${{ github.run_id }} on $RUNNER_NAME" > /tmp/superci-cache/stamp',
        "          head -c 8000000 /dev/urandom > /tmp/superci-cache/blob",
        "      - name: the runtime token's issuer and key id",
        "        uses: actions/github-script@v7",
        "        with:",
        "          script: |",
        "            const [h, p] = process.env.ACTIONS_RUNTIME_TOKEN.split('.').slice(0, 2).map(s => JSON.parse(Buffer.from(s, 'base64url').toString()))",
        "            core.info(`runtime token: iss ${p.iss}, kid ${h.kid}, alg ${h.alg}`)",
        "      - name: cache",
        "        id: cache",
        "        uses: actions/cache@v4",
        "        with:",
        "          path: /tmp/superci-cache",
        f"          key: superci-load-{branch.replace('/', '-')}-v1",
        "      - name: what the cache gave",
        "        run: |",
        '          echo "cache-hit=${{ steps.cache.outputs.cache-hit }}"',
        "          cat /tmp/superci-cache/stamp",
    ]
out += job("burst", burst_label, burst, "${{ contains(github.event.head_commit.message, 'burst') }}")
print("\n".join(out))
