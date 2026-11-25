#!/usr/bin/env python3
"""Prints the load-test workflow. Each push to BRANCH is one wave: for every size, a matrix of that many legs on
superci-<cpu>cpu, and CACHE legs on superci-2cpu that save and restore one key with actions/cache (the first wave saves
it, later waves of the same region restore it) and print their runtime token's issuer and key id, never the token. A
push whose commit message contains "burst" runs instead one matrix of BURST legs on BURST_LABEL. Each leg prints its
machine (type, vCPUs, memory) and sleeps 30-90 s. Push-triggered on one branch only; no secrets, no checkout.

  workflow.py BRANCH 2=9,4=27,8=9,16=1,32=1 [BURST] [BURST_LABEL] [CACHE] > .github/workflows/superci-load.yml
"""
import sys

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
