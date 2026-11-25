# superci-pull

GitHub runner scale sets served by EC2 spot in the `ci-runners` account (243891825325), one per runner size of
megpt-mono: `superci-2cpu`, `superci-4cpu`, `superci-8cpu`, `superci-16cpu` and `superci-32cpu`. GitHub holds every
job until a runner takes it; one listener reads each set's queue, keeps machines for it, and registers a runner for a
machine when the machine proves who it is. Its state is EC2's tags and GitHub's queues; a restart rebuilds the rest.

A workflow asks for a size by its label:

```yaml
runs-on: superci-4cpu
```

## How it works

```
GitHub Actions service: each set's queue, statistics, JIT registration
     ^ long poll per set (GetMessage), AcquireJobs, DeleteMessage, GenerateJitRunnerConfig, RemoveRunner
     |
superci-pull: one process on one c7g.large, an Auto Scaling group of one in us-east-1, Elastic IP 32.202.70.249
  sessions  : one message session per set; a message is acknowledged only after it is handled
  budget    : one registration budget for every set, inside what GitHub leaves the organization (below)
  launcher  : per set, desired = min(max, assigned + waiting + pool); launches desired - live as instant EC2 Fleets
              of at most 250, price-capacity-optimized over the size's 17-18 types x every zone of us-east-1 that
              offers them (5 of 6: use1-az3 offers none) at weight 2 and of us-east-2 (3 of 3) at weight 1; a
              region's spot shortfall goes to the other, then on-demand, at most 10% of the last half hour's
              launches; a ClientToken on every call
  /jit      : POST with the machine's signed identity document; a runner only when the machine holds none
  reconciler: every 10 s lists EC2 and GitHub's runners; adopts, removes stale runners, ends stuck machines, trims
              surplus idle runners (RemoveRunner first), accrues spend, applies the caps
  metrics   : CloudWatch namespace superci-pull, every minute; alarms on them (deploy/alarms.sh)
     |
EC2 machines tagged purpose=superci-pull, superci-pull-set=<set>
  RunsOn ubuntu24 x64 image (superci's pinned build rule); boot.sh asks /jit, runs the runner for its one job and
  powers off (or powers off when told it is not needed); metadata hop limit 1, so a job's containers cannot reach it
```

### What decides each fact

| Fact | Source | After a restart |
| --- | --- | --- |
| Jobs waiting, assigned, running | GitHub's statistics on each message | the session's first statistics; until then every machine waits and nothing launches or ends |
| Machines | EC2 instances with a set's tag | the first listing (machines are adopted) |
| A machine's runner, busy or not | JobStarted and JobCompleted, and GitHub's runner listing | the runner listing |
| Who a machine is | the instance identity document EC2 signed (PKCS7, RSA-2048), checked against AWS's certificate | the same |
| The organization's registration rate | the advance of runner ids (one sequence for the organization), less ours | blind (0.3/s) for about 90 s |
| Spend today and this month | accrued from running machines at their pool's spot price | the SSM parameter `/superci-pull/spend` |
| The listener's TLS key, which machines pin | the SSM SecureString `/superci-pull/tls` | the same key |

### Invariants

- **No job is lost inside our account.** GitHub redelivers a message until the listener acknowledges it, and the
  listener acknowledges only after it has acquired the message's jobs and taken in its statistics.
- **A machine ends only when its runner can take no job.** It powers itself off only when it holds no runner (it
  asked and was told 410). The listener ends a machine with an idle runner only after GitHub removed that runner
  (GitHub refuses to remove a runner that holds a job), and one without a fence only when no runner of it is
  registered in a complete listing made well after its last runner was issued.
- **We never take the organization's last registrations.** Registrations go only into what GitHub's cap leaves the
  rest of the organization, less a margin; when the organization is near the cap, our jobs wait, not Blacksmith's.
- **A machine runs one job, and holds at most one runner, bound to its instance.** A runner is named after the
  instance its identity document names; a machine gets one only on its first ask after boot (or again for one that
  never started), and none once a runner of it has taken a job. No job inherits what another left on a machine.
- **The supply converges.** Every decision is level-triggered from the listings, so a lost answer, a crash or a dead
  machine is repaired by the next reconcile.

### Registration budget

GitHub refuses runner registrations past 1,500 in 5 minutes across the whole organization
(`ConfigureTooManyRunnersException`), JIT runners of scale sets included, and Blacksmith's runners register in our
organization too. Runner ids are one sequence for the organization, so the advance of the highest id is everyone's
rate. The listener learns the highest id from each of its own registrations (exact) and, when it registers rarely,
from a listing of the organization's runners (every 100 s at most). Everyone else's rate is the advance less ours.

- The spare rate is `cap x (1 - margin) / window - others' rate`: with the defaults (1,500, 20%, 300 s) we register
  only into what keeps the organization as a whole under 4 a second; when the others alone reach 4, we register none.
