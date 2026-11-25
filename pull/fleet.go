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
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"
)

// A Region is where machines start: its launch template and the (type, subnet) pools a fleet may choose from.
type Region struct {
	Name     string
	Weight   int
	ec2      *ec2.Client
	template string
	version  string
	pools    []ec2types.FleetLaunchTemplateOverridesRequest
}

// Fleet launches and ends the scale set's machines. Every instance carries the tags purpose=superci-pull and
// superci-pull-set=<set>, which are all the reconciler needs to find them again.
type Fleet struct {
	set     string
	regions []*Region
	tags    []ec2types.Tag
}

type FleetConfig struct {
	Set         string
	Regions     map[string]int // name -> weight
	Types       []string
	ImageOwner  string
	ImageName   string
	ImagePin    string
	DiskGB      int32
	UserData    string
	SecurityGrp map[string]string // region -> runner security group
}

func NewFleet(ctx context.Context, c FleetConfig) (*Fleet, error) {
	f := &Fleet{set: c.Set,
		tags: []ec2types.Tag{{Key: aws.String("purpose"), Value: aws.String("superci-pull")}, {Key: aws.String("superci-pull-set"), Value: aws.String(c.Set)}}}
	names := make([]string, 0, len(c.Regions))
	for name := range c.Regions {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(name))
		if err != nil {
			return nil, err
		}
		r := &Region{Name: name, Weight: c.Regions[name], ec2: ec2.NewFromConfig(cfg)}
		image, root, err := r.image(ctx, c.ImageOwner, c.ImageName, c.ImagePin)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if err := r.ensureTemplate(ctx, f.templateName(), image, root, c, f.tags); err != nil {
			return nil, fmt.Errorf("%s template: %w", name, err)
		}
		if err := r.findPools(ctx, c.Types); err != nil {
			return nil, fmt.Errorf("%s pools: %w", name, err)
		}
		slog.Info("region ready", "region", name, "image", image, "template", r.template, "version", r.version, "pools", len(r.pools))
		f.regions = append(f.regions, r)
	}
	return f, nil
}

func (f *Fleet) templateName() string {
	return "superci-pull-" + strings.TrimPrefix(f.set, "superci-pull-")
}

// image is the image jobs boot, as superci picks it: the pinned build, or once its maker has deleted it the oldest
// newer build (out the longest), or with none newer the newest.
func (r *Region) image(ctx context.Context, owner, name, pin string) (id, root string, err error) {
	out, err := r.ec2.DescribeImages(ctx, &ec2.DescribeImagesInput{Owners: []string{owner, "self"},
		Filters: []ec2types.Filter{{Name: aws.String("name"), Values: []string{name}}, {Name: aws.String("state"), Values: []string{"available"}}}})
	if err != nil {
		return "", "", err
	}
	build := func(i ec2types.Image) string { n := aws.ToString(i.Name); return n[strings.LastIndexByte(n, '-')+1:] }
	imgs := out.Images
	if len(imgs) == 0 {
		return "", "", fmt.Errorf("no image %s/%s", owner, name)
	}
	slices.SortFunc(imgs, func(a, b ec2types.Image) int { return strings.Compare(build(a), build(b)) })
	pick := imgs[len(imgs)-1]
	for _, i := range imgs {
		if build(i) >= pin {
			pick = i
			break
		}
	}
	return aws.ToString(pick.ImageId), aws.ToString(pick.RootDeviceName), nil
}

