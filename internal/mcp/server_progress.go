package mcp

import "context"

// ProgressReporter emits an MCP $/progress update from inside a tool's Execute.
// progress is the current value; total is the optional upper bound (nil for
// indeterminate progress). A nil ProgressReporter means the caller did not
// request progress (no _meta.progressToken), so a tool's report is a no-op.
type ProgressReporter func(progress float64, total *float64)

// progressCtxKey is the unexported context key for the ProgressReporter.
type progressCtxKey struct{}

// withProgress returns a context carrying a ProgressReporter for the duration of
// a tools/call. A tool retrieves it via ProgressFromContext.
func withProgress(ctx context.Context, r ProgressReporter) context.Context {
	return context.WithValue(ctx, progressCtxKey{}, r)
}

// ProgressFromContext returns the ProgressReporter installed for the current
// tools/call, or nil when the client did not request progress. A tool that wants
// to stream progress calls this and, if non-nil, invokes it; a tool that never
// reports simply ignores it. This is how the fuse MCP server threads a progress
// callback into tool execution WITHOUT changing the tools.Tool interface.
func ProgressFromContext(ctx context.Context) ProgressReporter {
	r, _ := ctx.Value(progressCtxKey{}).(ProgressReporter)
	return r
}
