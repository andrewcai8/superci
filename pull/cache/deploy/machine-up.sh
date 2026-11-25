#!/bin/bash
# Run by boot.sh on a machine of the pull fleet (fetched from the cache bucket's bin/up.sh): starts the cache server
# and puts it in front of the runner's Node. It does nothing unless every step works, so a machine that cannot reach
# the bucket (no role, no endpoint) runs its jobs against GitHub's own cache, as it would without this.
#
# Settings, all optional: PULLCACHE_BUCKET (passed by boot.sh), PULLCACHE_DEFAULT_BRANCH (main),
# PULLCACHE_EXTERNALS (/home/runner/externals).
set -u
B=${PULLCACHE_BUCKET:?}
EXT=${PULLCACHE_EXTERNALS:-/home/runner/externals}
[ -d "$EXT" ] || exit 0
T=$(curl -s -m 2 -X PUT -H 'X-aws-ec2-metadata-token-ttl-seconds: 600' http://169.254.169.254/latest/api/token)
R=$(curl -s -m 2 -H "X-aws-ec2-metadata-token: $T" http://169.254.169.254/latest/meta-data/placement/region)
case $(uname -m) in x86_64) A=amd64 ;; aarch64) A=arm64 ;; *) exit 0 ;; esac
aws s3 cp --only-show-errors --region "$R" "s3://$B/bin/pullcache-linux-$A" /usr/local/bin/pullcache.new || exit 0
chmod 755 /usr/local/bin/pullcache.new && mv -f /usr/local/bin/pullcache.new /usr/local/bin/pullcache || exit 0
rm -f "$EXT/pullcache.address"
systemd-run --quiet --unit=pullcache --property=Restart=always --property=RestartSec=1 \
  --setenv=PULLCACHE_BUCKET="$B" --setenv=PULLCACHE_REGION="$R" --setenv=PULLCACHE_EXTERNALS="$EXT" \
  --setenv=PULLCACHE_DEFAULT_BRANCH="${PULLCACHE_DEFAULT_BRANCH:-main}" --setenv=GOMEMLIMIT=2GiB /usr/local/bin/pullcache serve || exit 0
for _ in $(seq 100); do [ -s "$EXT/pullcache.address" ] && break; sleep 0.05; done
[ -s "$EXT/pullcache.address" ] && /usr/local/bin/pullcache wrap "$EXT"
exit 0
