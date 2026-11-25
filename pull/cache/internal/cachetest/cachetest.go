// Package cachetest holds what tests and the proof share: a signer standing in for GitHub's token service, and a
// bucket held in memory.
package cachetest

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"sort"
	"strings"
	"sync"
	"time"

	"superci-pull-cache/internal/cache"
)

// Signer signs job tokens with a key of its own and publishes it as a JWKS.
type Signer struct {
	Key *rsa.PrivateKey
	Kid string
}

func NewSigner(kid string) *Signer {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return &Signer{Key: k, Kid: kid}
}

func enc(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// JWKS is the key set a verifier reads.
func (s *Signer) JWKS() []byte {
	e := big.NewInt(int64(s.Key.E)).Bytes()
	b, _ := json.Marshal(map[string]any{"keys": []map[string]string{{"kty": "RSA", "kid": s.Kid, "alg": "RS256", "use": "sig", "n": enc(s.Key.N.Bytes()), "e": enc(e)}}})
	return b
}

// Scope is "refs/heads/main:rw" or "refs/heads/main:r".
func AC(scopes ...string) string {
	var list []map[string]any
	for _, sc := range scopes {
		name, perm, _ := strings.Cut(sc, ":")
		p := 1
		if perm == "rw" {
			p = 3
		}
		list = append(list, map[string]any{"Scope": name, "Permission": p})
	}
	b, _ := json.Marshal(list)
	return string(b)
}

// Mint signs a token as GitHub does for a job: issuer, repository and the scopes in `ac`.
func (s *Signer) Mint(iss, repo, ac string, exp time.Time) string {
	head, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": s.Kid})
	body, _ := json.Marshal(map[string]any{"iss": iss, "repository_id": repo, "ac": ac, "exp": exp.Unix(), "nbf": exp.Add(-8 * time.Hour).Unix()})
	in := enc(head) + "." + enc(body)
	sum := sha256.Sum256([]byte(in))
	sig, err := rsa.SignPKCS1v15(rand.Reader, s.Key, crypto.SHA256, sum[:])
	if err != nil {
		panic(err)
	}
	return in + "." + enc(sig)
}

// Mem is a bucket in memory.
type Mem struct {
	// MinPart is the smallest block before the last that Assemble takes (S3's is 5 MiB).
	MinPart int64
	mu      sync.Mutex
	objs    map[string][]byte
	mod     map[string]time.Time
	Clock   func() time.Time
	// Opened counts the reads of a range; with FailAfter set, reads after that many fail.
	Opened    int
	FailAfter int
}

func NewMem() *Mem {
	return &Mem{MinPart: 5 << 20, objs: map[string][]byte{}, mod: map[string]time.Time{}, Clock: time.Now}
}

func (m *Mem) Get(key string) ([]byte, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.objs[key]
	return b, ok
}

func (m *Mem) Keys(prefix string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for k := range m.objs {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func (m *Mem) Set(key string, b []byte, at time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objs[key], m.mod[key] = b, at
}

func (m *Mem) Head(_ context.Context, key string) (int64, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.objs[key]
	return int64(len(b)), ok, nil
}

func (m *Mem) List(_ context.Context, prefix string, max int) ([]cache.Object, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []cache.Object
	for k, b := range m.objs {
		if strings.HasPrefix(k, prefix) && len(out) < max {
			out = append(out, cache.Object{Key: k, Size: int64(len(b)), Modified: m.mod[k]})
		}
	}
	return out, nil
}

func (m *Mem) Put(_ context.Context, key string, body []byte) error {
	m.Set(key, append([]byte(nil), body...), m.Clock())
	return nil
}

func (m *Mem) Assemble(_ context.Context, final string, parts []cache.Part) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.objs[final]; ok {
		return cache.ErrExists
	}
	var all bytes.Buffer
	for i, p := range parts {
		if i < len(parts)-1 && p.Size < m.MinPart {
			return cache.ErrPartsTooSmall
		}
		all.Write(m.objs[p.Key])
	}
	m.objs[final], m.mod[final] = all.Bytes(), m.Clock()
	return nil
}

func (m *Mem) Open(_ context.Context, key string, start, end int64) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.objs[key]
	if !ok || start < 0 || end >= int64(len(b)) || start > end {
		return nil, errors.New("no such range")
	}
	m.Opened++
	if m.FailAfter > 0 && m.Opened > m.FailAfter {
		return nil, errors.New("the read failed")
	}
	return io.NopCloser(bytes.NewReader(b[start : end+1])), nil
}

func (m *Mem) Delete(_ context.Context, keys []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, k := range keys {
		delete(m.objs, k)
		delete(m.mod, k)
	}
	return nil
}

func (m *Mem) DeletePrefix(ctx context.Context, prefix string) error {
	return m.Delete(ctx, m.Keys(prefix))
}
