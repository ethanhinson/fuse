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
		})
	}
}
