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


def obs(wall, queue=2.0, ok=True, fleet="superci", run=None, sha="a", branch="x", job_id=None, timed_out=False, attempt=1):
    n = next(_ids)
    return G.Obs(job_id or n, run or n, sha, branch, fleet, ok, queue, wall, timed_out, attempt)


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
        main_on_blacksmith = obs(30, ok=False, sha="s2", fleet="blacksmith", branch="main")
        row = judge(self.good() + fails, base=B + [main_on_blacksmith])["failures (superci)"]
        self.assertEqual(row.value, "2 of 13", "main's run on Blacksmith failed the same commit")

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


class Sources(unittest.TestCase):
    def test_a_sample_is_evenly_spread_and_the_same_on_every_rerun(self):
        items = list(range(1000))
        picked = G.sample(items, 100)
        self.assertEqual(picked, G.sample(items, 100))
        self.assertEqual((picked[0], picked[1], picked[-1]), (0, 10, 990))
        self.assertEqual(G.sample([1, 2], 100), [1, 2])

    def test_runs_the_listing_could_not_read_fail_the_verdict_unless_allowed(self):
        class Gh:
            skipped = [7, 9]
        self.assertEqual(G.unread(Gh)[0].status, G.FAIL)
        self.assertEqual(G.unread(Gh, 2)[0].status, G.PASS)
        self.assertEqual(G.unread(type("Clean", (), {})), [])

    def test_the_noise_floor_is_run_to_run_not_leg_to_leg(self):
        legs = [obs(w, run=r) for r, ws in enumerate(([10, 100, 200], [11, 101, 201], [9, 99, 199], [10, 100, 200]), 1) for w in ws]
        self.assertLess(G.run_spread(legs, lambda o: o.wall), 2.0)
        self.assertGreater(G.iqr([o.wall for o in legs]), 80)


class RecordedCanary(unittest.TestCase):
    """The verify-tools legs of Nov 25, as GitHub's API returned them: superci's canary against Blacksmith's day before."""

    @classmethod
    def setUpClass(cls):
        with open(os.path.join(HERE, "fixtures", "verify-tools-canary.json")) as fh:
            cls.rec = json.load(fh)
        cls.S = [o for o in (G.is_obs(j, {}) for j in cls.rec["superci"]) if o]
        cls.B = [o for o in (G.is_obs(j, {}) for j in cls.rec["blacksmith"]) if o]

    def test_the_recorded_canary_fails_on_wall_and_queue_wait_as_measured(self):
        rows = rows_by_name(G.evaluate_job(self.S, self.B, 625.0, [22.0] * len(self.S)))
        for name in ("wall p50", "wall p90", "queue wait p50", "queue wait p90", "unexplained p50 (timeline)"):
            self.assertEqual(rows[name].status, G.FAIL, name)
        self.assertEqual(rows["n (superci runs)"].status, G.PASS)
        self.assertGreater(T.q([o.wall for o in self.S], 50), 2 * T.q([o.wall for o in self.B], 50))

    def test_the_same_legs_at_a_third_of_the_time_pass(self):
        fast = [G.dataclasses.replace(o, wall=o.wall / 3, queue=None if o.queue is None else o.queue / 20) for o in self.S]
        rows = rows_by_name(G.evaluate_job(fast, self.B, 625.0, [0.4] * len(fast)))
        self.assertEqual({n: r.status for n, r in rows.items()}, {n: G.PASS for n in rows}, [r for r in rows.values() if r.status != G.PASS])

    def test_a_rerun_attempt_is_an_observation_without_a_queue_wait(self):
        reruns = [j for j in self.rec["blacksmith"] if (j["run_attempt"] or 1) > 1 and j["conclusion"] in G.OUTCOMES]
        self.assertTrue(reruns)
        self.assertEqual({(o.attempt > 1, o.queue is None) for o in self.B if o.attempt > 1}, {(True, True)})
        outcomes = [j for j in self.rec["blacksmith"] if j["conclusion"] in G.OUTCOMES and j.get("started_at") and j.get("completed_at")]
        self.assertEqual(len(self.B), len(outcomes))

    def test_the_fleet_comes_from_the_runner_group_and_labels(self):
        self.assertEqual({o.fleet for o in self.S}, {"superci"})
        self.assertEqual({o.fleet for o in self.B}, {"blacksmith"})


