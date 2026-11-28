package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Identities are the persistent runner identities of the identity plane (deploy/identity.sh): classic runners
// registered once, whose listeners live on identity hosts. This file is the listener's part of it, on the admin
// endpoint. A registration is the one thing that spends the organization's cap, so the token that makes one is only
// minted against a lease of the budget (lease.go); relabelling an identity (the cutover and its rollback) makes none.
const (
	identityPrefix   = "superci-id-"
	maxIdentityLease = 100 // identities one token call may lease: one token registers any number, the lease bounds the rate
	maxLabels        = 20
)

// Identities serves /identities*. repo is empty at the organization's scope, and otherwise the one repository the
// runners register to; its runners then sit in the repository's own list, not in a runner group.
type Identities struct {
	rest       *REST
	budget     *Budget
	deployment string
	repos      []string
	repo       string
}

// NewIdentities reads the scope the config names: "repository" registers to the first of its repositories, anything
// else to the organization.
func NewIdentities(rest *REST, budget *Budget, cfg Config) *Identities {
	ids := &Identities{rest: rest, budget: budget, deployment: cfg.Deployment, repos: cfg.Repositories}
	if cfg.Scope == "repository" && len(cfg.Repositories) > 0 {
		ids.repo = cfg.Repositories[0]
	}
	return ids
}

func (ids *Identities) groupName() string { return ids.deployment + "-identities" }

// Routes adds the endpoints to the admin mux.
func (ids *Identities) Routes(mux *http.ServeMux) {
	mux.HandleFunc("POST /identities/token", ids.token)
	mux.HandleFunc("POST /identities/labels", ids.labels)
	mux.HandleFunc("GET /identities", ids.list)
}

// runnersURL is where the runners of a scope are, and configURL what config.sh registers to.
func (g *REST) runnersURL(repo string) string {
	if repo != "" {
		return fmt.Sprintf("https://api.github.com/repos/%s/%s/actions/runners", g.org, repo)
	}
	return fmt.Sprintf("https://api.github.com/orgs/%s/actions/runners", g.org)
}

func (g *REST) configURL(repo string) string {
	if repo != "" {
		return fmt.Sprintf("https://github.com/%s/%s", g.org, repo)
	}
	return "https://github.com/" + g.org
}

// RegistrationToken mints the one-hour token config.sh registers a runner with.
func (g *REST) RegistrationToken(ctx context.Context, repo string) (token string, expires time.Time, err error) {
	var body struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := g.call(ctx, http.MethodPost, g.runnersURL(repo)+"/registration-token", nil, &body, http.StatusCreated); err != nil {
		return "", time.Time{}, err
	}
	if body.Token == "" {
		return "", time.Time{}, fmt.Errorf("registration token: GitHub answered no token")
	}
	return body.Token, body.ExpiresAt, nil
}

// Runner reads one runner of a scope, to tell what it is before anything is done to it.
func (g *REST) Runner(ctx context.Context, repo string, id int64) (name string, err error) {
	var body struct {
		Name string `json:"name"`
	}
	if err := g.get(ctx, fmt.Sprintf("%s/%d", g.runnersURL(repo), id), &body); err != nil {
		return "", err
	}
	return body.Name, nil
}

// SetRunnerLabels replaces a runner's custom labels.
func (g *REST) SetRunnerLabels(ctx context.Context, repo string, id int64, labels []string) error {
	return g.call(ctx, http.MethodPut, fmt.Sprintf("%s/%d/labels", g.runnersURL(repo), id), map[string]any{"labels": labels}, nil, http.StatusOK)
}

// GroupID finds the organization's runner group of this name without changing it: 0 when there is none.
func (g *REST) GroupID(ctx context.Context, name string) (int, error) {
	var list struct {
		Groups []struct {
			ID   int    `json:"id"`
			Name string `json:"name"`
		} `json:"runner_groups"`
	}
	if err := g.get(ctx, fmt.Sprintf("https://api.github.com/orgs/%s/actions/runner-groups?per_page=100", g.org), &list); err != nil {
		return 0, err
	}
	for _, gr := range list.Groups {
		if gr.Name == name {
			return gr.ID, nil
		}
	}
	return 0, nil
}

// IdentityRunner is one identity as GitHub lists it.
type IdentityRunner struct {
	ID     int64    `json:"id"`
	Name   string   `json:"name"`
	Status string   `json:"status"`
	Busy   bool     `json:"busy"`
	Labels []string `json:"labels"`
}

