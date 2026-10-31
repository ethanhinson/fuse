package loopconnect

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	loopv1 "github.com/ethanhinson/fuse/internal/loopwire/v1"
)

// GetCapabilities is the read-only discovery RPC. Until a capability source is
// wired it answers Unimplemented, so a client never mistakes an empty answer for
// a server that accepts nothing.
func (h *Handler) GetCapabilities(_ context.Context, _ *connect.Request[loopv1.GetCapabilitiesRequest]) (*connect.Response[loopv1.GetCapabilitiesResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, errors.New("loopconnect: capabilities are not advertised by this server"))
}
