package loopconnect

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	loopv1 "github.com/ethanhinson/fuse/internal/loopwire/v1"
)

// With no capability source wired, the RPC exists and answers Unimplemented,
// never an empty but plausible answer.
func TestGetCapabilitiesWithoutASourceIsUnimplemented(t *testing.T) {
	_, err := newTestServer(t, &fakeRuntime{}).GetCapabilities(context.Background(), connect.NewRequest(&loopv1.GetCapabilitiesRequest{}))
	if got := connect.CodeOf(err); got != connect.CodeUnimplemented {
		t.Fatalf("code = %v, want Unimplemented", got)
	}
}
