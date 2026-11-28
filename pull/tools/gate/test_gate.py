import atexit
import collections
import contextlib
import io
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
import time
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import gate as G  # noqa: E402

T = G.T

# No test reaches the listener: the token command is a stub from here on, so nothing here runs deploy/ssh.sh.
_STUBS = tempfile.mkdtemp()
atexit.register(shutil.rmtree, _STUBS, True)


def stub(name, body):
    path = os.path.join(_STUBS, name)
    with open(path, "w") as fh:
        fh.write("#!/bin/sh\n" + body)
    os.chmod(path, 0o755)
    return path


APP_TOKEN = "ghs_apptoken_for_the_tests"
TOKEN_OK = stub("token_ok", f"printf '{{\"token\":\"{APP_TOKEN}\",\"expires\":\"2999-01-01T00:00:00Z\"}}\\n200\\n'\n")
# A listener that answers by scope (the stub finds it in SUPERCI_TOKEN_SCOPE): a token of its own for each, or, for a scope named in
# REFUSE_SCOPES, the listener's own refusal of a permission the installation lacks (HTTP 422). SCOPE_ASKS logs each scope asked for.
VARS_TOKEN = "ghs_variables_token_for_the_tests"
LIMITS_TOKEN = "ghs_contents_token_for_the_tests"
TOKEN_SCOPED = stub("token_scoped", """scope=${SUPERCI_TOKEN_SCOPE:-actions}
[ -n "${SCOPE_ASKS-}" ] && echo "$scope" >> "$SCOPE_ASKS"
case ",${REFUSE_SCOPES-}," in
  *,"$scope",*) printf 'the superci App installation lacks the repository permission "%s"\\n422\\n' "$scope"; exit 0 ;;
esac
case "$scope" in
  variables) t=@VARS@ ;;
  contents) t=@LIMITS@ ;;
  *) t=@APP@ ;;
esac
printf '{"token":"%s","expires":"2999-01-01T00:00:00Z"}\\n200\\n' "$t"
""".replace("@VARS@", VARS_TOKEN).replace("@LIMITS@", LIMITS_TOKEN).replace("@APP@", APP_TOKEN))
TOKEN_DOWN = stub("token_down", "echo 'ssh: connect to host: Connection timed out' >&2\nexit 255\n")
os.environ["SUPERCI_APP_TOKEN_CMD"] = TOKEN_OK
os.environ["SUPERCI_TEST"] = "1"  # the token command is honoured only under this flag


def obs(wall, queue=2.0, ok=True, fleet="superci", run=None, sha="a", branch="x", job_id=None, timed_out=False, attempt=1, cancelled=False):
    n = next(_ids)
    return G.Obs(job_id or n, run or n, sha, branch, fleet, ok and not cancelled, queue, wall, timed_out, attempt, cancelled)


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
        cancelled = [j for j in self.rec["blacksmith"] if j["conclusion"] == "cancelled" and j.get("completed_at")]
        self.assertTrue(cancelled, "the recording holds cancelled jobs")
        self.assertEqual(len(self.B), len(outcomes) + len(cancelled))
        self.assertEqual(sum(1 for o in self.B if o.cancelled), len(cancelled))

    def test_the_fleet_comes_from_the_runner_group_and_labels(self):
        self.assertEqual({o.fleet for o in self.S}, {"superci"})
        self.assertEqual({o.fleet for o in self.B}, {"blacksmith"})


class Canary(unittest.TestCase):
    QUIET = {n: "OK" for n in G.ROLLBACK_ALARMS}

    def test_canary_4b_at_its_rollback_keeps_test_go(self):
        """16:19Z Nov 26: test-go at parity on every row, while Blacksmith and the organization's other runners held GitHub's
        registration cap at 5.03-5.26/s and GitHub refused two of superci's registrations. The cause is the organization's."""
        rows = list(judge(Gate1().good()).values())
        alarms = {**self.QUIET, "superci-pull-registration-refusals": "ALARM", "superci-pull-registration-errors": "OK"}
        self.assertEqual(G.canary_verdict(rows, alarms), [])

    def test_a_job_slower_than_blacksmith_rolls_back(self):
        rows = list(judge(fleet(spread_around(140, 2, 20), spread_around(1, 0.2, 20))).values())
        self.assertEqual([r.split(":")[0] for r in G.canary_verdict(rows, self.QUIET)], ["wall p50", "wall p90"])

    def test_a_queue_wait_past_blacksmiths_rolls_back(self):
        rows = list(judge(fleet(spread_around(80, 2, 20), spread_around(30, 1, 20))).values())
        self.assertEqual([r.split(":")[0] for r in G.canary_verdict(rows, self.QUIET)], ["queue wait p50", "queue wait p90"])

    def test_one_slow_job_among_few_sends_nothing_back(self):
        rows = list(judge(fleet([80.0] * 5 + [200.0], [1.0] * 6)).values())
        self.assertEqual(rows_by_name(rows)["wall p90"].status, G.FAIL)
        self.assertEqual(G.canary_verdict(rows, self.QUIET), [])

    def test_no_blacksmith_runs_to_compare_sends_nothing_back(self):
        rows = list(judge(fleet(spread_around(80, 2, 20), spread_around(1, 0.2, 20)), base=[]).values())
        self.assertEqual(rows_by_name(rows)["wall p50"].status, G.FAIL)
        self.assertEqual(G.canary_verdict(rows, self.QUIET), [])

    def test_every_harm_alarm_rolls_back(self):
        rows = list(judge(Gate1().good()).values())
        for name in G.ROLLBACK_ALARMS:  # these stop every set: whatever set the job ran on, or none yet
            for sets in ((), ["superci-4cpu"]):
                self.assertEqual(G.canary_verdict(rows, {**self.QUIET, name: "ALARM"}, sets), [f"{name} in ALARM"])
        for alarm in G.SET_ROLLBACK_ALARMS:  # these are one set's: they send back the jobs that ran on it
            self.assertEqual(G.canary_verdict(rows, {alarm + "-4cpu": "ALARM"}, ["superci-4cpu"]), [f"{alarm}-4cpu in ALARM"])

    def test_speed_rows_wait_for_n_but_failures_count_from_the_first_run(self):
        slow = list(judge(fleet([140.0, 141.0], [1.0, 1.0])).values())
        self.assertEqual(G.canary_verdict(slow, self.QUIET), [])
        failing = list(judge(fleet([80.0, 81.0], [1.0, 1.0], ok=False)).values())
        self.assertEqual([r.split(":")[0] for r in G.canary_verdict(failing, self.QUIET)], ["failures (superci)"])

    def test_noise_asks_for_runs_and_rolls_nothing_back(self):
        rows = [G.Row("n (superci runs)", "6", ">= 5", G.PASS), G.Row("wall p90", "50 s", "<= 51 s", G.NOISE)]
        self.assertEqual(G.canary_verdict(rows, self.QUIET), [])

    def test_alarms_sh_creates_every_rollback_alarm(self):
        with open(os.path.join(HERE, "..", "..", "deploy", "alarms.sh")) as fh:
            made = {f"superci-pull-{line.split()[1]}" for line in fh if line.startswith(("alarm ", "math "))}
        self.assertLessEqual(set(G.ROLLBACK_ALARMS), made)
        self.assertNotIn("superci-pull-registration-refusals", G.ROLLBACK_ALARMS)


class CanaryPerSet(unittest.TestCase):
    """Nov 27 17:43Z: test-go's canary (superci-4cpu) was rolled back on the listener-wide oldest-job-wait, which superci-8cpu's
    cold machine (ci-cache-warm's net-tools, 250 s) had tripped. A canary judges the alarms of the sets it ran on."""
    QUIET = {n: "OK" for n in G.ROLLBACK_ALARMS}

    def rows(self):
        return list(judge(Gate1().good()).values())

    def test_another_sets_wait_and_the_global_wait_keep_a_canary_that_ran_on_4cpu(self):
        alarms = {**self.QUIET, "superci-pull-oldest-job-wait": "ALARM", "superci-pull-oldest-job-wait-8cpu": "ALARM", "superci-pull-jobs-stalled": "ALARM",
                  "superci-pull-jobs-stalled-8cpu": "ALARM", "superci-pull-oldest-job-wait-4cpu": "OK", "superci-pull-jobs-stalled-4cpu": "OK"}
        self.assertEqual(G.canary_verdict(self.rows(), alarms, ["superci-4cpu"]), [])

    def test_its_own_sets_alarm_rolls_it_back_and_names_the_set(self):
        for alarm in ("oldest-job-wait", "jobs-stalled"):
            alarms = {**self.QUIET, f"superci-pull-{alarm}-4cpu": "ALARM", f"superci-pull-{alarm}-8cpu": "ALARM"}
            self.assertEqual(G.canary_verdict(self.rows(), alarms, ["superci-4cpu"]), [f"superci-pull-{alarm}-4cpu in ALARM"])

    def test_a_canary_on_two_sets_rolls_back_on_either(self):
        alarms = {**self.QUIET, "superci-pull-oldest-job-wait-8cpu": "ALARM", "superci-pull-jobs-stalled-16cpu": "ALARM"}
        self.assertEqual(G.canary_verdict(self.rows(), alarms, ["superci-4cpu", "superci-8cpu"]), ["superci-pull-oldest-job-wait-8cpu in ALARM"])

    def test_a_set_whose_own_alarm_does_not_exist_is_judged_on_the_listener_wide_one(self):
        wide = {**self.QUIET, "superci-pull-oldest-job-wait": "ALARM"}
        for why, sets in (("alarms.sh not yet run, no per-set alarm in the answer", ["superci-4cpu"]), ("a split set alarms.sh makes none for", ["superci-4cpu-2"]),
                          ("no set known", [])):
            self.assertEqual(G.canary_verdict(self.rows(), wide, sets), ["superci-pull-oldest-job-wait in ALARM"], why)
        stalled = {**self.QUIET, "superci-pull-jobs-stalled": "ALARM"}
        self.assertEqual(G.canary_verdict(self.rows(), stalled, ["superci-4cpu"]), ["superci-pull-jobs-stalled in ALARM"])

    def test_a_split_set_beside_a_sized_one_is_judged_on_each_ones_own_alarm_and_the_reason_is_not_repeated(self):
        alarms = {**self.QUIET, "superci-pull-oldest-job-wait": "ALARM", "superci-pull-oldest-job-wait-4cpu": "OK", "superci-pull-jobs-stalled-4cpu": "OK"}
        self.assertEqual(G.canary_verdict(self.rows(), alarms, ["superci-4cpu", "superci-4cpu-2", "superci-4cpu-3"]), ["superci-pull-oldest-job-wait in ALARM"])

    def test_a_set_whose_own_alarm_exists_and_is_ok_ignores_the_listener_wide_one(self):
        alarms = {**self.QUIET, "superci-pull-oldest-job-wait": "ALARM", "superci-pull-oldest-job-wait-4cpu": "OK", "superci-pull-jobs-stalled-4cpu": "OK"}
        self.assertEqual(G.canary_verdict(self.rows(), alarms, ["superci-4cpu"]), [])

    def test_the_line_says_which_alarm_each_set_was_judged_on(self):
        alarms = {"superci-pull-oldest-job-wait-4cpu": "OK", "superci-pull-jobs-stalled-4cpu": "OK"}
        self.assertEqual(G.judged_on(alarms, ["superci-4cpu", "superci-4cpu-2"]),
                         "superci-4cpu by superci-pull-oldest-job-wait-4cpu, superci-pull-jobs-stalled-4cpu; "
                         "superci-4cpu-2 by superci-pull-oldest-job-wait (no per-set alarm), superci-pull-jobs-stalled (no per-set alarm)")
        self.assertEqual(G.judged_on({}, []), "no set known by superci-pull-oldest-job-wait (no per-set alarm), superci-pull-jobs-stalled (no per-set alarm)")

    def test_listener_down_and_spend_capped_roll_back_whatever_the_set(self):
        for name in ("superci-pull-listener-down", "superci-pull-spend-capped"):
            for sets in ([], ["superci-4cpu"], ["superci-32cpu"]):
                self.assertEqual(G.canary_verdict(self.rows(), {**self.QUIET, name: "ALARM"}, sets), [f"{name} in ALARM"])

    def test_the_sets_are_the_records_sets_else_the_label_blacksmith_ran_it_under_else_none(self):
        rec = lambda s: G.synth_job(dict(record("r", 1, 10), set=s, labels=[]))
        self.assertEqual(G.job_sets([rec("superci-4cpu"), rec("superci-4cpu"), rec("superci-8cpu")], []), ["superci-4cpu", "superci-8cpu"])
        blacksmith = [dict(gh_job(1, 1, 1, "success", 1.0, 2.0, 3.0, fleet="blacksmith"))]
        self.assertEqual(G.job_sets([], blacksmith), ["superci-4cpu"])
        self.assertEqual(G.job_sets([rec("superci-8cpu")], blacksmith), ["superci-8cpu"], "a record outranks the label")
        self.assertEqual(G.job_sets([], []), [])
        self.assertEqual(G.job_sets([], [{"labels": ["ubuntu-latest"]}]), [])


def scale(**kw):
    base = dict(replayed=100, completed=100, stranded=[], lost=0, waits=spread_around(3, 1, 100), window_use=[(1000.0, 400.0), (1060.0, 700.0)], org_known=[1.0, 1.0],
                claims={f"i-{n}" for n in range(10)}, issued=collections.Counter({f"i-{n}": 1 for n in range(10)}), claim_taken=collections.Counter({f"i-{n}": 1 for n in range(10)}),
                claim_refused=0, launched=["spot"] * 19 + ["on-demand"], orphans=[(2000.0, 0.0)], orphans_due=1990.0, lo=1000.0, hi=1060.0,
                alarms={"superci-pull-orphans": "OK", "superci-pull-jobs-stalled": "OK"}, queuewait_samples=100.0)
    base.update(kw)
    return G.Scale(**base)


def judge_scale(**kw):
    return rows_by_name(G.evaluate_scale(scale(**kw)))


class Cost(unittest.TestCase):
    def test_aws_spend_a_job_beside_blacksmiths_for_the_same_vcpus(self):
        recs = [{"set": "superci-8cpu", "usd": 0.04, "wall_ms": 50000}, {"set": "superci-8cpu", "usd": 0.06, "wall_ms": 50000},
                {"set": "superci-4cpu", "usd": 0.05, "wall_ms": 90000}, {"set": "superci-8cpu", "usd": None, "wall_ms": 1}, {"set": "superci-8cpu", "usd": 1.0}]
        bs = [{"labels": ["blacksmith-8vcpu-ubuntu-2404"], "started_at": "2026-11-27T10:00:00Z", "completed_at": "2026-11-27T10:00:59Z"},
              {"labels": ["blacksmith-8vcpu-ubuntu-2404"], "started_at": "2026-11-27T10:00:00Z", "completed_at": "2026-11-27T10:01:01Z"},
              {"labels": ["ubuntu-latest"], "started_at": "2026-11-27T10:00:00Z", "completed_at": "2026-11-27T10:09:00Z"}]
        lines = G.cost_lines(recs, bs)
        # 8 vCPU: one started minute ($0.016) and two ($0.032): $0.024 a job; AWS $0.05 a job, 2.08x
        self.assertEqual(lines, ["cost superci-4cpu: AWS $0.0500 a job (n=1), Blacksmith 4vcpu: no job",
                                 "cost superci-8cpu: AWS $0.0500 a job (n=2), Blacksmith 8vcpu $0.0240 (n=2): AWS 2.08x"])


class Gate2(unittest.TestCase):
    def test_a_clean_replay_passes_every_row(self):
        rows = judge_scale()
        self.assertEqual({n: r.status for n, r in rows.items()}, {n: G.PASS for n in rows})
        self.assertEqual(len(rows), 9)

    def with_dispatched(self, **kw):
        """A window of dispatch machines alone: claimed when handed their one job, never registered. The field is set after
        construction, so this test runs (and fails at its assertion) on a Scale that does not know it."""
        disp = kw.pop("dispatched_machines")
        s = scale(**kw)
        s.dispatched_machines = disp
        return rows_by_name(G.evaluate_scale(s))["one runner per machine"]

    def test_dispatched_machines_stand_for_registrations(self):
        ids = {f"i-d{n}" for n in range(8)}
        row = self.with_dispatched(claims=ids, issued=collections.Counter(), claim_taken=collections.Counter(), dispatched_machines=collections.Counter({i: 1 for i in ids}))
        self.assertEqual(row.status, G.PASS, row)
        both = ids | {f"i-{n}" for n in range(10)}
        row = self.with_dispatched(claims=both, dispatched_machines=collections.Counter({i: 1 for i in ids}))
        self.assertEqual(row.status, G.PASS, row)

    def test_a_machine_handed_two_jobs_or_handed_one_unclaimed_fails(self):
        ids = {f"i-d{n}" for n in range(8)}
        twice = collections.Counter({i: 1 for i in ids}) + collections.Counter({"i-d0": 1})
        row = self.with_dispatched(claims=ids, issued=collections.Counter(), claim_taken=collections.Counter(), dispatched_machines=twice)
        self.assertEqual(row.status, G.FAIL)
        self.assertIn("i-d0", row.note)
        row = self.with_dispatched(claims=ids - {"i-d1"}, issued=collections.Counter(), claim_taken=collections.Counter(), dispatched_machines=collections.Counter({i: 1 for i in ids}))
        self.assertEqual(row.status, G.FAIL, "a machine handed a job with no claim")
        self.assertIn("i-d1", row.note)

    def test_a_dispatch_machine_spent_after_its_claim_is_the_fence_working(self):
        # the dispatcher claims a machine when it pairs it, and a JOB send that fails or a reclaim before the hand-off spends
        # it: a claim, no jit.claim (only a grant writes one) and no record
        ids = {f"i-d{n}" for n in range(8)}
        row = self.with_dispatched(claims=ids | {"i-spent"}, issued=collections.Counter(), claim_taken=collections.Counter(), dispatched_machines=collections.Counter({i: 1 for i in ids}))
        self.assertEqual(row.status, G.PASS, row)
        self.assertIn("1 dispatch machine(s) spent after their claim", row.note)
        # a grant's claim (jit.claim taken) that registered nothing still fails
        row = self.with_dispatched(claims=ids | {"i-g"}, issued=collections.Counter(), claim_taken=collections.Counter({"i-g": 1}), dispatched_machines=collections.Counter({i: 1 for i in ids}))
        self.assertEqual(row.status, G.FAIL)
        self.assertIn("granted, not registered ['i-g']", row.note)

    def test_handed_machines_counts_dispatched_records_by_their_handed_time(self):
        rec = lambda inst, start, dispatch=True: {"inst": inst, "dispatch": dispatch, "start_time": start}
        recs = [rec("i-a", "2026-11-27T23:59:00Z"), rec("i-a", "2026-11-28T00:05:00Z"), rec("i-b", "2026-11-28T00:20:00Z"),
                rec("i-c", "2026-11-28T00:01:00Z", dispatch=False), {"inst": "i-d", "dispatch": True}, rec("", "2026-11-28T00:01:00Z")]
        got = G.handed_machines(recs, T.ts("2026-11-27T23:58:00Z"), T.ts("2026-11-28T00:10:00Z"))
        self.assertEqual(got, collections.Counter({"i-a": 2}), "only dispatched records with a machine, handed inside the window")

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
        # a claim no grant wrote a jit.claim for is the dispatcher's, of a machine it spent before a hand-off
        self.assertEqual(judge_scale(claims={f"i-{n}" for n in range(11)})[key].status, G.PASS)
        self.assertEqual(judge_scale(claims={f"i-{n}" for n in range(11)}, claim_taken=collections.Counter({f"i-{n}": 1 for n in range(11)}))[key].status, G.FAIL)
        self.assertIn("granted, not registered ['i-10']", judge_scale(claims={f"i-{n}" for n in range(11)}, claim_taken=collections.Counter({f"i-{n}": 1 for n in range(11)}))[key].note)
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

    def test_an_on_demand_first_type_is_outside_the_share(self):
        row = judge_scale(launched=["spot"] * 9 + ["fast"] * 80 + ["on-demand"])["on-demand share"]
        self.assertEqual(row.status, G.PASS, row)
        self.assertEqual(judge_scale(launched=["fast"] * 8 + ["on-demand"] * 2)["on-demand share"].status, G.FAIL)

    def test_the_share_is_of_the_launches_it_bounds(self):
        # 80 launches of on_demand sizes (fast) do not dilute the others' share: 2 of their 10 is 20%
        row = judge_scale(launched=["spot"] * 8 + ["fast"] * 80 + ["on-demand"] * 2)["on-demand share"]
        self.assertEqual(row.status, G.FAIL, row)
        self.assertIn("2 of 10", row.value)
        row = judge_scale(launched=["fast"] * 40)["on-demand share"]
        self.assertEqual(row.status, G.PASS, row)
        self.assertIn("40 of an on_demand", row.note)

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

    def run_jobs(self, run_id, attempts="latest", fresh=False):
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
                a = G.build_parser().parse_args(["--cache", d, "job", "verify-tools", "--branch-glob", "ci/*", "--limits", limits, "--since", T.iso(now - 7200), "--superci-source", "github"])
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

    def test_slow_cancelled_superci_jobs_fail_the_gate_end_to_end(self):
        slow = [[gh_job(800 + i, 0, 1, "cancelled", 1_000_000.0, 1_000_001.0, 1_000_301.0)] for i in range(3)]
        code, out = self.run_gate(self.superci(5), self.blacksmith(8))
        self.assertEqual(code, 0, out)
        code, out = self.run_gate(self.superci(5) + slow, self.blacksmith(8))
        self.assertEqual(code, 1, out)
        self.assertRegex(out, r"FAIL\s+wall p90")
        self.assertRegex(out, r"n \(superci runs\)\s+5")
        self.assertIn("superci 3 runs (3 jobs)", out)

    def test_a_superci_job_queued_and_cancelled_is_found_by_its_label_and_fails_queue_wait(self):
        waiting = []
        for i in range(3):
            j = gh_job(850 + i, 0, 1, "cancelled", 1_000_000.0, None, 1_000_120.0)
            j.update(runner_group_name=None, runner_name=None)
            waiting.append([j])
        code, out = self.run_gate(self.superci(5) + waiting, self.blacksmith(8))
        self.assertEqual(code, 1, out)
        self.assertRegex(out, r"FAIL\s+queue wait p90")

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

    def test_cancelled_jobs_are_counted_per_fleet(self):
        cancelled = [gh_job(1, 1, 1, "cancelled", self.T0, self.T0 + 2, self.T0 + 5), gh_job(2, 1, 1, "cancelled", self.T0, self.T0 + 2, self.T0 + 5),
                     gh_job(3, 2, 1, "cancelled", self.T0, self.T0 + 2, self.T0 + 5, fleet="blacksmith")]
        self.assertEqual(G.cancelled_by_fleet(cancelled), {"superci": (1, 2), "blacksmith": (1, 1)})


