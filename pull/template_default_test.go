package main

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// templates is EC2's launch templates as ensureTemplate uses them: each template keeps its versions, a default and a
// log of the calls that wrote.
type templates struct {
	byName map[string]*fakeTemplate
	wrote  []string
}

type fakeTemplate struct {
	id       string
	versions []*ec2types.ResponseLaunchTemplateData // version n is versions[n-1]
	def      int64
}

func (m *templates) DescribeLaunchTemplates(_ context.Context, in *ec2.DescribeLaunchTemplatesInput, _ ...func(*ec2.Options)) (*ec2.DescribeLaunchTemplatesOutput, error) {
	out := &ec2.DescribeLaunchTemplatesOutput{}
	if t := m.byName[in.LaunchTemplateNames[0]]; t != nil {
		out.LaunchTemplates = []ec2types.LaunchTemplate{{LaunchTemplateId: aws.String(t.id), LaunchTemplateName: aws.String(in.LaunchTemplateNames[0]),
			DefaultVersionNumber: aws.Int64(t.def), LatestVersionNumber: aws.Int64(int64(len(t.versions)))}}
	}
	return out, nil
}

func (m *templates) byID(id string) *fakeTemplate {
	for _, t := range m.byName {
		if t.id == id {
			return t
		}
	}
	panic("no template " + id)
}

func (m *templates) DescribeLaunchTemplateVersions(_ context.Context, in *ec2.DescribeLaunchTemplateVersionsInput, _ ...func(*ec2.Options)) (*ec2.DescribeLaunchTemplateVersionsOutput, error) {
	t := m.byID(aws.ToString(in.LaunchTemplateId))
	if len(in.Versions) != 1 || in.Versions[0] != "$Latest" {
		panic(fmt.Sprintf("versions %v: the fake answers $Latest only", in.Versions))
	}
	n := len(t.versions)
	return &ec2.DescribeLaunchTemplateVersionsOutput{LaunchTemplateVersions: []ec2types.LaunchTemplateVersion{{VersionNumber: aws.Int64(int64(n)), LaunchTemplateData: t.versions[n-1]}}}, nil
}

// responseOf is what EC2 keeps of a request's data (the fields sameData reads).
func responseOf(r *ec2types.RequestLaunchTemplateData) *ec2types.ResponseLaunchTemplateData {
	d := r.BlockDeviceMappings[0]
	return &ec2types.ResponseLaunchTemplateData{ImageId: r.ImageId, UserData: r.UserData, SecurityGroupIds: r.SecurityGroupIds,
		IamInstanceProfile: &ec2types.LaunchTemplateIamInstanceProfileSpecification{Arn: aws.String("arn:aws:iam::243891825325:instance-profile/" + aws.ToString(r.IamInstanceProfile.Name))},
		MetadataOptions:    &ec2types.LaunchTemplateInstanceMetadataOptions{HttpTokens: ec2types.LaunchTemplateHttpTokensState(r.MetadataOptions.HttpTokens), HttpPutResponseHopLimit: r.MetadataOptions.HttpPutResponseHopLimit},
		BlockDeviceMappings: []ec2types.LaunchTemplateBlockDeviceMapping{{DeviceName: d.DeviceName,
			Ebs: &ec2types.LaunchTemplateEbsBlockDevice{VolumeSize: d.Ebs.VolumeSize, Throughput: d.Ebs.Throughput, Iops: d.Ebs.Iops}}}}
}

func (m *templates) CreateLaunchTemplate(_ context.Context, in *ec2.CreateLaunchTemplateInput, _ ...func(*ec2.Options)) (*ec2.CreateLaunchTemplateOutput, error) {
	name := aws.ToString(in.LaunchTemplateName)
	m.wrote = append(m.wrote, "create "+name)
	m.byName[name] = &fakeTemplate{id: "lt-" + name, versions: []*ec2types.ResponseLaunchTemplateData{responseOf(in.LaunchTemplateData)}, def: 1}
	return &ec2.CreateLaunchTemplateOutput{LaunchTemplate: &ec2types.LaunchTemplate{LaunchTemplateId: aws.String("lt-" + name), LatestVersionNumber: aws.Int64(1), DefaultVersionNumber: aws.Int64(1)}}, nil
}

func (m *templates) CreateLaunchTemplateVersion(_ context.Context, in *ec2.CreateLaunchTemplateVersionInput, _ ...func(*ec2.Options)) (*ec2.CreateLaunchTemplateVersionOutput, error) {
	t := m.byID(aws.ToString(in.LaunchTemplateId))
	t.versions = append(t.versions, responseOf(in.LaunchTemplateData))
	m.wrote = append(m.wrote, fmt.Sprintf("version %d of %s", len(t.versions), t.id))
	return &ec2.CreateLaunchTemplateVersionOutput{LaunchTemplateVersion: &ec2types.LaunchTemplateVersion{VersionNumber: aws.Int64(int64(len(t.versions)))}}, nil
}

