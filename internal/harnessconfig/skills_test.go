package harnessconfig

import (
	"fmt"
	"strings"
	"testing"

	harnessv1 "github.com/ethanhinson/fuse/harness/v1"
)

// carriedSpec returns a spec with n carried skills, each with body.
func carriedSpec(n int, body string) *harnessv1.HarnessSpec {
	spec := &harnessv1.HarnessSpec{}
	for i := 0; i < n; i++ {
		spec.Skills = append(spec.Skills, &harnessv1.SkillRef{
			Name:        fmt.Sprintf("s%d", i),
			Description: "a carried skill",
			Body:        body,
		})
	}
	return spec
}

func TestSkillProblemsCarriedBytes(t *testing.T) {
	body := strings.Repeat("x", 30000)

	got := SkillProblems(carriedSpec(9, body))
	if len(got) != 1 {
		t.Fatalf("9 x 30000 bytes: got %+v, want one problem", got)
	}
	p := got[0]
	if p.FieldPath != "skills" || p.RuleID != RuleSkillsCarriedBytes {
		t.Fatalf("problem = %+v, want skills %s", p, RuleSkillsCarriedBytes)
	}
	if !strings.Contains(p.Message, "270000") || !strings.Contains(p.Message, "262144") {
		t.Fatalf("message %q should name the total and the limit", p.Message)
	}

	if got := SkillProblems(carriedSpec(8, body)); len(got) != 0 {
		t.Fatalf("8 x 30000 bytes: got %+v, want none", got)
	}

	refs := &harnessv1.HarnessSpec{Skills: []*harnessv1.SkillRef{{Name: "research"}, {Name: "review", Description: "ignored"}}}
	if got := SkillProblems(refs); len(got) != 0 {
		t.Fatalf("reference entries: got %+v, want none", got)
	}
	if got := SkillProblems(nil); len(got) != 0 {
		t.Fatalf("nil spec: got %+v, want none", got)
	}
}

func TestSkillProblemsCountsBytesNotRunes(t *testing.T) {
	// "é" is two bytes in UTF-8: 9 bodies of 15000 runes are 270000 bytes
	// but only 135000 runes, so only a byte count trips the limit.
	body := strings.Repeat("é", 15000)
	if len(body) != 30000 {
		t.Fatalf("body is %d bytes, want 30000", len(body))
	}
	got := SkillProblems(carriedSpec(9, body))
	if len(got) != 1 || got[0].RuleID != RuleSkillsCarriedBytes || !strings.Contains(got[0].Message, "270000") {
		t.Fatalf("got %+v, want one carried_bytes problem counting 270000 bytes", got)
	}
}
