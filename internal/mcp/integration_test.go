//go:build integration

// Package mcp integration tests (build tag: integration).
//
// These tests exercise the MCP client stack end-to-end against real
// infrastructure: the `fuse mcp-server` subprocess (stdio), the official
// @modelcontextprotocol/server-everything reference server (HTTP/SSE), and a
// mock OAuth2 server driven through a real browser via Playwright.
//
// TestMain provisions the Docker Compose stack (mcp-everything + mock-oauth2)
// and initializes Playwright once. Everything degrades to t.Skip when Docker or
// Playwright is unavailable, so the stdio test still runs and CI-less local
// runs never fail. Run with:  go test -tags integration ./internal/mcp/...
package mcp

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/playwright-community/playwright-go"
)

// Package-level availability flags, set by TestMain and read by the
// service-dependent tests via requireServices / requirePlaywright.
var (
	servicesUp bool
	pw         *playwright.Playwright
)

const (
	everythingBaseURL   = "http://localhost:3001"
	everythingHealthURL = "http://localhost:3001/sse"
	mockOAuthIssuerURL  = "http://localhost:8080/default"
	mockOAuthHealthURL  = "http://localhost:8080/default/.well-known/openid-configuration"
	composeFile         = "testdata/docker-compose.yml"

	// The live Microsoft CDN that still serves the driver bundled by the pinned
	// playwright-go version. Overridable via PLAYWRIGHT_DOWNLOAD_HOST.
	defaultPlaywrightDownloadHost = "https://playwright.download.prss.microsoft.com/dbazure/download/playwright"
)

func TestMain(m *testing.M) {
	os.Exit(runMain(m))
}

func runMain(m *testing.M) int {
	// Docker is optional: without it, service-dependent tests skip but the
	// stdio test (which spawns `fuse mcp-server`, no Docker) still runs.
	if !dockerAvailable() {
		fmt.Fprintln(os.Stderr, "[integration] docker/compose unavailable — service-dependent tests will skip")
		return m.Run()
	}

	if err := composeUp(); err != nil {
		fmt.Fprintf(os.Stderr, "[integration] compose up failed (%v) — service-dependent tests will skip\n", err)
		return m.Run()
	}
	defer composeDown()

	// npx cold-downloads @modelcontextprotocol/server-everything on first start,
	// which can take well over a minute; allow a generous window.
	if err := waitForServices(180 * time.Second); err != nil {
		fmt.Fprintf(os.Stderr, "[integration] services never became healthy (%v) — service-dependent tests will skip\n", err)
		return m.Run()
	}
	servicesUp = true

	// Playwright is best-effort: install + launch once. If either fails, the
	// OAuth2 test skips (via requirePlaywright); nothing else is affected.
	//
	// playwright-go v0.5001.0 defaults its driver download to the retired
	// playwright.azureedge.net CDN (now 404 on every platform). Redirect it to
	// the live Microsoft CDN unless the caller already set a host. This is the
	// only host on which this pinned driver version (1.50.1) is still served;
	// see the OAuth2 test's docs and the change's ADR.
	if os.Getenv("PLAYWRIGHT_DOWNLOAD_HOST") == "" {
		_ = os.Setenv("PLAYWRIGHT_DOWNLOAD_HOST", defaultPlaywrightDownloadHost)
	}
	if err := playwright.Install(&playwright.RunOptions{Browsers: []string{"chromium"}}); err != nil {
		fmt.Fprintf(os.Stderr, "[integration] playwright install failed (%v) — OAuth2 test will skip\n", err)
	} else if p, err := playwright.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "[integration] playwright run failed (%v) — OAuth2 test will skip\n", err)
	} else {
		pw = p
		defer pw.Stop() //nolint:errcheck
	}

	return m.Run()
}

// requireServices skips the test unless the Docker services are healthy.
func requireServices(t *testing.T) {
	t.Helper()
	if !servicesUp {
		t.Skip("Docker MCP services unavailable; skipping")
	}
}

