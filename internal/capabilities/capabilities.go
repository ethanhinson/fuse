// Package capabilities is the answer a fuse loop server gives to
// LoopService.GetCapabilities. Plain Go types only; the Connect edge maps them
// to proto. Every listing reads the source StartLoop enforces against, so an
// advertisement and a StartLoop refusal cannot disagree.
package capabilities

import (
	"slices"
	"strings"

	"github.com/ethanhinson/fuse/harness"
	harnessv1 "github.com/ethanhinson/fuse/harness/v1"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/skills"
	"github.com/ethanhinson/fuse/internal/version"
)

// Capabilities is the whole answer for one tenant.
type Capabilities struct {
	// Models are the registry entries a server-mode loop can run.
	Models []Model
	// Skills are the installed skills a spec may name.
	Skills []Skill
	// Ceilings are the operator limits tighten-only spec fields are held to.
	Ceilings []Ceiling
	// SecretRefs are the secret names that resolve for the tenant.
	SecretRefs []string
	// SpecMCPAuthKinds are the MCP auth kinds a spec may use.
	SpecMCPAuthKinds []harnessv1.McpAuthType
	// Server identifies the server build.
	Server Server
}

// Model is one routable model as a client sees it.
type Model struct {
	// Alias is the registry name a spec selects the model by.
	Alias string
	// GatewayID is the gateway route the alias resolves to.
	GatewayID string
	// MaxOutputTokens is the effective output cap: the server's fallback when
	// the entry sets 0. It is 0 only when neither sets a cap.
	MaxOutputTokens int
	// ContextWindow is the effective window: the runtime default when the
	// entry sets 0.
	ContextWindow int
	// ContextWindowIsDefault reports that ContextWindow is that default.
	ContextWindowIsDefault bool
	// Persona is the entry's persona.
	Persona string
	// IsDefault reports that this is the registry's default alias.
	IsDefault bool
	// Label is operator display text.
	Label string
	// Description is operator display text.
	Description string
	// Tags are operator display tags.
	Tags []string
}

// Skill is one installed skill.
type Skill struct {
	// Name is what a spec names the skill by.
	Name string
	// Description is the skill's frontmatter description.
	Description string
}

// Direction is which way a spec may move a tighten-only value.
type Direction int

const (
	// DirectionMax means a spec may only lower the value.
	DirectionMax Direction = iota + 1
	// DirectionMin means a spec may only raise the value (unused today).
	DirectionMin
)

// Ceiling is the operator floor one tighten-only spec field is held to.
// FieldPath is in harness.Violation.FieldPath vocabulary. OperatorSet false
// means a spec cannot set the field at all; Value is meaningful only when set.
type Ceiling struct {
	// FieldPath names the spec field.
	FieldPath string
	// OperatorSet reports whether the operator set a value.
	OperatorSet bool
	// Value is the operator value.
	Value float64
	// Direction is which way a spec may move the value.
	Direction Direction
}

// Server identifies the server build.
type Server struct {
	// Version is the fuse build version.
	Version string
	// DigestAlgorithm is the id that prefixes every spec digest.
	DigestAlgorithm string
}

// Models lists, sorted by alias, the registry entries a server-mode loop can
// run: every entry except cli/ routes (a local interactive adapter a headless
// server cannot drive) and entries the operator hid. defaultContextWindow
// fills an entry whose ContextWindow is 0, and defaultMaxOutputTokens (the
// operator's top-level max_tokens) an entry whose MaxTokens is 0, so each
// advertised value is the one a loop runs with.
func Models(reg *model.Registry, defaultContextWindow, defaultMaxOutputTokens int) []Model {
	if reg == nil {
		return nil
	}
	var out []Model
	for _, e := range reg.Entries() {
		mc := e.Config
		if mc.Hidden || strings.HasPrefix(mc.ID, "cli/") {
			continue
		}
		m := Model{
			Alias:           e.Alias,
			GatewayID:       mc.ID,
			MaxOutputTokens: mc.MaxTokens,
			ContextWindow:   mc.ContextWindow,
			Persona:         mc.Persona,
			IsDefault:       e.Alias == reg.Default,
			Label:           mc.Label,
			Description:     mc.Description,
			Tags:            slices.Clone(mc.Tags),
		}
		if m.MaxOutputTokens == 0 {
			m.MaxOutputTokens = defaultMaxOutputTokens
		}
		m.MaxOutputTokens = max(m.MaxOutputTokens, 0)
		if m.ContextWindow <= 0 {
			m.ContextWindow, m.ContextWindowIsDefault = defaultContextWindow, true
		}
		out = append(out, m)
	}
	return out
}

// Skills lists the installed skills in discovery order, once per name. The
// first skill of a name is kept because Lookup, which StartLoop resolves
// names through, returns the first.
func Skills(set *skills.Set) []Skill {
	if set == nil {
		return nil
	}
	var out []Skill
	seen := map[string]bool{}
	for _, sk := range set.All() {
		if seen[sk.Name] {
			continue
		}
		seen[sk.Name] = true
		out = append(out, Skill{Name: sk.Name, Description: sk.Description})
	}
	return out
}

// ServerInfo reports this build's version and spec digest algorithm.
func ServerInfo() Server {
	return Server{Version: version.Version, DigestAlgorithm: harness.DigestAlgorithm}
}
