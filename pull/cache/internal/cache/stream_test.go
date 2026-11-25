package cache_test

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
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
	var logs bytes.Buffer
	s := &cache.Server{Store: mem, ReadChunk: 64, ReadAhead: 5, Log: log.New(&logs, "", 0)}
	front := httptest.NewServer(s)
	defer front.Close()
	for _, c := range []struct{ from, to int64 }{{0, 999}, {1, 998}, {64, 191}, {63, 64}, {990, 999}, {0, 63}} {
		got := s.Stream(t.Context(), "obj", c.from, c.to)
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
	s := &cache.Server{Store: mem, ReadChunk: 100, ReadAhead: 2}
	var out bytes.Buffer
	n, err := s.StreamTo(t.Context(), &out, "obj", 0, 999)
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
