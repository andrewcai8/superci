import contextlib
import io
import json
import os
import shutil
import stat
import subprocess
import sys
import tempfile
import time
import unittest

sys.path.insert(0, os.path.dirname(__file__))
import timeline as T

T0 = T.ts("2026-11-25T17:04:40Z")


def job(runner="superci-4cpu-i-0abc", group="superci-pull", started=21, completed=60, steps=None):
    return {
        "id": 1, "run_id": 9, "name": "verify-tools", "labels": ["superci-4cpu"], "runner_name": runner, "runner_group_name": group,
        "created_at": T.iso(T0), "started_at": T.iso(T0 + started), "completed_at": T.iso(T0 + completed),
        "steps": steps or [
            {"name": "Set up job", "started_at": T.iso(T0 + started), "completed_at": T.iso(T0 + started + 2)},
            {"name": "Run tests", "started_at": T.iso(T0 + started + 2), "completed_at": T.iso(T0 + completed - 1)},
            {"name": "Complete job", "started_at": T.iso(T0 + completed - 1), "completed_at": T.iso(T0 + completed)},
        ],
    }


def ev(name, dt_, **kw):
    return dict(ev=name, t=T0 + dt_, inst="i-0abc", **kw)


def cold_events():
    return [
        dict(ev="job.assigned", t=T0 + 0.4, job="g7", set="superci-4cpu", run=9),
        dict(ev="machine.launched", t=T0 + 3.0, inst="i-0abc", requested=T0 + 1.0, region="us-east-2", az="us-east-2a", type="c7i.xlarge", lifecycle="spot", round="r"),
        ev("ec2.state", 1.5, state="pending"), ev("ec2.state", 4, state="running"),
        ev("boot.start", 9, val="3.0", offset_ms=40), ev("cache.start", 9.5), ev("cache.up", 12), ev("doc.got", 12.2), ev("jit.sent", 12.3),
        ev("jit.ask", 12.5, verdict="wait"), ev("jit.granted", 13.0, waited_ms=500), ev("jit.issued", 14, register_ms=1000), ev("jit.answered", 14.2), ev("runner.start", 14.3),
        ev("runner.online", 17), dict(ev="job.started", t=T0 + 21, inst="i-0abc", run=9, job="g7", set="superci-4cpu", runner_assign_time=T.iso(T0 + 18.5)),
        ev("job.running", 19), ev("machine.ended", 62, why="job done"), ev("ec2.state", 66, state="terminated"),
    ]


class Build(unittest.TestCase):
    def test_every_second_of_a_cold_job_is_in_a_named_phase(self):
        tl = T.build(job(), cold_events())
        self.assertEqual(tl.attrs["pool"], "cold")
        self.assertAlmostEqual(tl.wall, 60)
        self.assertAlmostEqual(sum(p[2] for p in tl.phases) - tl.overlap, tl.wall)
        self.assertEqual(tl.gaps, [])
        self.assertAlmostEqual(tl.unexplained, 0)
        labels = [p[0] for p in tl.phases]
        for want in ("github to listener", "CreateFleet call", "cache daemon up", "registration token wait (budget, readiness, 3 s polling)", "GitHub registration and issued tag", "Run tests"):
            self.assertIn(want, labels)
        self.assertEqual([n for n, _ in tl.after], ["machine.ended", "ec2.terminated"])

    def test_a_missing_milestone_is_unexplained_not_hidden(self):
        events = [e for e in cold_events() if e["ev"] not in ("boot.start", "cache.start", "cache.up", "doc.got", "jit.sent", "jit.ask", "jit.granted", "jit.answered", "runner.start", "runner.online", "job.running")]
        tl = T.build(job(), events)
        self.assertEqual(len(tl.gaps), 3)
        self.assertGreater(tl.unexplained, 5)
        self.assertTrue(any(p[0].startswith("unobserved") for p in tl.phases))
        self.assertAlmostEqual(sum(p[2] for p in tl.phases) - tl.overlap, tl.wall)

    def test_a_warm_machine_leaves_the_queue_to_the_job(self):
        machine_side = {"machine.launched", "ec2.state", "boot.start", "cache.start", "cache.up", "doc.got", "jit.sent", "jit.ask", "jit.granted", "jit.issued", "jit.answered", "runner.start", "runner.online"}
        events = []
        for e in cold_events():
            e = dict(e)
            if e["ev"] in machine_side and not (e["ev"] == "ec2.state" and e["state"] == "terminated"):
                e["t"] -= 400
                if "requested" in e:
                    e["requested"] -= 400
            events.append(e)
        tl = T.build(job(started=21, completed=60), events)
        self.assertEqual(tl.attrs["pool"], "warm")
        self.assertTrue(tl.pre)
        self.assertNotIn("CreateFleet call", [p[0] for p in tl.phases])
        self.assertEqual(tl.gaps, [])
        self.assertEqual([p[0] for p in tl.phases][:3], ["github to listener", "runner online to job assigned (GitHub)", "assigned to running"])

    def test_blacksmith_is_the_api_alone_and_expects_nothing_more(self):
        tl = T.build(job(runner="blacksmith-4vcpu-ubuntu-2404-abc", group="blacksmith scale sets"), [])
        self.assertEqual(tl.fleet, "blacksmith")
        self.assertEqual(tl.phases[0][0], T.PHASE_API_QUEUE)
        self.assertAlmostEqual(tl.phases[0][2], 21)
        self.assertEqual(tl.unexplained, 0)

    def test_a_clock_that_runs_behind_is_clamped_and_counted(self):
        events = cold_events()
        for e in events:
            if e["ev"] == "runner.online":
                e["t"] += 5  # after the job was assigned
        tl = T.build(job(), events)
        self.assertGreater(tl.overlap, 1)
        self.assertAlmostEqual(tl.unexplained, 0)


