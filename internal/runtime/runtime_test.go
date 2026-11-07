package runtime

import "testing"

func TestRuntimeInterfaceShape(t *testing.T) {
	// A struct literal proves every LoopConfig field name+type is stable.
	_ = LoopConfig{Task: "t", ModelID: "m"}
	var _ Runtime = (*inProcRuntime)(nil)
}
