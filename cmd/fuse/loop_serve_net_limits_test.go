package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/loopwire/v1/loopv1connect"
	"github.com/ethanhinson/fuse/internal/tenantlimit"

	loopv1 "github.com/ethanhinson/fuse/internal/loopwire/v1"
)

// listOnlyRegistry is a LoopRegistry for the limiter constructor test: the
// constructor only needs a non-nil registry to count against.
type listOnlyRegistry struct{ event.LoopRegistry }

func TestLoopTenantLimiterFromConfig(t *testing.T) {
	now := func() time.Time { return time.Unix(0, 0) }
	reg := listOnlyRegistry{}

	lim, err := loopTenantLimiter(config.Config{}, reg, now)
	if err != nil || lim != nil {
		t.Fatalf("no limits: got (%v, %v), want (nil, nil)", lim, err)
	}

	var cfg config.Config
	cfg.LoopServer.Limits = config.LoopLimitsConfig{
		Default: config.LoopLimitConfig{MaxLiveLoops: 2, StartsPerMinute: 3},
		Tenants: map[string]config.LoopLimitConfig{"big": {MaxLiveLoops: 10}},
	}
	lim, err = loopTenantLimiter(cfg, reg, now)
	if err != nil || lim == nil {
		t.Fatalf("limits set: got (%v, %v), want a limiter", lim, err)
	}
	if got, want := lim.For("big"), (tenantlimit.Limits{MaxLiveLoops: 10, StartsPerMinute: 3}); got != want {
		t.Errorf("For(big) = %+v, want %+v", got, want)
	}
	if got, want := lim.For("x"), (tenantlimit.Limits{MaxLiveLoops: 2, StartsPerMinute: 3}); got != want {
		t.Errorf("For(x) = %+v, want %+v", got, want)
	}

	if _, err := loopTenantLimiter(cfg, nil, now); err == nil {
		t.Error("max_live_loops with no registry: want an error, got nil")
	}

	// A rate-only config needs no registry.
	var rateOnly config.Config
	rateOnly.LoopServer.Limits.Default.StartsPerMinute = 5
	if lim, err := loopTenantLimiter(rateOnly, nil, now); err != nil || lim == nil {
		t.Errorf("rate-only with no registry: got (%v, %v), want a limiter", lim, err)
	}
}

// TestLoopServeNetConstructsLimiter guards against the limiter being inert in
// the binary: some non-test file in cmd/fuse must call tenantlimit.New, and
// runLoopServeNet must thread the result into serveNetOptions.limits.
func TestLoopServeNetConstructsLimiter(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	var newCalls, limitsFields int
	for _, pkg := range pkgs {
		for _, f := range pkg.Files {
			ast.Inspect(f, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.CallExpr:
					if sel, ok := x.Fun.(*ast.SelectorExpr); ok {
						if id, ok := sel.X.(*ast.Ident); ok && id.Name == "tenantlimit" && sel.Sel.Name == "New" {
							newCalls++
						}
					}
				case *ast.CompositeLit:
					if id, ok := x.Type.(*ast.Ident); ok && id.Name == "serveNetOptions" {
						for _, el := range x.Elts {
							if kv, ok := el.(*ast.KeyValueExpr); ok {
								if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "limits" {
									limitsFields++
								}
							}
						}
					}
				}
				return true
			})
		}
	}
	if newCalls == 0 {
		t.Error("no non-test call to tenantlimit.New in cmd/fuse: the limiter is never built")
	}
	if limitsFields == 0 {
		t.Error("no serveNetOptions literal sets limits: the limiter never reaches the handler")
	}
}

// TestLoopServeNetEnforcesTenantLimitsOverH2C runs the real `fuse
// loop-serve-net` command with a loop_server.limits block and checks both
// refusals and the advertised limits over the wire.
func TestLoopServeNetEnforcesTenantLimitsOverH2C(t *testing.T) {
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"done"}}]}`))
	}))
	t.Cleanup(gateway.Close)

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("LLM_GATEWAY_URL", gateway.URL)
	t.Setenv("LLM_GATEWAY_KEY", "test-key")
	if err := os.Mkdir(filepath.Join(home, ".fuse"), 0o755); err != nil {
		t.Fatal(err)
	}
	configYAML := fmt.Sprintf(`gateway:
  url: %q
  key: test-key
