import atexit
import contextlib
import io
import os
import shutil
import sys
import tempfile
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import logs as L  # noqa: E402

T = L.T
Q = 1_000_000.0

# No test reaches the listener: the token command is a stub (honoured only under SUPERCI_TEST=1).
_STUBS = tempfile.mkdtemp()
atexit.register(shutil.rmtree, _STUBS, True)
with open(os.path.join(_STUBS, "token_ok"), "w") as _fh:
    _fh.write("#!/bin/sh\nprintf '{\"token\":\"ghs_for_the_tests\",\"expires\":\"2999-01-01T00:00:00Z\"}\\n200\\n'\n")
os.chmod(os.path.join(_STUBS, "token_ok"), 0o755)
os.environ["SUPERCI_TEST"] = "1"
os.environ["SUPERCI_APP_TOKEN_CMD"] = os.path.join(_STUBS, "token_ok")


def ev(kind, dt, **kw):
    return dict({"msg": "tl", "v": 1, "ev": kind, "t": Q + dt, "eid": f"b.{kind}.{dt}", "set": "superci-4cpu"}, **kw)


JOB = {"id": 55, "run_id": 9, "name": "unit (3)", "conclusion": "success", "status": "completed", "runner_name": "superci-4cpu-i-0abc1234567890de0",
       "created_at": T.iso(Q), "completed_at": T.iso(Q + 130)}


def events():
    return [ev("machine.launched", -40, inst="i-0abc1234567890de0", type="m8azn.xlarge", az="us-east-1b", lifecycle="spot"),
            ev("boot.start", -35, inst="i-0abc1234567890de0", val="", offset_ms=120),
            ev("job.started", 4, run=9, name="unit (3)", inst="i-0abc1234567890de0", queue_ms=4000),
            ev("job.started", 4, run=9, name="other leg", inst="i-0def1234567890de0"),
            ev("job.record", 130, run=9, name="unit (3)", inst="i-0abc1234567890de0", conclusion="success", queue_ms=4000, wall_ms=126000, set="superci-4cpu", type="m8azn.xlarge",
               lifecycle="spot", az="us-east-1b", warm=False, usd=0.0123, busy_usd=0.0101, partial=False),
            ev("machine.ended", 131, inst="i-0abc1234567890de0", why="completed")]


class Url(unittest.TestCase):
    def test_a_job_url_gives_repo_run_and_job(self):
        self.assertEqual(L.parse_url("https://github.com/Org/repo-x/actions/runs/123/job/456?pr=1"), ("Org/repo-x", 123, 456))

    def test_anything_else_is_refused(self):
        with self.assertRaises(SystemExit):
            L.parse_url("https://github.com/Org/repo/pull/3")


class Query(unittest.TestCase):
    def test_the_job_s_events_come_by_run_and_the_machine_s_by_instance(self):
        self.assertEqual(L.event_query(9, "i-0abc1234567890de0"), 'msg = "tl" and (run = 9 or inst = "i-0abc1234567890de0")')
        self.assertEqual(L.event_query(9, None), 'msg = "tl" and (run = 9)')


class Report(unittest.TestCase):
    def test_the_report_names_the_record_the_machine_events_and_the_missing_metrics(self):
        out = L.render(JOB, [e for e in events() if e.get("inst") != "i-0def1234567890de0"], ["2026 job lost with its machine i-0abc1234567890de0"], None, Q)
        self.assertIn("record: success  queue 4.0 s  wall 126.0 s  set superci-4cpu  m8azn.xlarge spot us-east-1b  cold  cost $0.0123 ($0.0101 while the job ran)", out)
        for name in ("machine.launched", "boot.start", "job.started", "job.record", "machine.ended"):
            self.assertIn(name, out)
        self.assertIn("-40.0 s", out)
        self.assertIn("the listener's other log lines naming the machine:", out)
        self.assertIn("host metrics: none uploaded", out)

    def test_a_summary_is_printed_and_an_unfinished_job_has_no_record(self):
        out = L.render(dict(JOB, conclusion=None, status="in_progress"), events()[:3], [], {"cpu_peak_pct": 91, "cold_read_mb": 812}, Q)
        self.assertIn("record: none", out)
        self.assertIn("cpu_peak_pct: 91", out)
        self.assertIn("cold_read_mb: 812", out)

    def test_a_partial_record_does_not_print_zero_times(self):
        e = ev("job.record", 130, run=9, name="unit (3)", inst="i-0abc1234567890de0", conclusion="success", partial=True, set="superci-4cpu", usd=0.5)
        self.assertIn("queue and wall unknown", L.render(JOB, [e], [], None, Q))

    def test_a_record_of_another_leg_of_the_run_is_not_this_job_s(self):
        other = ev("job.record", 130, run=9, name="other leg", conclusion="failure", queue_ms=1, wall_ms=1, set="s")
        self.assertIn("record: none", L.render(JOB, [other], [], None, Q))


