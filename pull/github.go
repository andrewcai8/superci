package main

import (
	"context"
	"encoding/json"
	"fmt"
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
	key, err := jwt.ParseRSAPrivateKeyFromPEM([]byte(g.app.PEM))
	if err != nil {
		return "", err
	}
	now := time.Now()
	signed, err := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.RegisteredClaims{Issuer: strconv.FormatInt(g.app.ID, 10),
		IssuedAt: jwt.NewNumericDate(now.Add(-time.Minute)), ExpiresAt: jwt.NewNumericDate(now.Add(9 * time.Minute))}).SignedString(key)
	if err != nil {
		return "", err
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("https://api.github.com/app/installations/%d/access_tokens", g.installation), nil)
	req.Header.Set("Authorization", "Bearer "+signed)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := g.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("installation token: %s", resp.Status)
	}
	var t struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&t); err != nil {
		return "", err
	}
	g.token, g.expires = t.Token, t.ExpiresAt
	return g.token, nil
}

type ListedRunner struct {
	ID     int64
	Name   string
	Busy   bool
	Online bool
}

// Runners lists every runner of a runner group whose name starts with prefix. It fails unless it read every page,
// so a partial listing is never taken for the whole.
func (g *REST) Runners(ctx context.Context, group int, prefix string) (map[string]ListedRunner, error) {
	token, err := g.tokenFor(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]ListedRunner{}
	for page := 1; ; page++ {
		url := fmt.Sprintf("https://api.github.com/orgs/%s/actions/runner-groups/%d/runners?per_page=100&page=%d", g.org, group, page)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/vnd.github+json")
		resp, err := g.http.Do(req)
		if err != nil {
			return nil, err
		}
		var body struct {
			Total   int `json:"total_count"`
			Runners []struct {
				ID     int64  `json:"id"`
				Name   string `json:"name"`
				Status string `json:"status"`
				Busy   bool   `json:"busy"`
			} `json:"runners"`
		}
		err = json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("runners page %d: %s", page, resp.Status)
		}
		if err != nil {
			return nil, err
		}
		for _, r := range body.Runners {
			if strings.HasPrefix(r.Name, prefix) {
				out[r.Name] = ListedRunner{ID: r.ID, Name: r.Name, Busy: r.Busy, Online: r.Status == "online"}
			}
		}
		if len(body.Runners) < 100 || page*100 >= body.Total {
			return out, nil
		}
	}
}
