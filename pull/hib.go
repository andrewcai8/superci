package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"
)

// A dispatch set's hibernated pool (SizeConfig.Hibernated) stands behind its running pool. Each hand-off orders a
// replacement; with no hibernated pool that is a cold launch, ready about 47 s later (28.9 s of it hydration), and at
// unit-tests' 100 legs a minute a running pool of 60 drains. A hibernated machine was booted, hydrated and pinned, then
// hibernated: StartInstances brings it back in about 10 s with its page cache and pin, and while it sleeps it costs its
// volume only. So a hand-off resumes one, and the hibernated pool is refilled by cold launches in the background.
//
// A machine of the pool (dmachine.back) is never the running pool's supply: live() leaves it out until it is drawn.
//   - Launched by BackDeficit's rounds (LaunchBack: on demand, the size's types in order, every zone), it boots as any
//     dispatch machine does and asks /work. Unless a job is waiting then, it is not put in the idle queue: it is
//     mHibernating, and the back pool's loop asks EC2 to hibernate it (Fleet.Hibernate). Its connection is left to break
//     when it sleeps; a /work it sends meanwhile is answered 503, so its agent asks again later.
//   - Once EC2 lists it stopped it is mHibernated: it may be resumed.
//   - Draw takes the oldest for the running pool's deficit before Deficit launches cold: each is drawn (back false),
//     mResuming, counted as supply as a booting machine is, and started (Fleet.Start). Its agent sees that it slept and
//     asks /work at once (agent.py), which puts it in the idle queue as a waiting machine.
//   - A resume EC2 refuses, or one whose machine has not asked /work within resumeLate, is ended: it stops counting, and
//     the next Plan launches cold for it (st.owed), never another resume. A hibernation EC2 refuses, or that EC2 has not finished within parkLate,
//     ends its machine too.

const (
	// resumeLate is how long a resumed machine has to ask /work. The lab's resumes of hydrated, pinned machines on
	// 1,000 MiB/s roots were ready (ssh) at 10.6 s; across 100 resumes a minute ready was p90 14.2 s and at most 22.0 s
	// (23.6 s from the first try when throttled), and the agent asks within a second of waking. 30 s is past every one
	// of those with room for the call's own retries, and a late one costs a cold launch on top: about 77 s for a job that
	// had no other machine, against a cold launch's 47 s.
	resumeLate = 30 * time.Second
	// parkLate is how long EC2 has to list a machine stopped after it took the hibernation: the lab's took 7 to 36 s.
	parkLate = 3 * time.Minute
	// hibReady is how long after its launch EC2 may still refuse a machine's hibernation as "not ready to hibernate yet":
	// the lab's pilots were refused at 1m41s and taken by 2m06s and by 3m53s. Until then that refusal is asked again,
	// parkRetry after the first and twice as long each time after, up to a minute.
	hibReady  = 6 * time.Minute
	parkRetry = 15 * time.Second
	// The hibernated machines' root: hibernation writes their memory to it, and a resume reads it back at the volume's
	// throughput (hydrated and pinned: 20 to 22 s at 250 MiB/s, 10.6 s at 1,000 MiB/s and 4,000 IOPS).
	hibIOPS = 4000
	hibMBps = 1000
	// powerBatch is the most machines one StartInstances or StopInstances names; one call with 100 was taken in 2.7 s.
	powerBatch = 50
)

// hibTemplate is the launch template a hibernated pool's machines of one type launch from in a region.
type hibTemplate struct {
	template, version string
	rootGB            int
}

// typesAPI is the call that reads a type's memory and whether it hibernates, so a test can stand in for EC2.
type typesAPI interface {
	DescribeInstanceTypes(ctx context.Context, in *ec2.DescribeInstanceTypesInput, opts ...func(*ec2.Options)) (*ec2.DescribeInstanceTypesOutput, error)
}

// powerAPI is the calls that hibernate and resume machines, so a test can stand in for EC2.
type powerAPI interface {
	StartInstances(ctx context.Context, in *ec2.StartInstancesInput, opts ...func(*ec2.Options)) (*ec2.StartInstancesOutput, error)
	StopInstances(ctx context.Context, in *ec2.StopInstancesInput, opts ...func(*ec2.Options)) (*ec2.StopInstancesOutput, error)
}

