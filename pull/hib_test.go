package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"
)

func apiErr(code string) error { return &smithy.GenericAPIError{Code: code, Message: code} }

// power stands in for one region's StartInstances and StopInstances. Calls draw from a bucket of burst tokens that
// refills at rate a second, and one past it is throttled (RequestLimitExceeded), as the lab's 100 calls at once were:
// about 32 taken, then a few a second. A call naming a machine in refuse fails whole with its code, as EC2's does; with
// hang set, StartInstances answers only when its context ends.
type power struct {
	mu          sync.Mutex
	burst, rate float64
	tokens      float64
	last        time.Time
	throttled   int
	refuse      map[string]string
	hang        bool
	notReady    int // StopInstances calls still to refuse as EC2 does for a machine launched minutes ago
	started     map[string]int
	stopped     map[string]int
	hibernate   []bool // each StopInstances call's Hibernate
}

func newPower(burst, rate float64) *power {
	return &power{burst: burst, rate: rate, tokens: burst, last: time.Now(), refuse: map[string]string{}, started: map[string]int{}, stopped: map[string]int{}}
}

func (p *power) take(ids []string) error {
	now := time.Now()
	p.tokens = min(p.burst, p.tokens+now.Sub(p.last).Seconds()*p.rate)
	p.last = now
	if p.tokens < 1 {
		p.throttled++
		return apiErr("RequestLimitExceeded")
	}
	p.tokens--
	for _, id := range ids {
		if code, ok := p.refuse[id]; ok {
			return apiErr(code)
		}
	}
	return nil
}

