package runtime

import (
	"context"
	"testing"

	"github.com/ethanhinson/fuse/internal/event"
)

func TestRuntimeInterfaceShape(t *testing.T) {
	// A struct literal proves every LoopConfig/SpawnOpts field name+type is stable.
	_ = LoopConfig{Task: "t", ModelID: "m"}
	_ = SpawnOpts{Label: "l", Task: "t", SystemPrompt: "s", ModelID: "m", Tools: []string{"bash"}, Worker: "w", Expects: map[string]any{}}
	var _ Runtime = (Runtime)(nil)
	var _ Runtime = (*inProcRuntime)(nil)
	var _ event.Seq = event.Seq(0)
	_ = context.Background()
}
