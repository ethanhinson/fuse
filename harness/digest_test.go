package harness

import (
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
