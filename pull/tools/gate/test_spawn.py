import contextlib
import io
import os
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import spawn as S  # noqa: E402

STEP = "first exec of the tools a job runs, and fork-exec rate"
ECHOED = '\x1b[36;1mecho "SPAWN node_first=$(ms "$node" -e 0) python_first=$(ms "$py" -c 0)"\x1b[0m'


def log(fleet, jobs, crlf=False):
    """A `gh run view --log` excerpt of probe jobs: the echoed script, the runner line, then the measures as ms:rc."""
    out = []
    for i, m in enumerate(jobs, 1):
        head = f"spawn ({i})\t{STEP}\t2026-11-26T21:04:0{i % 10}.1234567Z "
        out.append(head + ECHOED)
        out.append(head + f"runner={fleet}-4cpu-{i} uptime=130.2 nproc=4 mem=15657")
        out.append(head + "SPAWN " + " ".join(f"{k}={v}" for k, v in m.items()))
    return ("\r\n" if crlf else "\n").join(out)


def probe(node=40, python=8, docker=12, true500=113, node50=537, cold=9000, rc=0):
    return {"node_first": f"{node}:{rc}", "python_first": f"{python}:0", "docker_exec": f"{docker}:0", "git_first": "7:0",
            "true500": f"{true500}:0", "node50": f"{node50}:0", "coldread400": f"{cold}:0"}


BLACKSMITH = log("blacksmith", [probe(node=n) for n in (42, 41, 38, 40, 39)])
WARM = log("superci", [probe(node=11, docker=10, true500=97, node50=478) for _ in range(5)])


def judge(superci, blacksmith=BLACKSMITH):
    return S.verdict(S.parse(superci), S.parse(blacksmith))


def state(rows, k):
    return next(r[4] for r in rows if r[0] == k)


class Spawn(unittest.TestCase):
    def test_a_warmed_fleet_passes_against_blacksmith(self):
        ok, rows = judge(WARM)
        self.assertTrue(ok, rows)

    def test_one_cold_machine_in_five_fails_first_exec(self):
        ok, rows = judge(log("superci", [probe(node=11)] * 4 + [probe(node=400)]))
        self.assertFalse(ok)
        self.assertEqual(state(rows, "node_first"), "FAIL")

    def test_the_bound_is_blacksmiths_median_x_1_25_plus_20_ms(self):
        self.assertTrue(judge(log("superci", [probe(node=70)] * 5))[0])
        self.assertFalse(judge(log("superci", [probe(node=71)] * 5))[0])

    def test_a_failed_or_absent_command_is_no_sample(self):
        ok, rows = judge(log("superci", [probe(node=0, rc=127)] * 5))
        self.assertFalse(ok)
        self.assertTrue(state(rows, "node_first").startswith("MISSING"))
        ok, rows = judge(log("superci", [{**probe(), "docker_exec": ""} for _ in range(5)]))
        self.assertTrue(state(rows, "docker_exec").startswith("MISSING"))

    def test_each_gated_measure_needs_five_samples_on_each_fleet(self):
        self.assertFalse(judge(log("superci", [probe(node=11)] * 4))[0])
        self.assertFalse(judge(WARM, log("blacksmith", [probe()] * 4))[0])
        partial = log("superci", [probe(node=11)] * 4 + [{"node_first": "11:0"}])
        ok, rows = judge(partial)
        self.assertEqual(state(rows, "node_first"), "PASS")
        self.assertTrue(state(rows, "docker_exec").startswith("MISSING"))

    def test_a_shown_measure_never_gates(self):
        ok, rows = judge(log("superci", [probe(node=11, cold=99999)] * 5))
        self.assertTrue(ok, rows)
        self.assertEqual(state(rows, "coldread400"), "-")

    def test_the_echoed_script_ansi_and_crlf_parse_to_the_measures_alone(self):
        jobs = S.parse(log("blacksmith", [probe(node=n) for n in (42, 41, 38, 40, 39)], crlf=True))
        self.assertEqual(sorted(j["ms"]["node_first"] for j in jobs), [38, 39, 40, 41, 42])
        self.assertEqual({j["runner"] for j in jobs}, {f"blacksmith-4cpu-{i}" for i in range(1, 6)})

    def test_a_measure_seen_again_is_a_new_attempt_not_a_merge(self):
        text = log("superci", [probe(node=11)]) + "\n" + f"spawn (1)\t{STEP}\t2026-11-26T21:09:00.0Z SPAWN true500=900:0"
        jobs = S.parse(text)
        self.assertEqual(len(jobs), 2)
        self.assertEqual(jobs[1]["ms"], {"true500": 900})

    def test_main_refuses_two_runs_of_one_fleet_and_exits_on_the_verdict(self):
        with tempfile.TemporaryDirectory() as d:
            paths = {}
            for name, text in {"warm": WARM, "cold": log("superci", [probe(node=400)] * 5), "bs": BLACKSMITH}.items():
                paths[name] = os.path.join(d, name)
                open(paths[name], "w").write(text)
            run = lambda a, b: S.main(["--logs", paths[a], paths[b]])
            with contextlib.redirect_stdout(io.StringIO()):
                self.assertEqual(run("warm", "bs"), 0)
                self.assertEqual(run("cold", "bs"), 1)
                self.assertEqual(run("bs", "warm"), 2)
                self.assertEqual(run("warm", "cold"), 2)


if __name__ == "__main__":
    unittest.main()
