package cache

import (
	"net/url"
	"strings"
)

// Entry is an entry's object name taken apart: where it lives (class, repository, scope, version) and the key the
// job saved it under.
type Entry struct {
	Object
	Class, Repo, Scope, Version string
	Name                        string
}

// ParseEntry reads an entry's object name (v1/<class>/<repository id>/<scope>/<version>/<key>). A name that is not
// that shape is not an entry.
func ParseEntry(o Object) (Entry, bool) {
	p := strings.SplitN(o.Key, "/", 6)
	if len(p) != 6 || p[0]+"/" != EntryRoot || p[1] == "" || p[5] == "" {
		return Entry{}, false
	}
	scope, err1 := url.PathUnescape(p[3])
	name, err2 := url.PathUnescape(p[5])
	if err1 != nil || err2 != nil {
		return Entry{}, false
	}
	return Entry{Object: o, Class: p[1], Repo: p[2], Scope: scope, Version: p[4], Name: name}, true
}

// Family is what a key has in common with the keys that replace it. An entry's key is a name followed by the
// hashes of what it holds (a lockfile, a commit, a run's id), and a job that changes the inputs saves a new key of
// the same name: the old key is then only found by a job that still has the old inputs. So the family is the key
// without its trailing hash segments, and its first segment stays always (a key that is nothing but a hash is its
// own family).
//
//	git-seed-v1-Linux-X64-45662624410                          git-seed-v1-Linux-X64
//	acid-vp-deps-v1-Linux-X64-2503440cae4c07a2-bd71c52...e4a95  acid-vp-deps-v1-Linux-X64
//	bun-modules-v1-Linux-X64-bun1.4.3-api-scripts-on-91ff9...  bun-modules-v1-Linux-X64-bun1.4.3-api-scripts-on
//	bun-+NUKu9E/U4apAe78eSCZvQ/GSdc=                           bun
//	uv-toolcache-v1-Linux-X64-uv0.13.9                         (itself: a version is not a hash)
//	apt-Linux-X64-redis-server-2026-W48                        (itself)
//
// A key whose last segment is not a hash is its own family, so nothing is ever called superseded on a guess about
// a name: such entries leave by last use. Where the family is too wide (two live caches that share a name), the
// janitor still keeps an older entry that was used after the newer one was saved (see Plan).
func Family(key string) string {
	segs := strings.Split(key, "-")
	n := len(segs)
	for n > 1 && isHash(segs[n-1]) {
		n--
	}
	return strings.Join(segs[:n], "-")
}

// isHash: a run of at least 8 lowercase hex digits (a digest, a run id, a date such as 20261125), or a base64 text of
// at least 16 characters that holds a digit or one of + / = _ (a word never does).
func isHash(s string) bool {
	if len(s) >= 8 && strings.Trim(s, "0123456789abcdef") == "" {
		return true
	}
	if len(s) < 16 {
		return false
	}
	mark := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9', c == '+', c == '/', c == '=', c == '_':
			mark = true
		default:
			return false
		}
	}
	return mark
}
