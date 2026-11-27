"""Tests of agent.py against a fake dispatcher, a fake instance metadata service and run service, and a fake
Runner.Worker. Run from this directory: python3 -I -m unittest"""
import base64
import http.server
import importlib.util
import json
import os
import types
import unittest.mock
import shutil
import socket
import ssl
import struct
import subprocess
import tempfile
import threading
import time
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
spec = importlib.util.spec_from_file_location("agent", os.path.join(HERE, "agent.py"))
agent = importlib.util.module_from_spec(spec)
spec.loader.exec_module(agent)

DOC = "SIGNED-IDENTITY-DOCUMENT"
INSTANCE = "i-0test"
CERT_DIR = None
PIN = None


def setUpModule():
    global CERT_DIR, PIN
    CERT_DIR = tempfile.mkdtemp()
    key, crt = os.path.join(CERT_DIR, "key.pem"), os.path.join(CERT_DIR, "cert.pem")
    subprocess.run(["openssl", "req", "-x509", "-newkey", "ec", "-pkeyopt", "ec_paramgen_curve:prime256v1", "-nodes", "-days", "2",
                    "-subj", "/CN=fake", "-keyout", key, "-out", crt], check=True, capture_output=True)
    pub = subprocess.run(["openssl", "x509", "-in", crt, "-pubkey", "-noout"], check=True, capture_output=True).stdout
    der = subprocess.run(["openssl", "pkey", "-pubin", "-outform", "der"], input=pub, check=True, capture_output=True).stdout
    import hashlib
    PIN = "sha256//" + base64.b64encode(hashlib.sha256(der).digest()).decode()


def tearDownModule():
    shutil.rmtree(CERT_DIR, ignore_errors=True)


def wait_for(cond, what, timeout=20):
    end = time.monotonic() + timeout
    while time.monotonic() < end:
        if cond():
            return
        time.sleep(0.01)
    raise AssertionError("timed out waiting for " + what)


class Conn:
    """One connection the fake dispatcher accepted."""

    def __init__(self, n, path, headers, body, sock, rf):
        self.n, self.path, self.headers, self.body, self.sock, self.rf = n, path, headers, body, sock, rf
        self.frames = []

    def up(self):
        self.sock.sendall(b"HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: superci-dispatch/1\r\n\r\n")

    def status(self, code):
        self.sock.sendall(f"HTTP/1.1 {code} X\r\nContent-Length: 0\r\n\r\n".encode())

    def send(self, typ, payload=b""):
        self.sock.sendall(struct.pack("<BI", typ, len(payload)) + payload)

    def header(self, typ, n):
        self.sock.sendall(struct.pack("<BI", typ, n))

    def job(self, runner='{"agentName":"id-1"}', token="claim-token"):
        self.send(agent.JOB, json.dumps({"runner": runner, "token": token, "job": {"run": 1, "job": "x", "name": "n", "repo": "r"}}).encode())

    def frame(self):
        """The next frame, or None once the agent closed the connection."""
        try:
            h = self.rf.read(5)
            if len(h) < 5:
                return None
            typ, n = struct.unpack("<BI", h)
            p = self.rf.read(n)
        except TimeoutError:
            raise
        except OSError:
            return None
        self.frames.append((typ, p))
        return typ, p

    def until_exit(self):
        """Reads frames until the EXIT; returns its code, or None if the connection ended first."""
        while True:
            f = self.frame()
            if f is None:
                return None
            if f[0] == agent.EXIT:
                return struct.unpack("<i", f[1])[0]

    def drop(self):
        """Closes the connection now: the buffered reader holds the socket open until it is closed too."""
        self.rf.close()
        self.sock.close()

    def closed_within(self, seconds):
        end = time.monotonic() + seconds
        while time.monotonic() < end:
            self.sock.settimeout(max(end - time.monotonic(), 0.01))
            try:
                if self.frame() is None:
                    return True
            except TimeoutError:
                return False
        return False

    def output(self, typ):
        return b"".join(p for t, p in self.frames if t == typ)


