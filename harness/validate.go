// Package harness is the public Go library for fuse's portable harness
// specification, fuse.harness.v1 HarnessSpec (change 0084, ADR-0060). Its
// generated types live in github.com/ethanhinson/fuse/harness/v1; this package
// validates them.
//
// The package is public (not under internal/) on purpose: any Go client of the
// fuse runtime - including one in a different Go module - must be able to
// validate a spec with exactly the rules the runtime applies.
//
// Validation has two layers, reported together:
//
//   - Shape: the protovalidate rules declared in harness.proto (ranges,
//     required fields, URL and name shapes, enum membership). Every language
//     evaluates these from the same descriptors, so a TS client rejects the
//     same input this package does.
//   - Semantics: the runtime's own config rules (internal/config, plus model
//     alias resolution in internal/configcheck), reached by mapping the spec
//     into the runtime config and running them there - never re-implemented
//     here, so the spec validator and the CLI startup gate cannot drift.
package harness

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"buf.build/go/protovalidate"
	"connectrpc.com/connect"

	harnessv1 "github.com/ethanhinson/fuse/harness/v1"
	"github.com/ethanhinson/fuse/harness/v1/harnessv1connect"
	"github.com/ethanhinson/fuse/internal/configcheck"
	"github.com/ethanhinson/fuse/internal/harnessconfig"
)

// Semantic rule ids. Shape violations carry protovalidate's own rule ids
// (e.g. "double.lte", "string.uri") or the CEL ids declared in harness.proto.
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
)

// Violation is one problem with a spec.
type Violation struct {
	// FieldPath locates the offending field in protovalidate's path syntax
	// (`context.summarization.threshold`, `mcp_servers[0].url`,
	// `models.entries["fast"].id`), so a form can show the error next to the
	// field. Empty when the problem is not attributable to one field.
	FieldPath string
	// Message describes the problem relative to the field.
	Message string
	// RuleID identifies the rule that fired.
	RuleID string
}

// Proto converts the violation to its wire form.
func (v Violation) Proto() *harnessv1.Violation {
	return &harnessv1.Violation{FieldPath: v.FieldPath, Message: v.Message, RuleId: v.RuleID}
}

// Validate returns every violation in spec, sorted by field path then rule id;
// an empty result means the spec is valid. A nil spec is the empty spec, which
// is valid (it means "runtime defaults throughout"). When the shape layer and
// the semantic layer flag the same field, the field is reported once, with the
// shape violation (the one every language agrees on).
func Validate(spec *harnessv1.HarnessSpec) []Violation {
	if spec == nil {
		spec = &harnessv1.HarnessSpec{}
	}
	out := shape(spec)
	flagged := map[string]bool{}
	for _, v := range out {
		if v.RuleID == RuleInternal {
			return out
		}
		flagged[v.FieldPath] = true
	}

	cfg, err := harnessconfig.ToConfig(spec, harnessconfig.PlaceholderSecrets)
	if err != nil {
		out = append(out, Violation{Message: err.Error(), RuleID: RuleMapping})
	} else {
		semantic := func(rule string, path, msg string) {
			path = harnessconfig.SpecPath(path)
			if path != "" {
				if flagged[path] {
					return
				}
				flagged[path] = true
			}
			out = append(out, Violation{FieldPath: path, Message: msg, RuleID: rule})
		}
		for _, p := range cfg.Problems() {
			semantic(RuleConfig, p.Path, p.Message)
		}
		for _, p := range configcheck.ModelRefProblems(cfg, configcheck.Registry(cfg)) {
			semantic(RuleModelRef, p.Path, p.Message)
		}
	}

	sortViolations(out)
	return out
}

// ValidateShape runs only the protovalidate shape rules: exactly what the TS
// SDK's validateHarnessShape runs, with the same field paths and rule ids. Use
// Validate for the full verdict.
func ValidateShape(spec *harnessv1.HarnessSpec) []Violation {
	if spec == nil {
		spec = &harnessv1.HarnessSpec{}
	}
	out := shape(spec)
	sortViolations(out)
	return out
}

func shape(spec *harnessv1.HarnessSpec) []Violation {
	err := protovalidate.Validate(spec)
	if err == nil {
		return nil
	}
	var ve *protovalidate.ValidationError
	if !errors.As(err, &ve) {
		// A compilation or runtime error in the rules themselves: report it
		// rather than claiming the spec is valid.
		return []Violation{{Message: fmt.Sprintf("validator error: %v", err), RuleID: RuleInternal}}
	}
	out := make([]Violation, 0, len(ve.Violations))
	for _, v := range ve.Violations {
		out = append(out, Violation{
			FieldPath: protovalidate.FieldPathString(v.Proto.GetField()),
			Message:   v.Proto.GetMessage(),
			RuleID:    v.Proto.GetRuleId(),
		})
	}
	return out
}

func sortViolations(vs []Violation) {
	sort.SliceStable(vs, func(i, j int) bool {
		if vs[i].FieldPath != vs[j].FieldPath {
			return vs[i].FieldPath < vs[j].FieldPath
		}
		return vs[i].RuleID < vs[j].RuleID
	})
}

// Service implements fuse.harness.v1.HarnessService with Validate. fuse does not
// mount it on any of its own bindings; it is here so a server that wants to
// expose spec validation serves exactly this package's rules:
//
//	mux.Handle(harnessv1connect.NewHarnessServiceHandler(harness.Service{}))
type Service struct{}

var _ harnessv1connect.HarnessServiceHandler = Service{}

// ValidateHarness validates the request's spec.
func (Service) ValidateHarness(_ context.Context, req *connect.Request[harnessv1.ValidateHarnessRequest]) (*connect.Response[harnessv1.ValidateHarnessResponse], error) {
	vs := Validate(req.Msg.GetSpec())
	resp := &harnessv1.ValidateHarnessResponse{Violations: make([]*harnessv1.Violation, 0, len(vs))}
	for _, v := range vs {
		resp.Violations = append(resp.Violations, v.Proto())
	}
	return connect.NewResponse(resp), nil
}
