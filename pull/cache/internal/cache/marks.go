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
		enc := strings.ReplaceAll(url.QueryEscape(key), "+", "%20")
		fmt.Fprintf(f, "%s=%d.%06d=%d=%s:%d:%d:%s\n", name, t.Unix(), t.Nanosecond()/1000, ms, kind, bytes, ms, enc)
	}
}

func (s *Server) mark(name string, ms int64, kind string, bytes int64, key string) {
	if s.Mark != nil {
		s.Mark(name, ms, kind, bytes, key)
	}
}
