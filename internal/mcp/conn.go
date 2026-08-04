package mcp

import (
	"context"
	"encoding/json"
)

// mcpConn is the transport-agnostic interface used within this package.
// Both StdioClient and httpClient satisfy it.
type mcpConn interface {
	call(ctx context.Context, method string, params any) (json.RawMessage, error)
	stop()
}
