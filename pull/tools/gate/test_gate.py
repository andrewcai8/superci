import collections
import contextlib
import io
import json
import os
import subprocess
import sys
import tempfile
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import gate as G  # noqa: E402

T = G.T


def obs(wall, queue=2.0, ok=True, fleet="superci", run=None, sha="a", branch="x", job_id=None):
    n = next(_ids)
    return G.Obs(job_id or n, run or n, sha, branch, fleet, ok, queue, wall)


_ids = iter(range(1, 10 ** 6))


def rows_by_name(rows):
    return {r.name: r for r in rows}


def spread_around(centre, half, n=20):
    """n samples centred on `centre`, IQR about half the width: a tight sample that leaves a verdict out of the noise."""
    return [centre + half * (2 * i / (n - 1) - 1) for i in range(n)]


def fleet(walls, queues=None, **kw):
    queues = queues or [2.0] * len(walls)
    return [obs(w, q, **kw) for w, q in zip(walls, queues)]


B = fleet(spread_around(100, 10, 40), spread_around(3, 1, 40), fleet="blacksmith")  # wall p50 100, p90 ~118; queue p50 3, p90 ~3.9


def judge(S, ceiling=200.0, unexplained=(0.5,) * 10, base=B):
    return rows_by_name(G.evaluate_job(S, base, ceiling, list(unexplained)))


def printed(rows, name):
    with contextlib.redirect_stdout(io.StringIO()):
        return G.print_rows("t", rows, name)


class Gate1(unittest.TestCase):
    def good(self, **kw):
        return fleet(spread_around(80, 2, 10), spread_around(1, 0.2, 10), **kw)

    def test_a_job_at_parity_passes_every_row(self):
        rows = judge(self.good())
        self.assertEqual({n: r.status for n, r in rows.items()}, {n: G.PASS for n in rows}, [r for r in rows.values() if r.status != G.PASS])
        self.assertEqual(len(rows), 7)

    def test_n_counts_runs_not_shards(self):
        shards = [obs(80, run=1) for _ in range(30)]
        self.assertEqual(judge(shards)["n (superci runs)"].status, G.FAIL)
        self.assertEqual(judge(shards)["n (superci runs)"].value, "1")
        self.assertEqual(judge(fleet([80] * 5))["n (superci runs)"].status, G.PASS)

    def test_wall_p50_over_blacksmiths_fails_and_just_under_it_passes(self):
        self.assertEqual(judge(fleet(spread_around(130, 2, 10)))["wall p50"].status, G.FAIL)
        self.assertEqual(judge(fleet(spread_around(99, 0.5, 10)))["wall p50"].status, G.PASS)

    def test_wall_p90_is_held_to_the_smaller_of_ten_percent_over_and_the_ceiling(self):
        slow_tail = fleet([80] * 8 + [150, 150])  # p90 150: over 1.1 x B p90 (~130)
        self.assertEqual(judge(slow_tail, ceiling=None)["wall p90"].status, G.FAIL)
        tail_120 = fleet([80] * 8 + [115, 115])  # p90 115 < 1.1 x B p90 (118.8)
        self.assertEqual(judge(tail_120, ceiling=None)["wall p90"].status, G.PASS)
        self.assertEqual(judge(tail_120, ceiling=100.0)["wall p90"].status, G.FAIL)
        self.assertIn("ceiling 100", judge(tail_120, ceiling=100.0)["wall p90"].threshold)

    def test_queue_wait_p50_and_p90(self):
        self.assertEqual(judge(fleet([80] * 10, [20.0] * 10))["queue wait p50"].status, G.FAIL)
        self.assertEqual(judge(fleet([80] * 10, [1.0] * 10))["queue wait p50"].status, G.PASS)
        within = fleet([80] * 10, [1.0] * 8 + [6.5, 6.5])  # B p90 ~3.9 + 3 s = ~6.9
        self.assertEqual(judge(within)["queue wait p90"].status, G.PASS)
        beyond = fleet([80] * 10, [1.0] * 8 + [9.0, 9.0])
        self.assertEqual(judge(beyond)["queue wait p90"].status, G.FAIL)

    def test_failures_pass_within_blacksmiths_rate_plus_one_and_fail_beyond(self):
        S = self.good() + [obs(30, ok=False, sha=f"s{i}") for i in range(1)]
        self.assertEqual(judge(S)["failures (superci)"].status, G.PASS)
        S = self.good() + [obs(30, ok=False, sha=f"s{i}") for i in range(3)]
        self.assertEqual(judge(S)["failures (superci)"].status, G.FAIL)

    def test_a_failure_blacksmith_or_main_has_on_the_same_commit_is_not_superci_s(self):
        fails = [obs(30, ok=False, sha=f"s{i}") for i in range(3)]
        bad_b = B + [obs(30, ok=False, sha="s0", fleet="blacksmith"), obs(30, ok=False, sha="s1", fleet="blacksmith")]
        row = judge(self.good() + fails, base=bad_b)["failures (superci)"]
        self.assertEqual(row.value, "1 of 13")
        self.assertEqual(row.status, G.PASS)
        self.assertIn("2 more", row.note)
        on_main = obs(30, ok=False, sha="s2", fleet="superci", branch="main")
        row = judge(self.good() + fails[:2] + [obs(30, ok=False, sha="s2"), on_main], base=B)["failures (superci)"]
        self.assertEqual(row.value, "3 of 14", "a failure on main counts for the branch run, not for itself")

    def test_unexplained_time_must_be_under_two_seconds_at_p50(self):
        self.assertEqual(judge(self.good(), unexplained=[1.0] * 9)["unexplained p50 (timeline)"].status, G.PASS)
        self.assertEqual(judge(self.good(), unexplained=[2.0] * 9)["unexplained p50 (timeline)"].status, G.FAIL)
        self.assertEqual(judge(self.good(), unexplained=[])["unexplained p50 (timeline)"].status, G.FAIL)

    def test_a_pass_inside_the_noise_is_not_a_pass(self):
        noisy = fleet([60, 62, 64, 70, 75, 95, 96, 97, 98, 99])  # p50 85 against 100: margin 15 but IQR 31
        row = judge(noisy)["wall p50"]
        self.assertEqual(row.status, G.NOISE)
        self.assertIn("add runs", row.note)
        self.assertEqual(printed(G.evaluate_job(noisy, B, None, [0.5] * 10), "gate 1"), 1, "noise only: inconclusive, still exit 1")

    def test_no_baseline_fails_the_comparisons_instead_of_passing_them(self):
        rows = judge(self.good(), base=[])
        for n in ("wall p50", "wall p90", "queue wait p50", "queue wait p90"):
            self.assertEqual(rows[n].status, G.FAIL, n)

    def test_the_ceiling_comes_from_the_ci_limits_row(self):
        tsv = "# c\nlint-test-backend.yml\t*\t1370\tn\nlint-test-backend.yml\tverify-tools\t625\tp50 99\nlint-test-backend.yml\ttest-go\t130\tp50 63\n"
        limits = G.parse_limits(tsv)
        self.assertEqual(limits[("lint-test-backend.yml", "verify-tools")], 625.0)
        self.assertEqual(limits[("lint-test-backend.yml", "test-go")], 130.0)

    def test_a_matrix_leg_belongs_to_its_job(self):
        self.assertTrue(G.matches("verify-tools", "verify-tools"))
        self.assertTrue(G.matches("hermetic (3)", "hermetic"))
        self.assertFalse(G.matches("hermetic-infra", "hermetic"))

    def test_the_exit_code_is_zero_only_on_pass(self):
        rows = G.evaluate_job(self.good(), B, 200.0, [0.5] * 10)
        self.assertEqual(printed(rows, "gate 1"), 0)
        slow = G.evaluate_job(fleet(spread_around(150, 2, 10)), B, 200.0, [0.5] * 10)
        self.assertEqual(printed(slow, "gate 1"), 1)


