package observe

import (
	"context"
	"testing"
)

func TestObservationContracts(t *testing.T) {
	var _ OperationKind = OperationLoop
	var _ Outcome = OutcomeSuccess
	var _ = Descriptor{Kind: OperationModelAttempt, Name: "fuse.model.attempt", Fields: []Field{{Key: "model", Value: "opaque-model"}}}
	var _ Observer = NoopObserver{}
	var _ Handle = NoopHandle{}
}

func TestNoopObserverIsSafeAndPreservesContext(t *testing.T) {
	ctx := context.WithValue(context.Background(), struct{}{}, "sentinel")
	gotCtx, handle := NoopObserver{}.Start(ctx, Descriptor{Kind: OperationLoop, Name: "fuse.loop"})
	if gotCtx != ctx {
		t.Fatal("no-op observer replaced the context")
	}
	if handle == nil {
		t.Fatal("no-op observer returned a nil handle")
	}
	handle.End(OutcomeSuccess, Field{Key: "ignored", Value: "value"})
}
