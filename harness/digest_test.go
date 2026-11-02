package harness

import (
	"strings"
	"testing"

	harnessv1 "github.com/ethanhinson/fuse/harness/v1"
)

func TestDigestIsStableAndContentAddressed(t *testing.T) {
	a := &harnessv1.HarnessSpec{Workflows: map[string]*harnessv1.Workflow{"a": {Skill: "x"}, "b": {Skill: "y"}, "c": {Skill: "z"}}}
	for i := 0; i < 20; i++ {
		if Digest(a) != Digest(a) {
			t.Fatal("digest is not deterministic over map order")
		}
	}
	b := &harnessv1.HarnessSpec{Workflows: map[string]*harnessv1.Workflow{"a": {Skill: "x"}}}
	if Digest(a) == Digest(b) {
		t.Fatal("different specs share a digest")
	}
	if Digest(nil) != Digest(&harnessv1.HarnessSpec{}) {
		t.Fatal("nil and empty spec differ")
	}
}

func TestDigestAlgorithmIsExportedAndPrefixesDigest(t *testing.T) {
	if DigestAlgorithm != "sha256" {
		t.Fatalf("DigestAlgorithm = %q, want sha256", DigestAlgorithm)
	}
	if d := Digest(nil); !strings.HasPrefix(d, DigestAlgorithm+":") {
		t.Fatalf("Digest(nil) = %q, want prefix %q", d, DigestAlgorithm+":")
	}
}

// TestDigestCoversInstructions pins that the task brief is part of the digest,
// and that an empty brief is not encoded, so no existing digest moves.
func TestDigestCoversInstructions(t *testing.T) {
	if Digest(&harnessv1.HarnessSpec{Instructions: "a"}) == Digest(&harnessv1.HarnessSpec{Instructions: "b"}) {
		t.Fatal("specs differing only in instructions share a digest")
	}
	const empty = "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if got := Digest(&harnessv1.HarnessSpec{Instructions: ""}); got != empty {
		t.Errorf("Digest(empty instructions) = %s, want %s", got, empty)
	}
	if got := Digest(&harnessv1.HarnessSpec{}); got != empty {
		t.Errorf("Digest(empty spec) = %s, want %s", got, empty)
	}
}

// rolelessSpec is a fixed, realistic spec that uses no roles.
func rolelessSpec() *harnessv1.HarnessSpec {
	return &harnessv1.HarnessSpec{
		Models: &harnessv1.Models{
			DefaultAlias: "fast",
			Entries:      map[string]*harnessv1.Model{"fast": {Id: "cloud/fast-1"}},
		},
		Instructions: "Review the change.",
		MaxTokens:    4096,
		Workflows: map[string]*harnessv1.Workflow{
			"review": {
				Skill:   "review",
				Pool:    &harnessv1.Pool{Concurrent: 2, Total: 4},
				Workers: map[string]*harnessv1.Worker{"reader": {Tools: []string{"read_file"}}},
			},
		},
	}
}

// TestDigestOfRolelessSpecIsPinned pins today's digest of a spec without
// roles. Adding roles to the schema must not move it.
func TestDigestOfRolelessSpecIsPinned(t *testing.T) {
	const want = "sha256:350d50f5cdf104c50481eb73b47c9f8a4fda9990ca0364530fcafcf058065e59"
	if got := Digest(rolelessSpec()); got != want {
		t.Fatalf("Digest(rolelessSpec()) = %s, want %s", got, want)
	}
}