func (r *Region) ensureTemplate(ctx context.Context, name, image, root string, c FleetConfig, tags []ec2types.Tag) error {
	data := &ec2types.RequestLaunchTemplateData{
		ImageId:                           aws.String(image),
		UserData:                          aws.String(base64.StdEncoding.EncodeToString([]byte(c.UserData))),
		SecurityGroupIds:                  []string{c.SecurityGrp[r.Name]},
		InstanceInitiatedShutdownBehavior: ec2types.ShutdownBehaviorTerminate,
		MetadataOptions: &ec2types.LaunchTemplateInstanceMetadataOptionsRequest{HttpTokens: ec2types.LaunchTemplateHttpTokensStateRequired,
			HttpPutResponseHopLimit: aws.Int32(2), HttpEndpoint: ec2types.LaunchTemplateInstanceMetadataEndpointStateEnabled},
		BlockDeviceMappings: []ec2types.LaunchTemplateBlockDeviceMappingRequest{{DeviceName: aws.String(root), Ebs: &ec2types.LaunchTemplateEbsBlockDeviceRequest{
			VolumeSize: aws.Int32(c.DiskGB), VolumeType: ec2types.VolumeTypeGp3, Iops: aws.Int32(3000), Throughput: aws.Int32(250), DeleteOnTermination: aws.Bool(true)}}},
		TagSpecifications: []ec2types.LaunchTemplateTagSpecificationRequest{{ResourceType: ec2types.ResourceTypeInstance, Tags: tags}, {ResourceType: ec2types.ResourceTypeVolume, Tags: tags}},
	}
	got, err := r.ec2.DescribeLaunchTemplates(ctx, &ec2.DescribeLaunchTemplatesInput{LaunchTemplateNames: []string{name}})
	if err == nil && len(got.LaunchTemplates) == 1 {
		r.template = aws.ToString(got.LaunchTemplates[0].LaunchTemplateId)
		v, err := r.ec2.CreateLaunchTemplateVersion(ctx, &ec2.CreateLaunchTemplateVersionInput{LaunchTemplateId: aws.String(r.template), LaunchTemplateData: data})
		if err != nil {
			return err
		}
		r.version = fmt.Sprint(aws.ToInt64(v.LaunchTemplateVersion.VersionNumber))
		return nil
	}
	made, err := r.ec2.CreateLaunchTemplate(ctx, &ec2.CreateLaunchTemplateInput{LaunchTemplateName: aws.String(name), LaunchTemplateData: data,
		TagSpecifications: []ec2types.TagSpecification{{ResourceType: ec2types.ResourceTypeLaunchTemplate, Tags: tags}}})
	if err != nil {
		return err
	}
	r.template = aws.ToString(made.LaunchTemplate.LaunchTemplateId)
	r.version = fmt.Sprint(aws.ToInt64(made.LaunchTemplate.LatestVersionNumber))
	return nil
}

// findPools pairs each machine type with the default subnet of every zone that offers it.
func (r *Region) findPools(ctx context.Context, types []string) error {
	subnets, err := r.ec2.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{Filters: []ec2types.Filter{{Name: aws.String("default-for-az"), Values: []string{"true"}}}})
	if err != nil {
		return err
	}
	byZone := map[string]string{}
	for _, s := range subnets.Subnets {
		byZone[aws.ToString(s.AvailabilityZone)] = aws.ToString(s.SubnetId)
	}
	offers, err := r.ec2.DescribeInstanceTypeOfferings(ctx, &ec2.DescribeInstanceTypeOfferingsInput{LocationType: ec2types.LocationTypeAvailabilityZone,
		Filters: []ec2types.Filter{{Name: aws.String("instance-type"), Values: types}}})
	if err != nil {
		return err
	}
	for _, o := range offers.InstanceTypeOfferings {
		if subnet, ok := byZone[aws.ToString(o.Location)]; ok {
			r.pools = append(r.pools, ec2types.FleetLaunchTemplateOverridesRequest{InstanceType: o.InstanceType, SubnetId: aws.String(subnet)})
		}
	}
	if len(r.pools) == 0 {
		return errors.New("no pools")
	}
	return nil
}

// chunk is the most capacity one CreateFleet asks for; a burst is a handful of these per region.
const chunk = 250

