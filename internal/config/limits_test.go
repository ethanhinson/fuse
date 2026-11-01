package config

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
)

func TestLoopServerLimitsLoadFromTrustedFileOnly(t *testing.T) {
	doc := []byte(`
loop_server:
  limits:
    default: {max_live_loops: 2, starts_per_minute: 3}
    tenants:
      big: {max_live_loops: 10}
`)
	c, err := FromYAML(doc, "home")
	if err != nil {
		t.Fatal(err)
	}
	if c.LoopServer.Limits.Default != (LoopLimitConfig{MaxLiveLoops: 2, StartsPerMinute: 3}) {
		t.Fatalf("default = %+v", c.LoopServer.Limits.Default)
	}
	if got := c.LoopServer.Limits.Tenants["big"]; got != (LoopLimitConfig{MaxLiveLoops: 10}) {
		t.Fatalf("tenants[big] = %+v", got)
	}

	var warn bytes.Buffer
	old := warnw
	warnw = &warn
	defer func() { warnw = old }()
	u := Default()
	if err := mergeData(&u, doc, ".fuse.local.yml", false, nil); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(u.LoopServer.Limits, LoopLimitsConfig{}) {
		t.Fatalf("an untrusted file set limits: %+v", u.LoopServer.Limits)
	}
	if !strings.Contains(warn.String(), "loop_server") {
		t.Fatalf("no warning for an untrusted loop_server.limits block: %q", warn.String())
	}
}

func TestLoopServerLimitsDefaultIsUnlimited(t *testing.T) {
	c := Default()
	if !reflect.DeepEqual(c.LoopServer.Limits, LoopLimitsConfig{}) {
		t.Fatalf("default limits = %+v, want zero", c.LoopServer.Limits)
	}
	if c.LoopServer.Limits.Configured() {
		t.Fatal("zero limits report Configured")
	}
	if ps := c.Problems(); len(ps) != 0 {
		t.Fatalf("default config problems: %+v", ps)
	}
}

func TestLoopServerLimitsConfigured(t *testing.T) {
	cases := []struct {
		l    LoopLimitsConfig
		want bool
	}{
		{LoopLimitsConfig{}, false},
		{LoopLimitsConfig{Default: LoopLimitConfig{MaxLiveLoops: 1}}, true},
		{LoopLimitsConfig{Default: LoopLimitConfig{StartsPerMinute: 1}}, true},
		{LoopLimitsConfig{Tenants: map[string]LoopLimitConfig{"a": {}}}, true},
		{LoopLimitsConfig{Tenants: map[string]LoopLimitConfig{}}, false},
	}
	for i, tc := range cases {
		if got := tc.l.Configured(); got != tc.want {
			t.Errorf("case %d: Configured() = %v, want %v", i, got, tc.want)
		}
	}
}

func TestLoopServerLimitsProblems(t *testing.T) {
	badTenant := func(k string) string {
		return `tenant "` + k + `" must be _default or lowercase letters, digits, '-' or '_' (starting alphanumeric)`
	}
	cases := []struct {
		name     string
		limits   LoopLimitsConfig
		wantPath string
		wantMsg  string
	}{
		{"default max negative", LoopLimitsConfig{Default: LoopLimitConfig{MaxLiveLoops: -1}},
			"loop_server.limits.default.max_live_loops", "must be >= 0, got -1"},
		{"default rate negative", LoopLimitsConfig{Default: LoopLimitConfig{StartsPerMinute: -5}},
			"loop_server.limits.default.starts_per_minute", "must be >= 0, got -5"},
		{"tenant max negative", LoopLimitsConfig{Tenants: map[string]LoopLimitConfig{"acme": {MaxLiveLoops: -1}}},
			"loop_server.limits.tenants.acme.max_live_loops", "must be >= 0, got -1"},
		{"tenant rate negative", LoopLimitsConfig{Tenants: map[string]LoopLimitConfig{"acme": {StartsPerMinute: -2}}},
			"loop_server.limits.tenants.acme.starts_per_minute", "must be >= 0, got -2"},
		{"uppercase tenant", LoopLimitsConfig{Tenants: map[string]LoopLimitConfig{"Acme": {MaxLiveLoops: 1}}},
			"loop_server.limits.tenants", badTenant("Acme")},
		{"empty tenant", LoopLimitsConfig{Tenants: map[string]LoopLimitConfig{"": {MaxLiveLoops: 1}}},
			"loop_server.limits.tenants", badTenant("")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := Default()
			c.LoopServer.Limits = tc.limits
			ps := c.Problems()
			if len(ps) != 1 || ps[0].Path != tc.wantPath || ps[0].Message != tc.wantMsg {
				t.Fatalf("Problems() = %+v, want one {%s: %s}", ps, tc.wantPath, tc.wantMsg)
			}
		})
	}

	ok := Default()
	ok.LoopServer.Limits = LoopLimitsConfig{
		Default: LoopLimitConfig{MaxLiveLoops: 5, StartsPerMinute: 60},
		Tenants: map[string]LoopLimitConfig{
			"_default": {MaxLiveLoops: 1},
			"team-a":   {StartsPerMinute: 2},
		},
	}
	if ps := ok.Problems(); len(ps) != 0 {
		t.Fatalf("valid limits reported: %+v", ps)
	}
}
