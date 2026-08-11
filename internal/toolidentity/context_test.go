package toolidentity

import (
	"context"
	"testing"

	"github.com/ethanhinson/fuse/internal/loopauth"
)

// The principal placed at loop-start is the exact one read at the tool-execute
// site.
func TestPrincipalRoundTrips(t *testing.T) {
	p := loopauth.Principal{Tenant: "acme", Subject: "alice"}
	ctx := WithPrincipal(context.Background(), p)

	got, ok := PrincipalFrom(ctx)
	if !ok {
		t.Fatal("PrincipalFrom found no principal on a WithPrincipal context")
	}
	if got != p {
		t.Fatalf("PrincipalFrom = %+v, want %+v", got, p)
	}
}

// A context with no principal reports ok=false and the zero principal — the
// egress seam treats this as "no identity" and (for an OAuth target) fails
// closed, never as an empty/spoofable identity it would silently accept.
func TestPrincipalFromEmpty(t *testing.T) {
	got, ok := PrincipalFrom(context.Background())
	if ok {
		t.Fatal("PrincipalFrom on a bare context must report ok=false")
	}
	if got != (loopauth.Principal{}) {
		t.Fatalf("PrincipalFrom on a bare context returned %+v, want zero", got)
	}
}

// The carrier key is unexported: only WithPrincipal can set it. A caller cannot
// forge a principal by stashing a look-alike value under a string key — proving
// model-supplied context values can never overwrite the authenticated identity.
func TestPrincipalKeyIsPrivate(t *testing.T) {
	// A value stashed under a string key (the only thing untrusted code could
	// attempt) is invisible to PrincipalFrom.
	ctx := context.WithValue(context.Background(), "principal", loopauth.Principal{Subject: "attacker"}) //nolint:staticcheck
	if _, ok := PrincipalFrom(ctx); ok {
		t.Fatal("PrincipalFrom must ignore a principal stashed under a foreign key")
	}
}

// The most recent WithPrincipal wins (child context overrides), but there is no
// setter that mutates in place — each override is a fresh derived context.
func TestPrincipalOverride(t *testing.T) {
	base := WithPrincipal(context.Background(), loopauth.Principal{Subject: "first"})
	child := WithPrincipal(base, loopauth.Principal{Subject: "second"})
	if got, _ := PrincipalFrom(child); got.Subject != "second" {
		t.Fatalf("child override = %q, want second", got.Subject)
	}
	if got, _ := PrincipalFrom(base); got.Subject != "first" {
		t.Fatalf("base unchanged = %q, want first", got.Subject)
	}
}
