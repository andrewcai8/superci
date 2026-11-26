package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"
)

// A Region is where machines start: its client, the image jobs boot and the default subnet of each zone.
type Region struct {
	Name    string
	Weight  int
	ec2     *ec2.Client
	fleets  fleetAPI
	image   string
	root    string
	subnets map[string]string // zone -> default subnet
}

// fleetAPI is the call a launch makes, so a test can stand in for EC2's capacity.
type fleetAPI interface {
	CreateFleet(ctx context.Context, in *ec2.CreateFleetInput, opts ...func(*ec2.Options)) (*ec2.CreateFleetOutput, error)
}

// Fleet is the machines of every scale set. Every instance carries the tags purpose=superci-pull,
// superci-pull-deployment=<deployment> and superci-pull-set=<set>, which are all the reconciler needs to find them
// again; a listener sees and ends only the machines of its own deployment. A machine issued its runner also carries
// superci-pull-issued.
type Fleet struct {
	regions    []*Region
	deployment string
}

const (
	tagDeployment = "superci-pull-deployment"
	tagIssued     = "superci-pull-issued"
	tagNoticed    = "superci-pull-noticed" // <owner>/<repo>/<run id> of the job a spot notice found running
)

type ImageConfig struct {
	Owner, Name, Pin string
}

func NewFleet(ctx context.Context, deployment string, weights map[string]int, img ImageConfig) (*Fleet, error) {
	names := make([]string, 0, len(weights))
	for name := range weights {
		names = append(names, name)
	}
	sort.Strings(names)
	f := &Fleet{deployment: deployment}
	for _, name := range names {
		cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(name))
		if err != nil {
			return nil, err
		}
		client := ec2.NewFromConfig(cfg)
		r := &Region{Name: name, Weight: weights[name], ec2: client, fleets: client, subnets: map[string]string{}}
		if r.image, r.root, err = r.findImage(ctx, img); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		subnets, err := r.ec2.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{Filters: []ec2types.Filter{{Name: aws.String("default-for-az"), Values: []string{"true"}}}})
		if err != nil {
			return nil, err
		}
		for _, s := range subnets.Subnets {
			r.subnets[aws.ToString(s.AvailabilityZone)] = aws.ToString(s.SubnetId)
		}
		f.regions = append(f.regions, r)
	}
	return f, nil
}

// findImage is the image jobs boot, as superci picks it: the pinned build, or once its maker has deleted it the oldest
// newer build (out the longest), or with none newer the newest.
func (r *Region) findImage(ctx context.Context, img ImageConfig) (id, root string, err error) {
	out, err := r.ec2.DescribeImages(ctx, &ec2.DescribeImagesInput{Owners: []string{img.Owner, "self"},
		Filters: []ec2types.Filter{{Name: aws.String("name"), Values: []string{img.Name}}, {Name: aws.String("state"), Values: []string{"available"}}}})
	if err != nil {
		return "", "", err
	}
	build := func(i ec2types.Image) string { n := aws.ToString(i.Name); return n[strings.LastIndexByte(n, '-')+1:] }
	imgs := out.Images
	if len(imgs) == 0 {
		return "", "", fmt.Errorf("no image %s/%s", img.Owner, img.Name)
	}
	slices.SortFunc(imgs, func(a, b ec2types.Image) int { return strings.Compare(build(a), build(b)) })
	pick := imgs[len(imgs)-1]
	for _, i := range imgs {
		if build(i) >= img.Pin {
			pick = i
			break
		}
	}
	return aws.ToString(pick.ImageId), aws.ToString(pick.RootDeviceName), nil
}

// SetFleet launches one scale set's machines: its launch template and its (type, subnet) pools in each region.
type SetFleet struct {
	fleet   *Fleet
	spec    SetSpec
	tags    []ec2types.Tag
	regions map[string]*setRegion
}

type setRegion struct {
	template, version string
	pools             []ec2types.FleetLaunchTemplateOverridesRequest
}