class FakeDispatcher:
    def __init__(self, handler):
        self.handler = handler
        self.conns = []
        self.errors = []
        self.ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        self.ctx.load_cert_chain(os.path.join(CERT_DIR, "cert.pem"), os.path.join(CERT_DIR, "key.pem"))
        self.srv = socket.socket()
        self.srv.bind(("127.0.0.1", 0))
        self.srv.listen(16)
        self.port = self.srv.getsockname()[1]
        self.requests_seen = 0
        self.dirty = False
        threading.Thread(target=self.accept, daemon=True).start()

    def accept(self):
        while True:
            try:
                raw, _ = self.srv.accept()
            except OSError:
                return
            threading.Thread(target=self.serve, args=(raw,), daemon=True).start()

    def serve(self, raw):
        try:
            s = self.ctx.wrap_socket(raw, server_side=True)
        except (ssl.SSLError, OSError):
            raw.close()
            return
        rf = s.makefile("rb")
        try:
            try:
                line = rf.readline()
            except OSError:
                return  # the client left before it asked: a refused pin
            if not line:
                return
            path = line.split()[1].decode()
            headers = {}
            while True:
                h = rf.readline()
                if h in (b"\r\n", b""):
                    break
                k, _, v = h.decode().partition(":")
                headers[k.strip().lower()] = v.strip()
            body = rf.read(int(headers.get("content-length", "0"))).decode()
            self.requests_seen += 1
            c = Conn(len(self.conns), path, headers, body, s, rf)
            self.conns.append(c)
            self.handler(c)
        except Exception as e:  # noqa: BLE001 - a test failure surfaces through errors
            self.errors.append(repr(e))
        finally:
            if not self.dirty:
                try:
                    s.settimeout(1)
                    s.unwrap()
                except (OSError, ValueError):
                    pass
            try:
                rf.close()
                s.close()
            except OSError:
                pass

    def close(self):
        self.srv.close()


