package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/golang-jwt/jwt/v5"
)

// App is the GitHub App the scale set registers runners with, as superci keeps it.
type App struct {
	ID  int64  `json:"id"`
	PEM string `json:"pem"`
}

// LoadApp reads the App's credentials from SSM. They stay in memory.
func LoadApp(ctx context.Context, region, name string) (App, error) {
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
	if err != nil {
		return App{}, err
	}
	out, err := ssm.NewFromConfig(cfg).GetParameter(ctx, &ssm.GetParameterInput{Name: aws.String(name), WithDecryption: aws.Bool(true)})
	if err != nil {
		return App{}, err
	}
	var app App
	if err := json.Unmarshal([]byte(aws.ToString(out.Parameter.Value)), &app); err != nil {
		return App{}, fmt.Errorf("app credentials: %w", err)
	}
	return app, nil
}

// REST is the GitHub REST API as the App's installation, for what the scale set protocol does not offer: listing the
// runners of a runner group.
type REST struct {
	app          App
	installation int64
	org          string
	http         *http.Client
	mu           sync.Mutex
	token        string
	expires      time.Time
}

func NewREST(app App, installation int64, org string) *REST {
	return &REST{app: app, installation: installation, org: org, http: &http.Client{Timeout: 30 * time.Second}}
}

func (g *REST) tokenFor(ctx context.Context) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.token != "" && time.Until(g.expires) > 5*time.Minute {
		return g.token, nil
	}
	token, expires, err := g.mint(ctx, nil)
	if err != nil {
		return "", err
	}
	g.token, g.expires = token, expires
	return g.token, nil
}

// mint asks GitHub for an installation token, signing an App JWT in memory. A non-nil request narrows the token to the
// repositories and permissions it names (GitHub refuses a permission the App does not hold with 422, and says which).
func (g *REST) mint(ctx context.Context, narrow any) (string, time.Time, error) {
	signed, err := g.appJWT()
	if err != nil {
		return "", time.Time{}, err
	}
	var body io.Reader
	if narrow != nil {
		b, err := json.Marshal(narrow)
		if err != nil {
			return "", time.Time{}, err
		}
		body = bytes.NewReader(b)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("https://api.github.com/app/installations/%d/access_tokens", g.installation), body)
	req.Header.Set("Authorization", "Bearer "+signed)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := g.http.Do(req)
	if err != nil {
		return "", time.Time{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<10))
		return "", time.Time{}, &MintError{Status: resp.Status, Code: resp.StatusCode, Message: apiMessage(msg)}
	}
	var t struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&t); err != nil {
		return "", time.Time{}, err
	}
	if t.Token == "" {
		return "", time.Time{}, fmt.Errorf("installation token: GitHub answered no token")
	}
	return t.Token, t.ExpiresAt, nil
}

// appJWT is the App's short-lived JWT, signed in memory with its private key.
func (g *REST) appJWT() (string, error) {
	key, err := jwt.ParseRSAPrivateKeyFromPEM([]byte(g.app.PEM))
	if err != nil {
		return "", err
	}
	now := time.Now()
	return jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.RegisteredClaims{Issuer: strconv.FormatInt(g.app.ID, 10),
		IssuedAt: jwt.NewNumericDate(now.Add(-time.Minute)), ExpiresAt: jwt.NewNumericDate(now.Add(9 * time.Minute))}).SignedString(key)
}

