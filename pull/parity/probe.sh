#!/bin/bash
# The job environment as a job sees it, one fact a line, for pull/parity/compare.py to hold against a runner's manifest
# (pull/parity/blacksmith-ubuntu-2404.txt). Run it as a job step, or as the job's own process would run (agent.py --probe):
#
#   umask 0022                       the file mode mask
#   user runner                      the user, its uid and gid, its groups and HOME
#   sudo <rule>                      each of `sudo -n -l`'s rules
#   ulimit <flag> <value>            ulimit -a, by its flag
#   env <NAME>                       the name of each environment variable (never its value)
#   cmd <name>                       each command on PATH (compgen -c)
#   pkg <package> <version>          each installed Debian package
#   tool <name> <version> <path>     the key tools, `-` where one is missing
#
# It prints no value of any variable and reads nothing a job could not.
set -u
LC_ALL=C
export LC_ALL
echo "umask $(umask)"
echo "user $(id -un)"
echo "uid $(id -u)"
echo "gid $(id -g) $(id -gn)"
echo "groups $(id -Gn | tr ' ' '\n' | sort | paste -sd, -)"
echo "home ${HOME:-}"
sudo -n -l 2> /dev/null | sed -n 's/^    //p' | sed 's/^/sudo /' | sort -u
ulimit -a | sed -n 's/.*(\(-[a-zA-Z]\)) *\(.*\)$/ulimit \1 \2/p; s/.*(\([a-z]*\), *\(-[a-zA-Z]\)) *\(.*\)$/ulimit \2 \3/p' | sort -u
compgen -e | sort -u | sed 's/^/env /'
compgen -c | grep -v '[[:space:]]' | sort -u | sed 's/^/cmd /'
dpkg-query -W -f '${Package} ${Version}\n' 2> /dev/null | sort -u | sed 's/^/pkg /'
# version <command> <args>: the first version-looking word the command prints.
version() {
  local out
  out=$(timeout 10 "$@" 2>&1 < /dev/null | head -5) || :
  printf '%s\n' "$out" | grep -o '[0-9][0-9]*\.[0-9][0-9]*\(\.[0-9][0-9]*\)\?' | head -1
}
tool() {
  local name=$1 path v
  shift
  path=$(command -v "$name" 2> /dev/null) || { echo "tool $name - -"; return; }
  v=$(version "$path" "$@")
  echo "tool $name ${v:--} $path"
}
tool dpkg-deb --version
tool psql --version
for d in /usr/lib/postgresql/*/bin; do
  [ -x "$d/pg_ctl" ] && echo "tool pg_ctl@${d#/usr/lib/postgresql/} $(version "$d/pg_ctl" --version) $d/pg_ctl"
done
tool pg_ctl --version
tool initdb --version
tool docker --version
tool node --version
tool bun --version
tool go version
tool python3 --version
tool git --version
tool gcc --version
tool make --version
tool jq --version
tool gh --version
tool aws --version
tool tar --version
tool zstd --version
tool redis-server --version
tool sudo --version
exit 0