class Imds(http.server.BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass

    def do_PUT(self):
        self.send_response(200)
        self.end_headers()
        self.wfile.write(b"imds-token")

    def do_GET(self):
        if self.headers.get("X-aws-ec2-metadata-token") != "imds-token":
            self.send_response(401)
            self.end_headers()
            return
        body = {"/latest/dynamic/instance-identity/rsa2048": DOC, "/latest/meta-data/instance-id": INSTANCE}.get(self.path)
        self.send_response(200 if body else 404)
        self.end_headers()
        self.wfile.write((body or "").encode())


class RunService(http.server.BaseHTTPRequestHandler):
    calls = []
    status = 200
    redirect = None

    def log_message(self, *a):
        pass

    def do_POST(self):
        n = int(self.headers.get("Content-Length", "0"))
        type(self).calls.append((self.path, self.headers.get("Authorization"), json.loads(self.rfile.read(n))))
        if type(self).redirect:
            self.send_response(302)
            self.send_header("Location", type(self).redirect)
            self.send_header("Content-Length", "0")
            self.end_headers()
            return
        self.send_response(type(self).status)
        self.send_header("Content-Length", "2")
        self.end_headers()
        self.wfile.write(b"{}")

    def do_GET(self):
        type(self).calls.append((self.path, self.headers.get("Authorization"), None))
        self.send_response(200)
        self.send_header("Content-Length", "0")
        self.end_headers()


class PlainService(RunService):
    calls = []


WORKER = """#!/bin/bash
echo worker >> "$LOG"
head -c "$WANT" <&"$2"
[ -n "$OUT" ] && head -c "$OUT" /dev/zero | tr '\\0' x
echo stderr-line >&2
echo "env:$FROM_ENVIRONMENT:$FROM_DOTENV:$PWD" >> "$LOG"
[ -n "$HOLD" ] && while [ ! -e "$HOLD" ]; do sleep 0.02; done
[ -n "$SIGNAL" ] && kill -9 $$
exit "${CODE:-100}"
"""


class AgentCase(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, self.tmp, True)
        self.home = os.path.join(self.tmp, "home")
        os.makedirs(os.path.join(self.home, "bin"))
        self.log = os.path.join(self.tmp, "log")
        self.hold = os.path.join(self.tmp, "hold")
        self.spool = os.path.join(self.tmp, "marks")
        open(self.spool, "w").close()
        self.put_exec(os.path.join(self.home, "bin", "Runner.Worker"), WORKER)
        self.put_exec(os.path.join(self.home, "superci-hydrate-stop.sh"), '#!/bin/bash\necho hydrate >> "$LOG"\n')
        with open(os.path.join(self.tmp, "environment"), "w") as f:
            f.write('PATH="/usr/bin:/bin"\nFROM_ENVIRONMENT=env\n')
        self.dotenv(WANT="0")
        self.imds = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Imds)
        threading.Thread(target=self.imds.serve_forever, daemon=True).start()
        self.addCleanup(self.imds.shutdown)
        RunService.calls = []
        RunService.status = 200
        RunService.redirect = None
        PlainService.calls = []
        self.runsvc = http.server.ThreadingHTTPServer(("127.0.0.1", 0), RunService)
        tls = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        tls.load_cert_chain(os.path.join(CERT_DIR, "cert.pem"), os.path.join(CERT_DIR, "key.pem"))
        self.runsvc.socket = tls.wrap_socket(self.runsvc.socket, server_side=True)
        threading.Thread(target=self.runsvc.serve_forever, daemon=True).start()
        self.addCleanup(self.runsvc.shutdown)
        self.plainsvc = http.server.ThreadingHTTPServer(("127.0.0.1", 0), PlainService)
        threading.Thread(target=self.plainsvc.serve_forever, daemon=True).start()
        self.addCleanup(self.plainsvc.shutdown)
        self.trust = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
        self.trust.check_hostname = False
        self.trust.verify_mode = ssl.CERT_NONE  # the run service's certificate is the test's own
        self.disp = None
        self.agent = None
        self.result = {}

    def put_exec(self, path, text):
        with open(path, "w") as f:
            f.write(text)
        os.chmod(path, 0o755)

    def dotenv(self, **kw):
        kw.update(LOG=self.log, FROM_DOTENV="dot")
        kw.setdefault("HOLD", "")
        with open(os.path.join(self.home, ".env"), "w") as f:
            f.write("".join(f"{k}={v}\n" for k, v in kw.items()))

    def dispatcher(self, handler):
        self.disp = FakeDispatcher(handler)
        self.addCleanup(self.disp.close)
        return self.disp

    def config(self, **kw):
        cfg = dict(listener=f"https://127.0.0.1:{self.disp.port if self.disp else 1}", pin=PIN, imds=f"http://127.0.0.1:{self.imds.server_port}",
                   home=self.home, user=None, spool=self.spool, environment=os.path.join(self.tmp, "environment"),
                   ping_every=0.05, silence=5, window=1.0, wait_window=1.0, retry=0.02, renew_every=0.05, exit_settle=0.3,
                   hydrate_timeout=5, renew_context=self.trust)
        cfg.update(kw)
        return agent.Config(**cfg)

    def start(self, **kw):
        self.agent = agent.Agent(self.config(**kw))
        self.thread = threading.Thread(target=lambda: self.result.update(code=self.agent.run()), daemon=True)
        self.thread.start()

    def finish(self, timeout=30):
        self.thread.join(timeout)
        self.assertFalse(self.thread.is_alive(), "the agent did not finish")
        self.assertEqual(self.disp.errors, [])
        return self.result["code"]

    def lines(self, path):
        try:
            with open(path) as f:
                return f.read().split("\n")[:-1]
        except FileNotFoundError:
            return []

    def run_job(self, pipe=b"", token="claim-token", code_expected=100, **dot):
        """A dispatcher that gives its one machine a job and pipes bytes, reads until the EXIT, and closes."""
        out = {}

        def handler(c):
            c.up()
            c.job(token=token)
            if pipe:
                c.send(agent.PIPE, pipe)
            out["code"] = c.until_exit()
            out["conn"] = c
        self.dispatcher(handler)
        self.dotenv(WANT=str(len(pipe)), **dot)
        self.start()
        rc = self.finish()
        return rc, out


