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
              of at most 250, capacity-optimized-prioritized over the size's 12 types (listed fastest first: c8a and
              m8a, then c8i, m8i, c7a and m7a, then four older families for capacity) x every zone of us-east-1 that
              offers them (5 of 6: use1-az3 offers none) at weight 2 and of us-east-2 (3 of 3) at weight 1; a
              region's spot shortfall goes to the other, then on-demand, at most 10% of the last half hour's
              launches; a ClientToken on every call
  /jit      : POST with the machine's signed identity document; a runner only for a machine never issued one, only
              once its grant has made the machine's claim (s3://superci-pull-claims-<account>/issued/<instance>),
              and its config leaves only after EC2 has the machine's superci-pull-issued tag
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
| The organization's registration rate | the advance of runner ids (one sequence for the organization), less ours | blind (0.3/s) for about 90 s |
| Spend today and this month | accrued from running machines at their pool's spot price | the SSM parameter `/superci-pull/spend` |
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
- **We never take the organization's last registrations.** Registrations go only into what GitHub's cap leaves the
  rest of the organization, less a margin; when the organization is near the cap, our jobs wait, not Blacksmith's.
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
- Until the rate is measured (about 75 s after a start: two listings 61 s apart, each dated when it began) the
  listener registers at `blind_rate` (0.3/s) at most.
- Across every set, machines waiting for a runner are bounded by what the budget registers in 90 s (a waiting machine
  is paid for). Past twice that, once the rate is measured, machines that have waited three minutes end.

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
- The budget does not depend on the estimate alone. The 20% margin, the window guard at `cap x (1 - margin / 2)` and
  the backoff after a refusal bound the damage of an error in either direction.

Moving a job from Blacksmith to superci moves its registration rather than adding one, so a gradual ramp of the
fleet switch keeps the organization's total where it is. A test that adds jobs (a load test) adds registrations,
and at the organization's measured 3.5 to 4.5 a second it has almost nothing to add them into.

### Machine identity

A machine POSTs `/latest/dynamic/instance-identity/rsa2048` from its metadata service. The listener verifies the PKCS7
signature with AWS's RSA-2048 certificate of the region the document names (`certs/`, from AWS's documentation),
requires account 243891825325, and serves the instance only if it is a machine of one of the sets. Its address plays
no part. The runner it registers is named `<set>-<instance id>`. A machine gets that one runner, ever (Invariants).
Anything else on the machine that reads the document (a job) gets 410 and nothing, while its runner holds the job
and after, across restarts of the listener.

### Pools

Each set keeps a hot pool of idle runners: enough to cover what arrives during `fill_seconds` (45: launch, boot and
registration) times `factor` (1.5), measured over the set's last 10 minutes of arrivals, between a floor and a cap
for the hour (UTC). The caps come from megpt-mono's Blacksmith arrivals by hour (`tools/pools.py`); the floors are 1
for 2, 4 and 8 vCPU and 0 for 16 and 32. Within 15 minutes of the next hour, the larger of the two hours' bounds
applies, so a busy hour's pool is registered slowly ahead of it. A split size divides its bounds among its sets.
A job arrives with its first JobAssigned or JobAvailable message: GitHub assigns these sets' jobs directly, with
no JobAvailable, so before Nov 25 19:00Z every arrival (and every `QueueWait`) read as none and each pool stayed at
its floor; the load tests above pinned their pools, which hid it. After a start, arrivals are rated over the time
since it (at least a minute), and no idle runner is trimmed in the first window (10 minutes), so a restart does not
read as no arrivals and shed the pool.

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
  - the job's machine left before the job completed, and EC2 says a spot interruption terminated it.

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
2. **Put this branch's machine script in each bucket** (`bin/up.sh`; it reads the issuer from `bin/issuer` when one
   exists, and is otherwise the script already there). A machine that boots after this starts the daemon, if it has
   the role. One without the role runs on GitHub's own cache.
   ```bash
   for r in us-east-1 us-east-2; do
     aws --profile ci-runners s3 cp pull/cache/deploy/machine-up.sh s3://superci-pull-cache-243891825325-$r/bin/up.sh --region $r; done
   ```
3. **Let the listener pass the role.** This only adds a statement; the running listener is unaffected.
   ```bash
   aws --profile ci-runners iam put-role-policy --role-name superci-pull --policy-name superci-pull --policy-document file://pull/deploy/listener-policy.json
   ```