def scale(**kw):
    base = dict(replayed=100, completed=100, stranded=[], lost=0, waits=spread_around(3, 1, 100), window_use=[(1000.0, 400.0), (1060.0, 700.0)], org_known=[1.0, 1.0],
                claims={f"i-{n}" for n in range(10)}, issued=collections.Counter({f"i-{n}": 1 for n in range(10)}), claim_taken=collections.Counter({f"i-{n}": 1 for n in range(10)}),
                claim_refused=0, launched=["spot"] * 19 + ["on-demand"], orphans=[(2000.0, 0.0)], orphans_due=1990.0,
                alarms={"superci-pull-orphans": "OK", "superci-pull-jobs-stalled": "OK"}, queuewait_samples=100.0)
    base.update(kw)
    return G.Scale(**base)


def judge_scale(**kw):
    return rows_by_name(G.evaluate_scale(scale(**kw)))


class Gate2(unittest.TestCase):
    def test_a_clean_replay_passes_every_row(self):
        rows = judge_scale()
        self.assertEqual({n: r.status for n, r in rows.items()}, {n: G.PASS for n in rows})
        self.assertEqual(len(rows), 9)

    def test_completion_needs_every_replayed_job_and_none_lost_with_a_machine(self):
        self.assertEqual(judge_scale(completed=99, stranded=[("j", "failure")])["completion"].status, G.FAIL)
        self.assertEqual(judge_scale(lost=1)["completion"].status, G.FAIL)
        self.assertEqual(judge_scale(replayed=0, completed=0)["completion"].status, G.FAIL)

    def test_queue_wait_p50_and_p90_against_the_blacksmith_reference(self):
        self.assertEqual(judge_scale(waits=[7.0] * 100)["queue wait p50"].status, G.FAIL)
        self.assertEqual(judge_scale(waits=[1.0] * 85 + [11.0] * 15)["queue wait p90"].status, G.FAIL)
        self.assertEqual(judge_scale(waits=[1.0] * 85 + [9.0] * 15)["queue wait p90"].status, G.PASS)
        self.assertEqual(judge_scale(waits=[])["queue wait p50"].status, G.FAIL)

    def test_registrations_stay_under_eighty_percent_of_the_cap_in_every_window(self):
        self.assertEqual(judge_scale(window_use=[(1000.0, 1200.0)])["org registrations per 5 min"].status, G.PASS)
        row = judge_scale(window_use=[(1000.0, 400.0), (1060.0, 1201.0)])["org registrations per 5 min"]
        self.assertEqual(row.status, G.FAIL)
        self.assertIn("1201", row.value)
        self.assertEqual(judge_scale(org_known=[1.0, 0.0])["org registrations per 5 min"].status, G.FAIL)
        self.assertEqual(judge_scale(window_use=[])["org registrations per 5 min"].status, G.FAIL)

    def test_one_runner_per_machine(self):
        key = "one runner per machine"
        self.assertEqual(judge_scale(claims={f"i-{n}" for n in range(11)})[key].status, G.FAIL)
        self.assertIn("claimed not registered ['i-10']", judge_scale(claims={f"i-{n}" for n in range(11)})[key].note)
        self.assertEqual(judge_scale(issued=collections.Counter({**{f"i-{n}": 1 for n in range(9)}}))[key].status, G.FAIL)
        twice = collections.Counter({f"i-{n}": 1 for n in range(10)})
        twice["i-3"] = 2
        self.assertEqual(judge_scale(issued=twice)[key].status, G.FAIL)
        taken_twice = collections.Counter({f"i-{n}": 1 for n in range(10)})
        taken_twice["i-4"] = 2
        self.assertEqual(judge_scale(claim_taken=taken_twice)[key].status, G.FAIL)
        self.assertEqual(judge_scale(claim_refused=2)[key].status, G.PASS, "a refused duplicate grant is the fence working")
        self.assertEqual(judge_scale(claims=set(), issued=collections.Counter())[key].status, G.FAIL, "no claims at all proves nothing")

    def test_orphans_ten_minutes_after(self):
        self.assertEqual(judge_scale(orphans=[(2000.0, 0.0), (2060.0, 2.0)])["orphans 10 min after"].status, G.FAIL)
        none_yet = judge_scale(orphans=[])["orphans 10 min after"]
        self.assertEqual(none_yet.status, G.FAIL)
        self.assertIn("no Orphans datapoint", none_yet.note)

    def test_on_demand_share_at_most_ten_percent(self):
        self.assertEqual(judge_scale(launched=["spot"] * 9 + ["on-demand"])["on-demand share"].status, G.PASS)
        self.assertEqual(judge_scale(launched=["spot"] * 8 + ["on-demand"] * 2)["on-demand share"].status, G.FAIL)
        self.assertEqual(judge_scale(launched=[])["on-demand share"].status, G.FAIL)

    def test_no_alarm_in_alarm_at_the_end(self):
        firing = judge_scale(alarms={"superci-pull-orphans": "ALARM", "superci-pull-jobs-stalled": "OK"})["alarms at the end"]
        self.assertEqual(firing.status, G.FAIL)
        self.assertIn("superci-pull-orphans", firing.note)
        self.assertEqual(judge_scale(alarms={"a": "INSUFFICIENT_DATA"})["alarms at the end"].status, G.PASS)
        self.assertEqual(judge_scale(alarms={})["alarms at the end"].status, G.FAIL)

    def test_queue_wait_must_be_published(self):
        self.assertEqual(judge_scale(queuewait_samples=0.0)["QueueWait published"].status, G.FAIL)
        self.assertEqual(judge_scale(queuewait_samples=None)["QueueWait published"].status, G.FAIL)


