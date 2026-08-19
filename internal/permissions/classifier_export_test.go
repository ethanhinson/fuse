package permissions

import (
	"context"
	"testing"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/model"
)

// TestNewClassifierExportedSeam asserts the exported constructor seam CLI
// construction sites depend on: NewClassifier builds a non-nil *Classifier over
// a real *model.Adapter and *model.Registry (the concrete production types),
// and that classifier fails closed to VerdictAsk when its verdict call cannot
// reach a gateway. This exercises the seam without a live gateway: the adapter
// points at an unreachable local URL, so Complete errors and the classifier's
// fail-closed contract resolves to VerdictAsk.
func TestNewClassifierExportedSeam(t *testing.T) {
	// A real adapter aimed at an unroutable address: no network call succeeds,
	// so Complete returns an error and Classify must fail closed to ask.
	adapter := model.NewAdapter("http://127.0.0.1:0", "test-key", nil)
	reg := model.NewRegistry("m", map[string]model.ModelConfig{
		"m": {ID: "cloud/m", MaxTokens: 256},
	})
	cfg := config.AutoConfig{ClassifierModel: "m"}

	cls := NewClassifier(adapter, reg, cfg, nil)
	if cls == nil {
		t.Fatal("NewClassifier returned nil; CLI sites cannot wire a classifier")
	}

	got := cls.Classify(context.Background(), nil, "bash", "rm -rf /")
	if got.Verdict != VerdictAsk {
		t.Fatalf("unreachable gateway must fail closed to VerdictAsk, got %v", got)
	}
}