func (f *Fleet) AddSet(ctx context.Context, spec SetSpec, userData string, groups map[string]string) (*SetFleet, error) {
	sf := &SetFleet{fleet: f, spec: spec, regions: map[string]*setRegion{},
		tags: []ec2types.Tag{{Key: aws.String("purpose"), Value: aws.String("superci-pull")}, {Key: aws.String(tagDeployment), Value: aws.String(f.deployment)},
			{Key: aws.String("superci-pull-set"), Value: aws.String(spec.Name)}}}
	for _, r := range f.regions {
		sr := &setRegion{}
		if err := sr.ensureTemplate(ctx, r, "superci-pull-"+spec.Name, userData, groups[r.Name], spec.DiskGB, sf.tags); err != nil {
			return nil, fmt.Errorf("%s %s template: %w", spec.Name, r.Name, err)
		}
		if err := sr.findPools(ctx, r, spec.Types); err != nil {
			return nil, fmt.Errorf("%s %s pools: %w", spec.Name, r.Name, err)
		}
		types := map[string]bool{}
		for _, p := range sr.pools {
			types[string(p.InstanceType)] = true
		}
		slog.Info("set region ready", "set", spec.Name, "region", r.Name, "image", r.image, "template", sr.template,
			"version", sr.version, "pools", len(sr.pools), "types", len(types))
		sf.regions[r.Name] = sr
	}
	return sf, nil
}

func (sr *setRegion) ensureTemplate(ctx context.Context, r *Region, name, userData, group string, diskGB int, tags []ec2types.Tag) error {
	data := &ec2types.RequestLaunchTemplateData{
		ImageId:                           aws.String(r.image),
		UserData:                          aws.String(base64.StdEncoding.EncodeToString([]byte(userData))),
		SecurityGroupIds:                  []string{group},
		IamInstanceProfile:                &ec2types.LaunchTemplateIamInstanceProfileSpecificationRequest{Name: aws.String("superci-pull-cache-machine")},
		InstanceInitiatedShutdownBehavior: ec2types.ShutdownBehaviorTerminate,
		MetadataOptions: &ec2types.LaunchTemplateInstanceMetadataOptionsRequest{HttpTokens: ec2types.LaunchTemplateHttpTokensStateRequired,
			HttpPutResponseHopLimit: aws.Int32(1), HttpEndpoint: ec2types.LaunchTemplateInstanceMetadataEndpointStateEnabled},
		BlockDeviceMappings: []ec2types.LaunchTemplateBlockDeviceMappingRequest{{DeviceName: aws.String(r.root), Ebs: &ec2types.LaunchTemplateEbsBlockDeviceRequest{
			VolumeSize: aws.Int32(int32(diskGB)), VolumeType: ec2types.VolumeTypeGp3, Iops: aws.Int32(3000), Throughput: aws.Int32(250), DeleteOnTermination: aws.Bool(true)}}},
		TagSpecifications: []ec2types.LaunchTemplateTagSpecificationRequest{{ResourceType: ec2types.ResourceTypeInstance, Tags: tags}, {ResourceType: ec2types.ResourceTypeVolume, Tags: tags}},
	}
	got, err := r.ec2.DescribeLaunchTemplates(ctx, &ec2.DescribeLaunchTemplatesInput{LaunchTemplateNames: []string{name}})
	if err == nil && len(got.LaunchTemplates) == 1 {
		sr.template = aws.ToString(got.LaunchTemplates[0].LaunchTemplateId)
		// A start makes a version only when the machines' data changed (a template keeps at most 10,000).
		latest, err := r.ec2.DescribeLaunchTemplateVersions(ctx, &ec2.DescribeLaunchTemplateVersionsInput{LaunchTemplateId: aws.String(sr.template), Versions: []string{"$Latest"}})
		if err == nil && len(latest.LaunchTemplateVersions) == 1 && sameData(latest.LaunchTemplateVersions[0].LaunchTemplateData, data) {
			sr.version = fmt.Sprint(aws.ToInt64(latest.LaunchTemplateVersions[0].VersionNumber))
			return nil
		}
		v, err := r.ec2.CreateLaunchTemplateVersion(ctx, &ec2.CreateLaunchTemplateVersionInput{LaunchTemplateId: aws.String(sr.template), LaunchTemplateData: data})
		if err != nil {
			return err
		}
		sr.version = fmt.Sprint(aws.ToInt64(v.LaunchTemplateVersion.VersionNumber))
		return nil
	}
	made, err := r.ec2.CreateLaunchTemplate(ctx, &ec2.CreateLaunchTemplateInput{LaunchTemplateName: aws.String(name), LaunchTemplateData: data,
		TagSpecifications: []ec2types.TagSpecification{{ResourceType: ec2types.ResourceTypeLaunchTemplate, Tags: tags}}})
	if err != nil {
		return err
	}
	sr.template = aws.ToString(made.LaunchTemplate.LaunchTemplateId)
	sr.version = fmt.Sprint(aws.ToInt64(made.LaunchTemplate.LatestVersionNumber))
	return nil
}

