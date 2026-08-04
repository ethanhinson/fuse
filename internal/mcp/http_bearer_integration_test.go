//go:build integration

package mcp

import (
	"context"
	"net/url"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/config"
)

// TestIntegration_HTTP_Bearer exercises bearer-token auth against
// server-everything through an in-process bearer-enforcing proxy. The bearer
// token is carried in MCPAuthConfig.ClientSecret (there is no Token field).
func TestIntegration_HTTP_Bearer(t *testing.T) {
	requireServices(t)

	const token = "test-token-xyz"
	upstream, err := url.Parse(everythingBaseURL)
	if err != nil {
		t.Fatalf("parse upstream: %v", err)
	}
	proxy := newBearerProxy(t, upstream, token)

	t.Run("wrongToken", func(t *testing.T) {
		// A bad bearer token must be rejected by the proxy at SSE connect (401),
		// so the client cannot even establish the transport.
		_, err := newHTTPClient("everything", proxy.URL, "wrong-token")
		if err == nil {
			t.Fatal("expected newHTTPClient to fail with a wrong bearer token, got nil")
		}
	})

	t.Run("correctToken", func(t *testing.T) {
		// The token flows through GetAccessToken exactly as production does.
		tok, err := GetAccessToken("everything", proxy.URL, config.MCPAuthConfig{
			Type:         "bearer",
			ClientSecret: token,
		})
		if err != nil {
			t.Fatalf("GetAccessToken: %v", err)
		}
		if tok != token {
			t.Fatalf("GetAccessToken returned %q, want %q", tok, token)
		}

		client, err := newHTTPClient("everything", proxy.URL, tok)
		if err != nil {
			t.Fatalf("newHTTPClient with correct token: %v", err)
		}
		defer client.stop()

		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if _, err := client.call(ctx, "tools/list", nil); err != nil {
			t.Fatalf("tools/list through bearer proxy: %v", err)
		}
	})
}
