package main

import (
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/model"
)

// Locally a slow model is never killed for being slow: only silence or the user
// ends a call. An operator can still opt in to a limit.
func TestLocalModelCallsHaveNoTotalLimit(t *testing.T) {
	if got := gatewayAdapter(config.Config{}, nil).RequestTimeout; got != 0 {
		t.Errorf("RequestTimeout = %v, want none", got)
	}
	cfg := config.Config{}
	cfg.Gateway.RequestTimeout = "7m"
	if got := gatewayAdapter(cfg, nil).RequestTimeout; got != 7*time.Minute {
		t.Errorf("RequestTimeout = %v, want the configured 7m", got)
	}
}

// Helper calls block something small, so they keep a limit in every binding.
func TestHelperCallsKeepATotalLimit(t *testing.T) {
	if model.HelperRequestTimeout <= 0 || model.HelperRequestTimeout > 10*time.Minute {
		t.Errorf("HelperRequestTimeout = %v", model.HelperRequestTimeout)
	}
	cfg := config.Config{}
	cfg.Gateway.URL = "http://localhost:1"
	r := newHumanRouter(cfg, registryFromConfig(cfg))
	if got := r.(*humanRouter).client.(*model.Adapter).RequestTimeout; got != model.HelperRequestTimeout {
		t.Errorf("router limit = %v", got)
	}
}
