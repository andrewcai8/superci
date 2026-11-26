#!/bin/bash
# The runner's config.sh as the fake GitHub takes it: records how it was called, and registers the name.
echo "args=[$*] token_in_env=$([ -n "${ACTIONS_RUNNER_INPUT_TOKEN:-}" ] && echo yes || echo no) cwd=$PWD" >> "$FAKE/config.log"
name=""; labels=""
while [ $# -gt 0 ]; do case $1 in --name) name=$2; shift ;; --labels) labels=$2; shift ;; esac; shift; done
[ "$name" != "${FAKE_CONFIG_FAIL:-}" ] || { echo "Http response code: TooManyRequests (429) ConfigureTooManyRunners"; exit 1; }
echo "{\"agentName\":\"$name\"}" > .runner; echo '{"data":"x"}' > .credentials; echo '{"d":"x"}' > .credentials_rsaparams
python3 -I -c 'import json,os,sys
p = os.path.join(os.environ["FAKE"], "github.json")
r = json.load(open(p)) if os.path.exists(p) else []
r.append({"id": 1000 + len(r), "name": sys.argv[1], "status": "online", "busy": False, "labels": ["self-hosted", sys.argv[2]]})
json.dump(r, open(p, "w"))' "$name" "$labels"
