package cache

import (
	"strings"
	"testing"
)

func TestScopesAsGitHubsTokenSaysThem(t *testing.T) {
	got, err := ParseScopes(`[{"Scope":"refs/pull/5/merge","Permission":3},{"Scope":"refs/heads/main","Permission":1},{"Scope":"refs/heads/none","Permission":0}]`)
	if err != nil || len(got) != 2 || got[0] != (Scope{"refs/pull/5/merge", true}) || got[1] != (Scope{"refs/heads/main", false}) {
		t.Fatalf("scopes = %v, %v", got, err)
	}
	if _, err := ParseScopes("not a list"); err == nil {
		t.Fatal("a token's cache scopes that are not a list were taken")
	}
	if _, err := ParseScopes(`[{"Scope":"a\nb","Permission":3}]`); err == nil {
		t.Fatal("a scope with a control character was taken")
	}
	a := Access{Scopes: got}
	if w, ok := a.Writable(); !ok || w.Name != "refs/pull/5/merge" {
		t.Fatalf("writable = %v %v", w, ok)
	}
}

func TestEscKeepsPrefixesPrefixes(t *testing.T) {
	for _, s := range []string{"refs/heads/main", "a b%c", "Linux-go-", "ünï", ""} {
		for i := range len(s) + 1 {
			if !strings.HasPrefix(esc(s), esc(s[:i])) {
				t.Errorf("esc(%q) does not start with esc(%q)", s, s[:i])
			}
		}
	}
	if esc("a/b c%") != "a%2Fb%20c%25" {
		t.Fatalf("esc = %q", esc("a/b c%"))
	}
}

func TestOnlyTheDefaultBranchIsClassD(t *testing.T) {
	if Class("refs/heads/main", "main") != "d" || Class("refs/heads/main2", "main") != "r" || Class("refs/pull/1/merge", "main") != "r" || Class("refs/heads/feature/main", "main") != "r" {
		t.Fatal("class")
	}
}

func TestObjectKeyRefusesWhatGitHubRefuses(t *testing.T) {
	s := Scope{Name: "refs/heads/main", Write: true}
	v := strings.Repeat("a", 64)
	for _, key := range []string{"", "a,b", strings.Repeat("k", 513), "tab\tkey"} {
		if ObjectKey("1", s, v, key, "main") != "" {
			t.Errorf("key %q was taken", key)
		}
	}
	for _, ver := range []string{"", "a/b", strings.Repeat("a", 129)} {
		if ObjectKey("1", s, ver, "k", "main") != "" {
			t.Errorf("version %q was taken", ver)
		}
	}
	if k := ObjectKey("1", s, v, "Linux-bun-"+strings.Repeat("é", 200), "main"); k != "" {
		t.Errorf("a name past S3's limit was taken: %d bytes", len(k))
	}
}

func TestGrantIsTheWritableScopeOnly(t *testing.T) {
	pr := Access{Repo: "77", Scopes: []Scope{{"refs/pull/7/merge", true}, {"refs/heads/main", false}}}
	if g, ok := GrantFor(pr, "main"); !ok || g != (Grant{Entries: "v1/r/77/refs%2Fpull%2F7%2Fmerge/", Stage: "u/77/refs%2Fpull%2F7%2Fmerge/"}) {
		t.Fatalf("pull request grant = %+v %v", g, ok)
	}
	main := Access{Repo: "77", Scopes: []Scope{{"refs/heads/main", true}}}
	if g, _ := GrantFor(main, "main"); g.Entries != "v1/d/77/refs%2Fheads%2Fmain/" || g.Stage != "u/77/refs%2Fheads%2Fmain/" {
		t.Fatalf("main grant = %+v", g)
	}
	if _, ok := GrantFor(Access{Repo: "77", Scopes: []Scope{{"refs/heads/main", false}}}, "main"); ok {
		t.Fatal("a read-only token has a grant")
	}
	// No character of a scope survives into a grant that IAM reads as a wildcard or a policy variable.
	g, _ := GrantFor(Access{Repo: "1", Scopes: []Scope{{"refs/heads/*?${aws:username}", true}}}, "main")
	if strings.ContainsAny(g.Entries+g.Stage, "*?${}") {
		t.Fatalf("grant %+v", g)
	}
}
