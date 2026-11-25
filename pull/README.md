# superci-pull

One GitHub runner scale set (`superci-pull-4cpu`) served by EC2 spot in the `ci-runners` account. It replaces the
webhook-push demand path of superci with GitHub's pull protocol. GitHub holds the queue, one listener reads it, and
the machines are the only state.

Status: torn down on Nov 25 2026 after the burst bakeoff (scale set deleted, nothing tagged `purpose=superci-pull`
left); `deploy/up.sh` and `deploy/install.sh` bring it back.

A workflow asks for it by name:

```yaml
runs-on: superci-pull-4cpu
```

## How it works

```
GitHub Actions service: the set's queue, statistics, JIT registration
     ^ long poll (GetMessage), AcquireJobs, DeleteMessage, GenerateJitRunnerConfig, RemoveRunner
     |
superci-pull, one process on one t4g.medium in us-east-1 (systemd, restarts itself)
  listener  : one message session; acks a message only after it is handled
  launcher  : desired = min(max, assigned + available + pool); launches desired - live
              as instant EC2 Fleets of at most 250, price-capacity-optimized over
              18 4-vCPU types x every zone of us-east-1 (2/3) and us-east-2 (1/3);
              a region's spot shortfall goes to the other region, then on-demand,
              capped at 10% of the last half hour's launches; a ClientToken on every call
  /jit      : a booting or finished machine asks for a runner over TLS (pinned key);
              grant while live <= desired, else 410 and the machine powers off
  reconciler: every 10 s lists EC2 by tag and GitHub's runners; adopts, removes stale
              runners, ends stuck machines, trims surplus idle runners (RemoveRunner first)
     |
EC2 machines tagged purpose=superci-pull, superci-pull-set=superci-pull-4cpu
  RunsOn ubuntu24 x64 image (superci's pinned build rule); boot.sh asks /jit, runs the
  runner for one job, asks again; powers off when told it is not needed
```

### What decides each fact

| Fact | Source | After a restart |
| --- | --- | --- |
| Jobs waiting, assigned, running | GitHub's statistics on each message | the session's first statistics |
| Machines | EC2 instances with the set's tags | the first listing (machines are adopted) |
| A machine's runner, busy or not | JobStarted and JobCompleted, and GitHub's runner listing | the runner listing |
| Who a machine is | it asks from its own public address; EC2 must list that address for the instance it names | the same |
| A launch not to repeat | the call's ClientToken | a fresh token per process; the deficit is recomputed from EC2 |

### Invariants

- **No job is lost inside our account.** GitHub redelivers a message until the listener acknowledges it, and the
  listener acknowledges only after it has acquired the message's jobs and taken in its statistics.
- **A machine ends only when its runner can take no job.** It powers itself off only when it holds no runner (it asked
  for one and was told 410). The listener ends a machine with an idle runner only after GitHub removed that runner;
  GitHub refuses to remove a runner that holds a job. The listener ends a machine without a fence only when no runner
  of it is registered, after a complete listing made well after its last runner was issued.
- **Registrations stay under GitHub's cap.** At most `-reg-budget` (1,400) registrations in any 5 minutes; a machine
  over the budget is told to ask again (429).
- **The supply converges.** Every decision is level-triggered from the two listings, so a lost answer, a crash or a
  dead machine is repaired by the next reconcile.

## Operate

On the listener (`ssh ec2-user@<listener>`):

```bash
sudo journalctl -u superci-pull -o cat -f          # JSON events: message, fleet, register, ask, state, terminate
curl -s localhost:8080/state | jq                   # desired, live, idle, busy, registrations in the window
curl -s -X POST 'localhost:8080/pool?n=20'          # hot pool: idle runners kept beyond the queue
curl -s -X POST 'localhost:8080/max?n=0'            # stop launching (machines drain as their jobs end)
~/src/pull/deploy/install.sh -pool=20               # rebuild and restart with flags
```

Deploy from scratch: `deploy/up.sh <admin-cidr> <ssh-public-key>` (account resources and the listener instance), then
copy this directory to the listener and run `deploy/install.sh`. Tear down: `deploy/down.sh "<ssh command>"` deletes
the scale set and every resource tagged `purpose=superci-pull`.

Load test: `bench/workflow.py BRANCH superci-pull-4cpu 4 250` writes the 1,000-job workflow;
`bench/measure.py RUN_ID listener.jsonl` measures a run against the pass bar.

## Measured (Nov 25 2026, megpt-mono, 4 x 250 jobs sleeping 60-120 s)

| Run | Jobs | Queue wait p50 / p90 / max | On-demand | Registrations | Orphans at +10 min |
| --- | --- | --- | --- | --- | --- |
| Cold, pool 0 (45554738472) | 1,000 / 1,000 succeeded | 75 / 316 / 356 s | 0 of 1,063 | 1,023 made, 268 refused by GitHub | 3 machines and runners (stale statistics; fixed since) |
| Warm, pool of 1,000 (45559227065) | 1,000 / 1,000 succeeded | 31 / 48 / 316 s | 0 of 431 | 1,021 made over 16 min, 0 refused, at most 399 in 5 min | none |
| Two runs at once, pool of 300 (45562163344, 45562164825) | 300 / 300 | 7 and 16 / 13 and 20 s | 0 | 300 | |

Two limits of GitHub's, not of this design, set those numbers:

- **Registrations: 1,500 runners configured per 5 minutes across the organization, about 5 a second, shared with
  every other registrant.** GitHub's refusal is `ConfigureTooManyRunnersException: You have configured more than 1500
  runners within the last 5 minutes`. Runner ids are one sequence for the organization, and their advance shows the
  rest of the organization registering 3.9 a second before the cold run; while the cap held, no other runner in the
  organization registered at all. A just-in-time runner takes one job, so a cold burst starts at most as fast as
  the organization's share of the cap allows: here 290 at once, then 5 a second. Registrations made ahead of
  demand (the warm pool, paced at 1 a second) avoid it.
- **Hand-out: about 15 jobs a second per scale set.** Against 1,000 idle runners, jobs started at exactly 80 per
  5 s; two runs at once shared the same 15 a second. A 1,000-job burst on one label therefore needs about 66 s to
  start however many runners wait, so its median wait cannot be under 10 s.

Launches were never the limit: 1,000 spot machines were up within 53 s of the jobs being queued, across 7 zones of
both regions, with no on-demand fallback. us-east-1's instant fleets answer slowly (28-33 s for 66-449 machines;
us-east-2 took 7 s for 225), which the launcher absorbs by running rounds in parallel.