- A registration also needs the window's estimated use, plus those in flight, under `cap x (1 - margin / 2)`.
- A registration a waiting job needs comes first. A pool registration leaves half the saved-up burst to jobs and
  keeps to its own pace (`pool_rate`, and `pool_share` of the spare).
- GitHub's refusal stops every registration for 5 s, doubling to a minute while it keeps refusing.
- Until the rate is measured (90 s after a start) the listener registers at `blind_rate` (0.3/s) at most.
- Across every set, machines waiting for a runner are bounded by what the budget registers in 90 s (a waiting machine
  is paid for). Past twice that, once the rate is measured, machines that have waited three minutes end.

The estimate is published as `OrgRegistrationRate`, `OthersRegistrationRate`, `OurRegistrationRate`,
`SpareRegistrationRate` and `RegistrationWindowUse`, and logged every 30 s (`"msg":"budget"`).

Moving a job from Blacksmith to superci moves its registration rather than adding one, so a gradual ramp of the
fleet switch keeps the organization's total where it is. A test that adds jobs (a load test) adds registrations,
and at the organization's measured 3.5 to 4.5 a second it has almost nothing to add them into.

### Machine identity

A machine POSTs `/latest/dynamic/instance-identity/rsa2048` from its metadata service. The listener verifies the PKCS7
signature with AWS's RSA-2048 certificate of the region the document names (`certs/`, from AWS's documentation),
requires account 243891825325, and serves the instance only if it is a machine of one of the sets. Its address plays
no part. The runner it registers is named `<set>-<instance id>-<suffix>`. A machine runs one job: once its runner
has taken a job it is told to end (boot.sh powers off after its runner exits anyway). It gets a second runner only
for one that never started, after GitHub removed that one on the listener's request. Anything else on the machine
that reads the document (a job) gets nothing while the runner holds its job, and nothing after.

### Pools

Each set keeps a hot pool of idle runners: enough to cover what arrives during `fill_seconds` (45: launch, boot and
registration) times `factor` (1.5), measured over the set's last 10 minutes of arrivals, between a floor and a cap
for the hour (UTC). The caps come from megpt-mono's Blacksmith arrivals by hour (`tools/pools.py`); the floors are 1
for 2, 4 and 8 vCPU and 0 for 16 and 32. Within 15 minutes of the next hour, the larger of the two hours' bounds
applies, so a busy hour's pool is registered slowly ahead of it. A split size divides its bounds among its sets.

### Sizes and split

| Label | Types | Max machines | Disk |
| --- | --- | --- | --- |
| superci-2cpu | 18 `.large` of c5, c5a, c6a, c6i, c7a, c7i, c7i-flex, c8a, c8i, m5, m5a, m6a, m6i, m7a, m7i, m7i-flex, m8a, m8i | 600 | 40 GB |
| superci-4cpu | the same 18 at `.xlarge` | 1,200 | 60 GB |
| superci-8cpu | `.2xlarge` | 600 | 80 GB |
| superci-16cpu | `.4xlarge` | 100 | 120 GB |
| superci-32cpu | 17 `.8xlarge` (no c5) | 50 | 150 GB |

Memory is 2 GiB per vCPU (c families) or 4 (m families). One set hands out about 15 jobs a second; `"split": K`
runs a size as K sets (`superci-4cpu`, `superci-4cpu-2`, …), each carrying the size's label, and GitHub spreads the
label's jobs across them (measured below). A split changes at the next start; sets the config no longer has are
deleted at GitHub then, and a job one of their runners held fails, so shrink a split while the size is idle.

### Lifetimes, spend and lost jobs

- A job may run `job_minutes` + `margin_minutes` (360 + 30): the machine kills a runner past that and powers off.
  A pool machine gets no runner past `max_age_hours` (8), and every machine powers itself off at 8 h plus a job's
  length, whatever it is doing. A machine that cannot reach the listener for 15 minutes powers off (it holds no
  runner then).
- Spend is accrued every 10 s from each running machine's price (its pool's spot price, refreshed every 30 minutes;
  $0.058 per vCPU-hour for on-demand; gp3 included). At `daily_usd` (1,000) or `monthly_usd` (15,000) the listener
  launches nothing and keeps no pool; running jobs finish, and queued jobs wait until the day or month turns or the
  cap is raised. It is an estimate made to bound the bill, not the bill.
- A job lost to a spot reclaim is re-run once with GitHub's re-run API: a job that completed on a machine AWS gave
  notice it takes back, or one whose machine EC2 stopped listing before the job completed. It is re-run when its run
  has completed (GitHub re-runs a job only in a completed run) and only if GitHub's conclusion is not a success, so a
  job that finished inside the two minutes' notice is left alone. The list is in memory: a restart forgets it.

