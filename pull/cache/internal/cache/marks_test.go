package cache

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSpoolMarksAppendsALineTheSenderPosts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "marks")
	if SpoolMarks(path, time.Now) != nil {
		t.Fatal("a machine without the spool gets no marks")
	}
	if err := os.WriteFile(path, []byte("boot.start=1.000000=0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mark := SpoolMarks(path, func() time.Time { return time.Unix(1795631985, 123456000) })
	mark("cache.restore", 4500, "hit", 1081159529, "bun-modules:v1-a b,c=d")
	got, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimSpace(string(got)), "\n")
	want := "cache.restore=1795631985.123456=4500=hit:1081159529:4500:bun-modules%3Av1-a%20b%2Cc%3Dd"
	if len(lines) != 2 || lines[1] != want {
		t.Fatalf("got %q, want %q", lines, want)
	}
}

func TestAMarkLineIsCutToWhatTheListenerTakesNeverInsideAnEscape(t *testing.T) {
	path := filepath.Join(t.TempDir(), "marks")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	mark := SpoolMarks(path, func() time.Time { return time.Unix(1795631985, 0) })
	mark("cache.restore", 4500, "hit", 1081159529, strings.Repeat("é/", 2000)) // 4 KiB of key, 6 escaped bytes each pair
	mark("cache.save", 12, "miss", 0, "short-key")
	got, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimSpace(string(got)), "\n")
	val := func(l string) string {
		_, rest, _ := strings.Cut(l, "=")
		_, rest, _ = strings.Cut(rest, "=")
		_, val, _ := strings.Cut(rest, "=")
		return val
	}
	if v := val(lines[0]); len(v) > MarkValMax || !strings.HasPrefix(v, "hit:1081159529:4500:") {
		t.Fatalf("a 4 KiB key made a value of %d bytes (limit %d): %.60s", len(val(lines[0])), MarkValMax, lines[0])
	}
	if v := val(lines[0]); strings.LastIndex(v, "%")+3 > len(v) {
		t.Errorf("the value ends inside an escape: ...%s", v[len(v)-6:])
	}
	if lines[1] != "cache.save=1795631985.000000=12=miss:0:12:short-key" {
		t.Errorf("a short key is changed: %q", lines[1])
	}
}

func TestCutEscapedNeverEndsInsideAnEscape(t *testing.T) {
	for _, tc := range []struct {
		in   string
		n    int
		want string
	}{{"abcdef", 10, "abcdef"}, {"abcdef", 3, "abc"}, {"ab%C3%A9", 3, "ab"}, {"ab%C3%A9", 4, "ab"}, {"ab%C3%A9", 5, "ab%C3"}, {"a%2Fb", 2, "a"}, {"abc", -4, ""}} {
		if got := cutEscaped(tc.in, tc.n); got != tc.want {
			t.Errorf("cutEscaped(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
		}
	}
}