class Claim(unittest.TestCase):
    def test_a_listener_without_the_claim_owes_no_milestone_and_one_with_it_does(self):
        tl = T.build(job(), cold_events())
        self.assertNotIn("identity claim (S3 conditional write)", [p[0] for p in tl.phases])
        self.assertEqual(tl.gaps, [])
        withclaim = cold_events() + [ev("jit.claim", 13.4, result="taken", claim_ms=300)]
        tl = T.build(job(), withclaim)
        self.assertIn("identity claim (S3 conditional write)", [p[0] for p in tl.phases])
        self.assertEqual(tl.attrs["claim"], "taken")
        self.assertEqual(tl.gaps, [])
        missing = cold_events() + [dict(ev="jit.claim", t=T0 + 99, inst="i-other", result="taken")]
        self.assertTrue(T.build(job(), missing).gaps)


class Machine(unittest.TestCase):
    def test_a_clock_off_by_seconds_is_moved_onto_the_listeners(self):
        events = cold_events()
        for e in events:
            if e.get("offset_ms") is not None or e["ev"] in T.MACHINE_MARKS:
                e["t"] -= 4.0  # the machine's clock runs 4 s behind
                e["offset_ms"] = 4040
        tl = T.build(job(), events)
        self.assertTrue(tl.attrs["clock_corrected"])
        self.assertEqual(tl.attrs["clock_offset_ms"], 4040)
        self.assertLess(tl.overlap, 0.1)

    def test_lines_written_before_the_offset_estimate_carry_skew_ms_and_are_read_the_same(self):
        events = cold_events()
        for e in events:
            if e.get("offset_ms") is not None or e["ev"] in T.MACHINE_MARKS:
                e["t"] -= 4.0
                e.pop("offset_ms", None)
                e["skew_ms"] = 4040
        tl = T.build(job(), events)
        self.assertTrue(tl.attrs["clock_corrected"])
        self.assertEqual(tl.attrs["clock_offset_ms"], 4040)
        self.assertLess(tl.overlap, 0.1)

    def test_a_clock_that_chrony_steps_during_boot_is_corrected_post_by_post(self):
        events = cold_events()
        for e in events:
            if e["ev"] in ("boot.start", "cache.start", "cache.up", "doc.got", "jit.sent"):
                e["t"] -= 1.7
                e["offset_ms"] = 1700
            elif e["ev"] in ("jit.answered", "runner.start", "runner.online", "job.running"):
                e["offset_ms"] = 40
        tl = T.build(job(), events)
        self.assertEqual(tl.attrs["clock_offset_ms"], 1700)
        self.assertTrue(tl.attrs["clock_corrected"])
        self.assertLess(tl.overlap, 0.2)

    def test_cache_marks_of_the_daemon(self):
        events = cold_events() + [ev("cache.restore", 30, val="hit:1081159529:4500:bun-modules%3Av1")]
        tl = T.build(job(), events)
        self.assertEqual(tl.cache, [{"key": "bun-modules:v1", "hit": True, "op": "restore", "bytes": 1081159529, "seconds": 4.5}])

    def test_the_log_ships_at_least_once(self):
        rows = [{"@message": json.dumps({"msg": "tl", "v": 1, "ev": "poweroff", "t": "2026-11-25T17:00:00Z", "eid": "a.1"})}] * 2
        orig = T.insights
        T.insights = lambda *a, **k: rows
        try:
            self.assertEqual(len(T.listener_events("p", 0, 1)), 1)
        finally:
            T.insights = orig


