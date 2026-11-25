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
   |  uploads: the SDK's Put Block / Put Block List, kept as blocks, joined by S3 (UploadPartCopy) at finish
   |  downloads: one GET of the entry, read from S3 as 16 MiB ranges, twelve at a time, sent in order
   v   (S3 gateway endpoint in the machine's VPC: no NAT, no data charge)
S3, in the machine's region        entries: v1/<d|r>/<repository id>/<scope>/<version>/<key>   one object each
                                   blocks:  u/<upload id>/<block>        in an unversioned bucket of its own
```

### Scope, as GitHub's cache has it

The job's `ACTIONS_RUNTIME_TOKEN` is a JWT signed by GitHub. `pullcache` checks its RS256 signature against GitHub's
keys (`token.actions.githubusercontent.com/.well-known/jwks`, kept in memory, read again for an unknown key at most once
a minute) and its issuer and expiry, and reads two claims: `repository_id` and `ac`, the list of scopes the job may
read (`Permission` 1) and write (2), in the order to look. A pull request has its own `refs/pull/N/merge` (read and
write) and `refs/heads/main` (read). The scope is part of every object's name, so:

- a job reads its own scope first, then the others it is given, as GitHub does: in each scope in turn, the key itself,
  then each restore key as a prefix (the newest entry)
- a job writes only into the scope its token can write, and never over an entry: finishing a save completes the S3
  upload with `If-None-Match: *`, so a second save of a key (a race, or a job that did not look) is turned away
- a token that can only read (a fork's pull request) is told `cache write denied:`, which `actions/cache` v6 reports as a
  policy denial
- another repository, or another pull request, never sees an entry
- a token that is not GitHub's, is expired, or has been edited after signing is refused with 401

Entries are never changed once written. The scope of `main` is class `d`, every other scope class `r`.

### Eviction

| What | Rule | Where it is set |
| --- | --- | --- |
| Class `r` (pull requests, merge-queue batches, other branches) | gone 3 days after it was saved | `deploy/lifecycle.json` |
| Class `d` (the default branch) | gone 30 days after it was saved; replicated to the other region | `deploy/lifecycle.json` |
| Size | `pullcache janitor -cap-gb N` deletes class `r` then class `d`, oldest first, until under N | run from any host with delete rights |
| An upload's blocks | deleted at finish; the stage bucket expires anything left after a day | `deploy/lifecycle-stage.json` |

Age is from the save, not the last use: an entry in use is saved again, by one job, when it expires. The age rules
bound the cost of a quiet week and the janitor bounds a busy hour. An extra terabyte of retention costs about $23 a
month, so the cap is where hit rate is bought. Neither bound is tuned to megpt-mono's real save volume, which is not
known yet: read `save ... saved N MB` in the daemon's log for a few days and set `-cap-gb` from it.

### Two regions

The launcher spreads machines over `us-east-1` and `us-east-2` (equal weights by default). Each region has its own
bucket, so a machine never pays for cross-region reads. Class `d` is replicated both ways by S3 (about 20 s, measured),
so a pull request in either region reads the default branch's entries. Class `r` is not replicated.

## Why this and not the others

The measurements below are this change's. What is said of the others is from their documentation and source, read
Nov 25 2026, and was not run.

| | `superci-cache` (the old push plane's) | RunsOn magic cache, `runs-on/cache` | a cache server with a database (for example falcondev-oss/github-actions-cache-server) | **`pullcache` (this)** |
| --- | --- | --- | --- | --- |
| Unmodified `actions/cache` and `setup-*` | yes (wraps Node as here) | an S3 cache built into RunsOn's runner (`extras=s3-cache`), whose server and agent are commercially licensed; `runs-on/cache` is MIT but replaces the action in every workflow | yes, with the results URL pointed at it | yes (wrapped Node) |
| Scope rules per ref | in the plane, from GitHub's `ac` claim | not checked | its README says the cache is scoped to the repository; per ref not documented | in the daemon, from `ac`, in the object name |
| What it needs besides S3 | the push plane's control plane and key-value store, which this fleet does not have, and which is being retired | RunsOn's server | a database every machine can reach (SQLite is per machine) and a server | nothing |
| Data path | a download is proxied by the machine | S3 | optional signed storage URLs | a download is proxied by the machine, 16 MiB reads in parallel |
| Eviction | LRU by size, per repository, in the plane's index | S3 lifecycle | age and size, in the server | S3 lifecycle by class, plus a size janitor |
| Run cost besides S3 | the plane's functions and store | a license from EUR 300 a year | a server and a database | none |

`pullcache` wins because it needs nothing to run but the bucket. The old plane's cache was a good design for its
plane, a control plane that holds the index and signs links so the machines hold no keys. The pull fleet has no plane.
Putting the index in S3 itself, as the object's name and `If-None-Match`, replaces the plane's key-value store with
the one thing already there, and a machine's role (read and write of two prefixes, usable only through the VPC
endpoint) replaces its signed links. The price is the one the plane avoided: a job's step can use the machine's role.
Everything on the machine already shares one trust boundary (the job holds the repository's secrets), and the role
cannot delete an entry, cannot reach anything outside the cache's buckets, and is refused by the bucket policy from
outside the endpoint, but a malicious step could still write, or write over, an entry in `main`'s scope. The scope checks protect the
repository from honest jobs and from mistakes, not from a step the repository's own writers did not write.

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

Scope check, five real `actions/cache` runs and one raw call, all passing (`proof/run.sh scope`): main saves `deps`;
a pull request restores main's entry and saves the same key into its own scope; main's object is unchanged in key,
size and ETag, and main still restores its own bytes; the pull request now restores its own bytes first; a second pull
request still gets main's; a second pull request cannot see the first's entry; a read-only token writes nothing and is
told `cache write denied:`; a forged token gets 401.

## Cost

S3 Standard prices: writes, copies and lists $0.005 per 1,000; reads $0.0004 per 1,000; storage $0.023 per GB-month;
the gateway endpoint and in-region transfer $0; replication $0.02 per GB. The workload below is **assumed**, not
measured: 200,000 jobs a day (given), four lookups each, 60% restoring 1 GB, 5% saving 1 GB.

| Per day | Requests | Cost |
| --- | --- | --- |
| Lookups: 800,000 HEAD, 320,000 LIST for restore keys | | $0.32 + $1.60 |
| Restores: 120,000 x 1 GB = 64 reads each | 7.7 million GET | $3.07 |
| Saves: 10,000 x 1 GB = about 34 write-class requests each | 340,000 | $1.70 |
| **Requests** | | **about $6.70 a day, $200 a month** |

Plus storage at the janitor's cap (300 GB is $7 a month; each further TB $23), and replication of the default
branch's saves (assume 50 GB a day, $30 a month). About **$250 a month** at the stated assumptions. The request cost
is nearly flat in restored gigabytes (a restored GB costs $0.00003), so a heavier workload costs storage, not requests.
Nothing runs for the cache but the machines' own CPU (a tenth of a core while a 2 GB entry streams, 2 GiB memory cap).

## Deploy

```bash
pull/cache/deploy/up.sh                      # endpoints, buckets, roles, replication, in both regions; run again at will
# the binaries and the machine start script go in each bucket's bin/ (any Linux host with Go builds them):
cd pull/cache && for a in amd64 arm64; do CGO_ENABLED=0 GOOS=linux GOARCH=$a go build -trimpath -ldflags='-s -w' -o pullcache-linux-$a .; done
for r in us-east-1 us-east-2; do for f in pullcache-linux-amd64 pullcache-linux-arm64; do
  aws --profile ci-runners s3 cp $f s3://superci-pull-cache-243891825325-$r/bin/$f --region $r; done
  aws --profile ci-runners s3 cp deploy/machine-up.sh s3://superci-pull-cache-243891825325-$r/bin/up.sh --region $r; done
```

`deploy/down.sh` removes the endpoints, roles and replication and keeps the buckets; `-buckets` removes those too.
Everything made is tagged `purpose=superci-pull-cache`.

### The seam with the launcher

The machines need three things from the launcher, and the first is already in `boot.sh` here.

1. **`pull/boot.sh`, lines 16 and 17 (a comment and one command, after the `ID=` line).** The command reads the
   machine's region, takes the bucket from `PULLCACHE_BUCKET` (or, when that is not set, the name
   `superci-pull-cache-243891825325-<region>`), fetches `bin/up.sh` from it and runs it. `up.sh` fetches the program,
   starts it under systemd, waits for it, and wraps the runner's Node. It is a no-op without the role. It takes about
   2 to 4 seconds of a machine's boot.
2. **An instance profile on the machines**, `superci-pull-cache-machine`. The machines have none today. The change is
   one line in `fleet.go` (`IamInstanceProfile` in the launch template) and one statement in
   `deploy/listener-policy.json` (`iam:PassRole` on the role), in `seam/instance-profile.patch` (`git apply` from the
   repository root; it applies to `pull-arm`'s head). Not applied here, so this branch does not touch `fleet.go`.
3. **Nothing else.** The S3 gateway endpoints are in the default VPCs, where the runners' security group is.

Settings the daemon reads (all optional on a machine, all set by `up.sh`): `PULLCACHE_BUCKET`, `PULLCACHE_REGION`,
`PULLCACHE_STAGE_BUCKET` (default `<bucket>-stage`), `PULLCACHE_DEFAULT_BRANCH` (`main`), `PULLCACHE_EXTERNALS`,
`PULLCACHE_LISTEN`, `PULLCACHE_PORT` (8257), `PULLCACHE_UPSTREAM`, `PULLCACHE_JWKS_URL`, `PULLCACHE_ISSUER`.

### Operate

```bash
sudo journalctl -u pullcache -o cat -f       # on a machine: restore "k": found ...; save "k": saved N MB; blocks in ...
pullcache janitor -bucket B -region R -cap-gb 300 [-dry-run]
```

## What was not tested

- A real GitHub job token. The proof signs its own tokens, with the claim names superci's plane reads from GitHub's
  (`iss`, `repository_id`, `ac`). The first job on a pull machine will show whether GitHub's token has them as
  expected; a wrong guess is a miss, not a failed job. Watch `journalctl -u pullcache` there once.
- A job in a container (`container:`). The wrapped Node is reached through the runner's externals at `/__e` and the
  daemon listens on the private address a container reaches, but no container job was run.
- arm64 machines. The binary is built and published, and not run.
- The listener launching machines with the profile. The launch template's shape was checked with a dry run, the
  listener's `iam:PassRole` was not exercised.
- `docker/build-push-action` with `type=gha`, and `useblacksmith/setup-docker-builder` (Blacksmith's own, which has
  to be replaced separately).

## Proof

`proof/run.sh scope|big` is what produced the numbers (`proof/mint` signs the tokens). It needs a machine of the
fleet's image with the machines' role and a checkout of `actions/cache`.
