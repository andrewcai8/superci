#!/bin/bash
# Checks, reading only, that every launch template of superci-pull names an image that exists, so a machine that is
# lost can be replaced (Nov 28: both identity hosts' templates named a RunsOn image RunsOn had deregistered, and
# run-instances answered InvalidAMIID.NotFound). For each region of config.json, every template tagged
# purpose=superci-pull or named superci-pull-*:
#   superci-pull-identity-<az>   $Default's image is config.json's pin for the region: it exists, is ours and is available
#   <deployment>-<size>          (the config's deployment, superci-pull; each size of config.json) $Default and $Latest, the version the listener launches, name
#                                the pin, and it is available (the listener makes $Latest the default: fleet.go ensureTemplate)
#   superci-pull-listener        the version its Auto Scaling group launches names an image that exists and is available
#                                (Amazon's AL2023, not the pin); its DeprecationTime is printed as a note
#   another deployment's         (tagged superci-pull-deployment=<other>: a dev listener's) is noted, not checked
#   any other                    is retired: fails, with the delete-launch-template line to run by hand. Never deleted here.
# Prints a line per check and exits 1 when any fails.
#
#   pull/deploy/templates.sh check [--pre-upload] [--commit <rev>]
#
# --pre-upload checks the identity and listener templates only: a dispatch template names the pin once the listener this
# release starts has made its next version, so checking them before the upload would block the release that fixes them
# (release.sh; verify-release.sh checks all after the restart). --commit reads the pin and the sizes from the commit's
# pull/config.json (this checkout's when it has none), as parity.sh does.
set -uo pipefail
[ "${1:-}" = check ] || { echo "usage: templates.sh check [--pre-upload] [--commit <rev>]" >&2; exit 2; }
shift
SCOPE=all REV=
while [ $# -gt 0 ]; do
  case "$1" in
    --pre-upload) SCOPE=pre ;;
    --commit) REV=${2:?commit}; shift ;;
    *) echo "usage: templates.sh check [--pre-upload] [--commit <rev>]" >&2; exit 2 ;;
  esac
  shift
