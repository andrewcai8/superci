package main

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/actions/scaleset"
)

func loadOverride(t *testing.T, override string) (Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(override), 0o600); err != nil {
		t.Fatal(err)
	}
	return LoadConfig(path)
}

func TestTheSetsRegisterAtTheRepositoryOnlyWhenTheConfigSaysSoAndNamesOne(t *testing.T) {
	c, err := loadOverride(t, `{"scope": "repository"}`)
	if err != nil {
		t.Fatal(err)
	}
	if c.scopeURL() != "https://github.com/Authentic-Intelligence/megpt-mono" || c.repository() != "megpt-mono" {
		t.Fatalf("scope %q, repository %q: want megpt-mono's URL", c.scopeURL(), c.repository())
	}
	c, err = loadOverride(t, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if c.scopeURL() != "https://github.com/Authentic-Intelligence" || c.repository() != "" {
		t.Fatalf("default scope %q, repository %q: want the organization", c.scopeURL(), c.repository())
	}
	for _, bad := range []string{`{"scope": "repository", "repositories": ["megpt-mono", "other"]}`, `{"scope": "repo"}`} {
		if _, err := loadOverride(t, bad); err == nil {
			t.Fatalf("%s: accepted", bad)
		}
	}
}

// pathsGitHub answers every runner listing with one idle runner and records the paths asked.
type pathsGitHub struct {
	mu    sync.Mutex
	paths []string
}

func (f *pathsGitHub) RoundTrip(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	f.paths = append(f.paths, req.URL.Path)
	f.mu.Unlock()
	body := `{"total_count":1,"runners":[{"id":7,"name":"superci-4cpu-i-0abc","status":"online","busy":false}]}`
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
}

// At the repository, the runners and the id sequence the meter reads are the repository's: a listing of the
// organization's group would show none of the sets' runners, and the organization's ids are another bucket's.
func TestAtTheRepositoryTheListingAndTheHeadAreTheRepositorys(t *testing.T) {
	for _, tc := range []struct {
		repo          string
		runners, head string
	}{
		{"", "/orgs/o/actions/runner-groups/24/runners", "/orgs/o/actions/runners"},
		{"r", "/repos/o/r/actions/runners", "/repos/o/r/actions/runners"},
	} {
		gh := &pathsGitHub{}
		rest := &REST{org: "o", repo: tc.repo, http: &http.Client{Transport: gh}, token: "t", expires: time.Now().Add(time.Hour)}
		got, _, err := rest.Runners(context.Background(), 24, "superci-")
		if err != nil || len(got) != 1 {
			t.Fatalf("repo %q: listed %v, %v", tc.repo, got, err)
		}
		if _, err := rest.OrgHead(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(gh.paths) != 2 || gh.paths[0] != tc.runners || gh.paths[1] != tc.head {
			t.Fatalf("repo %q: asked %v, want %s then %s", tc.repo, gh.paths, tc.runners, tc.head)
		}
	}
}

type fakeOrgSets struct {
	sets    []scaleset.RunnerScaleSet
	deleted []int
}

func (f *fakeOrgSets) ListRunnerScaleSets(context.Context, int) ([]scaleset.RunnerScaleSet, error) {
	return f.sets, nil
}

func (f *fakeOrgSets) DeleteRunnerScaleSet(_ context.Context, id int) error {
	f.deleted = append(f.deleted, id)
	return nil
}

type fakeOrgGroup struct {
	id      int
	runners map[string]ListedRunner
}

func (f fakeOrgGroup) orgGroupID(context.Context, string) (int, error) { return f.id, nil }

func (f fakeOrgGroup) OrgGroupRunners(context.Context, int, string) (map[string]ListedRunner, error) {
	return f.runners, nil
}

// Moving to the repository deletes the organization's sets of this deployment, which would otherwise take a share of
// their labels' jobs with no listener, and only once none of their runners holds a job.
func TestMovingToTheRepositoryRetiresTheOrganizationsSetsOnlyWhenNoneHoldsAJob(t *testing.T) {
	sets := []scaleset.RunnerScaleSet{{ID: 21, Name: "superci-4cpu"}, {ID: 22, Name: "superci-8cpu"}, {ID: 23, Name: "someone-elses"}}
	busy := &fakeOrgSets{sets: sets}
	err := retireOrgSets(context.Background(), busy, fakeOrgGroup{id: 24, runners: map[string]ListedRunner{
		"superci-4cpu-i-a": {Name: "superci-4cpu-i-a", Busy: true}, "superci-4cpu-i-b": {Name: "superci-4cpu-i-b"}}}, "superci-pull")
	if err == nil || len(busy.deleted) != 0 {
		t.Fatalf("a runner holds a job: err %v, deleted %v; want a refusal and nothing deleted", err, busy.deleted)
	}
	idle := &fakeOrgSets{sets: sets}
	if err := retireOrgSets(context.Background(), idle, fakeOrgGroup{id: 24, runners: map[string]ListedRunner{
		"superci-4cpu-i-b": {Name: "superci-4cpu-i-b"}}}, "superci-pull"); err != nil {
		t.Fatal(err)
	}
	if len(idle.deleted) != 2 || idle.deleted[0] != 21 || idle.deleted[1] != 22 {
		t.Fatalf("deleted %v: want the deployment's two sets and not another's", idle.deleted)
	}
	none := &fakeOrgSets{sets: sets}
	if err := retireOrgSets(context.Background(), none, fakeOrgGroup{}, "superci-pull"); err != nil || len(none.deleted) != 0 {
		t.Fatalf("no group at the organization: err %v, deleted %v", err, none.deleted)
	}
}
