#!/usr/bin/python3 -I
"""Holds a superci job's environment (pull/parity/probe.sh's lines) against a runner's manifest and prints each difference:

  python3 -I compare.py <manifest> <probe output>      exit 0 when they match, 1 with one line a difference

The manifest is a probe of Blacksmith's job, cut to what a job relies on:
  umask, user, uid, gid, home      the job's own line must be the manifest's
  groups                           the job must be in each group (it may be in more)
  sudo <rule>                      the job's `sudo -n -l` rules must be the manifest's, no more and no fewer
  ulimit <flag> <value>            the job's limit must be the value; `>=<n>` takes n or more, or unlimited
  tool <name> <version>            the job's tool must be there and its version start with the manifest's, to the
                                   precision written (`22` takes 22.x, `16.15` only 16.15.x)
  cmd <name>                       the command must be on the job's PATH
  allow <kind> <name> <reason>     a difference the manifest accepts, and why: a command a superci job does not have,
                                   a tool at another version or a limit; the name may be a shell pattern (`*-16`)
Blank lines and lines from # are notes. Every other probe line (packages, variable names) is evidence, not held."""
import fnmatch
import sys

EXACT = ("umask", "user", "uid", "gid", "home")


def facts(lines):
    """The probe's lines as {kind: {name: rest}}; a kind with no name (umask) is keyed by itself."""
    out = {}
    for line in lines:
        line = line.rstrip("\n")
        if not line.strip() or line.startswith("#"):
            continue
        kind, _, rest = line.partition(" ")
        if kind in EXACT or kind == "groups":
            out.setdefault(kind, {})[kind] = rest
        elif kind == "sudo":  # a rule is the whole line, and two rules may share their first word
            out.setdefault(kind, {})[rest] = ""
        else:
            name, _, value = rest.partition(" ")
            out.setdefault(kind, {})[name] = value
    return out


def differences(manifest_lines, probe_lines):
    want, got = facts(manifest_lines), facts(probe_lines)
    allow = set()
    for line in manifest_lines:
        parts = line.split()
        if len(parts) >= 4 and parts[0] == "allow":
            allow.add((parts[1], parts[2]))
        elif len(parts) and parts[0] == "allow":
            yield f"manifest: {line.strip()!r} gives no reason"

    if "umask" not in got or "cmd" not in got:
        yield "probe: it holds no umask or no command: not probe.sh's output"
        return

    def allowed(kind, name):
        return any(k == kind and fnmatch.fnmatchcase(name, pattern) for k, pattern in allow)

    for kind in EXACT:
        w, g = want.get(kind, {}).get(kind), got.get(kind, {}).get(kind)
        if w is not None and w != g:
            yield f"{kind}: {g if g is not None else 'missing'}, Blacksmith's job has {w}"
    if "groups" in want:
        missing = set(want["groups"]["groups"].split(",")) - set(got.get("groups", {}).get("groups", "").split(","))
        if missing:
            yield f"groups: not in {', '.join(sorted(missing))}, as Blacksmith's job is"
    want_sudo, got_sudo = set(want.get("sudo", {})), set(got.get("sudo", {}))
    for rule in sorted(want_sudo - got_sudo):
        yield f"sudo: the job lacks {rule!r}, which Blacksmith's job has"
    for rule in sorted(got_sudo - want_sudo):
        yield f"sudo: the job has {rule!r}, which Blacksmith's job does not"
    for flag, w in sorted(want.get("ulimit", {}).items()):
        if allowed("ulimit", flag):
            continue
        g = got.get("ulimit", {}).get(flag)
        if w.startswith(">="):
            floor = int(w[2:])
            if not (g == "unlimited" or (g is not None and g.isdigit() and int(g) >= floor)):
                yield f"ulimit {flag}: {g if g is not None else 'missing'}, Blacksmith's job has at least {floor}"
        elif g != w:
            yield f"ulimit {flag}: {g if g is not None else 'missing'}, Blacksmith's job has {w}"
    for name, version in sorted(want.get("tool", {}).items()):
        if allowed("tool", name):
            continue
        have = got.get("tool", {}).get(name, "- -").split(" ")[0]
        if have == "-":
            yield f"tool {name}: missing, Blacksmith's job has {version}"
        elif not (have == version or have.startswith(version + ".")):
            yield f"tool {name}: {have}, Blacksmith's job has {version}"
    for name in sorted(want.get("cmd", {})):
        if not allowed("cmd", name) and name not in got.get("cmd", {}):
            yield f"cmd {name}: not on the job's PATH, as it is on Blacksmith's"


def main(argv):
    if len(argv) != 3:
        print(__doc__.split("\n\n")[0], file=sys.stderr)
        return 2
    with open(argv[1]) as m, open(argv[2]) as p:
        diffs = list(differences(m.readlines(), p.readlines()))
    for d in diffs:
        print(d)
    print(f"parity: {len(diffs)} difference(s) from {argv[1]}" if diffs else f"parity: the job environment matches {argv[1]}")
    return 1 if diffs else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
