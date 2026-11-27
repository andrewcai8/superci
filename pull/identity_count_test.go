package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
)

func listedIdentity(i int, status string, busy bool, labels ...string) map[string]any {
	ls := []map[string]any{{"name": "self-hosted"}}
	for _, l := range labels {
		ls = append(ls, map[string]any{"name": l})
	}
	return map[string]any{"id": i + 1, "name": fmt.Sprintf("superci-id-4cpu-%04d", i), "status": status, "busy": busy, "labels": ls}
}

func TestIdentitiesAreCountedPerDispatchSizeByTheirLabelAndStatus(t *testing.T) {
	runners := []IdentityRunner{
		{Name: "superci-id-4cpu-0001", Status: "online", Busy: true, Labels: []string{"self-hosted", "superci-4cpu"}},
		{Name: "superci-id-4cpu-0002", Status: "online", Labels: []string{"superci-4cpu"}},
		{Name: "superci-id-4cpu-0003", Status: "offline", Labels: []string{"superci-4cpu"}},
		{Name: "superci-id-4cpu-0004", Status: "online", Labels: []string{"superci-park"}},
		{Name: "superci-id-8cpu-0001", Status: "online", Busy: true, Labels: []string{"superci-8cpu"}},
	}
	got := CountIdentities(runners, map[string]string{"superci-4cpu": "superci-4cpu", "superci-8cpu": "superci-8cpu", "superci-16cpu": "superci-16cpu"})
	want := []IdentityCount{{"superci-16cpu", 0, 0}, {"superci-4cpu", 2, 1}, {"superci-8cpu", 1, 1}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("counted %v, want %v: offline and parked identities serve no size, and a size with none still reports 0", got, want)
	}
}

func TestTheListenerCountsTheIdentitiesGroupInOneListingAndKeepsItFreshForThreeListings(t *testing.T) {
	f := newFakeREST(t)
	f.groupList = `{"runner_groups":[{"id":9,"name":"dep-identities"}]}`
	for i := 0; i < 130; i++ {
		switch {
		case i < 100:
			f.runners = append(f.runners, listedIdentity(i, "online", i < 90, "superci-4cpu"))
		default:
			f.runners = append(f.runners, listedIdentity(i, "online", false, "superci-park"))
		}
	}
	ids := NewIdentities(f.rest(), spare(), Config{Deployment: "dep", Repositories: []string{"r1"}})
	l := &Listener{dsets: []*DispatchSet{{spec: SetSpec{Name: "superci-4cpu", Label: "superci-4cpu", Dispatch: true}}}}
	l.countIdentities(context.Background(), ids)
	if n := f.called("GET /orgs/o/actions/runner-groups/9/runners"); n != 2 {
		t.Errorf("130 runners are two pages and one listing: %d calls: %v", n, f.calls)
	}
	now := time.Now()
	got := l.identityCounts(now)
	if len(got) != 1 || got[0].Online != 100 || got[0].Busy != 90 {
		t.Fatalf("counts %+v, want 100 online and 90 busy of superci-4cpu", got)
	}
	if got := l.identityCounts(now.Add(identityCountFresh - time.Second)); len(got) != 1 {
		t.Errorf("a count three listings old is still the last one read: %v", got)
	}
	if got := l.identityCounts(now.Add(identityCountFresh + time.Second)); got != nil {
		t.Errorf("a count older than three listings is stale and is not published: %v", got)
	}
	if got := (&Listener{}).identityCounts(now); got != nil {
		t.Errorf("before any listing there is no count: %v", got)
	}
}

