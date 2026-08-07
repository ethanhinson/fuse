package mcp

import (
	"context"
	"encoding/json"
)

// mcpConn is the transport-agnostic interface used within this package.
// Both StdioClient and httpClient satisfy it.
type mcpConn interface {
	call(ctx context.Context, method string, params any) (json.RawMessage, error)
	// notify sends a JSON-RPC notification (no id, no response awaited). Used
	// for the id-less "notifications/initialized" handshake message.
	notify(ctx context.Context, method string, params any) error
	stop()
}