// hibRootGB is a hibernated machine's root: its memory, which hibernation writes there, and the size's disk.
func hibRootGB(memMiB int64, diskGB int) int { return int((memMiB+1023)/1024) + diskGB }

// hibTemplateData is a hibernated pool's template for one type: the set's machines' data, with an encrypted root of
// rootGB at hibIOPS and hibMBps, launched able to hibernate. Only these machines get that volume: every other
// machine of the set keeps the size's own.
func hibTemplateData(r *Region, userData, group string, rootGB int, spec SetSpec, tags []ec2types.Tag) *ec2types.RequestLaunchTemplateData {
	d := templateData(r, userData, group, rootGB, spec, tags)
	d.BlockDeviceMappings = []ec2types.LaunchTemplateBlockDeviceMappingRequest{{DeviceName: aws.String(r.root), Ebs: &ec2types.LaunchTemplateEbsBlockDeviceRequest{
		VolumeSize: aws.Int32(int32(rootGB)), VolumeType: ec2types.VolumeTypeGp3, Iops: aws.Int32(hibIOPS), Throughput: aws.Int32(hibMBps),
		Encrypted: aws.Bool(true), DeleteOnTermination: aws.Bool(true)}}}
	d.HibernationOptions = &ec2types.LaunchTemplateHibernationOptionsRequest{Configured: aws.Bool(true)}
	return d
}

// imageRootGiB is the size of the image's root snapshot, read once from DescribeImages.
func imageRootGiB(ctx context.Context, api imageAPI, image, root string) (int64, error) {
	out, err := api.DescribeImages(ctx, &ec2.DescribeImagesInput{ImageIds: []string{image}})
	if err != nil {
		return 0, err
	}
	for _, img := range out.Images {
		for _, b := range img.BlockDeviceMappings {
			if aws.ToString(b.DeviceName) == root && b.Ebs != nil && aws.ToInt32(b.Ebs.VolumeSize) > 0 {
				return int64(aws.ToInt32(b.Ebs.VolumeSize)), nil
			}
		}
	}
	return 0, fmt.Errorf("image %s lists no snapshot for its root %s", image, root)
}

// hibUserData is the set's boot script with the image's root snapshot size filled in, for boot.sh's pre-read: on a
// machine launched able to hibernate the image has grown the root before boot.sh runs (disk()).
func hibUserData(script string, gib int64) (string, error) {
	const empty = "IMAGE_ROOT_BYTES=''"
	if strings.Count(script, empty) != 1 {
		return "", errors.New("the boot script has no IMAGE_ROOT_BYTES line to fill in")
	}
	return strings.Replace(script, empty, fmt.Sprintf("IMAGE_ROOT_BYTES='%d'", gib<<30), 1), nil
}

// hibTemplateName is the hibernated pool's template of one type.
func hibTemplateName(deployment, set, typ string) string {
	return templateName(deployment, set) + "-hib-" + typ
}

// ensureHibTemplates makes the hibernated pool's template of each of the set's types the region offers and EC2 can
// hibernate, its root sized to the type's memory.
func (sr *setRegion) ensureHibTemplates(ctx context.Context, api templateAPI, types typesAPI, images imageAPI, r *Region, deployment, userData, group string, spec SetSpec, tags []ec2types.Tag) error {
	var offered []string
	for _, t := range spec.Types {
		if len(sr.poolsOf([]string{t})) > 0 {
			offered = append(offered, t)
		}
	}
	if len(offered) == 0 {
		return errors.New("no type offered")
	}
	out, err := types.DescribeInstanceTypes(ctx, &ec2.DescribeInstanceTypesInput{InstanceTypes: toTypes(offered)})
	if err != nil {
		return err
	}
	gib, err := imageRootGiB(ctx, images, r.image, r.root)
	if err != nil {
		return err
	}
	if userData, err = hibUserData(userData, gib); err != nil {
		return err
	}
	sr.hib = map[string]hibTemplate{}
	for _, it := range out.InstanceTypes {
		t := string(it.InstanceType)
		if !aws.ToBool(it.HibernationSupported) || it.MemoryInfo == nil {
			slog.Warn("type cannot hibernate: the hibernated pool launches none of it", "set", spec.Name, "region", r.Name, "type", t)
			continue
		}
		gb := hibRootGB(aws.ToInt64(it.MemoryInfo.SizeInMiB), spec.DiskGB)
		ht := &setRegion{}
		if err := ht.ensureData(ctx, api, hibTemplateName(deployment, spec.Name, t), hibTemplateData(r, userData, group, gb, spec, tags), tags); err != nil {
			return fmt.Errorf("%s: %w", t, err)
		}
		sr.hib[t] = hibTemplate{template: ht.template, version: ht.version, rootGB: gb}
	}
	if len(sr.hib) == 0 {
		return errors.New("no type of the size can hibernate")
	}
	return nil
}

