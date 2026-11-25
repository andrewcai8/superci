#!/usr/bin/env python3
"""Undoes each fix of round 2 in a copy of pull/ and runs the test written for it, which must fail. A fix whose
test still passes without it is reported as NOT RED. Run it in pull/ where Go is installed:

  python3 tools/redfirst.py
"""
import os, shutil, subprocess, sys, tempfile

T = "\t"
MUTANTS = [
    ("SEC-F1 an issued machine is granted", "state.go", f"{T}case m.Issued || r != nil:\n{T}{T}return End\n", "", "TestARestartNeverGivesAMachineASecondRunner"),
    ("SEC-F1 asking keeps an issued machine", "state.go", "if m.Ending || m.Issuing || m.Checking || now.Sub(m.Launched) < grace {",
     "if m.Ending || m.Issuing || m.Checking || now.Sub(m.Launched) < grace || now.Sub(m.Asked) < quiet {", "TestARestartNeverGivesAMachineASecondRunner"),
    ("SEC-F1 a completed job leaves its machine to end itself", "state.go", f"r.Busy, r.Done = true, true\n{T}{T}{T}m.Ending = true", "r.Busy, r.Done = true, true",
     "TestARestartNeverGivesAMachineASecondRunner"),
    ("SEC-F1 a completed job of an adopted machine is dropped", "state.go", f"r.Busy, r.Done = true, true\n{T}{T}{T}m.Ending = true", "r.Busy, r.Done = true, true",
     "TestAJobCompletedOnAMachineAdoptedAfterARestartEndsIt"),
    ("SEC-F1 a grant does not wait for a listing after the grace", "state.go", "if at.Before(s.started.Add(grantGrace)) {", "if at.Before(s.started) {",
     "TestAStartGrantsNothingUntilAListingBegunAfterTheGrace"),
    ("SEC-F1 a listed runner does not mark its machine issued", "state.go", f"m.Issued = true\n{T}{T}{T}held[m] = true", "held[m] = true",
     "TestAMachineWithAListedRunnerIsIssuedAndTagged"),
    ("SEC-F2 a runner that never started is replaced", "state.go", f"{T}case r != nil && !r.Busy:\n{T}{T}m.Retire = true\n{T}{T}return End\n",
     f"{T}case r != nil && !r.Busy:\n{T}{T}m.Runner = nil\n", "TestARunnerThatNeverStartedIsNeverReplaced"),
    ("OPS-2 no stall is reported", "state.go", "return now.Sub(s.waitingSince) > d && now.Sub(s.lastStart) > d", "return false",
     "TestJobsThatWaitWithNoneStartedAreStalled"),
    ("OPS-3 arrivals over the whole window", "state.go", "/ min(window, max(time.Minute, now.Sub(s.started))).Seconds()", "/ window.Seconds()",
     "TestARestartDoesNotReadAsNoArrivals"),
    ("OPS-3 pool slack trimmed in the first window", "state.go", " || now.Sub(s.started) < warmup", "", "TestNoPoolRunnerIsTrimmedInTheFirstWindowAfterAStart"),
    ("arrivals and queue waits only from JobAvailable", "state.go", f"{T}for _, j := range msg.JobAssignedMessages {{\n{T}{T}s.arrived(j.JobMessageBase, now)\n{T}}}\n", "",
     "TestAJobAssignedWithNoJobAvailableArrivesAndIsTimed"),
    ("arrivals keyed by the runner request id", "state.go", f"{T}case j.JobID != \"\":\n{T}{T}return j.JobID\n", "", "TestAJobAssignedWithoutAnOfferSizesThePool"),
    ("a job with neither id is merged with the others", "state.go", "\treturn \"\"\n}", "\treturn \"request:0\"\n}", "TestAJobWithNeitherIDCountsAndIsNeverMergedWithAnother"),
    ("arrivals dated by the message", "state.go", f"{T}{T}at = t\n", "", "TestARedeliveredBacklogKeepsItsOwnTimes"),
    ("a wait from the message, not the assignment", "state.go", f"{T}queued := at\n", f"{T}queued := now\n", "TestAJobAssignedWithoutAnOfferSizesThePool"),
    ("OPS-4 the second sample after 100 s", "budget.go", "wait = 61 * time.Second", "wait = 100 * time.Second", "TestTheSecondSampleComesAMinuteAfterTheFirst"),
    ("OPS-5 decisions before every region is listed", "state.go", f"{T}for _, at := range s.listings {{\n{T}{T}if at.IsZero() {{", f"{T}for _, at := range s.listings {{\n{T}{T}if false && at.IsZero() {{",
     "TestNothingIsDecidedUntilEveryRegionIsListed"),
    ("OPS-6 no bound from a job's start, none on idle age", "state.go", "r != nil && r.Busy && !r.Started.IsZero() && now.Sub(r.Started) > jobMax", "false",
     "TestJobsAreBoundedFromTheirStartAndIdleRunnersByAge"),
    ("OPS-8 any removal error is a job", "server.go", f"{T}default:\n{T}{T}return NotSure", f"{T}default:\n{T}{T}return HeldJob",
     "TestARemovalThatFailsIsTriedAgainNotTakenForAJob"),
    ("OPS-9 unknown fields accepted", "config.go", f"{T}d.DisallowUnknownFields()\n", "", "TestAnOverrideWithATypoOrAShortHourlyArrayIsRefused"),
    ("OPS-9 hourly arrays of any length", "config.go", "if len(v) != 24 {", "if false {", "TestAnOverrideWithATypoOrAShortHourlyArrayIsRefused"),
    ("OPS-11 no public IPv4 in the price", "spend.go", " + ipv4PerHour", "", "TestAMachinesPriceCountsItsAddress"),
    ("OPS-12 re-run whatever did not succeed", "rerun.go", 'if id == 0 || conclusion != "failure" {', 'if id == 0 || conclusion == "success" || conclusion == "skipped" {',
     "TestOnlyAFailedJobASpotReclaimTookIsReRun"),
    ("OPS-12 re-run with no proof of a reclaim", "rerun.go", "if !j.proven && !reclaimed[instanceOf(j.Runner)] {", "if !j.proven && reclaimed == nil {",
     "TestOnlyAFailedJobASpotReclaimTookIsReRun"),
    ("OPS-12 a spot notice records no job", "state.go", f"{T}case r != nil && r.Job != nil:\n{T}{T}j := *r.Job\n{T}{T}return &j\n", "",
     "TestASpotNoticeRecordsTheJobAndARestartReadsItBack"),
    ("OPS-13 a finished runner is checked as stuck", "state.go", f"r.Busy, r.Done = true, true\n{T}{T}{T}m.Ending = true", "r.Busy, r.Done = true, true",
     "TestAFinishedRunnerEndsItsMachineAndIsNeverChecked"),
]

here = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
bad = 0
for name, path, old, new, test in MUTANTS:
    with tempfile.TemporaryDirectory() as tmp:
        dst = os.path.join(tmp, "pull")
        shutil.copytree(here, dst)
        src = open(os.path.join(dst, path)).read()
        if src.count(old) < 1:
            print(f"MISSING  {name}: {path} has no {old!r}")
            bad += 1
            continue
        open(os.path.join(dst, path), "w").write(src.replace(old, new))
        out = subprocess.run(["go", "test", "-count=1", "-run", f"^{test}$", "."], cwd=dst, capture_output=True, text=True)
        red = out.returncode != 0 and f"--- FAIL: {test}" in out.stdout
        print(f"{'red    ' if red else 'NOT RED'}  {name}: {test}")
        if not red:
            print("    " + "\n    ".join((out.stdout + out.stderr).strip().splitlines()[:8]))
        bad += not red
sys.exit(1 if bad else 0)
