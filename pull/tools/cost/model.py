#!/usr/bin/env python3
"""What megpt-mono's CI would cost a month with every Linux job on superci-pull, from measured inputs, and what each
lever does to it.

  model.py BS_DIR MACHINES_JSON [--pool-low 278] [--pool-high 424]

BS_DIR holds Blacksmith CLI output: `days/<YYYY-MM-DD>.json` (`usage --breakdown-by runner_type,workflow` per UTC day)
`agg24.json` (`jobs aggregate --since 24h --group-by workflow,job_name,runner_label --limit 1000`) and
`usage24-runner-wf.json` (`usage` over the same 24 h, `--breakdown-by runner_type,workflow`). MACHINES_JSON
is `cost.py report DIR --json` output. The pool's idle dollars a day come from the pull-wait lane's weekday model
(plain machines low, hydrated high); nothing here measures them.

Every superci-pull cost is (job vCPU-hours + per-job machine overhead vCPU-hours) x the measured all-in $ per
vCPU-hour, plus the pool, plus the account's fixed lines. Blacksmith's work is its wall minutes x vCPU (no per-minute
round-up), so the comparison is per-second work on both sides.
"""
import argparse
import collections
import glob
import json
import os
import re
import statistics

LEVEL = {2, 4, 8, 16, 32}


def blacksmith(bs):
    days = {}
    for f in sorted(glob.glob(f"{bs}/days/*.json")):
        d = json.load(open(f))
        lin = sum(r["cost_usd"] for r in d["breakdowns"]["runner_type"] if "macos" not in r["runner_type"])
        mac = sum(r["cost_usd"] for r in d["breakdowns"]["runner_type"] if "macos" in r["runner_type"])
        days[os.path.basename(f)[:10]] = (lin, mac)
    rows = json.load(open(f"{bs}/agg24.json"))["groups"]
    w = collections.Counter()
    for r in rows:
        lab = r["runner_label"]
        if "ubuntu" not in lab or "arm" in lab:
            continue
        v = int(re.search(r"(\d+)vcpu", lab).group(1))
        w["billed"] += r["total_billable_minutes"]
        w["real"] += r["total_wall_minutes"] * v
        w["used"] += r["total_wall_minutes"] * v * (r["cpu_avg_pct_p50"] or 0) / 100
        w["jobs"] += r["run_count"]
        w["vcpu_jobs"] += v * r["run_count"]
    # The aggregate lists the top 1,000 job groups; usage over the same 24 h counts every job, so scale up to it.
    u = json.load(open(f"{bs}/usage24-runner-wf.json"))
    billed = sum(r["billable_minutes"] for r in u["breakdowns"]["runner_type"] if "macos" not in r["runner_type"])
    scale = billed / w["billed"]
    for k in list(w):
        w[k] *= scale
    w["scale"] = scale
    return days, w


def superci(machines, workflow):
    """The rate and per-job overhead of machines launched for a real job of WORKFLOW: the pool's idle time is its own
    line, so a machine that waited registered (10 s or more) is left out here."""
    ms = [m for m in json.load(open(machines)) if m.get("kind", "").startswith("pull:") and m.get("usd") is not None]
    withjob = [m for m in ms if (m.get("phases") or {}).get("job") and m.get("vcpu") and
               (m.get("workflow") or "").startswith(workflow) and m["phases"].get("wait", 0) < 10]
    rate = [m["usd"] / (m["seconds"] * m["vcpu"] / 3600) for m in withjob]
    over = [m["seconds"] - m["phases"]["job"] for m in withjob]
    return {"n": len(withjob), "rate_p50": statistics.median(rate), "rate_mean": sum(m["usd"] for m in withjob) /
            sum(m["seconds"] * m["vcpu"] / 3600 for m in withjob), "over_p50": statistics.median(over),
            "over_mean": sum(over) / len(over), "workflow": workflow}


