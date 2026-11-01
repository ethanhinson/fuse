package fuse

import (
	"context"

	harnessv1 "github.com/ethanhinson/fuse/harness/v1"
)

// Capabilities is what this Client's tenant may use in a harness spec; build a
// model picker from Models and pre-flight a spec against the rest before
// StartLoop. Every listing comes from the same source the server's StartLoop
// enforces with.
type Capabilities struct {
	// Models are the models a server-mode loop can run, by alias.
	Models []ModelInfo
	// Skills are the installed skills a spec's skills[].name may reference.
	Skills []SkillInfo
	// Ceilings are the operator limits tighten-only spec fields are held to.
	Ceilings []Ceiling
	// SecretRefs are the secret names that resolve for this tenant (names
	// only). A name the operator restricted to hosts still resolves only for
	// those hosts.
	SecretRefs []string
	// SpecMCPAuthKinds are the auth types a spec-declared MCP server may use.
	SpecMCPAuthKinds []harnessv1.McpAuthType
	// SpecPolicy is the operator's floor on a spec's own URLs and model ids.
	SpecPolicy SpecPolicy
	// Server identifies the server build.
	Server ServerInfo
}

// SpecPolicy is what a server holds a spec to besides the ceilings.
type SpecPolicy struct {
	// MCPHosts are hostname patterns an MCP server or research.custom URL
	// host must match ("*.example.com" matches subdomains); empty means any
	// public host.
	MCPHosts []string
	// DenyPrivateNetworks reports that loopback, link-local and private
	// destinations are refused.
	DenyPrivateNetworks bool
	// ModelsRegistryOnly reports that a spec's model ids must be gateway ids
	// of Models.
	ModelsRegistryOnly bool
}

// ModelInfo is one routable model as the server advertises it.
type ModelInfo struct {
	// Alias is the registry name a spec selects the model by.
	Alias string
	// GatewayID is the gateway route the alias resolves to.
	GatewayID string
	// MaxOutputTokens is the effective output cap: the server's top-level
	// max_tokens when the entry sets none. It is 0 only when neither the entry
	// nor the server sets a cap.
	MaxOutputTokens int
	// ContextWindow is the effective window: the runtime default when the
	// entry sets none.
	ContextWindow int
	// ContextWindowIsDefault reports that ContextWindow is that default.
	ContextWindowIsDefault bool
	// Persona is the entry's persona.
	Persona string
	// IsDefault reports that this is the server's default alias.
	IsDefault bool
	// Label is operator display text.
	Label string
	// Description is operator display text.
	Description string
	// Tags are operator display tags (well-known: tools, vision, reasoning,
	// long-context).
	Tags []string
}

// SkillInfo is one installed skill.
type SkillInfo struct {
	// Name is what a spec names the skill by.
	Name string
	// Description is the skill's description.
	Description string
}

// CeilingDirection is which way a spec may move a tighten-only value.
type CeilingDirection int

const (
	// CeilingMax means a spec value must be strictly below the ceiling's
	// Value; a value equal to it is refused.
	CeilingMax CeilingDirection = iota + 1
	// CeilingMin means a spec value must be strictly above the ceiling's
	// Value; a value equal to it is refused.
	CeilingMin
)

// Ceiling is the operator limit one tighten-only spec field is held to.
// FieldPath is in harness.Violation.FieldPath vocabulary, so a refusal from
// StartLoop names the same path. The limit is strict: a spec value equal to
// Value is refused, so keep it strictly inside the limit in Direction.
type Ceiling struct {
	// FieldPath names the spec field.
	FieldPath string
	// OperatorSet reports whether the operator set a value. False means a spec
	// cannot set the field at all.
	OperatorSet bool
	// Value is the operator value; meaningful only when OperatorSet.
	Value float64
	// Direction is which way a spec may move the value.
	Direction CeilingDirection
}

// ServerInfo identifies the server build.
type ServerInfo struct {
	// Version is the fuse build version.
	Version string
	// DigestAlgorithm is the id that prefixes every harness spec digest.
	DigestAlgorithm string
}

// Capabilities asks the runtime what this Client's tenant may use. A runtime
// that does not advertise (every NewLocal Client, or a server without the
// call) fails with ErrCapabilitiesUnsupported.
func (c *Client) Capabilities(ctx context.Context) (Capabilities, error) {
	return c.backend.capabilities(ctx)
}