### Cache seam

The S3 actions cache plugs in at three places: one line of `boot.sh` after the metadata token (`T=`) is fetched, an
`IamInstanceProfile` (read-only) in `setRegion.ensureTemplate` (`fleet.go`), and an `iam:PassRole` statement for
that profile in `deploy/listener-policy.json`. Nothing else in the listener knows about the cache.

## Operate

```bash
L=32.202.70.249; ssh ec2-user@$L               # SSH from the admin address up.sh allowed
tail -f /var/log/superci-pull/listener.jsonl    # JSON events: message, fleet, register, ask, budget, state, terminate
                                                # (also in CloudWatch Logs, group /superci-pull/listener, a stream per instance)
curl -s localhost:8080/state | jq               # budget, spend, and per set: desired, live, idle, busy, pool, orphans
curl -s localhost:8080/healthz                  # what the health timer checks
```

**Change pool sizes, caps or limits.** Write an override file and upload it; every field it names replaces the
built-in one (`config.json`), and a `pools` entry replaces that size's whole entry. The instance fetches it every
minute and the listener applies pools, caps, the registration budget and machine limits within 30 s; sizes, splits,
types, disks and regions apply at the next start (`sudo systemctl restart superci-pull`).

```bash
cat > override.json <<'JSON'
{"daily_usd": 500,
 "pools": {"superci-4cpu": {"floor": [2,2,2,2,2,2,2,2,2,2,2,2,2,2,2,2,2,2,2,2,2,2,2,2],
                            "cap":   [40,40,40,40,40,40,40,40,40,40,40,40,40,40,40,40,60,60,60,60,60,60,60,60]}}}
JSON
aws --profile ci-runners s3 cp override.json s3://superci-pull-243891825325/config/config.json
```

A pin until the file next changes: `curl -s -X POST 'localhost:8080/override?set=superci-4cpu&pool=20&max=300'`
(`pool=-1` drops the pin).

**Read metrics.** CloudWatch, namespace `superci-pull`, us-east-1: `Heartbeat`, `OrgRegistrationRate`,
`OthersRegistrationRate`, `OurRegistrationRate`, `SpareRegistrationRate`, `RegistrationWindowUse`, `OrgRateKnown`,
`Registrations`, `RegistrationRefusals`, `RegistrationErrors`, `QueueWait` (seconds per started job, with
percentiles), `Orphans`, `SpendTodayUSD`, `SpendMonthUSD`, `SpendPerHourUSD`, `SpendCapped`, `Machines` and
`OnDemandShare`; per set (dimension `Set`): `Machines`, `IdleRunners`, `BusyRunners`, `Pool`, `JobsWaiting`.

**Alarms** (`deploy/alarms.sh`, topic `superci-pull-alarms`; subscribe with `aws sns subscribe`):

| Alarm | Fires when |
| --- | --- |
| superci-pull-listener-down | no heartbeat for 3 minutes |
| superci-pull-registration-refusals | GitHub refused a registration in a 5-minute period |
| superci-pull-queue-wait-p90 | queue wait p90 over 60 s in two 5-minute periods running |
| superci-pull-orphans | orphans above 0 for 10 minutes |
| superci-pull-spend-daily | today's spend past 80% of the daily cap |
| superci-pull-spend-capped | a spend cap is reached |
| superci-pull-org-rate-unknown | the organization's rate unmeasured for 15 minutes |

**Release.** `deploy/build.sh <key> <out>` builds and tests on a tagged arm64 instance and terminates it;
`deploy/release.sh <binary> [override.json] ["ssh ec2-user@$L"]` uploads to `s3://superci-pull-243891825325` and
restarts the listener (a graceful stop closes its sessions, so the next opens at once).

**Roll back.** The bucket is versioned, the binary and the override config alike: copy the previous version over the
current one and restart.

```bash
B=superci-pull-243891825325
aws --profile ci-runners s3api list-object-versions --bucket $B --prefix bin/superci-pull --query 'Versions[].[VersionId,LastModified]' --output text
aws --profile ci-runners s3api copy-object --bucket $B --key bin/superci-pull --copy-source "$B/bin/superci-pull?versionId=<previous>"
ssh ec2-user@$L "sudo aws s3 cp --quiet s3://$B/bin/superci-pull /usr/local/bin/superci-pull && sudo systemctl restart superci-pull"
```

