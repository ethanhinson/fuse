//go:build integration

package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/mxschmitt/playwright-go"

	"github.com/ethanhinson/fuse/internal/config"
)

// TestIntegration_HTTP_OAuth2 drives the full OAuth2 authorization-code + PKCE
// flow end-to-end: production GetAccessToken opens a browser at the authorize
// URL, Playwright (headless Chromium) navigates it, mock-oauth2 auto-approves
// and redirects to the local callback, the code is exchanged for tokens, and
// the resulting access token authorizes an MCP tools/list through an OAuth
// proxy. A second pass verifies silent refresh (no browser).
//
// GetAccessToken's browser open is an `open`/`xdg-open` (or $BROWSER) exec that
// receives the authorize URL as its argument. To make that URL observable
// WITHOUT any production-code change, the test prepends a temp dir to PATH that
// shadows `open`/`xdg-open` with a tiny script writing its argument to a file;
// the test polls that file and hands the URL to Playwright.
func TestIntegration_HTTP_OAuth2(t *testing.T) {
	requireServices(t)
	requirePlaywright(t)

	upstream, err := url.Parse(everythingBaseURL)
	if err != nil {
		t.Fatalf("parse upstream: %v", err)
	}
	proxy := newOAuthProxy(t, upstream, mockOAuthIssuerURL)

	tokenFile := filepath.Join(t.TempDir(), "tokens.json")
	cfg := config.MCPAuthConfig{
		Type:         "oauth2",
		ClientID:     "fuse-test",
		ClientSecret: "secret",
		Scopes:       []string{"openid"},
		TokenFile:    tokenFile,
	}

	urlFile := installBrowserShim(t)

	// Run GetAccessToken in the background; it blocks on the browser callback.
	type result struct {
		tok string
		err error
	}
	resCh := make(chan result, 1)
	go func() {
		// Discovery is served by the mock issuer; the client transport target
		// is the OAuth proxy. We drive discovery against the issuer URL.
		tok, err := GetAccessToken("everything", mockOAuthIssuerURL, cfg)
		resCh <- result{tok: tok, err: err}
	}()

	// Wait for the shim to capture the authorize URL, then drive it.
	authURL := waitForURL(t, urlFile, 30*time.Second)
	driveBrowser(t, authURL)

	var tok string
	select {
	case r := <-resCh:
		if r.err != nil {
			t.Fatalf("GetAccessToken (browser flow): %v", r.err)
		}
		tok = r.tok
	case <-time.After(60 * time.Second):
		t.Fatal("GetAccessToken did not complete within 60s")
	}
	if tok == "" {
		t.Fatal("GetAccessToken returned an empty access token")
	}

	// The token authorizes a tools/list through the OAuth proxy.
	client, err := newHTTPClient("everything", proxy.URL, tok)
	if err != nil {
		t.Fatalf("newHTTPClient with oauth token: %v", err)
	}
	defer client.stop()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := client.call(ctx, "tools/list", nil); err != nil {
		t.Fatalf("tools/list through oauth proxy: %v", err)
	}

	// Tokens were persisted to the hermetic TokenFile.
	if _, statErr := os.Stat(tokenFile); statErr != nil {
		t.Fatalf("expected persisted token file at %s: %v", tokenFile, statErr)
	}

	// --- Silent refresh: expire the access token, expect no browser open. ---
	expireAccessToken(t, tokenFile)
	// Reset the shim capture; if a browser is opened during refresh, the file
	// reappears and we fail.
	_ = os.Remove(urlFile)

	refreshed, err := GetAccessToken("everything", mockOAuthIssuerURL, cfg)
	if err != nil {
		t.Fatalf("GetAccessToken (refresh): %v", err)
	}
	if refreshed == "" {
		t.Fatal("refresh returned an empty access token")
	}
	if _, statErr := os.Stat(urlFile); statErr == nil {
		t.Fatal("refresh unexpectedly opened a browser (authorize URL captured)")
	}
}

// installBrowserShim prepends a temp dir to PATH containing `open` (macOS) or
// `xdg-open` (Linux) scripts that write their first argument (the URL) to a
// file. Returns the file path the URL will be written to.
func installBrowserShim(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	urlFile := filepath.Join(dir, "authurl.txt")

	names := []string{"xdg-open"}
	if runtime.GOOS == "darwin" {
		names = []string{"open"}
	}
	script := "#!/bin/sh\nprintf '%s' \"$1\" > " + shellQuote(urlFile) + "\n"
	for _, n := range names {
		p := filepath.Join(dir, n)
		if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
			t.Fatalf("write browser shim %s: %v", n, err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	// Some environments honor $BROWSER; point it at our shim too for safety.
	t.Setenv("BROWSER", filepath.Join(dir, names[0]))
	return urlFile
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func waitForURL(t *testing.T, path string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil && len(b) > 0 {
			return strings.TrimSpace(string(b))
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("authorize URL not captured within %s", timeout)
	return ""
}

// driveBrowser navigates headless Chromium to authURL and lets mock-oauth2's
// non-interactive login auto-redirect to the local callback, completing the
// flow. mock-oauth2 with interactiveLogin:false issues the redirect directly.
func driveBrowser(t *testing.T, authURL string) {
	t.Helper()
	browser, err := pw.Chromium.Launch(playwright.BrowserTypeLaunchOptions{
		Headless: playwright.Bool(true),
	})
	if err != nil {
		t.Fatalf("launch chromium: %v", err)
	}
	defer browser.Close()
	page, err := browser.NewPage()
	if err != nil {
		t.Fatalf("new page: %v", err)
	}
	if _, err := page.Goto(authURL, playwright.PageGotoOptions{
		WaitUntil: playwright.WaitUntilStateNetworkidle,
		Timeout:   playwright.Float(30000),
	}); err != nil {
		// A navigation "error" can be the expected redirect to the local
		// callback (which returns a plain success page) — not necessarily fatal.
		fmt.Fprintf(os.Stderr, "[oauth2-test] navigation note: %v\n", err)
	}
	// Give the callback a moment to be delivered.
	time.Sleep(500 * time.Millisecond)
}

// expireAccessToken rewrites the persisted token file so the access token is
// already expired, forcing GetAccessToken down the refresh path.
func expireAccessToken(t *testing.T, tokenFile string) {
	t.Helper()
	b, err := os.ReadFile(tokenFile)
	if err != nil {
		t.Fatalf("read token file: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("parse token file: %v", err)
	}
	if _, ok := m["refresh_token"]; !ok || m["refresh_token"] == "" {
		t.Skip("issuer did not return a refresh_token; cannot exercise silent refresh")
	}
	m["expiry"] = time.Now().Add(-time.Hour).Format(time.RFC3339Nano)
	out, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(tokenFile, out, 0o600); err != nil {
		t.Fatalf("rewrite token file: %v", err)
	}
}
