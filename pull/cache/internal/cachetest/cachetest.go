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
	"fmt"
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
	return s.Sign(map[string]any{"iss": iss, "repository_id": repo, "ac": ac, "exp": exp.Unix(), "nbf": exp.Add(-8 * time.Hour).Unix()})
}

// Sign signs any claims.
func (s *Signer) Sign(claims map[string]any) string {
	head, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": s.Kid})
	body, _ := json.Marshal(claims)
	in := enc(head) + "." + enc(body)
	sum := sha256.Sum256([]byte(in))
	sig, err := rsa.SignPKCS1v15(rand.Reader, s.Key, crypto.SHA256, sum[:])
	if err != nil {
		panic(err)
	}
	return in + "." + enc(sig)
}

// Mem is a bucket in memory. Its writers keep to their grant as the broker's credentials do in S3, and an
// entry is never written over, as the bucket's policy refuses it.
type Mem struct {
	// MinPart is the smallest block before the last that Assemble takes (S3's is 5 MiB).
	MinPart int64
	mu      sync.Mutex
	objs    map[string][]byte
	mod     map[string]time.Time
	saved   map[string]time.Time
	tags    map[string]string
	n       int
	Clock   func() time.Time
	// Opened counts the reads of a range; with FailAfter set, reads after that many fail.
	Opened    int
	FailAfter int
	// OnOpen, when set, runs before the read of a range is answered (it is given the count of reads so far), and
	// may change the bucket.
	OnOpen func(opened int)
	// Granted is the grant of each writer made, in order.
	Granted []cache.Grant
}

// ErrDenied is what a write outside a writer's grant gets.
var ErrDenied = errors.New("AccessDenied")

func NewMem() *Mem {
	return &Mem{MinPart: 5 << 20, objs: map[string][]byte{}, mod: map[string]time.Time{}, saved: map[string]time.Time{}, tags: map[string]string{}, Clock: time.Now}
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

// Set writes an object as the test says, a new version with a new ETag (an overwrite only a test can make).
func (m *Mem) Set(key string, b []byte, at time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.set(key, b, at)
}

// SetCopy writes an object as the mirror does: copied at, holding the save time of its original.
func (m *Mem) SetCopy(key string, b []byte, saved, at time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.set(key, b, at)
	m.saved[key] = saved
}

func (m *Mem) set(key string, b []byte, at time.Time) {
	m.n++
	m.objs[key], m.mod[key], m.tags[key] = b, at, fmt.Sprintf(`"%d"`, m.n)
	delete(m.saved, key)
}

// object is what a listing says of k: no metadata, so no save time.
func (m *Mem) object(k string) cache.Object {
	return cache.Object{Key: k, Size: int64(len(m.objs[k])), Modified: m.mod[k], ETag: m.tags[k]}
}

func (m *Mem) Head(_ context.Context, key string) (cache.Object, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.objs[key]; !ok {
		return cache.Object{}, false, nil
	}
	o := m.object(key)
	o.Saved = m.saved[key]
	return o, true, nil
}

func (m *Mem) List(_ context.Context, prefix string, max int) ([]cache.Object, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var keys []string
	for k := range m.objs {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	// S3 lists in key order, so a listing cut at max loses the keys that sort last.
	sort.Strings(keys)
	var out []cache.Object
	for _, k := range keys[:min(len(keys), max)] {
		out = append(out, m.object(k))
	}
	return out, nil
}

func (m *Mem) Open(_ context.Context, key, etag string, start, end int64) (io.ReadCloser, error) {
	m.mu.Lock()
	m.Opened++
	n, hook := m.Opened, m.OnOpen
	m.mu.Unlock()
	if hook != nil {
		hook(n)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.objs[key]
	if !ok || m.tags[key] != etag {
		return nil, cache.ErrChanged
	}
	if start < 0 || end >= int64(len(b)) || start > end {
		return nil, errors.New("no such range")
	}
	if m.FailAfter > 0 && n > m.FailAfter {
		return nil, errors.New("the read failed")
	}
	return io.NopCloser(bytes.NewReader(b[start : end+1])), nil
}

// Delete is the janitor's: it deletes each object that is still the version listed (by ETag), as the bucket's
// conditional delete does, and says what it did to each.
func (m *Mem) Delete(_ context.Context, objs []cache.Object) ([]cache.Removal, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	got := make([]cache.Removal, len(objs))
	for i, o := range objs {
		switch tag, ok := m.tags[o.Key]; {
		case !ok:
			got[i] = cache.Gone
		case tag != o.ETag:
			got[i] = cache.Changed
		default:
			m.remove(o.Key)
		}
	}
	return got, nil
}

func (m *Mem) remove(key string) {
	delete(m.objs, key)
	delete(m.mod, key)
	delete(m.saved, key)
	delete(m.tags, key)
}

// CreateMark writes an empty object at key unless there is one (the machine's marker write).
func (m *Mem) CreateMark(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.objs[key]; !ok {
		m.set(key, nil, m.Clock())
	}
	return nil
}

func (m *Mem) Writer(_ context.Context, _ string, g cache.Grant) (cache.Writer, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Granted = append(m.Granted, g)
	return &memWriter{m: m, g: g}, nil
}

type memWriter struct {
	m *Mem
	g cache.Grant
}

func (w *memWriter) Put(_ context.Context, key string, body []byte) error {
	if !strings.HasPrefix(key, w.g.Stage) {
		return ErrDenied
	}
	w.m.mu.Lock()
	defer w.m.mu.Unlock()
	w.m.set(key, append([]byte(nil), body...), w.m.Clock())
	return nil
}

func (w *memWriter) Assemble(_ context.Context, final string, parts []cache.Part) error {
	if !strings.HasPrefix(final, w.g.Entries) {
		return ErrDenied
	}
	m := w.m
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.objs[final]; ok {
		return cache.ErrExists
	}
	var all bytes.Buffer
	for i, p := range parts {
		if !strings.HasPrefix(p.Key, w.g.Stage) {
			return ErrDenied
		}
		if i < len(parts)-1 && p.Size < m.MinPart {
			return cache.ErrPartsTooSmall
		}
		all.Write(m.objs[p.Key])
	}
	m.set(final, all.Bytes(), m.Clock())
	return nil
}

func (w *memWriter) Delete(_ context.Context, keys []string) error {
	for _, k := range keys {
		if !strings.HasPrefix(k, w.g.Stage) {
			return ErrDenied
		}
	}
	w.m.mu.Lock()
	defer w.m.mu.Unlock()
	for _, k := range keys {
		w.m.remove(k)
	}
	return nil
}