To stop taking jobs, send the jobs back to Blacksmith with the fleet switch (megpt-mono's `SUPERCI_JOBS`, which
since #34100 routes a switched job to `superci-<N>cpu`); to stop launching at once,
`curl -s -X POST 'localhost:8080/override?max=0'` (machines end as their jobs finish).

**Resilience.** systemd restarts the process (5 s). A health timer marks the instance unhealthy after five failed
`/healthz` checks and the Auto Scaling group replaces it; the group also replaces an instance EC2 reports down. A
replacement takes the Elastic IP and the TLS key, so machines keep their address and pin, adopts every machine from
EC2 and every runner from GitHub, and runs no decision until each set's session has given the queue's statistics.
After a crash, GitHub refuses a new session for a set while the dead one's lapses (30 to 75 s measured).

**Deploy from scratch.** `deploy/up.sh <admin-cidr> <ssh-public-key>` (role, parameters, bucket, groups, key pair,
address, launch template), `deploy/release.sh <binary>`, `deploy/up.sh` again (it starts the group once a release
exists), `deploy/alarms.sh`. **Tear down.** `deploy/down.sh "ssh ec2-user@$L"` deletes the scale sets and every
resource tagged `purpose=superci-pull`.

**Load test.** `bench/workflow.py` writes the workflow (a wave of mixed sizes per push, a matrix burst for a push
whose message says "burst"); `bench/measure.py BRANCH SINCE UNTIL listener.jsonl` measures it against the pass bar;
`bench/orphans.py` lists orphaned machines and runners.

## Cost

Spot prices on Nov 25 2026 08:00Z (median of each size's 10 cheapest pools), with its gp3 volume, a machine-hour:
2 vCPU $0.035, 4 vCPU $0.062, 8 vCPU $0.106, 16 vCPU $0.190, 32 vCPU $0.351.

| | Per hour |
| --- | --- |
| The listener (c7g.large on-demand, 30 GB gp3, the Elastic IP's public address) | $0.08 |
| Idle at the floors (1 machine each of 2, 4 and 8 vCPU; overnight, or with no jobs routed here) | $0.20 |
| Idle at the busiest hour's caps (57, 119, 37, 6 and 3 machines), reached only when that hour's arrivals are all here | $15.5 |

A busy hour costs what its jobs run for at those prices, plus about 30 s of boot and shutdown per job (a machine runs
one job), plus the pool. `SpendPerHourUSD` is the listener's live figure.

## Measured (Nov 25 2026, megpt-mono)

| Test | Run | Result |
| --- | --- | --- |
| Smoke, one job per size | 45568198731 | 5/5 succeeded; jobs on a pool runner (2, 4, 8 vCPU) waited 1 s, the 16 and 32 vCPU jobs (no pool) 16 and 17 s, from launch to a registered runner in 11-15 s; each ran on its size (c8a.large, c5a.xlarge, c5a.2xlarge, c7i-flex.4xlarge, c6a.8xlarge; `nproc` 2, 4, 8, 16, 32) |
| Split, 40 jobs on `superci-4cpu` run as two sets | 45568606692 | GitHub gave 20 to `superci-4cpu` and 20 to `superci-4cpu-2`; 40/40 succeeded |
| Crash mid-run, 30 jobs over every size | 45569779369 | `kill -9` of the process at 07:39:39, then the listener's instance terminated at 07:40:40: 30/30 succeeded; at +10 min, 0 orphaned machines, 0 orphaned runners, and `Orphans` 0 in every period |
| Listener down (stopped 08:24:59 to 08:28:47) | | `superci-pull-listener-down` went to ALARM at 08:28:40 and back to OK at 08:30:40 (on `FILL(Heartbeat, 0)`; a plain heartbeat alarm stayed OK through an earlier 5-minute outage) |
| Spot reclaim mid-job (AWS FIS `send-spot-instance-interruptions`, role `superci-pull-fis`) | 45573133090 | the notice reached the listener 25 s after it was sent; AWS took the machine 2 min later and GitHub failed the job (attempt 1); the listener re-ran it 5 s after the run completed, and attempt 2 succeeded on another machine |

Crash details. systemd restarted the process in 5 s. GitHub refused its sessions as conflicts while the dead
process's lapsed: 30 to 47 s per set. In that time the guard held every machine at Wait; no machine was told to end
and nothing launched (before the guard existed, the same crash ended 12 waiting machines). The Auto Scaling group
noticed the terminated instance in 55 s and started a replacement, which took the Elastic IP and answered machines
86 s after the termination, with all six sessions open at once (the dead ones had lapsed) and every machine adopted.
A graceful restart opens every session 9 s after the stop, with no conflict.

GitHub's registration cap binds the organization. The runner-id advance showed 2.9 to 5.3 registrations a second
across the organization from 06:54 to 08:00Z (Blacksmith, the push plane and us), so at the 20% margin the spare
rate was 0 to 1.1 a second. In 07:18-07:24Z megpt-mono started 1.93 jobs a second on runners (all Blacksmith) while
runner ids advanced 3.83 a second, so about half of the organization's registrations are not megpt-mono's jobs
(other repositories, or Blacksmith registering more runners than it runs jobs); which, I did not determine.