func (p *power) StartInstances(ctx context.Context, in *ec2.StartInstancesInput, _ ...func(*ec2.Options)) (*ec2.StartInstancesOutput, error) {
	if p.hang {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.take(in.InstanceIds); err != nil {
		return nil, err
	}
	for _, id := range in.InstanceIds {
		p.started[id]++
	}
	return &ec2.StartInstancesOutput{}, nil
}

func (p *power) StopInstances(_ context.Context, in *ec2.StopInstancesInput, _ ...func(*ec2.Options)) (*ec2.StopInstancesOutput, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.notReady > 0 {
		p.notReady--
		return nil, &smithy.GenericAPIError{Code: "UnsupportedOperation", Message: "Instance " + in.InstanceIds[0] + " is not ready to hibernate yet, retry in a few minutes"}
	}
	if err := p.take(in.InstanceIds); err != nil {
		return nil, err
	}
	p.hibernate = append(p.hibernate, aws.ToBool(in.Hibernate))
	for _, id := range in.InstanceIds {
		p.stopped[id]++
	}
	return &ec2.StopInstancesOutput{}, nil
}

func powerFleet(p *power) *Fleet {
	return &Fleet{regions: []*Region{{Name: "us-east-1", Weight: 1, power: p}}, powerWait: 2 * time.Millisecond}
}

// Condition 1: resumes fired at once are throttled, and every one is asked again until EC2 takes it: none is dropped.
func TestThrottledResumesAreAskedAgainUntilEveryMachineStarts(t *testing.T) {
	p := newPower(4, 200)
	f := powerFleet(p)
	var wg sync.WaitGroup
	var mu sync.Mutex
	refused := map[string]error{}
	ask := func(ids []string) {
		defer wg.Done()
		r := f.Start(context.Background(), "us-east-1", ids)
		mu.Lock()
		for id, err := range r {
			refused[id] = err
		}
		mu.Unlock()
	}
	var batch []string
	for i := range 100 { // 100 resumes at once, a call each, and one call naming 100 more
		wg.Add(1)
		go ask([]string{fmt.Sprintf("i-%03d", i)})
		batch = append(batch, fmt.Sprintf("i-%03d", 100+i))
	}
	wg.Add(1)
	go ask(batch)
	wg.Wait()
	if p.throttled == 0 {
		t.Fatal("no call was throttled: the fake did not test the pacing")
	}
	if len(refused) != 0 {
		t.Fatalf("%d resumes dropped (e.g. %v), want none: a throttle is asked again", len(refused), first(refused))
	}
	for i := range 200 {
		if n := p.started[fmt.Sprintf("i-%03d", i)]; n != 1 {
			t.Fatalf("i-%03d started %d times, want once (%d throttled calls in all)", i, n, p.throttled)
		}
	}
}

func first(m map[string]error) string {
	for k, v := range m {
		return k + ": " + v.Error()
	}
	return ""
}

// A batch EC2 refuses for one machine's state is asked again one machine a call: only that machine fails.
func TestAResumeBatchWithOneMachineInAWrongStateFailsOnlyThatMachine(t *testing.T) {
	p := newPower(100, 100)
	p.refuse["i-bad"] = "IncorrectInstanceState"
	refused := powerFleet(p).Start(context.Background(), "us-east-1", []string{"i-1", "i-bad", "i-2"})
	if len(refused) != 1 || errCode(refused["i-bad"]) != "IncorrectInstanceState" {
		t.Fatalf("refused %v, want i-bad alone", refused)
	}
	if p.started["i-1"] != 1 || p.started["i-2"] != 1 {
		t.Fatalf("started %v, want i-1 and i-2", p.started)
	}
}

// zones stands in for one region's on-demand capacity by (type, subnet). A pool whose zone has none refuses the rest
// of the call at it, naming its pool, as the lab's bulk launches in two zones were refused whole.
type zones struct {
	mu    sync.Mutex
	left  map[string]int  // "<type>/<subnet>"
	ice   map[string]bool // subnets with no capacity
	calls []*ec2.CreateFleetInput
	n     int
}

func (z *zones) CreateFleet(_ context.Context, in *ec2.CreateFleetInput, _ ...func(*ec2.Options)) (*ec2.CreateFleetOutput, error) {
	z.mu.Lock()
	defer z.mu.Unlock()
	z.calls = append(z.calls, in)
	want := int(aws.ToInt32(in.TargetCapacitySpecification.TotalTargetCapacity))
	out := &ec2.CreateFleetOutput{}
	for _, p := range in.LaunchTemplateConfigs[0].Overrides {
		subnet := aws.ToString(p.SubnetId)
		if z.ice[subnet] {
			out.Errors = append(out.Errors, ec2types.CreateFleetError{ErrorCode: aws.String("InsufficientInstanceCapacity"),
				LaunchTemplateAndOverrides: &ec2types.LaunchTemplateAndOverridesResponse{Overrides: &ec2types.FleetLaunchTemplateOverrides{
					InstanceType: p.InstanceType, SubnetId: p.SubnetId}}})
			return out, nil
		}
		key := string(p.InstanceType) + "/" + subnet
		k := min(want, z.left[key])
		z.left[key] -= k
		want -= k
		var ids []string
		for range k {
			z.n++
			ids = append(ids, fmt.Sprintf("i-%s-%04d", strings.TrimPrefix(subnet, "subnet-"), z.n))
		}
		if k > 0 {
			out.Instances = append(out.Instances, ec2types.CreateFleetInstance{InstanceType: p.InstanceType, InstanceIds: ids,
				LaunchTemplateAndOverrides: &ec2types.LaunchTemplateAndOverridesResponse{Overrides: &ec2types.FleetLaunchTemplateOverrides{AvailabilityZone: aws.String(strings.TrimPrefix(subnet, "subnet-"))}}})
		}
	}
	if want > 0 {
		out.Errors = append(out.Errors, ec2types.CreateFleetError{ErrorCode: aws.String("InsufficientInstanceCapacity")})
	}
	return out, nil
}

// hibFleet is superci-8cpu with a hibernated pool in one region whose three zones offer both its types, each type's
// hibernated template made.
func hibFleet(t *testing.T, z *zones) *SetFleet {
	t.Helper()
	c, err := LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	spec := c.Sets()[slices.IndexFunc(c.Sets(), func(s SetSpec) bool { return s.Name == "superci-8cpu" })]
	spec.Dispatch, spec.Hibernated = true, 120
	sf := &SetFleet{fleet: &Fleet{regions: []*Region{{Name: "us-east-1", Weight: 1, fleets: z}}}, spec: spec, regions: map[string]*setRegion{}}
	subnets := map[string]string{}
	var offers []ec2types.InstanceTypeOffering
	for _, az := range []string{"us-east-1a", "us-east-1b", "us-east-1c"} {
		subnets[az] = "subnet-" + az
		for _, typ := range spec.Types {
			offers = append(offers, ec2types.InstanceTypeOffering{InstanceType: ec2types.InstanceType(typ), Location: aws.String(az)})
		}
	}
	sf.regions["us-east-1"] = &setRegion{template: "lt", version: "7", pools: rankedPools(offers, subnets, spec.Types),
		hib: map[string]hibTemplate{"m8azn.3xlarge": {template: "lt-hib-m8azn", version: "3", rootGB: 128}, "m8a.2xlarge": {template: "lt-hib-m8a", version: "2", rootGB: 112}}}
	return sf
}

// Condition 2: the hibernated pool is refilled by CreateFleet on demand over every zone, the size's types in order, and
// a zone with no capacity does not stall it: what that zone refused is asked again of the others.
func TestARefillWithNoCapacityInOneZoneFillsFromTheOthers(t *testing.T) {
	z := &zones{left: map[string]int{"m8azn.3xlarge/subnet-us-east-1a": 100, "m8azn.3xlarge/subnet-us-east-1b": 30, "m8azn.3xlarge/subnet-us-east-1c": 30,
		"m8a.2xlarge/subnet-us-east-1b": 100}, ice: map[string]bool{"subnet-us-east-1a": true}}
	sf := hibFleet(t, z)
	got := sf.LaunchBack(context.Background(), "r-hib-1", 70)
	if len(got) != 70 {
		t.Fatalf("started %d of 70 with us-east-1a out of capacity, want 70 from the other zones and types", len(got))
	}
	byType := map[string]int{}
	for _, m := range got {
		if m.Lifecycle != "on-demand" || !m.Hib || strings.Contains(m.ID, "us-east-1a") {
			t.Fatalf("machine %+v: want on demand, of the hibernated pool, not from the zone with no capacity", m)
		}
		byType[m.Type]++
	}
	if byType["m8azn.3xlarge"] != 60 || byType["m8a.2xlarge"] != 10 {
		t.Fatalf("by type %v, want the first type's 60 in b and c, then 10 of the second", byType)
	}
	firstCall := z.calls[0]
	var zonesAsked []string
	for _, p := range firstCall.LaunchTemplateConfigs[0].Overrides {
		zonesAsked = append(zonesAsked, aws.ToString(p.SubnetId))
	}
	sort.Strings(zonesAsked)
	if want := []string{"subnet-us-east-1a", "subnet-us-east-1b", "subnet-us-east-1c"}; !slices.Equal(zonesAsked, want) {
		t.Fatalf("the first ask named %v, want every zone %v", zonesAsked, want)
	}
	spec := firstCall.LaunchTemplateConfigs[0].LaunchTemplateSpecification
	if firstCall.TargetCapacitySpecification.DefaultTargetCapacityType != ec2types.DefaultTargetCapacityTypeOnDemand ||
		aws.ToString(spec.LaunchTemplateId) != "lt-hib-m8azn" || aws.ToString(spec.Version) != "3" {
		t.Fatalf("first ask %v of %s v%s, want on demand from the hibernated template", firstCall.TargetCapacitySpecification.DefaultTargetCapacityType,
			aws.ToString(spec.LaunchTemplateId), aws.ToString(spec.Version))
	}
	if tag := tagValue(firstCall.TagSpecifications[0].Tags, tagHib); tag != "128" {
		t.Fatalf("instance tag %s=%q, want the root's 128 GiB", tagHib, tag)
	}
	if len(z.calls) < 2 || slices.ContainsFunc(z.calls[1].LaunchTemplateConfigs[0].Overrides, func(p ec2types.FleetLaunchTemplateOverridesRequest) bool {
		return aws.ToString(p.SubnetId) == "subnet-us-east-1a"
	}) {
		t.Fatal("the ask after us-east-1a's refusal named it again")
	}
	if aws.ToString(z.calls[0].ClientToken) == aws.ToString(z.calls[1].ClientToken) {
		t.Fatal("the second ask sent the first's ClientToken with other pools: EC2 answers IdempotentParameterMismatch")
	}
}

func tagValue(tags []ec2types.Tag, key string) string {
	for _, tg := range tags {
		if aws.ToString(tg.Key) == key {
			return aws.ToString(tg.Value)
		}
	}
	return ""
}

// recordingTemplates keeps each launch template's data as it was asked for.
type recordingTemplates struct {
	*templates
	data map[string]*ec2types.RequestLaunchTemplateData
}

func (r *recordingTemplates) CreateLaunchTemplate(ctx context.Context, in *ec2.CreateLaunchTemplateInput, o ...func(*ec2.Options)) (*ec2.CreateLaunchTemplateOutput, error) {
	r.data[aws.ToString(in.LaunchTemplateName)] = in.LaunchTemplateData
	return r.templates.CreateLaunchTemplate(ctx, in, o...)
}

// instanceTypes stands in for DescribeInstanceTypes: each type's memory, and whether it hibernates.
type instanceTypes map[string]struct {
	mib int64
	hib bool
}

func (it instanceTypes) DescribeInstanceTypes(_ context.Context, in *ec2.DescribeInstanceTypesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstanceTypesOutput, error) {
	out := &ec2.DescribeInstanceTypesOutput{}
	for _, t := range in.InstanceTypes {
		if v, ok := it[string(t)]; ok {
			out.InstanceTypes = append(out.InstanceTypes, ec2types.InstanceTypeInfo{InstanceType: t, HibernationSupported: aws.Bool(v.hib),
				MemoryInfo: &ec2types.MemoryInfo{SizeInMiB: aws.Int64(v.mib)}})
		}
	}
	return out, nil
}

// Condition 3: only the hibernated pool's machines get the 1,000 MiB/s root, encrypted, of the type's memory plus
// disk_gb, launched able to hibernate; the set's own template is what it was.
func TestOnlyTheHibernatedPoolsTemplatesHaveTheEncryptedFastRootAndHibernation(t *testing.T) {
	sf := hibFleet(t, &zones{})
	sr := &setRegion{pools: sf.regions["us-east-1"].pools}
	spec := sf.spec
	spec.Types = append(slices.Clone(spec.Types), "c5.2xlarge")
	sr.pools = append(sr.pools, ec2types.FleetLaunchTemplateOverridesRequest{InstanceType: "c5.2xlarge", SubnetId: aws.String("subnet-us-east-1a")})
	api := &recordingTemplates{templates: &templates{byName: map[string]*fakeTemplate{}}, data: map[string]*ec2types.RequestLaunchTemplateData{}}
	types := instanceTypes{"m8azn.3xlarge": {49152, true}, "m8a.2xlarge": {32768, true}, "c5.2xlarge": {16384, false}}
	r := &Region{Name: "us-east-1", image: "ami-pinned", root: "/dev/sda1"}
	tags := []ec2types.Tag{{Key: aws.String("purpose"), Value: aws.String("superci-pull")}}
	if err := sr.ensureHibTemplates(context.Background(), api, types, rootImage(30), r, "superci-pull", testBoot, "sg-1", spec, tags); err != nil {
		t.Fatal(err)
	}
	ordinary := templateData(r, testBoot, "sg-1", spec.DiskGB, spec, tags)
	for typ, gb := range map[string]int32{"m8azn.3xlarge": 48 + 80, "m8a.2xlarge": 32 + 80} {
		name := "superci-pull-superci-8cpu-hib-" + typ
		d := api.data[name]
		if d == nil {
			t.Fatalf("no template %s", name)
		}
		e := d.BlockDeviceMappings[0].Ebs
		if aws.ToInt32(e.VolumeSize) != gb || aws.ToInt32(e.Throughput) != 1000 || aws.ToInt32(e.Iops) != 4000 || !aws.ToBool(e.Encrypted) ||
			e.VolumeType != ec2types.VolumeTypeGp3 || !aws.ToBool(e.DeleteOnTermination) || aws.ToString(d.BlockDeviceMappings[0].DeviceName) != "/dev/sda1" {
			t.Fatalf("%s root %d GiB %d MiB/s %d IOPS encrypted %v, want %d GiB (memory + disk_gb) at 1,000 MiB/s and 4,000 IOPS, encrypted",
				typ, aws.ToInt32(e.VolumeSize), aws.ToInt32(e.Throughput), aws.ToInt32(e.Iops), aws.ToBool(e.Encrypted), gb)
		}
		if d.HibernationOptions == nil || !aws.ToBool(d.HibernationOptions.Configured) {
			t.Fatalf("%s is not launched able to hibernate", typ)
		}
		hibScript, _ := userDataScript(aws.ToString(d.UserData))
		if want := strings.Replace(testBoot, "IMAGE_ROOT_BYTES=''", "IMAGE_ROOT_BYTES='32212254720'", 1); hibScript != want {
			t.Fatalf("%s boots %q, want the set's script with the image's root snapshot (30 GiB) for the pre-read", typ, hibScript)
		}
		if aws.ToString(d.ImageId) != aws.ToString(ordinary.ImageId) ||
			!slices.Equal(d.SecurityGroupIds, ordinary.SecurityGroupIds) || d.InstanceInitiatedShutdownBehavior != ordinary.InstanceInitiatedShutdownBehavior {
			t.Fatalf("%s boots other than the set's machines", typ)
		}
		if sr.hib[typ].rootGB != int(gb) {
			t.Fatalf("%s's template kept root %d, want %d", typ, sr.hib[typ].rootGB, gb)
		}
	}
	if _, ok := sr.hib["c5.2xlarge"]; ok || api.data["superci-pull-superci-8cpu-hib-c5.2xlarge"] != nil {
		t.Fatal("a type that cannot hibernate got a hibernated template")
	}
	oe := ordinary.BlockDeviceMappings[0].Ebs
	if ordinary.HibernationOptions != nil || oe.Encrypted != nil || aws.ToInt32(oe.VolumeSize) != 80 || aws.ToInt32(oe.Throughput) != 250 || aws.ToInt32(oe.Iops) != 3000 {
		t.Fatalf("the set's own template changed: %d GiB %d MiB/s %d IOPS encrypted %v hibernation %v", aws.ToInt32(oe.VolumeSize),
			aws.ToInt32(oe.Throughput), aws.ToInt32(oe.Iops), oe.Encrypted, ordinary.HibernationOptions)
	}
	// A start with the same data writes nothing; a version without the encryption or the hibernation is replaced.
	api.wrote = nil
	if err := sr.ensureHibTemplates(context.Background(), api, types, rootImage(30), r, "superci-pull", testBoot, "sg-1", spec, tags); err != nil {
		t.Fatal(err)
	}
	if len(api.wrote) != 0 {
		t.Fatalf("a start with the same data wrote %v", api.wrote)
	}
	tpl := api.byName["superci-pull-superci-8cpu-hib-m8a.2xlarge"]
	tpl.versions[0].BlockDeviceMappings[0].Ebs.Encrypted, tpl.versions[0].HibernationOptions = nil, nil
	if err := sr.ensureHibTemplates(context.Background(), api, types, rootImage(30), r, "superci-pull", testBoot, "sg-1", spec, tags); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(api.wrote, "version 2 of lt-superci-pull-superci-8cpu-hib-m8a.2xlarge") {
		t.Fatalf("wrote %v, want a new version of the template whose root was not encrypted", api.wrote)
	}
}

// hibDispatchSet is a dispatch set keeping hibernated machines, every region listed, holding the given machines
// asleep (EC2 lists them stopped).
func hibDispatchSet(hibernated int, asleep ...string) *DispatchSet {
	now := time.Now()
	st := NewDispatchSet(SetSpec{Name: "superci-4cpu", Label: "superci-4cpu", Max: 100, Dispatch: true, Hibernated: hibernated}, nil, []string{"us-east-1"}, now.Add(-time.Hour))
	var listed []*Machine
	for _, id := range asleep {
		listed = append(listed, &Machine{ID: id, Set: "superci-4cpu", Region: "us-east-1", Launched: now.Add(-10 * time.Minute), Dispatch: true,
			Hib: true, RootGB: 64, State: "stopped", Lifecycle: "on-demand"})
	}
	st.Seen("us-east-1", listed, now, now)
	return st
}

// coldServes launches the cold machine Plan asked for and has it ask /work: the waiting job is handed to it.
func coldServes(t *testing.T, p *dispatcher, round string, n int, shim *peer) {
	t.Helper()
	if n != 1 {
		t.Fatalf("Plan asked %d cold machines, want 1 for the waiting job", n)
	}
	p.st.Launched(round, []*Machine{{ID: "i-0cold", Set: "superci-4cpu", Region: "us-east-1", Launched: time.Now(), Dispatch: true, Lifecycle: "on-demand"}}, time.Now())
	m := p.machine(t, "i-0cold")
	if j := m.job(); j.Token == "" {
		t.Fatal("the cold machine got no job")
	}
	if inst, _ := shim.paired(); inst != "i-0cold" {
		t.Fatalf("the job was paired with %s, want the cold machine", inst)
	}
}

// waitingJob starts a dispatcher over st and a shim whose job waits for a machine; the hibernated machine is drawn.
func waitingJob(t *testing.T, st *DispatchSet, now time.Time) (*dispatcher, *peer, []*Machine) {
	t.Helper()
	p := startDispatcher(t, &fakeS3{objects: map[string]string{}}, identityTags(), st)
	shim := p.shim(t)
	shim.sendJob(jobMessage(jobA, 7, "unit"))
	waitUntil(t, "the job waits", func() bool { st.mu.Lock(); defer st.mu.Unlock(); return len(st.queue) == 1 })
	resume, _, n := st.Plan(now)
	if len(resume) != 1 || resume[0].ID != "i-0h1" || n != 0 {
		t.Fatalf("Plan resumed %v and asked %d cold, want the hibernated machine and no cold launch: a resume counts as supply", resume, n)
	}
	return p, shim, resume
}

// Condition 4a: a resume whose call never comes back is late at resumeLate, and the job is served by a cold launch.
func TestAResumeThatNeverComesBackFallsBackToAColdLaunch(t *testing.T) {
	now := time.Now()
	st := hibDispatchSet(1, "i-0h1")
	p, shim, resume := waitingJob(t, st, now)
	pw := newPower(10, 10)
	pw.hang = true
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { st.resumeAll(ctx, powerFleet(pw), resume); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	if _, _, n := st.Plan(now.Add(resumeLate - time.Second)); n != 0 {
		t.Fatalf("a cold launch of %d before the resume was late", n)
	}
	_, round, n := st.Plan(now.Add(resumeLate + time.Second))
	coldServes(t, p, round, n, shim)
	if c := st.TakeCounts(); c.ResumeLate != 1 || c.ResumeFailed != 0 {
		t.Fatalf("counts %+v, want one late resume", c)
	}
}

// Condition 4b: a resume EC2 refuses ends at once, and the job is served by a cold launch without waiting out the
// resume's time.
func TestAResumeThatFailsFallsBackToAColdLaunchAtOnce(t *testing.T) {
	now := time.Now()
	st := hibDispatchSet(1, "i-0h1")
	p, shim, resume := waitingJob(t, st, now)
	pw := newPower(10, 10)
	pw.refuse["i-0h1"] = "InsufficientInstanceCapacity"
	st.resumeAll(context.Background(), powerFleet(pw), resume)
	_, round, n := st.Plan(now)
	coldServes(t, p, round, n, shim)
	if c := st.TakeCounts(); c.ResumeFailed != 1 || c.ResumeLate != 0 {
		t.Fatalf("counts %+v, want one refused resume", c)
	}
}

// Condition 4c: a machine that resumes but never asks /work is late at resumeLate, the job is served by a cold launch,
// and the late machine's /work, if it ever comes, gets no job.
func TestAResumedMachineThatNeverAsksForWorkFallsBackToAColdLaunch(t *testing.T) {
	now := time.Now()
	st := hibDispatchSet(1, "i-0h1")
	p, shim, resume := waitingJob(t, st, now)
	pw := newPower(10, 10)
	st.resumeAll(context.Background(), powerFleet(pw), resume)
	if pw.started["i-0h1"] != 1 {
		t.Fatal("the hibernated machine was not started")
	}
	if _, _, n := st.Plan(now.Add(10 * time.Second)); n != 0 {
		t.Fatalf("a cold launch of %d while the resume still has time", n)
	}
	_, round, n := st.Plan(now.Add(resumeLate + time.Second))
	coldServes(t, p, round, n, shim)
	if late := p.machine(t, "i-0h1"); late.code != 410 {
		t.Fatalf("the late machine's /work got %d, want 410", late.code)
	}
	if c := st.TakeCounts(); c.ResumeLate != 1 {
		t.Fatalf("counts %+v, want one late resume", c)
	}
}

// The lifecycle: a machine launched for the hibernated pool is no supply of the running pool; at /work with no job
// waiting it is put to hibernate, never offered a job, and a /work from it meanwhile is answered 503; once EC2 lists it
// stopped it sleeps; a hand-off resumes it before any cold launch, and its /work after the resume puts it in the idle
// queue. The pool is refilled for it.
func TestAHibernatedPoolMachineSleepsUntilAHandOffResumesItIntoTheIdleQueue(t *testing.T) {
	now := time.Now()
	st := hibDispatchSet(2)
	st.SetPool(1)
	round, n := st.BackDeficit(now)
	if n != 2 {
		t.Fatalf("the hibernated pool launched %d, want 2", n)
	}
	st.LaunchedBack(round, []*Machine{{ID: "i-0b1", Region: "us-east-1", Launched: now, Hib: true, RootGB: 64}, {ID: "i-0b2", Region: "us-east-1", Launched: now, Hib: true, RootGB: 64}}, now)
	if _, n := st.BackDeficit(now); n != 0 {
		t.Fatalf("the hibernated pool launched %d more while its machines boot", n)
	}
	resume, round, n := st.Plan(now)
	if len(resume) != 0 || n != 1 {
		t.Fatalf("resumed %v and launched %d, want the pool's one cold machine: the hibernated pool's are no supply", resume, n)
	}
	st.Launched(round, []*Machine{{ID: "i-0f1", Region: "us-east-1", Launched: now, Dispatch: true}}, now)
	p := startDispatcher(t, &fakeS3{objects: map[string]string{}}, identityTags(), st)
	_ = p.machine(t, "i-0b1")
	waitUntil(t, "i-0b1 is put to hibernate", func() bool { return phaseOf(st, "i-0b1") == mHibernating })
	if idleMachines(st) != 0 {
		t.Fatal("a machine put to hibernate is in the idle queue")
	}
	if again := p.machine(t, "i-0b1"); again.code != 503 {
		t.Fatalf("a /work from a machine put to hibernate got %d, want 503 so its agent asks again", again.code)
	}
	pw := newPower(10, 10)
	st.parkAll(context.Background(), powerFleet(pw), time.Now())
	if pw.stopped["i-0b1"] != 1 || !slices.Equal(pw.hibernate, []bool{true}) {
		t.Fatalf("stopped %v hibernate %v, want i-0b1 hibernated", pw.stopped, pw.hibernate)
	}
	st.Seen("us-east-1", []*Machine{{ID: "i-0b1", Region: "us-east-1", Launched: now, Hib: true, RootGB: 64, State: "stopped"},
		{ID: "i-0b2", Region: "us-east-1", Launched: now, Hib: true, RootGB: 64, State: "pending"}, {ID: "i-0f1", Region: "us-east-1", Launched: now, Dispatch: true, State: "running"}}, now, now)
	if phaseOf(st, "i-0b1") != mHibernated {
		t.Fatalf("i-0b1 is %v once listed stopped, want asleep", phaseOf(st, "i-0b1"))
	}
	run := st.Running()
	for _, m := range run {
		if m.ID == "i-0b1" && !m.Parked || m.ID != "i-0b1" && m.Parked {
			t.Fatalf("%s parked %v: only the sleeping machine costs its volume alone", m.ID, m.Parked)
		}
	}
	_ = p.machine(t, "i-0f1")
	waitUntil(t, "i-0f1 waits", func() bool { return idleMachines(st) == 1 })
	shim := p.shim(t)
	shim.sendJob(jobMessage(jobA, 7, "unit"))
	if inst, _ := shim.paired(); inst != "i-0f1" {
		t.Fatalf("the job went to %s, want the running pool's machine", inst)
	}
	at := now.Add(time.Minute)
	resume, _, n = st.Plan(at)
	if len(resume) != 1 || resume[0].ID != "i-0b1" || n != 0 {
		t.Fatalf("after the hand-off resumed %v and launched %d cold, want i-0b1 resumed and no cold launch", resume, n)
	}
	if round, n := st.BackDeficit(at); n != 1 || round == "" {
		t.Fatalf("the hibernated pool asked %d to refill, want 1 for the machine drawn", n)
	}
	st.resumeAll(context.Background(), powerFleet(pw), resume)
	_ = p.machine(t, "i-0b1")
	waitUntil(t, "the resumed machine waits", func() bool { return phaseOf(st, "i-0b1") == mWaiting && idleMachines(st) == 1 })
	c := st.TakeCounts()
	if c.Parked != 1 || c.Resumed != 1 || c.ResumeFailed+c.ResumeLate+c.ParkFailed != 0 {
		t.Fatalf("counts %+v, want one hibernation and one resume", c)
	}
	if w := st.TakeResumeWaits(); len(w) != 1 {
		t.Fatalf("resume waits %v, want one", w)
	}
	snap := st.Snapshot(at)
	if snap.Hibernated != 2 || snap.BackBooting != 1 || snap.Asleep != 0 || snap.Resuming != 0 || snap.Idle != 1 || snap.Live != 2 {
		t.Fatalf("snapshot %+v, want i-0b2 still booting for the pool, i-0b1 idle and i-0f1 running", snap)
	}
}

// A pool machine that comes to /work while a job waits takes the job instead of sleeping.
func TestAHibernatedPoolMachineThatArrivesWhileAJobWaitsTakesIt(t *testing.T) {
	now := time.Now()
	st := hibDispatchSet(1)
	round, _ := st.BackDeficit(now)
	st.LaunchedBack(round, []*Machine{{ID: "i-0b1", Region: "us-east-1", Launched: now, Hib: true}}, now)
	p := startDispatcher(t, &fakeS3{objects: map[string]string{}}, identityTags(), st)
	shim := p.shim(t)
	shim.sendJob(jobMessage(jobA, 7, "unit"))
	waitUntil(t, "the job waits", func() bool { st.mu.Lock(); defer st.mu.Unlock(); return len(st.queue) == 1 })
	m := p.machine(t, "i-0b1")
	if j := m.job(); j.Token == "" {
		t.Fatal("no job")
	}
	if len(st.TakeParks(now)) != 0 {
		t.Fatal("a machine that took a job was put to hibernate")
	}
	if _, n := st.BackDeficit(now); n != 1 {
		t.Fatalf("refill asked %d, want 1 for the machine drawn", n)
	}
}

// A hibernation EC2 refuses, or one EC2 has not finished in parkLate, ends its machine; so does every pool machine past
// the spend's hard stop. At the cap the pool launches nothing new.
func TestFailedHibernationsAndTheSpendStopsEndPoolMachines(t *testing.T) {
	now := time.Now()
	st := hibDispatchSet(3)
	round, _ := st.BackDeficit(now)
	st.LaunchedBack(round, []*Machine{{ID: "i-0b1", Region: "us-east-1", Launched: now}, {ID: "i-0b2", Region: "us-east-1", Launched: now}, {ID: "i-0b3", Region: "us-east-1", Launched: now}}, now)
	p := startDispatcher(t, &fakeS3{objects: map[string]string{}}, identityTags(), st)
	_, _ = p.machine(t, "i-0b1"), p.machine(t, "i-0b2")
	waitUntil(t, "both are put to hibernate", func() bool { return phaseOf(st, "i-0b1") == mHibernating && phaseOf(st, "i-0b2") == mHibernating })
	pw := newPower(10, 10)
	pw.refuse["i-0b1"] = "UnsupportedHibernationConfiguration"
	st.parkAll(context.Background(), powerFleet(pw), now)
	if phaseOf(st, "i-0b1") != mEnding || phaseOf(st, "i-0b2") != mHibernating {
		t.Fatalf("i-0b1 %v i-0b2 %v, want the refused one ending and the other going to sleep", phaseOf(st, "i-0b1"), phaseOf(st, "i-0b2"))
	}
	st.Plan(now.Add(parkLate + time.Second))
	if phaseOf(st, "i-0b2") != mEnding {
		t.Fatal("a machine EC2 did not hibernate within parkLate was kept")
	}
	if c := st.TakeCounts(); c.ParkFailed != 2 || c.Parked != 1 {
		t.Fatalf("counts %+v, want 2 failed hibernations, 1 taken", c)
	}
	st.SetCapped(true)
	if _, n := st.BackDeficit(now); n != 0 {
		t.Fatalf("at the cap the pool launched %d", n)
	}
	st.SetCapped(false)
	st.SetStopped(true)
	st.Reconcile(now, bounds)
	if phaseOf(st, "i-0b3") != mEnding {
		t.Fatal("past the hard stop a pool machine was kept")
	}
}

// A pool machine asleep costs its root alone: 120 of them on 128 GiB at 1,000 MiB/s and 4,000 IOPS are about $8 an hour.
func TestAHibernatedMachineCostsItsVolumeOnly(t *testing.T) {
	spec := SetSpec{Name: "superci-8cpu", CPU: 8, DiskGB: 80}
	s := NewSpend(0, 0, []SetSpec{spec}, time.Now())
	asleep := s.Price(Machine{Set: "superci-8cpu", Hib: true, RootGB: 128, Parked: true, Lifecycle: "on-demand"})
	want := 128*0.08/730 + 875*0.04/730 + 1000*0.005/730
	if math.Abs(asleep-want) > 1e-9 || 120*asleep < 8 || 120*asleep > 8.5 {
		t.Fatalf("asleep %.5f/h (120: %.2f), want %.5f", asleep, 120*asleep, want)
	}
	awake := s.Price(Machine{Set: "superci-8cpu", Hib: true, RootGB: 128, Lifecycle: "on-demand"})
	if math.Abs(awake-(8*onDemandPerVCPUHour+want+ipv4PerHour)) > 1e-9 {
		t.Fatalf("awake %.5f/h, want the instance, its root and its address", awake)
	}
	if got, base := s.Price(Machine{Set: "superci-8cpu", Lifecycle: "on-demand"}), 8*onDemandPerVCPUHour+80*gp3PerGBHour+125*gp3PerMBpsHour+ipv4PerHour; math.Abs(got-base) > 1e-9 {
		t.Fatalf("an ordinary machine costs %.5f/h, want %.5f as before", got, base)
	}
}

// With hibernated 0 (every size as built in) the set is what it was: Plan resumes nothing and asks what Deficit
// asks, its snapshot has no new field, its metrics no new datum, a listing asks for no stopped machine, and a fleet
// call is what it was.
func TestWithNoHibernatedPoolASetIsWhatItWas(t *testing.T) {
	c, err := LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	for _, spec := range c.Sets() {
		if spec.Hibernated != 0 {
			t.Fatalf("%s keeps %d hibernated as built in, want 0", spec.Name, spec.Hibernated)
		}
	}
	now := time.Now()
	a, b := hibDispatchSet(0), hibDispatchSet(0)
	for _, st := range []*DispatchSet{a, b} {
		st.SetPool(3)
		for i := range 2 {
			st.enqueue(&session{set: st, arrived: now}, jobHead{ID: fmt.Sprintf("job-%d", i)}, now)
		}
	}
	resume, _, n := a.Plan(now)
	if _, want := b.Deficit(now); len(resume) != 0 || n != want || n != 5 {
		t.Fatalf("Plan resumed %v and asked %d, want nothing resumed and Deficit's %d", resume, n, want)
	}
	if _, n := a.BackDeficit(now); n != 0 {
		t.Fatalf("a set with no hibernated pool launched %d for one", n)
	}
	raw, _ := json.Marshal(a.Snapshot(now))
	for _, k := range []string{"hibernated", "back_booting", "parking", "asleep", "resuming"} {
		if strings.Contains(string(raw), `"`+k+`"`) {
			t.Fatalf("snapshot %s names %s", raw, k)
		}
	}
	m := Metrics{Dispatch: []DispatchSnapshot{a.Snapshot(now)}, DispatchCounts: map[string]DispatchCounts{}, StalledBy: map[string]bool{}}
	for _, d := range metricData(m, now) {
		if name := aws.ToString(d.MetricName); strings.Contains(name, "Hibernat") || strings.Contains(name, "Resum") && name != "DispatchResumes" {
			t.Fatalf("a set with no hibernated pool published %s", name)
		}
	}
	// The one request that differs at hibernated 0: a listing also asks for stopped machines, so turning a pool off
	// strands none of its machines (TestASetThatKeepsNoHibernatedPoolEndsTheStoppedPoolMachinesItLists).
	if want := []string{"pending", "running", "shutting-down", "stopping", "stopped"}; !slices.Equal(listedStates, want) {
		t.Fatalf("a listing asks for %v, want %v", listedStates, want)
	}
	z := &zones{left: map[string]int{"m8azn.3xlarge/subnet-us-east-1a": 5}}
	sf := hibFleet(t, z)
	sf.ask(context.Background(), sf.fleet.regions[0], 1, ec2types.DefaultTargetCapacityTypeOnDemand, "r-1", "fast-on-demand", []string{"m8azn.3xlarge"})
	in := z.calls[0]
	if spec := in.LaunchTemplateConfigs[0].LaunchTemplateSpecification; aws.ToString(spec.LaunchTemplateId) != "lt" || aws.ToString(spec.Version) != "7" ||
		tagValue(in.TagSpecifications[0].Tags, tagHib) != "" || aws.ToString(in.ClientToken) != tokenOf("superci-8cpu/r-1/fast-on-demand/us-east-1/0") {
		t.Fatalf("an ordinary launch asked %s v%s tag %q token %s: want the set's template, no pool tag and the token it had",
			aws.ToString(spec.LaunchTemplateId), aws.ToString(spec.Version), tagValue(in.TagSpecifications[0].Tags, tagHib), aws.ToString(in.ClientToken))
	}
}

// Only a dispatch size keeps machines hibernated, and never fewer than none.
func TestHibernatedIsForADispatchSizeAndNotNegative(t *testing.T) {
	for _, tc := range []struct {
		size string
		ok   bool
	}{
		{`{"cpu": 8, "split": 1, "max": 10, "disk_gb": 80, "types": ["m8azn.3xlarge"], "dispatch": true, "hibernated": 120}`, true},
		{`{"cpu": 8, "split": 1, "max": 10, "disk_gb": 80, "types": ["m8azn.3xlarge"], "hibernated": 120}`, false},
		{`{"cpu": 8, "split": 1, "max": 10, "disk_gb": 80, "types": ["m8azn.3xlarge"], "dispatch": true, "hibernated": -1}`, false},
	} {
		_, err := loadOverride(t, `{"sizes": {"superci-8cpu": `+tc.size+`}}`)
		if (err == nil) != tc.ok {
			t.Fatalf("%s: err %v, want ok %v", tc.size, err, tc.ok)
		}
	}
	c, _ := loadOverride(t, `{"sizes": {"superci-8cpu": {"cpu": 8, "split": 1, "max": 10, "disk_gb": 80, "types": ["m8azn.3xlarge"], "dispatch": true, "hibernated": 120}}}`)
	if i := slices.IndexFunc(c.Sets(), func(s SetSpec) bool { return s.Name == "superci-8cpu" }); c.Sets()[i].Hibernated != 120 {
		t.Fatal("the set did not get the size's hibernated")
	}
}

// loadOverride loads the built-in config under an override file of body.
func loadOverride(t *testing.T, body string) (Config, error) {
	t.Helper()
	path := t.TempDir() + "/override.json"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return LoadConfig(path)
}

// superci-4cpu's merge-queue storm (Nov 27 17:55Z: 202 legs within 47 s at 100%; 178 at @50) against its own hibernated
// set, sized on its own (running pool 50, 240 hibernated): each leg's replacement is a resume, every tenth resume never
// asks /work, and the launcher's Plan runs every 2.5 s. Every leg is handed a machine, none waits for a stuck resume (a leg
// takes whichever machine asks first), each stuck resume is ended at resumeLate and launched cold for, and no stuck
// machine is given a job.
func TestA4cpuBurstOf178LegsIn47sResumesItsHibernatedSetAndNoLegWaitsOnAStuckResume(t *testing.T) {
	t0 := time.Now()
	spec := SetSpec{Name: "superci-4cpu", Label: "superci-4cpu", Max: 600, Dispatch: true, Hibernated: 240}
	st := NewDispatchSet(spec, nil, []string{"us-east-1"}, t0.Add(-time.Hour))
	var listed []*Machine
	for i := range 50 {
		listed = append(listed, &Machine{ID: fmt.Sprintf("i-run%03d", i), Region: "us-east-1", Launched: t0.Add(-10 * time.Minute), Dispatch: true, State: "running"})
	}
	for i := range 240 {
		listed = append(listed, &Machine{ID: fmt.Sprintf("i-hib%03d", i), Region: "us-east-1", Launched: t0.Add(-time.Duration(240-i) * time.Second), Dispatch: true,
			Hib: true, RootGB: 76, State: "stopped", Lifecycle: "on-demand"})
	}
	st.Seen("us-east-1", listed, t0, t0)
	st.SetPool(50)
	ask := func(id string, at time.Time) {
		c, _ := net.Pipe()
		t.Cleanup(func() { c.Close() })
		if !st.waiting(st.machine(id), &wire{conn: c, done: make(chan struct{})}, at) {
			t.Fatalf("%s's /work was refused", id)
		}
	}
	for i := range 50 {
		ask(fmt.Sprintf("i-run%03d", i), t0)
	}
	type due struct {
		id string
		at time.Duration
	}
	var asks []due
	stuck := map[string]bool{}
	resumes, cold := 0, 0
	waits := map[string]time.Duration{}
	arrived := map[string]time.Duration{}
	var handedTo []string
	next := 0
	for step := time.Duration(0); step <= 150*time.Second; step += 500 * time.Millisecond {
		now := t0.Add(step)
		for ; next < 178 && time.Duration(next)*47*time.Second/178 <= step; next++ { // 178 legs, evenly over 47 s
			id := fmt.Sprintf("job-%03d", next)
			arrived[id] = step
			st.enqueue(&session{set: st, arrived: now}, jobHead{ID: id}, now)
		}
		for _, a := range asks {
			if a.at == step {
				ask(a.id, now)
			}
		}
		for {
			s, m, mw := st.next(now)
			if s == nil {
				break
			}
			st.claimed(s, m, mw, "token")
			st.handed(s, now)
			waits[s.id] = step - arrived[s.id]
			handedTo = append(handedTo, m.ID)
		}
		if step%(2500*time.Millisecond) != 0 {
			continue
		}
		resume, round, n := st.Plan(now)
		for _, m := range resume {
			if resumes++; resumes%10 == 0 {
				stuck[m.ID] = true // started, and never asks /work
				continue
			}
			asks = append(asks, due{m.ID, step + 11*time.Second})
		}
		if n > 0 {
			var got []*Machine
			for range n {
				cold++
				got = append(got, &Machine{ID: fmt.Sprintf("i-cold%03d", cold), Region: "us-east-1", Launched: now, Dispatch: true})
				asks = append(asks, due{got[len(got)-1].ID, step + 47*time.Second})
			}
			st.Launched(round, got, now)
		}
	}
	if len(waits) != 178 {
		t.Fatalf("%d of 178 legs were handed a machine", len(waits))
	}
	var ws []time.Duration
	for _, w := range waits {
		ws = append(ws, w)
	}
	slices.Sort(ws)
	p90, worst := ws[len(ws)*9/10], ws[len(ws)-1]
	if worst > 15*time.Second {
		t.Fatalf("a leg waited %v (p90 %v): no leg may wait out a stuck resume (resumeLate %v, then a cold launch)", worst, p90, resumeLate)
	}
	for _, id := range handedTo {
		if stuck[id] {
			t.Fatalf("the stuck machine %s was given a job", id)
		}
	}
	c := st.TakeCounts()
	if len(stuck) == 0 || c.ResumeLate != len(stuck) || cold < len(stuck) {
		t.Fatalf("%d stuck resumes, %d ended late, %d cold launches: want every stuck resume ended and launched cold for", len(stuck), c.ResumeLate, cold)
	}
	t.Logf("178 legs: p90 %v, worst %v; %d resumes (%d stuck, all ended late), %d cold launches", p90, worst, resumes, len(stuck), cold)
}

// Turning a set's pool off (hibernated 0), or the hard stop, strands nothing: a listing always asks for stopped
// machines, and each stopped or stopping pool machine of a set that keeps no pool is ended in the Reconcile that
// follows, with nothing launched for it.
func TestASetThatKeepsNoHibernatedPoolEndsTheStoppedPoolMachinesItLists(t *testing.T) {
	if !slices.Contains(listedStates, "stopped") {
		t.Fatalf("a listing asks for %v: a stopped pool machine of a set whose pool is off is never seen, and is left asleep", listedStates)
	}
	now := time.Now()
	for _, tc := range []struct {
		name       string
		hibernated int
		stop       bool
	}{{"hibernated 0", 0, false}, {"past the hard stop", 2, true}} {
		st := hibDispatchSet(tc.hibernated, "i-0s1", "i-0s2", "i-0s3")
		st.SetStopped(tc.stop)
		end, _ := st.Reconcile(now, bounds)
		var ids []string
		for _, m := range end {
			ids = append(ids, m.ID)
		}
		slices.Sort(ids)
		if want := []string{"i-0s1", "i-0s2", "i-0s3"}; !slices.Equal(ids, want) {
			t.Fatalf("%s: terminated %v, want %v in one Reconcile", tc.name, ids, want)
		}
		for _, id := range ids {
			if why := st.machines[id].why; why != "its set keeps no hibernated pool" {
				t.Fatalf("%s: %s ended for %q", tc.name, id, why)
			}
		}
		if resume, _, n := st.Plan(now); len(resume) != 0 || n != 0 {
			t.Fatalf("%s: resumed %v and launched %d", tc.name, resume, n)
		}
		if _, n := st.BackDeficit(now); n != 0 {
			t.Fatalf("%s: the pool launched %d", tc.name, n)
		}
	}
}

// A stopped machine without the hib tag was not stopped by a hibernated pool: it is neither adopted nor ended, only
// named in the listing's log line.
func TestAStoppedMachineWithoutTheHibTagIsNeitherAdoptedNorEnded(t *testing.T) {
	now := time.Now()
	st := hibDispatchSet(0)
	listed := []*Machine{{ID: "i-0plain", Set: "superci-4cpu", Region: "us-east-1", Launched: now.Add(-time.Hour), Dispatch: true, State: "stopped"},
		{ID: "i-0hib", Set: "superci-4cpu", Region: "us-east-1", Launched: now.Add(-time.Hour), Dispatch: true, State: "stopped", Hib: true}}
	running, leaving := live(listed)
	if got := untaggedStopped(leaving); !slices.Equal(got, []string{"i-0plain"}) {
		t.Fatalf("untagged stopped %v, want i-0plain named for the log", got)
	}
	st.Seen("us-east-1", append(running, parkedMachines(leaving)...), now, now)
	if st.machine("i-0plain") != nil {
		t.Fatal("a stopped machine without the hib tag was adopted")
	}
	end, _ := st.Reconcile(now, bounds)
	if len(end) != 1 || end[0].ID != "i-0hib" {
		t.Fatalf("terminated %v, want only the pool machine", end)
	}
}

// testBoot is a boot script as bootDataMode renders it for the set's own template: the image's root snapshot size left
// empty, so a machine sizes its pre-read by its root (boot.sh disk()).
const testBoot = "#!/bin/bash\nIMAGE_ROOT_BYTES=''\necho boot\n"

// rootImage stands in for DescribeImages: the pinned image, whose root (/dev/sda1) is a snapshot of gib GiB.
type rootImage int32

func (g rootImage) DescribeImages(_ context.Context, in *ec2.DescribeImagesInput, _ ...func(*ec2.Options)) (*ec2.DescribeImagesOutput, error) {
	return &ec2.DescribeImagesOutput{Images: []ec2types.Image{{ImageId: aws.String(in.ImageIds[0]), RootDeviceName: aws.String("/dev/sda1"),
		BlockDeviceMappings: []ec2types.BlockDeviceMapping{{DeviceName: aws.String("/dev/sdb")},
			{DeviceName: aws.String("/dev/sda1"), Ebs: &ec2types.EbsBlockDevice{VolumeSize: aws.Int32(int32(g)), SnapshotId: aws.String("snap-0root")}}}}}}, nil
}

// prereadOf runs a rendered boot script's disk() as it runs on a machine whose root, /dev/nvme0n1p1, starts at sector
// 227,328 (the image's layout) and is now size bytes, with the commands that change the machine faked. It returns the
// ROOT_BYTES disk() exported, which preread() reads.
func prereadOf(t *testing.T, script, size string) string {
	t.Helper()
	line := regexp.MustCompile(`(?m)^IMAGE_ROOT_BYTES='[0-9]*'$`).FindString(script)
	fn := regexp.MustCompile(`(?s)\ndisk\(\) \{\n.*?\n\}\ndisk\n`).FindString(script)
	if line == "" || fn == "" {
		t.Fatalf("the boot script has no IMAGE_ROOT_BYTES line (%q) or no disk function it calls", line)
	}
	dir := t.TempDir()
	cmd := exec.Command("bash", "-c", `findmnt() { echo /dev/nvme0n1p1; }
lsblk() { case "$*" in *SIZE*) echo "$SIZE" ;; *) echo nvme0n1 ;; esac; }
part_start() { echo 227328; }
mount() { :; }; growpart() { :; }; resize2fs() { :; }
`+line+"\n"+fn+`wait
printf %s "$ROOT_BYTES" > "$1/root"`, "_", dir)
	cmd.Env = append(os.Environ(), "SIZE="+size)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	got, _ := os.ReadFile(dir + "/root")
	return string(got)
}

// PR 1's image grows the root before boot.sh on a machine launched able to hibernate (superci-hibinit-prepare), so the
// root's size is no longer the snapshot's part of the volume: a pool machine on a 128 GiB root would pre-read all of it,
// swap file too. Its template carries the image's root snapshot size, read once from DescribeImages, and boot.sh pre-reads
// that less the root partition's start, whatever the root has grown to and whenever it grew. The set's own machines size
// it by their root as before.
func TestAHibernatedPoolMachinePrereadsOnlyTheImagesRootSnapshot(t *testing.T) {
	c, err := LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	spec := c.Sets()[slices.IndexFunc(c.Sets(), func(s SetSpec) bool { return s.Name == "superci-8cpu" })]
	spec.Dispatch, spec.Hibernated = true, 150
	boot := bootDataMode(c, "https://listener.test", "sha256//pin", spec.Label, "dispatch")
	sf := hibFleet(t, &zones{})
	sr := &setRegion{pools: sf.regions["us-east-1"].pools}
	api := &recordingTemplates{templates: &templates{byName: map[string]*fakeTemplate{}}, data: map[string]*ec2types.RequestLaunchTemplateData{}}
	r := &Region{Name: "us-east-1", image: "ami-pinned", root: "/dev/sda1"}
	if err := sr.ensureHibTemplates(context.Background(), api, instanceTypes{"m8azn.3xlarge": {49152, true}, "m8a.2xlarge": {32768, true}}, rootImage(30), r,
		"superci-pull", boot, "sg-1", spec, nil); err != nil {
		t.Fatal(err)
	}
	hib, _ := userDataScript(aws.ToString(api.data["superci-pull-superci-8cpu-hib-m8azn.3xlarge"].UserData))
	want := strconv.FormatInt(30<<30-227328*512, 10)
	if got := prereadOf(t, hib, strconv.FormatInt(128<<30, 10)); got != want {
		t.Fatalf("a pool machine whose root is already 128 GiB pre-reads %s bytes, want the 30 GiB snapshot's part of it, %s", got, want)
	}
	ordinary, _ := userDataScript(aws.ToString(templateData(r, boot, "sg-1", spec.DiskGB, spec, nil).UserData))
	if got := prereadOf(t, ordinary, "30083776512"); got != "30083776512" {
		t.Fatalf("a machine of the set's own template pre-reads %s bytes, want its root's size before it grows, as before", got)
	}
}

// EC2 refuses to hibernate a machine for its first minutes ("not ready to hibernate yet": the lab's pilots were refused
// at 1m41s and taken by 2m06s and 3m53s). That refusal is asked again, later each time, until 6 minutes after the
// launch; parkLate runs from the call EC2 took.
func TestAHibernationRefusedAsNotReadyIsAskedAgainUntilItIsTaken(t *testing.T) {
	now := time.Now()
	st := hibDispatchSet(1)
	round, _ := st.BackDeficit(now)
	st.LaunchedBack(round, []*Machine{{ID: "i-0b1", Region: "us-east-1", Launched: now.Add(-100 * time.Second), Hib: true}}, now)
	st.machines["i-0b1"].Launched = now.Add(-100 * time.Second)
	p := startDispatcher(t, &fakeS3{objects: map[string]string{}}, identityTags(), st)
	_ = p.machine(t, "i-0b1")
	waitUntil(t, "i-0b1 is put to hibernate", func() bool { return phaseOf(st, "i-0b1") == mHibernating })
	pw := newPower(10, 10)
	pw.notReady = 2
	f := powerFleet(pw)
	st.parkAll(context.Background(), f, now) // refused: not ready
	if phaseOf(st, "i-0b1") != mHibernating {
		t.Fatalf("i-0b1 is %v after EC2 said it is not ready yet, want it kept to be asked again", phaseOf(st, "i-0b1"))
	}
	st.parkAll(context.Background(), f, now.Add(5*time.Second))
	if pw.notReady != 1 {
		t.Fatal("asked again 5 s after a not-ready refusal: it waits before the next ask")
	}
	st.parkAll(context.Background(), f, now.Add(20*time.Second)) // refused again
	st.parkAll(context.Background(), f, now.Add(80*time.Second)) // taken
	if pw.stopped["i-0b1"] != 1 || phaseOf(st, "i-0b1") != mHibernating {
		t.Fatalf("stopped %v, phase %v: want the third ask taken", pw.stopped, phaseOf(st, "i-0b1"))
	}
	st.Plan(now.Add(80*time.Second + parkLate - time.Second))
	if phaseOf(st, "i-0b1") != mHibernating {
		t.Fatal("ended before parkLate from the call EC2 took: parkLate ran from the first ask")
	}
	st.Seen("us-east-1", []*Machine{{ID: "i-0b1", Region: "us-east-1", Launched: now.Add(-100 * time.Second), Hib: true, State: "stopped"}}, now, now)
	if phaseOf(st, "i-0b1") != mHibernated {
		t.Fatalf("i-0b1 is %v once listed stopped, want asleep", phaseOf(st, "i-0b1"))
	}
	if c := st.TakeCounts(); c.Parked != 1 || c.ParkFailed != 0 {
		t.Fatalf("counts %+v, want one hibernation taken and none failed", c)
	}
}

// A machine EC2 still calls not ready 6 minutes after its launch is ended.
func TestAHibernationStillNotReadySixMinutesAfterTheLaunchEndsTheMachine(t *testing.T) {
	now := time.Now()
	st := hibDispatchSet(1)
	round, _ := st.BackDeficit(now)
	st.LaunchedBack(round, []*Machine{{ID: "i-0b1", Region: "us-east-1", Hib: true}}, now)
	st.machines["i-0b1"].Launched = now.Add(-350 * time.Second)
	p := startDispatcher(t, &fakeS3{objects: map[string]string{}}, identityTags(), st)
	_ = p.machine(t, "i-0b1")
	waitUntil(t, "i-0b1 is put to hibernate", func() bool { return phaseOf(st, "i-0b1") == mHibernating })
	pw := newPower(10, 10)
	pw.notReady = 100
	st.parkAll(context.Background(), powerFleet(pw), now) // 5m50s after its launch: asked again later
	if phaseOf(st, "i-0b1") != mHibernating {
		t.Fatal("a not-ready refusal before 6 min ended the machine")
	}
	st.parkAll(context.Background(), powerFleet(pw), now.Add(20*time.Second)) // 6m10s
	if phaseOf(st, "i-0b1") != mEnding || st.machines["i-0b1"].why == "" {
		t.Fatalf("i-0b1 is %v past 6 min of not-ready refusals, want it ended", phaseOf(st, "i-0b1"))
	}
	if c := st.TakeCounts(); c.ParkFailed != 1 {
		t.Fatalf("counts %+v, want one failed hibernation", c)
	}
}
