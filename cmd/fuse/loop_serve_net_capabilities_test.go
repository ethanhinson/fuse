package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"

	loopv1 "github.com/ethanhinson/fuse/internal/loopwire/v1"
	"github.com/ethanhinson/fuse/internal/loopwire/v1/loopv1connect"
	"github.com/ethanhinson/fuse/sdk/fuse"
)

// h2cClient speaks cleartext HTTP/2 (prior knowledge), the way a production
// client reaches a loop-serve-net listener without TLS.
func h2cClient() *http.Client {
	return &http.Client{Transport: &http2.Transport{AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		}}}
}

// TestLoopServeNetAdvertisesCapabilitiesOverH2C runs the real `fuse
// loop-serve-net` command against a scripted gateway and reads GetCapabilities
// over h2c, both raw and through the public SDK: per-tenant secret refs, hidden
// and cli/ models left out, and the auth refusals.
func TestLoopServeNetAdvertisesCapabilitiesOverH2C(t *testing.T) {
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"done"}}]}`))
	}))
	t.Cleanup(gateway.Close)

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("LLM_GATEWAY_URL", gateway.URL)
	t.Setenv("LLM_GATEWAY_KEY", "test-key")
	t.Setenv("FUSE_TEST_CAPS_KEY", "v")
	if err := os.Mkdir(filepath.Join(home, ".fuse"), 0o755); err != nil {
		t.Fatal(err)
	}
	configYAML := fmt.Sprintf(`gateway:
  url: %q
  key: test-key
models:
  default: fast
  fast: {id: cloud/fast, label: Fast}
  quiet: {id: cloud/quiet, advertise: false}
throughput: {requests_per_minute: 6000}
loop_server:
  auth:
    - {token: tok-a, tenant: tenant-a, subject: a}
    - {token: tok-b, tenant: tenant-b, subject: b}
  secrets:
    - {name: caps-key, env: FUSE_TEST_CAPS_KEY, tenants: [tenant-a]}
`, gateway.URL)
	if err := os.WriteFile(filepath.Join(home, ".fuse", "config.yml"), []byte(configYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	oldListen, oldContext := netListen, serveNetContext
	netListen = func(string, string) (net.Listener, error) { return ln, nil }
	serveNetContext = func() (context.Context, context.CancelFunc) { return ctx, func() {} }
	var logs lockedBuffer
	commandDone := make(chan int, 1)
	go func() {
		commandDone <- run([]string{"loop-serve-net", "--addr", ln.Addr().String()}, &logs, &bytes.Buffer{})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-commandDone:
		case <-time.After(5 * time.Second):
			t.Errorf("loop-serve-net did not exit within 5s; logs:\n%s", logs.String())
		}
		netListen, serveNetContext = oldListen, oldContext
	})

	base := "http://" + ln.Addr().String()
	raw := loopv1connect.NewLoopServiceClient(h2cClient(), base,
		connect.WithInterceptors(bearerClientInterceptor{token: "tok-a"}))
	var resp *connect.Response[loopv1.GetCapabilitiesResponse]
	deadline := time.Now().Add(3 * time.Second)
	for {
		resp, err = raw.GetCapabilities(context.Background(), connect.NewRequest(&loopv1.GetCapabilitiesRequest{}))
		if err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("raw GetCapabilities over h2c: %v; logs:\n%s", err, logs.String())
	}
	var aliases []string
	for _, m := range resp.Msg.Models {
		aliases = append(aliases, m.Alias)
	}
	if !slices.Contains(aliases, "fast") {
		t.Errorf("models %v do not list fast", aliases)
	}
	for _, hidden := range []string{"quiet", "claude-max"} {
		if slices.Contains(aliases, hidden) {
			t.Errorf("models %v list %s", aliases, hidden)
		}
	}
	if !slices.Equal(resp.Msg.SecretRefs, []string{"caps-key"}) {
		t.Errorf("tenant-a secret refs = %v, want [caps-key]", resp.Msg.SecretRefs)
	}

	sdkB := fuse.NewRemote(base, fuse.Credentials{Token: "tok-b", Tenant: "tenant-b"}, fuse.WithHTTPClient(h2cClient()))
	b, err := sdkB.Capabilities(context.Background())
	if err != nil {
		t.Fatalf("SDK Capabilities for tenant-b: %v", err)
	}
	if len(b.SecretRefs) != 0 {
		t.Errorf("tenant-b secret refs = %v, want none", b.SecretRefs)
	}
	if len(b.Models) != len(resp.Msg.Models) || len(b.Ceilings) != len(resp.Msg.Ceilings) {
		t.Errorf("SDK saw %d models and %d ceilings, raw saw %d and %d",
			len(b.Models), len(b.Ceilings), len(resp.Msg.Models), len(resp.Msg.Ceilings))
	}
	if len(b.Ceilings) == 0 {
		t.Error("no ceilings advertised")
	}

	spoof := fuse.NewRemote(base, fuse.Credentials{Token: "tok-b", Tenant: "tenant-a"}, fuse.WithHTTPClient(h2cClient()))
	if _, err := spoof.Capabilities(context.Background()); !errors.Is(err, fuse.ErrPermissionDenied) {
		t.Errorf("spoofed tenant: err = %v, want ErrPermissionDenied", err)
	}
	anon := fuse.NewRemote(base, fuse.Credentials{Tenant: "tenant-a"}, fuse.WithHTTPClient(h2cClient()))
	if _, err := anon.Capabilities(context.Background()); !errors.Is(err, fuse.ErrUnauthenticated) {
		t.Errorf("no token: err = %v, want ErrUnauthenticated", err)
	}
}