class AlarmHistory(unittest.TestCase):
    def test_state_at_a_past_time_is_the_last_update_not_after_it(self):
        def run(args_by_cmd, at, now=10_000.0):
            def fake(profile, *args):
                if args[1] == "describe-alarms":
                    return {"MetricAlarms": [{"AlarmName": "superci-pull-x", "StateValue": "OK"}]}
                return {"AlarmHistoryItems": args_by_cmd}
            old, G.T.aws = G.T.aws, fake
            try:
                return G.alarm_states("p", at, now)
            finally:
                G.T.aws = old
        hist = [{"Timestamp": T.iso(100), "HistoryData": json.dumps({"oldState": {"stateValue": "OK"}, "newState": {"stateValue": "ALARM"}})},
                {"Timestamp": T.iso(500), "HistoryData": json.dumps({"oldState": {"stateValue": "ALARM"}, "newState": {"stateValue": "OK"}})}]
        self.assertEqual(run(hist, 300), {"superci-pull-x": "ALARM"})
        self.assertEqual(run(hist, 600), {"superci-pull-x": "OK"})
        self.assertEqual(run(hist, 50), {"superci-pull-x": "OK"}, "before the first update it was that update's old state")


def rest(created, started, completed, labels, conclusion="success", name="test-go", run=1):
    return {"id": run * 1000 + int(created), "run_id": run, "name": name, "labels": labels, "conclusion": conclusion, "status": "completed",
            "created_at": T.iso(created), "started_at": T.iso(started) if started else None, "completed_at": T.iso(completed) if completed else None}


