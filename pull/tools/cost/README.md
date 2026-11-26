# cost

What moving CI from Blacksmith to superci saves, per UTC day, netted: the superci account's bill against the Blacksmith
spend its jobs would have cost. Python 3 standard library, `aws` and `blacksmith` on the path. It reads only.

```
python3 pull/tools/cost/cost.py [--days 8] [--end 2026-11-26] [--json]
python3 -m unittest pull/tools/cost/test_cost.py     # the arithmetic
```

It takes the `ci-runners` profile's credentials itself (`--profile`), ignores every other `AWS_*` variable (a host shell may
hold production keys), and exits unless `aws sts get-caller-identity` is account 243891825325. Run it with `python3 -I`
from outside the directory you do not trust. A run makes one Cost Explorer call (0.01 USD), two CloudWatch Logs Insights
queries (under 0.01 USD) and one `blacksmith usage`. It makes no GitHub call.

| Column | Source |
| --- | --- |
| superci | Cost Explorer `get-cost-and-usage`, DAILY, UnblendedCost, grouped by SERVICE, account 243891825325, every record type. Total and top three services. The same read as audit B's 479.1 (Nov 25) and 67.1 (Nov 24) |
| blacksmith | `blacksmith usage --breakdown-by day`, list USD of the whole installation, the same read as audit B (Nov 10 is 5,157.16 in both). It is what Blacksmith still bills after the move |
| jobs, job-min | the listener's `tl` events in `/superci-pull/listener`: `job.started` to `job.completed` of each job, on its start day |
| vCPU-min | job-min x the set's vCPUs, from its name (`superci-<cpu>cpu[-<k>]`) |
| avoided | vCPU-min x 0.002 USD, Blacksmith's x64 Linux list price per billing minute (`cost_usd / billing_minutes` of `blacksmith usage`, Nov 10 to 26) |
| net(list) | avoided - superci |
| net(inv) | the same with avoided at 50% through Nov 30, the YC discount on the invoice. Audit B read the discount; no script can |
| $/job-min | superci / job-min, fully loaded: idle pool, boot, disks, IPv4, transfer |
| job records table | the listener's `job.record` events: each finished job's machine, priced from launch to the job's finish, with the part outside the job (boot and idle) split out, per day; `of bill` is that estimate over Cost Explorer's superci, the rest being machines that took no job, disks and transfer. Printed once a day has records |

## Where it is not exact

- **Job events start late on Nov 25.** The listener wrote `tl` events from about 17Z on Nov 25. A day before that has no job-minutes here, so its avoided, net and rate print `n/a` and its superci cost stands alone. The day of the first event is marked `~`.
- **Every superci job counts as a Blacksmith job that would have run.** The log carries no branch or workflow, so a load replay (`ci/superci-load-pull-*`) is counted as if it were real CI.
- **A job counts only with both of its events in the window.** A job that started before the window's first day or has not finished is left out. The log ships each line at least once; the query keeps one line per `eid`.
- **Wall minutes, not rounded minutes.** Blacksmith may round a job up; avoided is a floor on that count.
- **Sets named `superci-41cpu` and `superci-81cpu`** (the 4 and 8 vCPU sets with an index in the digits) are read as 4 and 8. A name that gives no size (`superci-96cpu`) is left out of avoided and its minutes are printed under the table.
- **Today is partial**, and Cost Explorer lags a few hours.
- **A day's window is UTC**, as Cost Explorer's and Blacksmith's daily rows are.