// sameData says whether a template version holds what a new version would: image, user data, security group, the
// cache's instance profile, disk and metadata options.
func sameData(have *ec2types.ResponseLaunchTemplateData, want *ec2types.RequestLaunchTemplateData) bool {
	if have == nil || have.MetadataOptions == nil || len(have.BlockDeviceMappings) != 1 || have.BlockDeviceMappings[0].Ebs == nil {
		return false
	}
	h, w := have.BlockDeviceMappings[0], want.BlockDeviceMappings[0]
	return aws.ToString(have.ImageId) == aws.ToString(want.ImageId) && aws.ToString(have.UserData) == aws.ToString(want.UserData) &&
		slices.Equal(have.SecurityGroupIds, want.SecurityGroupIds) &&
		have.IamInstanceProfile != nil && strings.HasSuffix(aws.ToString(have.IamInstanceProfile.Arn), "/superci-pull-cache-machine") &&
		aws.ToString(h.DeviceName) == aws.ToString(w.DeviceName) &&
		aws.ToInt32(h.Ebs.VolumeSize) == aws.ToInt32(w.Ebs.VolumeSize) && aws.ToInt32(h.Ebs.Throughput) == aws.ToInt32(w.Ebs.Throughput) &&
		aws.ToInt32(have.MetadataOptions.HttpPutResponseHopLimit) == aws.ToInt32(want.MetadataOptions.HttpPutResponseHopLimit) &&
		string(have.MetadataOptions.HttpTokens) == string(want.MetadataOptions.HttpTokens)
}

// findPools pairs each machine type with the default subnet of every zone that offers it.
func (sr *setRegion) findPools(ctx context.Context, r *Region, types []string) error {
	offers, err := r.ec2.DescribeInstanceTypeOfferings(ctx, &ec2.DescribeInstanceTypeOfferingsInput{LocationType: ec2types.LocationTypeAvailabilityZone,
		Filters: []ec2types.Filter{{Name: aws.String("instance-type"), Values: types}}, MaxResults: aws.Int32(1000)})
	if err != nil {
		return err
	}
	sr.pools = rankedPools(offers.InstanceTypeOfferings, r.subnets, types)
	if len(sr.pools) == 0 {
		return errors.New("no pools")
	}
	return nil
}

// rankedPools is each offered type in each zone with a default subnet, with the type's place in the size's list as
// its priority (0 first). A fleet takes the first types that have capacity, so a size lists its types fastest first:
// on Nov 25 a CPU-bound test step ran 62 s on c5a.xlarge, 50 s on c7i-flex.xlarge, 38 s on c7a.xlarge and c8i.xlarge,
// and Blacksmith's 31 s needed c8a or m8a, whose spot pools are the smallest.
func rankedPools(offers []ec2types.InstanceTypeOffering, subnets map[string]string, types []string) []ec2types.FleetLaunchTemplateOverridesRequest {
	rank := make(map[string]int, len(types))
	for i, t := range types {
		rank[t] = i
	}
	var pools []ec2types.FleetLaunchTemplateOverridesRequest
	for _, o := range offers {
		if subnet, ok := subnets[aws.ToString(o.Location)]; ok {
			pools = append(pools, ec2types.FleetLaunchTemplateOverridesRequest{InstanceType: o.InstanceType, SubnetId: aws.String(subnet),
				Priority: aws.Float64(float64(rank[string(o.InstanceType)]))})
		}
	}
	return pools
}