def queued_cancel(jid, run, created, completed, fleet="superci", attempt=1):
    """A job GitHub cancelled in the queue, as GitHub records it: started_at equals created_at and no runner was assigned
    (measured on megpt-mono's Blacksmith jobs, Nov 25: 12 of 44 cancels, some with completed_at a second before started_at)."""
    j = gh_job(jid, run, attempt, "cancelled", created, created, completed, fleet=fleet)
    j.update(runner_group_name=None, runner_name=None)
    return j


class CancelledJobsAreLowerBounds(unittest.TestCase):
    """A superci job that was slow and then cancelled must not look fast; Blacksmith's cancels are read the same way."""
    T0 = 1_000_000.0

    def good(self):
        return fleet(spread_around(80, 2, 10), spread_around(1, 0.2, 10))

    def test_a_started_job_cancelled_is_a_wall_sample_of_the_time_it_ran_and_keeps_its_real_queue_wait(self):
        o = G.is_obs(gh_job(1, 1, 1, "cancelled", self.T0, self.T0 + 4, self.T0 + 304), {})
        self.assertEqual((o.cancelled, o.ok, o.wall, o.queue), (True, False, 300.0, 4.0))

    def test_a_job_cancelled_in_the_queue_is_a_queue_sample_of_created_to_completed_and_has_no_wall(self):
        o = G.is_obs(queued_cancel(1, 1, self.T0, self.T0 + 120), {})
        self.assertEqual((o.cancelled, o.wall, o.queue, o.fleet), (True, None, 120.0, "superci"))
        self.assertEqual(G.wall_sample([o], None), [])

    def test_a_queued_cancel_whose_completed_at_precedes_started_at_gives_no_negative_wall(self):
        j = queued_cancel(1, 1, self.T0, self.T0 - 1)
        o = G.is_obs(j, {})
        self.assertIsNone(o.wall)
        self.assertGreaterEqual(o.queue, 0.0)

    def test_a_rerun_attempt_cancelled_in_the_queue_has_no_real_queue_wait(self):
        o = G.is_obs(queued_cancel(1, 1, self.T0, self.T0 + 120, attempt=2), {})
        self.assertEqual((o.queue, o.wall), (None, None))

    def test_a_skipped_job_and_a_cancelled_one_that_never_completed_are_not_observations(self):
        self.assertIsNone(G.is_obs(gh_job(1, 1, 1, "skipped", self.T0, None, self.T0 + 1), {}))
        self.assertIsNone(G.is_obs(gh_job(2, 1, 1, "cancelled", self.T0, self.T0 + 1, None), {}))

    def test_five_fast_runs_and_slow_jobs_that_were_cancelled_fail_wall_p90(self):
        fast = fleet(spread_around(80, 2, 5), spread_around(1, 0.2, 5))
        self.assertEqual(judge(fast)["wall p90"].status, G.PASS)
        slow = [obs(300.0, 1.0, cancelled=True) for _ in range(3)]
        by = judge(fast + slow)
        self.assertEqual(by["wall p90"].status, G.FAIL, by["wall p90"])
        self.assertEqual(by["n (superci runs)"].value, "5", "a cancelled job is not a run")

    def test_cancelled_jobs_that_ran_less_than_the_bar_do_not_fail_it(self):
        S = self.good() + [obs(30.0, 1.0, cancelled=True) for _ in range(2)]
        self.assertEqual(judge(S)["wall p90"].status, G.PASS)

    def test_jobs_queued_and_cancelled_fail_queue_wait_p90(self):
        S = self.good()
        self.assertEqual(judge(S)["queue wait p90"].status, G.PASS)
        waiting = [obs(None, 120.0, cancelled=True) for _ in range(3)]
        by = judge(S + waiting)
        self.assertEqual(by["queue wait p90"].status, G.FAIL, by["queue wait p90"])
        self.assertEqual(by["wall p90"].status, G.PASS, "a job that never started has no wall time")

    def test_the_same_cancels_on_blacksmith_move_its_bar_the_same_way(self):
        slow = lambda f: [obs(300.0, 1.0, cancelled=True, fleet=f) for _ in range(8)]
        S = self.good()
        self.assertEqual(judge(S + slow("superci"), ceiling=None)["wall p90"].status, G.FAIL)
        self.assertEqual(judge(S + slow("superci"), ceiling=None, base=B + slow("blacksmith"))["wall p90"].status, G.PASS,
                         "Blacksmith's own slow cancels raise its p90 bar, so the fleets are measured alike")

    def test_a_cancelled_job_is_never_a_failure_and_never_excuses_one(self):
        S = self.good() + [obs(300.0, 1.0, cancelled=True)]
        self.assertEqual(judge(S)["failures (superci)"].value.split()[0], "0")
        sha = "z"
        failed = obs(80.0, 1.0, ok=False, sha=sha, run=9001)
        cancelled_b = obs(300.0, 1.0, cancelled=True, fleet="blacksmith", sha=sha)
        self.assertFalse(G.excused(failed, [failed, cancelled_b]))
        by = judge(self.good() + [failed], base=B + [cancelled_b])
        self.assertEqual(by["failures (superci)"].value, "1 of 11")

    def test_a_blacksmith_cancel_does_not_raise_its_failure_rate(self):
        base = B + [obs(300.0, 1.0, cancelled=True, fleet="blacksmith") for _ in range(40)]
        S = self.good() + [obs(80.0, 1.0, ok=False, run=7001), obs(80.0, 1.0, ok=False, run=7002)]
        self.assertEqual(judge(S, base=base)["failures (superci)"].status, G.FAIL)


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


def record(job, queue_s, wall_s, conclusion="success", queued_at=1_000_000.0, name="verify-tools", branch="ci/load-1", run=7, workflow="lint-test-backend.yml"):
    """The listener's job.record event of a job: queued at queued_at, taken queue_s later, run for wall_s. wall_s None is a job no runner took."""
    started = queued_at + queue_s
    finish = started + (wall_s or 0) if wall_s is not None else queued_at + queue_s
    e = {"msg": "tl", "v": 1, "ev": "job.record", "eid": f"b.{job}", "t": finish, "set": "superci-4cpu", "run": run, "job": job, "name": name,
         "workflow_ref": f"o/r/.github/workflows/{workflow}@refs/heads/{branch}", "labels": ["superci-4cpu"], "conclusion": conclusion,
         "queue_time": T.iso(queued_at), "finish_time": T.iso(finish)}
    if wall_s is not None:
        e.update(start_time=T.iso(started), runner="superci-4cpu-i-0abc", queue_ms=int(queue_s * 1000), wall_ms=int(wall_s * 1000), partial=False)
    else:  # the listener's record of a job no runner took: it has no start, so it is partial and has no queue or wall time of its own
        e.update(partial=True)
    return e


class JobRecords(unittest.TestCase):
    def obs_of(self, e):
        return G.is_obs(G.synth_job(e), {"head_branch": G.record_branch(e), "head_sha": ""})

    def test_a_record_gives_the_queue_wait_wall_time_and_outcome_the_rest_job_would(self):
        o = self.obs_of(record("g1", 3, 81))
        self.assertEqual((o.queue, o.wall, o.ok, o.cancelled, o.fleet, o.branch, o.run_id), (3.0, 81.0, True, False, "superci", "ci/load-1", 7))
        f = self.obs_of(record("g2", 1, 30, "failure"))
        self.assertEqual((f.ok, f.timed_out, f.wall), (False, False, 30.0))

    def test_a_job_cancelled_in_the_queue_is_a_queue_lower_bound_and_one_cancelled_running_a_wall_lower_bound(self):
        q = self.obs_of(record("g3", 120, None, "cancelled"))
        self.assertEqual((q.cancelled, q.queue, q.wall, q.fleet), (True, 120.0, None, "superci"))
        r = self.obs_of(record("g4", 2, 300, "cancelled"))
        self.assertEqual((r.cancelled, r.queue, r.wall), (True, 2.0, 300.0))

    def test_the_workflow_and_branch_come_from_the_workflow_ref(self):
        e = record("g5", 1, 1, branch="gh-readonly-queue/main/pr-1-abc", workflow="superci-load.yml")
        self.assertEqual((G.record_workflow(e), G.record_branch(e)), ("superci-load.yml", "gh-readonly-queue/main/pr-1-abc"))
        self.assertEqual((G.record_workflow({}), G.record_branch({})), ("", ""))

    def test_a_redelivered_completion_is_one_job(self):
        a, b = record("g6", 1, 10), dict(record("g6", 1, 10), eid="b.again")
        saved = G.T.listener_events
        G.T.listener_events = lambda profile, lo, hi, where=None: [a, b, record("g7", 1, 10)]
        try:
            self.assertEqual([e["job"] for e in G.job_records("p", 0, 1)], ["g6", "g7"])
        finally:
            G.T.listener_events = saved

    def test_a_partial_record_has_no_times_and_is_not_counted(self):
        partial = {k: v for k, v in record("g9", 1, 10).items() if k not in ("queue_ms", "wall_ms", "start_time", "queue_time")}
        saved = G.T.listener_events
        G.T.listener_events = lambda profile, lo, hi, where=None: [dict(partial, partial=True), record("g10", 1, 10)]
        try:
            self.assertEqual([e["job"] for e in G.job_records("p", 0, 1)], ["g10"])
        finally:
            G.T.listener_events = saved

    def test_a_timeline_of_a_record_ends_where_its_runner_took_the_job(self):
        j = G.synth_job(record("g8", 3, 81))
        self.assertEqual(G.timeline_job(j)["completed_at"], j["started_at"])


class GateJobFromRecords(unittest.TestCase):
    """cmd_job reading superci's side from job.record events: GitHub is asked only about Blacksmith's runs."""

    def run_gate(self, records, blacksmith_runs, extra=(), github_runs=None):
        now = __import__("time").time()
        FakeGitHub.runs_jobs, runs = {}, []
        for i, jobs in enumerate(blacksmith_runs):
            rid = 5000 + i
            for j in jobs:
                j["run_id"] = rid
                for k in ("created_at", "started_at", "completed_at"):
                    if j[k]:
                        j[k] = T.iso(T.ts(j[k]) - 1_000_000.0 + now - (1800 if j["runner_group_name"] == "superci-pull" else 10 * 3600))
            FakeGitHub.runs_jobs[rid] = jobs
            runs.append({"id": rid, "created_at": jobs[0]["created_at"], "head_branch": "main"})
        shift = now - 1800 - 1_000_000.0
        for rid, jobs in (github_runs or {}).items():  # the REST jobs of runs the superci records name, in the records' own times
            FakeGitHub.runs_jobs[rid] = [dict(j, **{k: T.iso(T.ts(j[k]) + shift) for k in ("created_at", "started_at", "completed_at") if j.get(k)}) for j in jobs]
        records = [dict(r, t=now, **{k: T.iso(T.ts(r[k]) + shift) for k in ("queue_time", "start_time", "finish_time") if r.get(k)}) for r in records]
        listed, calls = [], []
        saved = (G.T.GitHub, G.runs_of, G.T.gather, G.T.build, G.T.listener_events)
        G.T.GitHub, G.runs_of = FakeGitHub, lambda gh, wf, lo, hi: (listed.append((lo, hi)), runs)[1]
        G.T.gather = lambda *a, **k: {}
        G.T.build = lambda j, ev: type("TL", (), {"wall": 1.0, "unexplained": 0.5})()
        G.T.listener_events = lambda profile, lo, hi, where=None: records
        orig = FakeGitHub.run_jobs
        FakeGitHub.run_jobs = lambda self, run_id, attempts="latest", fresh=False: (calls.append(run_id), orig(self, run_id, attempts))[1]
        try:
            with tempfile.TemporaryDirectory() as d:
                limits = os.path.join(d, "limits.tsv")
                with open(limits, "w") as fh:
                    fh.write("lint-test-backend.yml\tverify-tools\t200\tx\n")
                a = G.build_parser().parse_args(["--cache", d, "job", "verify-tools", "--branch-glob", "ci/*", "--limits", limits, "--since", T.iso(now - 7200), "--superci-source", "events", *extra])
                buf = io.StringIO()
                with contextlib.redirect_stdout(buf):
                    code = a.fn(a)
                return code, buf.getvalue(), calls, listed
        finally:
            G.T.GitHub, G.runs_of, G.T.gather, G.T.build, G.T.listener_events = saved
            FakeGitHub.run_jobs = orig

    def blacksmith(self, n):
        return [[gh_job(5000 + i, 0, 1, "success", 1_000_000.0, 1_000_003.0, 1_000_103.0, fleet="blacksmith")] for i in range(n)]

    def test_superci_runs_are_read_from_records_and_only_blacksmiths_runs_from_github(self):
        recs = [record(f"g{i}", 1, 81, run=100 + i) for i in range(5)]
        code, out, calls, listed = self.run_gate(recs, self.blacksmith(8))
        self.assertEqual(code, 0, out)
        self.assertRegex(out, r"n \(superci runs\)\s+5")
        self.assertEqual(sorted(calls), [5000 + i for i in range(8)], "jobs were read for Blacksmith's runs alone")
        self.assertIn("5 jobs from the listener's job records (no GitHub call)", out)
        self.assertEqual(len(listed), 1, "one listing, of the baseline window")

    def test_a_record_of_another_workflow_branch_or_job_is_not_the_jobs(self):
        recs = [record(f"g{i}", 1, 81, run=100 + i) for i in range(5)] + [record("x1", 1, 999, workflow="other.yml"), record("x2", 1, 999, branch="main"), record("x3", 1, 999, name="other-job")]
        code, out, _, _ = self.run_gate(recs, self.blacksmith(8))
        self.assertEqual(code, 0, out)
        self.assertRegex(out, r"n \(superci runs\)\s+5")

    def test_a_slow_record_and_a_failure_fail_the_rows_as_a_rest_job_would(self):
        recs = [record(f"g{i}", 1, 81, run=100 + i) for i in range(5)] + [record(f"s{i}", 1, 400, run=200 + i) for i in range(3)] + [record("f", 1, 20, "failure", run=300), record("f2", 1, 20, "failure", run=301)]
        code, out, _, _ = self.run_gate(recs, self.blacksmith(8))
        self.assertEqual(code, 1, out)
        self.assertRegex(out, r"FAIL\s+wall p90")
        self.assertRegex(out, r"FAIL\s+failures \(superci\)\s+2 of 10")

    def test_a_job_cancelled_in_the_queue_fails_queue_wait_from_its_record(self):
        recs = [record(f"g{i}", 1, 81, run=100 + i) for i in range(5)] + [record(f"c{i}", 120, None, "cancelled", run=400 + i) for i in range(3)]
        code, out, _, _ = self.run_gate(recs, self.blacksmith(8))
        self.assertEqual(code, 1, out)
        self.assertRegex(out, r"FAIL\s+queue wait p90")

    def test_a_job_cancelled_before_a_runner_took_it_is_kept_as_a_queue_lower_bound(self):
        c = record("c1", 120, None, "cancelled")
        self.assertTrue(c["partial"] and "runner" not in c, "the listener's record of such a job is partial and has no runner")
        saved = G.T.listener_events
        G.T.listener_events = lambda profile, lo, hi, where=None: [c, dict(record("c2", 5, 10, "cancelled"), partial=True, eid="b.c2"), record("g", 1, 10)]
        try:
            self.assertEqual([e["job"] for e in G.job_records("p", 0, 1)], ["c1", "g"], "a partial cancel that had a runner is a restart's, and is dropped")
        finally:
            G.T.listener_events = saved

    def test_superci_jobs_inside_a_sampled_baseline_run_are_not_counted_twice(self):
        both = [[gh_job(7000 + i, 0, 1, "success", 1_000_000.0, 1_000_001.0, 1_000_081.0) for _ in range(1)] for i in range(2)]  # superci jobs the REST listing also shows
        recs = [record(f"g{i}", 1, 81, run=100 + i) for i in range(5)]
        code, out, _, _ = self.run_gate(recs, self.blacksmith(8) + both)
        self.assertRegex(out, r"superci\s+wall\s+n=5 ", out)
        self.assertRegex(out, r"superci\s+queue\s+n=5 ", out)

    def test_a_failure_that_ran_to_the_ceiling_is_a_timeout_and_pushes_wall_p90_over_it(self):
        recs = [record(f"g{i}", 1, 81, run=100 + i) for i in range(16)] + [record(f"t{i}", 1, 200, "failure", run=300 + i) for i in range(4)]  # ceiling 200
        code, out, _, _ = self.run_gate(recs, self.blacksmith(8))
        self.assertRegex(out, r"FAIL\s+wall p90", out)

    def test_a_non_default_runner_group_still_finds_the_records(self):
        recs = [record(f"g{i}", 1, 81, run=100 + i) for i in range(5)]
        code, out, _, _ = self.run_gate(recs, self.blacksmith(8), extra=("--superci-group", "other-group"))
        self.assertRegex(out, r"n \(superci runs\)\s+5")

    def canary(self, records, alarms, exist=("4cpu", "8cpu"), blacksmith=8, **kw):
        """`gate job --canary` over `records`, with the alarms in `alarms` in ALARM and every other OK: (exit code, the output's judged-on and canary lines).
        alarm_states answers the two listener-wide alarms and the per-set ones of the sizes in `exist`, as CloudWatch does for alarms that were made."""
        saved = G.alarm_states
        made = [*G.ROLLBACK_ALARMS, *G.SET_ROLLBACK_ALARMS, *(f"{a}-{z}" for a in G.SET_ROLLBACK_ALARMS for z in exist)]
        G.alarm_states = lambda *a, **k: {n: "ALARM" if n in alarms else "OK" for n in (*made, *alarms)}
        try:
            code, out, _, _ = self.run_gate(records, self.blacksmith(blacksmith), extra=("--canary",), **kw)
        finally:
            G.alarm_states = saved
        return code, "\n".join(l for l in out.splitlines() if l.startswith(("judged on:", "canary ")))

    def on(self, superci_set, n=5):
        return [dict(record(f"{superci_set}-{i}", 1, 81, run=100 + i), set=superci_set, labels=[superci_set]) for i in range(n)]

    def test_a_canary_that_ran_on_4cpu_is_kept_while_8cpu_waits_for_a_cold_machine(self):
        """17:43:50Z Nov 27: every test-go row passed, and superci-pull-oldest-job-wait (listener-wide) was in ALARM for 8cpu's net-tools."""
        code, line = self.canary(self.on("superci-4cpu"), {"superci-pull-oldest-job-wait", "superci-pull-oldest-job-wait-8cpu"})
        self.assertEqual((code, line.splitlines()[-1].split(":", 1)[1].split("[")[0].strip()), (0, "KEEP"), line)
        self.assertIn("superci-pull-oldest-job-wait-8cpu", line, "it still pages")

    def test_a_canary_that_ran_on_4cpu_is_rolled_back_by_4cpus_own_wait_naming_the_set(self):
        code, line = self.canary(self.on("superci-4cpu"), {"superci-pull-oldest-job-wait-4cpu", "superci-pull-oldest-job-wait-8cpu"})
        self.assertEqual(code, 3, line)
        self.assertIn("ROLL BACK (superci-pull-oldest-job-wait-4cpu in ALARM)", line)

    def test_listener_down_and_spend_capped_roll_back_a_canary_on_any_set(self):
        for name in ("superci-pull-listener-down", "superci-pull-spend-capped"):
            for sets in ("superci-4cpu", "superci-8cpu"):
                code, line = self.canary(self.on(sets), {name})
                self.assertEqual(code, 3, line)
                self.assertIn(f"ROLL BACK ({name} in ALARM)", line)

    def test_a_per_set_alarm_that_does_not_exist_falls_back_to_the_listener_wide_one(self):
        wide = {"superci-pull-oldest-job-wait"}
        # (a) the rollout window: alarms.sh has not made the per-set alarms yet
        code, line = self.canary(self.on("superci-4cpu"), wide, exist=())
        self.assertEqual(code, 3, line)
        self.assertIn("ROLL BACK (superci-pull-oldest-job-wait in ALARM)", line)
        self.assertIn("superci-4cpu by superci-pull-oldest-job-wait (no per-set alarm), superci-pull-jobs-stalled (no per-set alarm)", line)
        # (b) a split set, which alarms.sh makes no alarm for
        code, line = self.canary(self.on("superci-4cpu-2"), wide)
        self.assertEqual(code, 3, line)
        self.assertIn("judged on: superci-4cpu-2 by superci-pull-oldest-job-wait (no per-set alarm)", line)
        # (c) no set known: no superci record and no Blacksmith run in the baseline
        code, line = self.canary([], wide, blacksmith=0)
        self.assertEqual(code, 3, line)
        self.assertIn("judged on: no set known by superci-pull-oldest-job-wait (no per-set alarm)", line)

    def test_a_per_set_alarm_that_exists_is_what_the_set_is_judged_on_and_the_line_says_so(self):
        code, line = self.canary(self.on("superci-4cpu"), {"superci-pull-oldest-job-wait", "superci-pull-oldest-job-wait-8cpu"})
        self.assertEqual(code, 0, line)
        self.assertIn("judged on: superci-4cpu by superci-pull-oldest-job-wait-4cpu, superci-pull-jobs-stalled-4cpu\n", line)

    def test_with_no_record_yet_the_canary_is_judged_on_the_set_of_its_label(self):
        """No superci record: the label the job ran under on Blacksmith (blacksmith-4vcpu-ubuntu-2404) names superci-4cpu."""
        code, line = self.canary([], {"superci-pull-oldest-job-wait-8cpu"})
        self.assertEqual(code, 0, line)
        code, line = self.canary([], {"superci-pull-oldest-job-wait-4cpu"})
        self.assertEqual(code, 3, line)

    def test_the_github_source_is_still_there_for_a_window_before_the_records(self):
        a = G.build_parser().parse_args(["job", "verify-tools", "--branch-glob", "main"])
        self.assertEqual(a.superci_source, "auto")
        self.assertEqual(G.build_parser().parse_args(["job", "verify-tools", "--branch-glob", "main", "--superci-source", "github"]).superci_source, "github")


