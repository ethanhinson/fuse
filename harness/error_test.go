package harness

import (
	"errors"
	"fmt"
	"testing"

	"google.golang.org/protobuf/proto"
)

func TestValidationError(t *testing.T) {
	e := &ValidationError{Violations: []Violation{
		{FieldPath: "max_tokens", Message: "must be >= 0", RuleID: RuleConfig},
		{FieldPath: "skills[0].name", Message: `skill "x" is not installed on this server`, RuleID: RuleSkillRef},
	}}
	if got, want := e.Error(), "harness spec: 2 violations: max_tokens: must be >= 0 (fuse.config); ..."; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	one := &ValidationError{Violations: e.Violations[1:]}
	if got, want := one.Error(), `harness spec: 1 violation: skills[0].name: skill "x" is not installed on this server (fuse.skill_ref)`; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	var target *ValidationError
	if !errors.As(fmt.Errorf("start: %w", e), &target) || len(target.Violations) != 2 {
		t.Fatal("errors.As does not reach a wrapped ValidationError")
	}
	back := ValidationErrorFromProto(e.Proto())
	if len(back.Violations) != 2 || back.Violations[1] != e.Violations[1] {
		t.Fatalf("proto round trip = %+v", back.Violations)
	}
	if !proto.Equal(e.Proto(), back.Proto()) {
		t.Fatal("proto round trip is not stable")
	}
}
