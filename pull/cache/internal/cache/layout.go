package cache

import (
	"fmt"
	"strings"
)

// The bucket's layout (see the README):
//
//	v1/<class>/<repository id>/<scope>/<version>/<key>   an entry: one object, never changed once written
//	u/<repository id>/<scope>/<upload id>/<block>         an upload's blocks, until it is finished (stage bucket)
//	bin/...                                               the program, its start script and the functions' code
//
// <class> is d for the default branch's entries and r for every other scope: the bucket's lifecycle keeps the
// first longer; the second is only as old as a pull request or a queue batch lives. Scope and key are written with
// esc, which keeps a prefix of a key a prefix of its object name and leaves no character IAM reads as a wildcard
// or a variable, so a scope's prefix can stand in a policy as it is.
const (
	EntryRoot   = "v1/"
	StagingRoot = "u/"
	MaxKeyBytes = 1000
)

// esc writes s so that every byte outside [A-Za-z0-9._-] is %XX. It is applied byte by byte, so the escape of a
// prefix is a prefix of the escape.
func esc(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// Class is "d" for the default branch's scope and "r" for any other.
func Class(scope, defaultBranch string) string {
	if scope == "refs/heads/"+defaultBranch {
		return "d"
	}
	return "r"
}

func okVersion(v string) bool {
	if v == "" || len(v) > 128 {
		return false
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

// okKey is GitHub's rule for a key: at most 512 characters, no comma.
func okKey(k string) bool {
	return k != "" && len(k) <= 512 && !strings.ContainsFunc(k, func(r rune) bool { return r < 0x20 || r == 0x7f || r == ',' })
}

func scopeRoot(repo string, s Scope, defaultBranch string) string {
	return EntryRoot + Class(s.Name, defaultBranch) + "/" + repo + "/" + esc(s.Name) + "/"
}

// ScopePrefix is where a scope's entries of one version are; a key's escape follows it.
func ScopePrefix(repo string, s Scope, version, defaultBranch string) string {
	return scopeRoot(repo, s, defaultBranch) + version + "/"
}

// Grant is where one job may write: its scope's entries, and its uploads' blocks in the stage bucket. The broker
// gives a job credentials for these two prefixes and nothing else, so a job's step that takes them writes no more
// than the job's token already allows.
type Grant struct {
	Entries string
	Stage   string
}

// GrantFor is the job's grant, or false for a token that may not write.
func GrantFor(a Access, defaultBranch string) (Grant, bool) {
	sc, ok := a.Writable()
	if !ok {
		return Grant{}, false
	}
	return Grant{Entries: scopeRoot(a.Repo, sc, defaultBranch), Stage: StagingRoot + a.Repo + "/" + esc(sc.Name) + "/"}, true
}

// ObjectKey is the entry's object name, or "" for a key or version this cache does not keep.
func ObjectKey(repo string, s Scope, version, key, defaultBranch string) string {
	if !okVersion(version) || !okKey(key) {
		return ""
	}
	k := ScopePrefix(repo, s, version, defaultBranch) + esc(key)
	if len(k) > MaxKeyBytes {
		return ""
	}
	return k
}
