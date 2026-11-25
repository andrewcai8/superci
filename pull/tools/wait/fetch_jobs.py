"""Fetch every job of the given run ids (stdin, TSV with the id first) to JSONL on stdout, via gh api; a run whose fetch fails is retried, then reported on stderr."""
import json, subprocess, sys, time
from concurrent.futures import ThreadPoolExecutor

KEEP = ("run_id", "id", "name", "labels", "runner_name", "created_at", "started_at", "completed_at", "conclusion", "run_attempt", "workflow_name", "head_branch")

def fetch(run):
    for attempt in range(4):
        out = subprocess.run(["gh", "api", "--paginate", f"repos/Authentic-Intelligence/megpt-mono/actions/runs/{run}/jobs?per_page=100&filter=all",
                              "--jq", ".jobs[] | tojson"], capture_output=True, text=True)
        if out.returncode == 0:
            return [{k: j.get(k) for k in KEEP} for j in map(json.loads, out.stdout.splitlines())]
        time.sleep(2 * (attempt + 1))
    print(f"run {run}: {out.stderr.strip()[:200]}", file=sys.stderr)
    return []

runs = [l.split("\t")[0] for l in sys.stdin if l.strip()]
with ThreadPoolExecutor(int(sys.argv[1]) if len(sys.argv) > 1 else 8) as ex:
    for rows in ex.map(fetch, runs):
        for r in rows:
            print(json.dumps(r))