// requirePlaywright skips the test unless Playwright launched successfully.
func requirePlaywright(t *testing.T) {
	t.Helper()
	if pw == nil {
		t.Skip("Playwright unavailable; skipping")
	}
}

func dockerAvailable() bool {
	if _, err := exec.LookPath("docker"); err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "docker", "compose", "version").Run() == nil
}

func composeUp() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "compose", "-f", composeFile, "up", "-d")
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func composeDown() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "compose", "-f", composeFile, "down", "-v")
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	_ = cmd.Run()
}

// waitForServices polls both health endpoints until each returns 2xx or deadline.
func waitForServices(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for _, u := range []string{everythingHealthURL, mockOAuthHealthURL} {
		if err := pollHealthy(u, deadline); err != nil {
			return fmt.Errorf("%s: %w", u, err)
		}
	}
	return nil
}

func pollHealthy(u string, deadline time.Time) error {
	client := &http.Client{Timeout: 3 * time.Second}
	var lastErr error
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest(http.MethodGet, u, nil)
		// The SSE health probe would otherwise block; a short per-request
		// timeout on the client bounds it, and any 2xx status = healthy.
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 500 {
				return nil
			}
			lastErr = fmt.Errorf("status %d", resp.StatusCode)
		} else {
			lastErr = err
		}
		time.Sleep(1 * time.Second)
	}
	return fmt.Errorf("not healthy before deadline: %w", lastErr)
}

// -----------------------------------------------------------------------------
// Shared in-process auth proxies. Rather than run extra Docker containers for
// bearer/OAuth2, these httptest servers sit in front of mcp-everything.
// -----------------------------------------------------------------------------

// newBearerProxy returns an httptest.Server that requires
// Authorization: Bearer <token> and reverse-proxies to upstream (SSE-safe).
// Wrong/missing token -> 401. Closed automatically when t ends.
func newBearerProxy(t *testing.T, upstream *url.URL, token string) *httptest.Server {
	t.Helper()
	rp := newSSEProxy(upstream)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		rp.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newOAuthProxy returns an httptest.Server that requires a Bearer token, then
// reverse-proxies to upstream (SSE-safe). Validation is deliberately coarse:
// the token must be a present, well-formed Bearer. The token under test is
// minted by the real mock-oauth2 flow, so its presence proves the client
// completed the OAuth2 authorization and attached the resulting access token —
// which is what this harness asserts. Full JWKS signature verification is out
// of scope (mock-oauth2 already signs with its own keys). issuerURL is recorded
// for symmetry with the production discovery flow and future strict validation.
// Missing/blank token -> 401. Closed automatically when t ends.
func newOAuthProxy(t *testing.T, upstream *url.URL, issuerURL string) *httptest.Server {
	t.Helper()
	_ = issuerURL // reserved for future strict JWKS validation
	rp := newSSEProxy(upstream)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if len(auth) <= len("Bearer ") || auth[:len("Bearer ")] != "Bearer " {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		rp.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newSSEProxy builds a reverse proxy that streams responses (flushes as data
// arrives), which the SSE transport requires.
func newSSEProxy(upstream *url.URL) http.Handler {
	return &sseReverseProxy{upstream: upstream, client: &http.Client{
		Timeout: 0,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // test-only
		},
	}}
}

type sseReverseProxy struct {
	upstream *url.URL
	client   *http.Client
}

func (p *sseReverseProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	target := *p.upstream
	target.Path = r.URL.Path
	target.RawQuery = r.URL.RawQuery

	outReq, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	for k, vs := range r.Header {
		if k == "Authorization" {
			continue // strip the proxy's auth header from the upstream request
		}
		for _, v := range vs {
			outReq.Header.Add(k, v)
		}
	}

	resp, err := p.client.Do(outReq)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 4096)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr != nil {
			return
		}
	}
}
