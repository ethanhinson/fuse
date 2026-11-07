package tui

import (
	"strings"
	"testing"

	"github.com/ethanhinson/fuse/internal/agent"
)

func TestInterruptCommandSteersRunningAgent(t *testing.T) {
	m, bus, tree := shellWithHumanMessaging(t)
	fired := 0
	bus.SetInterrupt(tree.RootID(), func() { fired++ })
	m = typeAndEnter(m, "/interrupt stop and use plan B")
	p := bus.Pending(tree.RootID())
	if len(p) != 1 || !p[0].Interrupt || p[0].Text != "stop and use plan B" || p[0].Source != agent.SourceHuman {
		t.Fatalf("pending = %+v", p)
	}
	if fired != 1 {
		t.Fatalf("interrupt hook fired %d times, want 1", fired)
	}
	if !strings.Contains(plainLines(m), "stop and use plan B") {
		t.Fatalf("not echoed:\n%s", plainLines(m))
	}
}