class TestJob(AgentCase):
    def test_the_workers_pipe_is_echoed_and_its_exit_code_is_reported(self):
        rc, out = self.run_job(pipe=b"hello pipe")
        self.assertEqual(out["code"], 100)
        self.assertEqual(out["conn"].output(agent.STDOUT), b"hello pipe")
        self.assertEqual(out["conn"].output(agent.STDERR), b"stderr-line\n")
        self.assertEqual(rc, 0, "the exit was delivered")

    def test_the_request_proves_the_machine_and_carries_no_resume_before_a_job(self):
        rc, out = self.run_job(pipe=b"x")
        c = self.disp.conns[0]
        self.assertEqual((c.path, c.body), ("/work", DOC))
        self.assertEqual(c.headers["upgrade"], "superci-dispatch/1")
        self.assertNotIn("x-superci-resume", c.headers)

    def test_the_runner_file_is_written_and_no_credential_ever_is(self):
        self.run_job(pipe=b"x")
        self.assertEqual(open(os.path.join(self.home, ".runner")).read(), '{"agentName":"id-1"}')
        names = [n for _, _, fs in os.walk(self.home) for n in fs] + [n for _, ds, _ in os.walk(self.home) for n in ds]
        self.assertEqual([n for n in names if n.startswith(".credentials")], [])

    def test_the_hydration_stop_runs_before_the_worker(self):
        self.run_job(pipe=b"x")
        self.assertEqual(self.lines(self.log)[:2], ["hydrate", "worker"])

    def test_the_worker_gets_the_environment_boot_sh_gives_a_runner(self):
        self.run_job(pipe=b"x")
        self.assertIn(f"env:env:dot:{os.path.join(self.home, 'bin')}", self.lines(self.log))

    def test_a_worker_killed_by_a_signal_reports_128_plus_it(self):
        _, out = self.run_job(pipe=b"x", SIGNAL="1")
        self.assertEqual(out["code"], 128 + 9)

    def test_output_past_four_mebibytes_is_not_sent(self):
        _, out = self.run_job(pipe=b"x", OUT=str(5 << 20))
        sent = len(out["conn"].output(agent.STDOUT)) + len(out["conn"].output(agent.STDERR))
        self.assertLessEqual(sent, agent.OUT_BUDGET)
        self.assertGreater(sent, agent.OUT_BUDGET - agent.OUT_CHUNK - 100)

    def test_pings_go_out_while_it_waits(self):
        seen = threading.Event()

        def handler(c):
            c.up()
            while True:
                f = c.frame()
                if f is None:
                    return
                if f[0] == agent.PING:
                    seen.set()
                    c.job()
                    c.send(agent.PIPE, b"x")
                    c.until_exit()
                    return
        self.dispatcher(handler)
        self.dotenv(WANT="1")
        self.start()
        self.finish()
        self.assertTrue(seen.is_set())

    def test_timeline_marks_are_written_in_the_spool_format(self):
        self.run_job(pipe=b"x")
        marks = self.lines(self.spool)
        self.assertEqual([m.split("=")[0] for m in marks], ["job.got", "worker.start", "worker.exit"])
        for m in marks:
            name, epoch, ms, *val = m.split("=")
            self.assertRegex(epoch, r"^\d{10}\.\d{6}$")
            self.assertRegex(ms, r"^\d+$")
        self.assertEqual(marks[2].split("=")[3], "100")


class TestFrames(AgentCase):
    def test_frames_outside_their_bound_end_the_connection_and_start_no_worker(self):
        bad = {
            "a PIPE before the job": lambda c: c.send(agent.PIPE, b"x"),
            "a JOB over 64 KiB": lambda c: c.header(agent.JOB, agent.MAX_JOB + 1),
            "an unknown type": lambda c: c.header(99, 0),
            "an EXIT toward the machine": lambda c: c.send(agent.EXIT, b"\0\0\0\0"),
            "a PING with a body": lambda c: c.send(agent.PING, b"x"),
            "a malformed JOB": lambda c: c.send(agent.JOB, b"not json"),
        }
        for name, frame in bad.items():
            with self.subTest(name):
                open(self.log, "w").close()
                closed = []

                def handler(c):
                    if c.n > 0:
                        c.status(410)
                        return
                    c.up()
                    frame(c)
                    closed.append(c.closed_within(3))
                self.dispatcher(handler)
                self.start()
                self.finish()
                self.assertEqual(closed, [True], "the agent kept the connection")
                self.assertEqual(self.lines(self.log), [], "a worker started")

    def test_a_pipe_frame_over_its_bound_ends_the_connection_unread(self):
        closed = []

        def handler(c):
            c.up()
            if c.n > 0:
                c.job()
                c.send(agent.PIPE, b"x")
                c.until_exit()
                return
            c.job()
            c.header(agent.PIPE, agent.MAX_PIPE + 1)
            closed.append(c.closed_within(3))
        self.dispatcher(handler)
        self.dotenv(WANT="1")
        self.start()
        self.finish()
        self.assertEqual(closed, [True])
        self.assertEqual(self.disp.conns[1].headers["x-superci-resume"], f"{INSTANCE}.claim-token")


