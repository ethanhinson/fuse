package main

import (
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/config"
)

// gatewayAdapter applies gateway.request_timeout as the adapter's per-attempt
// deadline; an unset value keeps the adapter's built-in default.
func TestGatewayAdapterAppliesRequestTimeout(t *testing.T) {
	cfg := config.Default()
	def := gatewayAdapter(cfg, nil).RequestTimeout
	if def <= 0 {
		t.Fatalf("default RequestTimeout = %v, want a positive built-in", def)
	}
	cfg.Gateway.RequestTimeout = "20m"
	if got := gatewayAdapter(cfg, nil).RequestTimeout; got != 20*time.Minute {
		t.Fatalf("RequestTimeout = %v, want 20m", got)
	}
	// Every consumer of gatewayAdapter (main model, summarizer, relevance
	// classifier) shares the knob because they all build through it.
	cfg.Gateway.RequestTimeout = ""
	if got := gatewayAdapter(cfg, nil).RequestTimeout; got != def {
		t.Fatalf("empty request_timeout: RequestTimeout = %v, want the default %v", got, def)
	}
}