class AutoSource(unittest.TestCase):
    """auto reads the records only for a window that starts after the first one: a window from before them is never read as empty."""
    def resolve(self, flag, since, first):
        saved = G.T.insights
        G.T.insights = lambda *a, **k: [{"first": str(int(first * 1000))}] if first else []
        try:
            a = G.build_parser().parse_args(["job", "verify-tools", "--branch-glob", "main"] + (["--superci-source", flag] if flag else []))
            return G.resolve_source(a, since, 2_000_000.0)
        finally:
            G.T.insights = saved

    def test_a_window_after_the_first_record_reads_records(self):
        self.assertEqual(self.resolve(None, 1_500_000.0, 1_000_000.0), "events")

    def test_a_window_before_the_first_record_reads_github(self):
        self.assertEqual(self.resolve(None, 900_000.0, 1_000_000.0), "github")
        self.assertEqual(self.resolve(None, 1_000_030.0, 1_000_000.0), "github", "a minute of margin: a record written at the edge is not the whole window")

    def test_no_record_at_all_reads_github(self):
        self.assertEqual(self.resolve(None, 1_500_000.0, None), "github")

    def test_an_explicit_source_is_not_asked_about(self):
        saved = G.T.insights
        G.T.insights = lambda *a, **k: self.fail("auto's query ran for an explicit source")
        try:
            a = G.build_parser().parse_args(["job", "verify-tools", "--branch-glob", "main", "--superci-source", "events"])
            self.assertEqual(G.resolve_source(a, 0.0, 1.0), "events")
        finally:
            G.T.insights = saved


class GateScaleFromRecords(unittest.TestCase):
    def test_a_replay_is_judged_from_records_with_no_github_call(self):
        lo = 1_000_000.0
        recs = [record(f"g{i}", 1 + i % 3, 30, run=i, workflow="superci-load.yml", branch="ci/superci-load-pull-x", queued_at=lo + 5) for i in range(9)]
        recs.append(record("bad", 2, 30, "failure", workflow="superci-load.yml", branch="ci/superci-load-pull-x", queued_at=lo + 5))
        recs.append(record("other", 2, 30, workflow="lint-test-backend.yml", branch="ci/superci-load-pull-x", queued_at=lo + 5))
        saved = (G.T.GitHub, G.T.listener_events, G.T.insights, G.metric, G.claim_objects, G.alarm_states, G.runs_of)
        boom = lambda *a, **k: self.fail("GitHub was read")
        G.runs_of = boom
        windows = []
        G.T.listener_events = lambda profile, lo_, hi_, where=None: (windows.append((lo_, hi_)), recs if where else [])[1]
        G.T.insights = lambda *a, **k: []
        G.metric = lambda *a, **k: []
        G.claim_objects = lambda *a, **k: set()
        G.alarm_states = lambda *a, **k: {}
        try:
            with tempfile.TemporaryDirectory() as d:
                trace = os.path.join(d, "t.json")
                with open(trace, "w") as fh:
                    json.dump({"jobs": [{}] * 10}, fh)
                a = G.build_parser().parse_args(["--cache", d, "scale", "--from", T.iso(lo), "--to", T.iso(lo + 60), "--trace", trace, "--superci-source", "events"])
                s, gh = G.collect_scale(a)
        finally:
            G.T.GitHub, G.T.listener_events, G.T.insights, G.metric, G.claim_objects, G.alarm_states, G.runs_of = saved
        self.assertEqual((s.completed, gh.calls, len(s.waits)), (9, 0, 10))
        self.assertGreaterEqual(windows[0][1], lo + 60 + 7200, "records are written when jobs finish: the read runs past the replay's end")
        self.assertEqual(s.stranded, [("verify-tools", "failure")])
        self.assertEqual(sorted(s.waits)[:3], [1.0, 1.0, 1.0])


def dispatched(e, identity="superci-id-4cpu-0001"):
    """A job.record of a job the identity plane ran: queue_time is when the identity's shim brought it, the runner is the identity."""
    return dict(e, dispatch=True, runner=identity)


def rest_of_dispatched(e, created_before, jid, attempt=1, identity="superci-id-4cpu-0001"):
    """The REST job a dispatched record is: created created_before seconds before the shim's arrival, completed a second after the record's finish."""
    j = gh_job(jid, e["run"], attempt, "success", T.ts(e["queue_time"]) - created_before, T.ts(e["start_time"]), T.ts(e["finish_time"]) + 1, name=e["name"])
    j.update(runner_name=identity)
    return j


class StubGitHub:
    """The jobs listing of canned runs, counting the runs it was asked about."""
    runs = {}
    asked = []

    def __init__(self, repo="", cache=None):
        self.calls = 0

    def run_jobs(self, run_id, attempts="latest", fresh=False):
        StubGitHub.asked.append((run_id, attempts))
        self.calls += 1
        return list(StubGitHub.runs[run_id])


class DispatchedWaitFromGitHub(unittest.TestCase):
    """A dispatched job waits for a free identity at GitHub, before its shim brings it to the dispatcher: its record's queue_time is the
    shim's arrival, so the wait is measured from GitHub's own created_at or a size short of identities reads as fast."""

    def setUp(self):
        StubGitHub.runs, StubGitHub.asked = {}, []

    def test_a_dispatched_jobs_wait_starts_at_githubs_created_at_not_the_shims_arrival(self):
        e = dispatched(record("g1", 1, 81, run=7))
        StubGitHub.runs[7] = [rest_of_dispatched(e, 300, 11)]
        jobs, n, unmatched = G.with_github_created(StubGitHub(), [e])
        o = G.is_obs(jobs[0], {"head_branch": "", "head_sha": ""})
        self.assertEqual((n, unmatched, o.queue, o.wall), (1, [], 301.0, 81.0), "queued 300 s at GitHub, 1 s from the shim's arrival to a machine")
        self.assertEqual(G.is_obs(G.synth_job(e), {"head_branch": "", "head_sha": ""}).queue, 1.0, "the record alone reads 1 s")

    def test_one_listing_serves_every_job_of_a_run_and_a_record_that_is_not_dispatched_asks_nothing(self):
        legs = [dispatched(record(f"g{i}", 1, 81, run=7, name=f"verify-tools ({i})")) for i in range(3)]
        plain = record("p", 1, 81, run=8)
        StubGitHub.runs[7] = [rest_of_dispatched(e, 40, 20 + i) for i, e in enumerate(legs)]
        jobs, n, unmatched = G.with_github_created(StubGitHub(), legs + [plain])
        self.assertEqual((n, unmatched, StubGitHub.asked), (3, [], [(7, "all")]), "one call for the run, every attempt, and none for run 8")
        self.assertEqual([G.is_obs(j, {"head_branch": "", "head_sha": ""}).queue for j in jobs], [41.0, 41.0, 41.0, 1.0])

    def test_a_rerun_is_told_from_its_first_attempt_and_has_no_queue_wait(self):
        first = dispatched(record("g1", 1, 81, run=7, queued_at=1_000_000.0))
        again = dispatched(record("g2", 1, 81, run=7, queued_at=1_000_600.0))
        StubGitHub.runs[7] = [rest_of_dispatched(first, 5, 11, 1), dict(rest_of_dispatched(again, 5, 12, 2), created_at=T.iso(1_000_000.0 - 5))]
        jobs, _, unmatched = G.with_github_created(StubGitHub(), [first, again])
        a, b = (G.is_obs(j, {"head_branch": "", "head_sha": ""}) for j in jobs)
        self.assertEqual((unmatched, a.queue, a.attempt, b.queue, b.attempt), ([], 6.0, 1, None, 2), "the attempt is the one that completed at the record's finish, and a rerun keeps its first attempt's created_at")

    def test_a_job_of_another_identity_or_name_is_not_the_record(self):
        e = dispatched(record("g1", 1, 81, run=7))
        StubGitHub.runs[7] = [rest_of_dispatched(e, 300, 11, identity="superci-id-4cpu-0002"), dict(rest_of_dispatched(e, 300, 12), name="other")]
        _, _, unmatched = G.with_github_created(StubGitHub(), [e])
        self.assertEqual(unmatched, [e])

    def test_a_dispatched_record_no_rest_job_fits_fails_a_row_and_keeps_the_arrival_as_its_lower_bound(self):
        e = dispatched(record("g1", 1, 81, run=7))
        StubGitHub.runs[7] = []
        jobs, n, unmatched = G.with_github_created(StubGitHub(), [e])
        self.assertEqual(G.is_obs(jobs[0], {"head_branch": "", "head_sha": ""}).queue, 1.0)
        row = G.dispatched_row(n, unmatched)[0]
        self.assertEqual((row.name, row.value, row.status), ("dispatched wait from GitHub", "0 of 1", G.FAIL))
        self.assertEqual(G.dispatched_row(0, []), [], "no dispatched job, no row")
        self.assertEqual(G.dispatched_row(2, [])[0].status, G.PASS)

    def test_the_timeline_of_a_dispatched_job_still_starts_where_the_dispatcher_saw_it(self):
        e = dispatched(record("g1", 1, 81, run=7))
        StubGitHub.runs[7] = [rest_of_dispatched(e, 300, 11)]
        j = G.with_github_created(StubGitHub(), [e])[0][0]
        self.assertEqual(G.timeline_job(j)["created_at"], e["queue_time"])
        self.assertEqual(G.timeline_job(G.synth_job(record("g2", 1, 81)))["created_at"], T.iso(1_000_000.0))


class GateJobDispatched(unittest.TestCase):
    """cmd_job over dispatched records: a size starved of identities fails the gate."""
    run_gate = GateJobFromRecords.run_gate
    blacksmith = GateJobFromRecords.blacksmith

    def dispatched_records(self, n, created_before):
        recs = [dispatched(record(f"g{i}", 1, 81, run=100 + i)) for i in range(n)]
        return recs, {100 + i: [rest_of_dispatched(e, created_before, 900 + i)] for i, e in enumerate(recs)}

    def test_jobs_that_waited_five_minutes_for_an_identity_fail_queue_wait_though_every_record_says_one_second(self):
        recs, github = self.dispatched_records(5, 300)
        code, out, calls, _ = self.run_gate(recs, self.blacksmith(8), github_runs=github)
        self.assertEqual(code, 1, out)
        self.assertRegex(out, r"FAIL\s+queue wait p50\s+301\.0 s")
        self.assertRegex(out, r"PASS\s+dispatched wait from GitHub\s+5 of 5")
        self.assertEqual(sorted(c for c in calls if c < 5000), [100 + i for i in range(5)], "one listing for each dispatched run")
        self.assertIn("GitHub read for the created_at of 5 dispatched jobs", out)

    def test_jobs_an_identity_took_at_once_pass_as_the_record_says(self):
        recs, github = self.dispatched_records(5, 1)
        code, out, _, _ = self.run_gate(recs, self.blacksmith(8), github_runs=github)
        self.assertEqual(code, 0, out)
        self.assertRegex(out, r"PASS\s+queue wait p50\s+2\.0 s")

    def test_a_dispatched_record_with_no_job_at_github_fails_the_gate(self):
        recs, github = self.dispatched_records(5, 1)
        github[100] = []
        code, out, _, _ = self.run_gate(recs, self.blacksmith(8), github_runs=github)
        self.assertEqual(code, 1, out)
        self.assertRegex(out, r"FAIL\s+dispatched wait from GitHub\s+4 of 5")


class GateScaleDispatched(unittest.TestCase):
    def collect(self, recs, github):
        lo = 1_000_000.0
        StubGitHub.runs, StubGitHub.asked = github, []
        saved = (G.T.GitHub, G.T.listener_events, G.T.insights, G.metric, G.claim_objects, G.alarm_states, G.runs_of)
        G.T.GitHub = StubGitHub
        G.runs_of = lambda *a, **k: self.fail("runs were listed")
        G.T.listener_events = lambda profile, lo_, hi_, where=None: recs if where else []
        G.T.insights = lambda *a, **k: []
        G.metric = lambda *a, **k: []
        G.claim_objects = lambda *a, **k: set()
        G.alarm_states = lambda *a, **k: {}
        try:
            with tempfile.TemporaryDirectory() as d:
                trace = os.path.join(d, "t.json")
                with open(trace, "w") as fh:
                    json.dump({"jobs": [{}] * len(recs)}, fh)
                a = G.build_parser().parse_args(["--cache", d, "scale", "--from", T.iso(lo), "--to", T.iso(lo + 60), "--trace", trace, "--superci-source", "events"])
                return G.collect_scale(a)[0]
        finally:
            G.T.GitHub, G.T.listener_events, G.T.insights, G.metric, G.claim_objects, G.alarm_states, G.runs_of = saved

    def test_a_replay_of_dispatched_jobs_reads_each_wait_from_github_and_fails_queue_wait_for_a_starved_size(self):
        recs = [dispatched(record(f"g{i}", 1, 30, run=i + 1, workflow="superci-load.yml", branch="ci/superci-load-pull-x", queued_at=1_000_005.0)) for i in range(10)]
        s = self.collect(recs, {i + 1: [rest_of_dispatched(e, 120, 50 + i)] for i, e in enumerate(recs)})
        self.assertEqual((s.dispatched, s.dispatched_unmatched, sorted(set(s.waits))), (10, [], [121.0]))
        rows = rows_by_name(G.evaluate_scale(s))
        self.assertEqual((rows["queue wait p50"].status, rows["queue wait p90"].status, rows["dispatched wait from GitHub"].status), (G.FAIL, G.FAIL, G.PASS))

    def test_scale_sets_records_still_ask_github_nothing(self):
        recs = [record(f"g{i}", 1, 30, run=i + 1, workflow="superci-load.yml", branch="ci/superci-load-pull-x", queued_at=1_000_005.0) for i in range(3)]
        s = self.collect(recs, {})
        self.assertEqual((s.dispatched, StubGitHub.asked, s.waits), (0, [], [1.0, 1.0, 1.0]))
        self.assertNotIn("dispatched wait from GitHub", rows_by_name(G.evaluate_scale(s)))


class FakeHTTP:
    """What `gh api` answers for a run's jobs listing, counting the calls: the only stub under the real Cache and GitHub."""

    def __init__(self):
        self.runs, self.calls = {}, []

    def __call__(self, argv, capture_output=True, text=True):
        run = int(__import__("re").search(r"runs/(\d+)/jobs", argv[2]).group(1))
        self.calls.append(run)
        jobs = self.runs[run]
        return subprocess.CompletedProcess(argv, 0, stdout=json.dumps({"total_count": len(jobs), "jobs": jobs}), stderr="")


def in_progress(j):
    return dict(j, completed_at=None, conclusion=None, status="in_progress", runner_name=None)


class CachedRunListing(unittest.TestCase):
    """A listing read while its run was in flight, or before a rerun began, must not stay as it was: the check that follows reads it again."""

    def setUp(self):
        self.http, self.saved, self.personal = FakeHTTP(), subprocess.run, T.AUTH.personal
        T.AUTH.personal = True  # the stub stands for gh itself; the credential it is given is tested elsewhere
        subprocess.run = self.http
        self.tmp = tempfile.TemporaryDirectory()
        self.cache = T.Cache(self.tmp.name)

    def tearDown(self):
        subprocess.run = self.saved
        T.AUTH.personal = self.personal
        self.tmp.cleanup()

    def check(self, recs):
        """One check of the gate: a new GitHub over the same cache directory, as a rerun of the command has."""
        before = len(self.http.calls)
        jobs, n, unmatched = G.with_github_created(T.GitHub("o/r", self.cache), recs)
        return jobs, unmatched, len(self.http.calls) - before

    def test_a_run_in_flight_at_the_first_check_is_matched_at_the_next_with_one_new_call(self):
        a, b = (dispatched(record(f"g{i}", 1, 81, run=7, name=f"verify-tools ({i})")) for i in (1, 2))
        done_a, done_b = rest_of_dispatched(a, 40, 11), rest_of_dispatched(b, 40, 12)
        self.http.runs[7] = [done_a, in_progress(done_b)]
        _, unmatched, calls = self.check([a])
        self.assertEqual((unmatched, calls), ([], 1))
        self.http.runs[7] = [done_a, done_b]
        jobs, unmatched, calls = self.check([a, b])
        self.assertEqual((unmatched, calls), ([], 1), "the in-flight listing was not kept, so b is read from GitHub once")
        self.assertEqual(G.is_obs(jobs[1], {"head_branch": "", "head_sha": ""}).queue, 41.0)
        self.assertEqual(self.check([a, b])[1:], ([], 0), "now every job has completed, and the listing is kept")

    def test_a_listing_kept_complete_is_read_again_once_for_a_rerun_attempt_it_lacks(self):
        first = dispatched(record("g1", 1, 81, run=7, queued_at=1_000_000.0))
        again = dispatched(record("g2", 1, 81, run=7, queued_at=1_000_600.0))
        j1 = rest_of_dispatched(first, 5, 11, 1)
        self.http.runs[7] = [j1]
        self.assertEqual(self.check([first])[1:], ([], 1))
        self.assertEqual(self.check([first])[1:], ([], 0), "the complete listing is kept")
        self.http.runs[7] = [j1, dict(rest_of_dispatched(again, 5, 12, 2), created_at=T.iso(1_000_000.0 - 5))]
        jobs, unmatched, calls = self.check([first, again])
        self.assertEqual((unmatched, calls), ([], 1), "attempt 2 is not in the kept listing: one uncached read")
        self.assertEqual([G.is_obs(j, {"head_branch": "", "head_sha": ""}).attempt for j in jobs], [1, 2])
        self.assertEqual(self.check([first, again])[1:], ([], 0), "the read replaced the kept listing")

    def test_a_run_whose_jobs_all_match_makes_no_new_call(self):
        recs = [dispatched(record(f"g{i}", 1, 81, run=7, name=f"verify-tools ({i})")) for i in (1, 2, 3)]
        self.http.runs[7] = [rest_of_dispatched(e, 30, 20 + i) for i, e in enumerate(recs)]
        self.assertEqual(self.check(recs)[1:], ([], 1))
        self.assertEqual(self.check(recs)[1:], ([], 0))

    def test_records_no_listing_matches_cost_one_extra_read_a_run_not_one_a_record(self):
        recs = [dispatched(record(f"g{i}", 1, 81, run=7, name=f"verify-tools ({i})")) for i in (1, 2)]
        self.http.runs[7] = []
        _, unmatched, calls = self.check(recs)
        self.assertEqual((len(unmatched), calls), (2, 2), "the listing, and its one second read")


# ---------------------------------------------------------------------------------------------------------------------
# GitHub is read on the App's budget, never on the shared personal token unless the operator says so.
# ---------------------------------------------------------------------------------------------------------------------

PULL = os.path.normpath(os.path.join(HERE, "..", ".."))
GH_STUB = """printf '%s\\t%s\\n' "${GH_TOKEN-UNSET}" "$*" >> "$GHLOG"
printf '%s\\t%s\\t%s\\n' "${GH_TOKEN-UNSET}" "${GH_SHIM_KEEP_TOKEN-UNSET}" "$*" >> "$GHLOG.env"
case "$*" in
  *rate_limit*) if [ "${GH_TOKEN-}" = "@TOKEN@" ]; then echo ${APP_REMAINING:-4000}; else echo ${PERSONAL_REMAINING:-0}; fi ;;
  *actions/variables/SUPERCI_JOBS*) echo '["verify-tools"]' ;;
  *actions/workflows/*) if [ -n "${RUN_CREATED-}" ]; then echo '{"total_count":1,"workflow_runs":[{"id":7,"created_at":"'$RUN_CREATED'","head_branch":"main"}]}'; else echo '{"total_count":0,"workflow_runs":[],"jobs":[]}'; fi ;;
  *) echo '{"total_count":0,"workflow_runs":[],"jobs":[]}' ;;
esac
""".replace("@TOKEN@", APP_TOKEN)


class Sandbox(unittest.TestCase):
    """A PATH of stub aws, gh and sleep, and a log of every gh call with the GH_TOKEN it ran with."""

    def setUp(self):
        self.tmp = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, self.tmp, True)
        self.bin = os.path.join(self.tmp, "bin")
        os.makedirs(self.bin)
        self.log = os.path.join(self.tmp, "gh.log")
        open(self.log, "w").close()
        for name, body in {
            "gh": GH_STUB,
            "aws": 'case "$*" in *get-caller-identity*) echo 243891825325 ;; *) echo "stub aws: no network in tests" >&2; exit 1 ;; esac\n',
            "sleep": "exec /bin/sleep 1\n",
        }.items():
            path = os.path.join(self.bin, name)
            with open(path, "w") as fh:
                fh.write("#!/bin/sh\n" + body)
            os.chmod(path, 0o755)
        self.env = {k: v for k, v in os.environ.items() if k not in ("GH_TOKEN", "GITHUB_TOKEN", "SUPERCI_PERSONAL_TOKEN")}
        self.env.update(PATH=self.bin + os.pathsep + os.environ["PATH"], GHLOG=self.log, SUPERCI_APP_TOKEN_CMD=TOKEN_OK, SUPERCI_TEST="1")

    def gh_calls(self):
        with open(self.log) as fh:
            return [tuple(line.rstrip("\n").split("\t", 1)) for line in fh if line.strip()]

    def gh_envs(self):
        """(GH_TOKEN, GH_SHIM_KEEP_TOKEN, args) of every gh call: what the stub saw in its environment."""
        path = self.log + ".env"
        if not os.path.exists(path):
            return []
        with open(path) as fh:
            return [tuple(line.rstrip("\n").split("\t", 2)) for line in fh if line.strip()]

    def assert_app_calls_keep_the_token(self, token, only=None):
        calls = [c for c in self.gh_envs() if c[0] == token and (only is None or only in c[2])]
        self.assertTrue(calls, (token, only, self.gh_envs()))
        for tok, keep, args in calls:
            self.assertEqual(keep, "1", f"GH_SHIM_KEEP_TOKEN=1 on the App-token call `gh {args}`")


