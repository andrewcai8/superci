#!/usr/bin/env python3
"""apptoken: the token the gate, the timeline, the logs tool and the canary read and edit GitHub with.

They use the superci GitHub App's installation budget (5,000 an hour of its own), never the personal token every session on
the operator's host shares. The App's private key stays on the listener: this asks the listener's admin endpoint
(`POST /github/token?scope=S` on port 8080, reached through deploy/ssh.sh) for a short-lived installation token of one scope,
and keeps it in this process's memory only: a `gh` call gets it as GH_TOKEN in its own environment, nothing writes or prints it.

  actions    actions: read on the configured repositories: runs, jobs, logs (the default)
  variables  actions_variables: write on megpt-mono: reads and writes the SUPERCI_JOBS variable (canary.sh)
  contents   contents: read on megpt-mono: scripts/verify/ci-limits.tsv (gate.py)

  apptoken.py [SCOPE]    print the token on stdout, for `GH_TOKEN=$(apptoken.py variables) gh ...` (canary.sh); exit 2 with the reason when
                         there is none, 3 when the installation lacks the scope's permission (asking again will not help)

A caller that cannot get a token refuses (Auth.require); it never falls back to the personal token unless the operator
said so (`--personal-token`, SUPERCI_PERSONAL_TOKEN=1), which is announced. The one other personal-token call is the named
EXCEPTION: canary.sh's write of SUPERCI_JOBS in a rollback or a re-route when the variables token is refused and the operator
set SUPERCI_PERSONAL_TOKEN=1; pull/README.md lists it.

SUPERCI_APP_TOKEN_CMD replaces the ssh call, and is honoured only when SUPERCI_TEST=1 is also set (the tests set both): a
command whose stdout is the listener's answer, then a line with the HTTP status, and which finds the scope asked for in
SUPERCI_TOKEN_SCOPE. Without the flag the variable is ignored, so an environment that happens to carry it cannot redirect
where the tools take their tokens from. SUPERCI_TOKEN_TIMEOUT (seconds, default 90) bounds one ask.
"""
import datetime as dt
import json
import os
import re
import shlex
import subprocess
import sys
import threading
import time

# The scopes of the listener's /github/token: name -> (GitHub's permission key, the App permission as its settings page names it).
SCOPES = {
    "actions": ("actions: read", "Actions: read"),
    "variables": ("actions_variables: write", "Variables: read and write"),
    "contents": ("contents: read", "Contents: read"),
}
# The call that stays on the personal token: name -> (why the App's token is not used, what the call is). It is made by canary.sh
# only with SUPERCI_PERSONAL_TOKEN=1, so a rollback is never stuck on the endpoint.
EXCEPTIONS = {
    "SUPERCI_JOBS write": ("the variables token was refused", "writes (sets or deletes) the repository variable SUPERCI_JOBS in a rollback or a re-route"),
}
RESERVE_S = 600  # a token must live this much longer to be used: a tool holds it for a whole command
REFUSED = 2
PERMANENT = 3  # the CLI's exit when the installation lacks the permission, which asking again will not change
_HERE = os.path.dirname(os.path.abspath(__file__))
_ASK = "curl -s -m 20 -X POST -w '\\n%{http_code}' 'localhost:8080/github/token?scope=@SCOPE@'"


class NoAppToken(Exception):
    permanent = False  # the installation lacks the permission: asking again will not help


class AppTokenRefused(Exception):
    """No App token and no override. Deliberately not a SystemExit: a tool catches SystemExit to skip one run GitHub would not
    list, and a refusal in the middle of a command must stop the command, not shrink its sample. Each tool's main turns it into exit 2."""


_TOKEN = re.compile(r"gh[pousr]_[A-Za-z0-9_]+|github_pat_[A-Za-z0-9_]+")


def _clean(text):
    """text with anything shaped like a GitHub token removed: a message about a token never holds one."""
    return _TOKEN.sub("<token>", text)


def _command(scope="actions"):
    override = os.environ.get("SUPERCI_APP_TOKEN_CMD") if os.environ.get("SUPERCI_TEST") == "1" else None
    return shlex.split(override) if override else [os.path.join(_HERE, "..", "..", "deploy", "ssh.sh"), _ASK.replace("@SCOPE@", scope)]


def _timeout():
    try:
        return max(1.0, float(os.environ.get("SUPERCI_TOKEN_TIMEOUT") or 90))
    except ValueError:
        return 90.0


