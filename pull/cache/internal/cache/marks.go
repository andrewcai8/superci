package cache

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"
)

// SpoolMarks returns a Server.Mark that appends each fact to the machine's marks spool (the job timeline's, pull/events.go):
// a line name=epoch=ms=kind:bytes:ms:key, which boot.sh's sender posts to the listener. A machine without the spool (not
// one of the fleet, or an older boot.sh) gets no marks and the cache does exactly what it did.
func SpoolMarks(path string, now func() time.Time) func(name string, ms int64, kind string, bytes int64, key string) {
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	return func(name string, ms int64, kind string, bytes int64, key string) {
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			return
		}
		defer f.Close()
		t := now()
		head := fmt.Sprintf("%s:%d:%d:", kind, bytes, ms)
		enc := cutEscaped(strings.ReplaceAll(url.QueryEscape(key), "+", "%20"), MarkValMax-len(head))
		fmt.Fprintf(f, "%s=%d.%06d=%d=%s%s\n", name, t.Unix(), t.Nanosecond()/1000, ms, head, enc)
	}
}

// MarkValMax is the most a mark's value may hold, the listener's markValMax (pull/events.go): a longer one is cut there, and
// a spool line the listener cannot take at all (a key of kilobytes) held up every mark behind it.
const MarkValMax = 256

// cutEscaped cuts a url-escaped text to at most n bytes, never inside a %XX escape.
func cutEscaped(s string, n int) string {
	if len(s) <= n {
		return s
	}
	n = max(0, n)
	for i := max(0, n-2); i < n; i++ {
		if s[i] == '%' {
			return s[:i]
		}
	}
	return s[:n]
}

func (s *Server) mark(name string, ms int64, kind string, bytes int64, key string) {
	if s.Mark != nil {
		s.Mark(name, ms, kind, bytes, key)
	}
}