class GateReadsOnTheAppBudget(Sandbox):
    def run_gate(self, *argv, token_cmd=None, env=None):
        """gate.main in this process with the sandbox's PATH, returning (SystemExit code or None, stdout, stderr)."""
        env = dict(self.env, **({"SUPERCI_APP_TOKEN_CMD": token_cmd} if token_cmd else {}), **(env or {}))
        saved_env, saved_auth = dict(os.environ), getattr(T, "AUTH", None)
        os.environ.clear()
        os.environ.update(env)
        if saved_auth is not None:
            T.AUTH = T.apptoken.Auth()
        out, err, code = io.StringIO(), io.StringIO(), None
        try:
            with contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
                G.main(["--cache", os.path.join(self.tmp, "cache"), *argv])
        except SystemExit as e:
            code = e.code
        finally:
            os.environ.clear()
            os.environ.update(saved_env)
            if saved_auth is not None:
                T.AUTH = saved_auth
        return code, out.getvalue(), err.getvalue()

    def job_args(self, *more):
        limits = os.path.join(self.tmp, "limits.tsv")
        with open(limits, "w") as fh:
            fh.write("lint-test-backend.yml\tverify-tools\t200\tx\n")
        return [*more, "job", "verify-tools", "--branch-glob", "main", "--limits", limits, "--superci-source", "github"]

    def test_the_gate_refuses_without_an_app_token_and_makes_no_gh_call(self):
        code, out, err = self.run_gate(*self.job_args(), token_cmd=TOKEN_DOWN)
        self.assertEqual(code, 2, (out, err))
        self.assertIn("refusing: no GitHub App token", err)
        self.assertIn("--personal-token", err, "the refusal says how to spend the personal budget on purpose")
        self.assertNotIn("Connection timed out", out)
        self.assertEqual(self.gh_calls(), [], "a refused gate reads nothing from GitHub")

    def test_the_override_is_announced_and_reads_on_the_callers_own_credential(self):
        code, out, err = self.run_gate(*self.job_args("--personal-token"), token_cmd=TOKEN_DOWN)
        self.assertIn("spending the shared personal", err)
        self.assertNotIn("refusing", err)
        calls = self.gh_calls()
        self.assertTrue(calls, "the gate read GitHub once it was allowed to")
        self.assertEqual({tok for tok, _ in calls}, {"UNSET"}, "no App token was made, so no call carries one")

    def test_with_an_app_token_every_gh_call_carries_it(self):
        code, out, err = self.run_gate(*self.job_args())
        calls = self.gh_calls()
        self.assertTrue(calls, (code, out, err))
        self.assertEqual({tok for tok, _ in calls}, {APP_TOKEN}, calls)
        self.assertNotIn(APP_TOKEN, out + err, "the token is never printed")

    def test_with_an_app_token_every_gh_call_keeps_it_past_a_host_wrapper(self):
        code, out, err = self.run_gate(*self.job_args())
        self.assert_app_calls_keep_the_token(APP_TOKEN)
        self.assertEqual({tok for tok, _, _ in self.gh_envs()}, {APP_TOKEN})

    def test_a_personal_token_call_does_not_ask_the_wrapper_to_keep_anything(self):
        self.run_gate(*self.job_args("--personal-token"), token_cmd=TOKEN_DOWN)
        envs = self.gh_envs()
        self.assertTrue(envs)
        self.assertEqual({keep for _, keep, _ in envs}, {"UNSET"}, envs)

    def test_a_scale_window_the_records_cover_needs_no_token_to_start(self):
        code, out, err = self.run_gate("scale", "--from", "2026-11-25T14:00:00Z", "--to", "2026-11-25T14:01:00Z", "--trace", os.path.join(self.tmp, "none.json"),
                                       "--superci-source", "events", token_cmd=TOKEN_DOWN)
        self.assertNotIn("refusing: no GitHub App token", err)
        self.assertEqual(self.gh_calls(), [])

    def read_limits(self, token_cmd, **env):
        """gate.gh_raw for ci-limits.tsv in this process with the sandbox's PATH: (what it returned or the refusal raised, stderr)."""
        saved_env, saved_auth = dict(os.environ), T.AUTH
        os.environ.clear()
        os.environ.update(self.env, SUPERCI_APP_TOKEN_CMD=token_cmd, **env)
        T.AUTH = T.apptoken.Auth()
        err = io.StringIO()
        try:
            with contextlib.redirect_stderr(err):
                try:
                    return G.gh_raw("scripts/verify/ci-limits.tsv"), err.getvalue()
                except T.apptoken.AppTokenRefused as e:
                    return e, err.getvalue()
        finally:
            os.environ.clear()
            os.environ.update(saved_env)
            T.AUTH = saved_auth

    def test_the_limits_file_is_read_on_the_contents_token(self):
        got, err = self.read_limits(TOKEN_SCOPED)
        self.assertNotIsInstance(got, Exception, got)
        self.assertEqual({tok for tok, _ in self.gh_calls()}, {LIMITS_TOKEN}, "ci-limits.tsv is read with the contents token, not the actions token and not the personal one")
        self.assertNotIn("personal-token exception", err)
        self.assertNotIn(LIMITS_TOKEN, err)

    def test_a_refused_contents_token_stops_the_gate_with_the_reason_and_the_limits_hint(self):
        code, out, err = self.run_gate("job", "verify-tools", "--branch-glob", "main", "--superci-source", "github", token_cmd=TOKEN_SCOPED, env={"REFUSE_SCOPES": "contents"})
        self.assertEqual(code, 2, (out, err))
        self.assertIn("refusing: no GitHub App token for contents", err)
        self.assertIn("the repository permission", err, "the listener's reason is shown")
        self.assertIn("--limits FILE", err, "and how to go on without the call")
        self.assertNotIn("ghs_", out + err)
        self.assertNotIn("UNSET", {tok for tok, _ in self.gh_calls()}, "a refused contents token falls back to nothing")
        self.assertFalse([a for _, a in self.gh_calls() if "contents/" in a], "no limits read was made")

    def test_with_limits_file_no_contents_token_is_asked_for(self):
        asks = os.path.join(self.tmp, "asks")
        self.run_gate(*self.job_args(), token_cmd=TOKEN_SCOPED, env={"SCOPE_ASKS": asks})
        with open(asks) as fh:
            self.assertEqual(set(fh.read().split()), {"actions"})


class CanaryReadsOnTheAppBudget(Sandbox):
    def canary(self, token_cmd=None, **extra):
        until = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(time.time() + 2))
        env = dict(self.env, EVERY="600", LOG=self.tmp, **extra)
        if token_cmd:
            env["SUPERCI_APP_TOKEN_CMD"] = token_cmd
        p = subprocess.run(["bash", os.path.join(HERE, "canary.sh"), "2026-11-27T00:00:00Z", until, "verify-tools"], env=env, capture_output=True, text=True, timeout=120)
        return p.returncode, p.stdout, p.stderr

    def test_the_floor_check_reads_with_the_token_the_checks_use(self):
        code, out, err = self.canary(APP_REMAINING="100")
        rate = [tok for tok, args in self.gh_calls() if "rate_limit" in args]
        self.assertTrue(rate, (code, out, err))
        self.assertEqual(set(rate), {APP_TOKEN}, "the floor read another token's budget than the checks spend")
        self.assertIn("skipped, 100 GitHub reads left", out, "and it is the App's budget the floor compares with 1,500")
        self.assertNotIn(APP_TOKEN, out + err)

    def test_the_floor_read_and_the_variable_read_keep_their_tokens_past_a_host_wrapper(self):
        self.canary(TOKEN_SCOPED, APP_REMAINING="100")
        self.assert_app_calls_keep_the_token(APP_TOKEN, "rate_limit")
        self.assert_app_calls_keep_the_token(VARS_TOKEN, "actions/variables/SUPERCI_JOBS")

    def test_the_personal_canary_reads_do_not_ask_the_wrapper_to_keep_anything(self):
        self.canary(TOKEN_DOWN, SUPERCI_PERSONAL_TOKEN="1", PERSONAL_REMAINING="100")
        envs = self.gh_envs()
        self.assertTrue(envs)
        self.assertEqual({keep for _, keep, _ in envs}, {"UNSET"}, envs)

    def test_a_canary_without_an_app_token_refuses_and_reads_nothing(self):
        code, out, err = self.canary(TOKEN_DOWN)
        self.assertEqual(code, 2, (out, err))
        self.assertIn("refusing: no GitHub App token", err)
        self.assertEqual(self.gh_calls(), [])

    def test_the_override_is_announced_and_leaves_the_calls_on_the_personal_credential(self):
        code, out, err = self.canary(TOKEN_DOWN, SUPERCI_PERSONAL_TOKEN="1", PERSONAL_REMAINING="100")
        self.assertIn("spending the shared personal", err)
        self.assertNotIn("refusing", err)
        rate = [tok for tok, args in self.gh_calls() if "rate_limit" in args]
        self.assertTrue(rate and set(rate) == {"UNSET"}, (code, out, err))
        self.assertIn("skipped, 100 GitHub reads left", out)

    def test_the_variable_is_read_on_the_variables_token(self):
        code, out, err = self.canary(TOKEN_SCOPED, APP_REMAINING="100")
        var = [tok for tok, args in self.gh_calls() if "actions/variables/SUPERCI_JOBS" in args]
        self.assertTrue(var, (code, out, err))
        self.assertEqual(set(var), {VARS_TOKEN}, "SUPERCI_JOBS is read with the variables token, neither the actions token nor the personal one")
        self.assertNotIn("personal-token exception", err)
        self.assertNotIn(VARS_TOKEN, out + err)

    def test_a_refused_variables_token_skips_the_check_aloud_and_reads_nothing_on_the_personal_token(self):
        code, out, err = self.canary(TOKEN_SCOPED, REFUSE_SCOPES="variables")
        self.assertIn("no GitHub App token, no check (SUPERCI_JOBS, variables:", out, (out, err))
        self.assertEqual([a for _, a in self.gh_calls() if "actions/variables" in a], [])


class CanaryTree(Sandbox):
    """canary.sh beside a gate.py that is a stub (by default one answering ROLL BACK, exit 3), over a gh stub that records every call and its GH_TOKEN."""

    def setUp(self):
        super().setUp()
        # Writes of the variable are logged by the gh stub like every call; a write with a token in FAIL_WRITE_FOR fails like GitHub's 403.
        self.env.update(SUPERCI_APP_TOKEN_CMD=TOKEN_SCOPED, SCOPE_ASKS=os.path.join(self.tmp, "asks"))
        gh = os.path.join(self.bin, "gh")
        with open(gh) as fh:
            body = fh.read()
        body = body.replace('case "$*" in\n', 'case "$*" in\n  *"variable set"*|*"variable delete"*) [ "${GH_TOKEN-UNSET}" = "${FAIL_WRITE_FOR-}" ] && { echo "HTTP 403: Resource not accessible by integration" >&2; exit 1; }; touch "$GHLOG.written"; echo ok ;;\n', 1)
        # After a write of the variable GitHub no longer finds it (the stub's deleted it), so a later tick has nothing to check.
        body = body.replace('case "$*" in\n', 'if [ -e "$GHLOG.written" ]; then case "$*" in *actions/variables/SUPERCI_JOBS*) echo "gh: Not Found (HTTP 404)" >&2; exit 1 ;; esac; fi\ncase "$*" in\n', 1)
        with open(gh, "w") as fh:
            fh.write(body)

    def tree(self, gate="print('canary FAIL: stub')\nraise SystemExit(3)\n"):
        root = os.path.join(self.tmp, "tree")
        os.makedirs(os.path.join(root, "gate"), exist_ok=True)
        os.makedirs(os.path.join(root, "timeline"), exist_ok=True)
        shutil.copy(os.path.join(HERE, "canary.sh"), os.path.join(root, "gate", "canary.sh"))
        shutil.copy(os.path.join(HERE, "..", "timeline", "apptoken.py"), os.path.join(root, "timeline", "apptoken.py"))
        with open(os.path.join(root, "gate", "gate.py"), "w") as fh:
            fh.write(gate)
        return os.path.join(root, "gate", "canary.sh")

    def rollback(self, gate="print('canary FAIL: stub')\nraise SystemExit(3)\n", lasts=2, jobs=("verify-tools",), **extra):
        until = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(time.time() + lasts))
        env = dict(self.env, EVERY="600", LOG=self.tmp, APP_REMAINING="4000", **extra)
        return subprocess.run(["bash", self.tree(gate), "2026-11-27T00:00:00Z", until, *jobs], env=env, capture_output=True, text=True, timeout=300)

    def gh_prefix(self, text):
        """Put shell text in the gh stub ahead of its main case (after the stub's own handling of a variable written)."""
        gh = os.path.join(self.bin, "gh")
        with open(gh) as fh:
            body = fh.read()
        assert body.count('case "$*" in\n') >= 1
        with open(gh, "w") as fh:
            fh.write(body.replace('case "$*" in\n', text + '\ncase "$*" in\n', 1))

    def other_on_reads(self):
        """Reads of SUPERCI_JOBS number from 1 (the start's probe, then each tick's read, then a rollback's): those from OTHER_FROM to OTHER_TO list another job only, and from FAIL_FROM on answer 502."""
        self.gh_prefix('''case "$*" in *actions/variables/SUPERCI_JOBS*) n=$(($(cat "$GHLOG.nreads" 2>/dev/null || echo 0)+1)); echo $n > "$GHLOG.nreads"
  if [ $n -ge ${OTHER_FROM:-99999} ] && [ $n -le ${OTHER_TO:-99999} ]; then echo '["other"]'; exit 0; fi
  if [ $n -ge ${FAIL_FROM:-99999} ]; then echo "gh: HTTP 502: Bad Gateway" >&2; exit 1; fi ;; esac''')

    def stateful_jobs(self):
        """SUPERCI_JOBS as a file ($JOBS_FILE; absent = unset): the gh stub reads it, `variable set` writes it and `variable delete` removes it."""
        self.gh_prefix('''case "$*" in
  *actions/variables/SUPERCI_JOBS*) if [ -e "$JOBS_FILE" ]; then cat "$JOBS_FILE"; exit 0; else echo "gh: Not Found (HTTP 404)" >&2; exit 1; fi ;;
  "variable set"*) for a; do last=$a; done; printf %s "$last" > "$JOBS_FILE"; echo ok; exit 0 ;;
  "variable delete"*) rm -f "$JOBS_FILE"; echo ok; exit 0 ;;
esac''')
        self.env["JOBS_FILE"] = os.path.join(self.tmp, "jobs.json")

    def rollback_measured(self, **extra):
        """rollback() under the largest memory the watch's descendants may take (1 GiB of address space), returning its (returncode, stdout, stderr) with .maxrss in KiB and .t_end."""
        until = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(time.time() + 60))
        env = dict(self.env, EVERY="600", LOG=self.tmp, APP_REMAINING="4000", **extra)
        wrap = ("import json, resource, subprocess, sys, time\nresource.setrlimit(resource.RLIMIT_AS, (1 << 30, 1 << 30))\n"
                "p = subprocess.run(sys.argv[1:], capture_output=True, text=True)\n"
                "print(json.dumps([p.returncode, p.stdout, p.stderr, resource.getrusage(resource.RUSAGE_CHILDREN).ru_maxrss, time.time()]))\n")
        q = subprocess.run([sys.executable, "-I", "-c", wrap, "bash", self.tree(), "2026-11-27T00:00:00Z", until, "verify-tools"], env=env, capture_output=True, text=True, timeout=120)
        self.assertEqual(q.returncode, 0, q.stderr)
        code, out, err, rss, t_end = json.loads(q.stdout)
        return collections.namedtuple("Run", "returncode stdout stderr maxrss t_end")(code, out, err, rss, t_end)

    def writes(self):
        return [(tok, args) for tok, args in self.gh_calls() if args.startswith("variable ")]

    def refuse_after(self, n):
        """A token command that answers the first n asks for the variables scope and refuses the rest."""
        counter = os.path.join(self.tmp, "vasks")
        cmd = stub("after", f"""if [ "${{SUPERCI_TOKEN_SCOPE:-actions}}" = variables ]; then
  k=$(($(cat {counter} 2>/dev/null || echo 0)+1)); echo $k > {counter}
  [ $k -gt {n} ] && {{ printf 'the superci App installation lacks the repository permission "actions_variables: write"\\n422\\n'; exit 0; }}
fi
exec {TOKEN_SCOPED}
""")
        return {"SUPERCI_APP_TOKEN_CMD": cmd}



class RollbackWritesOnTheVariablesToken(CanaryTree):
    """canary.sh beside a gate.py that is a stub answering ROLL BACK (exit 3), so only the rollback's own writes are run."""

    def test_the_rollback_writes_with_the_variables_token(self):
        p = self.rollback()
        self.assertIn("SUPERCI_JOBS was", p.stdout, (p.stdout, p.stderr))
        writes = self.writes()
        self.assertEqual([args for _, args in writes], ["variable delete SUPERCI_JOBS -R Authentic-Intelligence/megpt-mono"], "verify-tools was the only job listed, so the variable is deleted")
        self.assertEqual({tok for tok, _ in writes}, {VARS_TOKEN}, "the rollback's write carries the variables token")
        self.assertNotIn("UNSET", {tok for tok, _ in self.gh_calls() if "actions/variables" in tok or "variable" in _})
        self.assertNotIn("personal-token exception", p.stderr)
        self.assertNotIn("ROLLBACK FAILED", p.stdout)
        self.assertNotIn(VARS_TOKEN, p.stdout + p.stderr)

    def test_a_refused_variables_token_fails_the_rollback_loudly_and_writes_nothing(self):
        # The token is asked for by the start's probe and the tick's read, and again (VT_TTL=0) for the rollback, which the listener now refuses.
        p = self.rollback(VT_TTL="0", TOKEN_TIMEOUT="5", **self.refuse_after(2))
        self.assertIn("ROLLBACK FAILED", p.stdout, (p.stdout, p.stderr))
        self.assertIn("ROLLBACK FAILED", p.stderr, "and on stderr too")
        self.assertIn("SUPERCI_PERSONAL_TOKEN=1", p.stdout, "it says how to roll back on the personal token")
        self.assertEqual(self.writes(), [], "nothing was written, with any token")
        self.assertNotIn("ghs_", p.stdout + p.stderr)

    def test_the_rollbacks_read_and_delete_keep_the_variables_token_past_a_host_wrapper(self):
        p = self.rollback()
        self.assertIn("SUPERCI_JOBS was", p.stdout, (p.stdout, p.stderr))
        self.assert_app_calls_keep_the_token(VARS_TOKEN, "actions/variables/SUPERCI_JOBS")
        self.assert_app_calls_keep_the_token(VARS_TOKEN, "variable delete")
        self.assert_app_calls_keep_the_token(APP_TOKEN, "rate_limit")

    def test_the_personal_rollback_write_does_not_ask_the_wrapper_to_keep_anything(self):
        self.rollback(SUPERCI_PERSONAL_TOKEN="1", PERSONAL_REMAINING="4000", VT_TTL="0", TOKEN_TIMEOUT="5", **self.refuse_after(0))
        writes = [c for c in self.gh_envs() if c[2].startswith("variable ")]
        self.assertTrue(writes)
        self.assertEqual({(tok, keep) for tok, keep, _ in writes}, {("UNSET", "UNSET")}, writes)

    def test_the_personal_token_is_used_for_the_rollback_only_when_the_operator_set_it(self):
        p = self.rollback(SUPERCI_PERSONAL_TOKEN="1", PERSONAL_REMAINING="4000", VT_TTL="0", TOKEN_TIMEOUT="5", **self.refuse_after(0))
        writes = self.writes()
        self.assertEqual([tok for tok, _ in writes], ["UNSET"], (p.stdout, p.stderr, self.gh_calls()))
        self.assertIn("personal-token exception SUPERCI_JOBS write", p.stderr, "the fallback says it is the personal budget")
        self.assertNotIn("ROLLBACK FAILED", p.stdout)
        self.assertIn("SUPERCI_JOBS was", p.stdout)

    def test_a_write_the_variables_token_is_refused_by_github_falls_back_only_when_asked(self):
        p = self.rollback(FAIL_WRITE_FOR=VARS_TOKEN)
        self.assertIn("ROLLBACK FAILED", p.stdout, (p.stdout, p.stderr))
        self.assertEqual({tok for tok, _ in self.writes()}, {VARS_TOKEN}, "one attempt on the App's token, none on the personal one")
        open(self.log, "w").close()
        p = self.rollback(FAIL_WRITE_FOR=VARS_TOKEN, SUPERCI_PERSONAL_TOKEN="1", PERSONAL_REMAINING="4000")
        self.assertEqual([tok for tok, _ in self.writes()][-1], "UNSET", (p.stdout, p.stderr))
        self.assertIn("personal-token exception SUPERCI_JOBS write", p.stderr)

    def test_an_endpoint_that_hangs_does_not_hang_the_rollback(self):
        hang = stub("hang", """if [ "${SUPERCI_TOKEN_SCOPE:-actions}" = variables ]; then exec /bin/sleep 60; fi
exec @OK@
""".replace("@OK@", TOKEN_SCOPED))
        start = time.time()
        p = subprocess.run(["bash", os.path.join(HERE, "canary.sh"), "unset"], env=dict(self.env, SUPERCI_APP_TOKEN_CMD=hang, TOKEN_TIMEOUT="1"),
                           capture_output=True, text=True, timeout=60)
        self.assertLess(time.time() - start, 30, "three asks of a second each, with the stubbed sleeps between")
        self.assertEqual(p.returncode, 1, (p.stdout, p.stderr))
        self.assertIn("ROLLBACK FAILED", p.stdout)
        self.assertEqual(self.writes(), [])


class RollbackWhenNoReadAnswers(CanaryTree):
    """The rollback when GitHub answers none of its reads of SUPERCI_JOBS, and a gate that dies without a verdict."""

    def test_a_rollback_whose_five_reads_all_fail_deletes_the_variable_and_the_watch_goes_on(self):
        # The probe's read and the tick's read answer; every read after them (the rollback's five) gets a 502.
        gh = os.path.join(self.bin, "gh")
        with open(gh) as fh:
            body = fh.read()
        body = body.replace('case "$*" in\n', 'case "$*" in *actions/variables/SUPERCI_JOBS*) n=$(($(cat "$GHLOG.reads" 2>/dev/null || echo 0)+1)); echo $n > "$GHLOG.reads"; [ $n -gt 2 ] && [ ! -e "$GHLOG.written" ] && { echo "gh: HTTP 502: Bad Gateway" >&2; exit 1; } ;; esac\ncase "$*" in\n', 1)
        with open(gh, "w") as fh:
            fh.write(body)
        p = self.rollback()
        self.assertNotIn("unbound variable", p.stderr, (p.stdout, p.stderr))
        self.assertIn("GitHub did not answer reading SUPERCI_JOBS (try 5)", p.stdout, (p.stdout, p.stderr))
        self.assertIn("SUPERCI_JOBS unread after 5 tries: deleting it", p.stdout, (p.stdout, p.stderr))
        self.assertEqual([args for _, args in self.writes()], ["variable delete SUPERCI_JOBS -R Authentic-Intelligence/megpt-mono"], "every job goes back to Blacksmith")
        self.assertEqual({tok for tok, _ in self.writes()}, {VARS_TOKEN}, "on the variables token")
        self.assertNotIn("ROLLBACK FAILED", p.stdout)

    def test_a_gate_that_dies_without_a_verdict_is_no_check(self):
        for code in (1, 137):
            p = self.rollback(gate=f"import sys\nprint('Traceback: boom ghs_leak_in_text')\nsys.exit({code})\n")
            self.assertIn("NO CHECK (the gate failed: Traceback: boom", p.stdout, (code, p.stdout, p.stderr))
            self.assertIn(f"(exit {code},", p.stdout)
            self.assertNotIn("ghs_leak_in_text", p.stdout, "a token-shaped word is scrubbed")
            self.assertEqual(self.writes(), [], "no verdict rolls nothing back")


