# logs

Everything superci knows about one job, from its own events, in one command:

```
pull/tools/logs/superci-logs https://github.com/OWNER/REPO/actions/runs/RUN/job/JOB
python3 -I pull/tools/logs/logs.py URL [--profile ci-runners] [--no-metrics]
python3 -I pull/tools/logs/test_logs.py
```

It prints, in this order:

1. The job's record: conclusion, queue wait, wall time, set, the machine's type, zone and lifecycle, warm or cold, and the
   machine's estimated cost with the part spent while the job ran (the `job.record` event).
2. Every listener and machine event of the job and of the machine that ran it, in time order, seconds from the job's creation:
   the launch, the boot marks (corrected to the listener's clock), the registration, the job, the machine's end. A warm-pool
   machine's events from before the job existed are the machine's own and show.
3. The listener's other log lines that name the machine (a job lost with its machine, a spot notice, an error).
4. The job's host metrics summary (peaks, IO wait, cold-read MB) from `s3://superci-pull-timeline-243891825325/jobs/<job id>/summary.json`,
   or a line saying none was uploaded.

It makes one GitHub call, the job's REST record, to turn the URL's numeric job id into the run and the machine (cached by `--cache`),
and reads the rest from CloudWatch Logs and S3 of the ci-runners account with `--profile`, read only. Run it with `python3 -I`.

`superci logs` is the name the task asked for, but the `superci` CLI belongs to upstream and takes no external subcommand, so this is
the `superci-logs` command: put this directory on `PATH`, or alias `superci logs` to it.