// LaunchBack starts n machines for the hibernated pool, on demand: EC2 stops no one-time spot machine, and Fleet
// instant spot is one-time. Each type is asked in the size's order (one type an ask, as Launch's on-demand asks are),
// across every zone of a region, each region its weighted part and then the others its shortfall. A zone with no
// capacity does not stall it: askBack asks again without the pools EC2 named.
func (sf *SetFleet) LaunchBack(ctx context.Context, round string, n int) []*Machine {
	var got []*Machine
	for _, t := range sf.spec.Types {
		if left := n - len(got); left > 0 {
			got = append(got, sf.backAcross(ctx, round, "hib-"+t, left, t)...)
		}
	}
	if left := n - len(got); left > 0 {
		slog.Warn("hibernated launch short", "set", sf.spec.Name, "round", round, "asked", n, "short", left)
	}
	return got
}

// backAcross asks each region for its weighted part of n machines of one type, then the other regions for a
// region's shortfall.
func (sf *SetFleet) backAcross(ctx context.Context, round, step string, n int, typ string) []*Machine {
	var got []*Machine
	want := sf.fleet.split(n)
	short := map[string]int{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, r := range sf.fleet.regions {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m := sf.askBack(ctx, r, want[r.Name], round, step, typ)
			mu.Lock()
			got = append(got, m...)
			short[r.Name] = want[r.Name] - len(m)
			mu.Unlock()
		}()
	}
	wg.Wait()
	for _, r := range sf.fleet.regions {
		for _, other := range sf.fleet.regions {
			if other != r && short[r.Name] > 0 {
				m := sf.askBack(ctx, other, short[r.Name], round, step+"-for-"+r.Name, typ)
				got = append(got, m...)
				short[r.Name] -= len(m)
			}
		}
	}
	return got
}

// askBack asks one region for n machines of one type for the hibernated pool, every zone's pool in one CreateFleet.
// What EC2 could not start in a pool it names (InsufficientInstanceCapacity in a zone) is asked again of the pools left,
// until n started, no pool is left, or EC2 named none: the lab saw bulk launches refused in two zones while others
// had room.
func (sf *SetFleet) askBack(ctx context.Context, r *Region, n int, round, step, typ string) []*Machine {
	sr := sf.regions[r.Name]
	if r.refused != "" || n <= 0 || sr == nil {
		return nil
	}
	ht, ok := sr.hib[typ]
	if !ok {
		return nil
	}
	pools := sr.poolsOf([]string{typ})
	var got []*Machine
	for try := 0; n > len(got) && len(pools) > 0; try++ {
		k := min(n-len(got), chunk)
		token := clientToken(sf.spec.Name, round, step+"-"+strconv.Itoa(try), r.Name, 0)
		started := time.Now()
		ms, errs, failed, err := sf.createFleet(ctx, r, k, ec2types.DefaultTargetCapacityTypeOnDemand, token, round, pools, &ht)
		slog.Info("fleet", "set", sf.spec.Name, "region", r.Name, "step", step, "round", round, "part", try, "asked", k, "got", len(ms),
			"ms", time.Since(started).Milliseconds(), "errors", errs, "err", errString(err), "token", token, "hibernated", true)
		tlLaunched(sf.spec.Name, round, step, started, ms)
		for _, m := range ms {
			m.Hib, m.RootGB = true, ht.rootGB
		}
		got = append(got, ms...)
		left := slices.DeleteFunc(slices.Clone(pools), func(p ec2types.FleetLaunchTemplateOverridesRequest) bool {
			return slices.Contains(failed, aws.ToString(p.SubnetId))
		})
		if err != nil || len(left) == len(pools) {
			break
		}
		pools = left
	}
	return got
}