class OperatorHearsOfARollback(CanaryTree):
    """ON_ROLLBACK and STOP_ON_ROLLBACK: a rollback (Nov 28 00:57:56Z, test-go) reaches the operator in minutes, not after 59 of reading a log."""

    def setUp(self):
        super().setUp()
        self.notes = os.path.join(self.tmp, "notes")
        # One line per call: the number of arguments, then the summary (the first of them).
        self.notifier = stub("notifier", '''printf '%s\\t%s\\n' "$#" "$1" >> "$NOTIFY_NOTES"\n[ -n "${NOTIFY_SAYS-}" ] && echo "$NOTIFY_SAYS"\nexit ${NOTIFY_EXIT:-0}\n''')
        self.env.update(NOTIFY_NOTES=self.notes, ON_ROLLBACK=self.notifier)

    def calls(self):
        if not os.path.exists(self.notes):
            return []
        with open(self.notes) as fh:
            return [tuple(line.rstrip("\n").split("\t", 1)) for line in fh if line.strip()]

    def test_a_rollback_calls_on_rollback_once_with_the_job_the_reason_and_the_variable_before_and_after(self):
        p = self.rollback()
        self.assertEqual(p.returncode, 0, (p.stdout, p.stderr))
        calls = self.calls()
        self.assertEqual(len(calls), 1, (calls, p.stdout, p.stderr))
        argc, line = calls[0]
        self.assertEqual(argc, "1", "one argument: the summary")
        self.assertRegex(line, r"^20\d\d-\d\d-\d\dT\d\d:\d\d:\d\dZ ", "the UTC time leads")
        self.assertIn("verify-tools", line)
        self.assertIn("canary FAIL: stub", line, "the reason line the gate printed")
        self.assertIn('SUPERCI_JOBS was ["verify-tools"], now unset', line)
        self.assertEqual([args for _, args in self.writes()], ["variable delete SUPERCI_JOBS -R Authentic-Intelligence/megpt-mono"])
        self.assertNotIn(VARS_TOKEN, line)

    def test_stop_on_rollback_exits_3_after_the_notice(self):
        p = self.rollback(lasts=60, STOP_ON_ROLLBACK="1")
        self.assertEqual(p.returncode, 3, (p.stdout, p.stderr))
        self.assertEqual(len(self.calls()), 1, "the notice was made before the exit")
        self.assertIn("SUPERCI_JOBS was", p.stdout, "and so was the rollback")
        self.assertEqual(len(self.writes()), 1)

    def test_without_stop_on_rollback_the_watch_goes_on_to_its_end(self):
        p = self.rollback(lasts=3)
        self.assertEqual(p.returncode, 0, (p.stdout, p.stderr))
        self.assertEqual(len(self.calls()), 1)

    def test_a_failing_on_rollback_neither_stops_the_watch_nor_undoes_the_rollback(self):
        p = self.rollback(lasts=6, NOTIFY_EXIT="7", NOTIFY_SAYS="boom ghs_leak_in_text")
        self.assertEqual(p.returncode, 0, (p.stdout, p.stderr))
        self.assertEqual(len(self.calls()), 1, "the rollback's notice, and no streak notice after it: the later ticks find SUPERCI_JOBS unset, which is no failure to check")
        self.assertIn("ON_ROLLBACK failed (exit 7): boom", p.stdout, "the failure is logged")
        self.assertNotIn("ghs_leak_in_text", p.stdout + p.stderr)
        self.assertEqual([args for _, args in self.writes()], ["variable delete SUPERCI_JOBS -R Authentic-Intelligence/megpt-mono"], "the rollback stands")
        self.assertNotIn("ROLLBACK FAILED", p.stdout)
        after = p.stdout.split("ON_ROLLBACK failed", 1)[1]
        self.assertIn("SUPERCI_JOBS unset, nothing to check", after, "a later tick still ran")

    def test_a_failing_on_rollback_still_lets_stop_on_rollback_exit_3(self):
        p = self.rollback(lasts=60, STOP_ON_ROLLBACK="1", NOTIFY_EXIT="7")
        self.assertEqual(p.returncode, 3, (p.stdout, p.stderr))
        self.assertIn("ON_ROLLBACK failed (exit 7)", p.stdout)

    def test_a_rollback_whose_write_fails_still_notifies_and_says_the_job_is_still_on_superci(self):
        p = self.rollback(lasts=3, FAIL_WRITE_FOR=VARS_TOKEN, STOP_ON_ROLLBACK="1")
        self.assertIn("ROLLBACK FAILED", p.stdout, (p.stdout, p.stderr))
        calls = self.calls()
        self.assertTrue(calls, (p.stdout, p.stderr))
        for argc, line in calls:
            self.assertEqual(argc, "1")
            self.assertIn("could NOT roll verify-tools back", line)
            self.assertIn("still on superci", line)
            self.assertIn("canary FAIL: stub", line)
            self.assertIn("ROLLBACK FAILED", line, "the cause the operator reads in the log")
        self.assertEqual(p.returncode, 0, "nothing was rolled back, so STOP_ON_ROLLBACK does not end the watch: the next tick tries again")
        self.assertNotIn(VARS_TOKEN, "".join(line for _, line in calls))

    def test_the_notifier_gets_path_home_lang_and_notify_variables_and_no_credential(self):
        self.env["ON_ROLLBACK"] = stub("env_notifier", 'env > "$NOTIFY_NOTES.env"\n')
        secrets = {"GH_TOKEN": "ghp_fake_gh_token_for_tests", "GITHUB_TOKEN": "ghp_fake_github_token_for_tests", "AWS_SECRET_ACCESS_KEY": "fake/aws+secret/for/tests",
                   "AWS_ACCESS_KEY_ID": "AKIA" + "FAKEFAKEFAKEFAKE", "AWS_SESSION_TOKEN": "fake-session-for-tests", "AWS_PROFILE": "ci-runners"}
        p = self.rollback(lasts=2, LANG="C.UTF-8", NOTIFY_FLAVOUR="kept", **secrets)
        self.assertEqual(p.returncode, 0, (p.stdout, p.stderr))
        with open(self.notes + ".env") as fh:
            seen = dict(line.rstrip("\n").split("=", 1) for line in fh if "=" in line)
        for name, value in secrets.items():
            self.assertNotIn(name, seen, f"{name} reached the notifier")
        self.assertNotIn("SUPERCI_APP_TOKEN_CMD", seen)
        self.assertEqual({"PATH", "HOME", "LANG", "NOTIFY_FLAVOUR", "NOTIFY_NOTES"} - set(seen), set(), "the allowlist reaches it")
        self.assertEqual(seen["NOTIFY_FLAVOUR"], "kept")
        self.assertFalse(set(seen) - {"PATH", "HOME", "LANG", "PWD", "SHLVL", "_", "OLDPWD"} - {n for n in seen if n.startswith("NOTIFY_")}, seen.keys())

    def timed_notifier(self, body):
        """A notifier that stamps its start (NOTIFY_NOTES.start) and then runs body."""
        self.env["ON_ROLLBACK"] = stub("timed_notifier", 'date +%s.%N > "$NOTIFY_NOTES.start"\n' + body)
        return self.env["ON_ROLLBACK"]

    def started_to(self, t_end):
        with open(self.notes + ".start") as fh:
            return t_end - float(fh.read())

    def test_a_notifier_that_traps_term_is_killed_within_the_timeout_and_6_s(self):
        self.timed_notifier("trap '' TERM\n/bin/sleep 20\n")
        p = self.rollback(lasts=60, STOP_ON_ROLLBACK="1", NOTIFY_TIMEOUT="2")
        t_end = time.time()
        self.assertEqual(p.returncode, 3, (p.stdout, p.stderr))
        self.assertLess(self.started_to(t_end), 2 + 6, "TERM is trapped, so KILL follows 5 s later")
        self.assertIn("ON_ROLLBACK failed", p.stdout)
        self.assertIn("timed out after 2 s", p.stdout)
        self.assertEqual([args for _, args in self.writes()], ["variable delete SUPERCI_JOBS -R Authentic-Intelligence/megpt-mono"], "the rollback stands")

    def test_a_notifier_that_leaves_a_background_child_holding_its_output_does_not_hold_the_watch(self):
        pidfile = os.path.join(self.tmp, "child.pid")
        self.timed_notifier(f"/bin/sleep 20 &\necho $! > {pidfile}\nexit 0\n")
        p = self.rollback(lasts=60, STOP_ON_ROLLBACK="1", NOTIFY_TIMEOUT="2")
        t_end = time.time()
        self.assertEqual(p.returncode, 3, (p.stdout, p.stderr))
        self.assertLess(self.started_to(t_end), 2 + 6, "the child's open output must not make the watch wait for it")
        self.assertEqual(len(self.writes()), 1, "the rollback stands")
        with open(pidfile) as fh:
            pid = int(fh.read())
        deadline = time.time() + 3
        while time.time() < deadline:
            try:
                with open(f"/proc/{pid}/stat") as fh:
                    state = fh.read().rsplit(")", 1)[1].split()[0]
            except OSError:
                state = "Z"
            if state == "Z":
                break
            time.sleep(0.1)
        self.assertEqual(state, "Z", "the child the notifier left behind is killed with its group, not left running")

    def test_a_notifier_that_floods_its_output_is_bounded_in_time_and_memory(self):
        self.timed_notifier("exec yes\n")
        p = self.rollback_measured(STOP_ON_ROLLBACK="1", NOTIFY_TIMEOUT="2")
        self.assertEqual(p.returncode, 3, (p.stdout[:300], p.stderr[:300]))
        self.assertLess(self.started_to(p.t_end), 2 + 6)
        self.assertLess(p.maxrss, 100_000, f"the watch's biggest process held {p.maxrss} KiB: the notifier's output is not buffered")
        line = [l for l in p.stdout.splitlines() if "ON_ROLLBACK failed" in l]
        self.assertEqual(len(line), 1, p.stdout[:500])
        self.assertLess(len(line[0]), 400, "the log line holds only the first bytes")
        self.assertEqual(len(self.writes()), 1, "the rollback stands")

    def test_the_log_scrubs_every_kind_of_token_the_gate_or_the_notifier_prints(self):
        pat = "github_pat_" + "11AAAAAAA0_fakefakefakefake"
        jwt = "eyJ" + "hbGciOiJIUzI1NiJ9.eyJzdWIiOiJmYWtlIn0.fakesignaturefake"
        akia = "AKIA" + "FAKEFAKEFAKEFAKE"
        asia = "ASIA" + "FAKEFAKEFAKEFAKE"
        said = f"{pat} {jwt} {akia} {asia}"
        p = self.rollback(gate=f"print('canary FAIL: stub {said}')\nraise SystemExit(3)\n", lasts=2, NOTIFY_EXIT="7", NOTIFY_SAYS=said)
        self.assertEqual(p.returncode, 0, (p.stdout, p.stderr))
        self.assertIn("ON_ROLLBACK failed (exit 7)", p.stdout, "the notifier's own output is in the log, scrubbed")
        seen = p.stdout + p.stderr + "".join(line for _, line in self.calls())
        for secret in (pat, jwt, akia, asia, pat[:20], jwt[:25]):
            self.assertNotIn(secret, seen)
        self.assertEqual(len(self.calls()), 1)
        self.assertIn("<token>", self.calls()[0][1], "the summary the notifier got is scrubbed too")

    def test_stop_on_rollback_checks_the_tick_s_other_jobs_before_it_exits_3(self):
        self.stateful_jobs()
        with open(self.env["JOBS_FILE"], "w") as fh:
            fh.write('["job-a","job-b"]')
        seen = os.path.join(self.tmp, "gate-jobs")
        gate = (f"import sys\njob = sys.argv[sys.argv.index('job') + 1]\nopen({seen!r}, 'a').write(job + '\\n')\n"
                "if job == 'job-a':\n    print('canary FAIL: stub')\n    raise SystemExit(3)\nprint('canary keep')\n")
        p = self.rollback(gate=gate, lasts=60, jobs=("job-a", "job-b"), STOP_ON_ROLLBACK="1")
        self.assertEqual(p.returncode, 3, (p.stdout, p.stderr))
        with open(seen) as fh:
            self.assertEqual(fh.read().split(), ["job-a", "job-b"], "the second job was checked in the tick that rolled the first back")
        self.assertEqual(len(self.calls()), 1)
        self.assertIn("job-a", self.calls()[0][1])
        self.assertEqual([args for _, args in self.writes()], ['variable set SUPERCI_JOBS -R Authentic-Intelligence/megpt-mono --body ["job-b"]'])

    def test_stop_on_rollback_also_rolls_back_a_second_job_of_the_same_tick(self):
        self.stateful_jobs()
        with open(self.env["JOBS_FILE"], "w") as fh:
            fh.write('["job-a","job-b"]')
        p = self.rollback(lasts=60, jobs=("job-a", "job-b"), STOP_ON_ROLLBACK="1")
        self.assertEqual(p.returncode, 3, (p.stdout, p.stderr))
        self.assertEqual(len(self.calls()), 2, "both rollbacks were told")
        self.assertFalse(os.path.exists(self.env["JOBS_FILE"]), "both are off superci")

    def test_a_job_another_watch_already_took_out_of_the_variable_is_not_this_watch_s_rollback(self):
        self.other_on_reads()  # the probe (1) and the tick's read (2) list the job; the rollback's re-read (3) and later reads do not
        p = self.rollback(lasts=4, STOP_ON_ROLLBACK="1", OTHER_FROM="3")
        self.assertEqual(p.returncode, 0, "nothing was rolled back here, so STOP_ON_ROLLBACK does not end the watch")
        self.assertEqual(self.calls(), [], "and nobody is told of a rollback that was not made")
        self.assertEqual(self.writes(), [], "no write: the variable lists the other job only")
        self.assertIn("verify-tools is no longer in SUPERCI_JOBS", p.stdout)
        self.assertNotIn("SUPERCI_JOBS was", p.stdout)

    def test_no_on_rollback_changes_nothing(self):
        del self.env["ON_ROLLBACK"]
        p = self.rollback()
        self.assertEqual((p.returncode, self.calls()), (0, []), (p.stdout, p.stderr))
        self.assertIn("SUPERCI_JOBS was", p.stdout)