def main():
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("bs")
    p.add_argument("machines")
    p.add_argument("--pool-low", type=float, default=278)
    p.add_argument("--pool-high", type=float, default=424)
    p.add_argument("--workflow", default="Lint & Test", help="the jobs whose machines measure rate and overhead")
    p.add_argument("--fixed-day", type=float, default=12.0, help="listener, cache, Lambda, alarms: $ a day")
    p.add_argument("--host-rate", type=float, nargs=2, default=[0.0094, 0.0139],
                   help="spot $/vCPU-hour of a 96-192 vCPU host with nested virtualization or metal, low and high")
    a = p.parse_args()
    days, w = blacksmith(a.bs)
    s = superci(a.machines, a.workflow)
    last7 = sorted(days)[-7:]
    lin7 = sum(days[d][0] for d in last7) / len(last7)
    mac7 = sum(days[d][1] for d in last7) / len(last7)
    real_h = w["real"] / 60
    vcpu_per_job = w["vcpu_jobs"] / w["jobs"]
    print("## Inputs\n")
    print(f"- Blacksmith Linux list $/day, mean of {last7[0]}..{last7[-1]}: {lin7:,.0f} ({30 * lin7:,.0f} a month) [M]")
    print(f"- Blacksmith Mac list $/day, same days: {mac7:,.0f} ({30 * mac7:,.0f} a month) [M]")
    print(f"- Linux last 24 h: {w['jobs']:,.0f} jobs, billed {w['billed']:,.0f} vCPU-min, real {w['real']:,.0f} "
          f"({w['real'] / w['billed']:.2f} of billed), CPU-busy {w['used']:,.0f} ({w['used'] / w['real']:.2f} of real); "
          f"{vcpu_per_job:.2f} vCPU and {w['real'] / w['vcpu_jobs'] * 60:.0f} s per job [M; the aggregate's top groups "
          f"scaled x{w['scale']:.2f} to all jobs]")
    print(f"- superci-pull machines launched for a {s['workflow']} job: n={s['n']}, all-in $/vCPU-hour p50 {s['rate_p50']:.4f}, "
          f"dollar-weighted {s['rate_mean']:.4f}; paid seconds beyond the job p50 {s['over_p50']:.0f}, mean "
          f"{s['over_mean']:.0f} [M]")
    print(f"- Pool idle $/day {a.pool_low:.0f}-{a.pool_high:.0f} [modelled by pull-wait], fixed ${a.fixed_day:.0f}/day [I]\n")

    def month(real_vcpu_h, over_s, rate, pool, jobs=w["jobs"], vpj=vcpu_per_job, fixed=a.fixed_day):
        return 30 * ((real_vcpu_h + jobs * vpj * over_s / 3600) * rate + pool + fixed)

    lo = month(real_h, s["over_p50"], s["rate_p50"], a.pool_low)
    hi = month(real_h, s["over_mean"], s["rate_mean"], a.pool_high)
    work = 30 * real_h * s["rate_p50"]
    print("## Full Linux migration, today's jobs as they are\n")
    print(f"- Job seconds alone at superci's rate: ${work:,.0f} a month")
    print(f"- With per-job machine overhead and the pool: ${lo:,.0f} to ${hi:,.0f} a month")
    print(f"- Against Blacksmith Linux ${30 * lin7:,.0f}: {30 * lin7 / hi:.1f}x to {30 * lin7 / lo:.1f}x")
    lin24 = 30 * w["billed"] * 0.002
    print(f"- Against the same 24 h's Linux at Blacksmith list (${lin24:,.0f} a month): {lin24 / hi:.1f}x to {lin24 / lo:.1f}x")
    print(f"- Whole bill with Mac unchanged: ${lo + 30 * mac7:,.0f} to ${hi + 30 * mac7:,.0f} against "
          f"${30 * (lin7 + mac7):,.0f}: {(lin7 + mac7) * 30 / (hi + 30 * mac7):.1f}x to "
          f"{(lin7 + mac7) * 30 / (lo + 30 * mac7):.1f}x")
    print(f"- 10x of the whole bill is ${3 * (lin7 + mac7):,.0f} a month\n")

    used = w["used"] / w["real"]
    mid_rate, mid_over, mid_pool = (s["rate_p50"] + s["rate_mean"]) / 2, (s["over_p50"] + s["over_mean"]) / 2, (a.pool_low + a.pool_high) / 2
    base = month(real_h, mid_over, mid_rate, mid_pool)
    host_lo, host_hi = a.host_rate
    levers = [
        # name, low-cost month, high-cost month, label
        ("Several jobs per host in microVMs (CPU packed to 60-75% busy, ~2 s per job, pool = 15% headroom, no per-job "
         "disk or IPv4)",
         30 * (real_h * used / 0.75 * 1.15 * host_lo + w["jobs"] * vcpu_per_job * 2 / 3600 * host_lo + a.fixed_day),
         30 * (real_h * used / 0.60 * 1.15 * host_hi + w["jobs"] * vcpu_per_job * 2 / 3600 * host_hi + a.fixed_day),
         "host $/vCPU-h [M, one snapshot], packing and headroom [I]"),
        ("Right-size each job to the smallest size its run's p95 or mean load fits",
         month(real_h * 0.82, mid_over, mid_rate, mid_pool), month(real_h * 0.92, mid_over, mid_rate, mid_pool),
         "0.82 and 0.92 of real vCPU-min from Blacksmith's per-job CPU [M]"),
        ("Less work: queue batching, owed-input on its filter, cancel on first failure, bake 32->8 (20-40% of real)",
         month(real_h * 0.6, mid_over, mid_rate, mid_pool * 0.7, jobs=w["jobs"] * 0.6),
         month(real_h * 0.8, mid_over, mid_rate, mid_pool * 0.85, jobs=w["jobs"] * 0.8),
         "shares [M], cut fractions [I]"),
        ("Half as many jobs (merge sub-minute jobs off the critical path)",
         month(real_h * (1 - 0.19 * 0.5), mid_over, mid_rate, mid_pool * 0.75, jobs=w["jobs"] * 0.5),
         month(real_h * (1 - 0.19 * 0.25), mid_over, mid_rate, mid_pool * 0.9, jobs=w["jobs"] * 0.5),
         "setup 19% of real [M, waste.md], half the jobs [I]"),
        ("Shorter launch-to-registered (19 s -> 12 s), pool $ linear in it",
         month(real_h, mid_over - 7, mid_rate, mid_pool * 12 / 19), month(real_h, mid_over - 4, mid_rate, mid_pool * 15 / 19),
         "linearity [I, audit F4]"),
    ]
    print(f"## Levers, each alone against the full-migration midpoint ${base:,.0f} a month\n")
    print("| lever | Linux $/month after | saves $/month | label |")
    print("|---|---:|---:|---|")
    for name, lo_, hi_, label in levers:
        print(f"| {name} | {lo_:,.0f}-{hi_:,.0f} | {base - hi_:,.0f}-{base - lo_:,.0f} | {label} |")
    packed_lo = 30 * (real_h * 0.6 * used / 0.75 * 1.15 * host_lo + w["jobs"] * 0.6 * vcpu_per_job * 2 / 3600 * host_lo + a.fixed_day)
    packed_hi = 30 * (real_h * 0.8 * used / 0.60 * 1.15 * host_hi + w["jobs"] * 0.8 * vcpu_per_job * 2 / 3600 * host_hi + a.fixed_day)
    print(f"\nmicroVM packing with 20-40% less work: Linux ${packed_lo:,.0f}-{packed_hi:,.0f} a month "
          f"({30 * lin7 / packed_hi:.0f}x-{30 * lin7 / packed_lo:.0f}x on Linux). Whole bill at 10x leaves Mac "
          f"${3 * (lin7 + mac7) - packed_hi:,.0f}-{3 * (lin7 + mac7) - packed_lo:,.0f} a month, against Mac today "
          f"${30 * mac7:,.0f}.")


if __name__ == "__main__":
    main()
