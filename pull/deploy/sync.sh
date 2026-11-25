#!/bin/bash
# Fetches the override config; replaces the listener's copy only when it changed, so its mtime marks a change.
set -euo pipefail
. /etc/superci-pull/bucket
tmp=$(mktemp)
if aws s3 cp --quiet "s3://$BUCKET/config/config.json" "$tmp" 2> /dev/null; then
  cmp -s "$tmp" /etc/superci-pull/config.json || install -m 644 "$tmp" /etc/superci-pull/config.json
fi
rm -f "$tmp"
