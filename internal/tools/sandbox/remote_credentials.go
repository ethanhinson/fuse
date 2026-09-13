package sandbox

// THE CREDENTIAL SEAM BETWEEN A REMOTE SUBSTRATE AND THE PROXY (change 0075).
//
// A remote substrate has to hand each sandbox the material its forwarder reaches
// the proxy with. It cannot call Proxy.Enroll directly: Proxy lives in this
// package, and internal/tools/sandbox/kubernetes imports this package — the
// dependency runs one way only (ADR-0058 rule 1), so the substrate needs a
// declared interface rather than a concrete type.
//
// The seam is deliberately NARROW. It exposes minting and revocation and nothing
// else: no listener address, no CA, no policy. A substrate that could read the
// policy would be a substrate that could be tempted to render it Pod-side, which
// is the one thing the whole egress design refuses — the relay understands no
// HTTP and no policy lives on the sandbox's side of the boundary.

import (
	"github.com/ethanhinson/fuse/internal/loopauth"
)

// PrincipalKey is the principal a credential is minted for.
//
// It is loopauth.Principal under another name so the seam's shape says what it
// means: the credential's identity is the PRINCIPAL's, fixed at enrollment, and
// not the sandbox's or the Pod's. Two sandboxes of one principal hold two
// credentials that select the same policy.
type PrincipalKey = loopauth.Principal

// SandboxCredential is the PEM material a sandbox's forwarder needs.
//
// It mirrors ClientCredential's three PEM fields and deliberately omits the
// serial: a substrate writes these bytes into a Secret and has no use for the
// proxy's registry key, and a serial in a substrate's hands is a value that could
// end up in a log or a label.
type SandboxCredential struct {
	// CertPEM is the client certificate the forwarder presents.
	CertPEM []byte
	// KeyPEM is that certificate's private key.
	KeyPEM []byte
	// CAPEM is the CA the forwarder verifies the PROXY against.
	CAPEM []byte
}

// SandboxCredentialSource mints and revokes the transport credentials a remote
// substrate's sandboxes reach the egress proxy with.
//
// *Proxy satisfies it (see proxyCredentialSource below), and the composition root
// is the only place the two meet.
type SandboxCredentialSource interface {
	// EnrollSandbox mints one credential for p, taking a lease on p's policy. The
	// caller MUST pair it with ReleaseSandbox, or the policy and the credential
	// outlive the sandbox.
	EnrollSandbox(p PrincipalKey) (SandboxCredential, error)

	// ReleaseSandbox drops one lease, revoking the credentials minted under it
	// when the last one goes. It is idempotent and never an error: teardown paths
	// call it unconditionally, and "there was nothing to release" is a normal
	// outcome for a Provision that failed after enrolling.
	ReleaseSandbox(p PrincipalKey)
}

// proxyCredentialSource adapts a Proxy onto SandboxCredentialSource by binding
// the POLICY at construction.
//
// The policy is bound here rather than passed per Enroll for the reason Listen
// takes it per principal: it is process-global configuration (Config.Egress), and
// a substrate that supplied it per call would be a substrate that could supply a
// different one — which is exactly the "which posture is this sandbox running"
// ambiguity ADR-0058 rule 2 exists to prevent.
type proxyCredentialSource struct {
	proxy  *Proxy
	policy Egress
}

// NewProxyCredentialSource binds p's enrollment to one policy.
//
// The composition root calls it with the same resolved Config.Egress the
// substrate is built from, so the policy a sandbox is served under and the posture
// its Pod was rendered to are the same value.
func NewProxyCredentialSource(p *Proxy, policy Egress) SandboxCredentialSource {
	return proxyCredentialSource{proxy: p, policy: policy}
}

// EnrollSandbox mints one credential under the bound policy.
func (s proxyCredentialSource) EnrollSandbox(p PrincipalKey) (SandboxCredential, error) {
	cred, err := s.proxy.Enroll(p, s.policy)
	if err != nil {
		return SandboxCredential{}, err
	}
	return SandboxCredential{CertPEM: cred.CertPEM, KeyPEM: cred.KeyPEM, CAPEM: cred.CAPEM}, nil
}

// ReleaseSandbox drops one lease. The error is dropped deliberately: Release's
// only failure is a listener teardown fault, which is not something a caller on a
// sandbox-teardown path can act on, and reporting it would tempt a caller into
// retrying a release.
func (s proxyCredentialSource) ReleaseSandbox(p PrincipalKey) {
	_ = s.proxy.Release(p)
}

var _ SandboxCredentialSource = proxyCredentialSource{}
