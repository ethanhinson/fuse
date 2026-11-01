// Package harnessresolve turns a client's fuse.harness.v1 HarnessSpec into the
// configuration one loop runs with on a fuse server (change 0085, ADR-0061).
//
// One networked fuse server serves many tenants and clients, and each may
// start a loop with its own harness. The server's trusted operator config is
// the floor: the spec is layered over it (config.OverlaySpec, through
// harnessconfig.Overlay), tighten-only fields may only tighten the operator's
// values, and operator-only surfaces always come from the server. Before any
// loop exists the spec is validated as a whole - the shared shape and semantic
// rules (the public harness validator's), the tighten-only floor, skill names
// against the skills the server has installed, and every secret_ref against
// the server's SecretResolver - and every violation is reported with its spec
// field path in one *harness.ValidationError.
//
// A resolved Config holds resolved secret VALUES. It lives only in memory for
// the loop it was resolved for: never log it, persist it, or put it in an
// error. What a loop records durably is the spec (secret names only) and its
// Digest.
package harnessresolve

import (
	"context"
	"fmt"
	"sort"
	"strconv"

	"github.com/ethanhinson/fuse/harness"
	harnessv1 "github.com/ethanhinson/fuse/harness/v1"
	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/harnessconfig"
)

// Resolver resolves specs over one server's operator config.
type Resolver struct {
	// Base is the server operator's trusted config: the floor every spec is
	// layered over.
	Base config.Config
	// Skills reports whether a skill name is installed on the server. Nil means
	// no skills are installed, so every SkillRef is refused.
	Skills func(name string) bool
	// Secrets resolves secret_ref names for the loop's tenant. Nil denies every
	// reference.
	Secrets SecretResolver
}

// Resolved is a validated spec resolved over the operator config.
type Resolved struct {
	// Config is the loop's resolved config. It holds resolved secret values.
	Config config.Config
	// Skills are the names of the skills the spec makes available, in spec
	// order; nil when the spec names none (the server's full set applies).
	Skills []string
	// Digest identifies the spec's content (harness.Digest).
	Digest string
}

// Resolve validates spec for a loop of tenant and resolves it over r.Base. Any
// problem returns a *harness.ValidationError listing every violation. A nil
// spec is the empty spec, which resolves to r.Base unchanged.
func (r Resolver) Resolve(ctx context.Context, tenant string, spec *harnessv1.HarnessSpec) (Resolved, error) {
	if spec == nil {
		spec = &harnessv1.HarnessSpec{}
	}
	shape := toProblems(harness.ValidateShape(spec))
	for _, p := range shape {
		if p.RuleID == harness.RuleInternal {
			return Resolved{}, &harness.ValidationError{Violations: fromProblems(shape)}
		}
	}

	values, secretProblems := r.resolveSecrets(ctx, tenant, spec)
	skills, skillProblems := r.resolveSkills(spec)
	skillProblems = append(skillProblems, operatorOnlyProblems(spec)...)
	skillProblems = append(skillProblems, r.specPolicyProblems(ctx, spec)...)

	var mapped []harnessconfig.Problem
	cfg, refused, err := harnessconfig.Overlay(r.Base, spec, func(name string) (string, error) {
		if v, ok := values[name]; ok {
			return v, nil
		}
		// An unavailable secret is already a violation; keep mapping so every
		// other problem is reported in the same response.
		return harnessconfig.SecretPlaceholder(name), nil
	})
	if err != nil {
		mapped = []harnessconfig.Problem{{Message: err.Error(), RuleID: harness.RuleMapping}}
	} else {
		mapped = r.tightenProblems(refused)
		mapped = append(mapped, harnessconfig.SemanticProblems(cfg)...)
	}

	if ps := harnessconfig.Combine(shape, secretProblems, skillProblems, mapped); len(ps) > 0 {
		return Resolved{}, &harness.ValidationError{Violations: fromProblems(ps)}
	}
	return Resolved{Config: cfg, Skills: skills, Digest: harness.Digest(spec)}, nil
}

// secretRef is one secret_ref field of a spec.
type secretRef struct {
	path string
	name string
	// destination is the URL the resolved value will be sent to.
	destination string
}

// secretRefs lists every secret_ref the spec sets, in a stable order.
func secretRefs(spec *harnessv1.HarnessSpec) []secretRef {
	var out []secretRef
	for i, s := range spec.GetMcpServers() {
		if n := s.GetAuth().GetClientSecretRef(); n != "" {
			out = append(out, secretRef{path: fmt.Sprintf("mcp_servers[%d].auth.client_secret_ref", i), name: n, destination: s.GetUrl()})
		}
	}
	headers := spec.GetResearch().GetCustom().GetHeaders()
	names := make([]string, 0, len(headers))
	for k := range headers {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		if n := headers[k].GetSecretRef(); n != "" {
			out = append(out, secretRef{path: fmt.Sprintf("research.custom.headers[%s].secret_ref", strconv.Quote(k)), name: n, destination: spec.GetResearch().GetCustom().GetUrl()})
		}
	}
	return out
}

