"""List every megpt-mono workflow run created in [start, start+minutes) as TSV (id, name, event, created, attempt), in 5-minute slices so no query passes GitHub's 1,000-result cap."""
import json, subprocess, sys
from datetime import datetime, timedelta, timezone

start = datetime.fromisoformat(sys.argv[1]).replace(tzinfo=timezone.utc)
minutes = int(sys.argv[2])
for k in range(0, minutes, 5):
    a, b = start + timedelta(minutes=k), start + timedelta(minutes=k + 5) - timedelta(seconds=1)
    out = subprocess.run(["gh", "api", "--paginate", "-X", "GET", "repos/Authentic-Intelligence/megpt-mono/actions/runs",
                          "-f", f"created={a:%Y-%m-%dT%H:%M:%SZ}..{b:%Y-%m-%dT%H:%M:%SZ}", "-f", "per_page=100",
                          "--jq", ".workflow_runs[] | [.id, .name, .event, .created_at, .run_attempt] | @tsv"], capture_output=True, text=True, check=True)
    rows = out.stdout.splitlines()
    print(f"{a:%H:%M} {len(rows)}", file=sys.stderr)
    if len(rows) >= 1000:
        print("slice at the 1,000 cap", file=sys.stderr)
    print("\n".join(rows))