// hibTags is the instance tags of a hibernated pool's machine: the set's, its round's, and its root's size.
func hibTags(t *hibTemplate) []ec2types.Tag {
	if t == nil {
		return nil
	}
	return []ec2types.Tag{{Key: aws.String(tagHib), Value: aws.String(strconv.Itoa(t.rootGB))}}
}

// clientToken is a CreateFleet call's ClientToken, unique to this set, round, step, region and part.
func clientToken(set, round, step, region string, part int) string {
	return tokenOf(fmt.Sprintf("%s/%s/%s/%s/%d", set, round, step, region, part))
}

// Start resumes hibernated machines of one region, and Hibernate hibernates running ones: powerCall's pacing for each.
// Each returns the machines EC2 refused, with why.
func (f *Fleet) Start(ctx context.Context, region string, ids []string) map[string]error {
	return f.powerCall(ctx, region, ids, func(ctx context.Context, r *Region, batch []string) error {
		_, err := r.power.StartInstances(ctx, &ec2.StartInstancesInput{InstanceIds: batch})
		return err
	})
}

func (f *Fleet) Hibernate(ctx context.Context, region string, ids []string) map[string]error {
	return f.powerCall(ctx, region, ids, func(ctx context.Context, r *Region, batch []string) error {
		_, err := r.power.StopInstances(ctx, &ec2.StopInstancesInput{InstanceIds: batch, Hibernate: aws.Bool(true)})
		return err
	})
}

// powerCall makes call for ids, at most powerBatch a call. A throttle (RequestLimitExceeded) or EC2's own fault is
// asked again after a jittered wait that doubles from f.powerWait (250 ms) up to 4 s, until ctx ends: of 100 starts
// fired at once in the lab 55 were throttled, and every one went through on its retries, so a throttle never drops a
// machine. A batch refused for any other reason is asked again one machine a call, so a machine in a wrong state fails
// only itself. It returns each machine that did not go through, with why.
func (f *Fleet) powerCall(ctx context.Context, region string, ids []string, call func(context.Context, *Region, []string) error) map[string]error {
	failed := map[string]error{}
	r := f.Region(region)
	if r == nil || r.power == nil {
		for _, id := range ids {
			failed[id] = fmt.Errorf("no region %q", region)
		}
		return failed
	}
	base := f.powerWait
	if base <= 0 {
		base = 250 * time.Millisecond
	}
	try := func(batch []string) error {
		for wait := base; ; wait = min(2*wait, 4*time.Second) {
			err := call(ctx, r, batch)
			if err == nil || !retryable(err) {
				return err
			}
			select {
			case <-ctx.Done():
				return fmt.Errorf("%w (last: %v)", ctx.Err(), err)
			case <-time.After(wait/2 + rand.N(wait/2+1)):
			}
		}
	}
	for i := 0; i < len(ids); i += powerBatch {
		batch := ids[i:min(len(ids), i+powerBatch)]
		err := try(batch)
		if err != nil && len(batch) > 1 && ctx.Err() == nil {
			for _, id := range batch {
				if err := try([]string{id}); err != nil {
					failed[id] = err
				}
			}
			continue
		}
		if err != nil {
			for _, id := range batch {
				failed[id] = err
			}
		}
	}
	return failed
}

// errCode is an EC2 error's code, or "" when it is not one.
func errCode(err error) string {
	var ae smithy.APIError
	if errors.As(err, &ae) {
		return ae.ErrorCode()
	}
	return ""
}

