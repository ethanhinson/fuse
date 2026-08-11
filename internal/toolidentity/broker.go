package toolidentity

import (
	"context"
	"errors"
	"fmt"

	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/loopauth"
)

// StaticCredential is an explicit, per-server legacy credential for a TierStatic
// target. It carries NO initiator identity — it is fuse-as-client, not
// delegation. Configured only from trusted config, never a silent default.
type StaticCredential struct {
	Scheme string
	Token  string
}

// AuditRecord is one credential-mint audit line: who, for what, at which tier,
// with the delegation chain. It NEVER contains the token itself.
type AuditRecord struct {
	Subject  string
	Tenant   event.TenantID
	Target   string
	Audience string
	Scopes   []string
	Tier     string // "oauth" | "static"
	ActChain string
}

// String renders the audit record without any secret.
func (r AuditRecord) String() string {
	return fmt.Sprintf("credential mint subject=%s tenant=%s target=%s audience=%s tier=%s scopes=%v act=%s",
		r.Subject, r.Tenant, r.Target, r.Audience, r.Tier, r.Scopes, r.ActChain)
}

// AuditSink receives one AuditRecord per credential resolution. Implementations
// must never persist the token (there is none in the record). A nil sink is a
// no-op.
type AuditSink interface {
	Record(AuditRecord)
}

// ErrNoStaticCredential is returned for a TierStatic target with no configured
// credential — never a silent fallback to OAuth or another server's credential.
var ErrNoStaticCredential = errors.New("toolidentity: static-tier target has no configured credential")

// ErrUndeclaredTarget is returned for a target that is not declared well enough
// to reach (a TierOAuth target with no audience).
var ErrUndeclaredTarget = errors.New("toolidentity: target is undeclared")

// Broker is the production CredentialSource. It fronts the TokenExchanger for
// OAuth-tier targets (minting a delegated, audience-bound token per call), serves
// an explicit static credential for static-tier targets (identity-free), and
// emits one audit record per resolution. It holds no long-lived secret for OAuth
// targets — only the exchanger seam and any explicitly-configured static creds.
//
// Per-tenant isolation is inherited from the exchanger: each minted token is
// signed under the requesting tenant's own key, so a tenant's credential material
// is never reachable by another tenant (D4). The broker keeps no cross-call
// credential cache in this cut — short-lived tokens are minted per call — so
// there is no cache-key-crossing hazard to guard here.
type Broker struct {
	exch   TokenExchanger
	static map[string]StaticCredential
	audit  AuditSink
}

// NewBroker builds a Broker. static may be nil (no legacy servers); audit may be
// nil (no-op).
func NewBroker(exch TokenExchanger, static map[string]StaticCredential, audit AuditSink) *Broker {
	cp := make(map[string]StaticCredential, len(static))
	for k, v := range static {
		cp[k] = v
	}
	return &Broker{exch: exch, static: cp, audit: audit}
}

// CredentialFor resolves the credential for principal p to reach target t.
func (b *Broker) CredentialFor(ctx context.Context, p loopauth.Principal, t Target) (Credential, error) {
	if !t.Valid() {
		return Credential{}, fmt.Errorf("%w: %q (tier=%d)", ErrUndeclaredTarget, t.Name, t.Tier)
	}

	switch t.Tier {
	case TierStatic:
		sc, ok := b.static[t.Name]
		if !ok {
			return Credential{}, fmt.Errorf("%w: %q", ErrNoStaticCredential, t.Name)
		}
		b.emit(AuditRecord{
			Subject: p.Subject, Tenant: p.Tenant, Target: t.Name,
			Audience: t.Audience, Scopes: t.Scopes, Tier: "static",
		})
		return NewCredential(sc.Scheme, sc.Token, false), nil

	default: // TierOAuth
		if b.exch == nil {
			return Credential{}, errors.New("toolidentity: no token exchanger configured for an OAuth-tier target")
		}
		res, err := b.exch.Exchange(ctx, ExchangeRequest{
			Tenant:   p.Tenant,
			Subject:  p.Subject,
			Actor:    "fuse",
			Audience: t.Audience,
			Scopes:   t.Scopes,
		})
		if err != nil {
			return Credential{}, fmt.Errorf("toolidentity: exchange for %q: %w", t.Name, err)
		}
		b.emit(AuditRecord{
			Subject: p.Subject, Tenant: p.Tenant, Target: t.Name,
			Audience: t.Audience, Scopes: t.Scopes, Tier: "oauth", ActChain: res.ActChain,
		})
		return NewCredential("Bearer", res.Token, true), nil
	}
}

func (b *Broker) emit(r AuditRecord) {
	if b.audit != nil {
		b.audit.Record(r)
	}
}
