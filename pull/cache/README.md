# pull/cache

GitHub's Actions cache for the machines of `superci-pull`, kept in S3 in the same region as the machine.

A workflow changes nothing. `actions/cache@v4` and later, `actions/setup-node`, `setup-go`, `setup-python`, `setup-bun`
and every other action that caches through `@actions/cache` find their cache here instead of at GitHub. Nothing is
published to GitHub's cache. Where the machine cannot reach this cache (no role, no endpoint, the daemon down) the
action reaches GitHub's own cache as it would without this.

## How it works

```
job step: actions/cache (JavaScript, run by the runner's Node)
   |  ACTIONS_RESULTS_URL is changed by the wrapped Node:  externals/node*/bin/node  ->  pullcache  ->  node.real
   v
pullcache serve   one process per machine, on the machine's private address (systemd, restarts itself)
   |  CacheService calls (twirp JSON): lookup, begin, finish     everything else goes on to GitHub unchanged
   |  uploads: the SDK's Put Block / Put Block List, kept as blocks, joined by S3 (UploadPartCopy) at finish,
   |           written with credentials the broker issued for the job's own scope
   |  downloads: one GET of the entry, read from S3 as 16 MiB ranges of one version, twelve at a time, sent in order
   v   (S3 gateway endpoint in the machine's VPC: no NAT, no data charge)
S3, in the machine's region        entries: v1/<d|r>/<repository id>/<scope>/<version>/<key>   one object each
                                   blocks:  u/<repository id>/<scope>/<upload id>/<block>      stage bucket
                                   marks:   a/<sha256 of the entry's name>/<yyyymmdd>           empty; the entry was restored that UTC day
broker (Lambda, per region)        job token -> credentials for that scope's two prefixes, an hour at a time
mirror (Lambda, per save of main)  copies the default branch's new entry into the other region's bucket, written once
janitor (Lambda, every 15 min)     deletes entries nobody restores, and superseded and least recently used ones past the cap; the only role that can
```

### Scope, as GitHub's cache has it

The job's `ACTIONS_RUNTIME_TOKEN` is a JWT signed by GitHub. `pullcache` checks its RS256 signature against GitHub's
keys (`token.actions.githubusercontent.com/.well-known/jwks`, kept in memory, read again for an unknown key at most once
a minute), its issuer (exactly), and its expiry (which it must have), and reads two claims: `repository_id` and `ac`, the list of scopes the job may
read (`Permission` 1) and write (2), in the order to look. A pull request has its own `refs/pull/N/merge` (read and
write) and `refs/heads/main` (read). The scope is part of every object's name, so:

- a job reads its own scope first, then the others it is given, as GitHub does: in each scope in turn, the key itself,
  then each restore key as a prefix (the newest entry)
- a job writes only into the scope its token can write, and never over an entry: finishing a save completes the S3
  upload with `If-None-Match: *`, so a second save of a key (a race, or a job that did not look) is turned away
- the machine's own role cannot write at all. A save is written with credentials from the broker, a Lambda function
  that checks the token as the daemon does and assumes the writer role with a session policy naming the token's one
  writable scope: `PutObject` and `AbortMultipartUpload` under `v1/<class>/<repo>/<scope>/`, and `PutObject`,
  `GetObject` and `DeleteObject` under `u/<repo>/<scope>/` in the stage bucket. They last an hour, are asked for
  again five minutes before they run out, and are kept per token. A step that takes the machine's credentials, or the
  daemon's, writes no more than the job's token allows; a pull request can never write `main`'s scope
- the bucket refuses any write into `v1/` that creates an object without `If-None-Match: *`, whoever sends it, so an
  entry cannot be written over even with the writer role
- a download reads every range with `If-Match` on the ETag the lookup saw, so an entry deleted and saved again while
  it is read ends the download short (a miss in `actions/cache`), never a mix of two archives