// backLive is the hibernated pool's machines, booting to hibernating to hibernated, and the capacity asked for it.
func (st *DispatchSet) backLive() int {
	n := 0
	for _, k := range st.backReserved {
		n += k
	}
	for _, m := range st.machines {
		if m.back && m.phase != mEnding {
			n++
		}
	}
	return n
}

// backTarget is the hibernated pool the set keeps: none past the hard stop, so the bill stops growing.
func (st *DispatchSet) backTarget() int {
	if st.stopped {
		return 0
	}
	return st.hibernated
}

// BackDeficit reserves the machines to launch for the hibernated pool now. At the spend cap it launches none; the
// machines it has are kept, and resumed for the jobs superci already holds.
func (st *DispatchSet) BackDeficit(now time.Time) (round string, n int) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.hibernated == 0 || st.capped || st.stopped || !st.ready() {
		return "", 0
	}
	if n = st.backTarget() - st.backLive(); n <= 0 {
		return "", 0
	}
	st.rounds++
	round = fmt.Sprintf("%s-hib-%d", st.boot, st.rounds)
	st.backReserved[round] = n
	return round, n
}

// LaunchedBack ends a hibernated round's reservation: the machines EC2 started join the set as the pool's.
func (st *DispatchSet) LaunchedBack(round string, got []*Machine, now time.Time) {
	st.mu.Lock()
	defer st.mu.Unlock()
	delete(st.backReserved, round)
	for _, m := range got {
		if d, ok := st.machines[m.ID]; ok {
			d.Hib, d.RootGB = true, m.RootGB
			continue
		}
		m.Ordered, m.Dispatch = now, true
		st.machines[m.ID] = &dmachine{Machine: m, since: now, back: true}
	}
}

// park takes a pool machine that came to /work for its hibernation (waiting's caller holds the lock): it is never
// in the idle queue, and its connection is left to break when it sleeps.
func (st *DispatchSet) park(m *dmachine, mw *wire, now time.Time) {
	m.wire, m.phase, m.since = mw, mHibernating, now
	st.toPark = append(st.toPark, m)
}

// wanted says whether a job waits that a machine could take now.
func (st *DispatchSet) wanted(now time.Time) bool {
	return slices.ContainsFunc(st.queue, func(s *session) bool { return !now.Before(s.after) })
}

// parked says whether a machine sleeps or is going to: a /work from it is answered 503 (Dispatcher.work).
func (st *DispatchSet) parked(m *dmachine) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return m.phase == mHibernating || m.phase == mHibernated
}

// TakeParks hands over the machines to hibernate now: those not waiting out a not-ready refusal (Parked).
func (st *DispatchSet) TakeParks(now time.Time) []*Machine {
	st.mu.Lock()
	defer st.mu.Unlock()
	var out []*Machine
	var later []*dmachine
	for _, m := range st.toPark {
		switch {
		case m.phase != mHibernating:
		case now.Before(m.parkAfter):
			later = append(later, m)
		default:
			out = append(out, m.Machine)
		}
	}
	st.toPark = later
	return out
}

// notReady says whether EC2 refused a hibernation because the machine was launched too recently.
func notReady(err error) bool {
	return errCode(err) == "UnsupportedOperation" && strings.Contains(err.Error(), "not ready to hibernate")
}

// Parked takes in EC2's answers to the hibernations asked at now. A machine EC2 took is timed from now (parkLate). One
// refused as not ready yet is asked again later until hibReady after its launch; any other refusal, or that one past
// hibReady, ends it.
func (st *DispatchSet) Parked(now time.Time, asked []*Machine, refused map[string]error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, a := range asked {
		m := st.machines[a.ID]
		if m == nil || m.phase != mHibernating {
			continue
		}
		err, no := refused[a.ID]
		switch {
		case !no:
			m.parkedAt = now
			st.counts.Parked++
		case notReady(err) && now.Sub(m.Launched) < hibReady:
			m.parkAfter = now.Add(min(parkRetry<<m.parkTries, time.Minute))
			m.parkTries++
			st.toPark = append(st.toPark, m)
		default:
			st.counts.ParkFailed++
			st.end(m, "its hibernation was refused: "+errCode(err))
		}
	}
}

