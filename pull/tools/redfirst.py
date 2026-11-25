#!/usr/bin/env python3
"""Undoes each fix of round 2, of the fence round (audit E) and of the wait round (F1 to F4) in a copy of pull/ and runs the test written for it, which must fail. A fix whose
test still passes without it is reported as NOT RED. Run it in pull/ where Go is installed:

  python3 tools/redfirst.py [name fragment ...]   # only the mutants whose name holds one of them
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
    ("E1 no claim before the registration", "server.go", "\tif err != nil {\n\t\th.budget.Done(time.Now(), Unused, 0)", "\tif false && err != nil {\n\t\th.budget.Done(time.Now(), Unused, 0)",
     "TestAMachineGetsOneConfigEverAcrossRestartsForgettingAndASecondListener"),
    ("E1 any grant may use a claim", "claims.go", "if string(holder) != token {", "if string(holder) == \"\" {",
     "TestAMachineGetsOneConfigEverAcrossRestartsForgettingAndASecondListener"),
    ("E1 a claim is the process's, not the grant's", "state.go", "\t\tm.Claim = newToken()", "\t\tm.Claim = s.boot", "TestAGrantThisProcessForgotCannotReuseItsClaim"),
    ("E1 a grant forgets its claim after a lost answer", "state.go", "\tif m.Claim == \"\" {\n\t\tm.Claim = newToken()\n\t}", "\tm.Claim = newToken()",
     "TestAGrantGoesOnAfterALostClaimAnswerOrAFailedRegistrationAndNothingLeavesWithoutItsClaim"),
    ("E1 a machine claimed elsewhere ends at once", "state.go", "\tm.Issuing, m.Issued, m.Claim = false, true, \"\"\n}", "\tm.Issuing, m.Issued, m.Claim, m.Ending = false, true, \"\", true\n}",
     "TestAMachineClaimedElsewhereIsCheckedWithGitHubNotEnded"),
    ("E1 a dropped machine forgets it was issued", "state.go", "\t\t\t\tl.Issued = true\n", "", "TestAMachineAListingDroppedStaysIssued"),
    ("E6 launches on a lost session", "state.go", "if s.capped || !s.ready() || !s.leased(now) {", "if s.capped || !s.ready() {",
     "TestALostSessionLaunchesRetiresAndEndsNothingOnOldStatistics"),
    ("E6 retires surplus on a lost session", "state.go", "!s.ready() || !s.leased(now) || now.Sub(s.started) < warmup", "!s.ready() || now.Sub(s.started) < warmup",
     "TestALostSessionLaunchesRetiresAndEndsNothingOnOldStatistics"),
    ("E6 ends surplus machines on a lost session", "state.go", "s.leased(now) && live > s.desired()", "live > s.desired()",
     "TestALostSessionLaunchesRetiresAndEndsNothingOnOldStatistics"),
    ("E7 Shed before the set is ready", "state.go", "\tif !s.ready() {\n\t\treturn nil\n\t}\n\tvar waiting []*Machine", "\tvar waiting []*Machine",
     "TestShedWaitsForReadinessAndARunnerListing"),
    ("E7 Shed with no runner listing", "state.go", " && m.Missed >= 1 && ", " && ", "TestShedWaitsForReadinessAndARunnerListing"),
    ("E8 a failed tag batch is tried id by id", "tagger.go", "\tt.tag(ctx, r, part[:len(part)/2])\n\tt.tag(ctx, r, part[len(part)/2:])",
     "\tfor i := range part {\n\t\tt.tag(ctx, r, part[i:i+1])\n\t}", "TestAFailedTagBatchIsSplitNotTriedIdByIdAlone"),
    ("E9 a dropped set deleted with machines", "state.go", "len(s.machines) > 0 || ", "", "TestADroppedSetIsDeletedOnlyOnceItHoldsNothing"),
    ("SEC-F1 a listed runner does not mark its machine issued", "state.go", f"m.Issued = true\n{T}{T}{T}held[m] = true", "held[m] = true",
     "TestAMachineWithAListedRunnerIsIssuedAndTagged"),
    ("SEC-F2 a runner that never started is replaced", "state.go", f"{T}case r != nil && !r.Busy:\n{T}{T}m.Retire = true\n{T}{T}return End\n",
     f"{T}case r != nil && !r.Busy:\n{T}{T}m.Runner = nil\n", "TestARunnerThatNeverStartedIsNeverReplaced"),
    ("OPS-2 no stall is reported", "state.go", "return now.Sub(s.waitingSince) > d && now.Sub(s.lastStart) > d", "return false",
     "TestJobsThatWaitWithNoneStartedAreStalled"),
    ("OPS-3 the pool reads no arrivals", "state.go", f"{T}return max(n, s.carriedIn(now, r))\n}}\n\n// carriedIn", f"{T}return 0\n}}\n\n// carriedIn", "TestARestartDoesNotReadAsNoArrivals"),
    ("the pool shrinks at once", "state.go", f"{T}for _, h := range s.held {{\n{T}{T}n = max(n, h.n)\n{T}}}\n{T}return max(n, s.carriedIn", f"{T}return max(n, s.carriedIn", "TestThePoolShrinksOnlyAfterTheHold"),
    ("the launch time is ready_seconds for ever", "state.go", "if len(s.launches) < 8 {", "if true {", "TestThePoolIsSizedForTheSetsOwnLaunchTime"),
    ("the launch time counts the budget's wait", "state.go", f"{T}{T}ready := m.FirstAsk\n", f"{T}{T}ready := r.Issued\n", "TestTheLaunchTimeEndsAtTheMachinesFirstAsk"),
    ("a restart forgets the carried burst", "state.go", "return max(n, s.carriedIn(now, r))\n}", "return n\n}", "TestARestartStartsFromTheBurstTheLastListenerCarried"),
    ("OPS-3 pool slack trimmed in the first window", "state.go", " || now.Sub(s.started) < warmup", "", "TestNoPoolRunnerIsTrimmedInTheFirstWindowAfterAStart"),
    ("arrivals and queue waits only from JobAvailable", "state.go", f"{T}for _, j := range msg.JobAssignedMessages {{\n{T}{T}s.arrived(j.JobMessageBase, now)\n{T}}}\n", "",
     "TestAJobAssignedWithNoJobAvailableArrivesAndIsTimed"),
    ("arrivals keyed by the runner request id", "state.go", f"{T}case j.JobID != \"\":\n{T}{T}return j.JobID\n", "", "TestAJobAssignedWithoutAnOfferSizesThePool"),
    ("a job with neither id is merged with the others", "state.go", "\treturn \"\"\n}", "\treturn \"request:0\"\n}", "TestAJobWithNeitherIDCountsAndIsNeverMergedWithAnother"),
    ("arrivals dated by the message", "state.go", f"{T}{T}at = t\n", "", "TestARedeliveredBacklogKeepsItsOwnTimes"),
    ("a wait from the message, not the assignment", "state.go", f"{T}queued := at\n", f"{T}queued := now\n", "TestAJobAssignedWithoutAnOfferSizesThePool"),
    ("OPS-4 the second sample after 100 s", "budget.go", "wait = 61 * time.Second", "wait = 100 * time.Second", "TestTheSecondSampleComesAMinuteAfterTheFirst"),
    ("a failed sample retried every tick", "budget.go", f"{T}if now.Before(b.sampleNext) {{\n{T}{T}return false\n{T}}}\n", "",
     "TestAFailedSampleIsNotRetriedEveryTick"),
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
    ("F1 a carried burst is dropped from the next save", "state.go", f"{T}c.Burst = max(c.Burst, s.carriedIn(now, r))\n", "", "TestACarriedBurstIsSavedAgainWhileItHolds"),
    ("F1 a carried burst never ends", "state.go", "if now.Sub(s.started) < r.window() {\n\t\treturn s.carried", "if true {\n\t\treturn s.carried",
     "TestACarriedBurstIsSavedAgainWhileItHolds"),
    ("F2 every deployment saves to one parameter", "carry.go", 'return strings.TrimRight(prefix, "/") + "/" + deployment', 'return strings.TrimRight(prefix, "/")',
     "TestTwoDeploymentsSavingDoNotEraseEachOther"),
    ("F2 every deployment saves to one parameter (name)", "carry.go", 'return strings.TrimRight(prefix, "/") + "/" + deployment', 'return strings.TrimRight(prefix, "/")',
     "TestEachDeploymentHasItsOwnPoolParameter"),
    ("F3 a probe's removal shares the stopping listener's context", "main.go", "rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)", "rctx, cancel := context.WithTimeout(ctx, 10*time.Second)",
     "TestAProbeIsRemovedEvenWhenTheListenerIsStopping"),
    ("F3 a probe is never removed", "main.go", "if err := c.RemoveRunner(rctx, int64(cfg.Runner.ID)); err != nil {", "if err := func() error { _ = rctx; return nil }(); err != nil {",
     "TestAProbeIsRemovedOnceItGaveTheHead"),
    ("F3 the lister sweeps no leaked probe", "main.go", "leaked := leakedProbes(runners, now, grace)", "var leaked []ListedRunner",
     "TestAProbeWhoseRemovalFailedIsSweptOnceItIsOldEnough"),
    ("F3 the sweep takes a probe still inside its grace", "main.go", "if now.Sub(time.Unix(0, ns)) > grace {", "if now.Sub(time.Unix(0, ns)) > 0 {",
     "TestAProbeWhoseRemovalFailedIsSweptOnceItIsOldEnough"),
    ("F3 the sweep takes any runner but a probe's", "main.go", 'if !strings.HasPrefix(name, probePrefix) || err != nil || r.Busy {', 'if err != nil && !strings.HasPrefix(name, probePrefix) {',
     "TestTheSweepKeepsWhatIsNotAnOldProbe"),
    ("F3 the sweep takes a busy probe", "main.go", "|| err != nil || r.Busy {", "|| err != nil {", "TestTheSweepKeepsWhatIsNotAnOldProbe"),
    ("F4 a save older than the window is resumed", "carry.go", f"{T}if now.Sub(saved.At) > window {{\n{T}{T}return nil\n{T}}}\n", "",
     "TestLoadingResumesOnlyWhatWasSavedWithinTheWindow"),
    ("F4 a save keeps no burst", "carry.go", "saved.Sets[s.spec.Name] = s.state.Carry(now, rule)", "saved.Sets[s.spec.Name] = Carried{}",
     "TestABurstSurvivesRestartsThroughTheSavedParameter"),
    ("F4 a first start with nothing saved fails", "carry.go", f"{T}if errors.As(err, &missing) {{\n{T}{T}return nil\n{T}}}\n", f"{T}if errors.As(err, &missing) {{\n{T}{T}return err\n{T}}}\n",
     "TestLoadingWithNothingSavedStartsFromTheFloor"),
]

here = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
bad = 0
for name, path, old, new, test in MUTANTS:
    if sys.argv[1:] and not any(a in name for a in sys.argv[1:]):
        continue
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
