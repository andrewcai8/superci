#!/usr/bin/python3 -I
"""The machine agent of superci-pull's dispatch mode. boot.sh fetches it from the listener (POST /agent) and runs it as
root on a one-job machine: python3 -I agent.py <listener url> <pinned key>.

It asks the dispatcher for the machine's one job (POST /work, proved with the instance identity document), takes the
JOB frame (the identity's .runner, no credential) and the PIPE frames (the listener's job message), and runs the real
Runner.Worker as the runner user with those bytes on its pipe. The machine never holds the identity's credentials: the
only secret it sees is the job's own token, inside the job message the Worker needs anyway. It frames the Worker's
output back, renews the job itself so the job outlives the identity host and the dispatcher, and reports the exit code.
The wire format is D-SPEC.md's: frames are <u8 type><u32 LE length><payload>."""
# Annotations stay unevaluated: the release build imports this file in its tests on Amazon Linux 2023's python3 (3.9),
# where `str | None` raises at class creation; the machines run the image's 3.12.
from __future__ import annotations

import base64
import dataclasses
import hashlib
import json
import os
import pwd
import random
import socket
import ssl
import struct
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.parse
import urllib.request

STDOUT, STDERR, EXIT, INFO, PIPE, JOB, PING, PAIRED = 1, 2, 3, 4, 5, 6, 7, 8
MAX_PIPE = 1 << 20
MAX_JOB = 64 << 10
OUT_CHUNK = 64 << 10
OUT_BUDGET = 4 << 20
MAX_MESSAGE = 32 << 20  # the first PIPE message the agent reads for the job's renewal; the dispatcher reads no more
NEW_JOB_REQUEST = 1
# The file mode mask of the Worker and every step it starts: Blacksmith's jobs' (pull/parity/blacksmith-ubuntu-2404.txt),
# whatever mask the agent itself runs under. Under boot's old 077 a job's files were private to it: dpkg-deb refused a
# package's control directory (700) and tests that make a file 444 read it back 400 (Nov 28).
JOB_UMASK = 0o022


@dataclasses.dataclass
class Config:
    listener: str
    pin: str
    imds: str = "http://169.254.169.254"
    home: str = "/home/runner"
    user: str | None = "runner"  # None runs the Worker as the agent's own user (tests)
    spool: str = "/run/superci-marks"
    environment: str = "/etc/environment"
    ping_every: float = 20
    silence: float = 60
    window: float = 180  # how long a broken connection with a job is resumed
    wait_window: float = 900  # how long a machine without a job keeps asking
    retry: float = 1  # the longest the first reconnect waits; the ceiling doubles with each failure in a row
    retry_max: float = 15  # the longest any reconnect waits
    renew_every: float = 60
    exit_settle: float = 10  # an EXIT sent and not answered by a reset or a close in this long is taken as delivered
    hydrate_timeout: float = 10
    first_message_wait: float = 60  # how long after JOB the listener's job message may take to arrive whole
    renew_context: ssl.SSLContext | None = None  # the TLS settings of the renewal; None is the system's trust (tests)

    @property
    def runner_file(self):
        return os.path.join(self.home, ".runner")

    @property
    def worker(self):
        return os.path.join(self.home, "bin", "Runner.Worker")

    @property
    def hydrate_stop(self):
        return os.path.join(self.home, "superci-hydrate-stop.sh")


def log(msg):
    print("superci-agent: " + msg, flush=True)


def _tlv(buf, pos):
    """The header of the DER element at pos: (header length, content length)."""
    first = buf[pos + 1]
    if first < 0x80:
        return 2, first
    n = first & 0x7F
    return 2 + n, int.from_bytes(buf[pos + 2:pos + 2 + n], "big")


