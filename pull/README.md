# superci-pull

GitHub runner scale sets served by EC2 spot in the `ci-runners` account (243891825325), one per runner size of
megpt-mono: `superci-2cpu`, `superci-4cpu`, `superci-8cpu`, `superci-16cpu` and `superci-32cpu`. GitHub holds every
job until a runner takes it; one listener reads each set's queue, keeps machines for it, and registers a machine's one
runner when the machine proves who it is. Its state is EC2's tags and GitHub's queues; a restart rebuilds the rest.
The sets are in the runner group `superci-pull`, which only megpt-mono may use.

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
              of at most 250, capacity-optimized-prioritized over the size's types, listed fastest first (4 and 8 vCPU: m8azn,
              then m8a and r8a; the others: c8a and m8a, then c8i, m8i, c7a and m7a, then older families) x every zone of us-east-1 that
              offers them (5 of 6: use1-az3 offers none) at weight 2 and of us-east-2 (3 of 3) at weight 1; a
              region's spot shortfall goes to the other, then on-demand, at most 10% of the last half hour's
              launches. A size with `on_demand` (superci-4cpu and superci-8cpu since Nov 28) asks each of its types on demand in
              its order, us-east-1 before us-east-2, whatever that share, and spot only for what on demand could not start:
              in F2 (Nov 27, unit-tests' volume) AWS took back 27 of 28 us-east-1 m8azn.xlarge spot machines in 18 minutes
              and 31 jobs with them, and the 146 on demand lost none. A size's `on_demand_first` leading types go first, spot then
              on demand outside that 10%, before any slower type: m8azn spot is thin (10 of 11 m8azn-only asks
              failed, Nov 26), on-demand m8azn.xlarge is $0.41/h against Blacksmith's $0.48/h for 4 vCPUs, and
              unit legs ran 12% slower on m8a and r8a (93 s against 83 s). A size's `spot_first` leading types
              (superci-8cpu's mix until Nov 28: m8azn.3xlarge) go first as spot alone and never on demand, then the others' spot: one
              spot ask over all of them is capacity-optimized, and superci-8cpu's jobs ran on r8a.2xlarge 33 times
              and m8a.2xlarge 4 times from one (Nov 26-27). m8azn.3xlarge on demand ($1.2388/h) would cost 2.40x
              Blacksmith per unit-tests leg; a ClientToken on every call
  /jit      : POST with the machine's signed identity document; a runner only for a machine never issued one, only
              once its grant has made the machine's claim (s3://superci-pull-claims-<account>/issued/<instance>),
              and its config leaves only after EC2 has the machine's superci-pull-issued tag
  /work /shim: the identity plane's dispatcher (Identity plane): a machine of a dispatch set takes the one job an
              identity's shim brings, with no registration
  lister    : lists the group's runners every 30 s (3 s a page), on its own: a slow GitHub slows only it
  reconciler: every 10 s lists EC2; adopts, ends machines whose job completed, stuck, overdue or shed ones, retires
              surplus idle runners (RemoveRunner first), accrues spend, applies the caps; its GitHub calls run apart
  metrics   : CloudWatch namespace superci-pull, every minute; alarms and a dashboard on them (deploy/alarms.sh)
     |
EC2 machines tagged purpose=superci-pull, superci-pull-deployment=superci-pull, superci-pull-set=<set>, and once
issued its runner superci-pull-issued=<when>
  RunsOn ubuntu24 x64 image (superci's pinned build rule); boot.sh asks /jit, runs the runner for its one job and
  powers off (or powers off when told it gets none); the listener ends it when its job completes, whatever runs on
  it; metadata hop limit 1, so a job's containers cannot reach it
```

### What decides each fact

| Fact | Source | After a restart |
| --- | --- | --- |
| Jobs waiting, assigned, running | GitHub's statistics on each message | the session's first statistics; until then, and until every region is listed, nothing launches or ends and no machine is granted a runner |
| Machines | EC2 instances with the deployment's tag and a set's tag | the first listing of every region (machines are adopted) |
| Whether a machine was ever issued a runner | its claim `issued/<instance id>` in `superci-pull-claims-<account>`, made once (S3 create-if-absent) before its runner is registered; the tag `superci-pull-issued`, written before the config leaves, and any runner GitHub lists or a job message names after it are hints | the claim, which a grant must make (S3 is strongly consistent, EC2's tag reads are not); the listing's tags |
| A machine's runner, busy or not | JobStarted and JobCompleted, GitHub's runner listing, and GitHub's answer for the runner's name | the messages GitHub redelivers, the listing, and a check by name |
| A job a spot notice found running | the tag `superci-pull-noticed=<owner>/<repo>/<run>` | the tags of machines EC2 still lists (about an hour after a reclaim) |
| Who a machine is | the instance identity document EC2 signed (PKCS7, RSA-2048), checked against AWS's certificate | the same |
| The organization's registration rate (a gauge) | the advance of runner ids (one sequence for the organization), less ours | unknown for about 90 s after a start; admission does not read it |
| Spend today and this month | accrued from running machines at their pool's spot price | the SSM parameter `/superci-pull/spend` |
| Each set's burst and launch times | the SSM parameter `/superci-pull/pool/<deployment>`, one per deployment so no listener overwrites another's (`-pool-param` is the prefix); saved every minute, the carried burst held for the first window | the same parameter, if saved within the window |
| The listener's TLS key, which machines pin | the SSM SecureString `/superci-pull/tls` | the same key |

### Invariants

- **No job is lost inside our account.** GitHub redelivers a message until the listener acknowledges it, and the
  listener acknowledges only after it has acquired the message's jobs and taken in its statistics.
- **A machine ends only on proof that its runner can take no job.** The proof is one of:
  - GitHub's JobCompleted for its runner: the listener ends the machine, whatever runs on it;
  - GitHub's removal of its idle runner (GitHub refuses to remove a runner that holds a job);
  - GitHub's answer that it has no runner of the machine's id or name;
  - for a machine never issued one, three complete listings in a row with none of it;
  - its age past a bound (below).

  An ask is no proof of anything: anything on a machine can ask.
- **GitHub decides when the organization is full.** Registrations go on until GitHub refuses one; a refusal halves
  our rate and holds every ask, and a ceiling of our own keeps us alone inside GitHub's documented cap.
- **A machine gets one runner, ever.** The runner is named `<set>-<instance id>`. A grant first creates the
  machine's claim, the S3 object `issued/<instance id>`, with `If-None-Match: *`: of every grant, across restarts,
  forgotten listings and overlapping listeners, exactly one creates it (DDIA ch. 9, compare-and-set; ch. 8, a fencing
  token). The object holds that grant's token, so only that grant goes on after a lost answer or a refused
  registration. Then the listener registers the runner, writes the instance's `superci-pull-issued` tag, and only
  then lets the config leave; a claim another grant holds answers 410, a claim not known to be ours answers 429, and
  a tag it cannot write withholds the config, and the machine ends. Every later ask, from the boot script that lost its answer or from a job that kept
  the machine alive, is answered 410 with nothing, before and after any restart. A runner is never replaced: a
  machine that asks again holding an idle runner ends once GitHub removes the runner. No job inherits what another
  left on a machine, and no job can take a second runner from it.
- **Nothing is decided before every region is listed and the queue is known.** A region not yet listed would read
  as no machines: everything launched again, every runner of it an orphan.
- **The supply converges.** Every decision is level-triggered from the listings, so a lost answer, a crash or a dead
  machine is repaired by the next reconcile.

### Registration budget

GitHub documents a cap of 1,500 runner registrations in 5 minutes per repository, organization or enterprise, not
raisable (https://docs.github.com/en/actions/reference/limits). The page does not say what a refusal looks like; the
listener reads every error of a registration as one (a 429, a 403 with or without a rate-limit header, a 400, 409 or
422, a 5xx under load), except the answers that say something else: the runner exists, our credential is bad, the scale
set is gone, or we cancelled the ask. The budget admits on that answer, not on a guess:

- **GitHub's refusal is the one hard signal.** It halves the admitted rate (down to 0.1/s at the least), drops the
  saved tokens, and holds every ask for 5 s, doubling to a minute while GitHub keeps refusing. Nothing saves up and
  the rate does not climb during a hold; the rate climbs back to the ceiling in `recover_s` (120 s) from the end of
  the hold while GitHub refuses nothing. Every refusal is counted
  (`RegistrationRefusals`, its alarm), logged (`"msg":"registration refused by GitHub"`) and written to the timeline
  (`jit.refused`, `why` `refused`).
- **Below the admitted rate no machine waits.** The organization's estimated rate decides nothing.
- **A ceiling of our own** bounds our registrations whatever GitHub answers: `ceiling_rate` a second and
  `ceiling_burst` at once. It starts at 2 a second and 50 at once, 650 in 300 s, well inside GitHub's documented
  1,500; 50 covers most simultaneous bursts (the largest measured was 115 jobs within 3 s from four merge queue
  drafts, Nov 26 03:11Z, which then queue behind the tokens). `ceiling_rate` x `window_s` + `ceiling_burst` must not
  pass `cap` (1,500): config load and `-validate` refuse a ceiling that could, such as a `ceiling_rate` of 6.

  **Raising it.** Raise one step at a time, set in the override file named by `-config`, and only after 24 hours
  in which all three hold:

  1. `RegistrationRefusals` is 0 for the whole 24 hours.
  2. Blacksmith's Linux queue-wait p90 stays under 10 s (the baseline the share on superci must not worsen).
  3. `denied_tokens` (the listener's budget log line; `BudgetDenied` reason `tokens`) shows we actually
     hit the ceiling. A ceiling nobody reached is not what held a job back.

  | Step | `ceiling_rate` | `ceiling_burst` | Most in 300 s |
  | --- | --- | --- | --- |
  | 0 (default) | 2 | 50 | 650 |
  | 1 | 3 | 100 | 1,000 |
  | 2 | 4 | 150 | 1,350 |

  A step that brings a refusal goes back one step. The next step, 5 a second with 150 at once, is 1,650 in one window
  and is refused by the validation: past step 2 the rate and the burst trade off at `ceiling_rate` x 300 +
  `ceiling_burst` = 1,500 at most (4 a second with 300 at once, for one), and nothing there leaves room for anyone
  else's registrations in the organization.
- **When tokens are short, machines are served in order**: critical before batch before pool, and in each class the
  machine launched first. A machine turned away keeps its place while it keeps asking (6 s, two polls); the next
  tokens are owed to the machines ahead of it (`BudgetDenied` reason `queued`).
- The budget is one allocator with three classes. **Critical** is a registration for a job of the required check; it
  has a reserve (`critical_reserve`, 2 tokens, cut to the burst less one). **Batch** is a job the `batch` rule names
  (by default the events `schedule` and `workflow_dispatch`: cache-warm crons and load branches; `workflow_refs` and
  `jobs` add patterns): it borrows only tokens the reserve leaves. Every job no rule names is critical. A machine
  takes no job in particular, so the waiting jobs are shared out to the machines waiting for a runner in launch
  order, critical jobs first. **Pool** only fills the hot pool: it leaves half the saved-up burst to jobs and keeps
  to its own pace (`pool_rate` 5, and `pool_share` 0.9 of the admitted rate). A pool runner is a job's registration
  made ahead of it: every job takes one, so the pool's pace must keep up with the jobs (2 to 5 a second at full
  rollout); 0.5 a second left the pool empty after every burst.
- Across every set, machines waiting for a runner are bounded by what the budget admits in 90 s (a waiting machine
  is paid for). Past twice that, machines that have waited three minutes end.

The organization's rate is a gauge. Runner ids are one sequence for the organization, so the advance of the highest
id is everyone's rate. The listener learns the highest id from each of its own registrations (exact) and, when it
registers rarely, from a probe: a runner registered and removed at once, whose id is the head, exact (every 100 s at
most, inside the budget and counted as ours). A listing of every organization runner (44 pages at 4,300 runners,
about 1,600 of the App's shared 5,000 requests an hour at that pace) is only the fallback. Everyone else's rate is
the advance less ours. Against megpt-mono's jobs it reads within about 10%: 4.67 a second against 4.90 ± 0.50 job
starts on Nov 26 00:27-00:37Z, and 1.07 registrations per job at 08:30Z and 14:00Z on Nov 25.

Why not admit on the estimate: on Nov 26 it read 4.2 to 4.9 a second from 00:22Z for 21 minutes, the 20% margin it
kept admitted nothing, and ten test-go jobs waited 482 to 898 s. GitHub refused no one: Blacksmith's jobs in the
same minutes started in 6 s at the median and 19 s at most, the estimate passed 1,500 in 14 minutes of Nov 25-26,
and the listener logged no refusal in its 20 hours of logs.

The estimate is published as `OrgRegistrationRate`, `OthersRegistrationRate`, `OurRegistrationRate`,
`SpareRegistrationRate` and `RegistrationWindowUse`, and logged every 30 s (`"msg":"budget"`).

How the advance is measured, and how far off it can be:

- A head is the highest id GitHub has issued by a time. One from our own registration is exact. One from a listing
  of the organization's runners is a lower bound: an ephemeral runner is deleted after its job, so the newest ids
  can be gone already. It is short by at most what registered and finished within the listing.
- The rate is the advance between the head 300 s ago (interpolated) and the newest head, over at least 60 s.
  Others' rate is that advance less ours. A lower-bound head reads the rate low by at most (missed ids) / span:
  with jobs of 30 s or more and a 10-15 s listing, a few ids over 60-300 s, under 0.1/s.
- Ids are not dense if GitHub skips some. The advance then overstates the rate, which only makes us register less.
- The estimate admits and refuses nothing, so an error in it changes no decision; the ceiling and the backoff
  after a refusal bound our registrations.

Moving a job from Blacksmith to superci moves its registration rather than adding one, so a gradual ramp of the
fleet switch keeps the organization's total where it is. A test that adds jobs (a load test) adds registrations,
and at the organization's measured 3.5 to 4.5 a second it has almost nothing to add them into.

**Leases.** A lane that registers runners outside the listener (a test of another runner plane) takes from the same
cap, and a registration the budget did not know of brings GitHub's refusal nearer for the listener's own. Such a lane
asks first, on the admin endpoint (localhost; reach it from another host through `deploy/ssh.sh`):

```bash
curl -s -X POST 'localhost:8080/lease?n=40&ttl=120'            # {"id":"…","granted":7,"expires":"…"}
curl -s -X POST 'localhost:8080/lease/settle?id=<id>&made=7'   # how many it made; 204, or 410 if it expired
```

A lease is up to `n` registrations at batch priority (the identity plane's `/identities/token` takes one too, Identity hosts), prepaid out of the budget and held in flight (`pending`,
`leased` in `/state`) for `ttl` (at most 10 minutes). Settling turns the ones made into ours; one never settled is
released when it expires. A lane runs only the registrations it was granted.

### Machine identity

A machine POSTs `/latest/dynamic/instance-identity/rsa2048` from its metadata service. The listener verifies the PKCS7
signature with AWS's RSA-2048 certificate of the region the document names (`certs/`, from AWS's documentation),
requires account 243891825325, and serves the instance only if it is a machine of one of the sets. Its address plays
no part. The runner it registers is named `<set>-<instance id>`. A machine gets that one runner, ever (Invariants).
Anything else on the machine that reads the document (a job) gets 410 and nothing, while its runner holds the job
and after, across restarts of the listener.

### Pools

Each set keeps a hot pool of idle runners sized from the bursts its jobs come in. A job finds a ready runner when
fewer jobs than the pool arrived in the launch time up to it, since each job takes a runner and launches the next. So
the pool is, for each job the set saw in the last 30 minutes (`window_minutes`), how many jobs arrived within one
launch time up to and including it, taken at the 88th percentile (`quantile`), and never less than the largest such
burst of the last 5 minutes (`hold_minutes`: a pool runner retired is a registration and a boot spent for nothing).
The launch time is the set's own: the 90th percentile, over its last 64 launches, from the launch call to the
machine's first ask for its runner, so it follows the image and hydration. The wait for the registration budget is
left out: it is a rate no pool can buy ahead, and counting it fed back (a longer launch time sized a larger pool, whose
registrations lengthened the wait, to 244 s on a dev set). Until a set has eight launches it is
`ready_seconds` (22). A job arrives with its first JobAvailable or JobAssigned; GitHub assigns a set's jobs without
offering them first, and a job is known by its job id. Before Nov 25 19:00Z every arrival (and every `QueueWait`)
read as none and each pool stayed at its floor; the load tests above pinned their pools, which hid it.

The pool is the measured burst of the jobs routed to the set, bounded by a cap for the hour (UTC) and a floor that ships at 0 for every size and
hour: the caps are flat spend guards (120, 200, 250, 40 and 12) above the largest pool the weekday model below asks for, and a split size divides
its bounds among its sets. A burst of 0 (no job arrived in the last `window_minutes`, none is held or carried from before a restart) means nothing
is routed to the set, and its pool is 0 whatever a floor says. No idle runner is trimmed in the first window after a start, while the set has seen too
few jobs to size its pool.

Only routed jobs count as arrivals. superci's own maintenance runs are not routed demand: `no_demand` in the config names them by workflow ref
(`/ci-cache-warm\.yml@`: megpt-mono's cache-warm and hydration runs, which come by push to main, by schedule and by dispatch), and they take a machine like
any job and size no pool. It is narrower than `batch` on purpose. `batch` names the jobs that may wait for the registration budget (events `schedule` and
`workflow_dispatch`), and many of those are real work: a developer's dispatch of the selected backend tests, a load test started by dispatch, the scheduled
integration tests. Those are routed demand and size the pool they take their runners from.

No hand floor ships. The first design held 70 (4 vCPU) and 100 (8 vCPU) from 14:00 to 02:59Z and 1 otherwise. On Nov 26 at 23:57Z that held 70, 100 and 9
idle runners of 4, 8 and 2 vCPU (43.8 USD an hour with `SUPERCI_JOBS` unset), and after a restart at 02:48Z one cache-warm job per set raised the pool to
the hour's floor: about 171 registrations and 20 USD for no routed work. A floor is a pool no arrival asked for. A single job now warms what its own burst
needs (a burst of 1 is a pool of 1), 40 at once warm about 36, and the pool is 0 a window plus the hold (35 minutes) after the last routed job. Pins from
`/override` last only until the config changes or the listener restarts, and nothing needs one.

The cost is the hand floors' benefit, and it is not only the first wave of a surge after a quiet stretch. The replay below measured what they bought: at the
Nov 26 peak, p90 queue wait of 0 s on 4 vCPU and 5 s on 8 vCPU with floors of 70 and 100, against 17 s on 8 vCPU with the 30-minute quantile's pool alone,
because the registration ceiling (2 a second, 50 at once, not raised here) holds its refills, not the pool. So 8 vCPU is slower at a steady peak too, and the
first wave after a quiet stretch meets the pool of the jobs seen so far. 17 s is under the `queue-wait-p90` alarm (60 s) and above Blacksmith's 13 s. Batch and
other pool runners are taken by any job of their label, so a maintenance run takes a runner a routed job may have wanted; the launcher replaces it at once.
The dashboard's queue wait shows the cost. Lower the cost by raising the ceiling, or by a rule that warms a quiet set from the same hour's earlier days.

The measurement: in the Nov 26 16:02-16:22Z peak, 15 merge-queue runs brought 24 legs each on 4 vCPU and 2 on 8 vCPU, and 47 PR pushes brought 28
legs each on 8 vCPU. The jobs within 20 s up to each arrival were p50 33, p88 67 and p99 92 on 4 vCPU, and p50 34, p88 66 and
p99 98 on 8 vCPU. Up to 224 8 vCPU legs arrived in a minute and 548 in 5 minutes. Blacksmith's unit legs waited p50 8 s and p90
10 s on 4 vCPU, and p50 7 s and p90 13 s on 8 vCPU. A replay of those arrivals (each arrival takes an idle runner or the next one
registered; a launch asks for its token after a 25 s boot; tokens at the ceiling of 2 a second and 50 at once) waits p90 0 s on
4 vCPU and 5 s on 8 vCPU with floors of 70 and 100, and 17 s on 8 vCPU with the quantile pool alone (about 66), and 6 s there at a ceiling of 3/100.

Why this rule, measured on Nov 25 2026 (`tools/wait/`):

- The 4cpu canary (lint-test-backend's `verify-tools` and `test-go`, 17:03-17:25Z, 140 jobs) waited 22 s p50 and
  24 s p90. The pool read no arrivals and sat at its floor of 1: 17 jobs found a ready runner (2 s), 123 waited for a
  launch: Fleet 3.7 s, boot 16.7 s (EC2 to kernel 2.5-5 s, systemd and cloud-init 7 s, boot.sh 4 s), registration
  0.8 s, runner start 2 s.
- Demand at full rollout (Blacksmith's Linux jobs; a weekday is about 177,000): at the Nov 24 02:00Z peak, 4 vCPU
  takes 2.5 jobs a second, 39 in 20 s at the median and 105 at p90. One push brings up to 114 jobs of a size at once.
- A model of the listener over that weekday (pushes at each hour's rate, each with a real push's jobs; launch times
  from the canary; validated against the canary at 21.5 s p50 against 22 measured) meets a wait of p50 5 s and p90
  10 s at every size with this rule at $278 a day idle with plain machines (quantile 0.8) and $424 with hydrated
  ones (quantile 0.88, launch 38 s median). Launching for a run's later jobs ahead of them saves at most $3 a day: a
  push's jobs arrive together. A stopped or hibernated spot pool cannot be started reliably: a stopped spot machine
  is pinned to its type and zone, and 7 starts in 7 over 3 minutes were refused for capacity.

The quantile is the cost dial (`tools/wait/curve.py`, the weekday model, launch times as measured for plain machines).
A push's slowest job of a size is the most a required check of that push waits for a machine:

| quantile | idle $ a day | job wait p50 / p90 | push's slowest job p50 / p90 | pushes all warm |
| ---: | ---: | --- | --- | ---: |
| 0.80 | 278 | 2.0 / 8.9 | 2.0 / 13.3 | 68% |
| 0.85 | 319 | 2.0 / 5.9 | 2.0 / 11.0 | 75% |
| **0.88** | **350** | **2.0 / 3.5** | **2.0 / 9.5** | **79%** |
| 0.90 | 377 | 2.0 / 2.0 | 2.0 / 8.1 | 82% |
| 0.93 | 422 | 2.0 / 2.0 | 2.0 / 5.4 | 86% |
| 0.95 | 461 | 2.0 / 2.0 | 2.0 / 2.5 | 90% |

A push's jobs arrive together, so it is warm more often than independent jobs would be. With hydration before the ask
(launch 38 s median) the same push bar needs 0.93, at $510 a day.

### Sizes and split

| Label | Types | Max machines | Disk |
| --- | --- | --- | --- |
| superci-2cpu | 18 `.large` of c5, c5a, c6a, c6i, c7a, c7i, c7i-flex, c8a, c8i, m5, m5a, m6a, m6i, m7a, m7i, m7i-flex, m8a, m8i | 600 | 40 GB |
| superci-4cpu | m8azn.xlarge (5 GHz), m8a.xlarge, r8a.xlarge | 1,200 | 60 GB |
| superci-8cpu | m8azn.3xlarge (12 vCPU, 5 GHz) spot first, then m8a.2xlarge | 600 | 80 GB |
| superci-16cpu | `.4xlarge` | 100 | 120 GB |
| superci-32cpu | 17 `.8xlarge` (no c5) | 50 | 150 GB |

Memory is 2 GiB per vCPU (c families), 4 (m) or 8 (r); the 4 and 8 vCPU sizes have 4 or more, as Blacksmith's runners do, since a unit-test leg needs 11 GB on 4 vCPUs. The root grows to the disk at boot: the image's own cloud-init does not grow it. One set hands out about 15 jobs a second; `"split": K`
runs a size as K sets (`superci-4cpu`, `superci-4cpu-2`, …), each carrying the size's label, and GitHub spreads the
label's jobs across them (measured below). A split changes at the next start. A set the config no longer has is
drained first: its session asks for no job (max 0), its machines end as their jobs complete or as idle supply, and
once it holds no machine it is deleted at GitHub, so no job fails.

### Lifetimes, spend and lost jobs

- The listener bounds every machine, whatever runs on it:
  - a job may run `job_minutes` + `margin_minutes` (360 + 30) from when it started (JobStarted);
  - an idle runner retires (removed, then its machine ends) once the machine is `max_age_hours` (8) old, and a
    machine never issued one ends then;
  - any machine ends at 8 h plus a job's length.

  The machine also powers itself off at 8 h plus a job's length, and after 15 minutes without reaching the listener
  (it holds no runner then). A job can stop those timers; it cannot stop the listener's.
- Spend is accrued every 10 s from each running machine's price: its pool's spot price (refreshed every 30
  minutes; $0.058 per vCPU-hour for on-demand), its gp3 volume and its public IPv4 address ($0.005/h). Data
  transfer out is not counted. At `daily_usd` (1,000) or `monthly_usd` (15,000) the listener launches nothing and
  keeps no pool. Running jobs finish; queued jobs wait until the day or month turns or the cap is raised: CI on
  superci stops. `superci-pull-spend-daily` alarms at 80% of the cap the listener runs with. To keep CI going,
  raise the cap (override file) or send the jobs back to Blacksmith: unset megpt-mono's `SUPERCI_JOBS`. The estimate
  bounds the bill; it is not the bill.
- A job lost to a spot reclaim is re-run once with GitHub's re-run API, only on proof of the reclaim:
  - the job completed on a machine AWS gave notice it takes back (the notice is recorded on the machine as
    `superci-pull-noticed`), or
  - the job's machine left before the job completed, and EC2 says a spot interruption terminated it, or
  - a dispatched job the dispatcher lost to a reclaim it learnt of (Identity plane, Reclaim): that is its proof.

  It is re-run once its run has completed (GitHub re-runs a job only in a completed run), and only if GitHub
  concluded the job `failure`; a job that succeeded, or that a person cancelled, is left alone. A start rebuilds the
  list from the notice tags of the machines EC2 still lists, about an hour after a reclaim.

### Cache seam

The S3 actions cache plugs in at three places: one line of `boot.sh` after the metadata token (`T=`) is fetched, an
`IamInstanceProfile` (read-only) in `setRegion.ensureTemplate` (`fleet.go`), and an `iam:PassRole` statement for
that profile in `deploy/listener-policy.json`. Nothing else in the listener knows about the cache.

The cache is `cache/` (its own Go module; `cache/README.md` says what it is). Its write isolation depends on one job
per machine, which is the invariant above: a machine that ran a second job could take the first job's credentials from
the daemon (`cache/SEAM.md`, "One job per machine").

Seen on the real image (Nov 25 2026): a c6i.xlarge of the fleet's image (`ami-0178d4ab7f659a2cc`), launched by hand with
the `superci-pull-cache-machine` profile and not tagged `superci-pull`, ran `boot.sh`'s two cache lines from user data.
The line exited 0, `pullcache.service` was serving from the region's bucket 3 s after boot, `externals/node24/bin/node`
was the 19 MB wrapper over the runner's `node.real`, and a token with an issuer and key id of its own was refused with
`refused: signed by a key GitHub does not list (iss "https://probe.example/issuer", kid "probe-kid")`. No job ran on it.

#### Deploy the cache seam

Deployed Nov 25 2026 12:09Z with round 2 (`pull-prod-r2`): every runner template's version 21 names the
`superci-pull-cache-machine` profile, and the machines launched since carry it. The steps below are how it was done,
and how to do it again on a fresh deployment. Every command uses `--profile ci-runners`. The order matters: the
policy before the listener, the listener last.

1. **Check the cache is there** (read-only).
   ```bash
   for r in us-east-1 us-east-2; do
     aws --profile ci-runners s3 ls s3://superci-pull-cache-243891825325-$r/bin/ --region $r        # pullcache-linux-amd64, -arm64, pullcache-lambda-arm64.zip, up.sh
     aws --profile ci-runners lambda get-function-configuration --region $r --function-name superci-pull-cache-broker --query '[State,LastUpdateStatus]' --output text   # Active Successful
   done
   aws --profile ci-runners iam get-instance-profile --instance-profile-name superci-pull-cache-machine --query 'InstanceProfile.Roles[].RoleName' --output text   # superci-pull-cache-machine
   ```
2. **Release the cache from main** (`bin/up.sh` is the commit's `machine-up.sh`; it reads the issuer from
   `bin/issuer` when one exists). A machine that boots after this starts the daemon, if it has the role. One without
   the role runs on GitHub's own cache.
   ```bash
   pull/deploy/build.sh /tmp/release
   pull/cache/deploy/release.sh /tmp/release
   ```
3. **Let the listener pass the role.** This only adds a statement; the running listener is unaffected.
   ```bash
   aws --profile ci-runners iam put-role-policy --role-name superci-pull --policy-name superci-pull --policy-document file://pull/deploy/listener-policy.json
   ```
4. **Build, test and release the listener** (the release restarts it; a graceful stop closes its sessions, the next
   opens at once). `deploy/test.sh` runs the tests of both modules on a tagged instance; `deploy/build.sh` builds
   the release, the listener and the cache's binaries (it tests `pull/` only).
   ```bash
   pull/deploy/test.sh
   pull/deploy/release.sh /tmp/release pull/deploy/ssh.sh
   ```
   The listener makes a new launch template version at start, because `sameData` finds the latest has no profile.
   Machines already running, and the idle pool from before the release, keep the old version: they run on GitHub's
   cache until they end, and a machine lives for one job.
5. **Attach the instance profile: check it took** (the template versions, then the first machines).
   ```bash
   for r in us-east-1 us-east-2; do for set in 2cpu 4cpu 8cpu 16cpu 32cpu; do
     aws --profile ci-runners ec2 describe-launch-template-versions --region $r --launch-template-name superci-pull-superci-$set --versions '$Latest' \
       --query 'LaunchTemplateVersions[0].[VersionNumber,LaunchTemplateData.IamInstanceProfile.Arn]' --output text; done; done   # .../instance-profile/superci-pull-cache-machine
   aws --profile ci-runners ec2 describe-instances --region us-east-1 --filters Name=tag:purpose,Values=superci-pull Name=instance-state-name,Values=pending,running \
     Name=iam-instance-profile.arn,Values='*superci-pull-cache-machine' --query 'length(Reservations[].Instances[])'     # grows as machines start
   pull/deploy/ssh.sh "grep -c 'iam:PassRole' /var/log/superci-pull/listener.jsonl"   # 0: no launch refused for the role
   ```
   Every launch refused with `iam:PassRole` (the listener's `fleet` events, the `LaunchShortfall` metric and its
   alarm) means step 3 did not apply: apply it again, or roll back (below).

#### Verify the first real job hits the cache

Any job of a superci set that uses `actions/cache` (or `setup-*` with a cache) on a machine launched after the release.
The runner machines take no inbound connection, so the daemon's journal is read through a temporary opening, or the
S3 and broker evidence below is read instead.

- **The entry reached S3** (definitive, needs no machine access). A save is `v1/<d|r>/<repository id>/<scope>/<version>/<key>`,
  `d` for the default branch's scope and `r` for any other:
  ```bash
  aws --profile ci-runners s3 ls s3://superci-pull-cache-243891825325-us-east-1/v1/ --recursive | sort | tail -5
  aws --profile ci-runners logs tail /aws/lambda/superci-pull-cache-broker --region us-east-1 --since 30m   # issued for <scope>, or refused with iss and kid
  ```
  The next job with the same key restores it: its restore step says "Cache restored from key" and the daemon logs
  `restore "<key>": found ...`.
- **The daemon's journal on the machine.** Find the machine (`aws ec2 describe-instances` as in step 5, by its
  `i-...` in the job's runner name), open port 22 on the runner group `superci-pull-runner` of its region for your
  address only, push a temporary key with EC2 Instance Connect, and close the port after:
  ```bash
  sudo journalctl -u pullcache -o cat     # serving GitHub's cache at http://<private ip>:8257 from superci-pull-cache-243891825325-<region> ...
                                          # restore "<key>": found ...   save "<key>": saved N MB   (a hit and a save)
  sudo journalctl -u pullcache -o cat | grep refused:     # <method> "<key>": refused: <why> (iss "<issuer>", kid "<key id>")
  ```
- **A `refused:` line names what the first job's token carries.** The daemon logs the token's `iss` and `kid`, never
  the token. Every refused job is a cache miss, never a failed job.
  - `iss` is not `https://token.actions.githubusercontent.com`: `ISSUER=<that iss> pull/cache/deploy/up.sh`. It sets
    the broker's issuer and writes `bin/issuer` in each bucket, which every machine that boots afterwards reads. Run
    `up.sh` without `ISSUER` later and the value stays; `ISSUER=default` clears it.
  - `kid` is not in GitHub's JWKS: the runtime token is signed by other keys. The broker takes
    `BROKER_JWKS_URL=<url> pull/cache/deploy/up.sh`. A machine's daemon has no knob for it yet: it needs
    `PULLCACHE_JWKS_URL` in `machine-up.sh`, and a new release of that script.

#### Roll back the cache seam

From fastest and smallest to full. None touches the listener's sessions, `SUPERCI_JOBS` or the push plane.

1. **Stop using the cache for new machines, at once, with no restart.** Remove the machine script. A machine that
   finds none starts no daemon and runs on GitHub's own cache; the profile stays attached and harmless.
   ```bash
   for r in us-east-1 us-east-2; do aws --profile ci-runners s3 rm s3://superci-pull-cache-243891825325-$r/bin/up.sh --region $r; done
   ```
   Put it back with step 2. Machines already running keep their daemon until they end.
2. **Take the profile off the templates.** Roll the listener back to the previous commit with the Roll back recipe
   below (a release of an older commit of main). The previous binary embeds
   the old `boot.sh`, so its user data differs from the latest template version and it makes a new version, without
   the profile and without the daemon line.
3. **Only then drop the `PassCacheRole` statement**, if wanted: `git show pull-prod:pull/deploy/listener-policy.json`
   to a file, then the `put-role-policy` of step 3 with it. Dropping it first makes every launch of a template that
   still names the profile fail.
4. **Remove the cache itself** (buckets stay): `pull/cache/deploy/down.sh`. Nothing in the listener needs it gone.

## Identity plane

GitHub caps runner registrations at 1,500 in 5 minutes for the organization, and the organization sat at that cap at
peak on Nov 26: superci jobs waited up to 300 s for a registration and the canaries were rolled back. A size whose
entry in `config.json` says `"dispatch": true` registers nothing per job. Persistent runner identities, registered
once on identity hosts, take its jobs, and each job still runs on a fresh single-use machine. A size moved to dispatch
has no scale set and no message session; its old scale set drains and is deleted, as a dropped set is. A dispatch
size is one set (`split` 1), since an identity names its size, not a set of it.

- **Identities.** An identity is a classic runner named `superci-id-<size>-<n>`, labelled with its size, on an
  identity host tagged `superci-pull-role=identity` and `superci-pull-deployment=<deployment>`. Its `bin/Runner.Worker`
  is the shim (`shim/`). The identity's `.credentials` and `.credentials_rsaparams` never leave its host; its
  `.runner` holds no secret and goes to the machine.
- **The dispatcher** is in the listener, on its TLS port (8443). A machine POSTs `/work` and a shim POSTs `/shim`,
  each with its EC2-signed identity document as the body (16 KiB at most), `Connection: Upgrade` and
  `Upgrade: superci-dispatch/1`, over HTTP/1.1. The answer is `101` and frames follow:
  `<u8 type><u32 little-endian length><payload>`.

  | Type | Name | From | Bound |
  | ---: | --- | --- | --- |
  | 1, 2 | STDOUT, STDERR | machine | 1 MiB a frame; 4 MiB a job together, past which frames are dropped and counted |
  | 3 | EXIT | machine | exactly 4 bytes (i32); ends the job; one a job, the rest dropped |
  | 4 | INFO | machine | 4 KiB a frame, 64 a job, a JSON object of `instance`, `boot`, `t_hdr`, `t_spawn`, `t_exit`, `rc` only |
  | 5 | PIPE | shim | 1 MiB a frame; 33 MiB held while the job has no machine |
  | 6 | JOB | dispatcher | 64 KiB: `{"runner", "token", "job": {run, job, name, repo}}` |
  | 7 | PING | both | empty, every 20 s; 60 s of silence ends the connection |
  | 8 | PAIRED | dispatcher | 1 KiB: `{"instance", "token"}` |

  Any other type, or a frame past its bound, ends the connection. What a machine sends while its shim is away is held
  for the shim up to 8 MiB, each frame counted at its payload and 64 bytes more, so a flood of empty frames is bounded
  too; past it frames are dropped and counted, and the one EXIT is always held. A `/shim` caller must be an identity
  host of this deployment and send its `.runner` in base64 as
  `X-Superci-Runner`; the agent name decides the set, and a `.runner` holding a field of the credential files is
  refused (403). Any instance of the account can call `/shim`, job code included, and each read of its tags spends
  the account's EC2 Describe budget: an identity host's tags are trusted for 5 minutes, an instance that is not one is
  refused for 1 minute without asking EC2 again, and at most 4 reads run at once, past which a caller gets 503. A
  `/work` caller must be a machine of a dispatch set that never held a claim (410 otherwise).
  `/agent` answers a dispatch machine with `agent.py`.
- **Pairing.** Each dispatch set keeps a queue of waiting machines, oldest first (the warmest), and a queue of jobs.
  The dispatcher reads the job's id and names from the listener's first message (`NewJobRequest`, at most 32 MiB) and
  keeps no secret field of it. A shim whose connection broke before PAIRED replays its job on a new connection: the
  replay takes the queued job's place, so a job is one job however often it arrives. To pair, the dispatcher takes the
  machine's claim (`issued/<instance>`, made once, holding the pair and the job's names) with a fresh token, sends JOB
  to the machine and PAIRED to the shim, takes the job's own claim (`issued/job-<job id>`, holding `<instance>.<token>` and, on a second line, the
  job's run, name, repository and identity, so a job lost after a restart can be re-run), and writes the first PIPE
  byte. A claim whose answer S3 lost is asked again with the same token, which finds it taken by that token if it
  landed: a machine's claim 3 times before the machine is spent, the job's claim for up to 3 minutes. Past those 3
  minutes the shim may have given up, so the job is stranded instead (Resume), once S3 answers: the first of the two
  claims to land decides whether the job was handed. A job that has
  spent 3 machines on claims S3 never answered waits 30 s before each next one, so a lasting S3 fault does not churn
  the set's machines. **The job's claim is the handed point.** Before it, any failure spends the machine (it is ended
  and never offered again) and puts the job back at the head of its queue. After it, the job goes to no other machine,
  across restarts and overlapping listeners: a replay of a handed job is sent the PAIRED of the pair that has it. A
  stop lets the hand-offs in flight finish for 5 s first.
- **Resume.** A side whose connection broke after the handed point reconnects with `X-Superci-Resume:
  <instance>.<token>` for up to 3 minutes. Each retry waits a random time up to a ceiling that doubles from 1 s to 15 s with
  each failure in a row (the shim and the agent both; the agent also waits at least a 503's `Retry-After`), so a
  restarted listener is not met by every side at once, and none starts a retry after the 3 minutes end. The token carries the job's id, so a new dispatcher reads the job's claim:
  it must name the pair (it was taken only once the machine's claim held that token), or the answer is 410. A pair
  never handed is also 410: the shim drops its job's bytes once PAIRED comes, so it cannot start the job again. A job
  with no claim at all is **stranded** on its first resume, by a side whose pair the machine's claim names: the job's
  claim is taken with no pair (naming the stranded pair on its second line), so no pair can be handed the job after,
  and the job is reported among the set's lost jobs, marked unrun. Every strand of one pair writes the same claim, so a
  lost answer or a strand from each side finds it its own. Bytes
  in flight at the break may be lost; the shim sends what it read while away, and the machine sends its EXIT again
  until a clean close follows it. A PIPE frame whose send failed is sent on the machine's next connection: a failed
  send puts no whole frame on the wire, and the agent acts only on whole frames, so no byte reaches the Worker twice.
- **Ending a connection.** Once the EXIT reaches the shim, both connections end with TLS close_notify and a FIN, and
  the dispatcher reads and drops what each peer still sends for up to 5 s before it closes the socket: a socket closed
  with bytes unread sends a reset, which discards the EXIT still on its way. A stop starts no hand-off, lets those in
  flight finish for 5 s, and ends every connection, which each side rejoins on the next dispatcher.
- **Loss.** A machine away for 3 minutes with no EXIT, a job past `job_minutes` + `margin_minutes`, or a machine
  past its life loses its job: the shim gets exit code 1, which is no TaskResult, and the identity's listener fails
  the job. A reclaim loses it sooner (below). A shim that is away loses nothing: the machine renews its job itself, so the job runs on to its EXIT
  however long the shim stays away. An EXIT that comes while the shim is away ends the machine and is held for the
  shim until 3 minutes after the shim left. A spot reclaim's lost job is re-run as a scale set's is, known by its
  machine and found in its run by its name (an identity takes many jobs). A stranded job is re-run the same way with no
  reclaim to prove: no machine ever got its bytes, so its failure is superci's own.
- **Reclaim.** AWS's notice that it takes a dispatch machine back reaches the dispatcher from the machine: boot.sh
  watches IMDS's `spot/instance-action` and posts `/notice` with its identity document and the instance-action
  (`X-Superci-Action`), again until the listener answers. The dispatcher acts on the document's instance only, and
  refuses (400) a notice from an on-demand machine, which AWS does not reclaim, or an instance-action whose time is more
  than a minute past or three minutes ahead, which no notice AWS gives now has: a stale one replayed. A notice that did
  not come is caught by the listing of EC2 the reconciler makes anyway (every 10 s, at most once a second a region): it
  also asks for `shutting-down` and `stopping`, a few more instances on the same pages, and a dispatch machine it shows
  leaving with a `Server.SpotInstance*` state reason is reclaimed; one leaving for another reason is left to the window.
  A reclaimed machine with no job ends at once; one being paired is spent and the job paired again, unless PAIRED had
  gone, when the job is lost at its handed point. A handed job is lost at once, waiting on no write to the machine: no
  frame starts on its connection after, its shim gets exit code 1, the machine ends (terminated by the next pass), and
  the job is reported with the reclaim as the proof its re-run needs, since it is failed before AWS takes the machine
  and its run may complete before EC2 could prove the reclaim. Its notice tag is not written, so a restart before the
  re-run forgets it, as it forgets a job lost at the window.
- **Machines.** A dispatch machine carries `superci-pull-mode=dispatch`, so a scale set and a dispatch set of one size
  never take each other's machines. Its demand is the jobs waiting plus the jobs running, and its pool follows the
  jobs that arrive at the dispatcher, by the same rule as a scale set's (Pools). It ends when its EXIT has been
  relayed or held for a shim that is away, when it has had no connection for 3 minutes (and is 8 minutes old: its warm takes up to 5), when it
  has waited past `max_age_hours`, past its life, or as surplus: the newest machines without a job first, after 20 s
  of surplus, never in the first window after a start. It holds no runner, so its end needs no GitHub fence. A
  dispatch machine left with no dispatch set, as when its size moved back to a scale set, may be running its job: it
  is ended only past a machine's life, not at 10 minutes as other machines of no set are, and it powers off by itself.
- **Names.** The lister takes a set's runners by `<set>-i-`, the form of a machine's runner. An identity is never a
  machine's runner or an orphan, and nothing removes one.
- **Metrics,** per dispatch set under the `Set` dimension: `JobsWaiting`, `Machines`, `Pool`,
  `OldestWaitingJobAgeS`, `DispatchIdleMachines`, `DispatchRunning`, and each minute's `DispatchHandoffs`,
  `DispatchRetries`, `DispatchResumes`, `DispatchLostJobs` and `DispatchDroppedFrames`. Their jobs' waits for a machine
  are in `QueueWait`, and their oldest wait in the alarms' `OldestWaitingJobAgeS`.
- **Limits.** A crash in a hand-off fails that job once (Known limits). A dispatch set's pool is carried across a
  restart as a scale set's is (`Carry`, `Resume`); it starts from the saved burst. Moving a size back to a scale set fails the jobs its machines run.

## Hydration

A machine idles in its set's pool until GitHub hands it its one job. With a manifest for its label, it spends part
of that time unpacking main's caches, so the job's setup takes them with a rename instead of a download and an untar.

- **What.** `bin/hydrate/<label>.json` in the region's cache bucket (the machines' role reads only `bin/` and `v1/`).
  An entry is a cache entry's whole identity: its scope, its version and its key. `key` is a restore key (`*` stands
  for any run of characters) and `paths` are the lines of the cache action's `path` input, as a job passes them: the
  version is `sha256(paths + compression + "1.0" joined by "|")`, as `@actions/cache` makes it, and `compression` is
  `zstd-without-long` on the runner image (read off main's bucket: the git seed's `.git` is version `e147b628…`). The
  machine lists that version's keys under the restore key's literal prefix, in each of `scopes` in order (as a job's
  restore reads its own scope and then main's), and takes the newest match. `checkout` also checks out the git work
  tree of the entry (a seed of `.git`), so a checkout over it writes only what its commit changes.

  ```json
  {"repository": "1351063449", "scopes": ["refs/heads/main"], "workspace": "/home/runner/_work/megpt-mono/megpt-mono",
   "entries": [{"key": "git-seed-v1-Linux-X64-", "paths": [".git"], "checkout": true},
               {"key": "bun-modules-v1-Linux-X64-bun*-aura-hono-api-scripts-on-", "paths": ["aura-hono-api/node_modules"]},
               {"key": "bun-modules-v1-Linux-X64-bun*-aura-hono-api-mind-scripts-on-", "paths": ["aura-hono-api/mind/node_modules"]},
               {"key": "mind-python-v1-Linux-X64-", "paths": ["/home/runner/_work/_temp/harness-python"]},
               {"python": "3.13"}]}
  ```
  `python` installs that CPython into the machine's tool cache as actions/setup-python would, so a job's
  setup-python of that version finds it. Every scope must be the default branch's: only main's jobs write it, so no
  PR's save can become what another machine's job starts from. `pullcache hydrate` refuses any other scope unless
  `PULLCACHE_HYDRATE_ANY_SCOPE=1`, which only a dev listener's A/B sets.
- **When.** `boot.sh` registers the machine's runner at once (launch to runner online is what a plain machine's is)
  and starts `pullcache hydrate` beside it, in its own process group, as the `runner` user (`setpriv`, which execs in place, so the group stays whole), for at most 100 s. Hydration
  never gates registration and a failure of it never blocks the machine. Each entry is unpacked in
  `.tmp-<version>-<key>/` and renamed into place whole, so a job sees all of an entry or none of it, whenever it starts.
  `<root>/.hydrated.json` says what became of each entry, how long it took and whether it was stopped. When the
  listener keeps the job timeline's spool of marks (`/run/superci-marks`), the machine posts `hydrate.start` and, when
  hydration ends on its own, `hydrate.done` with `<entries ok>/<entries>:<MB>MB` as its value; a start with no done and a
  `job.running` is a hydration the job stopped.
- **Stop.** `hydrate-stop.sh` ends the whole hydration group (SIGTERM, then SIGKILL after 2 s) as soon as the runner
  takes its job: `boot.sh` runs it, as the runner's user, on the runner's `Running job:` line. The runner's
  `ACTIONS_RUNNER_HOOK_JOB_STARTED` runs it again, which is too late on its own: the hook runs after "Set up job", which
  a running pre-read slowed from under 1 s to 19 s (Nov 26, canary 4). So hydration never competes with the job for CPU or disk.
  The job takes the entries already in place and restores the rest itself. A hydrator that already ended leaves the hook
  nothing to do.
- **Pre-read.** A volume made from the image's snapshot is restored as its blocks are first read: a first read costs 5 to
  16 ms (60 to 200 IOPS at one at a time) where a restored block costs 0.5 ms, and libLLVM's 123 MB took 0.3 s or 3.6 to
  9.2 s. When hydration ends (or at once with no manifest) the same process reads the root's snapshot-backed part, the
  29 GB the root had before it grew, with `fio` at 32 reads in flight: 120 to 140 s at the volume's 250 MB/s. It costs
  nothing (EBS reads are free), and 85% of jobs start on a machine older than that (Nov 26, 1,042 jobs: p50 303 s). On
  machines that had finished it, first reads ran at 1,600 to 2,100 IOPS, libLLVM took 0.18 to 0.34 s and 1 GiB read one
  request at a time took 3.2 to 3.4 s (3.1 s or 20 to 27 s without). The stop's removal of the pid file ends the read
  within half a second of the runner taking its job, so a job that starts first never shares its volume with it. The machine posts `preread.start`
  and `preread.done`, with `full` or `stopped` as its value.
  EC2's `VolumeInitializationRate` (100 to 300 MiB/s in the launch template, which RunsOn's snapshot accepts) ends
  no sooner (128 s at 200 MiB/s, 85 s at 300) and bills $0.0024 or $0.0036 per GB of the snapshot's 26.9 GB on every
  machine: $0.065 to $0.097 each, about $9,300 a month at 4,800 machines a day. It also did not speed first reads before it finished. Fast snapshot restore
  on a copy of the snapshot is $0.75 per AZ-hour (9 AZs: $4,860 a month) and its credit bucket for a 30 GiB snapshot
  refills 10 volumes an hour per AZ.
- **Read set, instead of the pre-read.** A size with a read set (`readsets/<label>.list`, released as `bin/warm/<label>.list`
  in the cache tree) reads only the image files its jobs were recorded reading, 64 at a time through the page cache, before
  the hydration starts. It skips the pre-read, the walk and the one-file-at-a-time warm, then starts dockerd. A unit-tests
  shard read 1.2 GB of the snapshot's 25.0 GiB of blocks. On 15 fresh m8azn.3xlarge and m8a.2xlarge machines its list took
  1.0 to 6.8 s (p50 2.2 s), against 270 s for the pre-read on the same volume and 172 s p50 in production (Nov 27). The
  machine posts `readset.start` and `readset.done` (`full`, `full:missing` when some paths are not in the image, or
  `stopped`). A path the image lacks costs one failed open; a file outside the list reads cold, as it would with no
  pre-read. A size without a list, or with an empty one, pre-reads as before; asking for its list costs one S3 GET.
  [tools/readset](tools/readset/README.md) records a list and proves that it holds only the image's files.
- **Burst.** An entry is read in ranges of 64 MiB, three ahead, and each machine waits a random 0 to 3 s before its first
  read: a thousand machines make about 16 requests per GB each to one prefix, not 64.
- **Where.** `/home/runner/hydrated/<version>/<key>/`, in which that directory stands for `/`: an archive's files are
  where actions/cache would restore them, under that prefix. The runner's `.env` gives every job
  `SUPERCI_HYDRATED=/home/runner/hydrated` (set whether or not a manifest exists, and empty without one). A job takes a path P saved
  from the path set `S` under key K by `mv $SUPERCI_HYDRATED/<version of S>/K/P P`, where it computes the version as above from
  the same `path` lines its own restore passes, and restores as before when it is not there (megpt-mono's `bun-install`,
  `mind-python-env` and lint-test-backend's git seed steps do).
- **Why it is safe.** It reads only S3, with the machine's read-only role, and only the manifest's scopes: main's
  entries are written only by main's jobs (the broker's per-job credentials), so nothing a pull request's job writes
  is hydrated anywhere. What a machine hydrates serves its one job; a machine runs one job, ever (Invariants).
- **Cost.** None at registration. A job that lands before an entry is in place restores it as it did. S3: about 1 GB of
  GETs per machine (gateway endpoint, no transfer charge).
- **Turn it on.** `aws --profile ci-runners s3 cp manifest.json s3://superci-pull-cache-243891825325-<region>/bin/hydrate/<label>.json`
  for each region; machines launched afterwards hydrate. It needs this branch's listener (the label and the hook in each
  set's user data) and its `pullcache` in `bin/`.
- **Turn it off.** Delete `bin/hydrate/<label>.json`; machines launched afterwards hydrate nothing. A job on a machine
  that hydrated nothing restores as it did.

## Identity hosts

The identity plane takes jobs without spending a registration on each. GitHub caps registrations at 1,500 in 5
minutes for the organization, and at peak the fleet already reaches it. An **identity** is a classic runner registered
once (`config.sh --disableupdate --no-default-labels`, one label) whose `bin/Runner.Worker` is the shim, which hands
each job to a fresh machine. Its listener runs on an **identity host**, and no job code runs there. The identity's
`.runner`, `.credentials` and `.credentials_rsaparams` never leave the host and S3.

- **Sizing.** Peak concurrency per size (Blacksmith, Nov 23-26) at 2/4/8/16/32 vCPU is 146/427/346/71/38. With 1.5x
  headroom that is 220/640/520/110/60, or 1,550 identities. Measured on the lab host after 3 jobs each, a default
  listener grows from 36 MiB private to 74-106 MiB. With the GC settings below one held 46 MiB, so the budget is **64 MiB
  private per identity plus 60 MiB shared per host**: 1,550 identities are about 97 GiB. That is two `r7i.4xlarge`
  (128 GiB each, 48 GiB of listeners apiece, and either one alone could hold all 1,550 while the other is replaced),
  on demand, in two AZs. Four `r7i.2xlarge` cost the same and cut a lost host to a quarter of the plane instead of a
  half, but need four AZs, so `up` defaults to the larger type and takes `--type` for the other.
  `identity.sh status` reads the real figure (Private_Dirty of each `Runner.Listener`, and how many are over 64 MiB).
- **Memory.** Each identity's unit `superci-id@<name>` sets `DOTNET_gcServer=0 DOTNET_GCgen0size=0x200000
  DOTNET_GCHeapHardLimit=0x6000000 DOTNET_TieredPGO=0 DOTNET_TC_QuickJitForLoops=1 DOTNET_GCConserveMemory=7` and
  `MemoryMax=192M`, with `Restart=always` and `RestartSec=5`: a listener that leaks is restarted, not left to starve the host.
  An identity GitHub has removed stops at `StartLimitBurst=10` in `StartLimitIntervalSec=600`, not crash-looping. The
  unit keeps systemd's default `KillMode`: a stop or restart takes the listener and its shim down together, so two
  listeners never share credentials, and a shim that dies alone loses no job (its machine renews the job itself).
- **Where an identity lives.** Identity `i` (the number in its name) is on the host of `IDENTITY_AZS[i mod n]`, whichever
  hosts are up. Its files are in `s3://superci-pull-identities-<account>/<deployment>/hosts/<az>/<name>/`, SSE-KMS under
  the customer-managed key `alias/superci-pull-identities` (made once by `up`; its policy names only the identity role,
  to encrypt and decrypt through S3, and the operator, to administer). The hosts' root volumes are encrypted. A host that
  replaces another in the same AZ restores that share at boot and registers nothing.
- **Who may read them.** The host's role `superci-pull-identity` may read and write that prefix and read the released
  shim and `bin/RELEASE.json`, nothing else (`identity-policy.json`; its KMS statement is that one key, for objects of
  that prefix only). The bucket policy
  (`identity-bucket-policy.json`) blocks public access, refuses a put that is not SSE-KMS or not the role's, refuses a
  read that is not the role's, and refuses a delete that is not the operator's. The host starts a shim only if its
  sha256 is the one `bin/RELEASE.json` names, as the listener does for its own binary.
- **The image** is the machines' (`runs-on-v2.2-ubuntu24-full-x64`, pinned as `fleet.go` pins it), so the runner is
  the same version. IMDSv2 only, hop limit 1. Tags: `superci-pull-role=identity`, `superci-pull-deployment`,
  `purpose=superci-pull`, `Name=superci-pull-identity-<az>`. The shim's `/shim` endpoint serves only an instance with
  the first two.
- **Self-healing.** Each AZ's host is the one instance (min = max = desired = 1) of an Auto Scaling group
  `superci-pull-identity-<az>`, from a launch template of the same name that carries the host's whole configuration and
  its user data (the gzip'd `identity-host.sh`, the pin, the key). EC2 replaces a host that is terminated or fails its
  status checks, and the group is set to terminate before it launches (`MinHealthyPercentage=0,MaxHealthyPercentage=100`),
  so the old host is ending before the new one starts. The replacement boots from the template exactly as `up`'s host did:
  it restores its AZ's share from S3 and registers nothing. Measured Nov 27 for a planned rebuild of a 295-identity host:
  terminate 34 s, launch +16 s, restore 213 s, GitHub online within 20 s after it, 284 s in all, the other AZ's host
  carrying the peak meanwhile (the 2x sizing above). Nothing is rolled by hand and nothing alerts by ruling.
- **A host that runs but whose listeners do not.** `superci-id-health.timer` runs `superci-id health` every 30 s, in the
  manner of the listener's `health.sh`, and judges nothing before the host is 15 minutes old (a restore takes minutes).
  **Healthy** is: boot finished (`/run/superci-id-ready`), every identity of the AZ's share stored in S3 restored on the
  host, and every listener running but one in fifty (rounded down, so none under 50 identities: an identity GitHub removed
  stops at its start limit). A host that is not healthy for 60 s is **revived** once per episode: boot is run again as a
  process of its own (so its errexit applies; it removes the ready file first and touches it only once every stored identity is
  restored and started, so a boot that fails leaves the host not ready), or, when all are restored and too few run, every unit
  is started. If it is still unhealthy 120 s after the revive ends (and the host is 20 minutes old) it **stops every
  listener**, so the host that goes holds no session at GitHub while its replacement boots (the group does not wait for the
  old instance's shutdown), then marks its own instance Unhealthy (`autoscaling:SetInstanceHealth` on
  `superci-pull-identity-*`, the role's one non-S3, non-KMS statement; `--no-should-respect-grace-period`), once, and its
  group replaces it. The health unit may run 2,700 s: its revive reruns boot, which restores 1,550 identities (a host alone
  after the other AZ's is lost) in about 1,120 s (213 s for 295, extrapolated), twice that. A boot or register
  that holds the host's lock is busy, not sick, until a **boot** has held it for 40 minutes (boot writes its start to `/run/superci-id-booting`; a boot an operator runs on a host that has been up for days is not mistaken for one that never ends). Listeners are stopped, and the instance marked, only when it is in this AZ's group (the role may read its own group, `autoscaling:DescribeAutoScalingInstances`); a plain host's health check says so and stops nothing. Boot and `check` refuse a host that cannot read its AZ (`us-east-1[a-f]`), and `check` fails when the share cannot be listed in S3, so `up` adopts nothing it cannot verify. A host whose
  boot always fails (a shim the release does not name, say) is replaced again and again, no oftener than every 20 minutes: read its
  boot log, `/var/log/superci-id-boot.log`.
- **Hardened at boot.** The image leaves much of `/usr/share`, `/usr/local` and `/opt` writable by others (mode 777)
  and its runner tree the runner user's, so boot runs `superci-id harden` first. Instance Connect's key command and
  every directory above it become root's, writable by neither group nor others (sshd refuses it otherwise, so no
  operator could log in); then every path others may write, outside `/proc`, `/sys`, `/dev`, `/run` and sticky
  directories, loses group and other write, and the runner tree becomes root's. Boot fails while a path stays writable
  by others or the chain is not root's alone. A path writable by its group alone is left.

```bash
pull/deploy/identity.sh up us-east-1a                         # bucket, role, launch template, group of one, host (r7i.4xlarge; --type T for another); waits until it has restored its share
pull/deploy/identity.sh register 4cpu 100                     # superci-id-4cpu-0001 .. 0100, label superci-park
pull/deploy/identity.sh register --rate 0.25 4cpu 100 101     # the next hundred (the default rate; 1 a second at most)
pull/deploy/identity.sh relabel 4cpu superci-park superci-4cpu   # cutover of a size (after deleting its scale set)
pull/deploy/identity.sh relabel 4cpu superci-4cpu superci-park   # rollback (then recreate the scale set)
pull/deploy/identity.sh status                                # groups, hosts, identities per host, listeners, memory against 64 MiB, labels at GitHub
pull/deploy/identity.sh down us-east-1a                       # group to 0 and deleted, host terminated (not healed back); identities stay, `up` restores them
pull/deploy/identity.sh up --replace us-east-1a               # a plain host from before the groups: end it, then let the group launch (see below)
```

**`up` and the template.** `up <az>` makes or updates, each time and without change when nothing changed, the role,
key, bucket, the launch template `superci-pull-identity-<az>` (tagged `superci-pull-role=identity`, which the listener's
role may neither launch from nor change, as for its own template) and the group. The template's data is hashed into its
version description: the same data adds no version, changed data (the host script, the pin, the image, `--type`) adds one
and makes it the group's default. A running host is not touched by it: the next host the group launches takes it
(`down <az>` then `up <az>`, or terminating the host, takes it now). Pass `--type` again each time, or the next version
is the default type. `down <az>` sets the group to 0, ends the host and deletes the group, so nothing launches it again
(`deploy/down.sh` deletes the identity groups before it terminates every instance, too); the template stays.

**Adopting a host from before the groups.** Two Runner.Listeners on one identity conflict at GitHub (a session for the runner
exists already), so an AZ's old host and a new one must never restore one share at once. `up <az>` on an AZ
whose host is a plain instance (the `Name` tag, no group) therefore does not launch another: it makes the group empty
(min 0, desired 0, so nothing launches), **attaches the running host to it** (`attach-instances`), then sets min 1. The host
is the group's instance from then on, no identity goes offline and nothing is restored. Before that `up` runs `superci-id check` on it (the same rule as the health check) and **refuses** to adopt a host that is not healthy: the health timer it is given would revive, then replace, a live host. Make it healthy (`sudo superci-id boot` on it restores and starts what is missing) and run `up` again, or use `--replace`; `up` pushes it this checkout's
`superci-id` and installs its health timer (`superci-id install-health`). Run it one AZ at a time, `status` between. If the
attach is refused the host still runs alone and the group holds nothing: run `up` again, or `up --replace <az>`, which
terminates the plain host, waits for it to be gone, and only then lets the group launch its own (the identities of that AZ
are offline for the rebuild, 284 s as measured, the other AZ carrying the peak). The order **launch the group's host, then
terminate the plain one** is never used, however short: the new host restores and starts the share 213 s before the old
one is gone. `up` refuses outright while a group's host and a plain host of one AZ both exist (run `down <az>`, which ends
both, then `up <az>`). `up` has no way left to make a plain host: a group is how it launches one.

**What this does not cover.** A host that is impaired but still running when EC2 replaces it may keep its listeners
for the seconds before it is gone (terminate before launch holds for the group, not for a hung kernel); its replacement
then meets GitHub's session conflict on those identities until the old session ends. The health check cannot see a listener that runs and is offline at
GitHub (that is the `identity-none` and `identity-starved` alarms' to catch).

**Registering.** Every registration spends the organization's cap, so `register` is bounded four ways:

- At most `--rate` registrations a second over all hosts (0.25 by default, never above 1): one identity at a time,
  each started at least `1/rate` seconds after the last. 1,550 identities take about 1.7 hours at the default.
- In leases of at most 20 (fewer at a low rate, so a batch fits the lease's 10 minutes), from the listener's
  `/identities/token`. Each lease is settled with the count made before the next is asked for. The listener grants
  at batch priority, never a token a waiting machine is owed. A registration starts only while its lease has longer
  to run than the registration may take (`REGISTER_TIMEOUT`, 180 s: its ssh, and `config.sh` inside it at 120 s);
  otherwise the lease is settled and a new one asked for.
- Identity `i` lives on `IDENTITY_AZS[i mod n]` (`us-east-1a us-east-1b` by default), whichever hosts are up, and
  `register` refuses while any of those hosts is down.
- One run at a time: `identity.sh` holds `flock` on `IDENTITY_LOCK`, and each host script holds its own, so two runs
  never delete each other's `.runner` or `.credentials`.
- Stopped by the first failure. A `config.sh` that exits non-zero (a 429 or `ConfigureTooManyRunners` in its log is
  named as GitHub's refusal) ends the command, settles the lease with what was made, and exits 1 naming the identity.
  A refusal is settled with `refused=1` (and `failed=<n>` for other failures), which halves the budget's rate and holds
  its asks as the listener's own refusal does, and the operator is told to wait 5 minutes, not to run it again at once.
  Otherwise run the same command again to continue: names already held, in S3 and at GitHub, are skipped, and one registered
  whose upload failed is stored without a new registration.
- Held back at peak unless `--peak-ok`: it waits (30 s a poll, 30 minutes at most) while the listener's `/state` shows
  the budget throttled after a refusal, the organization's rate unknown, or above 3.5 a second.

It prints the identities made, the time taken and the time the rest would take at that pace. `n` is at most 100 a
call and `first` defaults to the number after the highest identity of the size, so a later call continues a size.
The token goes from the listener's admin endpoint to the host's stdin, and to `config.sh` in its environment
(`ACTIONS_RUNNER_INPUT_TOKEN`): never in an argument, a file or the output.

**Cutover and rollback of a size.** Delete the size's scale set, then `relabel <size> superci-park superci-<size>`.
Rollback relabels back to `superci-park` and recreates the scale set. A relabel makes no registration: it is one
REST write per identity, at most one a second (60 a minute). It stops at the first 403 or 429, keeps the Retry-After
(the listener passes it on; 60 s when GitHub names none) beside the lock, refuses to start inside it, and resumes where
it stopped when run again. `/identities/labels` reads the runner first and refuses (403) any name that does not start
`superci-id-`, so a just-in-time runner is never relabelled.

**The listener's endpoints** (admin, localhost; from another host through `deploy/ssh.sh`):

```bash
curl -s -X POST 'localhost:8080/identities/token?n=20'          # {"lease","granted","token","expires","url","group","group_name"}
curl -s -X POST 'localhost:8080/identities/labels?id=<runner id>&labels=superci-4cpu'   # 204; replaces the runner's labels
curl -s localhost:8080/identities | jq                          # the group's superci-id-* runners, labels and status
```

`/identities/token` is a lease of `n` (1 to 100) at batch priority (`/lease`, above) and, when any is granted, a
registration token for the organization (`POST /orgs/<org>/actions/runners/registration-token`), after making sure the
runner group `<deployment>-identities` exists, open to the config's repositories only. A lease of nothing mints no
token. A token that cannot be minted gives the lease back. The token is never logged. With `"scope": "repository"` in
the config the token and the labels are the first repository's, and the group is the Default group (1).
`deploy/identity_test.sh` runs the scripts end to end against stubs of AWS, SSH, `config.sh` and these endpoints.

## Operate

```bash
pull/deploy/ssh.sh                               # a shell on the listener: a key made for this session (EC2 Instance
                                                 # Connect), from the admin address up.sh allowed; no key pair is kept
tail -f /var/log/superci-pull/listener.jsonl    # JSON events: message, fleet, register, ask, budget, state, retire, terminate
                                                # (also in CloudWatch Logs, group /superci-pull/listener, a stream per instance)
curl -s localhost:8080/state | jq               # budget, spend, and per set: ready, desired, live, idle, busy, issued, pool, orphans
curl -s localhost:8080/healthz                  # what the health timer checks (liveness: the reconciler finished a pass within 2 minutes)
journalctl -u superci-pull-health -n 20         # what it saw: failing for N s, restarting, asking to be replaced
```

**Change pool sizes, caps or limits.** Change the override file, `deploy/override.json`, on main, and release (Release,
below; it checks the file with the new binary on the listener before it uploads it). Every field it names replaces the built-in one (`config.json`), and a `pools` or `sizes` entry replaces that
size's whole entry (name every field of it). A field the listener does not know, or an hourly array of other than 24
values, is refused. The instance fetches the file every minute and installs it only if it is valid (`sync.sh`); the
listener applies pools, caps, the registration budget and machine limits within 30 s; sizes, splits, types, disks,
regions, the deployment and its repositories apply at the next start (`sudo systemctl restart superci-pull`).

```json
{"daily_usd": 500,
 "pools": {"superci-4cpu": {"floor": [2,2,2,2,2,2,2,2,2,2,2,2,2,2,2,2,2,2,2,2,2,2,2,2],
                            "cap":   [40,40,40,40,40,40,40,40,40,40,40,40,40,40,40,40,60,60,60,60,60,60,60,60]}}}
```

A pin until the file next changes: `curl -s -X POST 'localhost:8080/override?set=superci-4cpu&pool=20&max=300'`
(`pool=-1` drops the pin).

**Roll back a config.** Release an older commit of main (Roll back, below); the instance installs it within a minute.

**Read metrics.** The dashboard `superci-pull` (us-east-1) shows the alarms, queue wait, jobs waiting and machines per
set, the registration rates, registrations and errors, spend against the daily cap, and the listener's warnings.
CloudWatch, namespace `superci-pull`: `Heartbeat`, `OrgRegistrationRate`, `OthersRegistrationRate`,
`OurRegistrationRate`, `SpareRegistrationRate`, `RegistrationWindowUse`, `OrgRateKnown`, `AdmitRate` (the admitted
rate a second), `RegistrationQueue` (machines that wait in order), `Registrations`,
`RegistrationRefusals`, `RegistrationErrors`, `LaunchShortfall`, `QueueWait` (seconds per started job, with
percentiles, and again per set under the dimension `Set`), `StalledSets` (also per set, under the dimension `Set`: 1 while that set's jobs have waited 10 minutes with none started), `Orphans`, `SpendTodayUSD`, `SpendDailyCapUSD`, `SpendMonthUSD`, `SpendPerHourUSD`,
`SpendCapped`, `Machines`, `OnDemandShare`, `OldestWaitingJobAgeS` and `OldestUnregisteredMachineAgeS` (the longest
waits in progress of any set); `BudgetDenied` (registrations the budget turned away, GitHub never asked; dimension
`Reason`: `tokens`, `throttled`, `queued`); per set (dimension `Set`): `Machines`, `IdleRunners`,
`BusyRunners`, `Pool`, `JobsWaiting`, `OldestWaitingJobAgeS`, `OldestUnregisteredMachineAgeS`; per dispatch size (dimension `Set`) `IdentitiesOnline`
and `IdentitiesBusy`, from one listing of the identities group every 5 minutes (a count older than 15 minutes is not published). A wait is published
while it lasts: `OldestWaitingJobAgeS` is the age of the oldest job that was offered or assigned and has not started,
and a machine still told to wait writes a `jit.wait` timeline event every 30 s with the reason.

**Alarms** (`deploy/alarms.sh`, topic `superci-pull-alarms`). The topic has no subscriber until one is added:

```bash
aws --profile ci-runners --region us-east-1 sns subscribe --topic-arn arn:aws:sns:us-east-1:243891825325:superci-pull-alarms --protocol email --notification-endpoint <address>
```

| Alarm | Fires when | Rolls a canary back |
| --- | --- | --- |
| superci-pull-listener-down | no heartbeat for 3 minutes | yes, every canary |
| superci-pull-jobs-stalled | any set's jobs have waited 10 minutes with none of its jobs started, for 2 minutes (listener-wide: a page) | no |
| superci-pull-jobs-stalled-`<size>` (2cpu to 32cpu) | that set's own `StalledSets` (dimension `Set`) at 1 for 2 minutes | yes, a canary whose job ran on the set |
| superci-pull-registration-refusals | GitHub refused a registration in a 5-minute period | no |
| superci-pull-registration-errors | a registration failed otherwise in a 5-minute period (GitHub's API, a name GitHub already held, the issued tag refused) | no |
| superci-pull-oldest-job-wait | the oldest job waiting for a runner, of any set, is over 60 s old, two minutes running (listener-wide: a page) | no |
| superci-pull-oldest-job-wait-`<size>` (2cpu to 32cpu) | the oldest job waiting for a runner of that set (`OldestWaitingJobAgeS`, dimension `Set`) is over 60 s old, two minutes running | yes, a canary whose job ran on the set |
| superci-pull-queue-wait-p90 | queue wait p90 over 60 s in two 5-minute periods running | no |
| superci-pull-orphans | orphans above 0 for 10 minutes (a probe runner left registered more than 3 minutes counts, and the lister removes it) | no |
| superci-pull-identity-starved-`<size>` (2cpu to 32cpu) | a dispatch size's running and waiting jobs (`DispatchRunning` + `JobsWaiting`) are over 70% of its `IdentitiesOnline` for 10 minutes: its jobs wait at GitHub for a free identity, before the dispatcher sees them, which `oldest-job-wait` and the record's queue time cannot show | no |
| superci-pull-identity-none-`<size>` (2cpu to 32cpu) | a dispatch size has no identity online (`IdentitiesOnline` under 1) for 5 minutes: no job of the size can start, and the starved alarm sees no job, since none has reached the dispatcher | no |
| superci-pull-pool-at-floor | a set's pool at the hour's floor for 5 minutes while more of its jobs waited than it had idle runners | no |
| superci-pull-spend-daily | today's spend past 80% of the daily cap the listener runs with (`SpendDailyCapUSD`) | no |
| superci-pull-spend-capped | a spend cap is reached: CI on superci stops | yes, every canary |
| superci-pull-launch-shortfall | EC2 started fewer machines than asked for 15 minutes running | no |
| superci-pull-org-rate-unknown | the organization's rate unmeasured for 15 minutes | no |

A canary goes back to Blacksmith on harm to superci's own jobs: an alarm marked yes, or a FAIL of gate 1's wall, queue
wait or failure rows against Blacksmith over the same window (`tools/gate`, `gate job --canary` and `canary.sh`). The
others page. `superci-pull-registration-refusals` is the organization's cap, which every runner of the organization shares:
a job moved from Blacksmith takes one registration on either fleet.

**`superci-pull-pool-at-floor`.** The pool is not following the jobs. Read `curl -s localhost:8080/state | jq '.sets[]'`
on the listener: a set whose `pool` stays at its floor, or at 0, while jobs wait is reading no arrivals, or has no carried burst.
Look in the listener log for `load pools` and `save pools` errors (the IAM grant for `/superci-pull/pool/*`, or a
parameter the roll left unreadable), and for the set's JobAssigned and JobAvailable messages. To stop the wait at once,
pin the pool: `curl -s -X POST 'localhost:8080/override?set=<set>&pool=<n>'` (the next config change clears it).

**Release.** Off-peak (before 13:00Z or after 01:00Z), since a release restarts the listener: jobs wait about 10 s.
A listener that cannot reach its claims bucket does not start: run `deploy/up.sh` (it makes the bucket and grants
the role) before the first release that has claims. A release is one commit of andrewcai8/superci main, and the
same three commands release it and roll back to it:

```bash
pull/deploy/build.sh /tmp/release [commit]                # HEAD by default; git archive of the commit, built and tested on a tagged instance
pull/deploy/release.sh /tmp/release pull/deploy/ssh.sh    # the listener: binary, units, scripts, override config
pull/cache/deploy/release.sh /tmp/release                 # the cache: daemons, machine script, functions (runs cache/deploy/up.sh)
pull/deploy/verify-release.sh                             # read-only: what production runs is what the manifests name
```

`build.sh` writes a private release directory: the files at the S3 keys they go to, and the commit (`REVISION`).
Each release script writes its tree's `bin/RELEASE.json` (commit, build time, the sha256 of every file) with
`manifest.sh`, which refuses a commit not on andrewcai8/superci main (fetched exactly, from that remote only), a
script or config that is not the commit's, and a binary that does not carry the commit. It uploads from that
directory, `RELEASE.json` last, and ends with `verify-release.sh` for its part, failing if it fails. The listener
release checks the override with the new binary on the listener first, installs the binary only with the manifest's
sha256, and restarts the listener (a graceful stop closes its sessions, so the next opens at once). The listener logs
its commit at start (`"msg":"start","revision":…`; `superci-pull -version` prints it), and `pullcache version` the
daemon's.

What runs is only what a manifest names: a listener instance's boot refuses a `bin/superci-pull` whose sha256 is not
its `RELEASE.json`'s and starts no listener (the launch template takes this boot at `deploy/up.sh`), a machine runs
no `pullcache` whose sha256 is not its region's `RELEASE.json`'s (it runs on GitHub's cache), and `cache/deploy/up.sh`
puts on the functions only the zip whose sha256 the manifest names. A machine that boots in the seconds between a
cache release's `bin/up.sh` and its `RELEASE.json` runs on GitHub's cache. `deploy/up.sh` moves the listener's
template to this boot only once `bin/superci-pull` is the one `bin/RELEASE.json` names, so a replaced instance always
starts. Nothing refuses the other files where they are used: the listener's `deploy/*` and `config/config.json`, and a
machine's `bin/up.sh`, run as the bucket holds them, and `verify-release.sh` finds a change to one only after the fact;
`bin/issuer` is an operator's file, outside every manifest, which it only lists.

`verify-release.sh [listener|cache|all]` checks each bucket's manifest commit is on main, every file it lists is in
the bucket with its sha256, the bucket holds nothing else under the release's prefixes but an operator's files
(`bin/issuer`, `bin/hydrate/*.json`, `bin/dev-*/`), the running listener's sha256 and `-version`, and each cache
function's `CodeSha256` in both regions.

Before the release that moves the pool parameter to `/superci-pull/pool/<deployment>`: apply `deploy/listener-policy.json`
(`aws --profile ci-runners iam put-role-policy --role-name superci-pull --policy-name superci-pull --policy-document file://pull/deploy/listener-policy.json`)
so the new listener may read and write it, and run `deploy/alarms.sh` for the alarms. The first start after the
release finds no saved burst there and starts from 0 once. The policy still grants the old parameter
`/superci-pull/pool`, which the previous binary uses, so a roll back works; drop it with that binary.

**Recycle.** A release that changes what a machine runs at boot (boot.sh, the image, the disk) makes a new launch
template version. Each reconcile pass the listener replaces up to 5 idle machines per set that booted from an older
version (EC2's `aws:ec2launchtemplate:version` tag): an idle runner is removed first, as a retire is, a machine with no
runner yet ends, and a busy one finishes its job. A pool of 100 turns over in about 3 minutes; the timeline marks each
`machine.retired` `why` `recycle`. A canary is judged only once no idle machine of the old version is left.

**Roll back.** Release an older commit of main: `build.sh /tmp/rollback <commit>`, then the release scripts.

A build before round 2 does not know the deployment tag, the issued tag or the runner group `superci-pull`: rolling
back to one moves nothing back to the Default group, and it would grant runners to machines this build issued. Roll
back across round 2 only after draining (`override?pool=0&max=0`, then no machine left).

**Replace the listener, with no SSH.** Mark its instance unhealthy; the Auto Scaling group replaces it with one that
boots from the launch template (`up.sh` makes its versions) and the latest binary in S3. Measured Nov 25: the new
instance answered on the Elastic IP 32 s after the call.

```bash
aws --profile ci-runners --region us-east-1 autoscaling set-instance-health --health-status Unhealthy --instance-id \
  $(aws --profile ci-runners --region us-east-1 autoscaling describe-auto-scaling-groups --auto-scaling-group-names superci-pull-listener --query 'AutoScalingGroups[0].Instances[0].InstanceId' --output text)
```

To stop taking jobs, send the jobs back to Blacksmith with the fleet switch (megpt-mono's `SUPERCI_JOBS`, which
since #34100 routes a switched job to `superci-<N>cpu`; `tools/gate/canary.sh unset` deletes it and `canary.sh set '["test-go"]'`
re-routes, both on the App's variables token, [below](#the-tools-read-github-on-the-apps-budget)); to stop launching at once,
`curl -s -X POST 'localhost:8080/override?max=0'` (machines end as their jobs finish).

**Resilience.** systemd restarts a process that crashes (5 s), and the health check (`deploy/health.sh`, every 15 s)
acts on one that hangs. A check fails when `/healthz` does not answer 200 (the reconciler has not finished a pass for 2
minutes) or when the instance does not hold the Elastic IP; GitHub's or EC2's slowness does not fail it. `/healthz`
failing for 30 s on a listener that has run for 60 s restarts it, once (`TimeoutStopSec=15`: a graceful stop, else
killed). From the hang to serving again that is about 3 minutes after a graceful stop, 4 if the process had to be
killed and GitHub holds its dead sessions; a shim notices a silent connection after 60 s and then waits 3 minutes, so a
restart inside that window resumes its job. A restart that does not cure it (still failing 30 s after the new
process has run 60 s), a second hang within 10 minutes of a restart, or any failure that lasts 300 s (the Elastic IP
lost, a listener that never settles) marks the instance unhealthy, once, and the Auto Scaling group replaces it (the
call ignores the group's 300 s grace period, which would drop it). A listener started less than 60 s ago, or in
systemd's starting or stopping state (a release's restart among them), is not judged, and an instance under 10 minutes
old is not marked. An instance that cannot take the Elastic IP at boot marks itself unhealthy at once. The group also
replaces an instance EC2 reports down. A replacement takes the Elastic IP and the TLS key, so machines keep their
address and pin, adopts every machine of the deployment from EC2 and every runner from GitHub, and decides nothing
until every region is listed and each set's session has given the queue's statistics. After a crash, GitHub refuses a
new session for a set while the dead one's lapses (30 to 75 s measured).

**Deploy from scratch.** `deploy/up.sh <admin-cidr>` (role, parameters, bucket, groups, address, launch template),
`deploy/build.sh <dir>`, `deploy/release.sh <dir> pull/deploy/ssh.sh`, `deploy/up.sh` again (it starts the group once a release exists), `deploy/alarms.sh`.
The listener makes the runner group `superci-pull` at its first start. **Tear down.** `deploy/down.sh` deletes the
scale sets and every resource tagged `purpose=superci-pull`; the empty runner group stays at GitHub.

**Load test.** `bench/workflow.py` writes the workflow (a wave of mixed sizes per push, a matrix burst for a push
whose message says "burst"); `bench/measure.py BRANCH SINCE UNTIL listener.jsonl` measures it against the pass bar;
`bench/orphans.py` lists orphaned machines and runners.

## Security

- **Who may use the runners.** The runner group `superci-pull` admits megpt-mono only (`repositories` in the config),
  never a public repository. The listener makes and keeps the group with the App's
  `organization_self_hosted_runners: write`, and acts only on its own group's sets.
- **What a job can do.** A job is root on its machine, can read its identity document and its user data (boot.sh as gzip: the template holds it zipped
  because the script as text was 16,293 of EC2's 16,384 bytes with a real pin), and can
  stop its power-off. It cannot get a second runner, from this listener or the next (the machine's claim), and its
  machine ends when its job completes. The machine's role reads the cache bucket and invokes the cache broker; it
  cannot write EC2 tags. Measured below (Round 2, live proof).
- **What the listener can do.** Its role launches only from launch templates tagged `purpose=superci-pull`, only
  instances it tags so, only the RunsOn image (owner 135269210855) or our own, and only into our security groups.
  It changes runner templates only, never the listener's own (tagged `superci-pull-role=listener`) nor the push
  plane's. It writes only the issued and notice tags after launch, and ends only instances tagged
  `purpose=superci-pull`.
- **SSH.** Port 22 of the listener's group admits the admin address up.sh was given (72.226.74.73/32). The template
  names no key pair; `deploy/ssh.sh` pushes a key made for one session with EC2 Instance Connect. The key pair
  `superci-pull` an earlier `up.sh` imported is unused, and its private half is not kept here; delete it when no
  instance launched before round 2 is left.
- **Port 8443** is open to everyone: machines of both regions reach the listener there, and it serves only a machine
  whose identity document AWS signed for one of its instances.
- **Follow-up: a GitHub App of its own.** The listener uses the push plane's App (its credentials copied to
  `/superci-pull/github-app` by `up.sh`), whose key is shared with the push plane, and holds `actions: write` only
  to re-run lost jobs. An operator should create an App for superci-pull alone, with `organization_self_hosted_runners:
  write` and `actions: write` on megpt-mono, install it on the organization, and put its id and key in
  `/superci-pull/github-app` (and its installation id in `installation`).

### The tools read GitHub on the App's budget

The gate, the timeline, the logs tool and the canary (`tools/gate/gate.py`, `tools/timeline/timeline.py`, `tools/logs/logs.py`, `tools/gate/canary.sh`) read GitHub
on the App's installation budget, not on the operator's personal token, which every session on the operator's host shares
(it reached 0 on Nov 27 at 18:13Z, writes refused too, and a re-route and the canary's checks waited on it).

- **How.** The listener's admin endpoint (`POST /github/token?scope=S` on port 8080, `apptoken.go`) answers an installation
  token of one scope, held until under ten minutes remain (each scope separately), never logged, and not served on the public
  port. A token has exactly one permission, so what leaves can do that and nothing else:

  | Scope | Permission it is minted with | Repositories | Used for |
  | --- | --- | --- | --- |
  | `actions` (the default) | `actions: read` | the config's | runs, jobs, logs |
  | `variables` | `actions_variables: write` (the App's "Variables" permission) | megpt-mono | `SUPERCI_JOBS`: the canary's checks, its rollback, `canary.sh set/unset` |
  | `contents` | `contents: read` | megpt-mono | `scripts/verify/ci-limits.tsv` from main (`gate --limits FILE` makes no call) |

  The tools reach it through `deploy/ssh.sh`, so the App's key never leaves the listener and the tools never read the SSM
  parameter. `tools/timeline/apptoken.py` keeps each token in the process's memory and gives it to each `gh` call as
  `GH_TOKEN`; `canary.sh` takes it into a shell variable and sets it for each call. Nothing writes or prints it.
- **A scope the installation lacks.** The listener reads the installation's permissions with the App's JWT
  (`GET /app/installations/<id>`, held for a minute) before it mints, and answers 422 naming the permission
  (`"actions_variables: write"`, and what the installation holds) when it is missing or held at a lower level; GitHub's own 422 on the
  narrowed mint is answered the same way. It never answers a broader token in its place, and an unknown scope is a 400.
- **Refusal.** With no App token (ssh or the endpoint down, the permission not granted) `gate.py`, `timeline.py`, `logs.py` and
  `canary.sh` exit 2 and say why; they never fall back. A refusal in the middle of a command (a token refresh that fails during
  a long gate) is `apptoken.AppTokenRefused`, not a `SystemExit`, so no per-run "skip this run" catch can swallow it and judge a
  sample that lost runs; each tool's `main` ends with exit 2. A refused contents token ends `gate job` the same way, and the
  message says to pass `--limits FILE`. A message about a token never holds one (ssh failing after curl
  printed a token says only the exit code and status). `--personal-token` (`gate.py`, `timeline.py`, `logs.py`, before the
  subcommand for the first two) and `SUPERCI_PERSONAL_TOKEN=1 canary.sh` spend the shared personal budget on purpose and print
  that they do. `gate scale` reads no GitHub for a window the listener's job records cover, and refuses at its first call
  otherwise. `SUPERCI_APP_TOKEN_CMD` (a stub for the ssh call, which finds the scope in `SUPERCI_TOKEN_SCOPE`) is honoured only
  with `SUPERCI_TEST=1`, which the tests set.
- **The floor.** `canary.sh` reads `rate_limit` with the token its checks use, so its 1,500 floor is that token's own budget.
  A tick that cannot get a token is skipped and logged; the watch does not stop.
- **The rollback never waits on the endpoint.** `canary.sh` keeps the variables token at most 300 s (the listener hands out a held
  token with over 10 minutes left, so one may expire 10 minutes after the ask) and drops it when GitHub refuses a call made with it,
  asks once more and tries again. Before its loop it reads `SUPERCI_JOBS` once and refuses to start when it cannot (an old listener
  answers an actions token for `scope=variables`), so no watch runs that could not roll back. For a rollback it asks 3 times (each ask
  bounded to 30 s, none repeated when the installation lacks the permission). When the token is refused, or GitHub refuses the write
  with a fresh one, the rollback prints `ROLLBACK FAILED` with the cause (the listener would not give a token, the token expired, GitHub
  refused it) on stdout and stderr and leaves SUPERCI_JOBS as it was (the next tick tries again), unless `SUPERCI_PERSONAL_TOKEN=1`:
  then it asks the App once (10 s) and the write goes out on the personal token and prints
  `personal-token exception SUPERCI_JOBS write`. A gate that exits 2 is logged `NO CHECK (...)`. `canary.sh set '["test-go"]'` and
  `canary.sh unset` re-route by hand with the same token and fallback, and need no aws profile and no actions token.

What each call needs (GitHub's permission names; the first column is what the tools call):

| Call | Permission it needs | Held by the App? |
| --- | --- | --- |
| workflow runs, run jobs, job logs, a run's head (`gh api repos/R/actions/...`) | Actions: read | yes (`actions: write` includes it) |
| `rate_limit` | none | not applicable |
| `ci-limits.tsv` from main (`gh api repos/R/contents/...`) | Contents: read (`contents`) | yes, since Nov 27 |
| `SUPERCI_JOBS` read, set, delete (`repos/R/actions/variables/...`) | Variables: read and write (`actions_variables`) | yes, since Nov 27 |

Personal-token exceptions (the only call that uses the personal token without `--personal-token`; it prints
`personal-token exception <name>` when it runs, and the tests hold this list to the code):

| Exception | Where | When |
| --- | --- | --- |
| `SUPERCI_JOBS write` | `canary.sh` (`personal_fallback`: a rollback's, a `set`'s or an `unset`'s write of the variable) | only with `SUPERCI_PERSONAL_TOKEN=1`, and only when the variables token is refused or GitHub refuses the write with it |

To end the exception, delete its row, its key in `EXCEPTIONS` (`tools/timeline/apptoken.py`) and its call site
(`personal_fallback`), and the rollback then fails loudly whenever the variables token is not to be had.

## Cost

Spot prices on Nov 25 2026 08:00Z (median of each size's 10 cheapest pools), with its gp3 volume, a machine-hour:
2 vCPU $0.035, 4 vCPU $0.062, 8 vCPU $0.106, 16 vCPU $0.190, 32 vCPU $0.351.

These are the median of the cheapest pools, not what a size launches into. 8 vCPU carries unit-tests' 8 vCPU legs
(microvm-design-v5, Nov 27): m8azn.3xlarge spot first (`spot_first`), then m8a.2xlarge spot, then m8a.2xlarge on demand
within `on_demand_share`. Measured on the same commit and legs as Blacksmith, their tests ran at 0.75x Blacksmith's time
on m8azn.3xlarge (n=3) and 1.03x on m8a.2xlarge (n=5). At spot $0.51 and $0.175 an hour (us-east-1, Nov 27) with the read
set's 65 s to a warm machine, a leg costs $0.0234 (1.01x Blacksmith's $0.0232) on m8azn and $0.0092 (0.40x) on m8a; with
half on each, about $0.016 (0.71x) and a wall p50 at or under Blacksmith's 60 s (m8azn about 50 s, m8a about 61 s). A
leg on m8azn.3xlarge on demand would cost $0.0557 (2.40x), so it is never asked for. The pool cap of 250 holds the
warm-first pool v5 sized at 242 for the Nov 26 peak (146 legs a minute).

| | Per hour |
| --- | --- |
| The listener (c7g.large on-demand, 30 GB gp3, the Elastic IP's public address) | $0.08 |
| Idle with no jobs routed here (no floor ships: a set's pool is 0 a window plus the hold after its last routed job) | $0.00 |
| Idle pools at full rollout, modelled over a weekday (all of megpt-mono's Linux jobs here, hydrated machines) | $17.7 on average |

A busy hour costs what its jobs run for at those prices, plus about 30 s of boot and shutdown per job (a machine runs
one job), plus the pool. `SpendPerHourUSD` is the listener's live figure.

**The root volume.** gp3, 250 MiB/s and 3,000 IOPS unless a size sets `disk_mbps` and `disk_iops` (the config takes 125 to 1,000 MiB/s and 3,000 to 16,000
IOPS, the range priced and measured here, though gp3 now takes more, at most 0.25 MiB/s for each IOPS and 500 IOPS for each GiB; it refuses a size outside
that, since a launch template EC2 refuses fails the listener's start). `superci-4cpu` sets 1,000 and 4,000. The gate found
mind-snapshot shard 3 about 10 s slower than on Blacksmith, with 41 to 43 CPU-seconds in iowait against under 1, and a 1 GiB `fdatasync` write of
3.2 to 4.3 s against 0.5 to 0.6 s: the device, not the page cache. `pull/tools/disk/lab.sh` times it on lab instances (one per setting: an `m8azn.xlarge`
on the superci image with a 60 GB root, boot.sh's `vm.dirty_ratio=50` and `noauto_da_alloc`, the root grown), Nov 27:

| MiB/s : IOPS | 1 GiB `fdatasync` write, 3 runs | 4 GiB | 4 KiB random writes, 20 s |
| --- | --- | --- | --- |
| 250 : 3,000 (before) | 3.21, 4.06, 4.06 s | 16.35 s | 3,149 IOPS |
| 500 : 3,000 | 1.16, 2.01, 2.02 s | 8.16 s | 3,149 IOPS |
| 750 : 3,000 | 0.57, 1.26, 1.34 s | 5.43 s | 3,069 IOPS |
| **1,000 : 4,000** | **0.46, 0.68, 1.00 s** | **4.07 s** | 4,095 IOPS |
| 1,000 : 16,000 | 0.46, 0.67, 0.99 s | 4.06 s | 16,800 IOPS |

Each step is device-bound (4 GiB takes 4.07 s at 1,000 MiB/s, 16.35 s at 250). 1,000 MiB/s is the smallest setting that brings the 1 GiB write to
Blacksmith's 0.5 to 0.6 s on its first run and within about 1 s on the later ones, and 4,000 IOPS is the least gp3 allows with it. 16,000 IOPS
does nothing for these writes (the random-write row is the IOPS limit) and costs 0.082 USD an hour more; it is not set. Throughput over 125 MiB/s
is 0.04 USD a MiB/s-month and IOPS over 3,000 are 0.005 USD an IOPS-month, so 1,000 and 4,000 cost 0.0548 USD an hour beyond storage against 0.0068
for the defaults: **+0.048 USD per machine-hour** (+0.0016 for a machine that lives 2 minutes), +32% on an `m8azn.xlarge` spot machine (about 0.15) and
+12% on one on demand (0.41). The listener's spend estimate uses the size's volume. The 5.4 GB that shard 3 writes is 21 s of device time at 250
MiB/s and 5.4 s at 1,000: that the shard gets its 10 s back is a prediction until the gate measures it. `m8azn.xlarge` bursts to 1,875 MiB/s of EBS
for 30 minutes a day (312 MiB/s baseline), and a single-use machine always has its burst; `m8a.xlarge` and `r8a.xlarge` burst to 1,250 (156
baseline) and were not measured. A changed template makes new machines: the listener retires the idle 4 vCPU machines of the old template version (5 a pass), busy ones finish their job, and the other sizes keep their version.

## Known limits

- **GitHub's registration cap is the fleet's real limit.** Every job is a JIT registration, and the rest of the
  organization used 2 to 4.9 of GitHub's 5 a second through the morning. Moving jobs from Blacksmith moves their
  registrations, so ramp the fleet switch gradually and watch `OthersRegistrationRate`; adding load (a test) needs
  a pool registered ahead.
- **A burst waits on registrations past the pool.** A burst of N jobs on a size whose pool holds P idle runners
  starts P at once; the rest wait for machines (about 15 s from launch to a registered runner) and for
  registrations at the admitted rate S (`AdmitRate`, the ceiling of 2 a second until GitHub refuses one). The last job of the
  burst waits about `15 s + (N - P) / S`: at S = 2/s, a 200-job burst onto a pool of 10 waits up to about 110 s.
- **gp3 storage.** 40-150 GB disks use 50 TiB per region at about 800 machines; the quota is now 200 TiB in us-east-1
  and 100 TiB in us-east-2 (`LaunchShortfall` and its alarm show the next limit hit).
- **Spot vCPU quotas** are 15,000 (us-east-1) and 5,000 (us-east-2); the region weights (2:1) follow them.
- **Re-runs after a restart** reach only jobs a spot notice found running (the notice tag), within about an hour of
  the reclaim; a job whose machine was reclaimed with no notice, while the listener was down, stays failed.
- **A machine issued a runner whose answer was lost** (the connection dropped after the tag was written) ends
  unused: one machine and one registration, never a second runner.
- **EC2's tag reads are eventually consistent.** No grant trusts them: the claim decides. A machine issued by a
  previous process whose tag a listing does not show yet is granted, its grant finds the claim taken and answers 410,
  and its end waits for GitHub's word on its runner.
- **A dispatcher that crashes in a hand-off fails that job once, and never runs it twice.** The shim drops the job's
  bytes once PAIRED comes, so from then until the machine has them the job message is only in the dispatcher's memory.
  A crash between PAIRED and the job's claim (one S3 round trip) leaves no job claim: the next dispatcher strands the
  job when either side rejoins, both are answered 410, the shim fails the job, and it is re-run once its run has
  completed. A job is stranded only by a resume, or by the process that paired it: one whose sides both gave up before
  any dispatcher answered (a listener down past 3 minutes), or whose pairing process ended with S3 still unanswered,
  stays failed. A crash after the job's claim and before its bytes reach
  the machine leaves a machine with its JOB and no job message: its agent sends EXIT 1 when no complete first message
  comes within 60 s of JOB, the next dispatcher relays it through the job it adopts, and the job fails. In both cases
  the job's claim names at most one pair, so no other machine gets the job.
- **The statistics are a lease.** When the message session has not answered for 100 s (twice its long poll),
  nothing is launched, no idle runner is retired as surplus and no machine is told to end as surplus.

## Measured (Nov 25 2026, megpt-mono)

| Test | Run | Result |
| --- | --- | --- |
| Smoke, one job per size | 45568198731 | 5/5 succeeded; jobs on a pool runner (2, 4, 8 vCPU) waited 1 s, the 16 and 32 vCPU jobs (no pool) 16 and 17 s, from launch to a registered runner in 11-15 s; each ran on its size (c8a.large, c5a.xlarge, c5a.2xlarge, c7i-flex.4xlarge, c6a.8xlarge; `nproc` 2, 4, 8, 16, 32) |
| Split, 40 jobs on `superci-4cpu` run as two sets | 45568606692 | GitHub gave 20 to `superci-4cpu` and 20 to `superci-4cpu-2`; 40/40 succeeded |
| Crash mid-run, 30 jobs over every size | 45569779369 | `kill -9` of the process at 07:39:39, then the listener's instance terminated at 07:40:40: 30/30 succeeded; at +10 min, 0 orphaned machines, 0 orphaned runners, and `Orphans` 0 in every period |
| Listener down (stopped 08:24:59 to 08:28:47) | | `superci-pull-listener-down` went to ALARM at 08:28:40 and back to OK at 08:30:40 (on `FILL(Heartbeat, 0)`; a plain heartbeat alarm stayed OK through an earlier 5-minute outage) |
| Spot reclaim mid-job (AWS FIS `send-spot-instance-interruptions`, role `superci-pull-fis`) | 45573133090 | the notice reached the listener 25 s after it was sent; AWS took the machine 2 min later and GitHub failed the job (attempt 1); the listener re-ran it 5 s after the run completed, and attempt 2 succeeded on another machine |

### Round 2, live proof: a job that keeps its machine and keeps asking (Nov 25 2026, 12:18-12:28Z)

Four runs of one job on `superci-2cpu` (branch `ci/superci-load-pull-11`, deleted after). The job replaced its
machine's `poweroff` with `/bin/true` (so neither the boot script nor its life timer could end the machine), then
started a systemd unit outside the runner's process tree that POSTed the machine's identity document to `/jit` every
3 s, logging each answer, and kept doing so after the job ended.

| Run | What happened to the listener | Asks answered | Registrations of the machine | How the machine ended |
| --- | --- | --- | --- | --- |
| 45612544464 | none (this first version killed the boot script, which ended the runner with it: exit 143) | none made | 1 | the listener terminated it 6 s after its job failed |
| 45612774653 | none | 27, every one 410 with 0 bytes | 1 | the listener terminated it 5 s after its job completed (`Client.UserInitiatedShutdown`); an ask before JobStarted arrived was read as a second ask with an idle runner, GitHub refused the runner's removal (job still running) and the runner was kept busy |
| 45613138188 | stopped 12:23:02-12:24:21; the job completed at 12:23:51 with no listener up | 12 (3 after the restart), all 410 | 1 | the new process read GitHub's redelivered completion and terminated it 20 s after its start |
| 45613663785 | killed (`kill -9`) 140 ms after it read the job's completion, before it could end the machine | 43 (17 after the restart, through 40 s of session conflicts while the dead sessions lapsed), all 410 | 1 | the new process found no runner of the machine's name at GitHub (`stuck ... runner gone`) and terminated it 58 s after its start |

### Load test: 5 jobs a second for 10 minutes, mixed sizes, and a 200-job matrix burst

`bench/replay.sh` pushed one run every 10 s to `ci/superci-load-pull-<n>` (60 waves of 50 jobs: 12 at 2 vCPU, 27
at 4, 9 at 8, 1 at 16, 1 at 32; each job sleeps 30-90 s), and after wave 30 one run of a 200-leg matrix on
`superci-4cpu` (split in two sets). Every job adds a registration the organization would not otherwise make, so the
pool was registered ahead, into the spare rate only, as the pool rule does before a busy hour (floors 670, 1,690,
500, 30 and 30, and `pool_share` 0.9). For run 1 that took 07:53-10:00Z, mostly at 0.2-0.5 a second while the rest
of the organization ran at 3.5-4.9 a second (4.94 at 08:33, past GitHub's cap on its own), and stopped for 10
minutes at 50 TiB of gp3 per region (raised to 200 and 100 TiB); for run 2 it took 10:25-10:36Z, with the rest of
the organization at about 2 a second.

| | Run 2 (45596611275..45598160040, 10:36-10:48Z) | Run 1 (45590743411..45592336094, 10:00-10:15Z) |
| --- | --- | --- |
| Jobs | 3,200 / 3,200 succeeded | 3,198 / 3,200 (2 failed) |
| Queue wait p50 / p90 / p99 / max | 2 / 3 / 6 / 22 s | 2 / 3 / 6 / 308 s |
| The 200-job burst, p50 / p90 / max | 4 / 6 / 7 s | 4 / 6 / 308 s |
| Registrations during the test | 1,125 made, 0 refused | 1,113 made, 0 refused |
| On-demand | 0 of 2,352 machines (all spot; 8 zones, 54 types) | 0 of 2,142 |
| Spot notices (real reclaims) | 124; 12 jobs on noticed machines, all finished before AWS took them | 142 |
| Organization's registration rate before / during | 3.60 / 3.76 a second (others 2.10 / 2.12, ours 1.47 / 1.63; peak 4.61) | 3.49 / 3.94 (others 2.86 / 2.53, ours 0.62 / 1.42; peak 4.53) |

Run 1 found a bug: a listing paged while runners come and go misses some, an idle pool runner never asks, and the
listener ended 969 machines whose runner one listing missed, two as their runner took a job (both jobs failed) and
others just after GitHub handed them one (15 jobs waited 303 s for GitHub to hand them on). Run 2 is after the fix
(a missed runner is checked with GitHub before its machine ends).

Returning to the built-in config after run 2 (pools to their floors, the split back to one set) left 1,300 idle
runners to remove. The reconciler removed them one at a time, a pass took over a minute, the health check failed
five times, and the Auto Scaling group replaced the instance (11:05:47, back in about 90 s). Removals and checks now
run 16 at a time.

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