// Versions is the launch template version each region's new machines boot from.
func (sf *SetFleet) Versions() map[string]string {
	out := make(map[string]string, len(sf.regions))
	for name, sr := range sf.regions {
		out[name] = sr.version
	}
	return out
}

// poolsOf is the region's pools of the given types.
func (sr *setRegion) poolsOf(types []string) []ec2types.FleetLaunchTemplateOverridesRequest {
	var out []ec2types.FleetLaunchTemplateOverridesRequest
	for _, p := range sr.pools {
		if slices.Contains(types, string(p.InstanceType)) {
			out = append(out, p)
		}
	}
	return out
}

// chunk is the most capacity one CreateFleet asks for; a burst is a handful of these per region.
const chunk = 250

// Launch starts n machines. The size's leading OnDemandFirst types go first: spot across every region, then on demand,
// whatever the on-demand share. Then the other types' spot across every region, and only then their on-demand, at most
// odRoom of them. A region's spot shortfall is asked again of the others. It returns what started.
func (sf *SetFleet) Launch(ctx context.Context, round string, n, odRoom int) []*Machine {
	fast, rest := sf.spec.Types[:sf.spec.OnDemandFirst], sf.spec.Types[sf.spec.OnDemandFirst:]
	var got []*Machine
	if len(fast) > 0 {
		got = append(got, sf.spot(ctx, round, "fast-", n, fast)...)
		got = append(got, sf.onDemand(ctx, round, "fast-", n-len(got), n, fast)...)
	}
	if len(rest) > 0 {
		got = append(got, sf.spot(ctx, round, "", n-len(got), rest)...)
	}
	got = append(got, sf.onDemand(ctx, round, "", n-len(got), odRoom, rest)...)
	if left := n - len(got); left > 0 {
		slog.Warn("launch short", "set", sf.spec.Name, "round", round, "asked", n, "short", left)
	}
	return got
}

// spot asks each region for its weighted part of n machines of types, then the other regions for a region's shortfall.
func (sf *SetFleet) spot(ctx context.Context, round, step string, n int, types []string) []*Machine {
	if n <= 0 {
		return nil
	}
	var got []*Machine
	want := sf.fleet.split(n)
	short := map[string]int{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, r := range sf.fleet.regions {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m := sf.ask(ctx, r, want[r.Name], ec2types.DefaultTargetCapacityTypeSpot, round, step+"spot", types)
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
				m := sf.ask(ctx, other, short[r.Name], ec2types.DefaultTargetCapacityTypeSpot, round, step+"spot-for-"+r.Name, types)
				got = append(got, m...)
				short[r.Name] -= len(m)
			}
		}
	}
	return got
}

// onDemand asks the regions in turn for n on-demand machines of types, at most room of them.
func (sf *SetFleet) onDemand(ctx context.Context, round, step string, n, room int, types []string) []*Machine {
	var got []*Machine
	for _, r := range sf.fleet.regions {
		if n <= 0 || room <= 0 {
			break
		}
		m := sf.ask(ctx, r, min(n, room), ec2types.DefaultTargetCapacityTypeOnDemand, round, step+"on-demand", types)
		got = append(got, m...)
		n -= len(m)
		room -= len(m)
	}
	return got
}

// split divides n across regions by weight.
func (f *Fleet) split(n int) map[string]int {
	total := 0
	for _, r := range f.regions {
		total += r.Weight
	}
	out := map[string]int{}
	left := n
	for i, r := range f.regions {
		k := n * r.Weight / total
		if i == len(f.regions)-1 {
			k = left
		}
		out[r.Name] = k
		left -= k
	}
	return out
}

