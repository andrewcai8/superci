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