class TestPairing(AgentCase):
    def test_a_second_ask_or_a_gone_machine_gets_410_and_runs_nothing(self):
        self.dispatcher(lambda c: c.status(410))
        self.start()
        self.assertEqual(self.finish(), 0)
        self.assertEqual(self.disp.requests_seen, 1, "asked again after 410")
        self.assertEqual(self.lines(self.log), [])

    def test_a_listener_that_is_not_the_pinned_one_gets_nothing(self):
        self.dispatcher(lambda c: c.up())
        self.start(pin="sha256//" + base64.b64encode(b"x" * 32).decode(), wait_window=0.3)
        self.finish()
        self.assertEqual(self.disp.requests_seen, 0, "the identity document went to an unpinned listener")

    def test_the_pin_is_the_hash_curl_takes(self):
        crt = os.path.join(CERT_DIR, "cert.pem")
        der = subprocess.run(["openssl", "x509", "-in", crt, "-outform", "der"], check=True, capture_output=True).stdout
        self.assertTrue(agent.pin_matches(der, PIN))
        self.assertTrue(agent.pin_matches(der, "sha256//other;" + PIN))
        self.assertFalse(agent.pin_matches(der, "sha256//" + base64.b64encode(b"x" * 32).decode()))

    def test_a_break_after_the_job_resumes_and_the_exit_waits_for_a_connection(self):
        allow = threading.Event()
        got = {}

        def handler(c):
            if c.n == 0:
                c.up()
                c.job(token="tok-7")
                c.send(agent.PIPE, b"x")
                wait_for(lambda: os.path.exists(os.path.join(self.tmp, "log")) and "worker" in self.lines(self.log), "worker")
                c.drop()
                return
            allow.wait(10)
            c.up()
            got["code"] = c.until_exit()
        self.dispatcher(handler)
        self.dotenv(WANT="1", HOLD=self.hold)
        self.start(window=20, wait_window=20)
        wait_for(lambda: len(self.disp.conns) >= 1, "the first connection")
        wait_for(lambda: "worker" in self.lines(self.log), "the worker")
        wait_for(lambda: self.agent.sock is None, "the break")
        open(self.hold, "w").close()
        wait_for(lambda: self.agent.exit_code is not None, "the worker's exit")
        allow.set()
        self.finish()
        self.assertEqual(got["code"], 100, "the exit was lost across the break")
        self.assertEqual(self.disp.conns[1].headers["x-superci-resume"], f"{INSTANCE}.tok-7")
        self.assertEqual(self.lines(self.log).count("worker"), 1, "the Worker started again on resume")

    def test_an_exit_on_a_connection_that_resets_is_sent_again(self):
        got = []

        def handler(c):
            c.up()
            if c.n == 0:
                c.job()
                c.send(agent.PIPE, b"x")
                got.append(c.until_exit())
                self.disp.dirty = True
                c.sock.setsockopt(socket.SOL_SOCKET, socket.SO_LINGER, struct.pack("ii", 1, 0))
                c.drop()
                return
            self.disp.dirty = False
            got.append(c.until_exit())
        self.dispatcher(handler)
        self.dotenv(WANT="1")
        self.start()
        self.assertEqual(self.finish(), 0)
        self.assertEqual(got, [100, 100], "the EXIT of a reset connection was not sent again")
        self.assertEqual(self.disp.conns[1].headers["x-superci-resume"], f"{INSTANCE}.claim-token")


