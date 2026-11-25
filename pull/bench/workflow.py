#!/usr/bin/env python3
"""Prints the load-test workflow: JOBS jobs of LEGS matrix legs each, on one runner label, each leg sleeping 60 s
plus 0-60 s. Push-triggered on one branch only; no secrets, no checkout.

  workflow.py BRANCH LABEL JOBS LEGS > .github/workflows/superci-load.yml
"""
import sys

branch, label, jobs, legs = sys.argv[1], sys.argv[2], int(sys.argv[3]), int(sys.argv[4])
legs_list = ", ".join(str(i) for i in range(1, legs + 1))
out = [
    "name: superci-load",
    "# Load test for superci-pull, the runner scale set on EC2. Push-triggered on one branch only; no secrets, no checkout.",
    "on:",
    "  push:",
    f"    branches: [{branch}]",
    "",
    "permissions: {}",
    "",
    "jobs:",
]
for j in range(jobs):
    name = f"leg-{chr(ord('a') + j)}"
    out += [
        f"  {name}:",
        f"    runs-on: {label}",
        "    timeout-minutes: 5",
        "    strategy:",
        "      fail-fast: false",
        "      matrix:",
        f"        i: [{legs_list}]",
        "    steps:",
        "      - name: sleep",
        "        run: |",
        f'          echo "runner=$RUNNER_NAME job={name} i=${{{{ matrix.i }}}}"',
        "          sleep $((60 + RANDOM % 61))",
        "          echo done",
    ]
print("\n".join(out))