// IdentityRunners lists every runner of a listing whose name starts with prefix, with its labels. It fails unless it
// read every page.
func (g *REST) IdentityRunners(ctx context.Context, base, prefix string) ([]IdentityRunner, error) {
	var out []IdentityRunner
	for page := 1; ; page++ {
		var body struct {
			Total   int `json:"total_count"`
			Runners []struct {
				ID     int64  `json:"id"`
				Name   string `json:"name"`
				Status string `json:"status"`
				Busy   bool   `json:"busy"`
				Labels []struct {
					Name string `json:"name"`
				} `json:"labels"`
			} `json:"runners"`
		}
		if err := g.get(ctx, fmt.Sprintf("%s?per_page=100&page=%d", base, page), &body); err != nil {
			return nil, err
		}
		for _, r := range body.Runners {
			if !strings.HasPrefix(r.Name, prefix) {
				continue
			}
			ir := IdentityRunner{ID: r.ID, Name: r.Name, Status: r.Status, Busy: r.Busy, Labels: []string{}}
			for _, l := range r.Labels {
				ir.Labels = append(ir.Labels, l.Name)
			}
			out = append(out, ir)
		}
		if len(body.Runners) < 100 || page*100 >= body.Total {
			return out, nil
		}
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

// IdentityToken is what a registration needs. Token is a secret: it is written to the caller and nowhere else.
type IdentityToken struct {
	Lease        string    `json:"lease"`
	Granted      int       `json:"granted"`
	Token        string    `json:"token,omitempty"`
	Expires      time.Time `json:"expires,omitzero"`
	URL          string    `json:"url,omitempty"`
	Group        int       `json:"group,omitempty"`
	GroupName    string    `json:"group_name,omitempty"`
	LeaseExpires time.Time `json:"lease_expires,omitzero"`
}

// token leases n registrations at batch priority and, if any are granted, ensures the identities' runner group and
// mints a registration token. It mints none for a lease of nothing, and gives the lease back if anything after it fails.
func (ids *Identities) token(w http.ResponseWriter, r *http.Request) {
	n, err := strconv.Atoi(r.URL.Query().Get("n"))
	if err != nil || n < 1 || n > maxIdentityLease {
		http.Error(w, fmt.Sprintf("n is 1 to %d registrations", maxIdentityLease), http.StatusBadRequest)
		return
	}
	now := time.Now()
	l := ids.budget.Lease(now, n, maxLeaseTTL)
	slog.Info("identities lease", "asked", n, "granted", l.Granted, "id", l.ID)
	if l.Granted == 0 {
		writeJSON(w, IdentityToken{})
		return
	}
	give := func(why string, err error) {
		ids.budget.Settle(time.Now(), l.ID, Settlement{})
		slog.Error("identities token", "why", why, "lease", l.ID, "err", err.Error())
		http.Error(w, why+" failed; the lease is given back", http.StatusBadGateway)
	}
	group := 1 // the Default group, where a repository's runners are
	if ids.repo == "" {
		group, err = ids.rest.RunnerGroup(r.Context(), ids.groupName(), ids.repos)
		if err != nil {
			give("runner group", err)
			return
		}
	}
	token, expires, err := ids.rest.RegistrationToken(r.Context(), ids.repo)
	if err != nil {
		give("registration token", err)
		return
	}
	out := IdentityToken{Lease: l.ID, Granted: l.Granted, Token: token, Expires: expires, URL: ids.rest.configURL(ids.repo), Group: group, LeaseExpires: l.Expires}
	if ids.repo == "" {
		out.GroupName = ids.groupName()
	}
	writeJSON(w, out)
}

// labels replaces one runner's labels: the cutover and its rollback move an identity between the park label and its
// size's label this way, and no registration is made. It only touches an identity: the id is read first, because a
// just-in-time runner of the same scope would otherwise lose the labels its jobs are routed by.
func (ids *Identities) labels(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	id, err := strconv.ParseInt(q.Get("id"), 10, 64)
	if err != nil || id < 1 {
		http.Error(w, "id is a runner id", http.StatusBadRequest)
		return
	}
	labels := strings.Split(q.Get("labels"), ",")
	if len(labels) > maxLabels || slices.ContainsFunc(labels, func(l string) bool { return strings.TrimSpace(l) == "" || l != strings.TrimSpace(l) }) {
		http.Error(w, fmt.Sprintf("labels are 1 to %d non-empty names, comma separated", maxLabels), http.StatusBadRequest)
		return
	}
	name, err := ids.rest.Runner(r.Context(), ids.repo, id)
	if err != nil {
		slog.Error("identities labels", "id", id, "err", err.Error())
		if !limited(w, err) {
			http.Error(w, "GitHub could not say which runner that is", http.StatusBadGateway)
		}
		return
	}
	if !strings.HasPrefix(name, identityPrefix) {
		slog.Warn("identities labels refused", "id", id, "name", name)
		http.Error(w, "that runner is not an identity: its name does not start "+identityPrefix, http.StatusForbidden)
		return
	}
	if err := ids.rest.SetRunnerLabels(r.Context(), ids.repo, id, labels); err != nil {
		slog.Error("identities labels", "id", id, "err", err.Error())
		if !limited(w, err) {
			http.Error(w, "GitHub refused the labels", http.StatusBadGateway)
		}
		return
	}
	slog.Info("identities labels", "id", id, "labels", strings.Join(labels, ","))
	w.WriteHeader(http.StatusNoContent)
}

var (
	errGroups  = errors.New("runner groups could not be listed")
	errRunners = errors.New("runners could not be listed")
)

// Listing reads the identities of the scope: its runner group (0 when there is none yet, and then no identity) and every
// identity in it, one GitHub call a page. The operator's GET /identities and the listener's own count of identities per
// size are both this one read.
func (ids *Identities) Listing(ctx context.Context) (group int, runners []IdentityRunner, err error) {
	base := ids.rest.runnersURL(ids.repo)
	group = 1
	if ids.repo == "" {
		if group, err = ids.rest.GroupID(ctx, ids.groupName()); err != nil {
			return 0, nil, fmt.Errorf("%w: %v", errGroups, err)
		}
		base = fmt.Sprintf("https://api.github.com/orgs/%s/actions/runner-groups/%d/runners", ids.rest.org, group)
	}
	runners = []IdentityRunner{}
	if group != 0 {
		if runners, err = ids.rest.IdentityRunners(ctx, base, identityPrefix); err != nil {
			return 0, nil, fmt.Errorf("%w: %v", errRunners, err)
		}
	}
	return group, runners, nil
}

// list is for the operator: every identity of the scope, with its labels and status.
func (ids *Identities) list(w http.ResponseWriter, r *http.Request) {
	group, runners, err := ids.Listing(r.Context())
	if err != nil {
		slog.Error("identities list", "err", err.Error())
		if errors.Is(err, errGroups) {
			http.Error(w, errGroups.Error(), http.StatusBadGateway)
		} else {
			http.Error(w, errRunners.Error(), http.StatusBadGateway)
		}
		return
	}
	writeJSON(w, map[string]any{"group": group, "runners": runners})
}

// IdentityCount is how many identities of a dispatch size are online at GitHub, how many of them hold a job, and how many
// carry the size's label at all, whatever GitHub says of their status. An identity serves the size whose label it carries;
// a parked one (superci-park) serves none and is counted nowhere. Labeled is the count the alarms trust from GitHub: a label
// is what the REST list holds correctly, where its status read offline for listeners that were taking jobs (Nov 28), and the
// hosts' IdentitiesPolling cannot see a label (a parked identity's listener holds a session like any other).
type IdentityCount struct {
	Set                   string
	Online, Busy, Labeled int
}

// CountIdentities counts the identities of each dispatch set, given its label by set name. Every set has a count, zero
// when no identity carries its label: a size with none to take its jobs is the starvation this exists to show.
func CountIdentities(runners []IdentityRunner, labels map[string]string) []IdentityCount {
	by := make(map[string]*IdentityCount, len(labels))
	out := make([]IdentityCount, 0, len(labels))
	for set := range labels {
		out = append(out, IdentityCount{Set: set})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Set < out[j].Set })
	for i := range out {
		by[out[i].Set] = &out[i]
	}
	for _, r := range runners {
		for set, label := range labels {
			if !slices.Contains(r.Labels, label) {
				continue
			}
			by[set].Labeled++
			if r.Status != "online" {
				continue
			}
			by[set].Online++
			if r.Busy {
				by[set].Busy++
			}
		}
	}
	return out
}

// limited answers 429, with GitHub's Retry-After, when GitHub's answer was a limit (403 or 429), and says whether it
// did: the caller stops and waits that long, on the read before a relabel as on its write.
func limited(w http.ResponseWriter, err error) bool {
	var se *StatusError
	if !errors.As(err, &se) || (se.Code != http.StatusForbidden && se.Code != http.StatusTooManyRequests) {
		return false
	}
	if secs, err := strconv.Atoi(se.RetryAfter); err == nil && secs > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(secs))
	}
	http.Error(w, "GitHub is limiting requests", http.StatusTooManyRequests)
	return true
}