func (m *templates) ModifyLaunchTemplate(_ context.Context, in *ec2.ModifyLaunchTemplateInput, _ ...func(*ec2.Options)) (*ec2.ModifyLaunchTemplateOutput, error) {
	t := m.byID(aws.ToString(in.LaunchTemplateId))
	var n int64
	fmt.Sscan(aws.ToString(in.DefaultVersion), &n)
	t.def = n
	m.wrote = append(m.wrote, fmt.Sprintf("default %d of %s", n, t.id))
	return &ec2.ModifyLaunchTemplateOutput{}, nil
}

func templateSpec(t *testing.T) SetSpec {
	t.Helper()
	for _, s := range shippedConfig(t).Sets() {
		if s.Name == "superci-4cpu" {
			return s
		}
	}
	t.Fatal("no superci-4cpu in the shipped config")
	return SetSpec{}
}

const pinID = "ami-0ec23d36a2f286d1f"

func ensure(t *testing.T, api *templates, userData string) *setRegion {
	t.Helper()
	sr := &setRegion{}
	r := &Region{Name: "us-east-1", image: pinID, root: "/dev/sda1"}
	if err := sr.ensureTemplate(context.Background(), api, r, "superci-pull-superci-4cpu", userData, "sg-1", 60, templateSpec(t), nil); err != nil {
		t.Fatal(err)
	}
	return sr
}

// Nov 28: a template's default stayed version 1, naming an image RunsOn deleted, while every start made a newer version the
// listener launched by number: a machine made from $Default (an operator's, a check's) could not start.
func TestAStartMakesTheVersionItLaunchesTheTemplatesDefault(t *testing.T) {
	api := &templates{byName: map[string]*fakeTemplate{}}
	ensure(t, api, "#!/bin/sh\nv1")
	api.byName["superci-pull-superci-4cpu"].versions[0].ImageId = aws.String("ami-0gone") // version 1 names a deleted image
	sr := ensure(t, api, "#!/bin/sh\nv2")                                                 // the machines' data changed: a new version
	tpl := api.byName["superci-pull-superci-4cpu"]
	if sr.version != "2" || len(tpl.versions) != 2 {
		t.Fatalf("version %s of %d, want a second version launched", sr.version, len(tpl.versions))
	}
	if tpl.def != 2 {
		t.Fatalf("$Default is version %d, want 2, the version the fleet launches (calls: %v)", tpl.def, api.wrote)
	}
}

// A start that finds the latest version already holds the machines' data makes none, and still repairs a default left behind.
func TestAStartThatReusesTheLatestVersionMovesALaggingDefaultToIt(t *testing.T) {
	api := &templates{byName: map[string]*fakeTemplate{}}
	ensure(t, api, "#!/bin/sh\nv1")
	tpl := api.byName["superci-pull-superci-4cpu"]
	ensure(t, api, "#!/bin/sh\nv2")
	tpl.def = 1 // an operator's, or an older listener's, default
	api.wrote = nil
	sr := ensure(t, api, "#!/bin/sh\nv2")
	if sr.version != "2" || len(tpl.versions) != 2 {
		t.Fatalf("version %s of %d, want the second reused", sr.version, len(tpl.versions))
	}
	if tpl.def != 2 {
		t.Fatalf("$Default is version %d, want 2 (calls: %v)", tpl.def, api.wrote)
	}
}

func TestAStartThatChangesNothingWritesNothing(t *testing.T) {
	api := &templates{byName: map[string]*fakeTemplate{}}
	ensure(t, api, "#!/bin/sh\nv1")
	api.wrote = nil
	ensure(t, api, "#!/bin/sh\nv1")
	if len(api.wrote) != 0 {
		t.Fatalf("a second start with nothing changed wrote %v", api.wrote)
	}
	// and a template that is new has its first version as its default: no call moves it.
	if tpl := api.byName["superci-pull-superci-4cpu"]; tpl.def != 1 {
		t.Fatalf("a new template's default is version %d", tpl.def)
	}
}

// A dev listener (its own deployment) used superci-pull-<set> too: its start wrote versions into the templates production
// launches from. A deployment's templates are its own, and production's names (deployment superci-pull) do not change.
func TestEachDeploymentHasItsOwnTemplates(t *testing.T) {
	if got := templateName("superci-pull", "superci-4cpu"); got != "superci-pull-superci-4cpu" {
		t.Fatalf("production's template is %q, want superci-pull-superci-4cpu, as it has been", got)
	}
	prod, timeline, v4d := templateName("superci-pull", "superci-96cpu"), templateName("superci-pull-timeline", "superci-96cpu"), templateName("superci-pull-v4d", "superci-96cpu")
	if prod == timeline || prod == v4d || timeline == v4d {
		t.Fatalf("one set of three deployments is one template: %q, %q, %q", prod, timeline, v4d)
	}
	for _, n := range []string{timeline, v4d} {
		if !strings.HasPrefix(n, "superci-pull-") {
			t.Errorf("%q is outside the listener role's superci-pull-* templates", n)
		}
	}
}
