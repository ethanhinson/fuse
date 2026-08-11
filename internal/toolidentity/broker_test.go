package toolidentity

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/loopauth"
)

func testBroker(t *testing.T, static map[string]StaticCredential, audit AuditSink) *Broker {
	t.Helper()
	sts, err := NewBuiltinSTS(BuiltinSTSConfig{
		TTL: time.Minute,
		TenantKeys: map[event.TenantID][]byte{
			"tenantA": []byte("key-a-000000000000000000000000000000"),
			"tenantB": []byte("key-b-111111111111111111111111111111"),
		},
	})
	if err != nil {
		t.Fatalf("sts: %v", err)
	}
	return NewBroker(sts, static, audit)
}

// recordingAudit captures audit records for assertions.
type recordingAudit struct{ records []AuditRecord }

func (r *recordingAudit) Record(rec AuditRecord) { r.records = append(r.records, rec) }

// An OAuth-tier target yields a delegated, identity-carrying credential and an
// audit line that never contains the token.
func TestBroker_OAuthTierDelegates(t *testing.T) {
	audit := &recordingAudit{}
	b := testBroker(t, nil, audit)
	p := loopauth.Principal{Tenant: "tenantA", Subject: "alice"}
	tgt := Target{Name: "srv", Audience: "https://api.example.com", Scopes: []string{"read"}, Tier: TierOAuth}

	cred, err := b.CredentialFor(context.Background(), p, tgt)
	if err != nil {
		t.Fatalf("CredentialFor: %v", err)
	}
	if !cred.CarriesIdentity() {
		t.Fatal("an OAuth-tier credential must carry identity")
	}
	if cred.Header() == "" {
		t.Fatal("expected a bearer header")
	}
	if len(audit.records) != 1 {
		t.Fatalf("want 1 audit record, got %d", len(audit.records))
	}
	rec := audit.records[0]
	if rec.Subject != "alice" || rec.Tenant != "tenantA" || rec.Target != "srv" || rec.Tier != "oauth" {
		t.Fatalf("audit record fields wrong: %+v", rec)
	}
	if strings.Contains(rec.String(), cred.rawTokenForTest()) {
		t.Fatal("audit record leaked the token")
	}
}

// A static-tier target returns the configured static credential and carries NO
// initiator identity; the audit flags the weaker tier.
func TestBroker_StaticTierIdentityFree(t *testing.T) {
	audit := &recordingAudit{}
	static := map[string]StaticCredential{
		"legacy": {Scheme: "Bearer", Token: "legacy-static-token"},
	}
	b := testBroker(t, static, audit)
	p := loopauth.Principal{Tenant: "tenantA", Subject: "alice"}
	tgt := Target{Name: "legacy", Tier: TierStatic}

	cred, err := b.CredentialFor(context.Background(), p, tgt)
	if err != nil {
		t.Fatalf("CredentialFor: %v", err)
	}
	if cred.CarriesIdentity() {
		t.Fatal("a static credential must NOT carry initiator identity")
	}
	if cred.Header() != "Bearer legacy-static-token" {
		t.Fatalf("static header = %q", cred.Header())
	}
	if len(audit.records) != 1 || audit.records[0].Tier != "static" {
		t.Fatalf("audit should flag the static tier: %+v", audit.records)
	}
}

// A static-tier target with no configured credential is a distinct error — never
// a silent fallback to OAuth or to another server's credential.
func TestBroker_StaticTierNoCredentialErrors(t *testing.T) {
	b := testBroker(t, nil, nil)
	p := loopauth.Principal{Tenant: "tenantA", Subject: "alice"}
	_, err := b.CredentialFor(context.Background(), p, Target{Name: "legacy", Tier: TierStatic})
	if err == nil {
		t.Fatal("static tier with no configured credential must error")
	}
}

// An undeclared target (OAuth tier, no audience) is denied at the seam.
func TestBroker_UndeclaredTargetDenied(t *testing.T) {
	b := testBroker(t, nil, nil)
	p := loopauth.Principal{Tenant: "tenantA", Subject: "alice"}
	_, err := b.CredentialFor(context.Background(), p, Target{Name: "srv", Tier: TierOAuth})
	if err == nil {
		t.Fatal("an undeclared (no-audience) OAuth target must be denied")
	}
}

// Two tenants resolving the same audience get isolated, tenant-bound tokens: the
// token minted for tenant A does not verify under tenant B's key.
func TestBroker_PerTenantIsolation(t *testing.T) {
	b := testBroker(t, nil, nil)
	tgt := Target{Name: "srv", Audience: "https://api.example.com", Tier: TierOAuth}

	credA, err := b.CredentialFor(context.Background(), loopauth.Principal{Tenant: "tenantA", Subject: "alice"}, tgt)
	if err != nil {
		t.Fatalf("A: %v", err)
	}
	credB, err := b.CredentialFor(context.Background(), loopauth.Principal{Tenant: "tenantB", Subject: "bob"}, tgt)
	if err != nil {
		t.Fatalf("B: %v", err)
	}
	if credA.Header() == credB.Header() {
		t.Fatal("two tenants must not receive the same token for the same audience")
	}
	sts := b.exch.(*BuiltinSTS)
	if err := sts.Verify("tenantA", credA.rawTokenForTest()); err != nil {
		t.Fatalf("A token should verify under tenant A: %v", err)
	}
	if err := sts.Verify("tenantB", credA.rawTokenForTest()); err == nil {
		t.Fatal("A's token must not verify under tenant B (cross-tenant material reuse)")
	}
}

// The broker holds no long-lived secret for OAuth targets — only the exchanger
// seam and static creds. Structural assertion: with no static map, the broker
// carries no stored OAuth bearer.
func TestBroker_NoLongLivedOAuthSecret(t *testing.T) {
	b := testBroker(t, nil, nil)
	if len(b.static) != 0 {
		t.Fatal("expected no static (long-lived) credentials configured")
	}
	// OAuth credentials come only from the exchanger, minted per call.
	if b.exch == nil {
		t.Fatal("broker must resolve OAuth via the exchanger seam")
	}
}