class TestRenewal(AgentCase):
    def message(self, url, token="job-secret-token"):
        body = json.dumps({"plan": {"planId": "plan-guid"}, "jobId": "job-guid", "requestId": 1, "resources": {"endpoints": [
            {"name": "SystemVssConnection", "url": url, "authorization": {"parameters": {"AccessToken": token}, "scheme": "OAuth"}}]}})
        raw = body.encode("utf-16-le")
        return struct.pack("<ii", 1, len(raw)) + raw

    def test_the_job_is_renewed_with_its_own_token(self):
        msg = self.message(f"https://127.0.0.1:{self.runsvc.server_port}/abc/")

        def handler(c):
            c.up()
            c.job()
            c.send(agent.PIPE, msg[:30])
            c.send(agent.PIPE, msg[30:])
            c.until_exit()
        self.dispatcher(handler)
        self.dotenv(WANT=str(len(msg)), HOLD=self.hold)
        self.start()
        wait_for(lambda: len(RunService.calls) >= 2, "two renewals")
        open(self.hold, "w").close()
        self.finish()
        path, auth, body = RunService.calls[0]
        self.assertEqual((path, auth, body), ("/abc/renewjob", "Bearer job-secret-token", {"planId": "plan-guid", "jobId": "job-guid"}))

    def test_a_job_the_service_no_longer_knows_is_not_renewed_again(self):
        RunService.status = 404
        msg = self.message(f"https://127.0.0.1:{self.runsvc.server_port}/abc/")

        def handler(c):
            c.up()
            c.job()
            c.send(agent.PIPE, msg)
            c.until_exit()
        self.dispatcher(handler)
        self.dotenv(WANT=str(len(msg)), HOLD=self.hold)
        self.start(renew_every=0.02)
        wait_for(lambda: self.agent is not None and hasattr(self.agent, "threads"), "the job to start")
        wait_for(lambda: not self.agent.threads[3].is_alive(), "the renewals to stop")
        open(self.hold, "w").close()
        self.finish()
        self.assertEqual(len(RunService.calls), 1)

    def test_nothing_of_the_message_but_the_renewal_is_kept(self):
        m = agent.FirstMessage()
        msg = self.message("http://h/p/")
        m.feed(msg[:5])
        self.assertFalse(m.done)
        m.feed(msg[5:] + struct.pack("<ii", 2, 4) + b"zzzz")
        self.assertEqual(m.renewal, ("http://h/p/renewjob", "job-secret-token", "plan-guid", "job-guid"))
        self.assertEqual(len(m.buf), 0)
        other = agent.FirstMessage()
        other.feed(struct.pack("<ii", 2, 4) + b"zzzz")
        self.assertTrue(other.done)
        self.assertIsNone(other.renewal)

    def test_the_url_joins_as_the_runner_joins_it(self):
        for url, want in [("https://x/y/", "https://x/y/renewjob"), ("https://x/y", "https://x/renewjob")]:
            self.assertEqual(agent.FirstMessage.extract({"Plan": {"PlanId": "p"}, "JobId": "j", "Resources": {"Endpoints": [
                {"Name": "systemvssconnection", "Url": url, "Authorization": {"Parameters": {"accesstoken": "t"}}}]}})[0], want)


