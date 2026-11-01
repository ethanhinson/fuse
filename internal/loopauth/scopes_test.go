package loopauth_test

import (
	"reflect"
	"testing"

	"github.com/ethanhinson/fuse/internal/loopauth"
)

func TestNewScopesCanonicalizes(t *testing.T) {
	got := loopauth.NewScopes("loop:send", " loop:start ", "loop:send", "")
	if want := loopauth.Scopes("loop:send loop:start"); got != want {
		t.Fatalf("NewScopes = %q, want %q", got, want)
	}
	if got := loopauth.NewScopes(); got != loopauth.Scopes("") {
		t.Fatalf("NewScopes() = %q, want empty", got)
	}
}

func TestParseScopesSplitsOnWhitespace(t *testing.T) {
	got := loopauth.ParseScopes("loop:observe  loop:start\tcapabilities:read")
	want := loopauth.NewScopes("capabilities:read", "loop:observe", "loop:start")
	if got != want {
		t.Fatalf("ParseScopes = %q, want %q", got, want)
	}
}

func TestScopesListIsNonNil(t *testing.T) {
	empty := loopauth.Scopes("").List()
	if empty == nil || len(empty) != 0 {
		t.Fatalf("Scopes(\"\").List() = %#v, want non-nil empty slice", empty)
	}
	if got := loopauth.NewScopes("b", "a").List(); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("List = %#v, want [a b]", got)
	}
}

func TestPrincipalAllowsAndGrants(t *testing.T) {
	vocabulary := []string{
		loopauth.ScopeLoopStart, loopauth.ScopeLoopSend, loopauth.ScopeLoopObserve,
		loopauth.ScopeLoopAdmin, loopauth.ScopeCapabilitiesRead, loopauth.ScopeSecretsInline,
	}
	cases := []struct {
		name   string
		p      loopauth.Principal
		scope  string
		allows bool
		grants bool
	}{
		{"unrestricted allows start", loopauth.Principal{}, loopauth.ScopeLoopStart, true, false},
		{"unrestricted never grants admin", loopauth.Principal{}, loopauth.ScopeLoopAdmin, true, false},
		{"listed observe", loopauth.Principal{Scopes: loopauth.NewScopes(loopauth.ScopeLoopObserve)}, loopauth.ScopeLoopObserve, true, true},
		{"unlisted start", loopauth.Principal{Scopes: loopauth.NewScopes(loopauth.ScopeLoopObserve)}, loopauth.ScopeLoopStart, false, false},
	}
	for _, s := range vocabulary {
		cases = append(cases, struct {
			name   string
			p      loopauth.Principal
			scope  string
			allows bool
			grants bool
		}{"unknown scope restricts " + s, loopauth.Principal{Scopes: loopauth.NewScopes("foo:bar")}, s, false, false})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.p.Allows(tc.scope); got != tc.allows {
				t.Errorf("Allows(%q) = %v, want %v", tc.scope, got, tc.allows)
			}
			if got := tc.p.Grants(tc.scope); got != tc.grants {
				t.Errorf("Grants(%q) = %v, want %v", tc.scope, got, tc.grants)
			}
		})
	}
}

// The sandbox pool keys warm runners by Principal and compares Principals with
// !=, so Principal must stay comparable. This is a compile-time guard.
var _ = map[loopauth.Principal]bool{}

func TestPrincipalStaysComparable(t *testing.T) {
	a := loopauth.Principal{Tenant: "t", Scopes: loopauth.NewScopes("a")}
	b := loopauth.Principal{Tenant: "t", Scopes: loopauth.NewScopes("a")}
	if a != b {
		t.Fatal("equal Principals compare unequal")
	}
}

func TestScopeVocabulary(t *testing.T) {
	got := map[string]string{
		"start":   loopauth.ScopeLoopStart,
		"send":    loopauth.ScopeLoopSend,
		"observe": loopauth.ScopeLoopObserve,
		"admin":   loopauth.ScopeLoopAdmin,
		"caps":    loopauth.ScopeCapabilitiesRead,
		"secrets": loopauth.ScopeSecretsInline,
	}
	want := map[string]string{
		"start":   "loop:start",
		"send":    "loop:send",
		"observe": "loop:observe",
		"admin":   "loop:admin",
		"caps":    "capabilities:read",
		"secrets": "secrets:inline",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("vocabulary = %v, want %v", got, want)
	}
}
