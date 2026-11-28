package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"
)

const ourAccount = "243891825325"

// images stands in for EC2's DescribeImages: the images it knows by id, and the inputs it was asked with.
type images struct {
	byID  map[string]ec2types.Image
	asked []*ec2.DescribeImagesInput
	err   error // answered to every call, when set
}

func (m *images) DescribeImages(_ context.Context, in *ec2.DescribeImagesInput, _ ...func(*ec2.Options)) (*ec2.DescribeImagesOutput, error) {
	m.asked = append(m.asked, in)
	if m.err != nil {
		return nil, m.err
	}
	out := &ec2.DescribeImagesOutput{}
	for _, id := range in.ImageIds {
		img, ok := m.byID[id]
		if !ok {
			return nil, &smithy.GenericAPIError{Code: "InvalidAMIID.NotFound", Message: "The image id '[" + id + "]' does not exist"}
		}
		out.Images = append(out.Images, img)
	}
	if len(in.ImageIds) == 0 { // a listing by filter: every image, as a fallback by name would have found them
		for _, img := range m.byID {
			out.Images = append(out.Images, img)
		}
	}
	return out, nil
}

func image(id, owner string, state ec2types.ImageState) ec2types.Image {
	return ec2types.Image{ImageId: aws.String(id), OwnerId: aws.String(owner), State: state, RootDeviceName: aws.String("/dev/sda1"),
		Name: aws.String("runs-on-v2.2-ubuntu24-full-x64-20261120075252")}
}

func TestThePinnedImageIsFoundAndNothingElseIsAsked(t *testing.T) {
	api := &images{byID: map[string]ec2types.Image{
		"ami-0pinned": image("ami-0pinned", ourAccount, ec2types.ImageStateAvailable),
		"ami-0newer":  image("ami-0newer", ourAccount, ec2types.ImageStateAvailable), // a newer build of ours is not the pin
	}}
	id, root, err := findImage(context.Background(), api, ourAccount, "ami-0pinned")
	if err != nil || id != "ami-0pinned" || root != "/dev/sda1" {
		t.Fatalf("findImage = %q, %q, %v; want the pinned image and its root device", id, root, err)
	}
	if len(api.asked) != 1 || len(api.asked[0].ImageIds) != 1 || api.asked[0].ImageIds[0] != "ami-0pinned" || len(api.asked[0].Filters) != 0 || len(api.asked[0].Owners) != 0 {
		t.Fatalf("EC2 was asked %+v, want the pinned id alone: no name pattern, no owner list a newer build could answer", api.asked)
	}
}

// Fail closed: the pin is gone, so the region has no image. It is never given another build, however new.
func TestAMissingPinIsRefusedAndTheRegionLaunchesNothing(t *testing.T) {
	api := &images{byID: map[string]ec2types.Image{"ami-0other": image("ami-0other", ourAccount, ec2types.ImageStateAvailable)}}
	_, _, err := findImage(context.Background(), api, ourAccount, "ami-0gone")
	var refusal *imageRefusal
	if !errors.As(err, &refusal) || !strings.Contains(err.Error(), "ami-0gone") || !strings.Contains(err.Error(), "InvalidAMIID.NotFound") {
		t.Fatalf("a pin EC2 does not know gave %v, want an imageRefusal naming it", err)
	}
	f, sf := refusedFleet(t, refusal.Error())
	if f.Refused() != 2 {
		t.Fatalf("Refused() = %d, want both regions", f.Refused())
	}
	got := sf.Launch(context.Background(), "r1", 6, 6)
	if len(got) != 0 {
		t.Fatalf("a fleet whose images were refused launched %d machines", len(got))
	}
	for _, r := range f.regions {
		if asked := r.fleets.(*capacity).asked; len(asked) != 0 {
			t.Fatalf("%s was asked for machines: %v", r.Name, asked)
		}
	}
	if len(sf.regions) != 0 || len(sf.Versions()) != 0 {
		t.Fatalf("a refused region got a launch template: %v", sf.regions)
	}
	data := metricData(Metrics{ImageRefused: f.Refused(), SetWaits: map[string][]float64{}, Denied: map[string]int{}}, t0)
	if v := aws.ToFloat64(datum(data, "ImageRefused").Value); v != 2 {
		t.Fatalf("ImageRefused = %v, want 2: the alarm that pages reads it", v)
	}
	data = metricData(Metrics{SetWaits: map[string][]float64{}, Denied: map[string]int{}}, t0)
	if d := datum(data, "ImageRefused"); d == nil || aws.ToFloat64(d.Value) != 0 {
		t.Fatalf("a listener with every image published %v, want ImageRefused 0 (an alarm needs the 0 to clear)", d)
	}
}

// One region's refusal moves what it was to start to the other region: only the refused region goes quiet.
func TestARefusedRegionLaunchesNothingAndTheOtherOneAsksForItsShare(t *testing.T) {
	plenty := map[string]int{"m8a.2xlarge/on-demand": 100, "r8a.2xlarge/on-demand": 100, "m8azn.3xlarge/on-demand": 100}
	sf := launchSet(t, "superci-8cpu", plenty, maps(plenty))
	sf.fleet.regions[0].refused = "pinned image \"ami-0gone\" refused: gone"
	got := sf.Launch(context.Background(), "r1", 6, 6)
	if len(got) != 6 {
		t.Fatalf("started %d machines, want the 6 asked for from the other region", len(got))
	}
	if asked := sf.fleet.regions[0].fleets.(*capacity).asked; len(asked) != 0 {
		t.Fatalf("the refused region was asked for machines: %v", asked)
	}
	if asked := sf.fleet.regions[1].fleets.(*capacity).asked; len(asked) == 0 {
		t.Fatal("the other region was never asked")
	}
	if sf.fleet.Refused() != 1 {
		t.Fatalf("Refused() = %d, want 1", sf.fleet.Refused())
	}
}