class TestRenewalLimits(AgentCase):
    message = TestRenewal.message

    def held_job(self, url, **start):
        msg = self.message(url)

        def handler(c):
            c.up()
            c.job()
            c.send(agent.PIPE, msg)
            c.until_exit()
        self.dispatcher(handler)
        self.dotenv(WANT=str(len(msg)), HOLD=self.hold)
        self.start(**start)
        wait_for(lambda: self.agent is not None and hasattr(self.agent, "threads"), "the job to start")

    def test_renewal_stops_once_the_worker_has_exited(self):
        self.held_job(f"https://127.0.0.1:{self.runsvc.server_port}/abc/")
        wait_for(lambda: len(RunService.calls) >= 2, "two renewals")
        open(self.hold, "w").close()
        self.finish()
        wait_for(lambda: not self.agent.threads[3].is_alive(), "the renewals to stop", timeout=3)

    def test_a_job_token_never_goes_over_plain_http(self):
        self.held_job(f"http://127.0.0.1:{self.plainsvc.server_port}/abc/")
        wait_for(lambda: not self.agent.threads[3].is_alive(), "the renewal thread to give up", timeout=3)
        open(self.hold, "w").close()
        self.finish()
        self.assertEqual(PlainService.calls, [])

    def test_a_redirect_from_the_run_service_is_not_followed(self):
        RunService.redirect = f"https://127.0.0.1:{self.runsvc.server_port}/stolen"
        self.held_job(f"https://127.0.0.1:{self.runsvc.server_port}/abc/")
        wait_for(lambda: len(RunService.calls) >= 3, "three renewals")
        open(self.hold, "w").close()
        self.finish()
        self.assertEqual({path for path, _, _ in RunService.calls}, {"/abc/renewjob"}, "the token followed a redirect")

    def test_a_message_over_its_bound_is_dropped_unread(self):
        m = agent.FirstMessage()
        m.feed(struct.pack("<ii", 1, agent.MAX_MESSAGE + 1) + b"x" * 100)
        self.assertTrue(m.done)
        self.assertIsNone(m.renewal)
        m.feed(b"y" * 1000)
        self.assertEqual(len(m.buf), 0)
        at_bound = agent.FirstMessage()
        at_bound.feed(struct.pack("<ii", 1, agent.MAX_MESSAGE))
        self.assertFalse(at_bound.done, "a message at its bound is still read")


class TestFirstMessage(AgentCase):
    def test_a_job_whose_message_never_arrives_is_ended_with_one(self):
        got = {}

        def handler(c):
            c.up()
            c.job()
            got["code"] = c.until_exit()
        self.dispatcher(handler)
        self.put_exec(os.path.join(self.home, "bin", "Runner.Worker"), '#!/bin/bash\nexec head -c 1 <&"$2"\n')
        self.start(first_message_wait=0.3)
        self.assertEqual(self.finish(timeout=8), 0, "the exit was delivered")
        self.assertEqual(got["code"], 1)

    def test_a_message_that_arrived_whole_is_not_timed_out(self):
        msg = TestRenewal.message(self, f"https://127.0.0.1:{self.runsvc.server_port}/abc/")
        got = {}

        def handler(c):
            c.up()
            c.job()
            c.send(agent.PIPE, msg)
            got["code"] = c.until_exit()
        self.dispatcher(handler)
        self.dotenv(WANT=str(len(msg)), HOLD=self.hold)
        self.start(first_message_wait=0.2)
        wait_for(lambda: len(RunService.calls) >= 1, "a renewal")
        threading.Event().wait(0.6)
        self.assertIsNone(self.agent.proc.poll(), "the Worker was ended after its message arrived")
        open(self.hold, "w").close()
        self.finish()
        self.assertEqual(got["code"], 100)