class EndToEnd(unittest.TestCase):
    def test_one_github_call_and_only_this_job_s_events_and_machine_s(self):
        calls, filters = [], []
        saved = (L.T.GitHub, L.T.listener_events, L.query, L.read_summary)

        class GH:
            def __init__(self, repo, cache):
                calls.append(repo)

            def api(self, path, keep=True):
                calls.append((path, keep))
                return JOB

        L.T.GitHub = GH
        L.T.listener_events = lambda profile, lo, hi, where=None: (filters.append(where), events())[1]
        L.query = lambda *a, **k: [{"@timestamp": "t", "@message": '{"msg":"job lost with its machine","inst":"i-0abc1234567890de0"}'}]
        L.read_summary = lambda profile, inst: {"io_wait_pct_peak": 7} if inst == "i-0abc1234567890de0" else None
        try:
            buf = io.StringIO()
            with contextlib.redirect_stdout(buf):
                L.main(["--profile", "ci-runners", "https://github.com/Org/repo/actions/runs/9/job/55"])
        finally:
            L.T.GitHub, L.T.listener_events, L.query, L.read_summary = saved
        out = buf.getvalue()
        self.assertEqual(calls, ["Org/repo", ("actions/jobs/55", False)], "one GitHub call: the job, not cached")
        self.assertEqual(filters, ['msg = "tl" and (run = 9 or inst = "i-0abc1234567890de0")'])
        self.assertNotIn("i-0def1234567890de0", out, "another leg's machine is not this job's")
        self.assertIn("io_wait_pct_peak: 7", out)
        self.assertIn("job lost with its machine", out)


class Attempts(unittest.TestCase):
    def test_a_rerun_shows_its_own_machine_and_its_own_record_not_the_first_attempt_s(self):
        first = [ev("machine.launched", -900, inst="i-0111aaaa1111bbbb1"), ev("job.assigned", -800, run=9, name="unit (3)"), ev("job.started", -700, run=9, name="unit (3)", inst="i-0111aaaa1111bbbb1"),
                 ev("job.record", -600, run=9, name="unit (3)", inst="i-0111aaaa1111bbbb1", conclusion="failure", queue_ms=1, wall_ms=100000, set="s")]
        second = [ev("machine.launched", -40, inst="i-0abc1234567890de0"), ev("job.assigned", -2, run=9, name="unit (3)"), ev("job.started", 4, run=9, name="unit (3)", inst="i-0abc1234567890de0"),
                  ev("job.record", 130, run=9, name="unit (3)", inst="i-0abc1234567890de0", conclusion="success", queue_ms=4000, wall_ms=126000, set="s")]
        got = L.select(first + second, 9, "unit (3)", "i-0abc1234567890de0")
        self.assertEqual({e.get("inst") for e in got if e.get("inst")}, {"i-0abc1234567890de0"})
        self.assertEqual([e["ev"] for e in got if not e.get("inst")], ["job.assigned"])
        self.assertEqual([e["t"] for e in got if e["ev"] == "job.assigned"], [Q - 2], "the assignment before this machine took the job, not the first attempt's")
        out = L.render(JOB, got, [], None, Q)
        self.assertIn("record: success", out)
        self.assertNotIn("failure", out)

    def test_a_job_not_yet_started_has_no_machine_and_takes_the_runs_events_of_its_name(self):
        got = L.select(events(), 9, "unit (3)", None)
        self.assertEqual({e["name"] for e in got}, {"unit (3)"})


class Clocks(unittest.TestCase):
    def test_a_machine_mark_is_printed_on_the_listener_s_clock(self):
        mark = ev("boot.start", -35, inst="i-0abc1234567890de0", offset_ms=1700)
        self.assertAlmostEqual(L.at(mark), Q - 35 + 1.7)
        self.assertEqual(L.at(ev("job.started", 4, run=9)), Q + 4)
        self.assertIn("-33.3 s", L.render(JOB, [mark], [], None, Q))


class Summary(unittest.TestCase):
    def read(self, raised=None, returned=None):
        saved = L.T.aws

        def aws(*a):
            if raised:
                raise raised
            return returned
        L.T.aws = aws
        try:
            return L.read_summary("p", "i-0abc1234567890de0")
        finally:
            L.T.aws = saved

    def test_a_missing_object_is_none_and_a_failure_to_read_is_said(self):
        self.assertIsNone(self.read(raised=SystemExit("aws s3 cp: An error occurred (404) when calling the HeadObject operation: Key does not exist")))
        self.assertIn("unreadable", self.read(raised=SystemExit("aws s3 cp: AccessDenied"))) 

    def test_only_a_json_object_is_taken_from_a_machine_that_a_job_is_root_on(self):
        self.assertEqual(self.read(returned={"cpu": 1}), {"cpu": 1})
        self.assertIn("unreadable", self.read(raised=ValueError("x")))
        self.assertIn("unreadable", self.read(returned=["a"]))
        self.assertIn("unreadable", self.read(returned="text"))


class Instances(unittest.TestCase):
    def test_a_runner_name_that_names_no_instance_is_refused_before_it_reaches_a_query(self):
        saved = L.T.GitHub
        L.T.GitHub = lambda repo, cache: type("G", (), {"api": lambda self, path, keep=True: dict(JOB, runner_name='x-i-0abc1234567890de0" or 1=1 #')})()
        try:
            with self.assertRaises(SystemExit):
                L.main(["https://github.com/Org/repo/actions/runs/9/job/55"])
        finally:
            L.T.GitHub = saved


