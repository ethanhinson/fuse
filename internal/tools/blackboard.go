package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// BlackboardStore is the narrow seam the blackboard tools reach the shared store
// through. It is defined here (not imported from internal/agent) to break the
// import cycle — internal/agent imports internal/tools, so the store type cannot
// be referenced directly, exactly as SpawnFunc breaks the spawn cycle. The
// concrete implementation is a per-node handle from package agent that stamps
// provenance (the calling node's ID/label) on Write and yields that node during
// Wait, keeping provenance out of the model's hands and this package free of any
// agent import.
type BlackboardStore interface {
	// Write upserts key with value, stamping the calling agent's provenance.
	Write(key string, value any)
	// Read returns the stored value, the writer's display label, and whether the
	// key is set.
	Read(key string) (value any, writerLabel string, ok bool)
	// Delete removes key; idempotent.
	Delete(key string)
	// Keys returns the glob-matched, sorted key list (path.Match semantics; empty
	// or "*" returns all).
	Keys(pattern string) []string
	// Wait blocks until key is set (returning its value), timeout elapses, or ctx
	// is cancelled — yielding the calling agent's scheduler slot while blocked.
	Wait(ctx context.Context, key string, timeout time.Duration) (any, error)
}

// maxWaitSeconds caps blackboard_wait: there are no infinite waits (spec D1), and
// an over-large timeout is a model error rather than a silent multi-hour park.
const maxWaitSeconds = 300

// NewBlackboardTools returns the five discrete blackboard tools wired to store.
// Modeled on the spawn tool: each tool is a thin, panic-free wrapper that parses
// its args, calls the store, and returns a Result the model can read. Provenance
// is stamped by the store handle, never supplied here.
func NewBlackboardTools(store BlackboardStore) []Tool {
	return []Tool{
		&blackboardWriteTool{store: store},
		&blackboardReadTool{store: store},
		&blackboardWaitTool{store: store},
		&blackboardKeysTool{store: store},
		&blackboardDeleteTool{store: store},
	}
}

// --- blackboard_write -----------------------------------------------------------

type blackboardWriteTool struct{ store BlackboardStore }

func (t *blackboardWriteTool) Name() string { return "blackboard_write" }

func (t *blackboardWriteTool) Description() string {
	return "Write a structured value to a shared session blackboard key visible to " +
		"every agent. The value is a JSON string parsed into a structured value. " +
		"Last write wins."
}

func (t *blackboardWriteTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"key": map[string]any{
				"type":        "string",
				"description": "The blackboard key to write (required).",
			},
			"value": map[string]any{
				"type":        "string",
				"description": "The value as a JSON string (e.g. \"{\\\"a\\\":1}\", \"[1,2]\", \"\\\"text\\\"\") — parsed with JSON (required).",
			},
		},
		"required": []string{"key", "value"},
	}
}

type blackboardWriteInput struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

func (t *blackboardWriteTool) Execute(_ context.Context, args string) Result {
	var in blackboardWriteInput
	if err := json.Unmarshal([]byte(args), &in); err != nil {
		return Result{IsError: true, Output: fmt.Sprintf("blackboard_write: invalid args: %v", err)}
	}
	if in.Key == "" {
		return Result{IsError: true, Output: "blackboard_write: key is required"}
	}
	var value any
	if err := json.Unmarshal([]byte(in.Value), &value); err != nil {
		return Result{IsError: true, Output: fmt.Sprintf("blackboard_write: value is not valid JSON: %v", err)}
	}
	t.store.Write(in.Key, value)
	return Result{Output: fmt.Sprintf("wrote key %q", in.Key)}
}

// --- blackboard_read ------------------------------------------------------------

type blackboardReadTool struct{ store BlackboardStore }

func (t *blackboardReadTool) Name() string { return "blackboard_read" }

func (t *blackboardReadTool) Description() string {
	return "Read a value from the shared session blackboard by key. Returns the " +
		"JSON-encoded value, or a not-set notice if the key is unwritten."
}

func (t *blackboardReadTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"key": map[string]any{
				"type":        "string",
				"description": "The blackboard key to read (required).",
			},
		},
		"required": []string{"key"},
	}
}

type blackboardKeyInput struct {
	Key string `json:"key"`
}

func (t *blackboardReadTool) Execute(_ context.Context, args string) Result {
	var in blackboardKeyInput
	if err := json.Unmarshal([]byte(args), &in); err != nil {
		return Result{IsError: true, Output: fmt.Sprintf("blackboard_read: invalid args: %v", err)}
	}
	if in.Key == "" {
		return Result{IsError: true, Output: "blackboard_read: key is required"}
	}
	value, _, ok := t.store.Read(in.Key)
	if !ok {
		// A not-yet-written key is a normal condition, not a tool error.
		return Result{Output: fmt.Sprintf("key %q not set", in.Key)}
	}
	return Result{Output: encodeValue(value)}
}

// --- blackboard_wait ------------------------------------------------------------

type blackboardWaitTool struct{ store BlackboardStore }