4. **Build, test and release the listener** (the release restarts it; a graceful stop closes its sessions, the next
   opens at once). `deploy/test.sh` runs the tests of both modules on a tagged instance; `deploy/build.sh` builds
   the binary (it tests `pull/` only).
   ```bash
   pull/deploy/test.sh
   pull/deploy/build.sh /tmp/superci-pull
   pull/deploy/release.sh /tmp/superci-pull "" pull/deploy/ssh.sh
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
2. **Take the profile off the templates.** Roll the listener back to the previous binary with the Roll back recipe
   above (copy the previous `bin/superci-pull` version over the current one, restart). The previous binary embeds
   the old `boot.sh`, so its user data differs from the latest template version and it makes a new version, without
   the profile and without the daemon line.
3. **Only then drop the `PassCacheRole` statement**, if wanted: `git show pull-prod:pull/deploy/listener-policy.json`
   to a file, then the `put-role-policy` of step 3 with it. Dropping it first makes every launch of a template that
   still names the profile fail.
4. **Remove the cache itself** (buckets stay): `pull/cache/deploy/down.sh`. Nothing in the listener needs it gone.

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
- **Stop.** The runner's `ACTIONS_RUNNER_HOOK_JOB_STARTED` is `hydrate-stop.sh`: when the job starts it ends the
  whole hydration group (SIGTERM, then SIGKILL after 2 s), so hydration never competes with the job for CPU or disk.
  The job takes the entries already in place and restores the rest itself. A hydrator that already ended leaves the hook
  nothing to do.
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

## Operate

```bash
pull/deploy/ssh.sh                               # a shell on the listener: a key made for this session (EC2 Instance
                                                 # Connect), from the admin address up.sh allowed; no key pair is kept
tail -f /var/log/superci-pull/listener.jsonl    # JSON events: message, fleet, register, ask, budget, state, retire, terminate
                                                # (also in CloudWatch Logs, group /superci-pull/listener, a stream per instance)
curl -s localhost:8080/state | jq               # budget, spend, and per set: ready, desired, live, idle, busy, issued, pool, orphans
curl -s localhost:8080/healthz                  # what the health timer checks (the process's liveness)
```

**Change pool sizes, caps or limits.** Write an override file, check it with the listener's own binary, and upload
it. Every field it names replaces the built-in one (`config.json`), and a `pools` or `sizes` entry replaces that
size's whole entry (name every field of it). A field the listener does not know, or an hourly array of other than 24
values, is refused. The instance fetches the file every minute and installs it only if it is valid (`sync.sh`); the
listener applies pools, caps, the registration budget and machine limits within 30 s; sizes, splits, types, disks,
regions, the deployment and its repositories apply at the next start (`sudo systemctl restart superci-pull`).

```bash
cat > override.json <<'JSON'
{"daily_usd": 500,
 "pools": {"superci-4cpu": {"floor": [2,2,2,2,2,2,2,2,2,2,2,2,2,2,2,2,2,2,2,2,2,2,2,2],
                            "cap":   [40,40,40,40,40,40,40,40,40,40,40,40,40,40,40,40,60,60,60,60,60,60,60,60]}}}
JSON
pull/deploy/ssh.sh /usr/local/bin/superci-pull -validate -config /dev/stdin < override.json   # valid: /dev/stdin
aws --profile ci-runners s3 cp override.json s3://superci-pull-243891825325/config/config.json
```

A pin until the file next changes: `curl -s -X POST 'localhost:8080/override?set=superci-4cpu&pool=20&max=300'`
(`pool=-1` drops the pin).

**Roll back a config.** The bucket is versioned: copy the previous version of `config/config.json` over the current
one (as for the binary below); the instance installs it within a minute.

**Read metrics.** The dashboard `superci-pull` (us-east-1) shows the alarms, queue wait, jobs waiting and machines per
set, the registration rates, registrations and errors, spend against the daily cap, and the listener's warnings.
CloudWatch, namespace `superci-pull`: `Heartbeat`, `OrgRegistrationRate`, `OthersRegistrationRate`,
`OurRegistrationRate`, `SpareRegistrationRate`, `RegistrationWindowUse`, `OrgRateKnown`, `Registrations`,
`RegistrationRefusals`, `RegistrationErrors`, `LaunchShortfall`, `QueueWait` (seconds per started job, with
percentiles), `StalledSets`, `Orphans`, `SpendTodayUSD`, `SpendDailyCapUSD`, `SpendMonthUSD`, `SpendPerHourUSD`,
`SpendCapped`, `Machines` and `OnDemandShare`; per set (dimension `Set`): `Machines`, `IdleRunners`, `BusyRunners`,
`Pool`, `JobsWaiting`.

**Alarms** (`deploy/alarms.sh`, topic `superci-pull-alarms`). The topic has no subscriber until one is added:

```bash
aws --profile ci-runners --region us-east-1 sns subscribe --topic-arn arn:aws:sns:us-east-1:243891825325:superci-pull-alarms --protocol email --notification-endpoint <address>
```

| Alarm | Fires when |
| --- | --- |
| superci-pull-listener-down | no heartbeat for 3 minutes |
| superci-pull-jobs-stalled | a set's jobs have waited 10 minutes with none of its jobs started, for 2 minutes |
| superci-pull-registration-refusals | GitHub refused a registration in a 5-minute period |
| superci-pull-registration-errors | a registration failed otherwise in a 5-minute period (GitHub's API, a name GitHub already held, the issued tag refused) |
| superci-pull-queue-wait-p90 | queue wait p90 over 60 s in two 5-minute periods running |
| superci-pull-orphans | orphans above 0 for 10 minutes |
| superci-pull-spend-daily | today's spend past 80% of the daily cap the listener runs with (`SpendDailyCapUSD`) |
| superci-pull-spend-capped | a spend cap is reached: CI on superci stops |
| superci-pull-launch-shortfall | EC2 started fewer machines than asked for 15 minutes running |
| superci-pull-org-rate-unknown | the organization's rate unmeasured for 15 minutes |

**Release.** Off-peak (before 13:00Z or after 01:00Z), since a release restarts the listener: jobs wait about 10 s.
A listener that cannot reach its claims bucket does not start: run `deploy/up.sh` (it makes the bucket and grants
the role) before the first release that has claims. `deploy/build.sh <out>` builds and tests on a tagged arm64 instance and terminates it;
`deploy/release.sh <binary> [override.json] pull/deploy/ssh.sh` uploads to `s3://superci-pull-243891825325`
(checking the override with the new binary on the listener first) and restarts the listener (a graceful stop closes
its sessions, so the next opens at once).

