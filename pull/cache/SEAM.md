# Porting the cache seam into `pull-prod`

The pull machines need three changes in the launcher before they use this cache. None is applied on this branch,
because `pull-prod` is in flight. The line numbers below are those of `pull-prod` at `4c7b276` ("pull/README: zones,
waiting machines, re-runs and roll back as built"). If they have moved, find the anchor quoted with each step.

Deploy the cache first (`deploy/up.sh`, then the binaries and the function code as the README's Deploy section says).
A machine that boots before that runs its jobs on GitHub's own cache, so the order only decides when hits start.

## 1. `pull/boot.sh`: start the daemon after `T=`

Anchor: line 16, `T=$(curl -s -m 2 -X PUT -H 'X-aws-ec2-metadata-token-ttl-seconds: 21600' http://169.254.169.254/latest/api/token)`.
Insert these two lines directly after it, before `DOC=` (line 17):

```bash
# The actions cache (pull/cache): starts its daemon from the region's cache bucket and wraps the runner's Node; a machine without the role runs on GitHub's own cache.
CR=$(curl -s -m 2 -H "X-aws-ec2-metadata-token: $T" http://169.254.169.254/latest/meta-data/placement/region); PB=superci-pull-cache-243891825325-$CR; aws s3 cp --only-show-errors --region "$CR" "s3://$PB/bin/up.sh" - 2> /dev/null | PULLCACHE_BUCKET=$PB bash || :
```

It has to run before the `while :` loop that starts `run.sh`: the wrap replaces `externals/node*/bin/node`, and a job
that starts first runs unwrapped. It needs `T` (IMDSv2 is required) and nothing after it. It takes about 2 to 4
seconds of the boot, and `|| :` keeps a failure from stopping the boot.

The old seam's anchor, the `ID=` line, no longer exists: `pull-prod` replaced it with `DOC=`, which is why the old
hunk no longer applies.

## 2. `pull/fleet.go`: the instance profile in `setRegion.ensureTemplate`, and in `sameData`

Anchor: line 135, `SecurityGroupIds:                  []string{group},` in the `RequestLaunchTemplateData`
literal of `func (sr *setRegion) ensureTemplate` (line 131). Add after it:

```go
		IamInstanceProfile:                &ec2types.LaunchTemplateIamInstanceProfileSpecificationRequest{Name: aws.String("superci-pull-cache-machine")},
```

`ensureTemplate` makes a new template version only when `sameData` (line 171) says the latest one differs. That
function compares the image, user data, security groups, disk and metadata options. Without a profile check there, a
template that already exists keeps its version without the profile, and no machine gets the role. Add a clause to
`sameData`'s return (line 176), for example after the `slices.Equal(have.SecurityGroupIds, ...)` term:

```go
		have.IamInstanceProfile != nil && strings.HasSuffix(aws.ToString(have.IamInstanceProfile.Arn), "/superci-pull-cache-machine") &&
```

A version written with a profile `Name` is described back with the profile's `Arn` (and sometimes the `Name`), so
the check matches by the ARN's last part. Import `strings` if `fleet.go` does not yet.

Keep `HttpPutResponseHopLimit` at `1`. The daemon runs on the host, and a job's containers reach it at the machine's
private address, not through IMDS. With a hop limit of 1, a step in a container cannot get the role's credentials at
all. A step on the host can, but the role only reads the cache and calls the broker, which wants a valid job token.

## 3. `pull/deploy/listener-policy.json`: let the listener pass the role

`CreateFleet` with a template that names an instance profile needs `iam:PassRole` on its role. Without it every
launch fails. Add this statement, for example after `"Sid": "Read"` (line 58):

```json
    {
      "Sid": "PassCacheRole",
      "Effect": "Allow",
      "Action": "iam:PassRole",
      "Resource": "arn:aws:iam::243891825325:role/superci-pull-cache-machine",
      "Condition": { "StringEquals": { "iam:PassedToService": "ec2.amazonaws.com" } }
    },
```

Apply the policy before the listener restarts with the new `fleet.go`: the new template version is made at start.

## Not needed

- `cache_env` (writing `ACTIONS_RESULTS_URL` into `/home/runner/.env`) cannot take the wrap's place. The runner sets
  `ACTIONS_RESULTS_URL` for each step from the job message, after `.env`, so a value there never reaches an action.
  `pull-prod` has already removed `cache_env`. Do not bring it back for this.
- Nothing in `config.json`, the security groups or the subnets: the S3 gateway endpoints are in the default VPCs,
  where the runners are.

## Check

On one machine after the port:

```bash
sudo journalctl -u pullcache -o cat     # "serving GitHub's cache at http://<private ip>:8257 ..."
ls -la /home/runner/externals/node24/bin/node /home/runner/externals/node24/bin/node.real
```

The first job that uses `actions/cache` shows whether GitHub's runtime token is taken. Look for `restore "..."` and
`save "...": saved` lines. A refusal reads `refused: ... (iss "...", kid "...")`. If its `iss` is not
`https://token.actions.githubusercontent.com`, set `PULLCACHE_ISSUER` on the machines and the broker to the issuer it
shows. If the `kid` is not in GitHub's JWKS, the runtime token is signed by other keys: set `PULLCACHE_JWKS_URL` on
both. Until then every job is a cache miss, never a failed job.
