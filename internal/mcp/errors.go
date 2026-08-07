package mcp

// Standard JSON-RPC 2.0 error codes (the pre-defined server-error set). Named
// here so server call sites read intently instead of using magic numbers.
const (
	// ErrInvalidRequest — the JSON sent is not a valid Request object.
	ErrInvalidRequest = -32600
	// ErrMethodNotFound — the method does not exist / is not available.
	ErrMethodNotFound = -32601
	// ErrInvalidParams — invalid method parameter(s).
	ErrInvalidParams = -32602
	// ErrInternal — internal JSON-RPC error.
	ErrInternal = -32603
)

// MCP-specific JSON-RPC error codes.
//
// MCP v2025-03-26 (Error Codes section) reserves the JSON-RPC range -32900 to
// -32999 for MCP-specific conditions, distinct from the standard JSON-RPC codes
// (-32700 parse error, -32600..-32603 request/method/params errors). Clients
// that understand this range can render better diagnostics — e.g. distinguishing
// "tool not found" from "invalid params" — while clients that don't simply treat
// them as generic application errors.
const (
	// ErrToolNotFound is returned when a tools/call names a tool the server does
	// not expose.
	ErrToolNotFound = -32900
	// ErrResourceNotFound is returned when a resources/* request names an unknown
	// resource. Defined for completeness; fuse exposes no resource surface yet.
	ErrResourceNotFound = -32901
	// ErrPromptNotFound is returned when a prompts/* request names an unknown
	// prompt. Defined for completeness.
	ErrPromptNotFound = -32902
	// ErrListResultEmpty signals an empty paginated list result. Defined for
	// completeness.
	ErrListResultEmpty = -32903
	// ErrConnectionClosed signals the underlying transport closed mid-request.
	// Defined for completeness.
	ErrConnectionClosed = -32904
)

// isMCPErrorCode reports whether code falls in the MCP-specific reserved range
// (-32999..-32900 inclusive).
func isMCPErrorCode(code int) bool {
	return code >= -32999 && code <= -32900
}

// RPCError is a JSON-RPC error returned by a downstream MCP server. It preserves
// the numeric Code alongside the Message so a caller can recognize MCP-specific
// codes (see isMCPErrorCode) via errors.As, instead of only seeing a flattened
// message string. Client call paths wrap it with %w so the code survives.
type RPCError struct {
	Code    int
	Message string
}

func (e *RPCError) Error() string { return e.Message }

// IsMCPError reports whether the code carried by e is in the MCP-specific range.
func (e *RPCError) IsMCPError() bool { return isMCPErrorCode(e.Code) }