func TestAPinOwnedBySomeoneElseIsRefused(t *testing.T) {
	// RunsOn's own build, which they may delete: the pin must be our copy.
	api := &images{byID: map[string]ec2types.Image{"ami-0runson": image("ami-0runson", "135269210855", ec2types.ImageStateAvailable)}}
	_, _, err := findImage(context.Background(), api, ourAccount, "ami-0runson")
	var refusal *imageRefusal
	if !errors.As(err, &refusal) || !strings.Contains(err.Error(), "owned by 135269210855") {
		t.Fatalf("a pin another account owns gave %v, want a refusal naming its owner", err)
	}
}

func TestAPinThatIsNotAvailableOrIsUnsetIsRefused(t *testing.T) {
	api := &images{byID: map[string]ec2types.Image{
		"ami-0pending": image("ami-0pending", ourAccount, ec2types.ImageStatePending),
		"ami-0failed":  image("ami-0failed", ourAccount, ec2types.ImageStateFailed),
	}}
	for _, pin := range []string{"ami-0pending", "ami-0failed", ""} {
		_, _, err := findImage(context.Background(), api, ourAccount, pin)
		var refusal *imageRefusal
		if !errors.As(err, &refusal) {
			t.Errorf("pin %q gave %v, want an imageRefusal", pin, err)
		}
	}
}

// A throttle or an outage is not a verdict on the image: the listener does not start, as it did before, and systemd retries.
func TestAnEC2ErrorIsNotARefusal(t *testing.T) {
	api := &images{err: &smithy.GenericAPIError{Code: "RequestLimitExceeded", Message: "slow down"}}
	_, _, err := findImage(context.Background(), api, ourAccount, "ami-0pinned")
	var refusal *imageRefusal
	if err == nil || errors.As(err, &refusal) {
		t.Fatalf("a throttled call gave %v, want the error itself", err)
	}
}

// refusedFleet is a fleet whose regions both had their image refused, with a set added to it.
func refusedFleet(t *testing.T, why string) (*Fleet, *SetFleet) {
	t.Helper()
	c, err := LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	f := &Fleet{deployment: "superci-pull"}
	for i, name := range []string{"us-east-1", "us-east-2"} {
		f.regions = append(f.regions, &Region{Name: name, Weight: 2 - i, fleets: &capacity{left: map[string]int{"m8a.2xlarge/on-demand": 100, "m8a.2xlarge/spot": 100}}, refused: why})
	}
	var spec SetSpec
	for _, s := range c.Sets() {
		if s.Name == "superci-8cpu" {
			spec = s
		}
	}
	sf, err := f.AddSet(context.Background(), spec, "#!/bin/sh", nil)
	if err != nil {
		t.Fatalf("a set added to refused regions must start nothing, and no EC2 call is made for it: %v", err)
	}
	return f, sf
}

func TestTheBuiltInConfigPinsAnImageForEveryRegion(t *testing.T) {
	c, err := LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	for r := range c.Regions {
		if c.Image[r] == "" {
			t.Errorf("region %s has no image pin", r)
		}
	}
	c.Image = map[string]string{"us-east-1": "ami-0123456789abcdef0"}
	if err := c.validate(); err == nil || !strings.Contains(err.Error(), "us-east-2") {
		t.Fatalf("validate with no pin for us-east-2 = %v, want a refusal naming the region", err)
	}
	c.Image["us-east-2"] = "runs-on-v2.2-ubuntu24-full-x64-20261120075252"
	if err := c.validate(); err == nil {
		t.Fatal("a pin that is a build name, not an AMI id, passed validation")
	}
}

// The pin travels in the binary the release builds and deploy/parity.sh reads from the same commit's config.json, so an
// override file (a different file, shipped on its own) cannot move the image a parity check did not boot.
func TestTheOverrideCannotMoveTheImagePin(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"image": {"us-east-1": "ami-0123456789abcdef0"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "pinned in the built-in config.json") {
		t.Fatalf("an override that changes the image loaded: %v", err)
	}
	builtin, err := LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	same := `{"image": {"us-east-1": "` + builtin.Image["us-east-1"] + `"}}`
	if err := os.WriteFile(path, []byte(same), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err != nil {
		t.Fatalf("an override that repeats the pin is a no-op, got %v", err)
	}
}

// The placeholders (ami-REPLACE-<region>) stand for the copies the operator makes (README, "Moving the image pin"). This test
// fails the build, and deploy/parity.sh refuses the release, while one is left in config.json: a placeholder cannot ship.
func TestNoImagePlaceholderShips(t *testing.T) {
	c, err := LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	real := regexp.MustCompile(`^ami-[0-9a-f]{8}([0-9a-f]{9})?$`)
	for r, id := range c.Image {
		if !real.MatchString(id) {
			t.Errorf("config.json pins %q for %s, which is not an AMI id: make the copy (README, Moving the image pin) and put its id here", id, r)
		}
	}
}
