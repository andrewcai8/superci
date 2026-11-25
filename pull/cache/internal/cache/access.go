// Package cache serves GitHub's Actions cache (cache service v2, the protocol actions/cache@v4 and later speak) from
// an S3 bucket. A job's runtime token says which scopes it may read and write; the bucket's key layout carries the
// scope, so reading and writing stay inside what GitHub's own cache allows.
package cache

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	GitHubIssuer = "https://token.actions.githubusercontent.com"
	GitHubJWKS   = GitHubIssuer + "/.well-known/jwks"
)

// Scope is one branch's caches (refs/heads/main, refs/pull/5/merge) and whether the token may write there.
type Scope struct {
	Name  string
	Write bool
}

// Access is what a job's token lets it do: the repository's entries in these scopes, read in this order.
type Access struct {
	Repo   string
	Scopes []Scope
}

// Writable is the scope the job saves into. GitHub gives a job one: its own ref.
func (a Access) Writable() (Scope, bool) {
	for _, s := range a.Scopes {
		if s.Write {
			return s, true
		}
	}
	return Scope{}, false
}

// Verifier checks a job's runtime token as GitHub signed it (RS256, the keys at GitHub's JWKS) and reads its scopes.
type Verifier struct {
	JWKSURL string
	Issuer  string
	HTTP    *http.Client
	Now     func() time.Time

	mu      sync.Mutex
	keys    map[string]*rsa.PublicKey
	fetched time.Time
}

// ErrUnavailable means the token could not be judged (GitHub's keys could not be read), not that it is bad.
var ErrUnavailable = errors.New("GitHub's signing keys could not be read")

func (v *Verifier) now() time.Time {
	if v.Now != nil {
		return v.Now()
	}
	return time.Now()
}

func (v *Verifier) key(kid string) (*rsa.PublicKey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if k, ok := v.keys[kid]; ok {
		return k, nil
	}
	// A key not known is looked for again, at most once a minute: a token cannot make this fetch GitHub in a loop.
	if v.keys != nil && v.now().Sub(v.fetched) < time.Minute {
		return nil, errors.New("signed by a key GitHub does not list")
	}
	hc := v.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := hc.Get(v.JWKSURL)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("%w: status %d", ErrUnavailable, resp.StatusCode)
	}
	var set struct {
		Keys []struct{ Kty, Kid, N, E string } `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&set); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	keys := map[string]*rsa.PublicKey{}
	for _, k := range set.Keys {
		if k.Kty != "RSA" {
			continue
		}
		n, e := b64(k.N), b64(k.E)
		if n == nil || e == nil {
			continue
		}
		keys[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
	}
	v.keys, v.fetched = keys, v.now()
	if k, ok := keys[kid]; ok {
		return k, nil
	}
	return nil, errors.New("signed by a key GitHub does not list")
}

func b64(s string) []byte {
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
	if err != nil {
		return nil
	}
	return b
}

// Verify returns what the token allows. A token that is not GitHub's, is expired or has no expiry, or says nothing
// it can hold to is refused; ErrUnavailable (wrapped) means GitHub's keys could not be read. A refusal names the
// token's issuer and key id (neither is a secret), so a log shows what a real job's token carries when it is refused.
func (v *Verifier) Verify(token string) (Access, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Access{}, errors.New("not a job's token")
	}
	var head struct{ Alg, Kid string }
	if h := b64(parts[0]); h == nil || json.Unmarshal(h, &head) != nil {
		return Access{}, errors.New("not a job's token")
	}
	var c claims
	if p := b64(parts[1]); p == nil || json.Unmarshal(p, &c) != nil {
		return Access{}, errors.New("not a job's token")
	}
	a, err := v.check(parts, head.Alg, head.Kid, c)
	if err != nil {
		return Access{}, fmt.Errorf("%w (iss %q, kid %q)", err, clip(c.Iss), clip(head.Kid))
	}
	return a, nil
}

type claims struct {
	Iss    string          `json:"iss"`
	Exp    float64         `json:"exp"`
	Nbf    float64         `json:"nbf"`
	Repo   json.RawMessage `json:"repository_id"`
	Access string          `json:"ac"`
}

func clip(s string) string {
	if len(s) > 100 {
		return s[:100] + "..."
	}
	return s
}

func (v *Verifier) check(parts []string, alg, kid string, c claims) (Access, error) {
	if alg != "RS256" {
		return Access{}, errors.New("not signed with RS256")
	}
	sig := b64(parts[2])
	if sig == nil {
		return Access{}, errors.New("not a job's token")
	}
	pub, err := v.key(kid)
	if err != nil {
		return Access{}, err
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], sig) != nil {
		return Access{}, errors.New("bad signature")
	}
	if v.Issuer == "" || c.Iss != v.Issuer {
		return Access{}, errors.New("not a GitHub job's token")
	}
	now := float64(v.now().Unix())
	if c.Exp <= 0 {
		return Access{}, errors.New("a token without its expiry")
	}
	if now > c.Exp+60 {
		return Access{}, errors.New("expired")
	}
	if c.Nbf != 0 && now+60 < c.Nbf {
		return Access{}, errors.New("not yet valid")
	}
	repo := strings.Trim(string(c.Repo), `"`)
	if repo == "" || strings.Trim(repo, "0123456789") != "" || len(repo) > 20 {
		return Access{}, errors.New("a token without its repository")
	}
	scopes, err := ParseScopes(c.Access)
	if err != nil {
		return Access{}, err
	}
	return Access{Repo: repo, Scopes: scopes}, nil
}

// ParseScopes reads GitHub's `ac` claim: a JSON list in a string, [{"Scope":"refs/heads/main","Permission":3}],
// where 1 is read and 2 is write.
func ParseScopes(ac string) ([]Scope, error) {
	var list []struct {
		Scope      string
		Permission int
	}
	if err := json.Unmarshal([]byte(ac), &list); err != nil {
		return nil, fmt.Errorf("the token's cache scopes: %v", err)
	}
	var out []Scope
	for _, s := range list {
		if s.Permission&3 == 0 {
			continue
		}
		if s.Scope == "" || len(s.Scope) > 256 || strings.ContainsFunc(s.Scope, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
			return nil, errors.New("a cache scope this cache does not keep")
		}
		out = append(out, Scope{Name: s.Scope, Write: s.Permission&2 != 0})
	}
	return out, nil
}