class CanaryTokenLifetime(CanaryTree):
    """The variables token as the listener hands it out: it may expire 10 minutes after the ask. A virtual clock (the date and sleep stubs) moves the
    canary through ticks 600 s apart in no real time, and the listener stub numbers its tokens (ghs_vars_N), each valid for 600 s of that clock."""

    def setUp(self):
        super().setUp()
        self.clock = os.path.join(self.tmp, "clock")
        self.toks = os.path.join(self.tmp, "toks")
        os.makedirs(self.toks)
        for name, body in {
            "date": 'off=$(cat "$VCLOCK" 2>/dev/null || echo 0)\n[ "$1 $2" = "-u +%s" ] && { echo $(( $(/bin/date +%s) + off )); exit 0; }\nexec /bin/date "$@"\n',
            "sleep": 'if [ -n "${NO_ADVANCE-}" ]; then exec /bin/sleep 1; fi\necho $(( $(cat "$VCLOCK" 2>/dev/null || echo 0) + ${1%%[!0-9]*} )) > "$VCLOCK"\n',
        }.items():
            path = os.path.join(self.bin, name)
            with open(path, "w") as fh:
                fh.write("#!/bin/sh\n" + body)
            os.chmod(path, 0o755)
        gh = os.path.join(self.bin, "gh")
        with open(gh) as fh:
            body = fh.read()
        shebang, first, rest = body.split("\n", 2)
        first = shebang + "\n" + first
        # A variables token is refused (401) once its 600 s are up on the virtual clock, or when it is listed in dead (any call) or deadwrite (a write).
        check = """case "${GH_TOKEN-}" in ghs_vars_*)
  now=$(( $(/bin/date +%s) + $(cat "$VCLOCK" 2>/dev/null || echo 0) ))
  if [ "$now" -ge "$(cat "$TOKS/exp.$GH_TOKEN" 2>/dev/null || echo 0)" ] || grep -qx "$GH_TOKEN" "$TOKS/dead" 2>/dev/null; then echo "gh: Bad credentials (HTTP 401)" >&2; exit 1; fi
  case "$*" in variable*) if grep -qx "$GH_TOKEN" "$TOKS/deadwrite" 2>/dev/null; then echo "gh: Bad credentials (HTTP 401)" >&2; exit 1; fi ;; esac ;;
esac
case "$*" in *actions/variables/SUPERCI_JOBS*) if [ -n "${FORBID_READ_FOR-}" ] && [ "${GH_TOKEN-}" = "$FORBID_READ_FOR" ]; then echo "gh: Resource not accessible by integration (HTTP 403)" >&2; exit 1; fi ;; esac"""
        with open(gh, "w") as fh:
            fh.write(first + "\n" + check + "\n" + rest)
        listener = stub("lifetime_listener", """scope=${SUPERCI_TOKEN_SCOPE:-actions}
echo "$scope" >> "$TOKS/asks"
if [ "$scope" = variables ]; then
  n=$(($(cat "$TOKS/n" 2>/dev/null || echo 0)+1)); echo $n > "$TOKS/n"
  if [ -n "${REFUSE_AFTER-}" ] && [ $n -gt "$REFUSE_AFTER" ]; then printf 'the superci App installation lacks the repository permission "actions_variables: write"\\n422\\n'; exit 0; fi
  now=$(( $(/bin/date +%s) + $(cat "$VCLOCK" 2>/dev/null || echo 0) ))
  echo $((now + 600)) > "$TOKS/exp.ghs_vars_$n"
  printf '{"token":"ghs_vars_%s","expires":"2999-01-01T00:00:00Z"}\\n200\\n' $n
else
  printf '{"token":"@APP@","expires":"2999-01-01T00:00:00Z"}\\n200\\n'
fi
""".replace("@APP@", APP_TOKEN))
        self.env.update(SUPERCI_APP_TOKEN_CMD=listener, VCLOCK=self.clock, TOKS=self.toks)

    def mark(self, name, *tokens):
        with open(os.path.join(self.toks, name), "w") as fh:
            fh.write("".join(t + "\n" for t in tokens))

    def asks(self, scope="variables"):
        with open(os.path.join(self.toks, "asks")) as fh:
            return fh.read().split().count(scope)

    def test_a_token_ten_minutes_from_expiry_at_the_ask_is_not_used_past_it(self):
        # Three ticks 600 s apart: a token kept longer than 600 s (the listener's reserve) is expired at the next tick and answers 401.
        p = self.rollback(gate="print('canary keep')\nraise SystemExit(0)\n", lasts=1500)
        self.assertNotIn("SUPERCI_JOBS unread", p.stdout, "a tick read SUPERCI_JOBS with an expired token")
        self.assertEqual(p.stdout.count("canary keep"), 3, p.stdout)
        reads = [tok for tok, args in self.gh_calls() if "actions/variables/SUPERCI_JOBS" in args]
        self.assertEqual(len(set(reads)), len(reads), "each tick, 600 s apart, reads with a token asked for since the last: " + str(reads))
        self.assertNotIn("ghs_vars_", p.stdout + p.stderr)

    def test_a_401_on_the_read_drops_the_token_asks_again_and_reads_with_the_fresh_one(self):
        self.mark("dead", "ghs_vars_1")
        p = self.rollback(gate="print('canary keep')\nraise SystemExit(0)\n", NO_ADVANCE="1")
        self.assertNotIn("SUPERCI_JOBS unread", p.stdout, (p.stdout, p.stderr))
        self.assertIn("canary keep", p.stdout)
        reads = [tok for tok, args in self.gh_calls() if "actions/variables/SUPERCI_JOBS" in args]
        self.assertEqual(reads[:2], ["ghs_vars_1", "ghs_vars_2"], "the refused token is replaced once, and the fresh one is kept")
        self.assertEqual(set(reads[2:]), {"ghs_vars_2"})

    def test_a_401_on_the_rollbacks_write_asks_again_and_writes_with_the_fresh_token(self):
        self.mark("deadwrite", "ghs_vars_1")
        p = self.rollback(NO_ADVANCE="1")
        self.assertNotIn("ROLLBACK FAILED", p.stdout, (p.stdout, p.stderr))
        self.assertEqual([tok for tok, _ in self.writes()], ["ghs_vars_1", "ghs_vars_2"], "one failed write on the old token, then the fresh one")
        self.assertIn("SUPERCI_JOBS was", p.stdout)

    def test_the_failure_line_names_a_fresh_token_github_refused(self):
        self.mark("deadwrite", "ghs_vars_1", "ghs_vars_2")
        p = self.rollback(NO_ADVANCE="1")
        self.assertIn("ROLLBACK FAILED: GitHub refused the write with a fresh variables token (gh: Bad credentials (HTTP 401))", p.stdout, (p.stdout, p.stderr))
        self.assertEqual([tok for tok, _ in self.writes()][:2], ["ghs_vars_1", "ghs_vars_2"])

    def test_the_failure_line_names_a_listener_that_would_not_give_a_new_token(self):
        self.mark("deadwrite", "ghs_vars_1")
        p = self.rollback(NO_ADVANCE="1", REFUSE_AFTER="1")
        self.assertIn("ROLLBACK FAILED: GitHub refused the write with the variables token (gh: Bad credentials (HTTP 401)), which may have expired, and the listener would not give a new one", p.stdout, (p.stdout, p.stderr))
        self.assertIn("actions_variables: write", p.stdout, "with the listener's own reason")
        self.assertEqual([tok for tok, _ in self.writes()][:1], ["ghs_vars_1"])

    def test_the_failure_line_names_a_listener_that_gave_no_token_at_all(self):
        p = self.rollback(NO_ADVANCE="1", REFUSE_AFTER="0", VT_TTL="0")
        self.assertNotIn("ghs_", p.stdout)
        self.assertEqual(self.writes(), [])
        p = subprocess.run(["bash", self.tree(), "unset"], env=dict(self.env, REFUSE_AFTER="0"), capture_output=True, text=True, timeout=60)
        self.assertIn("ROLLBACK FAILED: the listener would not give a variables token", p.stdout, (p.stdout, p.stderr))

    def notifier(self, **extra):
        self.notes = os.path.join(self.tmp, "notes")
        self.env.update(NOTIFY_NOTES=self.notes, ON_ROLLBACK=stub("notifier", 'printf \'%s\\t%s\\n\' "$#" "$1" >> "$NOTIFY_NOTES"\n'), **extra)

    def calls(self):
        if not os.path.exists(self.notes):
            return []
        with open(self.notes) as fh:
            return [tuple(line.rstrip("\n").split("\t", 1)) for line in fh if line.strip()]

    def test_a_no_check_streak_of_3_ticks_notifies_once(self):
        # Four ticks 600 s apart on the virtual clock, the gate crashing on each: the 3rd notifies, the 4th does not repeat it.
        self.notifier()
        p = self.rollback(gate="import sys\nprint('Traceback: boom')\nsys.exit(1)\n", lasts=2000)
        self.assertEqual(p.stdout.count("NO CHECK (the gate failed"), 4, (p.stdout, p.stderr))
        calls = self.calls()
        self.assertEqual(len(calls), 1, (calls, p.stdout))
        argc, line = calls[0]
        self.assertEqual(argc, "1")
        self.assertIn("verify-tools", line)
        self.assertIn("3 ticks", line)
        self.assertIn("the gate failed: Traceback: boom", line, "why")
        self.assertEqual(self.writes(), [], "no verdict rolls nothing back")

    def test_a_check_between_no_checks_starts_the_streak_over(self):
        self.notifier()
        n = os.path.join(self.tmp, "ticks")
        gate = f"import sys\nk = int(open({n!r}).read() or 0) + 1 if __import__('os').path.exists({n!r}) else 1\nopen({n!r}, 'w').write(str(k))\nif k == 3:\n    print('canary keep')\n    sys.exit(0)\nsys.exit(1)\n"
        p = self.rollback(gate=gate, lasts=3000)
        self.assertEqual(p.stdout.count("NO CHECK (the gate failed"), 4, (p.stdout, p.stderr))
        self.assertIn("canary keep", p.stdout)
        self.assertEqual(self.calls(), [], "2 no-checks, a check, 2 no-checks: never 3 running")

    def test_a_rollback_followed_by_four_ticks_gives_exactly_one_notice(self):
        # The watch's own rollback leaves SUPERCI_JOBS unset: the 4 ticks after it are not 4 failures to check.
        self.notifier()
        p = self.rollback(lasts=2500)
        calls = self.calls()
        self.assertEqual(len(calls), 1, (calls, p.stdout))
        self.assertIn("rolled verify-tools back", calls[0][1])
        self.assertEqual(p.stdout.count("SUPERCI_JOBS unset, nothing to check"), 4, (p.stdout, p.stderr))

    def test_a_deliberate_unset_is_not_a_streak_of_no_checks(self):
        self.notifier()
        gh = os.path.join(self.bin, "gh")
        with open(gh) as fh:
            body = fh.read()
        with open(gh, "w") as fh:
            fh.write(body.replace('case "$*" in\n', 'case "$*" in\n  *actions/variables/SUPERCI_JOBS*) echo "gh: Not Found (HTTP 404)" >&2; exit 1 ;;\n', 1))
        p = self.rollback(gate="raise SystemExit(0)\n", lasts=2500)
        self.assertEqual(self.calls(), [], "a variable someone deleted on purpose (`canary.sh unset`) is not a watch that cannot check")
        self.assertEqual(p.stdout.count("SUPERCI_JOBS unset, nothing to check"), 5, (p.stdout, p.stderr))

    def test_an_unreadable_variable_still_counts_as_no_check(self):
        self.notifier()
        self.other_on_reads()  # the start's probe answers, every read after it is a 502
        p = self.rollback(gate="raise SystemExit(0)\n", lasts=2000, FAIL_FROM="2")
        self.assertEqual(p.stdout.count("SUPERCI_JOBS unread, no check"), 4, (p.stdout, p.stderr))
        calls = self.calls()
        self.assertEqual(len(calls), 1, calls)
        self.assertIn("3 ticks", calls[0][1])

    def test_a_job_that_leaves_the_list_starts_its_streak_over(self):
        # Ticks 1 and 2: the gate crashes (2 no-checks). Tick 3: the variable lists another job only. Ticks 4 and 5: the gate crashes again.
        self.notifier()
        self.other_on_reads()
        p = self.rollback(gate="import sys\nprint('Traceback: boom')\nsys.exit(1)\n", lasts=2500, OTHER_FROM="4", OTHER_TO="4")
        self.assertEqual(p.stdout.count("NO CHECK (the gate failed"), 4, (p.stdout, p.stderr))
        self.assertEqual(self.calls(), [], "2 no-checks, a tick with the job off the list, 2 no-checks: never 3 running")

    def test_a_streak_with_no_variables_token_says_why(self):
        self.notifier()
        p = self.rollback(gate="raise SystemExit(0)\n", lasts=1500, REFUSE_AFTER="0", VT_TTL="0")
        self.assertEqual(p.returncode, 0, (p.stdout, p.stderr))
        calls = self.calls()
        self.assertEqual(len(calls), 1, (calls, p.stdout, p.stderr))
        self.assertIn("no GitHub App token", calls[0][1])

    def test_an_exit_2_of_the_gate_is_no_check_and_says_why(self):
        p = self.rollback(gate="import sys\nprint('refusing: no GitHub App token for contents (the listener lacks it). ghs_leak_in_text', file=sys.stderr)\nsys.exit(2)\n")
        self.assertIn("NO CHECK (the gate refused: refusing: no GitHub App token for contents", p.stdout, (p.stdout, p.stderr))
        self.assertIn("(exit 2,", p.stdout)
        self.assertNotIn("ghs_leak_in_text", p.stdout, "a token-shaped word is scrubbed")
        self.assertEqual(self.writes(), [], "a refusal rolls nothing back")

    def test_a_watch_that_cannot_read_SUPERCI_JOBS_does_not_start(self):
        # An old listener answers the read token for scope=variables, and every read of the variable with it is refused (403).
        old = TOKEN_OK
        ran = os.path.join(self.tmp, "gate-ran")
        gate = f"open({ran!r}, 'w').close()\nraise SystemExit(0)\n"
        p = self.rollback(gate=gate, SUPERCI_APP_TOKEN_CMD=old, FORBID_READ_FOR=APP_TOKEN, NO_ADVANCE="1")
        self.assertEqual(p.returncode, 2, (p.stdout, p.stderr))
        self.assertIn("refusing: SUPERCI_JOBS cannot be read with the variables token", p.stderr)
        self.assertFalse(os.path.exists(ran), "the gate was never run: the watch did not start")
        self.assertEqual(p.stdout, "", "and no tick was logged")

    def test_a_watch_whose_variable_is_simply_unset_starts(self):
        p = self.rollback(gate="raise SystemExit(0)\\n", NO_ADVANCE="1")
        gh = os.path.join(self.bin, "gh")
        with open(gh) as fh:
            body = fh.read()
        with open(gh, "w") as fh:
            fh.write(body.replace('case "$*" in\n', 'case "$*" in\n  *actions/variables/SUPERCI_JOBS*) echo "gh: Not Found (HTTP 404)" >&2; exit 1 ;;\n', 1))
        p = self.rollback(gate="raise SystemExit(0)\\n", NO_ADVANCE="1")
        self.assertEqual(p.returncode, 0, (p.stdout, p.stderr))
        self.assertIn("SUPERCI_JOBS unset, nothing to check", p.stdout)

    def test_under_the_personal_override_the_rollback_asks_the_app_once_and_does_not_wait_on_it(self):
        flaky = stub("flaky_listener", 'echo variables >> "$TOKS/asks"\nprintf "bad gateway\\n502\\n"\n')
        p = subprocess.run(["bash", self.tree(), "unset"], env=dict(self.env, SUPERCI_APP_TOKEN_CMD=flaky, SUPERCI_PERSONAL_TOKEN="1"), capture_output=True, text=True, timeout=120)
        self.assertEqual(p.returncode, 0, (p.stdout, p.stderr))
        self.assertEqual(self.asks(), 1, "no retries: the personal fallback is not kept waiting on the App")
        self.assertEqual([tok for tok, _ in self.writes()], ["UNSET"])
        self.assertIn("personal-token exception SUPERCI_JOBS write", p.stderr)

    def test_a_hung_listener_delays_the_personal_fallback_by_seconds(self):
        hang = stub("hang_listener", 'echo variables >> "$TOKS/asks"\nexec /bin/sleep 120\n')
        start = time.time()
        p = subprocess.run(["bash", self.tree(), "unset"], env=dict(self.env, SUPERCI_APP_TOKEN_CMD=hang, SUPERCI_PERSONAL_TOKEN="1"), capture_output=True, text=True, timeout=400)
        self.assertLess(time.time() - start, 30, "one ask, bounded to 10 s")
        self.assertEqual((p.returncode, self.asks()), (0, 1), (p.stdout, p.stderr))
        self.assertEqual([tok for tok, _ in self.writes()], ["UNSET"])

    def test_without_the_override_the_rollback_still_retries_the_ask(self):
        flaky = stub("flaky_listener", 'echo variables >> "$TOKS/asks"\nprintf "bad gateway\\n502\\n"\n')
        p = subprocess.run(["bash", self.tree(), "unset"], env=dict(self.env, SUPERCI_APP_TOKEN_CMD=flaky), capture_output=True, text=True, timeout=120)
        self.assertEqual((p.returncode, self.asks()), (1, 3), (p.stdout, p.stderr))
        self.assertIn("ROLLBACK FAILED", p.stdout)


class TheRerouteHelper(Sandbox):
    def run_helper(self, *args, **extra):
        env = dict(self.env, SUPERCI_APP_TOKEN_CMD=TOKEN_SCOPED, **extra)
        return subprocess.run(["bash", os.path.join(HERE, "canary.sh"), *args], env=env, capture_output=True, text=True, timeout=60)

    def test_set_and_unset_write_SUPERCI_JOBS_on_the_variables_token(self):
        p = self.run_helper("set", '["test-go","verify-tools"]')
        self.assertEqual(p.returncode, 0, (p.stdout, p.stderr))
        p2 = self.run_helper("unset")
        self.assertEqual(p2.returncode, 0, (p2.stdout, p2.stderr))
        calls = self.gh_calls()
        self.assertEqual([args for tok, args in calls if args.startswith("variable ")],
                         ['variable set SUPERCI_JOBS -R Authentic-Intelligence/megpt-mono --body ["test-go","verify-tools"]', "variable delete SUPERCI_JOBS -R Authentic-Intelligence/megpt-mono"])
        self.assertEqual({tok for tok, _ in calls}, {VARS_TOKEN}, "the reads and both writes carry the variables token")
        self.assertNotIn(VARS_TOKEN, p.stdout + p.stderr + p2.stdout + p2.stderr)

    def test_set_and_unset_keep_the_variables_token_past_a_host_wrapper(self):
        self.run_helper("set", '["test-go"]')
        self.run_helper("unset")
        self.assert_app_calls_keep_the_token(VARS_TOKEN, "variable set")
        self.assert_app_calls_keep_the_token(VARS_TOKEN, "variable delete")
        self.assert_app_calls_keep_the_token(VARS_TOKEN, "actions/variables/SUPERCI_JOBS")
        self.assertEqual({keep for _, keep, _ in self.gh_envs()}, {"1"})

    def test_a_personal_set_does_not_ask_the_wrapper_to_keep_anything(self):
        self.run_helper("set", '["test-go"]', REFUSE_SCOPES="variables", SUPERCI_PERSONAL_TOKEN="1")
        writes = [c for c in self.gh_envs() if c[2].startswith("variable ")]
        self.assertTrue(writes)
        self.assertEqual({(tok, keep) for tok, keep, _ in writes}, {("UNSET", "UNSET")}, writes)

    def test_unset_of_an_unset_variable_is_not_a_failure(self):
        gh = os.path.join(self.bin, "gh")
        with open(gh) as fh:
            body = fh.read()
        with open(gh, "w") as fh:
            fh.write(body.replace('case "$*" in\n', 'case "$*" in\n  *actions/variables/SUPERCI_JOBS*) echo "gh: Not Found (HTTP 404)" >&2; exit 1 ;;\n', 1))
        p = self.run_helper("unset")
        self.assertEqual((p.returncode, p.stdout.strip()), (0, "SUPERCI_JOBS already unset"), p)
        self.assertEqual([a for _, a in self.gh_calls() if a.startswith("variable ")], [], "nothing to delete")

    def test_a_value_that_is_not_a_list_of_job_names_is_refused_before_any_call(self):
        for bad in ('{"a":1}', "test-go", "[1]", ""):
            p = self.run_helper("set", bad)
            self.assertEqual(p.returncode, 2, (bad, p.stdout, p.stderr))
        self.assertEqual(self.gh_calls(), [])

    def test_a_refused_variables_token_refuses_loudly_unless_the_operator_allows_the_personal_one(self):
        p = self.run_helper("set", '["test-go"]', REFUSE_SCOPES="variables")
        self.assertEqual(p.returncode, 1, (p.stdout, p.stderr))
        self.assertIn("ROLLBACK FAILED", p.stdout)
        self.assertIn("repository permission", p.stdout, "the listener's reason is shown")
        self.assertEqual(self.gh_calls(), [], "no call was made on any token")
        p = self.run_helper("set", '["test-go"]', REFUSE_SCOPES="variables", SUPERCI_PERSONAL_TOKEN="1")
        self.assertEqual(p.returncode, 0, (p.stdout, p.stderr))
        self.assertEqual([tok for tok, args in self.gh_calls() if args.startswith("variable set")], ["UNSET"])
        self.assertIn("personal-token exception SUPERCI_JOBS write", p.stderr)

    def test_the_helper_needs_neither_aws_nor_the_actions_token(self):
        asks = os.path.join(self.tmp, "asks")
        p = self.run_helper("unset", SCOPE_ASKS=asks, PATH=self.bin.replace("bin", "nowhere") + os.pathsep + "/usr/bin" + os.pathsep + "/bin")
        with open(asks) as fh:
            self.assertEqual(set(fh.read().split()), {"variables"}, "only the variables token was asked for")


class NoTokenInTheNewPaths(Sandbox):
    def test_no_token_reaches_the_output_of_a_rollback_a_helper_or_a_refused_gate(self):
        self.env.update(SUPERCI_APP_TOKEN_CMD=TOKEN_SCOPED)
        outs = []
        for args, extra in ((("set", '["a"]'), {}), (("unset",), {"REFUSE_SCOPES": "variables"}), (("unset",), {"SUPERCI_PERSONAL_TOKEN": "1", "REFUSE_SCOPES": "variables"})):
            p = subprocess.run(["bash", os.path.join(HERE, "canary.sh"), *args], env=dict(self.env, **extra), capture_output=True, text=True, timeout=60)
            outs.append(p.stdout + p.stderr)
        leaky = stub("leaky", """printf '{"token":"ghs_leaked_in_a_refusal","expires":"2999-01-01T00:00:00Z"}\\n200\\n'\necho 'ssh: reset' >&2\nexit 255\n""")
        p = subprocess.run(["bash", os.path.join(HERE, "canary.sh"), "unset"], env=dict(self.env, SUPERCI_APP_TOKEN_CMD=leaky), capture_output=True, text=True, timeout=60)
        outs.append(p.stdout + p.stderr)
        self.assertIn("ROLLBACK FAILED", p.stdout)
        for out in outs:
            for token in (APP_TOKEN, VARS_TOKEN, LIMITS_TOKEN, "ghs_leaked_in_a_refusal"):
                self.assertNotIn(token, out)


class PersonalTokenExceptions(unittest.TestCase):
    def test_the_exceptions_the_code_makes_are_exactly_the_readmes(self):
        def read(*parts):
            try:
                with open(os.path.join(PULL, *parts)) as fh:
                    return fh.read()
            except FileNotFoundError:
                return ""

        readme = read("README.md")
        table = readme.partition("Personal-token exceptions")[2]
        documented = set(re.findall(r"^\| `([^`]+)` \|", table, re.M))
        keys = set(re.findall(r'^    "([^"]+)": \(', read("tools", "timeline", "apptoken.py").partition("EXCEPTIONS = {")[2].partition("\n}")[0], re.M))
        used = set(re.findall(r'exception="([^"]+)"', read("tools", "gate", "gate.py") + read("tools", "timeline", "timeline.py")))
        used |= set(re.findall(r'\bpersonal_fallback "([^"]+)"', read("tools", "gate", "canary.sh")))
        self.assertEqual(documented, {"SUPERCI_JOBS write"}, "the one personal-token exception is the rollback's write of SUPERCI_JOBS")
        self.assertNotRegex(read("tools", "gate", "canary.sh"), r"(?m)^\s*(?!#)[^#\n]*\bpersonal\s+[A-Z_]+\s+(api|variable)\b", "an old personal-token call site is left in canary.sh")
        self.assertEqual(keys, documented, "EXCEPTIONS in apptoken.py and the README differ")
        self.assertEqual(used, documented, "a call site uses an exception the README does not list, or the README lists one nothing uses")


# ---------------------------------------------------------------------------------------------------------------------
# A refusal in the middle of a command stops it; it is never a skipped run.
# ---------------------------------------------------------------------------------------------------------------------

# A listener that answers for its first OK_MINTS asks (each token lives 2 minutes, so every later use asks again) and then fails
# the way an ssh or Instance Connect flake does: the way a refresh in the middle of a long gate can.
FLAKY = """n=$(($(cat "$MINTS" 2>/dev/null || echo 0)+1)); echo $n > "$MINTS"
exp=$(date -u -d '+120 seconds' +%Y-%m-%dT%H:%M:%SZ)
if [ $n -le ${OK_MINTS:-0} ]; then printf '{"token":"@TOKEN@","expires":"%s"}\\n200\\n' "$exp"; exit 0; fi
if [ -n "${LEAK-}" ]; then printf '{"token":"@TOKEN@","expires":"%s"}\\n200\\n' "$exp"; fi
echo 'ssh: Connection timed out during banner exchange' >&2
exit 255
""".replace("@TOKEN@", APP_TOKEN)


class RefusalMidRun(Sandbox):
    def setUp(self):
        super().setUp()
        self.mints = os.path.join(self.tmp, "mints")
        self.flaky = stub("flaky", FLAKY)
        self.env.update(SUPERCI_APP_TOKEN_CMD=self.flaky, MINTS=self.mints, RUN_CREATED=T.iso(time.time() - 3600))

    def in_env(self, **more):
        saved, saved_auth = dict(os.environ), T.AUTH
        os.environ.clear()
        os.environ.update(self.env, **more)
        T.AUTH = T.apptoken.Auth()
        def restore():
            os.environ.clear()
            os.environ.update(saved)
            T.AUTH = saved_auth
        self.addCleanup(restore)

    def test_a_refresh_that_fails_in_the_middle_of_jobs_of_stops_the_command_and_skips_no_run(self):
        self.in_env(OK_MINTS="1")
        gh = T.GitHub("o/r", T.Cache(os.path.join(self.tmp, "cache")))
        try:
            got, raised = G.jobs_of(gh, [{"id": i} for i in range(1, 7)]), None
        except BaseException as e:  # noqa: BLE001
            got, raised = None, e
        self.assertIsNotNone(raised, f"the refusal was swallowed: {gh.skipped} of 6 runs recorded as skipped, jobs {got}")
        self.assertEqual(gh.skipped, [], "no run is skipped for want of a token")
        self.assertIsInstance(raised, T.apptoken.AppTokenRefused)
        self.assertNotIsInstance(raised, SystemExit, "a SystemExit is what every skip-this-run catch swallows")

    def test_a_refresh_that_fails_in_the_middle_of_a_timeline_compare_is_not_a_skipped_run(self):
        self.in_env(OK_MINTS="0")
        gh = T.GitHub("o/r", T.Cache(os.path.join(self.tmp, "cache")))
        try:
            got, raised = T.safe(lambda: gh.run_jobs(7), 7), None
        except BaseException as e:  # noqa: BLE001
            got, raised = None, e
        self.assertIsNotNone(raised, f"timeline.safe() turned the refusal into an empty run listing: {got}")

    def test_the_gate_exits_2_with_the_reason_when_the_refresh_fails_after_the_listing_was_read(self):
        # mints: the gate's start, the run listing's two reads; the fourth is the first run's jobs.
        self.in_env(OK_MINTS="3")
        err, out, code = io.StringIO(), io.StringIO(), None
        limits = os.path.join(self.tmp, "limits.tsv")
        with open(limits, "w") as fh:
            fh.write("lint-test-backend.yml\tverify-tools\t200\tx\n")
        try:
            with contextlib.redirect_stderr(err), contextlib.redirect_stdout(out):
                G.main(["--cache", os.path.join(self.tmp, "cache"), "job", "verify-tools", "--branch-glob", "main", "--limits", limits, "--superci-source", "github"])
        except SystemExit as e:
            code = e.code
        self.assertEqual(code, 2, (out.getvalue()[-400:], err.getvalue()[-400:]))
        self.assertIn("refusing: no GitHub App token", err.getvalue())
        self.assertNotIn("skipped", out.getvalue(), "no criteria were judged on a sample that lost runs")

    def canary_tree(self, gate_body):
        """canary.sh beside a gate.py that is a stub, so only the canary's own handling of an exit code is run."""
        root = os.path.join(self.tmp, "tree")
        os.makedirs(os.path.join(root, "gate"))
        os.makedirs(os.path.join(root, "timeline"))
        shutil.copy(os.path.join(HERE, "canary.sh"), os.path.join(root, "gate", "canary.sh"))
        shutil.copy(os.path.join(HERE, "..", "timeline", "apptoken.py"), os.path.join(root, "timeline", "apptoken.py"))
        with open(os.path.join(root, "gate", "gate.py"), "w") as fh:
            fh.write(gate_body)
        return os.path.join(root, "gate", "canary.sh")

    def test_a_gate_that_refuses_does_not_send_a_job_back(self):
        script = self.canary_tree("import sys\nprint('refusing: no GitHub App token (stub)', file=sys.stderr)\nsys.exit(2)\n")
        until = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(time.time() + 2))
        env = dict(self.env, EVERY="600", LOG=self.tmp, OK_MINTS="100", APP_REMAINING="4000")
        p = subprocess.run(["bash", script, "2026-11-27T00:00:00Z", until, "verify-tools"], env=env, capture_output=True, text=True, timeout=120)
        self.assertIn("(exit 2,", p.stdout, (p.stdout, p.stderr))
        self.assertNotIn("SUPERCI_JOBS was", p.stdout, "remove() ran")
        self.assertEqual([a for _, a in self.gh_calls() if a.startswith("variable")], [], "the variable was edited")