models:
  default: fast
  fast: {id: cloud/fast}
throughput: {requests_per_minute: 6000}
loop_server:
  auth:
    - {token: tok-a, tenant: tenant-a, subject: a}
    - {token: tok-b, tenant: tenant-b, subject: b}
    - {token: tok-c, tenant: tenant-c, subject: c}
  limits:
    default: {max_live_loops: 1}
    tenants:
      tenant-b: {max_live_loops: 3}
      tenant-c: {starts_per_minute: 1}
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
	client := func(tok string) loopv1connect.LoopServiceClient {
		return loopv1connect.NewLoopServiceClient(h2cClient(), base,
			connect.WithInterceptors(bearerClientInterceptor{token: tok}))
	}
	a, b, c := client("tok-a"), client("tok-b"), client("tok-c")

	caps := func(cl loopv1connect.LoopServiceClient) *loopv1.TenantLimits {
		t.Helper()
		resp, err := cl.GetCapabilities(context.Background(), connect.NewRequest(&loopv1.GetCapabilitiesRequest{}))
		if err != nil {
			t.Fatalf("GetCapabilities: %v; logs:\n%s", err, logs.String())
		}
		return resp.Msg.GetLimits()
	}
	// Readiness: retry the first call until the server answers.
	deadline := time.Now().Add(3 * time.Second)
	for {
		_, err = a.GetCapabilities(context.Background(), connect.NewRequest(&loopv1.GetCapabilitiesRequest{}))
		if err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("GetCapabilities over h2c: %v; logs:\n%s", err, logs.String())
	}
	for _, tc := range []struct {
		name       string
		cl         loopv1connect.LoopServiceClient
		live, rate uint32
	}{{"tenant-a", a, 1, 0}, {"tenant-b", b, 3, 0}, {"tenant-c", c, 1, 1}} {
		l := caps(tc.cl)
		if l.GetMaxLiveLoops() != tc.live || l.GetStartsPerMinute() != tc.rate {
			t.Errorf("%s limits = {%d, %d}, want {%d, %d}", tc.name,
				l.GetMaxLiveLoops(), l.GetStartsPerMinute(), tc.live, tc.rate)
		}
	}

	start := func(cl loopv1connect.LoopServiceClient) error {
		_, err := cl.StartLoop(context.Background(), connect.NewRequest(&loopv1.StartLoopRequest{Task: "t", Interactive: true}))
		return err
	}
	refused := func(name string, err error, limit string) *connect.Error {
		t.Helper()
		var cerr *connect.Error
		if !errors.As(err, &cerr) || cerr.Code() != connect.CodeResourceExhausted {
			t.Fatalf("%s: err = %v, want ResourceExhausted", name, err)
		}
		if !strings.Contains(cerr.Message(), limit) {
			t.Errorf("%s: message %q does not name %s", name, cerr.Message(), limit)
		}
		return cerr
	}

	if err := start(a); err != nil {
		t.Fatalf("tenant-a first start: %v; logs:\n%s", err, logs.String())
	}
	refused("tenant-a second start", start(a), "max_live_loops")

	for i := 0; i < 2; i++ {
		if err := start(b); err != nil {
			t.Fatalf("tenant-b start %d: %v", i+1, err)
		}
	}

	if err := start(c); err != nil {
		t.Fatalf("tenant-c first start: %v", err)
	}
	cerr := refused("tenant-c second start", start(c), "starts_per_minute")
	ra := cerr.Meta().Get("Retry-After")
	if n, err := strconv.Atoi(ra); err != nil || n <= 0 {
		t.Errorf("tenant-c Retry-After = %q, want a positive integer", ra)
	}
}
