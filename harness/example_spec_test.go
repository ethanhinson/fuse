package harness_test

import (
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"

	"github.com/ethanhinson/fuse/harness"
	harnessv1 "github.com/ethanhinson/fuse/harness/v1"
)

// TestExampleSpecsAreValid keeps the shipped example harness specs parseable and valid.
func TestExampleSpecsAreValid(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("..", "examples", "harness", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatal("no example specs found under examples/harness")
	}
	foundReviewTeam, foundCarriedSkill := false, false
	for _, p := range paths {
		switch filepath.Base(p) {
		case "review-team.json":
			foundReviewTeam = true
		case "carried-skill.json":
			foundCarriedSkill = true
		}
	}
	if !foundReviewTeam {
		t.Fatal("review-team.json missing from examples/harness")
	}
	if !foundCarriedSkill {
		t.Fatal("carried-skill.json missing from examples/harness")
	}
	for _, p := range paths {
		t.Run(filepath.Base(p), func(t *testing.T) {
			raw, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			spec := &harnessv1.HarnessSpec{}
			if err := protojson.Unmarshal(raw, spec); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if v := harness.Validate(spec); len(v) != 0 {
				t.Fatalf("violations: %+v", v)
			}
			if filepath.Base(p) == "release-notes.json" && spec.GetInstructions() == "" {
				t.Fatal("release-notes.json must set instructions")
			}
			if filepath.Base(p) == "review-team.json" {
				if len(spec.GetRoles()) != 2 {
					t.Fatalf("review-team.json must define two roles, got %d", len(spec.GetRoles()))
				}
				wf := spec.GetWorkflows()["review"]
				if len(wf.GetRoles()) != 1 || spec.GetRoles()[wf.GetRoles()[0]] == nil {
					t.Fatal("review-team.json must have a review workflow listing one defined role")
				}
			}
			if filepath.Base(p) == "carried-skill.json" {
				carried, reference := false, false
				for _, sk := range spec.GetSkills() {
					if sk.GetBody() != "" && sk.GetDescription() != "" {
						carried = true
					}
					if sk.GetBody() == "" {
						reference = true
					}
				}
				if !carried || !reference {
					t.Fatalf("carried-skill.json must define one carried skill and one reference (carried=%v reference=%v)", carried, reference)
				}
			}
		})
	}
}