class NoTokenInAMessage(Sandbox):
    def setUp(self):
        super().setUp()
        self.mints = os.path.join(self.tmp, "mints")
        self.env.update(SUPERCI_APP_TOKEN_CMD=stub("flaky", FLAKY), MINTS=self.mints, LEAK="1")
        self.tmpdir = os.path.join(self.tmp, "tmpdir")
        os.makedirs(self.tmpdir)

    def canary(self, **extra):
        until = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(time.time() + 2))
        env = dict(self.env, EVERY="600", LOG=self.tmp, TMPDIR=self.tmpdir, **extra)
        p = subprocess.run(["bash", os.path.join(HERE, "canary.sh"), "2026-11-27T00:00:00Z", until, "verify-tools"], env=env, capture_output=True, text=True, timeout=120)
        return p

    def test_ssh_failing_after_curl_printed_a_token_puts_no_token_in_the_refusal(self):
        self.env["OK_MINTS"] = "0"
        p = subprocess.run([sys.executable, "-B", os.path.join(HERE, "..", "timeline", "apptoken.py")], env=self.env, capture_output=True, text=True)
        self.assertEqual(p.returncode, 2, p)
        self.assertNotIn("ghs_", p.stdout + p.stderr)
        self.assertIn("exit 255", p.stderr, "the refusal still says what failed")

    def test_the_canary_log_holds_no_token_when_the_refresh_of_a_tick_fails_that_way(self):
        p = self.canary(OK_MINTS="1")  # the start's mint answers; the tick's prints a token and then fails
        self.assertIn("no GitHub App token, no check", p.stdout, (p.stdout, p.stderr))
        self.assertNotIn("ghs_", p.stdout + p.stderr)
        self.assertNotIn("ghs_", "".join(open(os.path.join(self.tmp, f)).read() for f in os.listdir(self.tmp) if f.startswith("canary-")))

    def test_the_canary_log_holds_no_token_when_the_start_is_refused_that_way(self):
        p = self.canary(OK_MINTS="0")
        self.assertEqual(p.returncode, 2, (p.stdout, p.stderr))
        self.assertNotIn("ghs_", p.stdout + p.stderr)

    def test_the_temp_file_is_removed_after_a_refusal(self):
        self.canary(OK_MINTS="0")
        self.assertEqual(os.listdir(self.tmpdir), [])

    def test_the_temp_file_is_removed_when_the_watch_is_killed(self):
        until = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(time.time() + 60))
        env = dict(self.env, EVERY="600", LOG=self.tmp, TMPDIR=self.tmpdir, OK_MINTS="100")
        p = subprocess.Popen(["bash", os.path.join(HERE, "canary.sh"), "2026-11-27T00:00:00Z", until, "verify-tools"], env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        deadline = time.time() + 30
        while not self.gh_calls() and time.time() < deadline:  # the watch is in its loop once it has read SUPERCI_JOBS
            time.sleep(0.1)  # a bounded wait on a condition (the watch has started), not a fixed delay
        self.assertTrue(os.listdir(self.tmpdir), "the watch made no temp file to remove")
        p.terminate()
        p.communicate(timeout=30)
        self.assertEqual(os.listdir(self.tmpdir), [], "a killed watch left its temp file")


# ---------------------------------------------------------------------------------------------------------------------
# Failing tests: a superci failure whose tests are red on Blacksmith's main too is not superci's regression.
# ---------------------------------------------------------------------------------------------------------------------

STAMP = "2026-11-27T10:00:00.0000000Z "


def unit_log(*fails, file="src/a.test.ts", exit_line=True, exit_code=1, reported=None, errors=0, stopped=None, unhandled=False, summary=True):
    """What a unit-tests job's log holds for one test file: scripts/verify/test-files.ts's header, bun's lines and summary (` N fail`, ` N error`),
    its verdict on the file (`exited N`, or `was stopped at <limit>`)."""
    lines = [f"== bun test --isolate {file}", "bun test v1.3.0", "(pass) suite > fine [0.10ms]"]
    if unhandled:
        lines += ["# Unhandled error between tests", "-------------------------------", "error: boom", "-------------------------------"]
    lines += [f"(fail) {f} [12.50ms]" for f in fails]
    n = len(fails) if reported is None else reported
    if (n or errors) and summary:
        lines += [" 1 pass"] + ([f" {n} fail"] if n else []) + ([f" {errors} error{'s' if errors != 1 else ''}"] if errors else []) + [" 3 expect() calls", f"Ran {n + 1} tests across 1 file. [50.00ms]"]
    if stopped:
        lines.append(f"test-files: {file} was stopped at {stopped}")
    elif exit_line:
        lines.append(f"test-files: {file} exited {exit_code}")
    return "\n".join(STAMP + l for l in lines) + "\n"


def integration_log(target, *fails, exit_code=1, watchdog=False, crash=None, reported=None, summary=True):
    """What an integration shard's log holds for one failed target (testRunner.ts, INTEGRATION_QUIET_PASSING=1; targetReports.ts's list)."""
    n = len(fails) if reported is None else reported
    lines = ["[integration] Target 1/2 finished in 3.0s (exit 0): src/integration/ok.integration.ts",
             f"[integration] ── target 2/2 (worker 1, 9.0s, exit {exit_code}): {target}", "bun test v1.3.0", *(f"(fail) {f} [5001.00ms]" for f in fails),
             *([" 1 pass", f" {n} fail", f"Ran {n + 1} tests across 1 file. [50.00ms]"] if n and summary else []), "[integration] ── end target 2/2"]
    if watchdog:
        lines.insert(3, f"[integration] Target hung past its watchdog and was killed: {target}")
    lines += [f"[integration] Target failed with exit code {exit_code}: {target}", "[integration] 1 integration target(s) failed:", f" - {target} (exit code {exit_code})"]
    if crash:
        lines.append(f"[integration] Integration runner crashed: {crash}")
    return "\n".join(STAMP + l for l in lines) + "\n"


class LogGitHub:
    """GitHub for cmd_job with logs: the jobs of each run, the log of each job, and every log read."""
    jobs, logs, reads, extra_calls = {}, {}, [], []

    def __init__(self, repo, cache):
        self.repo, self.calls, self.cache = repo, 0, cache

    def run_jobs(self, run_id, attempts="latest", fresh=False):
        self.calls += 1
        return list(LogGitHub.jobs[run_id])

    def api(self, path, keep=True, raw=False):
        self.calls += 1
        if m := re.fullmatch(r"actions/jobs/(\d+)/logs", path):
            LogGitHub.reads.append(int(m.group(1)))
            if int(m.group(1)) not in LogGitHub.logs:
                raise SystemExit(f"gh api {path}: HTTP 404: Not Found (logs expired)")
            return LogGitHub.logs[int(m.group(1))]
        LogGitHub.extra_calls.append(path)
        return {"head_sha": "abc"}


NOW = T.ts("2026-12-01T12:00:00Z")


def tj(jid, run, conclusion, fleet, name, log=None, steps=None, when=None):
    """A REST job of a test run; `when` is how long before NOW it was created. Its log (None: GitHub has none) is registered."""
    at = NOW - (when if when is not None else (1800 if fleet == "superci" else 10 * 3600))
    j = gh_job(jid, run, 1, conclusion, at, at + 2, at + 90, fleet=fleet, name=name, sha=f"sha{jid}", branch="ci/load-1" if fleet == "superci" else "main")
    j["steps"] = steps or []
    if log is not None:
        LogGitHub.logs[jid] = log
    return j


class FailingHarness:
    """cmd_job over a canned GitHub whose jobs have logs, for the test classes below."""
    job = "unit-tests"
    workflow = "lint-test-backend.yml"

    def setUp(self):
        LogGitHub.logs, LogGitHub.reads = {}, []

    def run_gate(self, runs, extra=(), records=None, now=NOW, cache=None):
        """runs: [(run id, branch, run conclusion, [REST jobs][, event])]. Returns (code, output)."""
        LogGitHub.jobs, LogGitHub.extra_calls = {}, []
        listing = []
        for rid, branch, conclusion, jobs, *event in runs:
            for j in jobs:
                j["run_id"] = rid
            LogGitHub.jobs[rid] = jobs
            listing.append({"id": rid, "created_at": jobs[0]["created_at"], "head_branch": branch, "conclusion": conclusion, "event": event[0] if event else "push"})
        saved = (G.T.GitHub, G.runs_of, G.T.gather, G.T.build, G.T.listener_events, G.time.time)
        G.T.GitHub, G.runs_of, G.time.time = LogGitHub, lambda gh, wf, lo, hi: listing, lambda: now
        G.T.gather = lambda *a, **k: {}
        G.T.build = lambda j, ev: type("TL", (), {"wall": 1.0, "unexplained": 0.5})()
        G.T.listener_events = lambda profile, lo, hi, where=None: records or []
        try:
            with tempfile.TemporaryDirectory() as d:
                limits = os.path.join(d, "limits.tsv")
                with open(limits, "w") as fh:
                    fh.write(f"{self.workflow}\t{self.job}\t200\tx\n")
                since = [] if "--since" in extra else ["--since", T.iso(now - 7200)]
                a = G.build_parser().parse_args(["--cache", cache or d, "job", self.job, "--workflow", self.workflow, "--branch-glob", "ci/*", "--limits", limits,
                                                 "--superci-source", "events" if records is not None else "github", *since, *extra])
                buf = io.StringIO()
                with contextlib.redirect_stdout(buf):
                    code = a.fn(a)
                return code, buf.getvalue()
        finally:
            G.T.GitHub, G.runs_of, G.T.gather, G.T.build, G.T.listener_events, G.time.time = saved

    def baseline(self, red=(), n=8):
        """Blacksmith's side: n green main runs and, for each log in `red`, a failed main run. [(run, branch, conclusion, jobs)]."""
        out = [(5000 + i, "main", "success", [tj(5000 + i, 0, "success", "blacksmith", f"{self.job} (1)")]) for i in range(n)]
        for i, log in enumerate(red):
            out.append((6000 + i, "main", "failure", [tj(6000 + i, 0, "failure", "blacksmith", f"{self.job} (2)", log)]))
        return out

    def superci(self, n=5, failed=()):
        """n green superci runs and, for each (log, steps) of `failed`, a failed one: runs 9000+."""
        out = [(8000 + i, "ci/load-1", "success", [tj(8000 + i, 0, "success", "superci", f"{self.job} (1)")]) for i in range(n)]
        for i, f in enumerate(failed):
            log, steps = f if isinstance(f, tuple) else (f, None)
            out.append((9000 + i, "ci/load-1", "failure", [tj(9000 + i, 0, "failure", "superci", f"{self.job} (1)", log, steps)]))
        return out


    def beyond(self, red_log, sup_log, why, steps=None, conclusion="failure", base_red=None):
        """A superci failure whose log names a test red on main too, but that failed in a way those tests do not explain: it counts, and says why."""
        runs = self.baseline(red=[base_red or red_log])
        runs += [(8000 + i, "ci/load-1", "success", [tj(8000 + i, 0, "success", "superci", f"{self.job} (1)")]) for i in range(5)]
        runs += [(9000, "ci/load-1", conclusion, [tj(9000, 0, conclusion, "superci", f"{self.job} (1)", sup_log, steps)])]
        code, out = self.run_gate(runs)
        self.assertRegex(out, r"failures \(superci\)\s+1 of 6")
        self.assertIn(f"counted, failed beyond its named tests (fail closed): run 9000: {why}", out)
        self.assertNotIn("excluded (red on Blacksmith main too)", out)


class FailingTests(FailingHarness, unittest.TestCase):
    """The failures row of a job with a test-name source (unit-tests): what it counts, excludes and prints."""

    def test_a_superci_failure_whose_test_is_red_on_blacksmith_main_in_the_window_is_excluded_and_printed(self):
        code, out = self.run_gate(self.baseline(red=[unit_log("suite > stampDoubles")]) + self.superci(failed=[unit_log("suite > stampDoubles")]))
        self.assertRegex(out, r"failures \(superci\)\s+0 of 6")
        self.assertIn("excluded (red on Blacksmith main too): src/a.test.ts :: suite > stampDoubles (1 Blacksmith failure: flaky?) in 9000", out)
        self.assertIn("red on Blacksmith main in runs 6000", out)
        self.assertRegex(out, r"1 more failed only on tests red on Blacksmith main too")

    def test_a_superci_failure_whose_test_is_green_on_blacksmith_main_counts(self):
        code, out = self.run_gate(self.baseline(red=[unit_log("suite > stampDoubles")]) + self.superci(failed=[unit_log("suite > newlyBroken")]))
        self.assertRegex(out, r"failures \(superci\)\s+1 of 6")
        self.assertIn("counted, green on Blacksmith main: src/a.test.ts :: suite > newlyBroken in 9000", out)
        self.assertNotIn("excluded (red on Blacksmith main too)", out)

    def test_a_failure_with_one_red_and_one_green_test_counts(self):
        code, out = self.run_gate(self.baseline(red=[unit_log("suite > stampDoubles")]) + self.superci(failed=[unit_log("suite > stampDoubles", "suite > newlyBroken")]))
        self.assertRegex(out, r"failures \(superci\)\s+1 of 6")
        self.assertIn("counted, green on Blacksmith main: src/a.test.ts :: suite > newlyBroken in 9000", out)

    def test_a_failure_whose_tests_cannot_be_read_counts_and_says_why(self):
        code, out = self.run_gate(self.baseline(red=[unit_log("suite > stampDoubles")]) + self.superci(failed=[None]))
        self.assertRegex(out, r"failures \(superci\)\s+1 of 6")
        self.assertIn("counted, failing tests unreadable (fail closed): run 9000: log not read", out)
        self.assertIn("HTTP 404", out)

    def test_a_log_that_names_no_test_counts_and_says_so(self):
        code, out = self.run_gate(self.baseline(red=[unit_log("suite > stampDoubles")]) + self.superci(failed=["2026-11-27T10:00:00.0000000Z the runner lost its machine\n"]))
        self.assertRegex(out, r"failures \(superci\)\s+1 of 6")
        self.assertIn("names no failing test", out)

    def test_a_job_that_failed_before_its_tests_counts_with_no_log_read(self):
        steps = [{"name": "Run tests", "conclusion": "success"}, {"name": "Record this shard's coverage", "conclusion": "failure"}]
        code, out = self.run_gate(self.baseline(red=[unit_log("suite > stampDoubles")]) + self.superci(failed=[(unit_log("suite > stampDoubles"), steps)]))
        self.assertRegex(out, r"failures \(superci\)\s+1 of 6")
        self.assertIn("counted, no test failed: run 9000: failed outside the 'Run tests' step, in 'Record this shard's coverage'", out)
        self.assertNotIn(9000, LogGitHub.reads)

    def test_with_no_blacksmith_main_failure_in_the_window_every_failure_counts(self):
        code, out = self.run_gate(self.baseline() + self.superci(failed=[unit_log("suite > stampDoubles")]))
        self.assertRegex(out, r"failures \(superci\)\s+1 of 6")
        self.assertIn("0 main runs ended red", out)

    def test_a_blacksmith_main_failure_before_the_window_or_on_another_branch_excludes_nothing(self):
        base = self.baseline()
        old = tj(6100, 0, "failure", "blacksmith", f"{self.job} (2)", unit_log("suite > stampDoubles"), when=40 * 3600)
        other = tj(6101, 0, "failure", "blacksmith", f"{self.job} (2)", unit_log("suite > stampDoubles"), when=3600)
        base += [(6100, "main", "failure", [old]), (6101, "ci/feature", "failure", [other])]
        code, out = self.run_gate(base + self.superci(failed=[unit_log("suite > stampDoubles")]))
        self.assertRegex(out, r"failures \(superci\)\s+1 of 6")

    def test_a_second_check_reads_no_log_again(self):
        with tempfile.TemporaryDirectory() as cache:
            runs = lambda: self.baseline(red=[unit_log("suite > stampDoubles")]) + self.superci(failed=[unit_log("suite > stampDoubles")])
            self.run_gate(runs(), cache=cache)
            first = sorted(LogGitHub.reads)
            LogGitHub.reads.clear()
            code, out = self.run_gate(runs(), cache=cache)
        self.assertEqual(first, [6000, 9000])
        self.assertEqual(LogGitHub.reads, [], "a finished job's names are kept")
        self.assertIn("excluded (red on Blacksmith main too)", out)

    def test_a_check_reads_no_more_logs_than_its_budget_and_what_is_not_read_counts(self):
        code, out = self.run_gate(self.baseline(red=[unit_log("suite > stampDoubles")]) + self.superci(failed=[unit_log("suite > stampDoubles")]), extra=["--max-test-reads", "0"])
        self.assertEqual(LogGitHub.reads, [], "no log was read past the budget of 0")
        self.assertRegex(out, r"failures \(superci\)\s+1 of 6")
        self.assertIn("--max-test-reads", out)

    def test_a_record_is_matched_to_its_rest_job_and_its_log_read(self):
        rec = record("gF", 1, 30, "failure", run=9000, name=f"{self.job} (1)", workflow=self.workflow, branch="ci/load-1")
        rec["runner"] = "superci-i-1"
        recs = [record(f"g{i}", 1, 81, run=8000 + i, name=f"{self.job} (1)", workflow=self.workflow, branch="ci/load-1") for i in range(5)] + [rec]
        rest = tj(9000, 9000, "failure", "superci", f"{self.job} (1)", unit_log("suite > stampDoubles"))
        shift = NOW - 1800 - 1_000_000.0
        rec_t = lambda e: dict(e, t=NOW, **{k: T.iso(T.ts(e[k]) + shift) for k in ("queue_time", "start_time", "finish_time") if e.get(k)})
        done = T.ts(rec_t(rec)["finish_time"])
        rest.update(created_at=T.iso(done - 33), started_at=T.iso(done - 30), completed_at=T.iso(done))
        runs = self.baseline(red=[unit_log("suite > stampDoubles")]) + [(9000, "ci/load-1", "failure", [rest])]
        LogGitHub.logs[9000] = unit_log("suite > stampDoubles")
        code, out = self.run_gate(runs, records=[rec_t(e) for e in recs])
        self.assertRegex(out, r"failures \(superci\)\s+0 of 6")
        self.assertIn("excluded (red on Blacksmith main too): src/a.test.ts :: suite > stampDoubles (1 Blacksmith failure: flaky?) in 9000", out)


    def test_a_red_cluster_that_excused_superci_failures_leaves_blacksmith_s_rate_so_the_allowance_does_not_grow(self):
        red = unit_log("suite > stampDoubles")
        failed = [red, red, unit_log("suite > newlyBroken"), unit_log("suite > alsoBroken")]
        code, out = self.run_gate(self.baseline(red=[red, red, red]) + self.superci(failed=failed))
        self.assertRegex(out, r"FAIL\s+failures \(superci\)\s+2 of 9\s+<= B rate 0\.0% x 9 \+ 1 = 1\.0")
        self.assertIn("Blacksmith failure rate: 27.3% (3 of 11) before, 0.0% (0 of 11) after excluding 3 failed main jobs whose every failing test excused a superci failure", out)

    def test_scattered_blacksmith_flakes_on_other_tests_still_count_toward_the_allowance(self):
        """Main 6 of 20 failing on six different flaky tests, superci 3 of 10 on three others: 30% x 10 + 1 = 4, so 3 pass."""
        main = [unit_log(f"suite > mainFlake{i}") for i in range(6)]
        sup = [unit_log(f"suite > superciFlake{i}") for i in range(3)]
        code, out = self.run_gate(self.baseline(red=main, n=14) + self.superci(n=7, failed=sup))
        self.assertRegex(out, r"PASS\s+failures \(superci\)\s+3 of 10\s+<= B rate 30\.0% x 10 \+ 1 = 4\.0")
        self.assertIn("30.0% (6 of 20) before, 30.0% (6 of 20) after excluding 0 failed main jobs", out)

    def test_a_blacksmith_main_failure_that_cannot_be_read_still_counts_toward_the_rate(self):
        red = unit_log("suite > stampDoubles")
        code, out = self.run_gate(self.baseline(red=[red, None]) + self.superci(failed=[red]))
        self.assertIn("20.0% (2 of 10) before, 10.0% (1 of 10) after excluding 1 failed main job", out)
        self.assertIn("B rate 10.0% x 6 + 1 = 1.6", out)

    def test_a_blacksmith_failure_off_main_or_with_no_test_named_still_counts_toward_the_rate(self):
        red = unit_log("suite > stampDoubles")
        feature = tj(6200, 0, "failure", "blacksmith", f"{self.job} (2)", red)
        feature["head_branch"] = "ci/feature"
        setup = tj(6300, 0, "failure", "blacksmith", f"{self.job} (2)", steps=[{"name": "Run tests", "conclusion": "success"}, {"name": "Install", "conclusion": "failure"}])
        code, out = self.run_gate(self.baseline(red=[red]) + [(6200, "ci/feature", "failure", [feature]), (6300, "main", "failure", [setup])] + self.superci(failed=[red]))
        self.assertIn("27.3% (3 of 11) before, 18.2% (2 of 11) after excluding 1 failed main job", out)

    def test_a_blacksmith_failure_with_a_hard_reason_or_a_second_test_outside_the_excused_ones_counts_toward_the_rate(self):
        red = unit_log("suite > stampDoubles")
        both = unit_log("suite > stampDoubles", "suite > other")
        stopped = unit_log("suite > stampDoubles", stopped="the 180 s a test file may run")
        code, out = self.run_gate(self.baseline(red=[red, both, stopped]) + self.superci(failed=[red]))
        self.assertIn("27.3% (3 of 11) before, 18.2% (2 of 11) after excluding 1 failed main job", out)

    def test_an_exclusion_resting_on_one_blacksmith_failure_is_tagged_and_one_resting_on_two_is_not(self):
        once, twice = unit_log("suite > once"), unit_log("suite > twice")
        base = self.baseline(red=[once + twice, twice])
        code, out = self.run_gate(base + self.superci(failed=[unit_log("suite > once", "suite > twice")]))
        self.assertIn("excluded (red on Blacksmith main too): src/a.test.ts :: suite > once (1 Blacksmith failure: flaky?), src/a.test.ts :: suite > twice in 9000", out)

    def test_an_exclusion_resting_on_two_blacksmith_failures_carries_no_tag(self):
        red = unit_log("suite > stampDoubles")
        code, out = self.run_gate(self.baseline(red=[red, red]) + self.superci(failed=[red]))
        self.assertIn("excluded (red on Blacksmith main too): src/a.test.ts :: suite > stampDoubles in 9000", out)
        self.assertNotIn("flaky?", out)

    def test_a_file_stopped_at_its_time_limit_counts_though_a_red_test_is_in_its_log(self):
        red = unit_log("suite > stampDoubles")
        self.beyond(red, unit_log("suite > stampDoubles", stopped="the 180 s a test file may run"), "src/a.test.ts was stopped at the 180 s a test file may run", base_red=red)

    def test_a_file_terminated_by_a_signal_counts_though_a_red_test_is_in_its_log(self):
        red = unit_log("suite > stampDoubles")
        self.beyond(red, red + STAMP + "test-files: src/a.test.ts terminated by SIGSEGV; stopping\n", "src/a.test.ts was terminated by SIGSEGV")

    def test_an_error_between_tests_counts_though_a_red_test_is_in_its_log(self):
        red = unit_log("suite > stampDoubles")
        self.beyond(red, unit_log("suite > stampDoubles", unhandled=True, errors=1), "src/a.test.ts: an error between tests")

    def test_a_summary_counting_more_failures_than_the_log_names_counts(self):
        red = unit_log("suite > stampDoubles")
        self.beyond(red, unit_log("suite > stampDoubles", reported=2), "src/a.test.ts: bun's summary counts 2 failures and errors, the log names 1 tests")

    def test_a_job_cut_by_its_timeout_counts_whether_the_conclusion_the_step_or_the_log_says_so(self):
        red = unit_log("suite > stampDoubles")
        self.beyond(red, red, "the job timed out", conclusion="timed_out")
        self.beyond(red, red, "step 'Run tests' was cancelled: the job was cut short", steps=[{"name": "Run tests", "conclusion": "cancelled"}])
        self.beyond(red, red + STAMP + "##[error]The operation was canceled.\n", "the job was cut short (##[error]The operation was canceled.)")

    def test_a_failed_step_other_than_the_tests_counts_though_a_red_test_is_in_its_log(self):
        red = unit_log("suite > stampDoubles")
        steps = [{"name": "Run tests", "conclusion": "failure"}, {"name": "Record this shard's coverage", "conclusion": "failure"}]
        self.beyond(red, red, "step 'Record this shard's coverage' failed", steps=steps)

    def test_a_failure_named_only_by_its_file_counts_and_says_why(self):
        """Both fleets log the file with no test named: the names match, but a coarse name excuses nothing."""
        coarse = unit_log()
        code, out = self.run_gate(self.baseline(red=[coarse]) + self.superci(failed=[coarse]))
        self.assertRegex(out, r"failures \(superci\)\s+1 of 6")
        self.assertIn("counted, named by file or target only (fail closed): run 9000: named only by its file or target (src/a.test.ts)", out)
        self.assertNotIn("excluded (red on Blacksmith main too)", out)

    def test_a_blacksmith_run_started_by_hand_is_not_a_main_run_of_the_window(self):
        red = unit_log("suite > stampDoubles")
        by_hand = (6100, "main", "failure", [tj(6100, 0, "failure", "blacksmith", f"{self.job} (2)", red)], "workflow_dispatch")
        code, out = self.run_gate(self.baseline() + [by_hand] + self.superci(failed=[red]))
        self.assertRegex(out, r"failures \(superci\)\s+1 of 6")
        self.assertIn("counted, green on Blacksmith main: src/a.test.ts :: suite > stampDoubles in 9000", out)
        self.assertIn("0 main runs ended red", out)

    def test_a_log_that_is_not_valid_utf8_is_unreadable_and_counts_instead_of_stopping_the_check(self):
        LogGitHub.logs[9000] = None
        orig = LogGitHub.api

        def api(self, path, keep=True, raw=False):
            if path == "actions/jobs/9000/logs":
                raise UnicodeDecodeError("utf-8", b"\xff", 0, 1, "invalid start byte")
            return orig(self, path, keep, raw)

        LogGitHub.api = api
        try:
            try:
                code, out = self.run_gate(self.baseline(red=[unit_log("suite > stampDoubles")]) + self.superci(failed=[unit_log("suite > stampDoubles")]))
            except UnicodeDecodeError as e:
                self.fail(f"the check stopped on an undecodable log: {e}")
        finally:
            LogGitHub.api = orig
        self.assertRegex(out, r"failures \(superci\)\s+1 of 6")
        self.assertIn("counted, failing tests unreadable (fail closed): run 9000: log not read (it is not valid UTF-8", out)

    def test_a_file_that_died_with_exit_1_and_no_bun_summary_counts_though_a_red_test_is_in_its_log(self):
        """bun 1.4.3: a second test calling process.exit(1) after a red one prints `(fail) red`, then nothing, and the runner logs `exited 1`."""
        red = unit_log("suite > stampDoubles")
        self.beyond(red, unit_log("suite > stampDoubles", summary=False), "src/a.test.ts exited 1 with no bun summary: the run died after its failures")

    def test_a_step_killed_with_exit_137_counts_though_a_red_test_is_in_its_log(self):
        red = unit_log("suite > stampDoubles")
        self.beyond(red, red + STAMP + "##[error]Process completed with exit code 137.\n", "the step's process exited 137")

    def test_a_runner_that_lost_communication_with_the_server_counts(self):
        red = unit_log("suite > stampDoubles")
        self.beyond(red, red + STAMP + "##[error]The runner has lost communication with the server.\n", "the job was cut short (##[error]The runner has lost communication with the server.)")

    def test_a_step_with_no_conclusion_on_a_finished_job_counts(self):
        red = unit_log("suite > stampDoubles")
        steps = [{"name": "Run tests", "conclusion": "failure"}, {"name": "Upload unit test observations", "conclusion": None}]
        self.beyond(red, red, "step 'Upload unit test observations' has no conclusion on a finished job: the runner was lost", steps=steps)

    def test_an_unreadable_blacksmith_main_failure_names_its_hard_reason_not_an_empty_one(self):
        killed = STAMP + "##[error]Process completed with exit code 137.\n"
        code, out = self.run_gate(self.baseline(red=[killed]) + self.superci(failed=[unit_log("suite > stampDoubles")]))
        self.assertIn("1 of 1 failed jobs could not be read (the step's process exited 137); no exclusion is drawn from them", out)

    def test_a_cached_read_from_an_older_parser_is_not_served_and_the_job_is_read_again(self):
        """9152daa cached `hard=[]` for a job that died after a red test; the parser has since learned to mark it, so its entry must not be served."""
        red = unit_log("suite > stampDoubles")
        with tempfile.TemporaryDirectory() as cache:
            old = T.Cache(cache)
            old.get(f"tests:v2:{G.REPO}:9000", lambda: {"tests": ["src/a.test.ts :: suite > stampDoubles"], "coarse": [], "hard": [], "why": ""})
            code, out = self.run_gate(self.baseline(red=[red]) + self.superci(failed=[unit_log("suite > stampDoubles", summary=False)]), cache=cache)
        self.assertRegex(out, r"failures \(superci\)\s+1 of 6")
        self.assertIn(9000, LogGitHub.reads, "the job's log was read again")
        self.assertIn("exited 1 with no bun summary", out)

    def test_a_test_s_own_output_saying_the_runner_s_exit_text_does_not_make_a_red_on_main_failure_count(self):
        red = unit_log("suite > stampDoubles")
        code, out = self.run_gate(self.baseline(red=[red]) + self.superci(failed=[red + STAMP + "Process completed with exit code 137\n"]))
        self.assertRegex(out, r"failures \(superci\)\s+0 of 6")
        self.assertIn("excluded (red on Blacksmith main too)", out)

    def test_a_job_with_no_test_name_source_keeps_the_job_level_rule_and_says_so(self):
        self.job = "verify-tools"
        try:
            code, out = self.run_gate(self.baseline(red=[unit_log("suite > stampDoubles")]) + self.superci(failed=[unit_log("suite > stampDoubles")]))
        finally:
            self.job = "unit-tests"
        self.assertRegex(out, r"failures \(superci\)\s+1 of 6")
        self.assertIn("no test-name source for verify-tools", out)
        self.assertEqual(LogGitHub.reads, [])


class IntegrationFailingTests(FailingHarness, unittest.TestCase):
    """The same rules for integration, whose log names failing targets as well as tests."""
    job = "integration"
    workflow = "integration-tests.yml"

    def test_a_target_red_on_main_is_excluded_and_a_new_one_counts(self):
        red = integration_log("src/integration/stampDoubles.integration.ts", "stamp > doubles")
        code, out = self.run_gate(self.baseline(red=[red]) + self.superci(failed=[red, integration_log("src/integration/other.integration.ts", "other > breaks")]))
        self.assertRegex(out, r"failures \(superci\)\s+1 of 7")
        self.assertIn("excluded (red on Blacksmith main too): src/integration/stampDoubles.integration.ts :: stamp > doubles (1 Blacksmith failure: flaky?) in 9000", out)
        self.assertIn("counted, green on Blacksmith main: src/integration/other.integration.ts :: other > breaks in 9001", out)

    def test_a_target_killed_by_its_watchdog_counts_even_when_main_has_it_too(self):
        red = integration_log("src/integration/stagedHistoryBare.integration.ts", watchdog=True)
        code, out = self.run_gate(self.baseline(red=[red]) + self.superci(failed=[red]))
        self.assertRegex(out, r"failures \(superci\)\s+1 of 6")
        self.assertIn("counted, failed beyond its named tests (fail closed): run 9000: src/integration/stagedHistoryBare.integration.ts hung past its watchdog and was killed", out)
        self.assertNotIn("excluded (red on Blacksmith main too)", out)

    def test_a_runner_crash_counts_though_a_red_target_is_in_its_log(self):
        red = integration_log("src/integration/stampDoubles.integration.ts", "stamp > doubles")
        crashed = integration_log("src/integration/stampDoubles.integration.ts", "stamp > doubles", crash="Gel went away")
        self.beyond(red, crashed, "the integration runner crashed: Gel went away")

    def test_a_target_that_exited_with_a_crash_code_counts_though_its_tests_are_red_on_main(self):
        red = integration_log("src/integration/stampDoubles.integration.ts", "stamp > doubles")
        self.beyond(red, integration_log("src/integration/stampDoubles.integration.ts", "stamp > doubles", exit_code=139),
                    "src/integration/stampDoubles.integration.ts exited 139, not bun's 1 for failed tests")

    def test_a_shard_cut_short_counts_though_a_red_target_is_in_its_log(self):
        red = integration_log("src/integration/stampDoubles.integration.ts", "stamp > doubles")
        cut = red + "\n".join(STAMP + l for l in ["[integration] 1 of 40 target(s) did not finish before SIGTERM:", " - src/integration/slow.integration.ts"]) + "\n"
        self.beyond(red, cut, "src/integration/slow.integration.ts did not finish: the shard was cut short")

    def test_a_target_that_died_with_exit_1_and_no_bun_summary_counts_though_a_red_test_is_in_its_log(self):
        t = "src/integration/stampDoubles.integration.ts"
        red = integration_log(t, "stamp > doubles")
        self.beyond(red, integration_log(t, "stamp > doubles", summary=False), f"{t} exited 1 with no bun summary: the run died after its failures")

    def test_a_census_leg_and_a_manual_run_are_not_main_s_ordinary_runs(self):
        a, b = integration_log("src/integration/a.integration.ts", "a > one"), integration_log("src/integration/b.integration.ts", "b > one")
        interim = (6100, "main", "failure", [tj(6100, 0, "failure", "blacksmith", "integration (2, interim)", a)], "schedule")
        census = (6101, "main", "failure", [tj(6101, 0, "failure", "blacksmith", "integration (2, v3)", b)], "schedule")
        runs = self.baseline() + [interim, census] + self.superci(failed=[a, b])
        code, out = self.run_gate(runs)
        self.assertRegex(out, r"failures \(superci\)\s+1 of 7")
        self.assertIn("excluded (red on Blacksmith main too): src/integration/a.integration.ts :: a > one (1 Blacksmith failure: flaky?) in 9000", out)
        self.assertIn("counted, green on Blacksmith main: src/integration/b.integration.ts :: b > one in 9001", out)
        self.assertIn("1 failed job read", out)

    def test_the_floor_is_reported_and_the_window_starts_after_it_here(self):
        code, out = self.run_gate(self.baseline() + self.superci())
        self.assertIn("window floor (integration): 4e75d94 at 2026-11-27T20:33:52Z", out)
        self.assertIn("nothing clamped", out)

    def test_a_window_that_starts_before_the_floor_is_clamped_and_says_so(self):
        now = T.ts("2026-11-28T00:00:00Z")
        floor = "2026-11-27T20:33:52Z"
        code, out = self.run_gate(self.baseline() + self.superci(), now=now, extra=["--since", "2026-11-27T12:00:00Z"])
        self.assertIn(f"--since 2026-11-27T12:00:00Z clamped to {floor}", out)
        self.assertIn(f"Blacksmith baseline start 2026-11-27T00:00:00Z clamped to {floor}", out)
        self.assertIn(f"blacksmith: created {floor} to 2026-11-28T00:00:00Z", out)
        self.assertIn(f"created {floor} to 2026-11-28T00:00:00Z", out.split("superci:")[1])

    def test_a_blacksmith_window_that_ends_before_the_floor_is_refused(self):
        with self.assertRaises(SystemExit) as e:
            self.run_gate(self.baseline() + self.superci(), extra=["--baseline-until", "2026-11-27T18:00:00Z", "--since", "2026-11-27T18:00:00Z"])
        self.assertIn("before window floor (integration)", str(e.exception))


class LogParsers(unittest.TestCase):
    def test_unit_names_are_file_and_test_with_the_timing_removed(self):
        p = G.parse_unit_log(unit_log("a > b", "c > d [with brackets]"))
        self.assertEqual((p.tests, p.coarse, p.hard), ({"src/a.test.ts :: a > b", "src/a.test.ts :: c > d [with brackets]"}, frozenset(), ()))

    def test_a_unit_file_that_failed_with_no_fail_line_is_coarse(self):
        p = G.parse_unit_log(unit_log(file="src/x.test.ts"))
        self.assertEqual((p.tests, p.coarse, p.hard), (frozenset(), {"src/x.test.ts"}, ()))

    def test_a_unit_file_stopped_at_its_limit_or_killed_is_hard_even_with_a_fail_line(self):
        p = G.parse_unit_log(unit_log("a > b", stopped="the 180 s a test file may run"))
        self.assertEqual(p.tests, {"src/a.test.ts :: a > b"})
        self.assertEqual(p.hard, ("src/a.test.ts was stopped at the 180 s a test file may run",))
        k = G.parse_unit_log(unit_log("a > b") + STAMP + "test-files: src/a.test.ts terminated by SIGSEGV; stopping\n")
        self.assertEqual(k.hard, ("src/a.test.ts was terminated by SIGSEGV",))

    def test_a_file_that_exited_with_a_code_other_than_bun_s_is_hard(self):
        self.assertEqual(G.parse_unit_log(unit_log("a > b", exit_code=137)).hard, ("src/a.test.ts exited 137, not bun's 1 for failed tests",))

    def test_an_error_between_tests_and_a_summary_counting_more_than_the_log_names_are_hard(self):
        self.assertEqual(G.parse_unit_log(unit_log("a > b", unhandled=True, errors=1)).hard,
                         ("src/a.test.ts: bun's summary counts 2 failures and errors, the log names 1 tests", "src/a.test.ts: an error between tests"))
        self.assertIn("counts 3 failures and errors, the log names 1 tests", G.parse_unit_log(unit_log("a > b", reported=3)).hard[0])

    def test_a_run_with_failures_a_non_zero_exit_and_no_ran_line_is_hard_and_one_that_ran_to_its_end_is_not(self):
        self.assertEqual(G.parse_unit_log(unit_log("a > b", summary=False)).hard, ("src/a.test.ts exited 1 with no bun summary: the run died after its failures",))
        self.assertEqual(G.parse_unit_log(unit_log("a > b")).hard, ())
        self.assertEqual(G.parse_integration_log(integration_log("t.ts", "s > one", summary=False)).hard, ("t.ts exited 1 with no bun summary: the run died after its failures",))
        self.assertEqual(G.parse_unit_log(unit_log("a > b", exit_line=False, summary=False)).hard, (), "no exit line: nothing says the run died")

    def test_a_step_process_exit_other_than_1_is_hard_and_1_is_not(self):
        self.assertEqual(G.parse_unit_log(STAMP + "##[error]Process completed with exit code 137.\n").hard, ("the step's process exited 137",))
        self.assertEqual(G.parse_integration_log(STAMP + "##[error]Process completed with exit code 143.\n").hard, ("the step's process exited 143",))
        self.assertEqual(G.parse_unit_log(STAMP + "##[error]Process completed with exit code 1.\n").hard, ())

    def test_a_file_that_runs_twice_is_judged_per_run_so_a_later_run_cannot_finish_an_earlier_one(self):
        passing = STAMP + "== bun test --isolate src/a.test.ts\n" + STAMP + " 1 pass\n" + STAMP + "Ran 1 test across 1 file. [5.00ms]\n"
        died = "src/a.test.ts exited 1 with no bun summary: the run died after its failures"
        self.assertEqual(G.parse_unit_log(unit_log("a > b", summary=False) + passing).hard, (died,), "a died run, then a finished one")
        self.assertEqual(G.parse_unit_log(unit_log("a > b") + unit_log("a > b", summary=False)).hard, (died,), "a finished red run, then a died one")
        self.assertEqual(G.parse_unit_log(unit_log("a > b") + passing).hard, (), "a finished red run and a passing one are not hard")

    def test_a_target_that_runs_twice_is_judged_per_run(self):
        t = "t.ts"
        passing = "\n".join(STAMP + l for l in ["[integration] ── target 1/2 (worker 1, 3.0s, exit 0): t.ts", " 1 pass", "Ran 1 test across 1 file. [5.00ms]", "[integration] ── end target 1/2"]) + "\n"
        died = "t.ts exited 1 with no bun summary: the run died after its failures"
        self.assertEqual(G.parse_integration_log(integration_log(t, "s > one", summary=False) + passing).hard, (died,), "a died target, then a finished one")
        self.assertEqual(G.parse_integration_log(integration_log(t, "s > one") + integration_log(t, "s > one", summary=False)).hard, (died,), "a finished red target, then a died one")
        self.assertEqual(G.parse_integration_log(integration_log(t, "s > one") + passing).hard, ())

    def test_the_text_of_the_runner_s_exit_line_inside_a_test_s_output_is_not_the_runner_s_line(self):
        self.assertEqual(G.parse_unit_log(unit_log("a > b") + STAMP + "Process completed with exit code 137\n").hard, ())
        self.assertEqual(G.parse_unit_log(unit_log("a > b") + STAMP + "  error: Process completed with exit code 137 was expected\n").hard, ())
        self.assertEqual(G.parse_unit_log(unit_log("a > b") + STAMP + "##[error]Process completed with exit code 137.\n").hard, ("the step's process exited 137",))
        self.assertEqual(G.parse_integration_log(integration_log("t.ts", "s > one") + STAMP + "Process completed with exit code 137\n").hard, ())

    def test_a_job_cut_by_its_timeout_is_hard(self):
        text = unit_log("a > b") + STAMP + "##[error]The operation was canceled.\n"
        self.assertEqual(G.parse_unit_log(text).hard, ("the job was cut short (##[error]The operation was canceled.)",))

    def test_a_clean_unit_log_and_ansi_colours_are_handled(self):
        self.assertEqual(G.parse_unit_log(STAMP + "== bun test --isolate f.ts\n" + STAMP + "\x1b[31m(fail)\x1b[0m x\n").tests, {"f.ts :: x"})
        self.assertEqual(G.parse_unit_log(STAMP + "test-files: all 3 files passed\n"), G.Parsed())

    def test_integration_targets_cut_short_are_coarse_and_hard(self):
        text = "\n".join(STAMP + l for l in ["[integration] 2 of 40 target(s) did not finish before SIGTERM:", " - src/integration/a.integration.ts", " - src/integration/b.integration.ts", "other line"]) + "\n"
        p = G.parse_integration_log(text)
        self.assertEqual((p.tests, p.coarse), (frozenset(), {"src/integration/a.integration.ts", "src/integration/b.integration.ts"}))
        self.assertEqual(len(p.hard), 2)

    def test_an_integration_target_with_fail_lines_is_named_by_its_tests_and_one_without_is_coarse(self):
        p = G.parse_integration_log(integration_log("t.ts", "s > one", "s > two"))
        self.assertEqual((p.tests, p.coarse, p.hard), ({"t.ts :: s > one", "t.ts :: s > two"}, frozenset(), ()))
        q = G.parse_integration_log(integration_log("t.ts"))
        self.assertEqual((q.tests, q.coarse, q.hard), (frozenset(), {"t.ts"}, ()))

    def test_an_integration_runner_crash_a_watchdog_kill_and_an_odd_exit_code_are_hard(self):
        self.assertEqual(G.parse_integration_log(integration_log("t.ts", "s > one", crash="boom")).hard, ("the integration runner crashed: boom",))
        self.assertEqual(G.parse_integration_log(integration_log("t.ts", "s > one", watchdog=True)).hard, ("t.ts hung past its watchdog and was killed",))
        self.assertEqual(G.parse_integration_log(integration_log("t.ts", "s > one", exit_code=139)).hard, ("t.ts exited 139, not bun's 1 for failed tests",))


if __name__ == "__main__":
    unittest.main()