class TestDelivery(AgentCase):
    def test_a_clean_close_before_the_exit_is_a_break_to_resume_not_a_delivery(self):
        got = {}

        def handler(c):
            c.up()
            if c.n == 0:
                c.job(token="tok-3")
                c.send(agent.PIPE, b"x")
                wait_for(lambda: "worker" in self.lines(self.log), "worker")
                return
            got["resume"] = c.headers.get("x-superci-resume")
            got["code"] = c.until_exit()
        self.dispatcher(handler)
        self.dotenv(WANT="1", HOLD=self.hold)
        self.start()
        wait_for(lambda: len(self.disp.conns) >= 2, "a resumed connection", timeout=5)
        open(self.hold, "w").close()
        self.assertEqual(self.finish(), 0)
        self.assertEqual(got, {"resume": f"{INSTANCE}.tok-3", "code": 100})

    def test_an_exit_the_dispatcher_leaves_open_counts_as_delivered_after_the_settle(self):
        open_ = threading.Event()
        self.addCleanup(open_.set)

        def handler(c):
            c.up()
            c.job()
            c.send(agent.PIPE, b"x")
            c.until_exit()
            open_.wait(10)
        self.dispatcher(handler)
        self.dotenv(WANT="1")
        self.start(window=5, exit_settle=0.3)
        started = time.monotonic()
        self.assertEqual(self.finish(), 0)
        self.assertLess(time.monotonic() - started, 4, "waited out the resume window")

    def test_an_exit_no_connection_took_makes_the_agent_fail(self):
        def handler(c):
            if c.n > 0:
                c.status(410)
                return
            c.up()
            c.job()
            c.send(agent.PIPE, b"x")
            wait_for(lambda: "worker" in self.lines(self.log), "worker")
            c.drop()
        self.dispatcher(handler)
        self.dotenv(WANT="1", HOLD=self.hold)
        self.start()
        wait_for(lambda: self.agent.finished.is_set(), "the dispatcher to refuse the resume", timeout=5)
        open(self.hold, "w").close()
        self.assertEqual(self.finish(), 1)

    def test_the_exit_is_sent_once_per_connection_whoever_asks(self):
        a = agent.Agent(self.config())
        a.exit_code = 1
        a.sock = s = object()
        sent = []

        def slow_send(typ, payload=b""):
            sent.append(typ)
            time.sleep(0.1)
            return True
        a.send = slow_send
        threads = [threading.Thread(target=a.send_exit, args=(s,)) for _ in range(2)]
        for t in threads:
            t.start()
        for t in threads:
            t.join()
        self.assertEqual(sent, [agent.EXIT])

    def test_the_output_budget_holds_when_stdout_and_stderr_race(self):
        class Racy(agent.Agent):
            _sent = 0

            @property
            def out_sent(self):
                v = self._sent
                time.sleep(0.0002)
                return v

            @out_sent.setter
            def out_sent(self, v):
                self._sent = v

        a = Racy(self.config())
        sent = []
        a.send = lambda typ, payload=b"": sent.append(len(payload)) or True

        class Src:
            def __init__(self):
                self.left = 6 << 20

            def read1(self, n):
                n = min(n, self.left)
                self.left -= n
                return b"x" * n

        threads = [threading.Thread(target=a.pump_out, args=(Src(), typ)) for typ in (agent.STDOUT, agent.STDERR)]
        for t in threads:
            t.start()
        for t in threads:
            t.join()
        self.assertLessEqual(sum(sent), agent.OUT_BUDGET)


class TestWorkerStart(AgentCase):
    RUNNER = types.SimpleNamespace(pw_uid=41001, pw_gid=41002)

    def start_as_runner(self, popen):
        a = agent.Agent(self.config(user="runner"))
        ran = []

        def run(*args, **kw):
            ran.append(kw)
            raise OSError("not run")
        job = json.dumps({"runner": "{}", "token": "t"})
        with unittest.mock.patch.object(agent.pwd, "getpwnam", return_value=self.RUNNER), \
                unittest.mock.patch.object(os, "getgrouplist", return_value=[41002, 41003]), \
                unittest.mock.patch.object(os, "chown"), \
                unittest.mock.patch.object(agent.subprocess, "run", run), \
                unittest.mock.patch.object(agent.subprocess, "Popen", popen):
            a.start_job(job)
        return a, ran

    def test_the_hydration_stop_and_the_worker_run_as_the_runner_user_not_root(self):
        popped = []

        def popen(*args, **kw):
            popped.append(kw)
            raise OSError("not started")
        a, ran = self.start_as_runner(popen)
        want = dict(user=41001, group=41002, extra_groups=[41002, 41003])
        for kw in ran + popped:
            self.assertEqual({k: kw.get(k) for k in want}, want)
        self.assertEqual((len(ran), len(popped)), (1, 1))
        self.assertEqual(agent.Agent(self.config()).as_runner(), {}, "a test config runs as the agent's own user")

    def test_a_worker_that_does_not_start_closes_each_of_its_pipes_once(self):
        def popen(*args, **kw):
            raise OSError("not started")
        closed, made = [], []
        real_close, real_pipe = os.close, os.pipe

        def pipe():
            made.extend(real_pipe())
            return made[-2:]

        def close(fd):
            if made:
                closed.append(fd)
            real_close(fd)
        with unittest.mock.patch.object(os, "close", close), unittest.mock.patch.object(os, "pipe", pipe):
            a, _ = self.start_as_runner(popen)
        self.assertEqual(sorted(closed), sorted(made), "a pipe end was left open or closed twice")
        self.assertEqual(a.exit_code, 1)


if __name__ == "__main__":
    unittest.main()