def spki(cert):
    """The SubjectPublicKeyInfo of a DER certificate, the bytes curl's --pinnedpubkey hashes."""
    h, _ = _tlv(cert, 0)  # Certificate
    pos = h
    h, _ = _tlv(cert, pos)  # tbsCertificate
    pos += h
    if cert[pos] == 0xA0:  # the optional version
        h, n = _tlv(cert, pos)
        pos += h + n
    for _ in range(5):  # serial, signature, issuer, validity, subject
        h, n = _tlv(cert, pos)
        pos += h + n
    h, n = _tlv(cert, pos)
    return cert[pos:pos + h + n]


def pin_matches(cert, pin):
    got = base64.b64encode(hashlib.sha256(spki(cert)).digest()).decode()
    return any(p.strip() == "sha256//" + got for p in pin.split(";"))


class Refused(Exception):
    def __init__(self, status, retry_after=None):
        super().__init__(f"the dispatcher answered {status}")
        self.status = status
        self.retry_after = retry_after  # seconds the dispatcher asked to wait, or None


def retry_after_seconds(value):
    """The delay of a Retry-After header, which the dispatcher sends as whole seconds; None for anything else."""
    value = value.strip()
    return int(value) if value.isascii() and value.isdigit() else None


def backoff(attempt, base, cap, draw):
    """The wait before reconnect number attempt (0 is the first after a connection that worked): a uniform draw from
    [0, min(cap, base * 2**attempt)], so a fleet that lost its dispatcher together does not come back together."""
    return draw * min(cap, base * 2 ** min(attempt, 32))


def http_get(opener, url, headers=None, method="GET", timeout=5):
    req = urllib.request.Request(url, headers=headers or {}, method=method)
    with opener.open(req, timeout=timeout) as r:
        return r.read(1 << 16).decode()


NO_PROXY = urllib.request.build_opener(urllib.request.ProxyHandler({}))


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):
        return None  # the job's token goes to the run service it names and nowhere else


def imds(cfg, path):
    token = http_get(NO_PROXY, cfg.imds + "/latest/api/token", {"X-aws-ec2-metadata-token-ttl-seconds": "600"}, "PUT")
    return http_get(NO_PROXY, cfg.imds + path, {"X-aws-ec2-metadata-token": token})


def upgrade(cfg, doc, path, resume=None):
    """Opens a connection to the dispatcher, pinned to the listener's key, and upgrades it to frames. Returns the
    socket and its buffered reader."""
    u = urllib.parse.urlsplit(cfg.listener)
    ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
    ctx.minimum_version = ssl.TLSVersion.TLSv1_2
    ctx.check_hostname = False
    ctx.verify_mode = ssl.CERT_NONE  # the listener's certificate is self-issued: its key is what is pinned
    raw = socket.create_connection((u.hostname, u.port or 443), timeout=10)
    # A close without the TLS close_notify (a crashed dispatcher, a reset) must not read as the clean close that tells the
    # agent its EXIT was relayed.
    s = ctx.wrap_socket(raw, suppress_ragged_eofs=False)
    try:
        if not pin_matches(s.getpeercert(binary_form=True), cfg.pin):
            raise ssl.SSLError("the listener's key does not match its pin")
        head = (f"POST {path} HTTP/1.1\r\nHost: {u.netloc}\r\nConnection: Upgrade\r\nUpgrade: superci-dispatch/1\r\n"
                f"Content-Type: text/plain\r\nContent-Length: {len(doc)}\r\n")
        if resume:
            head += f"X-Superci-Resume: {resume}\r\n"
        s.sendall(head.encode() + b"\r\n" + doc.encode())
        rf = s.makefile("rb")
        status = rf.readline(8192).split()
        if len(status) < 2 or not status[1].isdigit():
            raise OSError("no HTTP answer")
        retry_after = None
        for _ in range(64):
            line = rf.readline(8192)
            if line in (b"\r\n", b"\n", b""):
                break
            name, _, value = line.decode("latin-1").partition(":")
            if name.strip().lower() == "retry-after":
                retry_after = retry_after_seconds(value)
        if int(status[1]) != 101:
            raise Refused(int(status[1]), retry_after)
    except BaseException:
        s.close()
        raise
    s.settimeout(cfg.silence)
    return s, rf


