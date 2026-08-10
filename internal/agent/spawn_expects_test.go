package agent

import (
	"context"
	"strings"
	"testing"
)

// fixedBuilder returns a ChildBuilder that always yields the given text/err and
// records the opts it was called with (for prompt-injection assertions).
func fixedBuilder(text string, err error, captured *SpawnOpts) ChildBuilder {
	return func(_ context.Context, opts SpawnOpts, _ *AgentNode, _ *AgentTree) (string, error) {
		if captured != nil {
			*captured = opts
		}
		return text, err
	}
}

// TestSpawnNilExpects_ByteIdentical pins the load-bearing invariant: with
// Expects nil, the child's raw text is unchanged (no note appended) and
// Structured is nil.
func TestSpawnNilExpects_ByteIdentical(t *testing.T) {
	tree := NewAgentTree("root", "m")
	const raw = "just some free text, no schema here {maybe}"
	var captured SpawnOpts
	s := NewSpawner(WithTree(tree), WithChildBuilder(fixedBuilder(raw, nil, &captured)))

	h, err := s.Spawn(context.Background(), SpawnOpts{Label: "c", SystemPrompt: "orig"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	d := h.Wait()
	if d.Err != nil {
		t.Fatalf("unexpected err: %v", d.Err)
	}
	if d.Result != raw {
		t.Fatalf("nil-expects must leave result unchanged; got %q want %q", d.Result, raw)
	}
	if d.Structured != nil {
		t.Fatalf("Structured must be nil when no schema; got %#v", d.Structured)
	}
	if captured.SystemPrompt != "orig" {
		t.Fatalf("nil-expects must leave system prompt untouched; got %q", captured.SystemPrompt)
	}
}

// TestSpawnExpects_ProducerPromptInjection verifies the change-0042 producer-side
// behavior: with Expects (a map schema) set, the child's effective system prompt
// carries the SHORT return_result hint (naming the tool + the serialized schema),
// preserves the base prompt, and does NOT carry the superseded "final message MUST
// be a single JSON object" directive (which competed with the tool channel — the
// bug this change fixes). Task 3 supersedes the pre-0042 directive assertion.
func TestSpawnExpects_ProducerPromptInjection(t *testing.T) {
	tree := NewAgentTree("root", "m")
	schema := map[string]any{
		"type":       "object",
		"properties": map[string]any{"n": map[string]any{"type": "integer"}},
	}
	var captured SpawnOpts
	s := NewSpawner(WithTree(tree), WithChildBuilder(fixedBuilder(`{"n":1}`, nil, &captured)))

	h, err := s.Spawn(context.Background(), SpawnOpts{Label: "c", SystemPrompt: "base prompt", Expects: schema})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	h.Wait()

	got := captured.SystemPrompt
	if !strings.Contains(got, "base prompt") {
		t.Errorf("injected prompt must preserve the base prompt; got %q", got)
	}
	if !strings.Contains(got, "return_result") {
		t.Errorf("injected prompt must name the return_result tool; got %q", got)
	}
	if !strings.Contains(got, "JSON Schema") || !strings.Contains(got, "\"type\":\"object\"") {
		t.Errorf("injected prompt must carry the serialized schema; got %q", got)
	}
	// The superseded message-channel directive must be gone (D2).
	if strings.Contains(got, "final message MUST be a single JSON object") {
		t.Errorf("injected prompt must NOT carry the superseded JSON-only directive; got %q", got)
	}
	// The spawner must have allocated a capture sink for the map-schema path.
	if captured.ExpectsSink() == nil {
		t.Errorf("map-schema Expects must allocate a return_result capture sink")
	}
}

// TestSpawnExpects_MatchPath verifies conforming JSON populates Structured,
// appends the match note, and is reachable via Result().
func TestSpawnExpects_MatchPath(t *testing.T) {
	tree := NewAgentTree("root", "m")
	schema := map[string]any{
		"type":       "object",
		"properties": map[string]any{"name": map[string]any{"type": "string"}},
		"required":   []any{"name"},
	}
	s := NewSpawner(WithTree(tree), WithChildBuilder(fixedBuilder(`{"name":"Ada"}`, nil, nil)))

	h, err := s.Spawn(context.Background(), SpawnOpts{Label: "c", Expects: schema})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	d := h.Wait()
	if d.Err != nil {
		t.Fatalf("match path must not error: %v", d.Err)
	}
	if !strings.HasSuffix(d.Result, "(matched expected schema)") {
		t.Errorf("match result must end with the match note; got %q", d.Result)
	}
	if d.Structured == nil {
		t.Fatal("Structured must be populated on match")
	}
	m, ok := d.Structured.(map[string]any)
	if !ok || m["name"] != "Ada" {
		t.Fatalf("Structured wrong: %#v", d.Structured)
	}
	// Result() returns the same structured value.
	sv, rerr := h.Result()
	if rerr != nil {
		t.Fatalf("Result() err: %v", rerr)
	}
	if sm, _ := sv.(map[string]any); sm["name"] != "Ada" {
		t.Fatalf("Result() structured wrong: %#v", sv)
	}
}

// TestSpawnExpects_LenientMatch verifies fenced/prose-wrapped conforming JSON
// still matches through the spawn path.
func TestSpawnExpects_LenientMatch(t *testing.T) {
	tree := NewAgentTree("root", "m")
	schema := map[string]any{
		"type":       "object",
		"properties": map[string]any{"ok": map[string]any{"type": "boolean"}},
		"required":   []any{"ok"},
	}
	raw := "Here is the result:\n```json\n{\"ok\": true}\n```\nHope that helps."
	s := NewSpawner(WithTree(tree), WithChildBuilder(fixedBuilder(raw, nil, nil)))

	h, _ := s.Spawn(context.Background(), SpawnOpts{Label: "c", Expects: schema})
	d := h.Wait()
	if d.Structured == nil {
		t.Fatalf("lenient wrapped JSON should match; result=%q", d.Result)
	}
	if !strings.HasSuffix(d.Result, "(matched expected schema)") {
		t.Errorf("expected match note; got %q", d.Result)
	}
}

// TestSpawnExpects_MismatchPath verifies non-conforming output does not error,
// keeps the raw text, appends the mismatch note, leaves Structured nil, and
// records a KindError/schema_mismatch event on the child node.
func TestSpawnExpects_MismatchPath(t *testing.T) {
	tree := NewAgentTree("root", "m")
	schema := map[string]any{
		"type":       "object",
		"properties": map[string]any{"n": map[string]any{"type": "integer"}},
		"required":   []any{"n"},
	}
	const raw = `{"n":"not-an-int"}`
	s := NewSpawner(WithTree(tree), WithChildBuilder(fixedBuilder(raw, nil, nil)))

	h, err := s.Spawn(context.Background(), SpawnOpts{Label: "c", Expects: schema})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	d := h.Wait()
	if d.Err != nil {
		t.Fatalf("mismatch must NOT fail the spawn; got err %v", d.Err)
	}
	if d.Structured != nil {
		t.Fatalf("Structured must be nil on mismatch; got %#v", d.Structured)
	}
	if !strings.HasPrefix(d.Result, raw) {
		t.Errorf("mismatch result must keep the raw text; got %q", d.Result)
	}
	if !strings.Contains(d.Result, "result did NOT match expected schema:") {
		t.Errorf("mismatch result must carry the note; got %q", d.Result)
	}
	// The child node carries a labeled schema_mismatch event.
	node := tree.Node(h.NodeID)
	if node == nil {
		t.Fatalf("child node %s not found", h.NodeID)
	}
	found := false
	for _, e := range node.CopyEvents() {
		if e.Kind == KindError && e.Name == "schema_mismatch" {
			found = true
			if e.Payload["error"] == nil {
				t.Errorf("schema_mismatch event must carry the first error in Payload")
			}
		}
	}
	if !found {
		t.Errorf("expected a KindError/schema_mismatch event on the child node")
	}
}

// TestSpawnExpects_UnparseableIsMismatch verifies output with no JSON at all is a
// (non-fatal) mismatch.
func TestSpawnExpects_UnparseableIsMismatch(t *testing.T) {
	tree := NewAgentTree("root", "m")
	schema := map[string]any{"type": "object"}
	s := NewSpawner(WithTree(tree), WithChildBuilder(fixedBuilder("I could not do it, sorry.", nil, nil)))

	h, _ := s.Spawn(context.Background(), SpawnOpts{Label: "c", Expects: schema})
	d := h.Wait()
	if d.Err != nil {
		t.Fatalf("unparseable must not error: %v", d.Err)
	}
	if d.Structured != nil {
		t.Fatalf("Structured must be nil: %#v", d.Structured)
	}
	if !strings.Contains(d.Result, "result did NOT match expected schema:") {
		t.Errorf("expected mismatch note; got %q", d.Result)
	}
}

// TestSpawnHandle_DrainSafe verifies Result() and Wait() return consistent values
// in either call order (memoized SpawnDone).
func TestSpawnHandle_DrainSafe(t *testing.T) {
	schema := map[string]any{
		"type":       "object",
		"properties": map[string]any{"v": map[string]any{"type": "integer"}},
		"required":   []any{"v"},
	}

	t.Run("result_then_wait", func(t *testing.T) {
		tree := NewAgentTree("root", "m")
		s := NewSpawner(WithTree(tree), WithChildBuilder(fixedBuilder(`{"v":7}`, nil, nil)))
		h, _ := s.Spawn(context.Background(), SpawnOpts{Label: "c", Expects: schema})
		sv, err := h.Result()
		if err != nil {
			t.Fatalf("Result err: %v", err)
		}
		d := h.Wait()
		if d.Result == "" {
			t.Fatal("Wait() after Result() must still return the cached result")
		}
		if sm, _ := sv.(map[string]any); sm["v"] == nil {
			t.Fatalf("Result() structured lost: %#v", sv)
		}
		if d.Structured == nil {
			t.Fatal("Wait() structured must match Result()")
		}
	})

	t.Run("wait_then_result", func(t *testing.T) {
		tree := NewAgentTree("root", "m")
		s := NewSpawner(WithTree(tree), WithChildBuilder(fixedBuilder(`{"v":9}`, nil, nil)))
		h, _ := s.Spawn(context.Background(), SpawnOpts{Label: "c", Expects: schema})
		d := h.Wait()
		sv, err := h.Result()
		if err != nil {
			t.Fatalf("Result err: %v", err)
		}
		if d.Structured == nil || sv == nil {
			t.Fatalf("both must see structured; wait=%#v result=%#v", d.Structured, sv)
		}
	})
}