class CacheLog(unittest.TestCase):
    def test_restore_hit_and_miss(self):
        log = "\n".join([
            "2026-11-25T17:05:10.0000000Z Cache Size: ~1031 MB (1081159529 B)",
            "2026-11-25T17:05:14.5000000Z Cache restored from key: bun-modules-v1-abc",
            "2026-11-25T17:05:15.0000000Z Cache not found for input keys: git-seed-v1-x",
        ])
        got = T.cache_restores(log)
        self.assertEqual(got[0], {"key": "bun-modules-v1-abc", "hit": True, "bytes": 1081159529, "seconds": 4.5})
        self.assertFalse(got[1]["hit"])


class Schema(unittest.TestCase):
    def test_every_event_the_tool_reads_is_in_the_schema(self):
        with open(os.path.join(os.path.dirname(__file__), "..", "..", "timeline.schema.json")) as f:
            schema = json.load(f)
        self.assertEqual(schema["version"], T.SCHEMA_VERSION)
        names = set(schema["events"])
        for name in T.MACHINE_MARKS + ("poweroff", "jit.ask", "jit.granted", "jit.claim", "jit.issued", "machine.ended", "machine.launched", "job.assigned", "job.started", "job.completed", "ec2.state"):
            self.assertIn(name, names)


class Stats(unittest.TestCase):
    def test_percentile(self):
        self.assertEqual(T.q([1, 2, 3, 4, 5], 50), 3)
        self.assertAlmostEqual(T.q([1, 2, 3, 4, 5], 90), 4.6)
        self.assertIsNone(T.q([], 50))