func (t *blackboardWaitTool) Name() string { return "blackboard_wait" }

func (t *blackboardWaitTool) Description() string {
	return "Block until another agent writes a blackboard key, up to a required " +
		"timeout in seconds. Returns the JSON-encoded value, or a timeout notice. " +
		"Yields this agent's scheduler slot while blocked."
}

func (t *blackboardWaitTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"key": map[string]any{
				"type":        "string",
				"description": "The blackboard key to wait for (required).",
			},
			"timeout": map[string]any{
				"type":        "number",
				"description": fmt.Sprintf("Maximum seconds to wait — required, positive, at most %d.", maxWaitSeconds),
			},
		},
		"required": []string{"key", "timeout"},
	}
}

type blackboardWaitInput struct {
	Key     string  `json:"key"`
	Timeout float64 `json:"timeout"`
}

func (t *blackboardWaitTool) Execute(ctx context.Context, args string) Result {
	var in blackboardWaitInput
	if err := json.Unmarshal([]byte(args), &in); err != nil {
		return Result{IsError: true, Output: fmt.Sprintf("blackboard_wait: invalid args: %v", err)}
	}
	if in.Key == "" {
		return Result{IsError: true, Output: "blackboard_wait: key is required"}
	}
	if in.Timeout <= 0 {
		return Result{IsError: true, Output: "blackboard_wait: timeout (seconds) is required and must be positive"}
	}
	if in.Timeout > maxWaitSeconds {
		return Result{IsError: true, Output: fmt.Sprintf("blackboard_wait: timeout %g exceeds the %d-second ceiling", in.Timeout, maxWaitSeconds)}
	}
	timeout := time.Duration(in.Timeout * float64(time.Second))
	value, err := t.store.Wait(ctx, in.Key, timeout)
	if err != nil {
		// Timeout / cancel are results the model reads and acts on, not panics.
		return Result{Output: fmt.Sprintf("wait on key %q ended without a value: %v", in.Key, err)}
	}
	return Result{Output: encodeValue(value)}
}

// --- blackboard_keys ------------------------------------------------------------

type blackboardKeysTool struct{ store BlackboardStore }

func (t *blackboardKeysTool) Name() string { return "blackboard_keys" }

func (t *blackboardKeysTool) Description() string {
	return "List blackboard keys matching a glob pattern (path.Match semantics; " +
		"empty or \"*\" lists all). Returns the sorted keys."
}

func (t *blackboardKeysTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"pattern": map[string]any{
				"type":        "string",
				"description": "Glob pattern to match keys; empty or \"*\" lists all.",
			},
		},
	}
}

type blackboardPatternInput struct {
	Pattern string `json:"pattern"`
}

func (t *blackboardKeysTool) Execute(_ context.Context, args string) Result {
	var in blackboardPatternInput
	// Empty args is valid (list all); only malformed non-empty JSON is an error.
	if args != "" {
		if err := json.Unmarshal([]byte(args), &in); err != nil {
			return Result{IsError: true, Output: fmt.Sprintf("blackboard_keys: invalid args: %v", err)}
		}
	}
	keys := t.store.Keys(in.Pattern)
	out, err := json.Marshal(keys)
	if err != nil {
		return Result{IsError: true, Output: fmt.Sprintf("blackboard_keys: encode: %v", err)}
	}
	return Result{Output: string(out)}
}

// --- blackboard_delete ----------------------------------------------------------

type blackboardDeleteTool struct{ store BlackboardStore }

func (t *blackboardDeleteTool) Name() string { return "blackboard_delete" }

func (t *blackboardDeleteTool) Description() string {
	return "Remove a blackboard key. Idempotent — deleting a missing key succeeds."
}

func (t *blackboardDeleteTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"key": map[string]any{
				"type":        "string",
				"description": "The blackboard key to delete (required).",
			},
		},
		"required": []string{"key"},
	}
}

func (t *blackboardDeleteTool) Execute(_ context.Context, args string) Result {
	var in blackboardKeyInput
	if err := json.Unmarshal([]byte(args), &in); err != nil {
		return Result{IsError: true, Output: fmt.Sprintf("blackboard_delete: invalid args: %v", err)}
	}
	if in.Key == "" {
		return Result{IsError: true, Output: "blackboard_delete: key is required"}
	}
	t.store.Delete(in.Key)
	return Result{Output: fmt.Sprintf("deleted key %q", in.Key)}
}

// encodeValue JSON-encodes a stored value for return to the model, falling back
// to a Go-formatted string if the value is somehow not JSON-encodable (it always
// should be — it was parsed from JSON on write).
func encodeValue(value any) string {
	b, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprintf("%v", value)
	}
	return string(b)
}

var (
	_ Tool = (*blackboardWriteTool)(nil)
	_ Tool = (*blackboardReadTool)(nil)
	_ Tool = (*blackboardWaitTool)(nil)
	_ Tool = (*blackboardKeysTool)(nil)
	_ Tool = (*blackboardDeleteTool)(nil)
)