done
A="aws --profile ci-runners"
OUR=243891825325
[ "$($A sts get-caller-identity --query Account --output text)" = $OUR ] || { echo "refusing: not ci-runners" >&2; exit 1; }
here=$(cd "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
cp "$here/config.json" "$tmp/config.json"
if [ -n "$REV" ]; then
  git -C "$here/.." show "$REV:pull/config.json" > "$tmp/rev-config.json" 2> /dev/null && python3 -I -c 'import json,sys; sys.exit(0 if "image" in json.load(open(sys.argv[1])) else 1)' "$tmp/rev-config.json" && cp "$tmp/rev-config.json" "$tmp/config.json"
fi
exec python3 -I - "$tmp/config.json" "$SCOPE" "$OUR" << 'PY'
import json, re, subprocess, sys

config, scope, our = json.load(open(sys.argv[1])), sys.argv[2], sys.argv[3]
A = ["aws", "--profile", "ci-runners"]
bad = 0


def ok(msg):
    print("ok    " + msg)


def note(msg):
    print("note  " + msg)


def fail(msg):
    global bad
    bad = 1
    print("FAIL  " + msg)


def aws(*args):
    """The call's JSON, or None when it fails (the CLI's own message goes to stderr, so a failed call is not silent)."""
    p = subprocess.run(A + list(args) + ["--output", "json"], stdout=subprocess.PIPE, stdin=subprocess.DEVNULL, text=True)
    if p.returncode != 0:
        return None
    return json.loads(p.stdout) if p.stdout.strip() else {}


images = {}


def image(region, ami):
    """(owner, state, deprecation time) of an image, or None when EC2 does not list it."""
    if (region, ami) not in images:
        r = aws("ec2", "describe-images", "--region", region, "--image-ids", ami) if re.fullmatch(r"ami-[0-9a-f]{8}([0-9a-f]{9})?", ami or "") else None
        i = (r or {}).get("Images") or []
        images[(region, ami)] = (i[0].get("OwnerId"), i[0].get("State"), i[0].get("DeprecationTime")) if i else None
    return images[(region, ami)]


def versions(region, t, numbers):
    """{version number: image id} of a template's versions."""
    r = aws("ec2", "describe-launch-template-versions", "--region", region, "--launch-template-id", t["LaunchTemplateId"],
            "--versions", *sorted({str(n) for n in numbers}))
    if r is None:
        return None
    return {v["VersionNumber"]: (v.get("LaunchTemplateData") or {}).get("ImageId") for v in r.get("LaunchTemplateVersions", [])}


def tag(t, key):
    return next((x["Value"] for x in t.get("Tags", []) if x["Key"] == key), None)


def pinned(region, name, v, which, ami, pin):
    """One version that must name the pin: it is, and the pin exists, is ours and available."""
    where = "%s %s v%s (%s)" % (region, name, v, which)
    if ami != pin:
        fail("%s: names %s, not %s, config.json's pin for %s" % (where, ami, pin, region))
        return
    img = image(region, ami)
    if img is None:
        fail("%s: image %s does not exist (EC2 does not list it)" % (where, ami))
    elif img[0] != our:
        fail("%s: image %s is owned by %s, not %s" % (where, ami, img[0], our))
    elif img[1] != "available":
        fail("%s: image %s is %s, not available" % (where, ami, img[1]))
    else:
        ok("%s: image %s" % (where, ami))


def listener(region, t):
    g = aws("autoscaling", "describe-auto-scaling-groups", "--region", region, "--auto-scaling-group-names", "superci-pull-listener")
    groups = (g or {}).get("AutoScalingGroups") or []
    if g is None:
        fail("%s superci-pull-listener: its Auto Scaling group could not be read" % region)
        return
    if not groups:
        note("%s superci-pull-listener: no Auto Scaling group yet (deploy/up.sh makes it once a release is in place)" % region)
        return
    lt = groups[0].get("LaunchTemplate") or (groups[0].get("MixedInstancesPolicy") or {}).get("LaunchTemplate", {}).get("LaunchTemplateSpecification") or {}
    if lt.get("LaunchTemplateId") != t["LaunchTemplateId"]:
        fail("%s superci-pull-listener: its group launches template %s, not %s" % (region, lt.get("LaunchTemplateId"), t["LaunchTemplateId"]))
        return
    want = lt.get("Version", "$Default")
    n = {"$Default": t["DefaultVersionNumber"], "$Latest": t["LatestVersionNumber"]}.get(want)
    if n is None:
        n = int(want)
    where = "%s superci-pull-listener v%s (its group's %s)" % (region, n, want)
    vs = versions(region, t, [n])
    if vs is None or n not in vs:
        fail("%s: the version could not be read" % where)
        return
    ami = vs[n]
    img = image(region, ami)
    if img is None:
        fail("%s: image %s does not exist (EC2 does not list it)" % (where, ami))
    elif img[1] != "available":
        fail("%s: image %s is %s, not available" % (where, ami, img[1]))
    else:
        ok("%s: image %s" % (where, ami))
        note("%s: image %s DeprecationTime %s" % (where, ami, img[2] or "none"))


sizes, deployment = sorted(config["sizes"]), config["deployment"]
for region in sorted(config["regions"]):
    pin = (config.get("image") or {}).get(region)
    found = {}
    for flt in ("Name=tag:purpose,Values=superci-pull", "Name=launch-template-name,Values=superci-pull-*"):
        r = aws("ec2", "describe-launch-templates", "--region", region, "--filters", flt)
        if r is None:
            fail("%s: launch templates could not be listed (%s)" % (region, flt))
            continue
        for t in r.get("LaunchTemplates", []):
            # (superci's own plane, superci-<12 characters>-..., is another live system's, whatever its tags: not ours to retire.)
            if re.match(r"superci-[0-9a-z]{12}-", t["LaunchTemplateName"]):
                continue
            if t["LaunchTemplateName"].startswith("superci-pull-") or tag(t, "purpose") == "superci-pull":
                found[t["LaunchTemplateId"]] = t
    for t in sorted(found.values(), key=lambda t: t["LaunchTemplateName"]):
        name = t["LaunchTemplateName"]
        if tag(t, "superci-pull-deployment") not in (None, deployment):
            note("%s %s: a template of deployment %s, not %s; not checked" % (region, name, tag(t, "superci-pull-deployment"), deployment))
            continue
        d, l = t["DefaultVersionNumber"], t["LatestVersionNumber"]
        if re.fullmatch(r"superci-pull-identity-[a-z0-9-]+", name):
            kind = "identity"
        elif name == "superci-pull-listener":
            kind = "listener"
        elif name.startswith(deployment + "-") and name[len(deployment) + 1:] in sizes:
            kind = "dispatch"
        else:
            kind = "retired"
        if kind == "retired":
            if scope == "all":
                fail("%s %s (%s, default v%s, latest v%s) is retired: no size of config.json, no identity host and no listener names it; delete it by hand: %s ec2 delete-launch-template --region %s --launch-template-id %s"
                     % (region, name, t["LaunchTemplateId"], d, l, " ".join(A), region, t["LaunchTemplateId"]))
            continue
        if kind == "dispatch" and scope != "all":
            continue
        if kind == "listener":
            listener(region, t)
            continue
        vs = versions(region, t, [d, l] if kind == "dispatch" else [d])
        if vs is None or d not in vs or (kind == "dispatch" and l not in vs):
            fail("%s %s: its versions could not be read" % (region, name))
            continue
        pinned(region, name, d, "$Default", vs[d], pin)
        if kind == "dispatch":
            pinned(region, name, l, "$Latest", vs[l], pin)
    if scope == "all":
        names = {t["LaunchTemplateName"] for t in found.values()}
        for s in sizes:
            if deployment + "-" + s not in names:
                note("%s %s-%s: no template yet (the listener makes it at its start)" % (region, deployment, s))
print("launch templates ok: %s" % scope if not bad else "launch templates NOT ok: %s" % scope)
sys.exit(bad)
PY