def _expiry(text):
    return dt.datetime.strptime(text, "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=dt.timezone.utc).timestamp()


def mint(scope="actions"):
    """(token, expires as epoch seconds) of a scope from the listener, or NoAppToken with why. The message never holds a token."""
    if scope not in SCOPES:
        raise ValueError(f"{scope} is not a token scope: {', '.join(SCOPES)}")
    try:
        p = subprocess.run(_command(scope), capture_output=True, text=True, timeout=_timeout(), env={**os.environ, "SUPERCI_TOKEN_SCOPE": scope})
    except (OSError, subprocess.SubprocessError) as e:
        raise NoAppToken(_clean(f"could not reach the listener: {e}")) from None
    body, _, status = p.stdout.rstrip("\n").rpartition("\n")
    try:
        got = json.loads(body)
        token, expires = got["token"], _expiry(got["expires"])
    except (ValueError, KeyError, TypeError):
        got = token = expires = None
    status = status.strip()
    if p.returncode or status != "200":
        # What the listener said is quoted only for a refusal that holds no token: an answer that was a token (ssh failing after curl
        # printed it, say) is never repeated, and neither is any body that came with a 200.
        said = "" if token or status == "200" else _clean((body.strip() or p.stderr.strip())[:300])
        err = NoAppToken(f"the listener's /github/token did not answer 200 (exit {p.returncode}, status {status[:20] or 'none'})" + (f": {said}" if said else ""))
        err.permanent = status == "422" and not p.returncode
        raise err
    if token is None:
        raise NoAppToken("the listener's /github/token answered something that is not a token")
    if not token:
        raise NoAppToken("the listener's /github/token answered an empty token")
    return token, expires


def refusal(why, scope="actions"):
    named = "" if scope == "actions" else f" for {scope} ({SCOPES[scope][1]})"
    return _clean(f"refusing: no GitHub App token{named} ({why}).\n"
            "The gate reads GitHub on the superci App's budget, not on the personal token every session on this host shares.\n"
            f"Make the listener's admin endpoint answer (deploy/ssh.sh, `curl -X POST 'localhost:8080/github/token?scope={scope}'`), or pass --personal-token "
            "(SUPERCI_PERSONAL_TOKEN=1 for canary.sh) to spend the shared personal budget knowingly.")


class Auth:
    """Where a `gh` call gets its credential. App by default; personal only when asked, or for a named exception."""

    def __init__(self):
        self.personal, self._tokens, self._lock = False, {}, threading.Lock()

    def token(self, scope="actions"):
        with self._lock:
            held = self._tokens.get(scope)
            if held is None or held[1] - time.time() < RESERVE_S:
                held = self._tokens[scope] = mint(scope)
            return held[0]

    def require(self):
        """Settles the credential before any call: the App's token, or the announced override. Raises AppTokenRefused without either."""
        if self.personal:
            print("spending the shared personal GitHub token's budget (--personal-token), not the App's", file=sys.stderr)
            return
        try:
            self.token()
        except NoAppToken as e:
            raise AppTokenRefused(refusal(e)) from None

    def env(self, exception=None, scope="actions"):
        """The environment of a `gh` call, or None to leave the caller's (the personal credential)."""
        if exception is not None:
            if exception not in EXCEPTIONS:
                raise ValueError(f"{exception} is not a named personal-token exception")
            why, what = EXCEPTIONS[exception]
            print(f"personal-token exception {exception}: {why}; this call {what} on the shared personal budget", file=sys.stderr)
            return None
        if self.personal:
            return None
        try:
            return {**os.environ, "GH_TOKEN": self.token(scope)}
        except NoAppToken as e:
            raise AppTokenRefused(refusal(e, scope)) from None

    def gh(self, args, exception=None, scope="actions"):
        env = self.env(exception, scope)
        return subprocess.run(["gh", *args], capture_output=True, text=True, **({"env": env} if env is not None else {}))


def refuse_as_exit(main):
    """main wrapped so an AppTokenRefused ends the command with the reason and exit 2, whichever call of it was the one refused."""
    def run(*args, **kw):
        try:
            return main(*args, **kw)
        except AppTokenRefused as e:
            print(e, file=sys.stderr)
            raise SystemExit(REFUSED) from None
    return run


AUTH = Auth()


if __name__ == "__main__":
    scope = sys.argv[1] if len(sys.argv) > 1 else "actions"
    if len(sys.argv) > 2 or scope not in SCOPES:
        print(f"usage: apptoken.py [{'|'.join(SCOPES)}]", file=sys.stderr)
        sys.exit(REFUSED)
    if sys.stdout.isatty():
        print("refusing: the token would print on the terminal; capture it: GH_TOKEN=$(apptoken.py) gh ...", file=sys.stderr)
        sys.exit(REFUSED)
    try:
        sys.stdout.write(mint(scope)[0])
    except NoAppToken as e:
        print(refusal(e, scope), file=sys.stderr)
        sys.exit(PERMANENT if e.permanent else REFUSED)