// ask makes the CreateFleet calls for n machines in one region, at most chunk each, all at once. Each call carries a
// ClientToken unique to this process, round and part, so a call retried after a lost answer starts nothing twice.
func (sf *SetFleet) ask(ctx context.Context, r *Region, n int, kind ec2types.DefaultTargetCapacityType, round, step string, types []string) []*Machine {
	pools := sf.regions[r.Name].poolsOf(types)
	if n <= 0 || len(pools) == 0 {
		return nil
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	var got []*Machine
	for part := 0; n > 0; part++ {
		k := min(n, chunk)
		n -= k
		wg.Add(1)
		go func(part, k int) {
			defer wg.Done()
			sum := sha256.Sum256(fmt.Appendf(nil, "%s/%s/%s/%s/%d", sf.spec.Name, round, step, r.Name, part))
			token := hex.EncodeToString(sum[:16])
			started := time.Now()
			ms, errs, err := sf.createFleet(ctx, r, k, kind, token, round, pools)
			slog.Info("fleet", "set", sf.spec.Name, "region", r.Name, "step", step, "round", round, "part", part, "asked", k, "got", len(ms),
				"ms", time.Since(started).Milliseconds(), "errors", errs, "err", errString(err), "token", token)
			tlLaunched(sf.spec.Name, round, step, started, ms)
			mu.Lock()
			got = append(got, ms...)
			mu.Unlock()
		}(part, k)
	}
	wg.Wait()
	return got
}

func (sf *SetFleet) createFleet(ctx context.Context, r *Region, n int, kind ec2types.DefaultTargetCapacityType, token, round string,
	pools []ec2types.FleetLaunchTemplateOverridesRequest) ([]*Machine, map[string]int, error) {
	sr := sf.regions[r.Name]
	tags := append([]ec2types.Tag{{Key: aws.String("superci-pull-round"), Value: aws.String(round)}}, sf.tags...)
	in := &ec2.CreateFleetInput{
		Type:                        ec2types.FleetTypeInstant,
		ClientToken:                 aws.String(token),
		TargetCapacitySpecification: &ec2types.TargetCapacitySpecificationRequest{TotalTargetCapacity: aws.Int32(int32(n)), DefaultTargetCapacityType: kind},
		SpotOptions:                 &ec2types.SpotOptionsRequest{AllocationStrategy: ec2types.SpotAllocationStrategyCapacityOptimizedPrioritized},
		OnDemandOptions:             &ec2types.OnDemandOptionsRequest{AllocationStrategy: ec2types.FleetOnDemandAllocationStrategyPrioritized},
		LaunchTemplateConfigs: []ec2types.FleetLaunchTemplateConfigRequest{{
			LaunchTemplateSpecification: &ec2types.FleetLaunchTemplateSpecificationRequest{LaunchTemplateId: aws.String(sr.template), Version: aws.String(sr.version)},
			Overrides:                   pools,
		}},
		TagSpecifications: []ec2types.TagSpecification{{ResourceType: ec2types.ResourceTypeInstance, Tags: tags}, {ResourceType: ec2types.ResourceTypeFleet, Tags: sf.tags}},
	}
	var out *ec2.CreateFleetOutput
	var err error
	for try := 0; try < 4; try++ {
		out, err = r.fleets.CreateFleet(ctx, in)
		if err == nil || !retryable(err) {
			break
		}
		time.Sleep(time.Duration(500*(try+1)) * time.Millisecond)
	}
	if err != nil {
		return nil, nil, err
	}
	now := time.Now()
	lifecycle := "spot"
	if kind == ec2types.DefaultTargetCapacityTypeOnDemand {
		lifecycle = "on-demand"
	}
	var ms []*Machine
	for _, i := range out.Instances {
		az := ""
		if i.LaunchTemplateAndOverrides != nil && i.LaunchTemplateAndOverrides.Overrides != nil {
			az = aws.ToString(i.LaunchTemplateAndOverrides.Overrides.AvailabilityZone)
		}
		for _, id := range i.InstanceIds {
			ms = append(ms, &Machine{ID: id, Set: sf.spec.Name, Region: r.Name, Type: string(i.InstanceType), AZ: az, Lifecycle: lifecycle,
				Launched: now, Asked: now, Round: round, Template: sr.version})
		}
	}
	errs := map[string]int{}
	for _, e := range out.Errors {
		errs[aws.ToString(e.ErrorCode)]++
	}
	return ms, errs, nil
}

func retryable(err error) bool {
	var ae smithy.APIError
	if errors.As(err, &ae) {
		switch ae.ErrorCode() {
		case "RequestLimitExceeded", "InternalError", "ServiceUnavailable", "Unavailable":
			return true
		}
	}
	return false
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// Describe lists every scale set's machines of this deployment that are starting or running in a region.
func (f *Fleet) Describe(ctx context.Context, r *Region) ([]*Machine, error) {
	var out []*Machine
	p := ec2.NewDescribeInstancesPaginator(r.ec2, &ec2.DescribeInstancesInput{MaxResults: aws.Int32(1000), Filters: []ec2types.Filter{
		{Name: aws.String("tag:purpose"), Values: []string{"superci-pull"}},
		{Name: aws.String("tag:" + tagDeployment), Values: []string{f.deployment}},
		{Name: aws.String("tag-key"), Values: []string{"superci-pull-set"}},
		{Name: aws.String("instance-state-name"), Values: []string{"pending", "running"}},
	}})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, res := range page.Reservations {
			for _, i := range res.Instances {
				m := &Machine{ID: aws.ToString(i.InstanceId), Region: r.Name, IP: aws.ToString(i.PublicIpAddress), Type: string(i.InstanceType),
					Lifecycle: "on-demand", Launched: aws.ToTime(i.LaunchTime)}
				if i.InstanceLifecycle == ec2types.InstanceLifecycleTypeSpot {
					m.Lifecycle = "spot"
				}
				if i.Placement != nil {
					m.AZ = aws.ToString(i.Placement.AvailabilityZone)
				}
				for _, t := range i.Tags {
					switch aws.ToString(t.Key) {
					case "superci-pull-round":
						m.Round = aws.ToString(t.Value)
					case "superci-pull-set":
						m.Set = aws.ToString(t.Value)
					case tagIssued:
						m.Issued, m.Tagged = true, true
					case tagNoticed:
						m.Notice = true
					case "aws:ec2launchtemplate:version":
						m.Template = aws.ToString(t.Value)
					}
				}
				out = append(out, m)
			}
		}
	}
	return out, nil
}

// TagIssued records on EC2 that machines of a region were issued their runner. Every machine of one call gets the
// same value (when), so a burst of issues is one call; EC2 allows only a few mutating calls a second.
func (f *Fleet) TagIssued(ctx context.Context, r *Region, ids []string, when time.Time) error {
	in := &ec2.CreateTagsInput{Resources: ids, Tags: []ec2types.Tag{{Key: aws.String(tagIssued), Value: aws.String(when.UTC().Format(time.RFC3339))}}}
	var err error
	for try := 0; try < 4; try++ {
		if _, err = r.ec2.CreateTags(ctx, in); err == nil || !retryable(err) {
			return err
		}
		time.Sleep(time.Duration(250*(try+1)) * time.Millisecond)
	}
	return err
}

// TagNoticed records on a machine the job a spot notice found running on it.
func (f *Fleet) TagNoticed(ctx context.Context, m *Machine, j JobRef) error {
	r := f.Region(m.Region)
	if r == nil {
		return fmt.Errorf("no region %q", m.Region)
	}
	_, err := r.ec2.CreateTags(ctx, &ec2.CreateTagsInput{Resources: []string{m.ID},
		Tags: []ec2types.Tag{{Key: aws.String(tagNoticed), Value: aws.String(fmt.Sprintf("%s/%s/%d", j.Owner, j.Repo, j.RunID))}}})
	return err
}

// NoticedJobs is the job of every machine of this deployment EC2 lists with a notice tag, running or ended (EC2
// lists an ended machine for about an hour).
func (f *Fleet) NoticedJobs(ctx context.Context) ([]JobRef, error) {
	var out []JobRef
	for _, r := range f.regions {
		p := ec2.NewDescribeInstancesPaginator(r.ec2, &ec2.DescribeInstancesInput{MaxResults: aws.Int32(1000), Filters: []ec2types.Filter{
			{Name: aws.String("tag:purpose"), Values: []string{"superci-pull"}},
			{Name: aws.String("tag:" + tagDeployment), Values: []string{f.deployment}},
			{Name: aws.String("tag-key"), Values: []string{tagNoticed}},
		}})
		for p.HasMorePages() {
			page, err := p.NextPage(ctx)
			if err != nil {
				return out, err
			}
			for _, res := range page.Reservations {
				for _, in := range res.Instances {
					var set, noticed string
					for _, t := range in.Tags {
						switch aws.ToString(t.Key) {
						case "superci-pull-set":
							set = aws.ToString(t.Value)
						case tagNoticed:
							noticed = aws.ToString(t.Value)
						}
					}
					if j, ok := noticedJob(set, aws.ToString(in.InstanceId), noticed); ok {
						out = append(out, j)
					}
				}
			}
		}
	}
	return out, nil
}

// noticedJob reads a notice tag (<owner>/<repo>/<run id>) as the job of the machine's runner.
func noticedJob(set, id, tag string) (JobRef, bool) {
	parts := strings.Split(tag, "/")
	if len(parts) != 3 || set == "" {
		return JobRef{}, false
	}
	run, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil || parts[0] == "" || parts[1] == "" {
		return JobRef{}, false
	}
	return JobRef{Owner: parts[0], Repo: parts[1], RunID: run, Runner: runnerName(set, id)}, true
}

// SpotReclaimed says which of the given machines EC2 reports terminated by a spot interruption. EC2 lists a
// terminated instance for about an hour, so the proof must be read soon after the loss.
func (f *Fleet) SpotReclaimed(ctx context.Context, ids []string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, r := range f.regions {
		for i := 0; i < len(ids); i += 200 {
			p := ec2.NewDescribeInstancesPaginator(r.ec2, &ec2.DescribeInstancesInput{Filters: []ec2types.Filter{
				{Name: aws.String("instance-id"), Values: ids[i:min(len(ids), i+200)]}}})
			for p.HasMorePages() {
				page, err := p.NextPage(ctx)
				if err != nil {
					return nil, err
				}
				for _, res := range page.Reservations {
					for _, in := range res.Instances {
						if in.StateReason != nil && strings.HasPrefix(aws.ToString(in.StateReason.Code), "Server.SpotInstance") {
							out[aws.ToString(in.InstanceId)] = true
						}
					}
				}
			}
		}
	}
	return out, nil
}

// Terminate ends machines, grouped by region.
func (f *Fleet) Terminate(ctx context.Context, ms []*Machine, why string) {
	by := map[string][]string{}
	for _, m := range ms {
		by[m.Region] = append(by[m.Region], m.ID)
	}
	for _, r := range f.regions {
		ids := by[r.Name]
		for len(ids) > 0 {
			k := min(len(ids), 500)
			_, err := r.ec2.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: ids[:k]})
			slog.Info("terminate", "region", r.Name, "why", why, "count", k, "err", errString(err))
			tlEnded(r.Name, why, ids[:k], time.Now())
			ids = ids[k:]
		}
	}
}