**Roll back.** The bucket is versioned, the binary and the override config alike: copy the previous version over the
current one and restart.

```bash
B=superci-pull-243891825325
aws --profile ci-runners s3api list-object-versions --bucket $B --prefix bin/superci-pull --query 'Versions[].[VersionId,LastModified]' --output text
aws --profile ci-runners s3api copy-object --bucket $B --key bin/superci-pull --copy-source "$B/bin/superci-pull?versionId=<previous>"
pull/deploy/ssh.sh "sudo aws s3 cp --quiet s3://$B/bin/superci-pull /usr/local/bin/superci-pull && sudo systemctl restart superci-pull"
```

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
since #34100 routes a switched job to `superci-<N>cpu`); to stop launching at once,
`curl -s -X POST 'localhost:8080/override?max=0'` (machines end as their jobs finish).

**Resilience.** systemd restarts the process (5 s). A health timer marks the instance unhealthy after five failed
checks in a row and the Auto Scaling group replaces it. A check fails when the process's reconciler has not finished a
pass for a minute, or when the instance does not hold the Elastic IP; GitHub's or EC2's slowness does not fail it. An
instance that cannot take the Elastic IP at boot marks itself unhealthy at once. The group also replaces an instance
EC2 reports down. A replacement takes the Elastic IP and the TLS key, so machines keep their address and pin, adopts
every machine of the deployment from EC2 and every runner from GitHub, and decides nothing until every region is
listed and each set's session has given the queue's statistics. After a crash, GitHub refuses a new session for a
set while the dead one's lapses (30 to 75 s measured).

**Deploy from scratch.** `deploy/up.sh <admin-cidr>` (role, parameters, bucket, groups, address, launch template),
`deploy/release.sh <binary>`, `deploy/up.sh` again (it starts the group once a release exists), `deploy/alarms.sh`.
The listener makes the runner group `superci-pull` at its first start. **Tear down.** `deploy/down.sh` deletes the
scale sets and every resource tagged `purpose=superci-pull`; the empty runner group stays at GitHub.

**Load test.** `bench/workflow.py` writes the workflow (a wave of mixed sizes per push, a matrix burst for a push
whose message says "burst"); `bench/measure.py BRANCH SINCE UNTIL listener.jsonl` measures it against the pass bar;
`bench/orphans.py` lists orphaned machines and runners.

## Security

- **Who may use the runners.** The runner group `superci-pull` admits megpt-mono only (`repositories` in the config),
  never a public repository. The listener makes and keeps the group with the App's
  `organization_self_hosted_runners: write`, and acts only on its own group's sets.
- **What a job can do.** A job is root on its machine, can read its identity document and its user data, and can
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

## Known limits

- **GitHub's registration cap is the fleet's real limit.** Every job is a JIT registration, and the rest of the
  organization used 2 to 4.9 of GitHub's 5 a second through the morning. Moving jobs from Blacksmith moves their
  registrations, so ramp the fleet switch gradually and watch `OthersRegistrationRate`; adding load (a test) needs
  a pool registered ahead.
- **A burst waits on registrations past the pool.** A burst of N jobs on a size whose pool holds P idle runners
  starts P at once; the rest wait for machines (about 15 s from launch to a registered runner) and for
  registrations at the spare rate S (`SpareRegistrationRate`, `cap x 0.8 / 300 - others' rate`). The last job of the
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
