import os
import sys
import unittest

sys.path.insert(0, os.path.dirname(__file__))
import cost as C


class VCPU(unittest.TestCase):
    def test_names(self):
        self.assertEqual([C.vcpu_of(n) for n in ("superci-2cpu", "superci-16cpu", "superci-4cpu-2", "superci-41cpu", "superci-81cpu", "superci-32cpu")],
                         [2, 16, 4, 4, 8, 32])

    def test_unreadable(self):
        self.assertEqual([C.vcpu_of(n) for n in ("superci-96cpu", "blacksmith-4vcpu", "superci-cpu")], [None, None, None])


class Rows(unittest.TestCase):
    def rows(self, day="2026-11-25"):
        ce = {day: {"EC2": 300.0, "EC2 - Other": 60.0, "VPC": 20.0, "DynamoDB": 1.0, "Tax": 0.0}}
        jobs = {day: {"superci-4cpu": (10, 1000.0), "superci-41cpu": (5, 500.0), "superci-2cpu": (20, 250.0), "superci-96cpu": (1, 3.0)}}
        return C.build_rows([day], ce, {day: 6000.0}, jobs, "2026-11-01T00:00:00Z")[0]

    def test_net_of_avoided_against_cost(self):
        r = self.rows()
        self.assertEqual(r["vcpu_minutes"], 4 * 1500 + 2 * 250)
        self.assertAlmostEqual(r["avoided_list_usd"], 6500 * 0.002)
        self.assertAlmostEqual(r["superci_usd"], 381.0)
        self.assertAlmostEqual(r["net_list_usd"], 13.0 - 381.0)
        self.assertAlmostEqual(r["net_invoice_usd"], 13.0 * 0.5 - 381.0)

    def test_per_job_minute_counts_every_minute_and_job(self):
        r = self.rows()
        self.assertEqual(r["jobs"], 36)
        self.assertAlmostEqual(r["job_minutes"], 1753.0)
        self.assertAlmostEqual(r["superci_usd_per_job_minute"], 381.0 / 1753.0)
        self.assertAlmostEqual(r["unmapped_minutes"], 3.0)

    def test_top_services_and_invoice_after_discount(self):
        self.assertEqual([k for k, _ in self.rows()["top_services"]], ["EC2", "EC2 - Other", "VPC"])
        r = self.rows("2026-12-01")
        self.assertAlmostEqual(r["net_invoice_usd"], r["net_list_usd"])

    def test_days_before_the_first_job_event_have_no_net(self):
        ce = {d: {"EC2": 100.0} for d in ("2026-11-24", "2026-11-25", "2026-11-26")}
        jobs = {"2026-11-25": {"superci-4cpu": (3, 30.0)}, "2026-11-26": {"superci-4cpu": (10, 100.0)}}
        a, b, c = C.build_rows(sorted(ce), ce, {}, jobs, "2026-11-25T17:00:00Z")
        self.assertEqual((a["job_events"], a["avoided_list_usd"], a["net_list_usd"]), ("none", None, None))
        self.assertEqual((b["job_events"], b["superci_usd_per_job_minute"]), ("partial", None))
        self.assertAlmostEqual(b["net_list_usd"], 120 * 0.002 - 100.0)
        self.assertAlmostEqual(c["superci_usd_per_job_minute"], 1.0)
        self.assertAlmostEqual(c["net_list_usd"], 400 * 0.002 - 100.0)

    def test_a_missing_blacksmith_day_prints_a_dash_in_the_total(self):
        rows = C.build_rows(["2026-11-25", "2026-11-26"], {}, {"2026-11-25": 10.0}, {}, "2026-11-01T00:00:00Z")
        total = [l for l in C.render(rows, "x").splitlines() if l.startswith("total")][0]
        self.assertEqual(total.split()[2], "-")

    def test_a_day_with_no_jobs_has_no_rate(self):
        r = C.build_rows(["2026-11-20"], {"2026-11-20": {}}, {}, {}, "2026-11-01T00:00:00Z")[0]
        self.assertEqual((r["superci_usd"], r["superci_usd_per_job_minute"], r["net_list_usd"], r["blacksmith_list_usd"]), (0, None, 0, None))

class Records(unittest.TestCase):
    def rows(self, records):
        day = "2026-11-26"
        return C.build_rows([day], {day: {"EC2": 100.0}}, {}, {day: {"superci-4cpu": (10, 100.0)}}, "2026-11-01T00:00:00Z", records=records)[0]

    def test_the_records_price_a_job_and_split_the_idle_from_the_busy_share(self):
        r = self.rows({"2026-11-26": {"superci-4cpu": (30, 40.0, 25.0), "superci-8cpu": (10, 20.0, 15.0)}})
        self.assertEqual(r["record_jobs"], 40)
        self.assertAlmostEqual(r["record_usd"], 60.0)
        self.assertAlmostEqual(r["record_idle_usd"], 20.0)
        self.assertAlmostEqual(r["record_idle_share"], 1 / 3)
        self.assertAlmostEqual(r["record_usd_per_job"], 1.5)
        self.assertAlmostEqual(r["record_share_of_bill"], 0.6)

    def test_a_day_with_no_records_has_no_estimate_and_the_table_leaves_it_out(self):
        r = self.rows({})
        self.assertEqual((r["record_jobs"], r["record_usd"], r["record_idle_share"], r["record_usd_per_job"]), (0, None, None, None))
        self.assertNotIn("job records", C.render([r], "x"))

    def test_the_rendering_names_the_records_and_the_bill_they_cover(self):
        out = C.render([self.rows({"2026-11-26": {"superci-4cpu": (30, 40.0, 25.0)}})], "x")
        self.assertIn("the listener's job records", out)
        self.assertRegex(out, r"2026-11-26\s+30\s+40\.00\s+15\.00\s+38%\s+1\.333\s+40%")

    def test_the_query_reads_one_line_per_job(self):
        self.assertIn('ev="job.record"', C.RECORDS_QUERY)
        self.assertIn("by job, set", C.RECORDS_QUERY)
        self.assertIn("isPresent(busy_usd)", C.RECORDS_QUERY, "a partial record has no busy share to split")


if __name__ == "__main__":
    unittest.main()
