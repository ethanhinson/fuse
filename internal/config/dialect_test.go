package config

import (
	"strings"
	"testing"
)

func TestModelDialectIsValidated(t *testing.T) {
	cfg := Default()
	cfg.Models.Entries = map[string]ModelConfig{
		"ok":    {ID: "a", Dialect: "anthropic"},
		"auto":  {ID: "b"},
		"wrong": {ID: "c", Dialect: "claude"},
	}
	var msgs []string
	for _, p := range cfg.Problems() {
		msgs = append(msgs, p.Error())
	}
	all := strings.Join(msgs, "\n")
	if !strings.Contains(all, "models.wrong.dialect") || !strings.Contains(all, "anthropic, openai") {
		t.Errorf("unknown dialect not reported: %q", all)
	}
	if strings.Contains(all, "models.ok.dialect") || strings.Contains(all, "models.auto.dialect") {
		t.Errorf("valid dialects reported: %q", all)
	}
}