// resolveSecrets resolves every secret_ref for its destination. A reference
// that does not resolve is a violation at its field, with one fixed message
// whatever the cause, so a client cannot probe which secrets exist or where
// they may be sent. The overlay later looks values up by name, so a name that
// resolved for one destination and failed for another is a violation either way.
func (r Resolver) resolveSecrets(ctx context.Context, tenant string, spec *harnessv1.HarnessSpec) (map[string]string, []harnessconfig.Problem) {
	values := map[string]string{}
	failed := map[string]bool{}
	var problems []harnessconfig.Problem
	for _, ref := range secretRefs(spec) {
		var v string
		err := ErrSecretUnavailable
		if r.Secrets != nil {
			v, err = r.Secrets.ResolveSecret(ctx, tenant, ref.name, ref.destination)
		}
		if err != nil {
			failed[ref.name] = true
			problems = append(problems, harnessconfig.Problem{
				FieldPath: ref.path,
				Message:   fmt.Sprintf("secret %q is not available to this tenant", ref.name),
				RuleID:    harness.RuleSecretRef,
			})
			continue
		}
		values[ref.name] = v
	}
	for name := range failed {
		delete(values, name)
	}
	return values, problems
}

// operatorOnlyProblems reports spec values only the server's operator may
// configure: an MCP server whose auth type reads or writes the host's token
// cache and may open an interactive browser flow on the server (oauth2), or
// mints fuse-signed delegation tokens for an audience (identity,
// oauth-exchange). A client could otherwise name a server after one the
// operator authorized and have the cached token sent to its own URL.
func operatorOnlyProblems(spec *harnessv1.HarnessSpec) []harnessconfig.Problem {
	var out []harnessconfig.Problem
	for i, s := range spec.GetMcpServers() {
		if t := s.GetAuth().GetType(); !specAuthAllowed(t) {
			out = append(out, harnessconfig.Problem{
				FieldPath: fmt.Sprintf("mcp_servers[%d].auth.type", i),
				Message:   fmt.Sprintf("auth type %s is configured by the server operator only; use none, bearer or static in a harness spec", t),
				RuleID:    harness.RuleOperatorOnly,
			})
		}
	}
	return out
}

// resolveSkills checks every SkillRef against the installed skills.
func (r Resolver) resolveSkills(spec *harnessv1.HarnessSpec) ([]string, []harnessconfig.Problem) {
	var names []string
	var problems []harnessconfig.Problem
	for i, ref := range spec.GetSkills() {
		name := ref.GetName()
		names = append(names, name)
		if r.Skills == nil || !r.Skills(name) {
			problems = append(problems, harnessconfig.Problem{
				FieldPath: fmt.Sprintf("skills[%d].name", i),
				Message:   fmt.Sprintf("skill %q is not installed on this server", name),
				RuleID:    harness.RuleSkillRef,
			})
		}
	}
	return names, problems
}

// tightenProblems reports each refused tighten-only loosening at its spec path,
// naming the operator's value the spec may only lower.
func (r Resolver) tightenProblems(refused []string) []harnessconfig.Problem {
	out := make([]harnessconfig.Problem, 0, len(refused))
	for _, key := range refused {
		path, limit := r.tightenField(key)
		msg := "may only tighten a limit this server sets, and it sets none here"
		if limit != "" {
			msg = "may only tighten this server's limit of " + limit
		}
		out = append(out, harnessconfig.Problem{FieldPath: path, Message: msg, RuleID: harness.RuleTightenOnly})
	}
	return out
}

// tightenField maps a refused config key (config.OverlaySpec's form) to its
// spec field path and the operator's current value ("" when unset). The value
// is read through operatorLimit, the accessor Ceilings advertises from.
func (r Resolver) tightenField(key string) (path, limit string) {
	if v, set, _ := r.operatorLimit(key); set {
		limit = strconv.FormatFloat(v, 'f', -1, 64) // 'f': 2000000 must not render as 2e+06
	}
	return harnessconfig.SpecPath(key), limit
}

func toProblems(vs []harness.Violation) []harnessconfig.Problem {
	out := make([]harnessconfig.Problem, len(vs))
	for i, v := range vs {
		out[i] = harnessconfig.Problem{FieldPath: v.FieldPath, Message: v.Message, RuleID: v.RuleID}
	}
	return out
}

func fromProblems(ps []harnessconfig.Problem) []harness.Violation {
	out := make([]harness.Violation, len(ps))
	for i, p := range ps {
		out[i] = harness.Violation{FieldPath: p.FieldPath, Message: p.Message, RuleID: p.RuleID}
	}
	return out
}
