import os
import sys
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import spawn as S  # noqa: E402


def log(jobs):
    """A `gh run view --log` excerpt: one probe job per entry, as the measured runs of Nov 26 printed them."""
    out = []
    for i, (node, docker, git, true500, node50) in enumerate(jobs, 1):
        j = f"spawn ({i})\tfirst exec of the tools a job runs, and fork-exec rate\t2026-11-26T21:04:00.0000000Z "
        out.append(j + 'echo "SPAWN node_first=$(ms "$node" -e 0)"')
        out.append(j + f"SPAWN node_first={node} node_second=12 git_first={git} python_first=8 docker_first={docker} bash_first=1")
        out.append(j + f"SPAWN true500={true500}")
        out.append(j + f"SPAWN node50={node50}")
        out.append(j + "SPAWN coldread400=9000")
    return "\n".join(out)


BLACKSMITH = log([(42, 813, 7, 116, 533), (41, 813, 7, 110, 549), (38, 763, 6, 113, 537)])
SUPERCI_POOL = log([(274, 3772, 0, 97, 501), (400, 1717, 0, 95, 499), (403, 1713, 0, 94, 496)])
SUPERCI_WARM = log([(11, 10, 1, 99, 478), (10, 10, 1, 96, 485), (11, 10, 1, 97, 473)])


class Spawn(unittest.TestCase):
    def test_a_warmed_machine_passes_against_blacksmith(self):
        ok, rows = S.verdict(S.parse(SUPERCI_WARM), S.parse(BLACKSMITH))
        self.assertTrue(ok, rows)

    def test_the_pool_machines_of_nov_26_fail_on_first_node_and_docker(self):
        ok, rows = S.verdict(S.parse(SUPERCI_POOL), S.parse(BLACKSMITH))
        self.assertFalse(ok)
        failed = {r[0] for r in rows if r[4] == "FAIL"}
        self.assertEqual(failed, {"node_first", "docker_first"})

    def test_fewer_than_three_probe_jobs_a_fleet_decide_nothing(self):
        ok, _ = S.verdict(S.parse(log([(11, 10, 1, 99, 478)] * 2)), S.parse(BLACKSMITH))
        self.assertFalse(ok)

    def test_the_probes_own_script_lines_are_not_measures(self):
        jobs = S.parse(BLACKSMITH)
        self.assertEqual(len(jobs), 3)
        self.assertEqual(sorted(j["node_first"] for j in jobs), [38, 41, 42])


if __name__ == "__main__":
    unittest.main()
