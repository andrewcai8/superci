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
