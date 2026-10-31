package agent

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/ethanhinson/fuse/internal/model"
)

// toolsForever answers every call with a distinct tool call.
type toolsForever struct{ n int }

func (c *toolsForever) Complete(ctx context.Context, req model.CompletionReq) (model.CompletionResp, error) {
	c.n++
	return model.CompletionResp{
		ToolCalls: []model.ToolCall{{ID: fmt.Sprintf("c%d", c.n), Name: "t", Arguments: fmt.Sprintf(`{"n":%d}`, c.n)}},
	}, nil
}

// A bus-less interactive agent behaves exactly like a non-interactive one at the turn
// cap: it returns ErrMaxTurns instead of silently ending with nil.
func TestBuslessInteractiveReturnsErrMaxTurns(t *testing.T) {
	comp := &toolsForever{}
	a := New(comp, &fakeExec{}, nopRenderer{}, "m", "", 2, 0)
	a.SetInteractive(true)
	a.SetMaxTurnsExplicit(true)
	a.SetHumanInjector(NewHumanInjector("n", nil))
	_, err := a.Run(context.Background(), []model.Message{{Role: "user", Content: "go"}})
	if !errors.Is(err, ErrMaxTurns) {
		t.Fatalf("err = %v, want ErrMaxTurns", err)
	}
	if comp.n != 2 {
		t.Fatalf("model calls = %d, want 2", comp.n)
	}
}

// The call gate refuses the model call for a non-interactive agent.
func TestCallGateStopsNonInteractive(t *testing.T) {
	comp := &toolsForever{}
	a := New(comp, &fakeExec{}, nopRenderer{}, "m", "", 0, 0)
	a.SetCallGate(func() error {
		if comp.n >= 1 {
			return ErrTokenQuotaExhausted
		}
		return nil
	})
	_, err := a.Run(context.Background(), []model.Message{{Role: "user", Content: "go"}})
	if !errors.Is(err, ErrTokenQuotaExhausted) {
		t.Fatalf("err = %v, want ErrTokenQuotaExhausted", err)
	}
	if comp.n != 1 {
		t.Fatalf("model calls = %d, want 1", comp.n)
	}
}
