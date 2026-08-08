package tools

import (
	"context"
	"errors"
	"path"
	"sort"
	"strings"
	"testing"
	"time"
)

// fakeStore is a minimal in-memory BlackboardStore for exercising the tools in
// isolation. Wait behavior is scripted so tests are deterministic.
type fakeStore struct {
	values map[string]any
	labels map[string]string

	// waitValue/waitErr script the next Wait call.
	waitValue any
	waitErr   error
	lastWait  string

	writes  []string
	deletes []string
}

func newFakeStore() *fakeStore {
	return &fakeStore{values: map[string]any{}, labels: map[string]string{}}
}

func (f *fakeStore) Write(key string, value any) {
	f.values[key] = value
	f.labels[key] = "fake-writer"
	f.writes = append(f.writes, key)
}

func (f *fakeStore) Read(key string) (any, string, bool) {
	v, ok := f.values[key]
	return v, f.labels[key], ok
}

func (f *fakeStore) Delete(key string) {
	delete(f.values, key)
	f.deletes = append(f.deletes, key)
}

func (f *fakeStore) Keys(pattern string) []string {
	all := pattern == "" || pattern == "*"
	var out []string
	for k := range f.values {
		if all {
			out = append(out, k)
			continue
		}
		if ok, err := path.Match(pattern, k); err == nil && ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func (f *fakeStore) Wait(_ context.Context, key string, _ time.Duration) (any, error) {
	f.lastWait = key
	return f.waitValue, f.waitErr
}

func toolByName(ts []Tool, name string) Tool {
	for _, t := range ts {
		if t.Name() == name {
			return t
		}
	}
	return nil
}

func TestNewBlackboardToolsNames(t *testing.T) {
	ts := NewBlackboardTools(newFakeStore())
	want := []string{"blackboard_write", "blackboard_read", "blackboard_wait", "blackboard_keys", "blackboard_delete"}
	for _, n := range want {
		if toolByName(ts, n) == nil {
			t.Errorf("missing tool %q", n)
		}
		if tl := toolByName(ts, n); tl != nil {
			if tl.Description() == "" {
				t.Errorf("%q: empty description", n)
			}
			if tl.Parameters() == nil {
				t.Errorf("%q: nil parameters", n)
			}
		}
	}
}

func TestBlackboardWriteTool(t *testing.T) {
	f := newFakeStore()
	w := toolByName(NewBlackboardTools(f), "blackboard_write")

	// Valid JSON object value.
	r := w.Execute(context.Background(), `{"key":"k","value":"{\"a\":1}"}`)
	if r.IsError {
		t.Fatalf("valid write errored: %q", r.Output)
	}
	if len(f.writes) != 1 || f.writes[0] != "k" {
		t.Fatalf("write not recorded: %v", f.writes)
	}
	m, ok := f.values["k"].(map[string]any)
	if !ok || m["a"] != float64(1) {
		t.Fatalf("value not parsed as JSON: %#v", f.values["k"])
	}

	// Malformed JSON value => tool error, not panic.
	r = w.Execute(context.Background(), `{"key":"k","value":"{not json}"}`)
	if !r.IsError {
		t.Fatal("malformed value JSON should be a tool error")
	}

	// Empty key => error.
	r = w.Execute(context.Background(), `{"key":"","value":"1"}`)
	if !r.IsError {
		t.Fatal("empty key should be a tool error")
	}

	// Malformed args => error.
	r = w.Execute(context.Background(), `not json`)
	if !r.IsError {
		t.Fatal("malformed args should be a tool error")
	}
}

func TestBlackboardReadTool(t *testing.T) {
	f := newFakeStore()
	f.values["k"] = map[string]any{"a": float64(1)}
	rd := toolByName(NewBlackboardTools(f), "blackboard_read")

	r := rd.Execute(context.Background(), `{"key":"k"}`)
	if r.IsError {
		t.Fatalf("read errored: %q", r.Output)
	}
	if !strings.Contains(r.Output, `"a":1`) {
		t.Fatalf("read did not JSON-encode value: %q", r.Output)
	}

	// Missing key => non-error notice.
	r = rd.Execute(context.Background(), `{"key":"nope"}`)
	if r.IsError {
		t.Fatalf("missing key should be non-error: %q", r.Output)
	}
	if !strings.Contains(r.Output, "not set") {
		t.Fatalf("missing key notice unexpected: %q", r.Output)
	}

	// Empty key => error.
	if r := rd.Execute(context.Background(), `{"key":""}`); !r.IsError {
		t.Fatal("empty key should be a tool error")
	}
}

func TestBlackboardWaitTool(t *testing.T) {
	f := newFakeStore()
	wt := toolByName(NewBlackboardTools(f), "blackboard_wait")

	// Value returned.
	f.waitValue = "produced"
	f.waitErr = nil
	r := wt.Execute(context.Background(), `{"key":"k","timeout":5}`)
	if r.IsError {
		t.Fatalf("wait with value errored: %q", r.Output)
	}
	if f.lastWait != "k" {
		t.Fatalf("Wait called with %q, want k", f.lastWait)
	}
	if !strings.Contains(r.Output, "produced") {
		t.Fatalf("wait output missing value: %q", r.Output)
	}

	// Timeout error from store => non-error result the model reads.
	f.waitValue = nil
	f.waitErr = errors.New("timed out")
	r = wt.Execute(context.Background(), `{"key":"k","timeout":1}`)
	if r.IsError {
		t.Fatalf("store timeout should be a readable result, not a tool error: %q", r.Output)
	}
	if !strings.Contains(r.Output, "without a value") {
		t.Fatalf("timeout notice unexpected: %q", r.Output)
	}

	// Missing timeout => error.
	if r := wt.Execute(context.Background(), `{"key":"k"}`); !r.IsError {
		t.Fatal("missing timeout should be a tool error")
	}
	// Non-positive timeout => error.
	if r := wt.Execute(context.Background(), `{"key":"k","timeout":0}`); !r.IsError {
		t.Fatal("zero timeout should be a tool error")
	}
	if r := wt.Execute(context.Background(), `{"key":"k","timeout":-3}`); !r.IsError {
		t.Fatal("negative timeout should be a tool error")
	}
	// Over-ceiling timeout => error.
	if r := wt.Execute(context.Background(), `{"key":"k","timeout":100000}`); !r.IsError {
		t.Fatal("over-ceiling timeout should be a tool error")
	}
	// Empty key => error.
	if r := wt.Execute(context.Background(), `{"key":"","timeout":5}`); !r.IsError {
		t.Fatal("empty key should be a tool error")
	}
	// Malformed args => error.
	if r := wt.Execute(context.Background(), `nope`); !r.IsError {
		t.Fatal("malformed args should be a tool error")
	}
}

func TestBlackboardKeysTool(t *testing.T) {
	f := newFakeStore()
	f.values["foo/a"] = 1
	f.values["foo/b"] = 2
	f.values["bar"] = 3
	kt := toolByName(NewBlackboardTools(f), "blackboard_keys")

	r := kt.Execute(context.Background(), `{"pattern":"foo/*"}`)
	if r.IsError {
		t.Fatalf("keys errored: %q", r.Output)
	}
	if !strings.Contains(r.Output, "foo/a") || !strings.Contains(r.Output, "foo/b") || strings.Contains(r.Output, "bar") {
		t.Fatalf("glob match unexpected: %q", r.Output)
	}

	// Empty args lists all (valid).
	r = kt.Execute(context.Background(), ``)
	if r.IsError {
		t.Fatalf("empty args should list all: %q", r.Output)
	}
	if !strings.Contains(r.Output, "bar") {
		t.Fatalf("list-all missing keys: %q", r.Output)
	}

	// Malformed non-empty args => error.
	if r := kt.Execute(context.Background(), `nope`); !r.IsError {
		t.Fatal("malformed args should be a tool error")
	}
}

func TestBlackboardDeleteTool(t *testing.T) {
	f := newFakeStore()
	f.values["k"] = 1
	dt := toolByName(NewBlackboardTools(f), "blackboard_delete")

	r := dt.Execute(context.Background(), `{"key":"k"}`)
	if r.IsError {
		t.Fatalf("delete errored: %q", r.Output)
	}
	if len(f.deletes) != 1 || f.deletes[0] != "k" {
		t.Fatalf("delete not recorded: %v", f.deletes)
	}

	// Idempotent: deleting again succeeds.
	if r := dt.Execute(context.Background(), `{"key":"k"}`); r.IsError {
		t.Fatalf("idempotent delete errored: %q", r.Output)
	}

	// Empty key => error.
	if r := dt.Execute(context.Background(), `{"key":""}`); !r.IsError {
		t.Fatal("empty key should be a tool error")
	}
	// Malformed args => error.
	if r := dt.Execute(context.Background(), `nope`); !r.IsError {
		t.Fatal("malformed args should be a tool error")
	}
}
