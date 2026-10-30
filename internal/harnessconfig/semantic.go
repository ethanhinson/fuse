package harnessconfig

import (
	"fmt"
	"sort"

	"gopkg.in/yaml.v3"

	harnessv1 "github.com/ethanhinson/fuse/harness/v1"
	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/configcheck"
)

// Rule ids for problems found outside the protovalidate shape layer. The public
// validator (github.com/ethanhinson/fuse/harness) re-exports them.
const (
	// RuleConfig marks a structural rule from internal/config.
	RuleConfig = "fuse.config"
	// RuleModelRef marks a model alias that resolves to no configured or
	// built-in model.
	RuleModelRef = "fuse.model_ref"
	// RuleMapping marks a spec the runtime could not map into its config.
	RuleMapping = "fuse.mapping"
	// RuleInternal marks a validator failure (not a problem with the spec).
	RuleInternal = "fuse.internal"
	// RuleTightenOnly marks a tighten-only field whose value would loosen the
	// limit the server's operator set (change 0085).
	RuleTightenOnly = "fuse.tighten_only"
	// RuleSkillRef marks a skill name the server has not installed (change 0085).
	RuleSkillRef = "fuse.skill_ref"
	// RuleSecretRef marks a secret name the server will not resolve for the
	// loop's tenant (change 0085).
	RuleSecretRef = "fuse.secret_ref"
)

// Problem is one validation problem located at a HarnessSpec field path (empty
// when no client-visible field is at fault). It is the internal form of the
// public harness.Violation.
type Problem struct {
	FieldPath string
	Message   string
	RuleID    string
}

// SemanticProblems runs the runtime's own config rules on a resolved config -
// the structural rules (config.Problems) then model-alias resolution against
// the registry the config itself resolves - and reports each at its spec field
// path (SpecPath; empty for an operator-only key). It is the single semantic
// layer behind both the public validator, which resolves a spec over the
// defaults, and a server resolving a spec over its operator config (change
// 0085), so the two can never apply different rules.
func SemanticProblems(cfg config.Config) []Problem {
	var out []Problem
	for _, p := range cfg.Problems() {
		out = append(out, Problem{FieldPath: SpecPath(p.Path), Message: p.Message, RuleID: RuleConfig})
	}
	for _, p := range configcheck.ModelRefProblems(cfg, configcheck.Registry(cfg)) {
		out = append(out, Problem{FieldPath: SpecPath(p.Path), Message: p.Message, RuleID: RuleModelRef})
	}
	return out
}

// Combine merges problem lists into one verdict. Every problem of primary is
// kept (the shape layer: the one every language agrees on). A later problem is
// dropped when an earlier one already reported its field, so a field is
// reported once; a problem with no field path is always kept. The result is
// sorted by field path, then rule id.
func Combine(primary []Problem, rest ...[]Problem) []Problem {
	out := append([]Problem(nil), primary...)
	flagged := map[string]bool{}
	for _, p := range primary {
		flagged[p.FieldPath] = true
	}
	for _, list := range rest {
		for _, p := range list {
			if p.FieldPath != "" {
				if flagged[p.FieldPath] {
					continue
				}
				flagged[p.FieldPath] = true
			}
			out = append(out, p)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].FieldPath != out[j].FieldPath {
			return out[i].FieldPath < out[j].FieldPath
		}
		return out[i].RuleID < out[j].RuleID
	})
	return out
}

// Overlay resolves spec over base, the server operator's trusted config
// (change 0085): the spec rendered as the loader's document and merged by the
// loader itself at its spec trust level (config.OverlaySpec), so every layering
// rule lives in internal/config. It returns the resolved config and the config
// key paths of the tighten-only loosenings the merge refused (see
// config.OverlaySpec); the caller decides how to report them. secret_ref fields
// are resolved through secrets (nil: PlaceholderSecrets).
func Overlay(base config.Config, spec *harnessv1.HarnessSpec, secrets SecretResolver) (config.Config, []string, error) {
	if secrets == nil {
		secrets = PlaceholderSecrets
	}
	d, err := toDocument(spec, secrets)
	if err != nil {
		return config.Config{}, nil, err
	}
	data, err := yaml.Marshal(d)
	if err != nil {
		return config.Config{}, nil, fmt.Errorf("harness spec: render config document: %w", err)
	}
	return config.OverlaySpec(base, data, "harness spec")
}