def read_exactly(rf, n):
    data = rf.read(n)
    if len(data) != n:
        raise EOFError
    return data


class FirstMessage:
    """Takes the job's id, plan and run-service endpoint from the listener's first pipe message (<i32 type><i32 length>
    <UTF-16LE JSON>), the only fields the job's renewal needs, and keeps nothing else of it."""

    def __init__(self):
        self.buf = bytearray()
        self.done = False
        self.renewal = None  # (renew url, job token, plan id, job id)

    def feed(self, data):
        if self.done:
            return
        self.buf += data
        if len(self.buf) < 8:
            return
        kind, n = struct.unpack("<ii", self.buf[:8])
        if kind != NEW_JOB_REQUEST or n <= 0 or n > MAX_MESSAGE:
            self.done = True
            self.buf = bytearray()
            return
        if len(self.buf) >= 8 + n:
            body = bytes(self.buf[8:8 + n])
            self.buf = bytearray()
            try:
                self.renewal = self.extract(json.loads(body.decode("utf-16-le")))
            except (ValueError, KeyError, TypeError, AttributeError):
                self.renewal = None
            self.done = True

    @staticmethod
    def extract(msg):
        def get(d, key):
            for k, v in d.items():
                if k.lower() == key.lower():
                    return v
            raise KeyError(key)
        plan = get(get(msg, "plan"), "planId")
        job = get(msg, "jobId")
        for ep in get(get(msg, "resources"), "endpoints"):
            if get(ep, "name").lower() == "systemvssconnection":
                token = get(get(get(ep, "authorization"), "parameters"), "AccessToken")
                return urllib.parse.urljoin(get(ep, "url"), "renewjob"), token, plan, job
        raise KeyError("SystemVssConnection")


