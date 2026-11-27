#!/usr/bin/env python3
"""apptoken: the token the gate, the timeline and the canary read GitHub with.

They read on the superci GitHub App's installation budget (5,000 an hour of its own), never on the personal token every
session on the operator's host shares. The App's private key stays on the listener: this asks the listener's admin
endpoint (`POST /github/token` on port 8080, reached through deploy/ssh.sh) for a short-lived read-only installation
token, and keeps it in this process's memory only: a `gh` call gets it as GH_TOKEN in its own environment, nothing
writes or prints it.

  apptoken.py            print the token on stdout, for `GH_TOKEN=$(apptoken.py) gh ...` (canary.sh); exit 2 with the reason when there is none

A caller that cannot get a token refuses (Auth.require); it never falls back to the personal token unless the operator
said so (`--personal-token`, SUPERCI_PERSONAL_TOKEN=1), which is announced. The only other personal-token calls are the
named EXCEPTIONS, each of a permission the App is not documented to hold; pull/README.md lists them and what the operator
must grant to end each one.

SUPERCI_APP_TOKEN_CMD replaces the ssh call (tests): a command whose stdout is the listener's answer, then a line with the
HTTP status.
"""
import datetime as dt
import json
import os
import shlex
import subprocess
import sys
import threading
import time

# The calls that stay on the personal token: name -> (permission the App lacks, what the call is).
EXCEPTIONS = {
    "ci-limits.tsv": ("Contents: read", "reads scripts/verify/ci-limits.tsv from main (gate --limits FILE makes no call)"),
    "SUPERCI_JOBS": ("Variables: read and write", "reads, and on a rollback edits or deletes, the repository variable SUPERCI_JOBS"),
}
RESERVE_S = 600  # a token must live this much longer to be used: a tool holds it for a whole command
REFUSED = 2
_HERE = os.path.dirname(os.path.abspath(__file__))
_ASK = "curl -s -m 20 -X POST -w '\\n%{http_code}' localhost:8080/github/token"


class NoAppToken(Exception):
    pass


def _command():
    override = os.environ.get("SUPERCI_APP_TOKEN_CMD")
    return shlex.split(override) if override else [os.path.join(_HERE, "..", "..", "deploy", "ssh.sh"), _ASK]


def _expiry(text):
    return dt.datetime.strptime(text, "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=dt.timezone.utc).timestamp()


def mint():
    """(token, expires as epoch seconds) from the listener, or NoAppToken with why. The message never holds a token."""
    try:
        p = subprocess.run(_command(), capture_output=True, text=True, timeout=90)
    except (OSError, subprocess.SubprocessError) as e:
        raise NoAppToken(f"could not reach the listener: {e}") from None
    body, _, status = p.stdout.rstrip("\n").rpartition("\n")
    if p.returncode or status.strip() != "200":
        raise NoAppToken(f"the listener's /github/token did not answer 200 (exit {p.returncode}, status {status.strip()[:20] or 'none'}): "
                         f"{(body.strip() or p.stderr.strip())[:300]}")
    try:
        got = json.loads(body)
        token, expires = got["token"], _expiry(got["expires"])
    except (ValueError, KeyError, TypeError):
        raise NoAppToken("the listener's /github/token answered something that is not a token") from None
    if not token:
        raise NoAppToken("the listener's /github/token answered an empty token")
    return token, expires


def refusal(why):
    return (f"refusing: no GitHub App token ({why}).\n"
            "The gate reads GitHub on the superci App's budget, not on the personal token every session on this host shares.\n"
            "Make the listener's admin endpoint answer (deploy/ssh.sh, `curl -X POST localhost:8080/github/token`), or pass --personal-token "
            "(SUPERCI_PERSONAL_TOKEN=1 for canary.sh) to spend the shared personal budget knowingly.")


class Auth:
    """Where a `gh` call gets its credential. App by default; personal only when asked, or for a named exception."""

    def __init__(self):
        self.personal, self._token, self._expires, self._lock = False, None, 0.0, threading.Lock()

    def token(self):
        with self._lock:
            if self._token is None or self._expires - time.time() < RESERVE_S:
                self._token, self._expires = mint()
            return self._token

    def require(self):
        """Settles the credential before any call: the App's token, or the announced override. Exits REFUSED without either."""
        if self.personal:
            print("spending the shared personal GitHub token's budget (--personal-token), not the App's", file=sys.stderr)
            return
        try:
            self.token()
        except NoAppToken as e:
            print(refusal(e), file=sys.stderr)
            raise SystemExit(REFUSED) from None

    def env(self, exception=None):
        """The environment of a `gh` call, or None to leave the caller's (the personal credential)."""
        if exception is not None:
            if exception not in EXCEPTIONS:
                raise ValueError(f"{exception} is not a named personal-token exception")
            permission, what = EXCEPTIONS[exception]
            print(f"personal-token exception {exception}: the App lacks {permission}; this call {what} on the shared personal budget", file=sys.stderr)
            return None
        if self.personal:
            return None
        try:
            return {**os.environ, "GH_TOKEN": self.token()}
        except NoAppToken as e:
            print(refusal(e), file=sys.stderr)
            raise SystemExit(REFUSED) from None

    def gh(self, args, exception=None):
        env = self.env(exception)
        return subprocess.run(["gh", *args], capture_output=True, text=True, **({"env": env} if env is not None else {}))


AUTH = Auth()


if __name__ == "__main__":
    if sys.stdout.isatty():
        print("refusing: the token would print on the terminal; capture it: GH_TOKEN=$(apptoken.py) gh ...", file=sys.stderr)
        sys.exit(REFUSED)
    try:
        sys.stdout.write(mint()[0])
    except NoAppToken as e:
        print(refusal(e), file=sys.stderr)
        sys.exit(REFUSED)