- a token that can only read (a fork's pull request) is told `cache write denied:`, which `actions/cache` v6 reports as a
  policy denial
- another repository, or another pull request, never sees an entry
- a token that is not GitHub's, is expired or has no expiry, or has been edited after signing is refused with 401;
  the daemon's log names the token's `iss` and `kid` (never the token), so the first real job shows what GitHub's
  runtime token carries

Entries are never changed once written. The scope of `main` is class `d`, every other scope class `r`.

### Eviction

| What | Rule | Where it is set |
| --- | --- | --- |
| Class `r` (pull requests, merge-queue batches, other branches) | gone 3 days after it was saved | `deploy/lifecycle.json` |
| Class `d` (the default branch) | no age rule in S3. The janitor deletes an entry nobody has restored for `MAX_IDLE` (30 days), and a superseded one nobody restored for 2 days, however small the bucket's use | `deploy/up.sh` (`MAX_IDLE=`), `cache.DefaultSupersededGrace` |
| Size | the janitor function, every 15 minutes in each region, deletes until the region holds at most `CAP_GB` (1,000 GB): superseded entries first, then other scopes' entries, then the default branch's, each the least recently restored first (below) | `deploy/up.sh` (`CAP_GB=`) |
| Markers of use | gone 35 days after they were written | `deploy/lifecycle.json` |
| An upload's blocks | deleted at finish, with the job's own credentials; the stage bucket expires anything left after a day | `deploy/lifecycle-stage.json` |
| Old versions, delete markers | versioning is suspended, so a deleted entry frees its bytes at once; versions from before that expire a day after they became old | `deploy/lifecycle.json` |

An entry in use is never old. The default branch's entries used to expire 30 days after their save however hot they
were, and the janitor deleted oldest save first: on the day a key that every job restores expired, every job missed
at once and each tried to upload 0.3 to 0.5 GB that write-once refused (a stampede), and superseded entries, which
nothing restores, were kept as long as the hot ones. Now the order is by use, and nothing but the janitor decides it.

**Last use.** A restore that returns an entry has the daemon write an empty object `a/<sha256 of the entry's name>/<yyyymmdd>`
(`cache.MarkKey`), once per entry per UTC day: the daemon remembers the markers it wrote, asks the bucket (a HEAD, a
tenth of a write's price) before it writes one, and creates it only if it is absent (`If-None-Match: *`). However many
jobs and machines restore a hot entry, its day is one write and a read per machine, so no key is hot (the hash leads the
name, so the markers of the hot entries spread over the bucket's partitions). A marker is a hint, not a fact a restore
depends on: a lost, late or forged one changes what is evicted first and never what a restore returns, so the daemon
writes it beside the restore and a failure only logs. A day is the finest a marker says, and it counts as the day's end
(an entry is never called less used than it was). An entry no marker names was last used when it was saved.

**Superseded.** An entry's key is a name and the hashes of its inputs, and a changed lockfile saves a new key of the same
name. The *family* of a key is the key without its trailing hash segments (`cache.Family`: 8 or more lowercase hex digits,
which covers digests, run ids and dates, or a base64 text of 16 or more characters that holds a digit or one of `+/=_`;
the first segment always stays). In the production buckets on Nov 25:

| Key | Family |
| --- | --- |
| `git-seed-v1-Linux-X64-45662624410` | `git-seed-v1-Linux-X64` |
| `acid-vp-deps-v1-Linux-X64-2503440cae4c07a2-bd71c5201090...` | `acid-vp-deps-v1-Linux-X64` |
| `bun-modules-v1-Linux-X64-bun1.4.3-aura-hono-api-scripts-on-91ff91ba...` | `bun-modules-v1-Linux-X64-bun1.4.3-aura-hono-api-scripts-on` |
| `bun-+NUKu9E/U4apAe78eSCZvQ/GSdc=` | `bun` |
| `uv-toolcache-v1-Linux-X64-uv0.13.9`, `apt-Linux-X64-playwright-chromium-2026-W48` | themselves (a version or a week is not a hash) |

Entries of one family in one (class, repository, scope, version) are ranked by save time, and the newest is kept. Any
other is superseded when nobody restored it after the newest was saved: a family that is too wide for its name (two live
caches that share it) loses nothing it still restores, and a key that is not a hash is its own family, so nothing is
ever called superseded on a guess about a name. Save time is `Object.SaveTime`, the notion a restore key uses: the
save's own time, which a copy in the other region keeps as `x-amz-meta-saved` (the copy's `LastModified` is when it was
copied). A listing carries no metadata, so when a run has something to delete the janitor first reads the save time of
the default branch's entries that share a family with another (a HEAD each, 32 at a time), and plans again on that.
A run with nothing to delete reads nothing more than its listings. A superseded entry that nobody restored for two days
(`DefaultSupersededGrace`) is deleted whatever the size, so a key saved every 12 minutes, as the git seed is, holds
days of saves and not a terabyte of them.

**The delete.** The janitor lists the entries and the markers, computes the plan as a function of that listing alone
(`cache.Plan`, so two runs over one listing agree, and a run over what a run left deletes nothing), and deletes each
entry with the ETag it listed (`DeleteObjects` with each object's `ETag`; S3 answers `PreconditionFailed` for an entry a
job saved again since, which stays, and `NoSuchKey` for one another janitor deleted first). Janitors may run at once, or
one again after a failed run. The janitor runs as its own role, the only one that can delete an entry (`ListBucket` and
`DeleteObject` on `v1/`, nothing else). An extra terabyte of retention costs about $23 a month, so the cap is where hit
rate is bought: at the assumed 10 TB of saves a day, 2 TB in all keeps a pull request's entry about five hours, and the
default branch's for as long as other scopes' entries are there to go first. The cap is not tuned to megpt-mono's real
save volume, which is not known yet: read `save ... saved N MB` in the daemon's log for a few days and set `CAP_GB` from
it. `pullcache janitor -cap-gb N -dry-run` (or `-cap-mb`) lists what a cap would delete, one line each with its reason and
last use, with list rights and nothing else.

### Two regions

The launcher spreads machines over `us-east-1` and `us-east-2` (equal weights by default). Each region has its own
bucket, broker, janitor and mirror, and a machine reads only its own region's bucket, so it never pays for a
cross-region read.

A default-branch job runs in one region, so it saves its entry in one bucket: on Nov 25 the git seed was only in
us-east-2 and the Go modules only in us-east-1, and a pull request's job in the other region missed each. The mirror
function copies each entry the default branch saves into the other bucket, on the bucket's notice of the new object.
It reads the version it was told of (`If-Match` on its ETag) and completes the copy with `If-None-Match`, so it never
writes over an entry: a key both regions saved keeps each region's own first save, and an entry deleted and saved
again mid-copy ends the copy. A copy is 16 MiB part copies, 32 at a time (one cross-region `CopyObject` of a 452 MB
entry took 88 s, ten parts at a time 13 s). A copy keeps its original's save time as `x-amz-meta-saved`
(its own LastModified is when it was copied), and a restore key takes the newest save (`cache.Newest`, by
`Object.SaveTime`), so its newest entry is the same in both regions. Pull requests' entries are not copied: they are read by later runs of the
same pull request only, and they are most of what is saved. S3 replication is not used: it needs versioning, and its
writer is one the `If-None-Match` rule cannot see (two regions saving one key at once overwrote each other through
it). Each copy logs its lag from the save (`lag_ms`), the metric `superci-pull-cache/MirrorLagMs`, and the alarm
`superci-pull-cache-mirror-lag` fires on a copy over 5 minutes. `pullcache mirror -bucket B -region R -peer-bucket P -peer-region PR` copies whatever an entry prefix of B
(`-prefix`, `v1/d/` by default) holds that P lacks, as the function does for one save.

The default branch's entries are kept by use, not by age (see Eviction): the warm jobs of `ci-cache-warm.yml` save them
again within an hour of a miss (most every 12 minutes), and the git seed is a new 450 MB key every 12 minutes. The janitor
keeps the newest seed of a region and deletes the ones superseded for two days, so the seeds hold about 100 GB a region
(48 hours of saves) instead of the 1,600 GB that 30 days of them would pass the cap with.

## Why this and not the others

The measurements below are this change's. What is said of the others is from their documentation and source, read
Nov 25 2026, and was not run.

| | `superci-cache` (the old push plane's) | RunsOn magic cache, `runs-on/cache` | a cache server with a database (for example falcondev-oss/github-actions-cache-server) | **`pullcache` (this)** |
| --- | --- | --- | --- | --- |
| Unmodified `actions/cache` and `setup-*` | yes (wraps Node as here) | an S3 cache built into RunsOn's runner (`extras=s3-cache`), whose server and agent are commercially licensed; `runs-on/cache` is MIT but replaces the action in every workflow | yes, with the results URL pointed at it | yes (wrapped Node) |
| Scope rules per ref | in the plane, from GitHub's `ac` claim | not checked | its README says the cache is scoped to the repository; per ref not documented | in the daemon, from `ac`, in the object name |
| What it needs besides S3 | the push plane's control plane and key-value store, which this fleet does not have, and which is being retired | RunsOn's server | a database every machine can reach (SQLite is per machine) and a server | nothing |
| Data path | a download is proxied by the machine | S3 | optional signed storage URLs | a download is proxied by the machine, 16 MiB reads in parallel |
| Eviction | LRU by size, per repository, in the plane's index | S3 lifecycle | age and size, in the server | superseded first, then least recently used by a daily marker, in a size janitor; S3 lifecycle only for pull requests |
| Run cost besides S3 | the plane's functions and store | a license from EUR 300 a year | a server and a database | none |

`pullcache` wins because it needs nothing to run but the bucket and two small functions. The old plane's cache was a
good design for its plane, a control plane that holds the index and signs links so the machines hold no keys. The
pull fleet has no plane. Putting the index in S3 itself, as the object's name and `If-None-Match`, replaces the
plane's key-value store with the one thing already there. The broker replaces its signed links: what a job can write
is decided from the job's token, by a function the job cannot change, not by the machine. A step can take the
machine's role (a job's steps run as a user with `sudo`), and with it read any entry in the region and call the
broker with its own token, and nothing more. The machine's role and the writer role are refused by the bucket policy
outside the region's VPC endpoint, so credentials carried off the machine are of no use.

Reads are not scoped the same way: the machine's role reads every entry, so a step could read another pull request's
or another repository's entries in the account. GitHub's cache would not let it. Only integrity was asked for here;
scoping reads too would move `GetObject` into the broker's credentials, a call to it for every job that restores
(about 120,000 a day at the assumed load, still under a dollar a month).

## Measured (Nov 25 2026, c6i.xlarge, the fleet's image, 30 GB gp3 at 3,000 IOPS and 250 MB/s, same region)

Real `actions/cache` v6.1.0 (the release the monorepo pins, `55cc834`), run by the runner's own wrapped Node, against
this bucket. Token signed by a key of the proof's own. 2,001 MiB of random files (incompressible, so the archive is
2,000 MB).

| | Wire (the daemon's own log) | The whole step (tar, zstd, wire, untar) |
| --- | --- | --- |
| Save, workspace on EBS | blocks reach the bucket at 664 to 672 MB/s, joined by S3 in 1.5 to 1.9 s | 11.3 to 12.2 s, 165 to 177 MB/s |
| Restore, workspace on EBS | 555 to 565 MB/s | 12.2 to 12.7 s; the rest is untarring 2 GB onto a 250 MB/s volume |
| Save, workspace in RAM (1,000 MiB) | 961 MB/s | 4.1 s |
| Restore, workspace in RAM (1,000 MiB) | 552 MB/s | 2.9 s |

The download is one GET because `@actions/cache` reads a non-Azure address that way (it makes concurrent 4 MiB reads
only for `*.blob.core.windows.net`). One S3 read goes at about 90 to 100 MB/s, which is what the first version of this
daemon measured; reading the entry as sixteen-MiB ranges, twelve ahead, took it to 555 MB/s. At 100 MB/s Node used a third of a
core and the daemon a tenth, so neither was busy; the client's one stream is the likely limit now (not measured further).

Again on Nov 25 2026, with the machine's role read-only and every save written with the broker's credentials
(c6i.2xlarge, `proof/run.sh big`): a 2,001 MiB save at 727 MB/s on the wire, joined in 1.7 s, the step 11.4 s; the
restore at 669 MB/s with every range sent `If-Match`, the step 8.5 s, files identical.

Credentials, tried on the bucket from that machine (`proof/run.sh iam`, all passing): the machine's role is
`AccessDenied` creating an entry in `main`'s scope or a pull request's, and putting a block, and can read. The broker
issues pull request 7's credentials for `v1/r/77/refs%2Fpull%2F7%2Fmerge/` and `u/77/refs%2Fpull%2F7%2Fmerge/`,
59 minutes left, and refuses a read-only token. With them: `AccessDenied` creating an entry in `main`'s scope or
another repository's, allowed in its own; writing over its own entry is `AccessDenied` without `If-None-Match` and
`PreconditionFailed` with it; creating an entry without it is `AccessDenied`; a block in `main`'s stage is
`AccessDenied`, in its own allowed, and deleted; deleting an entry is `AccessDenied`. `main`'s credentials create an
entry in `main`'s scope.

An entry replaced mid-download: a client reading the 2,100 MiB entry at 40 MB/s was paused at 109 MB, the entry was
deleted (as the janitor does) and saved again as other bytes, and the client resumed. The daemon's next read got
`412 PreconditionFailed`, it logged `cut short after 301989888 of 2306867200 bytes: the entry changed while it was
read`, and curl ended `(18) transfer closed with 2004877312 bytes remaining`. The 301,989,888 bytes it got are the
first bytes of the version it began with. (Without the pause, a read that lands between the delete and the new save
gets `404`, with the same end.)

Scope check, five real `actions/cache` runs and one raw call, all passing (`proof/run.sh scope`, run again with the
broker): main saves `deps`;
a pull request restores main's entry and saves the same key into its own scope; main's object is unchanged in key,
size and ETag, and main still restores its own bytes; the pull request now restores its own bytes first; a second pull
request still gets main's; a second pull request cannot see the first's entry; a read-only token writes nothing and is
told `cache write denied:`; a forged token gets 401.

## Cost

S3 Standard prices: writes, copies and lists $0.005 per 1,000; reads $0.0004 per 1,000; storage $0.023 per GB-month;
the gateway endpoint and in-region transfer $0. The workload below is **assumed**, not measured: 200,000 jobs a day
(given), four lookups each, 60% restoring 1 GB, 5% saving 1 GB (10,000 saves, 10 TB a day, over both regions).

| Per day | Requests | Cost |
| --- | --- | --- |
| Lookups: 800,000 HEAD, 320,000 LIST for restore keys | | $0.32 + $1.60 |
| Restores: 120,000 x 1 GB = 60 ranged reads of 16 MiB each | 7.2 million GET | $2.90 |
| Saves: 10,000 x 1 GB in 32 MiB blocks = 30 block PUTs, 30 UploadPartCopy, create, complete, one DeleteObjects | 630,000 | $3.15 |
| Broker: at most one call per saving job per hour, 256 MB for about 0.1 s | about 10,000 | under $0.01 |
| Janitor: 96 runs a region, a listing of a few thousand entries and markers and a batch delete each | about 400 | under $0.01 |
| Markers: a HEAD per machine and entry restored a day, one write per entry a day | under 150,000 | under $0.10 |
| **Requests** | | **about $8 a day, $240 a month** |

Storage is the janitor's cap: 1,000 GB a region, 2 TB in all, plus what 15 minutes of saves add before the next run
(about 100 GB), $48 a month. Versioning is suspended, so a deleted entry costs nothing after it goes. With versioning
on, each deleted entry would stay a day or two as an old version: at 10 TB of saves a day that is 10 to 15 TB held,
$230 to $345 a month, most of the review's estimate of about $500 for a janitor at 300 GB. Blocks are deleted when a
save ends; those of a save that never ends wait a day or two (assume 1% of saves, $4 a month). Nothing is replicated.
About **$290 a month** at the stated assumptions, against about $950 for lifecycle rules alone (30 TB of class `r`
alive at once). The request cost is nearly flat in restored gigabytes (a restored GB costs $0.00002), so a heavier
workload costs storage, which the cap bounds, not requests. Each further TB of cap is $23 a month.

## Deploy

```bash
pull/deploy/build.sh /tmp/release [commit]  # a commit of main: the daemons, the functions' code (the same program, named
                                             # bootstrap) and the machine start script, each binary carrying the commit
pull/cache/deploy/release.sh /tmp/release    # each bucket's bin/ with bin/RELEASE.json, then up.sh, then verify-release.sh cache
pull/cache/deploy/up.sh                      # endpoints, buckets, roles, functions, schedules, in both regions; run again at will
```

`pullcache version` prints the commit a daemon was built from, and `serve` and each function log it at start
(`-X main.revision=<sha>`; `-trimpath` keeps it out of `go version -m`). `bin/RELEASE.json` names the commit, the build
time and each file's sha256; a machine runs only the daemon it names, and `up.sh` puts only its zip on the functions.
The release, its checks and its rollback are in the listener's README (Release).

`deploy/down.sh` removes the endpoints, functions, schedules and roles and keeps the buckets; `-buckets` removes those
too.
Everything made is tagged `purpose=superci-pull-cache`.

### The seam with the launcher

The machines need three things from the launcher: two lines in `boot.sh` after `T=`, the instance profile in the
launch template (and in the check that decides whether the template changed), and `iam:PassRole` for the listener.
`SEAM.md` says exactly where, against `pull-prod`; branch `pull-prod-cache` applies it. The cache's write isolation
depends on one job per machine, which `pull-prod` keeps (`SEAM.md`, "One job per machine").

Settings the daemon reads (all optional on a machine, all set by `up.sh`): `PULLCACHE_BUCKET`, `PULLCACHE_REGION`,
`PULLCACHE_STAGE_BUCKET` (default `<bucket>-stage`), `PULLCACHE_DEFAULT_BRANCH` (`main`), `PULLCACHE_EXTERNALS`,
`PULLCACHE_LISTEN`, `PULLCACHE_PORT` (8257), `PULLCACHE_UPSTREAM`, `PULLCACHE_JWKS_URL`, `PULLCACHE_ISSUER`,
`PULLCACHE_BROKER` (`superci-pull-cache-broker`). The broker reads `PULLCACHE_DEFAULT_BRANCH`, `PULLCACHE_JWKS_URL`
and `PULLCACHE_ISSUER` too, and they must agree with the machines': a broker that works out another grant than the
daemon expected is refused by the daemon, and the save is not made.

The issuer is the one setting that has to be changed after the first real job if GitHub's runtime token names another.
`ISSUER=<iss> deploy/up.sh` writes it to each bucket's `bin/issuer`, sets it on the broker, and `deploy/machine-up.sh`
reads the same object at boot for the machine's daemon (`PULLCACHE_ISSUER` on a machine overrides it). A run of
`up.sh` without `ISSUER` keeps what the bucket holds, so the broker's setting is not wiped; `ISSUER=default` removes
it. A machine that booted before the change keeps its old issuer until it ends, and a machine lives for one job.

### Operate

```bash
sudo journalctl -u pullcache -o cat -f       # on a machine: restore "k": found ...; save "k": saved N MB; blocks in ...
aws --profile ci-runners logs tail /aws/lambda/superci-pull-cache-broker --region R    # issued / refused, with iss and kid
aws --profile ci-runners logs tail /aws/lambda/superci-pull-cache-janitor --region R   # what each run deleted
pullcache janitor -bucket B -region R -cap-gb 1000 -dry-run   # with list rights: what a cap would delete
```

## What was not tested

- A real GitHub job token. The proof signs its own tokens, with the claim names superci's plane reads from GitHub's
  (`iss`, `repository_id`, `ac`, `exp`). The first job on a pull machine will show whether GitHub's token has them as
  expected, and whether it is signed by the keys at GitHub's JWKS; a wrong guess is a miss, not a failed job, and
  the refusal names the token's `iss` and `kid`. Watch `journalctl -u pullcache` there once (`SEAM.md`, Check).
- The janitor deleting under load. It ran on the schedule and deleted to its cap in the proof, not against a bucket
  of millions of entries; its listing reads at most 5 million entries a run.
- Eviction by use in production. The unit tests run the plan over a simulated bucket and the S3 tests (set
  `PULLCACHE_TEST_BUCKET` to a bucket of yours ending in `-pull-evict`, and its region) run the conditional delete, the
  marker write and the whole janitor against S3. The janitor's dry run on Nov 25 read buckets with no markers yet, a
  day old, so it ranked by save time; no machine has written a marker, and the machines' role needs the `Mark` statement
  of `deploy/up.sh` first.
- The broker's credentials being renewed after an hour inside one long save; the SDK's credential cache asks again
  five minutes before they run out, and no save in the proof lasted that long.
- A job in a container (`container:`). The wrapped Node is reached through the runner's externals at `/__e` and the
  daemon listens on the private address a container reaches, but no container job was run.
- arm64 machines. The binary is built and published, and not run.
- The listener launching machines with the profile (`SEAM.md`). The proof machine was launched with it by hand.
- `docker/build-push-action` with `type=gha`, and `useblacksmith/setup-docker-builder` (Blacksmith's own, which has
  to be replaced separately).

## Proof

`proof/run.sh scope|big|iam` is what produced the numbers (`proof/mint` signs the tokens). It needs a machine of the
fleet's image with the machines' role, a checkout of `actions/cache`, and the broker pointed at the proof's key set
(`BROKER_JWKS_URL=<an address of it the function can read> deploy/up.sh`; run `up.sh` again without it afterwards).

## `pullcache pin`

`pullcache pin [-max-bytes n] [-max-files n] DIR` locks the regular files under DIR in the page cache (each is mapped and
`mlock`ed) and holds the locks until it is killed or the machine powers off. It skips `node_modules` and `.git` but
`.git/index`, prints one line (`pinned files=N bytes=N seconds=S capped=no|yes|stopped failed=N`) and stops locking at
either cap or at the first lock the machine refuses (RLIMIT_MEMLOCK), which it reports and never exits non-zero for.
boot.sh starts it on the checkout `pullcache hydrate` printed (`checkout <dir>`); see [pull/README.md](../README.md), "Pin".
