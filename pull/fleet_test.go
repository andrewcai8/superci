package main

import (
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

func templatePair() (*ec2types.ResponseLaunchTemplateData, *ec2types.RequestLaunchTemplateData) {
	have := &ec2types.ResponseLaunchTemplateData{
		ImageId: aws.String("ami-1"), UserData: aws.String("data"), SecurityGroupIds: []string{"sg-1"},
		IamInstanceProfile: &ec2types.LaunchTemplateIamInstanceProfileSpecification{Arn: aws.String("arn:aws:iam::243891825325:instance-profile/superci-pull-cache-machine")},
		MetadataOptions:    &ec2types.LaunchTemplateInstanceMetadataOptions{HttpTokens: ec2types.LaunchTemplateHttpTokensStateRequired, HttpPutResponseHopLimit: aws.Int32(1)},
		BlockDeviceMappings: []ec2types.LaunchTemplateBlockDeviceMapping{{DeviceName: aws.String("/dev/sda1"),
			Ebs: &ec2types.LaunchTemplateEbsBlockDevice{VolumeSize: aws.Int32(30), Throughput: aws.Int32(250)}}},
	}
	want := &ec2types.RequestLaunchTemplateData{
		ImageId: aws.String("ami-1"), UserData: aws.String("data"), SecurityGroupIds: []string{"sg-1"},
		IamInstanceProfile: &ec2types.LaunchTemplateIamInstanceProfileSpecificationRequest{Name: aws.String("superci-pull-cache-machine")},
		MetadataOptions:    &ec2types.LaunchTemplateInstanceMetadataOptionsRequest{HttpTokens: ec2types.LaunchTemplateHttpTokensStateRequired, HttpPutResponseHopLimit: aws.Int32(1)},
		BlockDeviceMappings: []ec2types.LaunchTemplateBlockDeviceMappingRequest{{DeviceName: aws.String("/dev/sda1"),
			Ebs: &ec2types.LaunchTemplateEbsBlockDeviceRequest{VolumeSize: aws.Int32(30), Throughput: aws.Int32(250)}}},
	}
	return have, want
}

func TestATemplateVersionWithTheCacheProfileIsTheSame(t *testing.T) {
	have, want := templatePair()
	if !sameData(have, want) {
		t.Fatal("a version that names the profile by its ARN differs from one that asks for it by name")
	}
}

func TestATemplateVersionWithoutTheProfileGetsANewVersion(t *testing.T) {
	have, want := templatePair()
	have.IamInstanceProfile = nil
	if sameData(have, want) {
		t.Fatal("a version without a profile was kept, so no machine would get the role")
	}
	have.IamInstanceProfile = &ec2types.LaunchTemplateIamInstanceProfileSpecification{Arn: aws.String("arn:aws:iam::243891825325:instance-profile/other")}
	if sameData(have, want) {
		t.Fatal("a version with another profile was kept")
	}
}

func TestPoolsRankTheirTypesInTheSizesOrder(t *testing.T) {
	offers := []ec2types.InstanceTypeOffering{
		{InstanceType: "c8i.xlarge", Location: aws.String("us-east-1a")},
		{InstanceType: "c8a.xlarge", Location: aws.String("us-east-1a")},
		{InstanceType: "c8a.xlarge", Location: aws.String("us-east-1b")},
		{InstanceType: "c8a.xlarge", Location: aws.String("us-east-1z")},
	}
	pools := rankedPools(offers, map[string]string{"us-east-1a": "subnet-a", "us-east-1b": "subnet-b"}, []string{"c8a.xlarge", "c8i.xlarge"})
	got := map[string]float64{}
	for _, p := range pools {
		got[string(p.InstanceType)+" "+aws.ToString(p.SubnetId)] = aws.ToFloat64(p.Priority)
	}
	want := map[string]float64{"c8i.xlarge subnet-a": 1, "c8a.xlarge subnet-a": 0, "c8a.xlarge subnet-b": 0}
	if len(got) != len(want) {
		t.Fatalf("pools %v, want %v (a zone without a default subnet has no pool)", got, want)
	}
	for k, v := range want {
		if p, ok := got[k]; !ok || p != v {
			t.Fatalf("pool %s: priority %v, want %v (all: %v)", k, p, v, got)
		}
	}
}

// The root volume is gp3 at the size's IOPS and throughput, 250 MiB/s and 3,000 IOPS unless the size says more. The lab
// (m8azn.xlarge, the superci image, a 60 GB root, Nov 27) wrote 1 GiB with fdatasync in 3.2 to 4.1 s at 250 MiB/s,
// 1.2 to 2.0 s at 500, 0.6 to 1.3 s at 750 and 0.5 to 1.0 s at 1,000 (4,000 IOPS): Blacksmith's disk takes 0.5 to 0.6 s.
func TestTheRootVolumeIsTheSizesAndFourVCPUGetsTheMeasuredOne(t *testing.T) {
	c := shippedConfig(t)
	got := map[string][2]int32{}
	for _, spec := range c.Sets() {
		ebs := rootDevice("/dev/sda1", spec.DiskGB, spec)[0].Ebs
		if ebs.VolumeType != ec2types.VolumeTypeGp3 || aws.ToInt32(ebs.VolumeSize) != int32(spec.DiskGB) {
			t.Errorf("%s: root %v %d GB", spec.Name, ebs.VolumeType, aws.ToInt32(ebs.VolumeSize))
		}
		got[spec.Name] = [2]int32{aws.ToInt32(ebs.Iops), aws.ToInt32(ebs.Throughput)}
	}
	if got["superci-4cpu"] != [2]int32{4000, 1000} {
		t.Errorf("superci-4cpu root is %v IOPS and MiB/s, want 4000 and 1000", got["superci-4cpu"])
	}
	for _, name := range []string{"superci-2cpu", "superci-8cpu", "superci-16cpu", "superci-32cpu"} {
		if got[name] != [2]int32{3000, 250} {
			t.Errorf("%s root is %v, want the defaults 3000 and 250: it was not measured", name, got[name])
		}
	}
}

// A template version whose IOPS differ is a new version: machines of the size would otherwise keep the old volume.
func TestATemplateVersionWithOtherIOPSGetsANewVersion(t *testing.T) {
	have, want := templatePair()
	have.BlockDeviceMappings[0].Ebs.Iops = aws.Int32(3000)
	want.BlockDeviceMappings[0].Ebs.Iops = aws.Int32(3000)
	if !sameData(have, want) {
		t.Fatal("versions with the same IOPS differ")
	}
	want.BlockDeviceMappings[0].Ebs.Iops = aws.Int32(4000)
	if sameData(have, want) {
		t.Fatal("a version with other IOPS was kept, so the size's machines keep the old volume")
	}
}

// EC2 refuses a gp3 volume past its limits, which would fail the listener's start: the config refuses it first.
func TestAVolumeEC2WouldRefuseIsRefusedByTheConfig(t *testing.T) {
	for name, tc := range map[string]struct {
		mbps, iops int
		ok         bool
	}{
		"the defaults":                        {0, 0, true},
		"the measured 1000 MiB/s, 4000 IOPS":  {1000, 4000, true},
		"1000 MiB/s with the default IOPS":    {1000, 0, false},
		"throughput over 1000":                {1001, 16000, false},
		"throughput under 125":                {100, 3000, false},
		"IOPS over 16000":                     {250, 16001, false},
		"500 MiB/s needs 2000 IOPS, has 3000": {500, 3000, true},
		"a negative value":                    {-1, 0, false},
	} {
		c, err := LoadConfig("")
		if err != nil {
			t.Fatal(err)
		}
		s := c.Sizes["superci-8cpu"]
		s.DiskMBps, s.DiskIOPS = tc.mbps, tc.iops
		c.Sizes["superci-8cpu"] = s
		if err := c.validate(); (err == nil) != tc.ok {
			t.Errorf("%s: validate says %v, want ok=%v", name, err, tc.ok)
		}
	}
}

// gp3 allows 500 IOPS for each GiB: 4,000 IOPS on a 6 GB root is a launch template EC2 refuses, which would fail the listener's start.
func TestIOPSPastWhatTheDiskSizeAllowsIsRefused(t *testing.T) {
	c, err := LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	s := c.Sizes["superci-4cpu"]
	s.DiskMBps, s.DiskIOPS, s.DiskGB = 1000, 4000, 8
	c.Sizes["superci-4cpu"] = s
	if err := c.validate(); err != nil {
		t.Fatalf("4,000 IOPS on 8 GB is 500 for each GiB, which gp3 allows: %v", err)
	}
	s.DiskGB = 7
	c.Sizes["superci-4cpu"] = s
	if err := c.validate(); err == nil {
		t.Fatal("4,000 IOPS on a 7 GB root passed: gp3 allows 500 for each GiB")
	}
}

// The spend prices the volume a size asks for: 1000 MiB/s and 4000 IOPS is 0.0548 USD an hour beyond storage against
// 0.0068 for the defaults, 0.048 more per machine-hour.
func TestTheSpendPricesTheVolumeASizeAsksFor(t *testing.T) {
	c := shippedConfig(t)
	sp := NewSpend(0, 0, c.Sets(), t0)
	m := Machine{Set: "superci-4cpu", Lifecycle: "on-demand"}
	four := sp.Price(m)
	c4 := c.Sizes["superci-4cpu"]
	c4.DiskMBps, c4.DiskIOPS = 0, 0
	c.Sizes["superci-4cpu"] = c4
	plain := NewSpend(0, 0, c.Sets(), t0).Price(m)
	if d := four - plain; d < 0.0479 || d > 0.0481 {
		t.Fatalf("the volume adds %.5f USD an hour over the defaults, want 0.0480", d)
	}
}
