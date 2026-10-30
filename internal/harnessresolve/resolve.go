// Package harnessresolve turns a client's fuse.harness.v1 HarnessSpec into the
// configuration one loop runs with on a fuse server (change 0085).
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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"google.golang.org/protobuf/proto"

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
	// Digest identifies the spec's content (see Digest).
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
	return Resolved{Config: cfg, Skills: skills, Digest: Digest(spec)}, nil
}

// Digest is the spec's content digest: "sha256:" and the hex SHA-256 of its
// deterministic protobuf encoding. A nil spec digests as the empty spec.
func Digest(spec *harnessv1.HarnessSpec) string {
	if spec == nil {
		spec = &harnessv1.HarnessSpec{}
	}
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(spec)
	if err != nil {
		// A generated message with no unknown-field quirks always marshals; a
		// failure here is a programming error, not a spec problem.
		panic(fmt.Sprintf("harnessresolve: marshal spec: %v", err))
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// secretRef is one secret_ref field of a spec.
type secretRef struct {
	path string
	name string
}

// secretRefs lists every secret_ref the spec sets, in a stable order.
func secretRefs(spec *harnessv1.HarnessSpec) []secretRef {
	var out []secretRef
	for i, s := range spec.GetMcpServers() {
		if n := s.GetAuth().GetClientSecretRef(); n != "" {
			out = append(out, secretRef{path: fmt.Sprintf("mcp_servers[%d].auth.client_secret_ref", i), name: n})
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
			out = append(out, secretRef{path: fmt.Sprintf("research.custom.headers[%s].secret_ref", strconv.Quote(k)), name: n})
		}
	}
	return out
}

// resolveSecrets resolves every secret_ref once per name. A name that does not
// resolve is a violation at each field that references it, with one fixed
// message whatever the cause, so a client cannot probe which secrets exist.
func (r Resolver) resolveSecrets(ctx context.Context, tenant string, spec *harnessv1.HarnessSpec) (map[string]string, []harnessconfig.Problem) {
	values := map[string]string{}
	failed := map[string]bool{}
	var problems []harnessconfig.Problem
	for _, ref := range secretRefs(spec) {
		if _, ok := values[ref.name]; !ok && !failed[ref.name] {
			var v string
			var err error
			if r.Secrets == nil {
				err = ErrSecretUnavailable
			} else {
				v, err = r.Secrets.ResolveSecret(ctx, tenant, ref.name)
			}
			if err != nil {
				failed[ref.name] = true
			} else {
				values[ref.name] = v
			}
		}
		if failed[ref.name] {
			problems = append(problems, harnessconfig.Problem{
				FieldPath: ref.path,
				Message:   fmt.Sprintf("secret %q is not available to this tenant", ref.name),
				RuleID:    harness.RuleSecretRef,
			})
		}
	}
	return values, problems
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
// spec field path and the operator's current value ("" when unset).
func (r Resolver) tightenField(key string) (path, limit string) {
	num := func(v int) string {
		if v <= 0 {
			return ""
		}
		return strconv.Itoa(v)
	}
	b := r.Base
	if rest, ok := strings.CutPrefix(key, "workflows."); ok {
		// workflows.<name>.pool.<field>; a workflow name cannot contain a dot
		// (the spec's key pattern), so the last two segments are pool.<field>.
		i := strings.LastIndex(rest, ".pool.")
		name, field := rest[:i], rest[i+len(".pool."):]
		p := b.Workflows[name].Pool
		v := map[string]int{"concurrent": p.Concurrent, "total": p.Total, "max_depth": p.MaxDepth, "tokens": p.Tokens}[field]
		return fmt.Sprintf("workflows[%s].pool.%s", strconv.Quote(name), field), num(v)
	}
	switch key {
	case "agents.queue_bound":
		if b.Agents.QueueBound > 0 {
			limit = strconv.FormatFloat(b.Agents.QueueBound, 'g', -1, 64)
		}
	case "agents.tool_timeout_seconds":
		limit = num(b.Agents.ToolTimeoutSeconds)
	case "throughput.requests_per_minute":
		limit = num(b.Throughput.RequestsPerMinute)
	case "throughput.tokens_per_minute":
		limit = num(b.Throughput.TokensPerMinute)
	case "throughput.session_tokens":
		limit = num(b.Throughput.SessionTokens)
	case "pipeline.synthesis.max_steps":
		limit = num(b.Pipeline.Synthesis.MaxSteps)
	case "pipeline.synthesis.max_fanout":
		limit = num(b.Pipeline.Synthesis.MaxFanout)
	case "pipeline.synthesis.max_depth":
		limit = num(b.Pipeline.Synthesis.MaxDepth)
	case "pipeline.synthesis.max_attempts":
		limit = num(b.Pipeline.Synthesis.MaxAttempts)
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