class Trace(unittest.TestCase):
    T0 = T.ts("2026-11-25T14:00:00Z")

    def test_linux_jobs_become_legs_on_a_superci_label_and_the_rest_are_counted(self):
        jobs = [rest(self.T0 + 5, self.T0 + 8, self.T0 + 68, ["blacksmith-4vcpu-ubuntu-2404"]),
                rest(self.T0 + 70, self.T0 + 71, self.T0 + 99, ["superci-2cpu"]),
                rest(self.T0 + 80, self.T0 + 81, self.T0 + 90, ["blacksmith-4vcpu-ubuntu-2404-arm"]),
                rest(self.T0 + 81, self.T0 + 82, self.T0 + 90, ["blacksmith-64vcpu-ubuntu-2404"]),
                rest(self.T0 + 85, self.T0 + 86, self.T0 + 90, ["macos-14"]),
                rest(self.T0 + 90, None, self.T0 + 91, ["blacksmith-4vcpu-ubuntu-2404"], conclusion="skipped"),
                rest(self.T0 - 5, self.T0 - 4, self.T0 + 30, ["blacksmith-4vcpu-ubuntu-2404"]),
                rest(self.T0 + 3600, self.T0 + 3601, self.T0 + 3700, ["blacksmith-4vcpu-ubuntu-2404"])]
        trace = G.build_trace(jobs, self.T0, 1, 60)
        self.assertEqual([(l["t"], l["label"], l["duration_s"]) for l in trace["jobs"]], [(5.0, "superci-4cpu", 60.0), (70.0, "superci-2cpu", 28.0)])
        self.assertEqual(sum(trace["dropped"].values()), 4)
        self.assertEqual(trace["start"], "2026-11-25T14:00:00Z")
        self.assertEqual(trace["bucket_s"], 60)

    def test_the_bench_drives_the_trace_bucket_by_bucket(self):
        legs = [{"t": 5.0, "label": "superci-4cpu", "duration_s": 60.0, "name": "a", "run": 1}, {"t": 12.0, "label": "superci-4cpu", "duration_s": 5000.0, "name": "b", "run": 1},
                {"t": 70.0, "label": "superci-2cpu", "duration_s": 28.0, "name": "c", "run": 2}] + [
            {"t": 130.0 + i * 0.1, "label": "superci-8cpu", "duration_s": 10.0, "name": "d", "run": 3} for i in range(300)]
        trace = {"version": 1, "start": "x", "hours": 1, "bucket_s": 60, "jobs": legs}
        bench = os.path.join(HERE, "..", "..", "bench", "workflow.py")
        with tempfile.TemporaryDirectory() as d:
            path = os.path.join(d, "trace.json")
            with open(path, "w") as fh:
                json.dump(trace, fh)
            sched = subprocess.run([sys.executable, "-I", bench, "--schedule", path], capture_output=True, text=True, check=True).stdout.split("\n")
            self.assertEqual(sched[:3], ["0 0 2", "1 60 1", "2 120 300"])
            wf = lambda i: subprocess.run([sys.executable, "-I", bench, "--trace", path, "ci/superci-load-pull-t", str(i), "900"], capture_output=True, text=True, check=True).stdout
            first = wf(0)
            self.assertIn("runs-on: superci-4cpu", first)
            self.assertIn("{i: 1, d: 60}, {i: 2, d: 900}", first, "a sleep is the real duration, capped")
            self.assertIn("branches: [ci/superci-load-pull-t]", first)
            big = wf(2)
            self.assertIn("cpu8_1:", big)
            self.assertIn("cpu8_2:", big, "a matrix holds at most 256 legs")
            self.assertEqual(big.count("{i: "), 300)


if __name__ == "__main__":
    unittest.main()
