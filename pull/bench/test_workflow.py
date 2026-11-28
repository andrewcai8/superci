"""workflow.py --need and --check-need: python3 -m unittest pull/bench/test_workflow.py"""
import json
import os
import subprocess
import tempfile
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))


def run(trace, *args, stdin=""):
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump({"bucket_s": 60, "jobs": trace}, f)
    try:
        p = subprocess.run(["python3", "-I", os.path.join(HERE, "workflow.py"), args[0], f.name, *args[1:]], input=stdin, capture_output=True, text=True)
        return p.returncode, p.stdout
    finally:
        os.unlink(f.name)


def leg(t, d, label="superci-8cpu"):
    return {"t": t, "label": label, "duration_s": d, "name": "u", "run": 1}


class Need(unittest.TestCase):
    def test_a_buckets_legs_arrive_together_at_its_push(self):
        # three legs of bucket 0 (t 0, 20 and 50 of 60 s) are queued by one push: all three at once
        self.assertEqual(run([leg(0, 30), leg(20, 30), leg(50, 30), leg(0, 5, "superci-4cpu")], "--need"), (0, "superci-4cpu 1\nsuperci-8cpu 3\n"))
        # 100 legs of 20 s spread over one bucket hold 100 identities, not the 51 their trace times overlap in
        self.assertEqual(run([leg(t * 0.6, 20) for t in range(100)], "--need"), (0, "superci-8cpu 100\n"))

    def test_a_leg_that_ends_as_another_arrives_is_not_counted_with_it(self):
        # bucket 0's leg holds 0-60 s (50 s and the 10 s hold), bucket 1's arrives at its push at 60 s
        self.assertEqual(run([leg(0, 50), leg(70, 30)], "--need"), (0, "superci-8cpu 1\n"))

    def test_a_sleep_is_counted_as_capped(self):
        self.assertEqual(run([leg(0, 2000), leg(130, 30)], "--need", "60"), (0, "superci-8cpu 1\n"))

    def test_check_need_refuses_a_short_or_missing_label(self):
        trace = [leg(0, 30), leg(5, 30), leg(0, 30, "superci-4cpu"), leg(0, 30, "superci-2cpu")]
        both = "superci-4cpu,superci-8cpu"
        rc, out = run(trace, "--check-need", "900", both, stdin="superci-8cpu 2\nsuperci-4cpu 1\nsuperci-park 9\n")
        self.assertEqual(rc, 0, out)
        self.assertIn("superci-2cpu: a scale set's label, no identities: not checked", out)
        rc, out = run(trace, "--check-need", "900", both, stdin="superci-8cpu 1\nsuperci-4cpu 1\n")
        self.assertEqual(rc, 1)
        self.assertIn("superci-8cpu: 1 idle, the trace's peak holds 2: SHORT", out)
        rc, out = run(trace, "--check-need", "900", both, stdin="superci-8cpu 2\n")
        self.assertEqual(rc, 1)
        self.assertIn("superci-4cpu: 0 idle", out)


if __name__ == "__main__":
    unittest.main()