// SpotPrices is the latest spot price of each (zone, type) of the given types in a region, in dollars an hour.
func (f *Fleet) SpotPrices(ctx context.Context, r *Region, types []string) (map[string]float64, error) {
	out := map[string]float64{}
	now := time.Now()
	for i := 0; i < len(types); i += 100 {
		batch := types[i:min(len(types), i+100)]
		p := ec2.NewDescribeSpotPriceHistoryPaginator(r.ec2, &ec2.DescribeSpotPriceHistoryInput{StartTime: aws.Time(now), EndTime: aws.Time(now),
			ProductDescriptions: []string{"Linux/UNIX"}, InstanceTypes: toTypes(batch)})
		for p.HasMorePages() {
			page, err := p.NextPage(ctx)
			if err != nil {
				return nil, err
			}
			for _, sp := range page.SpotPriceHistory {
				var v float64
				fmt.Sscan(aws.ToString(sp.SpotPrice), &v)
				out[aws.ToString(sp.AvailabilityZone)+"/"+string(sp.InstanceType)] = v
			}
		}
	}
	return out, nil
}

func toTypes(names []string) []ec2types.InstanceType {
	out := make([]ec2types.InstanceType, len(names))
	for i, n := range names {
		out[i] = ec2types.InstanceType(n)
	}
	return out
}

func (f *Fleet) Region(name string) *Region {
	for _, r := range f.regions {
		if r.Name == name {
			return r
		}
	}
	return nil
}
