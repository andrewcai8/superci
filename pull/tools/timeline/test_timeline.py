import json
import os
import sys
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


if __name__ == "__main__":
    unittest.main()