def scale(**kw):
    base = dict(replayed=100, completed=100, stranded=[], lost=0, waits=spread_around(3, 1, 100), window_use=[(1000.0, 400.0), (1060.0, 700.0)], org_known=[1.0, 1.0],
                claims={f"i-{n}" for n in range(10)}, issued=collections.Counter({f"i-{n}": 1 for n in range(10)}), claim_taken=collections.Counter({f"i-{n}": 1 for n in range(10)}),
                claim_refused=0, launched=["spot"] * 19 + ["on-demand"], orphans=[(2000.0, 0.0)], orphans_due=1990.0, lo=1000.0, hi=1060.0,
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
        self.assertEqual(judge_scale(window_use=[(1000.0, 1200.0), (1060.0, 100.0)])["org registrations per 5 min"].status, G.PASS)
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


def gh_job(jid, run, attempt, conclusion, created, started, completed, fleet="superci", name="verify-tools", sha="s", branch="ci/load-1"):
    """A job as GitHub's REST API returns it."""
    j = {"id": jid, "run_id": run, "run_attempt": attempt, "name": name, "conclusion": conclusion, "status": "completed", "head_sha": sha, "head_branch": branch,
         "created_at": T.iso(created), "started_at": T.iso(started) if started else None, "completed_at": T.iso(completed) if completed else None}
    if fleet == "superci":
        j.update(runner_group_name="superci-pull", runner_name="superci-i-1", labels=["superci-4cpu"])
    else:
        j.update(runner_group_name="Default", runner_name="blacksmith-4vcpu-ubuntu-2404-abc", labels=["blacksmith-4vcpu-ubuntu-2404"])
    return j


class Excuses(unittest.TestCase):
    def test_superci_failures_on_main_do_not_excuse_each_other(self):
        legs = [(r, leg) for r in range(5) for leg in range(2)]
        failing = {(0, 0), (0, 1), (1, 0)}
        S = [obs(80 + leg, 1.0, ok=(r, leg) not in failing, run=100 + r, sha="m", branch="main") for r, leg in legs]
        B20 = fleet(spread_around(100, 10, 20), spread_around(3, 1, 20), fleet="blacksmith")
        rows = G.evaluate_job(S, B20, 200.0, [0.5] * 10)
        by = rows_by_name(rows)
        self.assertEqual(by["failures (superci)"].value, "3 of 10")
        self.assertEqual(by["failures (superci)"].status, G.FAIL)
        self.assertEqual({n: r.status for n, r in by.items() if n != "failures (superci)"}, {n: G.PASS for n in by if n != "failures (superci)"}, "only the failures row is red")
        self.assertEqual(printed(rows, "gate 1"), 1)

    def test_a_failed_superci_run_on_the_same_commit_in_another_branch_does_not_excuse_either(self):
        a, b = obs(30, ok=False, sha="s9", branch="ci/a"), obs(30, ok=False, sha="s9", branch="ci/b")
        row = judge(self.good_runs() + [a, b])["failures (superci)"]
        self.assertEqual(row.value, "2 of 12")

    def good_runs(self):
        return fleet(spread_around(80, 2, 10), spread_around(1, 0.2, 10))


class Attempts(unittest.TestCase):
    T0 = 1_000_000.0

    def test_a_rerun_attempt_counts_its_failure_and_wall_time_but_not_its_queue_wait(self):
        first = G.is_obs(gh_job(1, 1, 1, "failure", self.T0, self.T0 + 2, self.T0 + 62), {})
        self.assertFalse(first.ok)
        rerun = G.is_obs(gh_job(2, 1, 2, "success", self.T0, self.T0 + 500, self.T0 + 560), {})
        self.assertIsNotNone(rerun, "a rerun's wall time is real")
        self.assertEqual((rerun.ok, rerun.attempt, rerun.wall), (True, 2, 60.0))
        self.assertIsNone(rerun.queue, "a rerun keeps its first attempt's created_at: its queue wait is not real")

    def test_queue_wait_rows_skip_a_rerun_s_missing_queue_wait_and_its_wall_time_counts(self):
        S = self.good() + [obs(500, None, attempt=2) for _ in range(3)]
        rows = judge(S, ceiling=None)
        self.assertEqual(rows["queue wait p50"].status, G.PASS)
        self.assertEqual(rows["wall p90"].status, G.FAIL, "the 500 s rerun is in the wall sample")

    def good(self):
        return fleet(spread_around(80, 2, 10), spread_around(1, 0.2, 10))


class FakeGitHub:
    """GitHub's jobs listing: `filter=latest` is the newest attempt's jobs, `filter=all` every attempt's."""
    runs_jobs = {}

    def __init__(self, repo, cache):
        self.calls = 0

    def run_jobs(self, run_id, attempts="latest"):
        jobs = FakeGitHub.runs_jobs[run_id]
        if attempts == "all":
            return list(jobs)
        top = max(j["run_attempt"] for j in jobs)
        return [j for j in jobs if j["run_attempt"] == top]


class GateJobEndToEnd(unittest.TestCase):
    """cmd_job over a canned GitHub: what it reads and what it prints."""
    now = None

    def run_gate(self, superci_runs, blacksmith_runs):
        now = self.now = __import__("time").time()
        runs, FakeGitHub.runs_jobs = [], {}
        for i, jobs in enumerate(superci_runs + blacksmith_runs):
            rid = 1000 + i
            for j in jobs:
                j["run_id"] = rid
                shift = (now - 1800) if i < len(superci_runs) else (now - 10 * 3600)
                for k in ("created_at", "started_at", "completed_at"):
                    if j[k]:
                        j[k] = T.iso(T.ts(j[k]) - 1_000_000.0 + shift)
            FakeGitHub.runs_jobs[rid] = jobs
            runs.append({"id": rid, "created_at": jobs[0]["created_at"], "head_branch": "ci/load-1"})
        saved = (G.T.GitHub, G.runs_of, G.T.gather, G.T.build)
        G.T.GitHub, G.runs_of = FakeGitHub, lambda gh, wf, lo, hi: runs
        G.T.gather = lambda *a, **k: {}
        G.T.build = lambda j, ev: type("TL", (), {"wall": 1.0, "unexplained": 0.5})()
        try:
            with tempfile.TemporaryDirectory() as d:
                limits = os.path.join(d, "limits.tsv")
                with open(limits, "w") as fh:
                    fh.write("lint-test-backend.yml\tverify-tools\t200\tx\n")
                a = G.build_parser().parse_args(["--cache", d, "job", "verify-tools", "--branch-glob", "ci/*", "--limits", limits, "--since", T.iso(now - 7200)])
                buf = io.StringIO()
                with contextlib.redirect_stdout(buf):
                    code = a.fn(a)
                return code, buf.getvalue()
        finally:
            G.T.GitHub, G.runs_of, G.T.gather, G.T.build = saved

    def superci(self, n, **kw):
        return [[gh_job(10 * i + 1, 0, 1, "success", 1_000_000.0, 1_000_001.0, 1_000_081.0)] for i in range(n)]

    def blacksmith(self, n):
        return [[gh_job(5000 + i, 0, 1, "success", 1_000_000.0, 1_000_003.0, 1_000_103.0, fleet="blacksmith")] for i in range(n)]

    def test_a_failure_that_a_rerun_then_passed_is_counted(self):
        rerun_run = [gh_job(901, 0, 1, "failure", 1_000_000.0, 1_000_001.0, 1_000_031.0), gh_job(902, 0, 2, "success", 1_000_000.0, 1_000_400.0, 1_000_480.0)]
        code, out = self.run_gate(self.superci(5) + [rerun_run], self.blacksmith(8))
        self.assertRegex(out, r"failures \(superci\)\s+1 of 7")

    def test_the_jobs_of_every_attempt_are_read(self):
        seen = []
        orig = FakeGitHub.run_jobs
        FakeGitHub.run_jobs = lambda self, run_id, attempts="latest": (seen.append(attempts), orig(self, run_id, attempts))[1]
        try:
            self.run_gate(self.superci(5), self.blacksmith(2))
        finally:
            FakeGitHub.run_jobs = orig
        self.assertEqual(set(seen), {"all"})

    def test_cancelled_jobs_are_reported_per_fleet(self):
        cancelled_s = [gh_job(801, 0, 1, "cancelled", 1_000_000.0, 1_000_001.0, 1_000_011.0)]
        cancelled_b = [gh_job(802, 0, 1, "cancelled", 1_000_000.0, 1_000_003.0, 1_000_013.0, fleet="blacksmith")]
        code, out = self.run_gate(self.superci(5) + [cancelled_s], self.blacksmith(8) + [cancelled_b])
        line = next((l for l in out.splitlines() if l.strip().startswith("cancelled")), "")
        self.assertIn("superci 1 run", line, out)
        self.assertIn("blacksmith 1 run", line, out)


class TimedOutAndCancelled(unittest.TestCase):
    T0 = 1_000_000.0

    def test_timed_out_is_a_failure_with_its_wall_time_floored_at_the_ceiling(self):
        o = G.is_obs(gh_job(1, 1, 1, "timed_out", self.T0, self.T0 + 2, self.T0 + 52), {})
        self.assertIsNotNone(o, "a job GitHub timed out is an observation")
        self.assertEqual((o.ok, o.timed_out), (False, True))

    def test_a_timed_out_leg_pushes_wall_p90_over_the_ceiling_and_counts_as_a_failure(self):
        timed = [G.is_obs(gh_job(1, 1, 1, "timed_out", self.T0, self.T0 + 2, self.T0 + 52), {}) for _ in range(2)]
        self.assertTrue(all(timed))
        S = fleet(spread_around(80, 2, 10), spread_around(1, 0.2, 10)) + timed
        by = judge(S, ceiling=200.0)
        self.assertEqual(by["wall p90"].status, G.FAIL)
        self.assertGreater(float(by["wall p90"].value.split()[0]), 150.0, "a 50 s timed-out leg counts at the 200 s ceiling, not at 50 s")
        self.assertEqual(by["failures (superci)"].value, "2 of 12")
        self.assertEqual(by["failures (superci)"].status, G.FAIL)

    def test_without_a_ceiling_a_timed_out_leg_keeps_the_wall_time_it_ran(self):
        timed = [G.is_obs(gh_job(i, 1, 1, "timed_out", self.T0, self.T0 + 2, self.T0 + 150), {}) for i in (1, 2)]
        self.assertTrue(all(timed))
        S = fleet(spread_around(80, 2, 10), spread_around(1, 0.2, 10))
        self.assertEqual(judge(S + timed, ceiling=None)["wall p90"].status, G.FAIL)
        self.assertEqual(judge(S, ceiling=None)["wall p90"].status, G.PASS)

    def test_cancelled_jobs_are_not_observations_and_are_counted_per_fleet(self):
        cancelled = [gh_job(1, 1, 1, "cancelled", self.T0, self.T0 + 2, self.T0 + 5), gh_job(2, 1, 1, "cancelled", self.T0, self.T0 + 2, self.T0 + 5),
                     gh_job(3, 2, 1, "cancelled", self.T0, self.T0 + 2, self.T0 + 5, fleet="blacksmith")]
        self.assertIsNone(G.is_obs(cancelled[0], {}))
        self.assertEqual(G.cancelled_by_fleet(cancelled), {"superci": (1, 2), "blacksmith": (1, 1)})


class Flags(unittest.TestCase):
    def parse(self, *argv):
        with contextlib.redirect_stderr(io.StringIO()):
            return G.build_parser().parse_args(list(argv))

    def test_gate_1_needs_a_branch_glob_so_the_superci_window_is_named(self):
        with self.assertRaises(SystemExit):
            self.parse("job", "verify-tools")
        self.assertEqual(self.parse("job", "verify-tools", "--branch-glob", "main").branch_glob, "main")

    def test_gate_2_needs_the_trace_so_a_push_that_never_ran_is_a_missing_job(self):
        with self.assertRaises(SystemExit):
            self.parse("scale", "--from", "2026-11-25T18:00:00Z", "--to", "2026-11-25T19:00:00Z")
        self.assertEqual(self.parse("scale", "--from", "a", "--to", "b", "--trace", "t.json").trace, "t.json")


class Gate2Completion(unittest.TestCase):
    def test_extra_jobs_do_not_hide_a_stranded_one(self):
        row = judge_scale(replayed=100, completed=100, stranded=[("extra", "failure")])["completion"]
        self.assertEqual(row.status, G.FAIL)
        self.assertIn("extra", row.note)

    def test_a_job_that_never_ran_is_named_as_missing(self):
        row = judge_scale(replayed=100, completed=95, stranded=[])["completion"]
        self.assertEqual(row.status, G.FAIL)
        self.assertIn("5 never ran", row.note)

    def test_more_jobs_completed_than_the_trace_held_is_not_complete_either(self):
        self.assertEqual(judge_scale(replayed=100, completed=101)["completion"].status, G.FAIL)


class RegistrationCoverage(unittest.TestCase):
    def test_a_minute_without_a_datapoint_fails_the_row_and_names_the_gap(self):
        row = judge_scale(lo=960.0, hi=1140.0, window_use=[(960.0, 100.0), (1080.0, 100.0)])["org registrations per 5 min"]
        self.assertEqual(row.status, G.FAIL)
        self.assertIn("1 of 3 minutes", row.note)
        self.assertIn(T.iso(1020.0), row.note)

    def test_the_first_and_last_minutes_count(self):
        late_start = judge_scale(lo=960.0, hi=1140.0, window_use=[(1020.0, 100.0), (1080.0, 100.0)])["org registrations per 5 min"]
        self.assertEqual(late_start.status, G.FAIL)
        early_stop = judge_scale(lo=960.0, hi=1140.0, window_use=[(960.0, 100.0), (1020.0, 100.0)])["org registrations per 5 min"]
        self.assertEqual(early_stop.status, G.FAIL)
        self.assertEqual(judge_scale(lo=960.0, hi=1140.0, window_use=[(960.0, 100.0), (1020.0, 100.0), (1080.0, 100.0)])["org registrations per 5 min"].status, G.PASS)


if __name__ == "__main__":
    unittest.main()
