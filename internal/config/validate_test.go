package config

import "testing"

func TestValidate(t *testing.T) {
	cases := []struct {
		desc    string
		mutate  func(*Config)
		wantErr bool
	}{
		{desc: "default config is valid", mutate: func(*Config) {}, wantErr: false},
		{
			desc:    "unknown permission mode rejected",
			mutate:  func(c *Config) { c.Permissions.Mode = "smrat" },
			wantErr: true,
		},
		{
			desc:    "each valid mode accepted",
			mutate:  func(c *Config) { c.Permissions.Mode = "auto" },
			wantErr: false,
		},
		{
			desc:    "empty mode accepted (loader default applies)",
			mutate:  func(c *Config) { c.Permissions.Mode = "" },
			wantErr: false,
		},
		{
			desc:    "negative max_turns rejected",
			mutate:  func(c *Config) { n := -3; c.MaxTurns = &n },
			wantErr: true,
		},
		{
			desc:    "zero max_turns accepted (unlimited)",
			mutate:  func(c *Config) { n := 0; c.MaxTurns = &n },
			wantErr: false,
		},
		{
			desc:    "summarization threshold above 1 rejected",
			mutate:  func(c *Config) { c.Context.Summarization.Threshold = 1.5 },
			wantErr: true,
		},
		{
			desc:    "relevance borderline band out of order rejected",
			mutate:  func(c *Config) { c.Context.Relevance.BorderlineLo = 0.9; c.Context.Relevance.BorderlineHi = 0.2 },
			wantErr: true,
		},
		{
			desc:    "recency floor over 100 rejected",
			mutate:  func(c *Config) { c.Context.Relevance.RecencyFloorPct = 150 },
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			cfg := Default()
			tc.mutate(&cfg)
			err := cfg.Validate()
			if tc.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// TestValidateAllModesAccepted guards the mode allow-list against drift from
// permissions.ParseMode.
func TestValidateAllModesAccepted(t *testing.T) {
	for _, m := range []string{"off", "prompt-all", "smart", "auto"} {
		cfg := Default()
		cfg.Permissions.Mode = m
		if err := cfg.Validate(); err != nil {
			t.Errorf("mode %q should be valid, got: %v", m, err)
		}
	}
}
