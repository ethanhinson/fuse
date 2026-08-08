package agent

import (
	"encoding/json"
	"strings"
	"testing"
)

// mustSchema decodes a JSON schema literal into the map[string]any shape the
// validator receives (mirroring how `expects` arrives from the tool seam).
func mustSchema(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("bad test schema: %v", err)
	}
	return m
}

// TestValidateAgainstSchema_Match verifies conforming JSON returns the parsed
// value and no error.
func TestValidateAgainstSchema_Match(t *testing.T) {
	schema := mustSchema(t, `{
		"type": "object",
		"properties": {
			"name": {"type": "string"},
			"age":  {"type": "integer"},
			"tags": {"type": "array", "items": {"type": "string"}},
			"addr": {"type": "object", "properties": {"city": {"type": "string"}}, "required": ["city"]}
		},
		"required": ["name", "age"]
	}`)
	raw := `{"name":"Ada","age":36,"tags":["a","b"],"addr":{"city":"London"}}`
	parsed, err := validateAgainstSchema(schema, raw)
	if err != nil {
		t.Fatalf("expected match, got error: %v", err)
	}
	if parsed == nil {
		t.Fatal("expected non-nil parsed value on match")
	}
	m, ok := parsed.(map[string]any)
	if !ok || m["name"] != "Ada" {
		t.Fatalf("parsed value not the expected object: %#v", parsed)
	}
}

// TestValidateAgainstSchema_FidelityCases covers the D2 fidelity cases: each
// class of mismatch a shallow hand-rolled check would miss must be caught.
func TestValidateAgainstSchema_FidelityCases(t *testing.T) {
	cases := []struct {
		name   string
		schema string
		raw    string
	}{
		{
			name: "top_level_type_mismatch",
			schema: `{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"]}`,
			raw:  `["not","an","object"]`,
		},
		{
			name: "nested_object_mismatch",
			schema: `{"type":"object","properties":{"addr":{"type":"object","properties":{"zip":{"type":"integer"}},"required":["zip"]}},"required":["addr"]}`,
			raw:  `{"addr":{"zip":"90210"}}`,
		},
		{
			name: "array_of_wrong_type",
			schema: `{"type":"object","properties":{"tags":{"type":"array","items":{"type":"string"}}},"required":["tags"]}`,
			raw:  `{"tags":[1,2,3]}`,
		},
		{
			name: "enum_violation",
			schema: `{"type":"object","properties":{"color":{"enum":["red","green","blue"]}},"required":["color"]}`,
			raw:  `{"color":"purple"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := validateAgainstSchema(mustSchema(t, tc.schema), tc.raw)
			if err == nil {
				t.Fatalf("expected %s to be caught, got nil error", tc.name)
			}
		})
	}
}

// TestValidateAgainstSchema_LenientExtraction verifies fenced / prose-wrapped
// conforming JSON still matches (false-negative mitigation).
func TestValidateAgainstSchema_LenientExtraction(t *testing.T) {
	schema := mustSchema(t, `{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"]}`)
	cases := []struct {
		name string
		raw  string
	}{
		{"fenced_json", "```json\n{\"ok\": true}\n```"},
		{"fenced_bare", "```\n{\"ok\": true}\n```"},
		{"leading_prose", `Here is the result: {"ok": true}`},
		{"trailing_prose", `{"ok": true}. Let me know if you need anything else.`},
		{"prose_both_sides", "Sure!\n```json\n{\"ok\": true}\n```\nDone."},
		{"whitespace", "\n\n   {\"ok\": true}\n\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parsed, err := validateAgainstSchema(schema, tc.raw)
			if err != nil {
				t.Fatalf("lenient extraction failed for %s: %v", tc.name, err)
			}
			if parsed == nil {
				t.Fatalf("expected parsed value for %s", tc.name)
			}
		})
	}
}

// TestValidateAgainstSchema_UnparseableAndBraceStrings guards the extraction of
// balanced objects even when string values contain braces, and the unparseable
// case surfaces an error rather than panicking.
func TestValidateAgainstSchema_BraceInStrings(t *testing.T) {
	schema := mustSchema(t, `{"type":"object","properties":{"expr":{"type":"string"}},"required":["expr"]}`)
	raw := `{"expr": "a { nested } brace"} trailing`
	parsed, err := validateAgainstSchema(schema, raw)
	if err != nil {
		t.Fatalf("brace-in-string extraction failed: %v", err)
	}
	if m, _ := parsed.(map[string]any); m["expr"] != "a { nested } brace" {
		t.Fatalf("wrong extraction: %#v", parsed)
	}
}

func TestValidateAgainstSchema_NoJSON(t *testing.T) {
	schema := mustSchema(t, `{"type":"object"}`)
	_, err := validateAgainstSchema(schema, "I could not complete the task.")
	if err == nil {
		t.Fatal("expected error when no JSON is present")
	}
}

// TestFirstValidationError_Concise checks the model-facing note is a single line.
func TestFirstValidationError_Concise(t *testing.T) {
	schema := mustSchema(t, `{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"]}`)
	_, err := validateAgainstSchema(schema, `{"n":"nope"}`)
	if err == nil {
		t.Fatal("expected mismatch error")
	}
	if strings.Contains(err.Error(), "\n") {
		t.Fatalf("first error should be a single line, got: %q", err.Error())
	}
}