// Draw draws hibernated machines for the running pool's deficit, oldest first, before Deficit launches cold for the
// rest: each is resuming and counts as supply. It first ends the resumes and hibernations past their time (expire).
// The caller starts the machines it returns (resumeAll).
func (st *DispatchSet) Draw(now time.Time) []*Machine {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.expire(now)
	if st.stopped || !st.ready() {
		return nil
	}
	n := st.desired() - st.live()
	// A resume that failed or was late is owed a cold launch, not another resume: a fault that keeps resumes from
	// arriving costs each job one resumeLate at most, never one per hibernated machine.
	st.owed = min(st.owed, max(0, n))
	if n -= st.owed; n <= 0 {
		return nil
	}
	var sleeping []*dmachine
	for _, m := range st.machines {
		if m.back && m.phase == mHibernated {
			sleeping = append(sleeping, m)
		}
	}
	slices.SortFunc(sleeping, func(a, b *dmachine) int { return a.Launched.Compare(b.Launched) })
	var out []*Machine
	for _, m := range sleeping[:min(n, len(sleeping))] {
		m.back, m.phase, m.since, m.resumed = false, mResuming, now, now
		out = append(out, m.Machine)
	}
	return out
}

// Plan is one launcher tick's decision for the running pool: the hibernated machines to resume (Draw), then the cold
// launch for what they leave (Deficit).
func (st *DispatchSet) Plan(now time.Time) (resume []*Machine, round string, n int) {
	resume = st.Draw(now)
	round, n = st.Deficit(now)
	st.mu.Lock()
	st.owed = max(0, st.owed-n)
	st.mu.Unlock()
	return resume, round, n
}

// ResumeRefused ends each resumed machine EC2 refused to start: it stops counting, and the next Deficit launches cold
// for it.
func (st *DispatchSet) ResumeRefused(refused map[string]error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	for id, err := range refused {
		if m := st.machines[id]; m != nil && m.phase == mResuming {
			st.counts.ResumeFailed++
			st.owed++
			st.end(m, "its resume was refused: "+err.Error())
		}
	}
	if len(refused) > 0 {
		select {
		case st.wake <- struct{}{}:
		default:
		}
	}
}

// expire ends a resumed machine that has not asked /work within resumeLate, and a machine EC2 has not hibernated
// within parkLate. The caller holds the lock.
func (st *DispatchSet) expire(now time.Time) {
	for _, m := range st.machines {
		switch {
		case m.phase == mResuming && now.Sub(m.resumed) > resumeLate:
			st.counts.ResumeLate++
			st.owed++
			st.end(m, "its resume did not ask for work in time")
		case m.phase == mHibernating && !m.parkedAt.IsZero() && now.Sub(m.parkedAt) > parkLate,
			m.phase == mHibernating && m.parkedAt.IsZero() && now.Sub(m.Launched) > hibReady+parkLate:
			st.counts.ParkFailed++
			st.end(m, "it did not hibernate in time")
		}
	}
}

// listedParked is what a listing of a pool machine says: stopped once its hibernation is done. One running that sleeps
// in this process was started by someone else, and one leaving running for good ends.
func (st *DispatchSet) listedParked(m *dmachine, state string) {
	switch {
	case m.phase == mHibernating && state == string(ec2types.InstanceStateNameStopped):
		m.phase = mHibernated
	case m.phase == mHibernated && (state == string(ec2types.InstanceStateNameRunning) || state == string(ec2types.InstanceStateNamePending)):
		st.end(m, "it runs, and was not resumed by this listener")
	case m.back && (state == string(ec2types.InstanceStateNameShuttingDown) || state == string(ec2types.InstanceStateNameTerminated)):
		st.end(m, "EC2 lists it "+state)
	}
}

// adoptParked is a pool machine a listing found that this process did not launch, as after a restart: one stopped or
// stopping sleeps (a running one is any machine of the set, which comes to /work).
func adoptParked(l *Machine, now time.Time) *dmachine {
	m := &dmachine{Machine: l, since: now}
	switch l.State {
	case string(ec2types.InstanceStateNameStopped):
		m.back, m.phase = true, mHibernated
	case string(ec2types.InstanceStateNameStopping):
		m.back, m.phase = true, mHibernating
	}
	return m
}

