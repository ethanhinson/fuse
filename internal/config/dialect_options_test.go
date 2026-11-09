package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func problemsFor(t *testing.T, yamlDoc string) []string {
	t.Helper()
	c, err := FromYAML([]byte(yamlDoc), "t")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, p := range c.Problems() {
		out = append(out, p.Error())
	}
	return out
}

func TestDialectOptionsParse(t *testing.T) {
	c, err := FromYAML([]byte(`
models:
  m:
    id: cloud/x
    dialect: anthropic
    dialect_options: {cache_markers: false, reasoning_field: reasoning, recover_tool_calls: true}
`), "t")
	if err != nil {
		t.Fatal(err)
	}
	o := c.Models.Entries["m"].DialectOptions
	if o.CacheMarkers == nil || *o.CacheMarkers || o.RecoverToolCalls == nil || !*o.RecoverToolCalls || o.ReasoningField != "reasoning" {
		t.Fatalf("options = %+v", o)
	}
	mo := o.Model()
	if mo.CacheMarkers == nil || *mo.CacheMarkers || mo.RecoverToolCalls == nil || !*mo.RecoverToolCalls || mo.ReasoningField != "reasoning" {
		t.Fatalf("Model() = %+v", mo)
	}
	if mo.CacheMarkers == o.CacheMarkers {
		t.Error("Model() shares the pointer")
	}
	if back := DialectOptionsFrom(mo); !reflect.DeepEqual(back.Model(), mo) {
		t.Errorf("DialectOptionsFrom round trip = %+v", back)
	}
	if ps := c.Problems(); len(ps) != 0 {
		t.Errorf("problems: %v", ps)
	}
}

func TestDialectOptionsUnknownKey(t *testing.T) {
	// retry_empty_unstreamed is a per-family capability, not a user option.
	all := strings.Join(problemsFor(t, `
models:
  m:
    id: cloud/x
    dialect_options: {cache_marker: true, cache_markers: true, retry_empty_unstreamed: false}
`), "\n")
	for _, want := range []string{
		"models.m.dialect_options.cache_marker",
		"models.m.dialect_options.retry_empty_unstreamed",
		"is not a dialect option (want cache_markers, reasoning_field or recover_tool_calls)",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in:\n%s", want, all)
		}
	}
	if strings.Contains(all, "dialect_options.cache_markers") {
		t.Errorf("valid key reported:\n%s", all)
	}
	if strings.Index(all, "dialect_options.cache_marker") > strings.Index(all, "dialect_options.retry_empty_unstreamed") {
		t.Errorf("unknown keys not sorted:\n%s", all)
	}
}

func TestDialectOptionsReasoningFieldValidated(t *testing.T) {
	all := strings.Join(problemsFor(t, "models:\n  m: {id: a, dialect_options: {reasoning_field: foo}}\n"), "\n")
	want := `must be reasoning_content, reasoning or reasoning_text (or empty to replay reasoning in the field it arrived in), got "foo"`
	if !strings.Contains(all, "models.m.dialect_options.reasoning_field") || !strings.Contains(all, want) {
		t.Errorf("not reported: %q", all)
	}
	for _, f := range []string{"reasoning_content", "reasoning", "reasoning_text", `""`} {
		if ps := problemsFor(t, "models:\n  m: {id: a, dialect_options: {reasoning_field: "+f+"}}\n"); len(ps) != 0 {
			t.Errorf("%s rejected: %v", f, ps)
		}
	}
}

func TestDialectOptionsNotAMapping(t *testing.T) {
	for _, v := range []string{"true", "[a, b]"} {
		_, err := FromYAML([]byte("models:\n  m: {id: a, dialect_options: "+v+"}\n"), "t")
		if err == nil || !strings.Contains(err.Error(), "dialect_options must be a mapping") || !strings.Contains(err.Error(), `parse model "m"`) {
			t.Errorf("%s: err = %v", v, err)
		}
	}
	if _, err := FromYAML([]byte("models:\n  m: {id: a, dialect_options: {cache_markers: yes please}}\n"), "t"); err == nil {
		t.Error("a non-bool cache_markers must be a load error")
	}
}

func TestDialectOptionsNullIsUnset(t *testing.T) {
	for _, v := range []string{"", "{}", "null"} {
		doc := "models:\n  m:\n    id: a\n    dialect_options: " + v + "\n"
		c, err := FromYAML([]byte(doc), "t")
		if err != nil {
			t.Fatalf("%q: %v", v, err)
		}
		o := c.Models.Entries["m"].DialectOptions
		if o.CacheMarkers != nil || o.RecoverToolCalls != nil || o.ReasoningField != "" || len(o.unknown) != 0 {
			t.Errorf("%q: options = %+v", v, o)
		}
		if ps := c.Problems(); len(ps) != 0 {
			t.Errorf("%q: problems %v", v, ps)
		}
	}
}

func TestDialectOptionsWriteRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := SetModel("plain", ModelConfig{ID: "cloud/plain"}); err != nil {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	raw, err := os.ReadFile(filepath.Join(home, ".fuse", "config.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "dialect_options") {
		t.Errorf("a model with no options wrote the key:\n%s", raw)
	}

	f := false
	want := DialectOptions{CacheMarkers: &f, ReasoningField: "reasoning"}
	if err := SetModel("tuned", ModelConfig{ID: "cloud/tuned", DialectOptions: want}); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Models.Entries["tuned"].DialectOptions; !reflect.DeepEqual(got, want) {
		t.Errorf("reloaded = %+v, want %+v", got, want)
	}
}