class Cap(unittest.TestCase):
    def test_the_machine_s_lines_are_one_query_capped_at_fifty_never_split(self):
        calls = []
        saved = (L.T.aws, L.time.sleep)

        def aws(profile, *args):
            calls.append(args)
            if args[1] == "start-query":
                return {"queryId": "q"}
            return {"status": "Complete", "results": [[{"field": "@timestamp", "value": "t"}, {"field": "@message", "value": "x"}]] * 50}
        L.T.aws, L.time.sleep = aws, lambda s: None
        try:
            lines = L.machine_lines("p", "i-0abc1234567890de0", 0, 3 * 3600)
        finally:
            L.T.aws, L.time.sleep = saved
        starts = [c for c in calls if c[1] == "start-query"]
        self.assertEqual((len(starts), len(lines)), (1, 50), "a full answer is not a reason to split the window")
        self.assertTrue(starts[0][-1].endswith("| limit 50"))


class GitHubAccess(unittest.TestCase):
    """logs.py reads one job from GitHub: on the App's budget, or refused, or on the personal one when asked, like gate and timeline."""
    URL = "https://github.com/Org/repo/actions/runs/9/job/55"

    def setUp(self):
        self.tmp = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, self.tmp, True)
        self.log = os.path.join(self.tmp, "gh.log")
        os.makedirs(os.path.join(self.tmp, "bin"))
        for name, body in {"gh": 'printf "%s\\t%s\\n" "${GH_TOKEN-UNSET}" "$*" >> "$GHLOG"\nprintf "%s\\n" "${GH_SHIM_KEEP_TOKEN-UNSET}" >> "$GHLOG.keep"\necho \'{"id":55,"run_id":9,"name":"unit","runner_name":null,"created_at":"2026-11-25T17:00:00Z"}\'\n',
                           "aws": 'case "$*" in *get-caller-identity*) echo 243891825325 ;; *) echo "stub aws" >&2; exit 1 ;; esac\n',
                           "down": "echo 'ssh: timed out' >&2\nexit 255\n"}.items():
            path = os.path.join(self.tmp, "bin", name)
            with open(path, "w") as fh:
                fh.write("#!/bin/sh\n" + body)
            os.chmod(path, 0o755)
        saved, saved_auth = dict(os.environ), T.AUTH
        for k in ("GH_TOKEN", "GITHUB_TOKEN"):
            os.environ.pop(k, None)
        os.environ.update(PATH=os.path.join(self.tmp, "bin") + os.pathsep + os.environ["PATH"], GHLOG=self.log, SUPERCI_TEST="1",
                          SUPERCI_APP_TOKEN_CMD=os.path.join(self.tmp, "bin", "down"))
        T.AUTH = T.apptoken.Auth()

        def restore():
            os.environ.clear()
            os.environ.update(saved)
            T.AUTH = saved_auth
        self.addCleanup(restore)

    def run_logs(self, *argv):
        out, err, code = io.StringIO(), io.StringIO(), None
        try:
            with contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
                L.main([self.URL, "--cache", os.path.join(self.tmp, "cache"), *argv])
        except SystemExit as e:
            code = e.code
        return code, out.getvalue(), err.getvalue()

    def calls(self):
        if not os.path.exists(self.log):
            return []
        with open(self.log) as fh:
            return [line.split("\t")[0] for line in fh]

    def test_without_an_app_token_it_refuses_before_any_gh_call(self):
        code, out, err = self.run_logs()
        self.assertEqual(code, 2, (out, err))
        self.assertIn("refusing: no GitHub App token", err)
        self.assertIn("--personal-token", err)
        self.assertEqual(self.calls(), [])

    def test_the_override_is_announced_and_reads_on_the_callers_own_credential(self):
        code, out, err = self.run_logs("--personal-token")
        self.assertIn("spending the shared personal", err)
        self.assertNotIn("refusing", err)
        self.assertEqual(self.calls(), ["UNSET"])

    def keeps(self):
        with open(self.log + ".keep") as fh:
            return fh.read().split()

    def test_the_job_read_on_the_app_token_asks_a_host_wrapper_to_keep_it(self):
        tok = os.path.join(self.tmp, "bin", "tok")
        with open(tok, "w") as fh:
            fh.write("#!/bin/sh\nprintf '{\"token\":\"ghs_x\",\"expires\":\"2999-01-01T00:00:00Z\"}\\n200\\n'\n")
        os.chmod(tok, 0o755)
        os.environ["SUPERCI_APP_TOKEN_CMD"] = tok
        code, out, err = self.run_logs()
        self.assertEqual(self.calls(), ["ghs_x"], (code, out, err))
        self.assertEqual(self.keeps(), ["1"])

    def test_the_personal_job_read_does_not_set_it(self):
        self.run_logs("--personal-token")
        self.assertEqual(self.keeps(), ["UNSET"])


if __name__ == "__main__":
    unittest.main()