// backSurplus ends the newest pool machines past the pool's target. A set that keeps no pool (hibernated 0, or past the
// hard stop) ends every one a listing shows, asleep or not, so turning a pool off strands none.
func (st *DispatchSet) backSurplus() {
	excess := st.backLive() - st.backTarget()
	if excess <= 0 {
		return
	}
	var back []*dmachine
	for _, m := range st.machines {
		if m.back && m.phase != mEnding {
			back = append(back, m)
		}
	}
	slices.SortFunc(back, func(a, b *dmachine) int { return b.Launched.Compare(a.Launched) })
	why := "the hibernated pool's surplus"
	if st.backTarget() == 0 {
		why = "its set keeps no hibernated pool"
	}
	for _, m := range back[:min(len(back), excess)] {
		st.end(m, why)
	}
}

// TakeResumeWaits hands over the seconds from each resume to its machine's /work since the last call.
func (st *DispatchSet) TakeResumeWaits() []float64 {
	st.mu.Lock()
	defer st.mu.Unlock()
	w := st.resumeWaits
	st.resumeWaits = nil
	return w
}

// parkedMachines is the pool machines of a listing that are leaving running for their sleep, or asleep: a dispatch
// set lists them with its running ones (DispatchSet.Seen).
func parkedMachines(leaving []*Machine) []*Machine {
	var out []*Machine
	for _, m := range leaving {
		if m.Hib && (m.State == string(ec2types.InstanceStateNameStopping) || m.State == string(ec2types.InstanceStateNameStopped)) {
			out = append(out, m)
		}
	}
	return out
}

// untaggedStopped is the stopped machines of a listing that carry no superci-pull-hib tag: no hibernated pool stopped
// them, so the listener names them in its log and leaves them alone.
func untaggedStopped(leaving []*Machine) []string {
	var out []string
	for _, m := range leaving {
		if !m.Hib && m.State == string(ec2types.InstanceStateNameStopped) {
			out = append(out, m.ID)
		}
	}
	return out
}

// resumeAll starts the machines Draw drew, and takes in EC2's refusals; a call still throttled at resumeLate gives
// up, and its machines are ended as late.
func (st *DispatchSet) resumeAll(ctx context.Context, f *Fleet, ms []*Machine) {
	by := map[string][]string{}
	for _, m := range ms {
		by[m.Region] = append(by[m.Region], m.ID)
	}
	ctx, cancel := context.WithTimeout(ctx, resumeLate)
	defer cancel()
	var wg sync.WaitGroup
	for region, ids := range by {
		wg.Add(1)
		go func() {
			defer wg.Done()
			started := time.Now()
			refused := f.Start(ctx, region, ids)
			slog.Info("resume", "set", st.spec.Name, "region", region, "asked", len(ids), "refused", len(refused), "ms", time.Since(started).Milliseconds())
			st.ResumeRefused(refused)
		}()
	}
	wg.Wait()
}

// parkAll hibernates the machines that came to /work for the pool.
func (st *DispatchSet) parkAll(ctx context.Context, f *Fleet, now time.Time) {
	ms := st.TakeParks(now)
	by := map[string][]string{}
	for _, m := range ms {
		by[m.Region] = append(by[m.Region], m.ID)
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	refused := map[string]error{}
	for region, ids := range by {
		for id, err := range f.Hibernate(ctx, region, ids) {
			refused[id] = err
		}
		slog.Info("hibernate", "set", st.spec.Name, "region", region, "asked", len(ids), "refused", len(refused))
	}
	st.Parked(now, ms, refused)
}

// refill launches the hibernated pool's deficit.
func (st *DispatchSet) refill(ctx context.Context) {
	round, n := st.BackDeficit(time.Now())
	if n == 0 {
		return
	}
	got := st.fleet.LaunchBack(ctx, round, n)
	st.LaunchedBack(round, got, time.Now())
	slog.Info("launched hibernated", "set", st.spec.Name, "round", round, "asked", n, "got", len(got))
}