// Launch starts n machines: spot across every pool of every region, split by weight, each region's shortfall asked
// again of the others, and only then on-demand, at most odRoom of them. It returns what started.
func (f *Fleet) Launch(ctx context.Context, round string, n, odRoom int) []*Machine {
	var got []*Machine
	want := f.split(n)
	short := map[string]int{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, r := range f.regions {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m := f.ask(ctx, r, want[r.Name], ec2types.DefaultTargetCapacityTypeSpot, round, "spot")
			mu.Lock()
			got = append(got, m...)
			short[r.Name] = want[r.Name] - len(m)
			mu.Unlock()
		}()
	}
	wg.Wait()
	for _, r := range f.regions {
		for _, other := range f.regions {
			if other != r && short[r.Name] > 0 {
				m := f.ask(ctx, other, short[r.Name], ec2types.DefaultTargetCapacityTypeSpot, round, "spot-for-"+r.Name)
				got = append(got, m...)
				short[r.Name] -= len(m)
			}
		}
	}
	left := 0
	for _, k := range short {
		left += k
	}
	for _, r := range f.regions {
		if left <= 0 || odRoom <= 0 {
			break
		}
		m := f.ask(ctx, r, min(left, odRoom), ec2types.DefaultTargetCapacityTypeOnDemand, round, "on-demand")
		got = append(got, m...)
		left -= len(m)
		odRoom -= len(m)
	}
	if left > 0 {
		slog.Warn("launch short", "round", round, "asked", n, "short", left)
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
func (f *Fleet) ask(ctx context.Context, r *Region, n int, kind ec2types.DefaultTargetCapacityType, round, step string) []*Machine {
	if n <= 0 {
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
			sum := sha256.Sum256(fmt.Appendf(nil, "%s/%s/%s/%s/%d", f.set, round, step, r.Name, part))
			token := hex.EncodeToString(sum[:16])
			started := time.Now()
			ms, errs, err := f.createFleet(ctx, r, k, kind, token, round)
			slog.Info("fleet", "region", r.Name, "step", step, "round", round, "part", part, "asked", k, "got", len(ms),
				"ms", time.Since(started).Milliseconds(), "errors", errs, "err", errString(err), "token", token)
			mu.Lock()
			got = append(got, ms...)
			mu.Unlock()
		}(part, k)
	}
	wg.Wait()
	return got
}

func (f *Fleet) createFleet(ctx context.Context, r *Region, n int, kind ec2types.DefaultTargetCapacityType, token, round string) ([]*Machine, map[string]int, error) {
	tags := append([]ec2types.Tag{{Key: aws.String("superci-pull-round"), Value: aws.String(round)}}, f.tags...)
	in := &ec2.CreateFleetInput{
		Type:                        ec2types.FleetTypeInstant,
		ClientToken:                 aws.String(token),
		TargetCapacitySpecification: &ec2types.TargetCapacitySpecificationRequest{TotalTargetCapacity: aws.Int32(int32(n)), DefaultTargetCapacityType: kind},
		SpotOptions:                 &ec2types.SpotOptionsRequest{AllocationStrategy: ec2types.SpotAllocationStrategyPriceCapacityOptimized},
		OnDemandOptions:             &ec2types.OnDemandOptionsRequest{AllocationStrategy: ec2types.FleetOnDemandAllocationStrategyLowestPrice},
		LaunchTemplateConfigs: []ec2types.FleetLaunchTemplateConfigRequest{{
			LaunchTemplateSpecification: &ec2types.FleetLaunchTemplateSpecificationRequest{LaunchTemplateId: aws.String(r.template), Version: aws.String(r.version)},
			Overrides:                   r.pools,
		}},
		TagSpecifications: []ec2types.TagSpecification{{ResourceType: ec2types.ResourceTypeInstance, Tags: tags}, {ResourceType: ec2types.ResourceTypeFleet, Tags: f.tags}},
	}
	var out *ec2.CreateFleetOutput
	var err error
	for try := 0; try < 4; try++ {
		out, err = r.ec2.CreateFleet(ctx, in)
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
			ms = append(ms, &Machine{ID: id, Region: r.Name, Type: string(i.InstanceType), AZ: az, Lifecycle: lifecycle, Launched: now, Asked: now, Round: round})
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

// Describe lists the set's machines that are starting or running in a region.
func (f *Fleet) Describe(ctx context.Context, r *Region) ([]*Machine, error) {
	var out []*Machine
	p := ec2.NewDescribeInstancesPaginator(r.ec2, &ec2.DescribeInstancesInput{MaxResults: aws.Int32(1000), Filters: []ec2types.Filter{
		{Name: aws.String("tag:purpose"), Values: []string{"superci-pull"}},
		{Name: aws.String("tag:superci-pull-set"), Values: []string{f.set}},
		{Name: aws.String("instance-state-name"), Values: []string{"pending", "running"}},
	}})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, res := range page.Reservations {
			for _, i := range res.Instances {
				lifecycle := "on-demand"
				if i.InstanceLifecycle == ec2types.InstanceLifecycleTypeSpot {
					lifecycle = "spot"
				}
				az := ""
				if i.Placement != nil {
					az = aws.ToString(i.Placement.AvailabilityZone)
				}
				round := ""
				for _, t := range i.Tags {
					if aws.ToString(t.Key) == "superci-pull-round" {
						round = aws.ToString(t.Value)
					}
				}
				out = append(out, &Machine{ID: aws.ToString(i.InstanceId), Region: r.Name, IP: aws.ToString(i.PublicIpAddress),
					Type: string(i.InstanceType), AZ: az, Lifecycle: lifecycle, Launched: aws.ToTime(i.LaunchTime), Round: round})
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
			ids = ids[k:]
		}
	}
}

func (f *Fleet) Region(name string) *Region {
	for _, r := range f.regions {
		if r.Name == name {
			return r
		}
	}
	return nil
}
