# Pool sizing: the measurements and the model

The evidence behind the pool rule (`State.Burst`, the README's Pools section). Every script takes the data directory
`DATA` first and runs with Python 3 alone; the fetchers need `gh`, the canary join needs the listener's log and EC2.

| Step | Command | Writes or prints |
| --- | --- | --- |
| Canary jobs | `fetch_runs.py START MINUTES \| fetch_jobs.py > DATA/logs/ltb-jobs.jsonl` | every job of the runs (GitHub) |
| Canary join | `canary.py DATA` | per job: warm or cold, pool at arrival, Fleet, boot, registration, runner start; `DATA/logs/canary-rows.json` |
| Peak demand | `fetch_runs.py 2026-11-24T02:00:00 15 \| fetch_jobs.py > DATA/demand/win15.jsonl`, then `demand.py DATA/demand/win15.jsonl` | rate, per-minute and per-20 s quantiles, burst per run, by size |
| Model check | `validate.py DATA` | the model's waits on the canary's arrivals against the measured ones |
| Weekday | `day.py DATA HOURLY all CONFIG` | today's rule, the rate rule, searched static pools; `DATA/demand/day-<size>.json` |
| Burst rule | `adaptive.py DATA` | the burst rule over a grid of quantile, launch time, window and hold |
| Shipped rule | `rule.py DATA CONFIG SHIFT` | the config's rule with launch times SHIFT s longer (19 for hydrated machines) |
| Cold path | `leadvalue.py DATA` | the cheapest pool's idle $ as the launch time shrinks |
| Prediction | `predictive.py DATA HOURLY` | the bound on launching ahead for a push's later jobs |
| Curve | `curve.py DATA HOURLY SHIFT` | idle $ against job and per-push wait, per quantile |
| Live | `bench/burst.py`, then `bench/burstwait.py BRANCH WARMUP LOG` | a replay's waits, per job and per run |

`DATA/logs/` holds the listener's log events for the canary (`canary.json`, from CloudWatch `/superci-pull/listener`),
EC2's listing of the machines (`ec2-<region>.json`, read within the hour) and the jobs; `HOURLY` is Blacksmith's
hourly usage export, one JSON a UTC hour. `pull/bench/burst.py` replays real pushes on a set to prove a rule live.