class Agent:
    def __init__(self, cfg):
        self.cfg = cfg
        self.sock = None
        self.wlock = threading.Lock()
        self.instance = None
        self.token = None  # the claim token JOB brought, which with the instance id resumes the connection
        self.doc = None
        self.proc = None
        self.job_started = False
        self.w_in = None
        self.pipe_lock = threading.Lock()
        self.first = FirstMessage()
        self.out_sent = 0
        self.out_lock = threading.Lock()  # the stdout and stderr pumps share the budget
        self.last_mark = time.monotonic()
        self.exit_code = None
        self.exit_sent_on = None
        self.exit_sent_at = 0.0
        self.exit_lock = threading.Lock()
        self.no_message = False
        self.worker_done = threading.Event()
        self.finished = threading.Event()  # nothing more to ask of the dispatcher
        self.delivered = False
        self.job_taken = threading.Event()
        self.clock = time.monotonic  # the clock, wait and draw of the reconnect loop, replaced by the tests
        self.sleep = time.sleep
        self.jitter = random.random

    def mark(self, name, value=None):
        """A line of the spool boot.sh posts to the listener: name=epoch=ms since the previous mark[=value]."""
        now = time.time()
        mono = time.monotonic()
        line = f"{name}={int(now)}.{int((now % 1) * 1e6):06d}={int((mono - self.last_mark) * 1000)}"
        if value is not None:
            line += f"={value}"
        self.last_mark = mono
        try:
            fd = os.open(self.cfg.spool, os.O_WRONLY | os.O_APPEND)
        except OSError:
            return
        try:
            os.write(fd, (line + "\n").encode())
        finally:
            os.close(fd)

    def send(self, typ, payload=b""):
        with self.wlock:
            s = self.sock
            if s is None:
                return False
            try:
                s.sendall(struct.pack("<BI", typ, len(payload)) + payload)
                return True
            except OSError:
                self.sock = None
                try:
                    s.close()
                except OSError:
                    pass
                return False

    def env(self):
        env = {}
        try:
            with open(self.cfg.environment) as f:
                for line in f:
                    line = line.strip()
                    if "=" in line and not line.startswith("#"):
                        k, v = line.split("=", 1)
                        env[k] = v.strip('"')
        except OSError:
            pass
        user = self.cfg.user or pwd.getpwuid(os.getuid()).pw_name
        env.update(HOME=self.cfg.home, USER=user, LOGNAME=user, SHELL="/bin/bash", LANG="C.UTF-8")
        try:
            with open(os.path.join(self.cfg.home, ".path")) as f:
                env["PATH"] = f.read().strip()
        except OSError:
            pass
        try:
            with open(os.path.join(self.cfg.home, ".env")) as f:
                for line in f:
                    if "=" in line:
                        k, v = line.rstrip("\n").split("=", 1)
                        env[k] = v
        except OSError:
            pass
        return env

    def as_runner(self):
        """Popen's arguments for running as the job runs: the runner user and its groups, with the job's file mode mask."""
        if self.cfg.user is None:
            return dict(umask=JOB_UMASK)
        pw = pwd.getpwnam(self.cfg.user)
        return dict(user=pw.pw_uid, group=pw.pw_gid, extra_groups=os.getgrouplist(self.cfg.user, pw.pw_gid), umask=JOB_UMASK)

    def start_job(self, payload):
        job = json.loads(payload)
        runner, token = job["runner"], job["token"]
        if not isinstance(runner, str) or not isinstance(token, str) or not token.isascii() or not token.isprintable() or " " in token:
            raise ValueError("JOB is malformed")
        self.token = token
        self.mark("job.got")
        # The runner's settings are the runner user's alone, by their own mode: the agent's mask is the job's.
        fd = os.open(self.cfg.runner_file, os.O_WRONLY | os.O_CREAT | os.O_TRUNC | os.O_NOFOLLOW, 0o600)
        with os.fdopen(fd, "w") as f:
            os.fchmod(fd, 0o600)  # a file an earlier write left keeps its own mode through O_CREAT
            if self.cfg.user is not None:
                pw = pwd.getpwnam(self.cfg.user)
                os.fchown(fd, pw.pw_uid, pw.pw_gid)
            f.write(runner)
        env = self.env()
        # The job-started hook stops the hydration only after "Set up job", which a running pre-read slowed by 19 s, so it ends
        # here, before the Worker starts, as boot.sh does when the runner logs "Running job:".
        try:
            subprocess.run([self.cfg.hydrate_stop], env=env, cwd=self.cfg.home, stdin=subprocess.DEVNULL,
                           stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=self.cfg.hydrate_timeout, **self.as_runner())
        except (OSError, subprocess.SubprocessError) as e:
            log(f"hydration stop: {type(e).__name__}")
        self.job_started = True
        r_in, w_in = os.pipe()
        r_out, w_out = os.pipe()
        try:
            self.proc = subprocess.Popen([self.cfg.worker, "spawnclient", str(r_in), str(w_out)], pass_fds=(r_in, w_out),
                                         cwd=os.path.join(self.cfg.home, "bin"), env=env, stdout=subprocess.PIPE,
                                         stderr=subprocess.PIPE, stdin=subprocess.DEVNULL, **self.as_runner())
        except OSError as e:
            log(f"the Worker did not start: {type(e).__name__}")
            for fd in (w_in, r_out):  # the finally below closes the other two
                os.close(fd)
            self.exit_code = 1
            self.job_taken.set()
            return
        finally:
            for fd in (r_in, w_out):
                try:
                    os.close(fd)
                except OSError:
                    pass
        self.w_in = w_in
        self.mark("worker.start")
        self.threads = [threading.Thread(target=self.pump_out, args=(self.proc.stdout, STDOUT), daemon=True),
                        threading.Thread(target=self.pump_out, args=(self.proc.stderr, STDERR), daemon=True),
                        threading.Thread(target=self.drain, args=(r_out,), daemon=True),
                        threading.Thread(target=self.renew_loop, daemon=True),
                        threading.Thread(target=self.first_message_watch, daemon=True)]
        for t in self.threads:
            t.start()
        self.job_taken.set()

    def probe(self, script):
        """Runs script as start_job runs the Worker (user, groups, mask, environment, directory) and returns its exit code:
        the release's parity check (pull/deploy/parity.sh) reads the job environment through it."""
        return subprocess.run(["/bin/bash", script], env=self.env(), cwd=os.path.join(self.cfg.home, "bin"),
                              stdin=subprocess.DEVNULL, **self.as_runner()).returncode

    def pump_out(self, src, typ):
        for chunk in iter(lambda: src.read1(OUT_CHUNK), b""):
            with self.out_lock:
                if self.out_sent + len(chunk) > OUT_BUDGET:
                    continue  # the dispatcher drops it past the budget; the Worker must still never block on its pipe
                self.out_sent += len(chunk)
            self.send(typ, chunk)

    @staticmethod
    def drain(fd):
        try:
            while os.read(fd, 65536):
                pass
        except OSError:
            pass
        finally:
            os.close(fd)

    def to_worker(self, data):
        self.first.feed(data)
        view = memoryview(data)
        with self.pipe_lock:
            try:
                while view and self.w_in is not None:
                    view = view[os.write(self.w_in, view):]
            except OSError:
                pass  # the Worker is gone: its exit is reported

    def renew_loop(self):
        """Renews the job every renew_every seconds with the job's own token, as the identity's listener does, so the job
        outlives the identity host and the dispatcher."""
        while not self.first.done and not self.worker_done.wait(0.05):
            pass
        if self.first.renewal is None:
            if self.first.done:
                log("no renewal: the job message names no run service")
            return
        url, token, plan, job = self.first.renewal
        if urllib.parse.urlsplit(url).scheme != "https":
            log("no renewal: the job message names a run service that is not https")
            return
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), urllib.request.HTTPSHandler(context=self.cfg.renew_context),
                                             NoRedirect)
        body = json.dumps({"planId": plan, "jobId": job}).encode()
        while True:
            req = urllib.request.Request(url, data=body, method="POST", headers={
                "Content-Type": "application/json", "Accept": "application/json", "Authorization": "Bearer " + token})
            try:
                opener.open(req, timeout=30).close()
            except urllib.error.HTTPError as e:
                if e.code == 404:
                    log("the job is no longer valid: no more renewals")
                    return
                log(f"renew: {e.code}")
            except (OSError, ValueError) as e:
                log(f"renew: {type(e).__name__}")
            if self.worker_done.wait(self.cfg.renew_every):
                return

    def first_message_watch(self):
        """A job message lost between the dispatcher's pairing and its forwarding leaves the Worker reading an empty pipe
        while the identity's listener keeps renewing the job. Ends the Worker, and with it the job, if the message does not
        arrive whole in time."""
        deadline = time.monotonic() + self.cfg.first_message_wait
        while not self.first.done:
            left = deadline - time.monotonic()
            if left <= 0:
                log("no job message arrived: ending the job")
                self.no_message = True
                self.proc.kill()
                return
            if self.worker_done.wait(min(left, 0.05)):
                return

    def ping_loop(self):
        while not self.finished.wait(self.cfg.ping_every):
            self.send(PING)

    def serve(self, s, rf):
        """Reads frames from one connection until it breaks or the dispatcher closes it after the EXIT. Returns whether
        any frame arrived."""
        progressed = False
        try:
            while True:
                if self.exit_code is not None and self.exit_sent_on is not s:
                    self.send_exit(s)
                try:
                    typ, n = struct.unpack("<BI", read_exactly(rf, 5))
                except EOFError:
                    if self.exit_sent_on is s:
                        self.delivered = True
                        self.finished.set()
                    return progressed
                progressed = True
                if typ == PING and n == 0:
                    continue
                if typ == PIPE and n <= MAX_PIPE and self.job_started:
                    self.to_worker(read_exactly(rf, n))
                elif typ == JOB and n <= MAX_JOB:
                    payload = read_exactly(rf, n)
                    if not self.job_started:
                        self.start_job(payload)
                else:
                    log(f"refused a frame of type {typ}, {n} bytes")
                    return progressed
        except (OSError, EOFError, ValueError, KeyError, TypeError) as e:
            log(f"connection: {type(e).__name__}")
            return progressed

    def send_exit(self, s):
        """Sends the EXIT on s, once. Half-closing a TLS socket would end its reads, so the connection stays open: the
        dispatcher closes it after relaying."""
        with self.exit_lock:
            if self.sock is not s or self.exit_sent_on is s:
                return
            if self.send(EXIT, struct.pack("<i", self.exit_code)):
                self.exit_sent_on = s
                self.exit_sent_at = time.monotonic()

    def connect_loop(self):
        outage = self.clock()
        attempt = 0
        while not self.finished.is_set():
            asked = 0  # the Retry-After of a refusal, a floor under the wait
            resume = f"{self.instance}.{self.token}" if self.token else None
            try:
                s, rf = upgrade(self.cfg, self.doc, "/work", resume)
            except Refused as e:
                log(str(e))
                if e.status in (403, 410):
                    break
                asked = e.retry_after or 0
            except (OSError, ssl.SSLError, EOFError) as e:
                log(f"connect: {type(e).__name__}")
            else:
                with self.wlock:
                    self.sock = s
                progressed = self.serve(s, rf)
                with self.wlock:
                    if self.sock is s:
                        self.sock = None
                try:
                    s.close()
                except OSError:
                    pass
                if self.finished.is_set():
                    return
                if progressed:
                    outage = self.clock()
                    attempt = 0
            limit = self.cfg.window if self.token else self.cfg.wait_window
            left = limit - (self.clock() - outage)
            if left <= 0:
                break
            wait = asked + backoff(attempt, self.cfg.retry, self.cfg.retry_max, self.jitter())
            attempt += 1
            self.sleep(min(wait, left))  # the last reconnect starts at the window's end, never after
        self.finished.set()

    def run(self):
        for attempt in range(60):
            try:
                self.doc = imds(self.cfg, "/latest/dynamic/instance-identity/rsa2048")
                self.instance = imds(self.cfg, "/latest/meta-data/instance-id").strip()
                break
            except (OSError, ValueError):
                time.sleep(self.cfg.retry)
        else:
            log("no identity document")
            return 1
        threading.Thread(target=self.ping_loop, daemon=True).start()
        conn = threading.Thread(target=self.connect_loop, daemon=True)
        conn.start()
        while not self.job_taken.wait(0.1):
            if self.finished.is_set():
                log("no job")
                return 0
        if self.proc is not None:
            code = self.proc.wait()
            self.worker_done.set()
            for t in self.threads[:2]:
                t.join(5)
            with self.pipe_lock:
                os.close(self.w_in)
                self.w_in = None
            self.mark("worker.exit", code)
            self.exit_code = 1 if self.no_message else code if code >= 0 else 128 - code
        self.worker_done.set()
        self.deliver_exit()
        self.finished.set()
        return 0 if self.delivered else 1

    def deliver_exit(self):
        """Keeps the EXIT until a connection delivers it: sent, it counts once the dispatcher closes the connection (it
        relays and ends the machine) or leaves it open for exit_settle seconds; a break sends it again on the resumed
        connection, for as long as the resume window."""
        deadline = time.monotonic() + self.cfg.window
        while not self.finished.is_set() and time.monotonic() < deadline:
            s = self.sock
            if s is not None:
                if self.exit_sent_on is not s:
                    self.send_exit(s)
                elif time.monotonic() - self.exit_sent_at >= self.cfg.exit_settle:
                    self.delivered = True
                    return
            self.finished.wait(0.05)


def main(argv):
    if len(argv) == 3 and argv[1] == "--probe":
        return Agent(Config(listener="", pin="")).probe(argv[2])
    if len(argv) != 3:
        print("usage: agent.py <listener url> <pinned key> | agent.py --probe <script>", file=sys.stderr)
        return 2
    return Agent(Config(listener=argv[1], pin=argv[2])).run()


if __name__ == "__main__":
    sys.exit(main(sys.argv))
