package cache_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"superci-pull-cache/internal/cache"
	"superci-pull-cache/internal/cachetest"
)

// A whole entry is asked for in one GET; it is read from the bucket in chunks, several at a time, and arrives in order.
func TestLargeReadsAreMadeOfChunksAndStayInOrder(t *testing.T) {
	mem := cachetest.NewMem()
	body := make([]byte, 1000)
	for i := range body {
		body[i] = byte(i * 7)
	}
	mem.Set("obj", body, mem.Clock())
	obj, _, _ := mem.Head(t.Context(), "obj")
	var logs bytes.Buffer
	s := &cache.Server{Store: mem, ReadChunk: 64, ReadAhead: 5, Log: log.New(&logs, "", 0)}
	front := httptest.NewServer(s)
	defer front.Close()
	for _, c := range []struct{ from, to int64 }{{0, 999}, {1, 998}, {64, 191}, {63, 64}, {990, 999}, {0, 63}} {
		got := s.Stream(t.Context(), obj, c.from, c.to)
		if !bytes.Equal(got, body[c.from:c.to+1]) {
			t.Errorf("%d-%d: %d bytes, not the object's", c.from, c.to, len(got))
		}
	}
	if mem.Opened < 16 {
		t.Errorf("only %d reads of the bucket: the whole entry was read in one", mem.Opened)
	}
}

func TestAReadThatFailsMidwayEndsTheResponseShort(t *testing.T) {
	mem := cachetest.NewMem()
	mem.Set("obj", bytes.Repeat([]byte("x"), 1000), mem.Clock())
	mem.FailAfter = 3
	obj, _, _ := mem.Head(t.Context(), "obj")
	s := &cache.Server{Store: mem, ReadChunk: 100, ReadAhead: 2}
	var out bytes.Buffer
	n, err := s.StreamTo(t.Context(), &out, obj, 0, 999)
	if err == nil || n >= 1000 || int64(out.Len()) != n {
		t.Fatalf("sent %d bytes (%d written), error %v: a failed read must show", n, out.Len(), err)
	}
}

func TestRestoreSendsWholeEntryInChunksOverHTTP(t *testing.T) {
	r := newRig(t)
	srv := r.srv.Config.Handler.(*cache.Server)
	srv.ReadChunk, srv.ReadAhead = 4, 3
	tok := r.token("77", "refs/heads/main:rw")
	payload := []byte(fmt.Sprintf("%0100d", 42))
	r.save(tok, "k", [][]byte{payload}, true)
	_, d := r.twirp(tok, "GetCacheEntryDownloadURL", map[string]any{"key": "k", "version": version})
	resp, err := http.Get(d["signed_download_url"].(string))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.Equal(got, payload) {
		t.Fatalf("got %q", got)
	}
}

// An entry replaced while it is read (deleted by the janitor and saved again, or written by anything else) never
// reaches the client as a mix of two archives: every range is read as the version the download began with, and
// the first range of another version ends the response short, which actions/cache reports as a miss.
func TestAnEntryReplacedMidReadEndsShortNeverSpliced(t *testing.T) {
	old, replacement := bytes.Repeat([]byte("o"), 1000), bytes.Repeat([]byte("N"), 1000)
	mem := cachetest.NewMem()
	mem.Set("obj", old, mem.Clock())
	obj, _, _ := mem.Head(t.Context(), "obj")
	mem.OnOpen = func(n int) {
		if n == 4 {
			mem.Set("obj", replacement, mem.Clock())
		}
	}
	s := &cache.Server{Store: mem, ReadChunk: 100, ReadAhead: 1}
	var out bytes.Buffer
	n, err := s.StreamTo(t.Context(), &out, obj, 0, 999)
	if !errors.Is(err, cache.ErrChanged) || n >= 1000 {
		t.Fatalf("sent %d of 1000 bytes, error %v: the replacement must end the read", n, err)
	}
	if !bytes.Equal(out.Bytes(), old[:out.Len()]) {
		t.Fatalf("the %d bytes sent are not all the old version's", out.Len())
	}
}

func TestAnEntryReplacedMidDownloadIsAShortResponseOverHTTP(t *testing.T) {
	r := newRig(t)
	srv := r.srv.Config.Handler.(*cache.Server)
	srv.ReadChunk, srv.ReadAhead = 10, 1
	tok := r.token("77", "refs/heads/main:rw")
	old := bytes.Repeat([]byte("o"), 100)
	if !r.save(tok, "k", [][]byte{old}, true) {
		t.Fatalf("not saved:\n%s", r.log)
	}
	_, d := r.twirp(tok, "GetCacheEntryDownloadURL", map[string]any{"key": "k", "version": version})
	key := r.mem.Keys("v1/")[0]
	r.mem.Opened = 0
	r.mem.OnOpen = func(n int) {
		if n == 3 {
			r.mem.Set(key, bytes.Repeat([]byte("N"), 100), r.mem.Clock())
		}
	}
	resp, err := http.Get(d["signed_download_url"].(string))
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err == nil || len(got) >= len(old) || resp.ContentLength != int64(len(old)) {
		t.Fatalf("read %d of %d bytes (Content-Length %d), error %v: the client must see the download cut short", len(got), len(old), resp.ContentLength, err)
	}
	if !bytes.Equal(got, old[:len(got)]) {
		t.Fatalf("the client got bytes of the replacement: %q", got)
	}
	if !strings.Contains(r.log.String(), "cut short") {
		t.Fatalf("the daemon did not say the download was cut short:\n%s", r.log)
	}
}
