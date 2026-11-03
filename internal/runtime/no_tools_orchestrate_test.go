package runtime

import (
	"context"
	"errors"
	"testing"

	harnessv1 "github.com/ethanhinson/fuse/harness/v1"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/tools"
)

// toolLessFactory wraps fakeHarness so the factory it returns reports a
// tool-less resolved config, as a spec layered over a tool-less floor would.
type toolLessFactory struct{ *fakeHarness }

func (f toolLessFactory) ResolveHarness(ctx context.Context, tenant event.TenantID, spec *harnessv1.HarnessSpec) (LoopFactory, error) {
	lf, err := f.fakeHarness.ResolveHarness(ctx, tenant, spec)
	lf.NoTools = true
	return lf, err
}

// An orchestrator's only tools are the watch-and-steer tools, so a tool-less
// orchestrator is a contradiction: StartLoop refuses it before anything exists,
// whether no_tools comes from the spec, the resolved factory or the binding.
func TestOrchestrateRefusesNoTools(t *testing.T) {
	cases := []struct {
		name string
		deps func(fh *fakeHarness) Deps
		cfg  LoopConfig
		// resolved is whether the resolver is consulted before the refusal.
		resolved bool
	}{
		{
			name: "spec no_tools",
			deps: func(fh *fakeHarness) Deps { return Deps{Harness: fh} },
			cfg:  LoopConfig{Task: "watch", Orchestrate: "target", Harness: &harnessv1.HarnessSpec{NoTools: true}},
		},
		{
			name:     "resolved factory no_tools",
			deps:     func(fh *fakeHarness) Deps { return Deps{Harness: toolLessFactory{fh}} },
			cfg:      LoopConfig{Task: "watch", Orchestrate: "target", Harness: testSpec()},
			resolved: true,
		},
		{
			name: "binding no_tools",
			deps: func(fh *fakeHarness) Deps { return Deps{NoTools: true} },
			cfg:  LoopConfig{Task: "watch", Orchestrate: "target"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fh := &fakeHarness{completer: &scriptedCompleter{}}
			hits := 0
			d := tc.deps(fh)
			d.MaxConcurrent, d.InMemoryEvents = 1, true
			d.BuildAgent = processBuild(t, &scriptedCompleter{}, &hits)
			d.NewToolRegistry = func() *tools.Registry {
				t.Error("tool registry built for a refused loop")
				return tools.NewRegistry()
			}
			rt := New(d)
			h, err := rt.StartLoop(context.Background(), tc.cfg)
			if !errors.Is(err, ErrNoToolsOrchestrate) {
				t.Fatalf("StartLoop err = %v (handle %v), want ErrNoToolsOrchestrate", err, h)
			}
			calls, _, built, regs := fh.snapshot()
			if built != 0 || regs != 0 || hits != 0 {
				t.Fatalf("refused loop built something: built=%d regs=%d hits=%d", built, regs, hits)
			}
			if got := len(calls) > 0; got != tc.resolved {
				t.Fatalf("resolver consulted = %v, want %v", got, tc.resolved)
			}
		})
	}
}
