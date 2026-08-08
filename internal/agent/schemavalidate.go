package agent

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

// enPrinter localizes jsonschema error kinds. The library's ErrorKind
// LocalizedString panics on a nil printer, so we pass a fixed English one.
var enPrinter = message.NewPrinter(language.English)

// extractJSON leniently pulls a single JSON value out of a child agent's final
// message. Child models frequently wrap JSON in ```json fences or surround it
// with prose ("Here is the result: {...}. Let me know if..."). Rather than fail
// on that wrapping (a false negative — good data, bad envelope), this strips
// fences and takes the outermost balanced JSON object or array. It returns the
// trimmed candidate string; the caller unmarshals + validates it.
func extractJSON(raw string) string {
	s := strings.TrimSpace(raw)

	// Strip a leading ```json / ``` fence and its closing fence, if present.
	if strings.HasPrefix(s, "```") {
		// Drop the opening fence line (```json or ```) entirely.
		if nl := strings.IndexByte(s, '\n'); nl >= 0 {
			s = s[nl+1:]
		} else {
			s = strings.TrimPrefix(s, "```")
		}
		// Drop a trailing closing fence.
		if idx := strings.LastIndex(s, "```"); idx >= 0 {
			s = s[:idx]
		}
		s = strings.TrimSpace(s)
	}

	// If it already starts with a JSON structural char, try to take the outermost
	// balanced value from the start.
	if len(s) > 0 && (s[0] == '{' || s[0] == '[') {
		if end := balancedEnd(s); end > 0 {
			return s[:end]
		}
		return s
	}

	// Otherwise scan for the first '{' or '[' and take the outermost balanced
	// value from there (surrounding prose case).
	start := -1
	for i := 0; i < len(s); i++ {
		if s[i] == '{' || s[i] == '[' {
			start = i
			break
		}
	}
	if start < 0 {
		return s
	}
	sub := s[start:]
	if end := balancedEnd(sub); end > 0 {
		return sub[:end]
	}
	return sub
}

// balancedEnd returns the index one past the end of the outermost balanced JSON
// value (object or array) starting at s[0], honoring string literals and escapes
// so braces inside strings do not throw off the depth count. It returns 0 if no
// balanced value is found.
func balancedEnd(s string) int {
	if len(s) == 0 {
		return 0
	}
	open := s[0]
	if open != '{' && open != '[' {
		return 0
	}
	close := byte('}')
	if open == '[' {
		close = ']'
	}
	depth := 0
	inStr := false
	esc := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			if esc {
				esc = false
				continue
			}
			switch c {
			case '\\':
				esc = true
			case '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case open:
			depth++
		case close:
			depth--
			if depth == 0 {
				return i + 1
			}
		}
	}
	return 0
}

// augmentPromptWithSchema appends the producer-side directive to a child's
// system prompt (change 0024): it tells the child its final message MUST be a
// single JSON object conforming to the given schema, with no fences or prose.
// The schema is serialized to compact JSON. A schema that fails to marshal (or a
// nil one) leaves the prompt untouched — the result-side validator is the
// backstop and a missing directive never fails a spawn.
func augmentPromptWithSchema(prompt string, schema any) string {
	if schema == nil {
		return prompt
	}
	b, err := json.Marshal(schema)
	if err != nil {
		return prompt
	}
	directive := "Your final message MUST be a single JSON object conforming to this JSON Schema: " +
		string(b) +
		". Output ONLY the JSON object — no code fences, no commentary, no surrounding prose."
	if strings.TrimSpace(prompt) == "" {
		return directive
	}
	return prompt + "\n\n" + directive
}

// validateAgainstSchema leniently extracts a JSON value from raw and validates it
// against the given JSON Schema (a decoded schema document, typically a
// map[string]any from the model's `expects` param). On success it returns the
// parsed value and a nil error. On any failure — unparseable JSON, a schema that
// fails to compile, or a validation mismatch — it returns a non-nil error whose
// message is a concise, model-readable description of the FIRST problem.
//
// It is deliberately pure: no tree, opts, or IO dependencies, so it is unit
// testable in isolation and reusable at the one result-assembly site.
func validateAgainstSchema(schema map[string]any, raw string) (parsed any, err error) {
	candidate := extractJSON(raw)
	if strings.TrimSpace(candidate) == "" {
		return nil, fmt.Errorf("no JSON object found in result")
	}

	var value any
	if uerr := json.Unmarshal([]byte(candidate), &value); uerr != nil {
		return nil, fmt.Errorf("result is not valid JSON: %v", uerr)
	}

	compiler := jsonschema.NewCompiler()
	// santhosh-tekuri/v6 compiles from an in-memory resource added by URL. The URL
	// is arbitrary but must be consistent between AddResource and Compile.
	const schemaURL = "mem://expects.json"
	if aerr := compiler.AddResource(schemaURL, schema); aerr != nil {
		return nil, fmt.Errorf("invalid expected schema: %v", aerr)
	}
	sch, cerr := compiler.Compile(schemaURL)
	if cerr != nil {
		return nil, fmt.Errorf("invalid expected schema: %v", cerr)
	}

	if verr := sch.Validate(value); verr != nil {
		return nil, fmt.Errorf("%s", firstValidationError(verr))
	}
	return value, nil
}

// firstValidationError renders a single, concise line describing the first
// (most specific) validation failure, suitable for appending to the model-facing
// result note. jsonschema's default Error() is multi-line and verbose; the leaf
// causes carry the actionable message, so we surface the deepest one.
func firstValidationError(err error) string {
	ve, ok := err.(*jsonschema.ValidationError)
	if !ok {
		return err.Error()
	}
	leaf := deepestCause(ve)
	loc := "/"
	if len(leaf.InstanceLocation) > 0 {
		loc = "/" + strings.Join(leaf.InstanceLocation, "/")
	}
	msg := leaf.ErrorKind.LocalizedString(enPrinter)
	return fmt.Sprintf("at %s: %s", loc, msg)
}

// deepestCause walks to the most specific validation error (the leaf cause),
// which carries the concrete keyword failure (type/enum/etc.) rather than the
// aggregate "doesn't validate with ..." wrapper.
func deepestCause(ve *jsonschema.ValidationError) *jsonschema.ValidationError {
	for len(ve.Causes) > 0 {
		ve = ve.Causes[0]
	}
	return ve
}
