#!/bin/bash
# Fetches the override config; replaces the listener's copy only when it changed and the listener would accept it, so
# its mtime marks a change and a broken upload never reaches it.
set -euo pipefail
. /etc/superci-pull/bucket
tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT
if aws s3 cp --quiet "s3://$BUCKET/config/config.json" "$tmp" 2> /dev/null && ! cmp -s "$tmp" /etc/superci-pull/config.json; then
  if /usr/local/bin/superci-pull -validate -config "$tmp" > /dev/null; then
    install -m 644 "$tmp" /etc/superci-pull/config.json
  else
    echo "superci-pull-sync: s3://$BUCKET/config/config.json is invalid; the listener keeps the config it runs" >&2
  fi
fi