// InstallationPermissions is what the installation was granted, permission name to "read", "write" or "admin", read with the
// App's JWT (GET /app/installations/<id>), which names a permission the narrowed mint would refuse before it is asked for.
func (g *REST) InstallationPermissions(ctx context.Context) (map[string]string, error) {
	signed, err := g.appJWT()
	if err != nil {
		return nil, err
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("https://api.github.com/app/installations/%d", g.installation), nil)
	req.Header.Set("Authorization", "Bearer "+signed)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := g.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &StatusError{Method: http.MethodGet, URL: "app/installation", Status: resp.Status, Code: resp.StatusCode}
	}
	var body struct {
		Permissions map[string]string `json:"permissions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	return body.Permissions, nil
}

// MintError is GitHub's refusal to make an installation token. Its message is GitHub's own and never holds a token.
type MintError struct {
	Status  string
	Code    int
	Message string
}

func (e *MintError) Error() string {
	if e.Message == "" {
		return "installation token: " + e.Status
	}
	return "installation token: " + e.Status + ": " + e.Message
}

// apiMessage is the "message" of a GitHub error body, or nothing.
func apiMessage(body []byte) string {
	var m struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &m) != nil {
		return ""
	}
	return m.Message
}

type ListedRunner struct {
	ID     int64
	Name   string
	Busy   bool
	Online bool
}

// get decodes one GET of the REST API into out.
func (g *REST) get(ctx context.Context, url string, out any) error {
	return g.call(ctx, http.MethodGet, url, nil, out, http.StatusOK)
}

// call makes one request of the REST API with a JSON body (when in is not nil), and decodes the answer into out
// (when not nil) if its status is one of ok.
func (g *REST) call(ctx context.Context, method, url string, in, out any, ok ...int) error {
	token, err := g.tokenFor(ctx)
	if err != nil {
		return err
	}
	var body *bytes.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	} else {
		body = bytes.NewReader(nil)
	}
	req, _ := http.NewRequestWithContext(ctx, method, url, body)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	for _, code := range ok {
		if resp.StatusCode == code {
			if out == nil {
				return nil
			}
			return json.NewDecoder(resp.Body).Decode(out)
		}
	}
	return &StatusError{Method: method, URL: url, Status: resp.Status, Code: resp.StatusCode, RetryAfter: resp.Header.Get("Retry-After")}
}

// StatusError is GitHub's answer to a request that was not one of the expected statuses.
type StatusError struct {
	Method, URL, Status string
	Code                int
	RetryAfter          string // the header's value as sent, when there was one
}

func (e *StatusError) Error() string { return fmt.Sprintf("%s %s: %s", e.Method, e.URL, e.Status) }

// RunnerGroup finds or makes the organization's runner group of this name, open to the given repositories of the
// organization only (never public ones), and keeps it so: a runner of the group takes jobs of those repositories
// alone. It returns the group's id.
func (g *REST) RunnerGroup(ctx context.Context, name string, repos []string) (int, error) {
	ids := make([]int64, 0, len(repos))
	for _, r := range repos {
		var repo struct {
			ID int64 `json:"id"`
		}
		if err := g.get(ctx, fmt.Sprintf("https://api.github.com/repos/%s/%s", g.org, r), &repo); err != nil {
			return 0, err
		}
		ids = append(ids, repo.ID)
	}
	base := fmt.Sprintf("https://api.github.com/orgs/%s/actions/runner-groups", g.org)
	var list struct {
		Groups []struct {
			ID   int    `json:"id"`
			Name string `json:"name"`
		} `json:"runner_groups"`
	}
	if err := g.get(ctx, base+"?per_page=100", &list); err != nil {
		return 0, err
	}
	want := map[string]any{"name": name, "visibility": "selected", "selected_repository_ids": ids, "allows_public_repositories": false}
	for _, gr := range list.Groups {
		if gr.Name == name {
			if err := g.call(ctx, http.MethodPatch, fmt.Sprintf("%s/%d", base, gr.ID), map[string]any{"visibility": "selected", "allows_public_repositories": false}, nil, http.StatusOK); err != nil {
				return 0, err
			}
			if err := g.call(ctx, http.MethodPut, fmt.Sprintf("%s/%d/repositories", base, gr.ID), map[string]any{"selected_repository_ids": ids}, nil, http.StatusNoContent); err != nil {
				return 0, err
			}
			return gr.ID, nil
		}
	}
	var made struct {
		ID int `json:"id"`
	}
	if err := g.call(ctx, http.MethodPost, base, want, &made, http.StatusCreated); err != nil {
		return 0, err
	}
	return made.ID, nil
}

// listRunners reads every page of a runner listing. It fails unless it read every page, so a partial listing is
// never taken for the whole.
func (g *REST) listRunners(ctx context.Context, base string, keep func(name string) bool) (map[string]ListedRunner, int64, int, error) {
	out := map[string]ListedRunner{}
	var highest int64
	for page := 1; ; page++ {
		var body struct {
			Total   int `json:"total_count"`
			Runners []struct {
				ID     int64  `json:"id"`
				Name   string `json:"name"`
				Status string `json:"status"`
				Busy   bool   `json:"busy"`
			} `json:"runners"`
		}
		if err := g.get(ctx, fmt.Sprintf("%s?per_page=100&page=%d", base, page), &body); err != nil {
			return nil, 0, page, err
		}
		for _, r := range body.Runners {
			highest = max(highest, r.ID)
			if keep(r.Name) {
				out[r.Name] = ListedRunner{ID: r.ID, Name: r.Name, Busy: r.Busy, Online: r.Status == "online"}
			}
		}
		if len(body.Runners) < 100 || page*100 >= body.Total {
			return out, highest, page, nil
		}
	}
}

// Runners lists every runner of a runner group whose name starts with prefix, and says how many requests it took.
func (g *REST) Runners(ctx context.Context, group int, prefix string) (map[string]ListedRunner, int, error) {
	out, _, pages, err := g.listRunners(ctx, fmt.Sprintf("https://api.github.com/orgs/%s/actions/runner-groups/%d/runners", g.org, group),
		func(name string) bool { return strings.HasPrefix(name, prefix) })
	return out, pages, err
}

// OrgHead is the highest runner id among every runner the organization has registered now: a lower bound, by
// seconds, of the last id GitHub issued, since a runner lives at least until it takes its job.
func (g *REST) OrgHead(ctx context.Context) (int64, error) {
	_, highest, _, err := g.listRunners(ctx, fmt.Sprintf("https://api.github.com/orgs/%s/actions/runners", g.org), func(string) bool { return false })
	return highest, err
}

// RunCompleted says whether a workflow run has completed.
func (g *REST) RunCompleted(ctx context.Context, owner, repo string, run int64) (bool, error) {
	var body struct {
		Status string `json:"status"`
	}
	err := g.get(ctx, fmt.Sprintf("https://api.github.com/repos/%s/%s/actions/runs/%d", owner, repo, run), &body)
	return body.Status == "completed", err
}

// JobOf finds the job of a run that a runner took, by the runner's name and, when name is not "", the job's name, in
// the run's latest attempt. An identity takes many jobs, so its name alone may match more than one of a run.
func (g *REST) JobOf(ctx context.Context, owner, repo string, run int64, runner, name string) (id int64, conclusion string, err error) {
	for page := 1; page <= 20; page++ {
		var body struct {
			Jobs []struct {
				ID         int64  `json:"id"`
				Name       string `json:"name"`
				RunnerName string `json:"runner_name"`
				Conclusion string `json:"conclusion"`
			} `json:"jobs"`
		}
		if err := g.get(ctx, fmt.Sprintf("https://api.github.com/repos/%s/%s/actions/runs/%d/jobs?filter=latest&per_page=100&page=%d", owner, repo, run, page), &body); err != nil {
			return 0, "", err
		}
		for _, j := range body.Jobs {
			if j.RunnerName == runner && (name == "" || j.Name == name) {
				return j.ID, j.Conclusion, nil
			}
		}
		if len(body.Jobs) < 100 {
			break
		}
	}
	return 0, "", nil
}

// RerunJob asks GitHub to run a job again (with the jobs that need it), as its "Re-run job" button does.
func (g *REST) RerunJob(ctx context.Context, owner, repo string, job int64) error {
	token, err := g.tokenFor(ctx)
	if err != nil {
		return err
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("https://api.github.com/repos/%s/%s/actions/jobs/%d/rerun", owner, repo, job), nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := g.http.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("rerun job %d: %s", job, resp.Status)
	}
	return nil
}