class AppToken(unittest.TestCase):
    """The tools read GitHub with a token the listener minted, held in memory, handed to each gh call as GH_TOKEN."""

    def setUp(self):
        self.assertTrue(hasattr(T, "AUTH"), "timeline reads GitHub through an AUTH (apptoken.py)")
        self.tmp = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, self.tmp, True)
        self.count = os.path.join(self.tmp, "count")
        self.log = os.path.join(self.tmp, "gh.log")
        self.saved = (dict(os.environ), T.AUTH)
        T.AUTH = T.apptoken.Auth()
        for k in ("GH_TOKEN", "GITHUB_TOKEN"):
            os.environ.pop(k, None)
        os.makedirs(os.path.join(self.tmp, "bin"))
        self.script("bin/gh", 'printf "%s\\t%s\\n" "${GH_TOKEN-UNSET}" "$*" >> "$GHLOG"\necho \'{"total_count":0,"jobs":[]}\'\n')
        os.environ.update(PATH=os.path.join(self.tmp, "bin") + os.pathsep + os.environ["PATH"], GHLOG=self.log)

    def tearDown(self):
        os.environ.clear()
        os.environ.update(self.saved[0])
        T.AUTH = self.saved[1]

    def script(self, name, body):
        path = os.path.join(self.tmp, name)
        with open(path, "w") as fh:
            fh.write("#!/bin/sh\n" + body)
        os.chmod(path, os.stat(path).st_mode | stat.S_IXUSR)
        return path

    def listener(self, expires="2999-01-01T00:00:00Z", status=200, body=None):
        """A token command that counts its runs and numbers its tokens."""
        out = f"printf '%s\\n%s\\n' '{body}' {status}" if body else f"printf '{{\"token\":\"ghs_%s\",\"expires\":\"{expires}\"}}\\n{status}\\n' \"$n\""
        os.environ["SUPERCI_TEST"] = "1"
        os.environ["SUPERCI_APP_TOKEN_CMD"] = self.script("listener", f"n=$(($(cat {self.count} 2>/dev/null || echo 0)+1)); echo $n > {self.count}\n{out}\n")
        def minted():
            if not os.path.exists(self.count):
                return 0
            with open(self.count) as fh:
                return int(fh.read())

        return minted

    def calls(self):
        with open(self.log) as fh:
            return [line.split("\t")[0] for line in fh]

    def test_a_github_read_carries_the_app_token_in_its_environment_and_the_token_is_minted_once(self):
        minted = self.listener()
        gh = T.GitHub("o/r", T.Cache(os.path.join(self.tmp, "cache")))
        gh.api("actions/runs/1/jobs", keep=False)
        gh.api("actions/runs/2/jobs", keep=False)
        self.assertEqual(self.calls(), ["ghs_1", "ghs_1"])
        self.assertEqual(minted(), 1, "a token with a day left is minted once, not for each call")

    def test_a_token_near_expiry_is_replaced_before_it_is_used(self):
        soon = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(time.time() + 120))
        minted = self.listener(expires=soon)
        gh = T.GitHub("o/r", T.Cache(os.path.join(self.tmp, "cache")))
        gh.api("actions/runs/1/jobs", keep=False)
        gh.api("actions/runs/2/jobs", keep=False)
        self.assertEqual(self.calls(), ["ghs_1", "ghs_2"])
        self.assertEqual(minted(), 2)

    def test_no_token_refuses_before_any_gh_call_and_names_the_override(self):
        self.assertTrue(hasattr(T.apptoken, "AppTokenRefused"), "a refusal is its own exception, which no skip-this-run catch of SystemExit swallows")
        self.listener(status=502, body="installation token: 422 Unprocessable Entity: permissions not granted")
        with self.assertRaises(T.apptoken.AppTokenRefused) as raised:
            T.GitHub("o/r", T.Cache(os.path.join(self.tmp, "cache"))).api("actions/runs/1/jobs", keep=False)
        self.assertIn("refusing: no GitHub App token", str(raised.exception))
        self.assertIn("permissions not granted", str(raised.exception), "the listener's reason is shown")
        self.assertIn("--personal-token", str(raised.exception))
        self.assertFalse(os.path.exists(self.log), "gh was never run")

    def test_the_command_exits_2_with_the_reason(self):
        self.listener(status=502, body="installation token: 422 Unprocessable Entity: permissions not granted")
        err = io.StringIO()
        with contextlib.redirect_stderr(err), self.assertRaises(SystemExit) as raised:
            T.main(["--cache", os.path.join(self.tmp, "cache"), "critical-path", "--run", "1"])
        self.assertEqual(raised.exception.code, 2)
        self.assertIn("refusing: no GitHub App token", err.getvalue())
        self.assertFalse(os.path.exists(self.log))

    def test_the_personal_override_leaves_the_environment_alone(self):
        self.listener(status=502, body="down")
        T.AUTH.personal = True
        with contextlib.redirect_stderr(io.StringIO()):
            T.GitHub("o/r", T.Cache(os.path.join(self.tmp, "cache"))).api("actions/runs/1/jobs", keep=False)
        self.assertEqual(self.calls(), ["UNSET"])

    def test_a_malformed_answer_is_no_token_and_never_echoes_what_it_got(self):
        self.listener(body='{"token":"ghs_secret_in_a_bad_shape"}')
        with self.assertRaises(T.apptoken.NoAppToken) as raised:
            T.apptoken.mint()
        self.assertNotIn("ghs_secret", str(raised.exception))

    def test_the_token_command_hook_is_ignored_without_the_test_flag(self):
        marker = os.path.join(self.tmp, "ran")
        os.environ["SUPERCI_APP_TOKEN_CMD"] = self.script("hook", f"touch {marker}\n")
        os.environ.pop("SUPERCI_TEST", None)
        cmd = T.apptoken._command()
        self.assertNotIn(os.environ["SUPERCI_APP_TOKEN_CMD"], cmd, "the hook was honoured without SUPERCI_TEST=1")
        self.assertTrue(cmd[0].endswith("deploy/ssh.sh"), cmd)
        os.environ["SUPERCI_TEST"] = "1"
        self.assertEqual(T.apptoken._command(), [os.environ["SUPERCI_APP_TOKEN_CMD"]], "and with the flag it is")

    def test_a_good_answer_followed_by_a_failed_exit_is_not_quoted_in_the_message(self):
        os.environ["SUPERCI_TEST"] = "1"
        os.environ["SUPERCI_APP_TOKEN_CMD"] = self.script("leaky", "printf '{\"token\":\"ghs_leak\",\"expires\":\"2999-01-01T00:00:00Z\"}\\n200\\n'\necho 'ssh: connection reset' >&2\nexit 255\n")
        with self.assertRaises(T.apptoken.NoAppToken) as raised:
            T.apptoken.mint()
        self.assertNotIn("ghs_leak", str(raised.exception))
        self.assertIn("exit 255", str(raised.exception))

    def test_any_token_shaped_text_in_a_refusal_is_removed(self):
        os.environ["SUPERCI_TEST"] = "1"
        os.environ["SUPERCI_APP_TOKEN_CMD"] = self.script("odd", "printf 'oops ghs_abc123 in a 502 body\\n502\\n'\n")
        with self.assertRaises(T.apptoken.NoAppToken) as raised:
            T.apptoken.mint()
        self.assertNotIn("ghs_abc123", str(raised.exception))

    def scoped_listener(self, status="200"):
        """A token command that numbers tokens by the scope asked for (SUPERCI_TOKEN_SCOPE) and counts each scope's asks."""
        os.environ["SUPERCI_TEST"] = "1"
        asks = os.path.join(self.tmp, "scope-asks")
        os.environ["SUPERCI_APP_TOKEN_CMD"] = self.script("scoped", f"echo \"$SUPERCI_TOKEN_SCOPE\" >> {asks}\n"
                                                          f"if [ \"$SUPERCI_TOKEN_SCOPE\" = variables ] && [ {status} != 200 ]; then printf 'lacks \"actions_variables: write\"\\n{status}\\n'; exit 0; fi\n"
                                                          "printf '{\"token\":\"ghs_%s\",\"expires\":\"2999-01-01T00:00:00Z\"}\\n200\\n' \"$SUPERCI_TOKEN_SCOPE\"\n")

        def scopes():
            with open(asks) as fh:
                return fh.read().split()

        return scopes

    def test_each_scope_has_its_own_token_held_apart_from_the_others(self):
        scopes = self.scoped_listener()
        auth = T.apptoken.Auth()
        self.assertEqual([auth.token(), auth.token("variables"), auth.token("contents"), auth.token("variables"), auth.token()],
                         ["ghs_actions", "ghs_variables", "ghs_contents", "ghs_variables", "ghs_actions"])
        self.assertEqual(scopes(), ["actions", "variables", "contents"], "each scope is asked for once")

    def test_a_gh_call_carries_the_token_of_the_scope_it_names(self):
        self.scoped_listener()
        T.AUTH.gh(["api", "x"], scope="variables")
        T.AUTH.gh(["api", "y"], scope="contents")
        T.AUTH.gh(["api", "z"])
        self.assertEqual(self.calls(), ["ghs_variables", "ghs_contents", "ghs_actions"])

    def test_a_refused_scope_refuses_with_the_listeners_reason_and_names_the_scope(self):
        self.scoped_listener(status="422")
        with self.assertRaises(T.apptoken.AppTokenRefused) as raised:
            T.AUTH.gh(["api", "x"], scope="variables")
        self.assertIn("refusing: no GitHub App token for variables (Variables: read and write)", str(raised.exception))
        self.assertIn('actions_variables: write', str(raised.exception))
        self.assertIn("scope=variables", str(raised.exception))
        self.assertFalse(os.path.exists(self.log), "gh was never run")

    def test_an_unknown_scope_is_never_asked_for(self):
        self.scoped_listener()
        with self.assertRaises(ValueError):
            T.apptoken.mint("administration")

    def test_the_command_line_prints_one_scopes_token_and_exits_3_when_the_permission_is_missing(self):
        self.scoped_listener()
        env = dict(os.environ)
        ok = subprocess.run([sys.executable, "-B", T.apptoken.__file__, "variables"], env=env, capture_output=True, text=True)
        self.assertEqual((ok.returncode, ok.stdout), (0, "ghs_variables"), ok)
        bad = subprocess.run([sys.executable, "-B", T.apptoken.__file__, "administration"], env=env, capture_output=True, text=True)
        self.assertEqual(bad.returncode, 2, "an unknown scope is a usage error")
        self.scoped_listener(status="422")
        gone = subprocess.run([sys.executable, "-B", T.apptoken.__file__, "variables"], env=dict(os.environ), capture_output=True, text=True)
        self.assertEqual(gone.returncode, 3, "a missing permission is permanent: the canary does not retry it")
        self.assertIn("refusing: no GitHub App token for variables", gone.stderr)
        self.scoped_listener(status="502")
        flaky = subprocess.run([sys.executable, "-B", T.apptoken.__file__, "variables"], env=dict(os.environ), capture_output=True, text=True)
        self.assertEqual(flaky.returncode, 2, "any other refusal may pass")

    def test_the_only_named_exception_is_the_rollbacks_write(self):
        self.assertEqual(set(T.apptoken.EXCEPTIONS), {"SUPERCI_JOBS write"})

    def test_only_a_named_exception_may_use_the_personal_token_by_name(self):
        with self.assertRaises(ValueError):
            T.AUTH.env("anything-else")


if __name__ == "__main__":
    unittest.main()
