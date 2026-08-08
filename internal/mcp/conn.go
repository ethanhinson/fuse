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

// notificationRouter receives inbound id-less JSON-RPC notification frames a
// read pump decodes from a server. server is the managed server's name (the
// client already knows it); method and params are the notification's fields.
// The Manager implements this (see notification_router.go); a client's pump
// calls it for every id-less method frame. A nil router means notifications are
// dropped (the pre-#0020 behavior, preserved for clients constructed without a
// manager, e.g. some unit tests).
type notificationRouter interface {
	dispatchNotification(server, method string, params json.RawMessage)
}