func TestAListingThatFailsKeepsTheLastCountAndLogsIt(t *testing.T) {
	f := newFakeREST(t)
	f.groupList = `{"runner_groups":[{"id":9,"name":"dep-identities"}]}`
	f.runners = append(f.runners, listedIdentity(0, "online", true, "superci-4cpu"))
	ids := NewIdentities(f.rest(), spare(), Config{Deployment: "dep", Repositories: []string{"r1"}})
	l := &Listener{dsets: []*DispatchSet{{spec: SetSpec{Name: "superci-4cpu", Label: "superci-4cpu", Dispatch: true}}}}
	l.countIdentities(context.Background(), ids)
	f.groupList = `not json`
	buf := logs(t)
	l.countIdentities(context.Background(), ids)
	if got := l.identityCounts(time.Now()); len(got) != 1 || got[0].Online != 1 {
		t.Errorf("a failed listing replaced the count: %v", got)
	}
	if !strings.Contains(buf.String(), "count identities") {
		t.Errorf("the failure is not logged: %s", buf.String())
	}
}

func TestIdentitiesOnlineAndBusyArePublishedPerSizeUnderTheSetDimension(t *testing.T) {
	m := Metrics{Identities: []IdentityCount{{"superci-4cpu", 100, 90}, {"superci-8cpu", 0, 0}}, SetWaits: map[string][]float64{}, Denied: map[string]int{}}
	got := map[string]float64{}
	for _, d := range metricData(m, t0) {
		name := aws.ToString(d.MetricName)
		if name != "IdentitiesOnline" && name != "IdentitiesBusy" {
			continue
		}
		if len(d.Dimensions) != 1 || aws.ToString(d.Dimensions[0].Name) != "Set" {
			t.Fatalf("%s has dimensions %v, want the Set", name, d.Dimensions)
		}
		got[name+" "+aws.ToString(d.Dimensions[0].Value)] = aws.ToFloat64(d.Value)
	}
	want := map[string]float64{"IdentitiesOnline superci-4cpu": 100, "IdentitiesBusy superci-4cpu": 90, "IdentitiesOnline superci-8cpu": 0, "IdentitiesBusy superci-8cpu": 0}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("published %v, want %v", got, want)
	}
	for _, d := range metricData(Metrics{SetWaits: map[string][]float64{}, Denied: map[string]int{}}, t0) {
		if n := aws.ToString(d.MetricName); n == "IdentitiesOnline" || n == "IdentitiesBusy" {
			t.Errorf("no count was read, and %s is published", n)
		}
	}
}

// A size whose identities are all offline or parked is the case identity-none exists for: its count must be published as 0, not left out.
func TestASizeWhoseIdentitiesAreAllOfflineOrParkedIsPublishedAsZeroOnline(t *testing.T) {
	f := newFakeREST(t)
	f.groupList = `{"runner_groups":[{"id":9,"name":"dep-identities"}]}`
	f.runners = append(f.runners, listedIdentity(0, "offline", false, "superci-4cpu"), listedIdentity(1, "online", false, "superci-park"), listedIdentity(2, "offline", false, "superci-4cpu"))
	ids := NewIdentities(f.rest(), spare(), Config{Deployment: "dep", Repositories: []string{"r1"}})
	l := &Listener{dsets: []*DispatchSet{{spec: SetSpec{Name: "superci-4cpu", Label: "superci-4cpu", Dispatch: true}}}}
	l.countIdentities(context.Background(), ids)
	published := map[string]float64{}
	for _, d := range metricData(Metrics{Identities: l.identityCounts(time.Now()), SetWaits: map[string][]float64{}, Denied: map[string]int{}}, t0) {
		if n := aws.ToString(d.MetricName); n == "IdentitiesOnline" || n == "IdentitiesBusy" {
			published[n+" "+aws.ToString(d.Dimensions[0].Value)] = aws.ToFloat64(d.Value)
		}
	}
	want := map[string]float64{"IdentitiesOnline superci-4cpu": 0, "IdentitiesBusy superci-4cpu": 0}
	if fmt.Sprint(published) != fmt.Sprint(want) {
		t.Errorf("published %v, want %v: a size with no identity online reports 0, so identity-none can fire", published, want)
	}
}
