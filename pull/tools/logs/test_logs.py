import contextlib
import io
import os
import sys
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import logs as L  # noqa: E402

T = L.T
Q = 1_000_000.0


def ev(kind, dt, **kw):
    return dict({"msg": "tl", "v": 1, "ev": kind, "t": Q + dt, "eid": f"b.{kind}.{dt}", "set": "superci-4cpu"}, **kw)


JOB = {"id": 55, "run_id": 9, "name": "unit (3)", "conclusion": "success", "status": "completed", "runner_name": "superci-4cpu-i-0abc",
       "created_at": T.iso(Q), "completed_at": T.iso(Q + 130)}


def events():
    return [ev("machine.launched", -40, inst="i-0abc", type="m8azn.xlarge", az="us-east-1b", lifecycle="spot"),
            ev("boot.start", -35, inst="i-0abc", val="", offset_ms=120),
            ev("job.started", 4, run=9, name="unit (3)", inst="i-0abc", queue_ms=4000),
            ev("job.started", 4, run=9, name="other leg", inst="i-0def"),
            ev("job.record", 130, run=9, name="unit (3)", inst="i-0abc", conclusion="success", queue_ms=4000, wall_ms=126000, set="superci-4cpu", type="m8azn.xlarge",
               lifecycle="spot", az="us-east-1b", warm=False, usd=0.0123, busy_usd=0.0101, partial=False),
            ev("machine.ended", 131, inst="i-0abc", why="completed")]


class Url(unittest.TestCase):
    def test_a_job_url_gives_repo_run_and_job(self):
        self.assertEqual(L.parse_url("https://github.com/Org/repo-x/actions/runs/123/job/456?pr=1"), ("Org/repo-x", 123, 456))

    def test_anything_else_is_refused(self):
        with self.assertRaises(SystemExit):
            L.parse_url("https://github.com/Org/repo/pull/3")


class Query(unittest.TestCase):
    def test_the_job_s_events_come_by_run_and_the_machine_s_by_instance(self):
        self.assertEqual(L.event_query(9, "i-0abc"), 'msg = "tl" and (run = 9 or inst = "i-0abc")')
        self.assertEqual(L.event_query(9, None), 'msg = "tl" and (run = 9)')


class Report(unittest.TestCase):
    def test_the_report_names_the_record_the_machine_events_and_the_missing_metrics(self):
        out = L.render(JOB, [e for e in events() if e.get("inst") != "i-0def"], ["2026 job lost with its machine i-0abc"], None, Q)
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
        e = ev("job.record", 130, run=9, name="unit (3)", conclusion="success", partial=True, set="superci-4cpu", usd=0.5)
        self.assertIn("queue and wall unknown", L.render(JOB, [e], [], None, Q))

    def test_a_record_of_another_leg_of_the_run_is_not_this_job_s(self):
        other = ev("job.record", 130, run=9, name="other leg", conclusion="failure", queue_ms=1, wall_ms=1, set="s")
        self.assertIn("record: none", L.render(JOB, [other], [], None, Q))


class EndToEnd(unittest.TestCase):
    def test_one_github_call_and_only_this_job_s_events_and_machine_s(self):
        calls, filters = [], []
        saved = (L.T.GitHub, L.T.listener_events, L.T.insights, L.read_summary)

        class GH:
            def __init__(self, repo, cache):
                calls.append(repo)

            def job(self, job_id):
                calls.append(job_id)
                return JOB

        L.T.GitHub = GH
        L.T.listener_events = lambda profile, lo, hi, where=None: (filters.append(where), events())[1]
        L.T.insights = lambda *a, **k: [{"@timestamp": "t", "@message": '{"msg":"job lost with its machine","inst":"i-0abc"}'}]
        L.read_summary = lambda profile, job_id: {"io_wait_pct_peak": 7}
        try:
            buf = io.StringIO()
            with contextlib.redirect_stdout(buf):
                L.main(["--profile", "ci-runners", "https://github.com/Org/repo/actions/runs/9/job/55"])
        finally:
            L.T.GitHub, L.T.listener_events, L.T.insights, L.read_summary = saved
        out = buf.getvalue()
        self.assertEqual(calls, ["Org/repo", 55], "one GitHub call: the job")
        self.assertEqual(filters, ['msg = "tl" and (run = 9 or inst = "i-0abc")'])
        self.assertNotIn("i-0def", out, "another leg's machine is not this job's")
        self.assertIn("io_wait_pct_peak: 7", out)
        self.assertIn("job lost with its machine", out)


if __name__ == "__main__":
    unittest.main()
