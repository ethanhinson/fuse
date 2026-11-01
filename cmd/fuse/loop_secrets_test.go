package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/ethanhinson/fuse/harness"
	harnessv1 "github.com/ethanhinson/fuse/harness/v1"
	"github.com/ethanhinson/fuse/internal/event/fsstore"
	"github.com/ethanhinson/fuse/internal/loopauth"
	"github.com/ethanhinson/fuse/internal/loopsecret"
	loopv1 "github.com/ethanhinson/fuse/internal/loopwire/v1"
	"github.com/ethanhinson/fuse/internal/runtime"
)

// loopSecretSpec is a one-turn spec whose only MCP server, at url, takes a
// bearer credential from the secret_ref "my-token".
func loopSecretSpec(url string) *harnessv1.HarnessSpec {
	turns := uint32(1)
	return &harnessv1.HarnessSpec{MaxTurns: &turns, McpServers: []*harnessv1.McpServer{{
		Name: "s", Transport: harnessv1.McpTransport_MCP_TRANSPORT_STREAMABLE_HTTP, Url: url,
		Auth: &harnessv1.McpAuth{Type: harnessv1.McpAuthType_MCP_AUTH_TYPE_BEARER, ClientSecretRef: "my-token"},
	}}}
}

// assertGatewayNeverSaw fails when any request the model gateway received
// holds value: the model never sees a loop-scoped secret.
func assertGatewayNeverSaw(t *testing.T, gw *recordingGateway, value string) {
	t.Helper()
	for i, c := range gw.snapshot() {
		b, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), value) {
			t.Fatalf("gateway request %d holds the secret value", i)
		}
	}
}

// TestLoopScopedSecretReachesTheMCPServer is the composition-root proof for
// loop-scoped secrets: on the production loop-server deps, a value carried in
// LoopConfig.Secrets reaches the spec's MCP server as its bearer credential,
// and no recorded event and no model request ever holds it.
func TestLoopScopedSecretReachesTheMCPServer(t *testing.T) {
	const value = "cr-SENTINEL"
	rt, gw, _ := harnessServer(t, harnessOperator)
	srv, auths := mcpAuthFakeServer(t)
	h, err := rt.StartLoop(context.Background(), runtime.LoopConfig{
		Task: "go", Harness: loopSecretSpec(srv.URL),
		Secrets: []loopsecret.Secret{{Name: "my-token", Value: value, Hosts: []string{"127.0.0.1"}}},
	})
	if err != nil {
		t.Fatalf("StartLoop: %v", err)
	}
	_, _ = h.Wait()

	seen := false
	for _, a := range auths() {
		seen = seen || a == "Bearer "+value
	}
	if !seen {
		t.Fatalf("no MCP request carried the loop-scoped secret; saw %v", auths())
	}
	hist, err := rt.Attach(context.Background(), "", h.ID(), 0)
	if err != nil {
		t.Fatal(err)
	}
	evs := mcpServerEvents(t, hist)
	if len(evs) != 1 || !evs[0].Connected {
		t.Fatalf("mcp.server events = %+v, want one connected", evs)
	}
	for _, e := range hist {
		if strings.Contains(string(e.Payload), value) {
			t.Fatalf("a %s event holds the secret value", e.Kind)
		}
	}
	if len(gw.snapshot()) == 0 {
		t.Fatal("the gateway received no request")
	}
	assertGatewayNeverSaw(t, gw, value)
}

// TestLoopScopedSecretHostMismatchIsRefused: a loop-scoped value bound to
// another host is never sent; the start fails with the host-rule violation at
// the ref's own field, before anything dials the MCP server or the model.
func TestLoopScopedSecretHostMismatchIsRefused(t *testing.T) {
	const value = "cr-SENTINEL"
	rt, gw, _ := harnessServer(t, harnessOperator)
	srv, auths := mcpAuthFakeServer(t)
	_, err := rt.StartLoop(context.Background(), runtime.LoopConfig{
		Task: "go", Harness: loopSecretSpec(srv.URL),
		Secrets: []loopsecret.Secret{{Name: "my-token", Value: value, Hosts: []string{"other.test"}}},
	})
	var ve *harness.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want a ValidationError", err)
	}
	if len(ve.Violations) != 1 {
		t.Fatalf("violations = %+v, want exactly one", ve.Violations)
	}
	v := ve.Violations[0]
	if v.FieldPath != "mcp_servers[0].auth.client_secret_ref" || v.RuleID != harness.RuleSecretRef {
		t.Fatalf("violation = %s %s, want mcp_servers[0].auth.client_secret_ref %s", v.FieldPath, v.RuleID, harness.RuleSecretRef)
	}
	if want := `loop secret "my-token" may only be sent to the hosts it lists, and "127.0.0.1" is not one of them`; v.Message != want {
		t.Fatalf("message = %q, want %q", v.Message, want)
	}
	if strings.Contains(err.Error(), value) {
		t.Fatal("the refusal echoes the secret value")
	}
	if n := len(auths()); n != 0 {
		t.Fatalf("the MCP server got %d requests for a refused start", n)
	}
	if n := len(gw.snapshot()); n != 0 {
		t.Fatalf("a refused start reached the model %d times", n)
	}
}

// TestOperatorSecretWinsAtTheCompositionRoot: an operator loop_server.secrets
// entry of the same name always wins over the loop-scoped value.
func TestOperatorSecretWinsAtTheCompositionRoot(t *testing.T) {
	t.Setenv("FUSE_TEST_OP_TOKEN", "operator-v")
	rt, _, _ := harnessServer(t, harnessOperator+"loop_server:\n  secrets:\n    - {name: my-token, env: FUSE_TEST_OP_TOKEN}\n")
	srv, auths := mcpAuthFakeServer(t)
	h, err := rt.StartLoop(context.Background(), runtime.LoopConfig{
		Task: "go", Harness: loopSecretSpec(srv.URL),
		Secrets: []loopsecret.Secret{{Name: "my-token", Value: "client-v", Hosts: []string{"127.0.0.1"}}},
	})
	if err != nil {
		t.Fatalf("StartLoop: %v", err)
	}
	_, _ = h.Wait()
	sawOperator := false
	for _, a := range auths() {
		if a == "Bearer client-v" {
			t.Fatal("the loop-scoped value was sent although an operator secret of that name exists")
		}
		sawOperator = sawOperator || a == "Bearer operator-v"
	}
	if !sawOperator {
		t.Fatalf("no MCP request carried the operator secret; saw %v", auths())
	}
}

// The tests below are the acceptance proof for loop-scoped secrets over the
// real Connect edge: the production verifier (buildLoopVerifier), the
// production serveNet mount, the production loop-server deps and a durable
// fsstore. They prove the secrets:inline scope, the wire limits, and that a
// value is never recorded: not in the store, not in any log, not in an error,
// not in a model request.

// e2eValue is the loop-scoped secret value the end-to-end tests grep for.
const e2eValue = "e2e-SENTINEL-d41d8"

// secretsOperator is harnessOperator with three static tokens for tenant acme:
// one without secrets:inline, one with it, and one unrestricted.
const secretsOperator = harnessOperator + `
loop_server:
  auth:
    - {token: start-tok, tenant: acme, subject: alice, scopes: [loop:start, loop:observe]}
    - {token: inline-tok, tenant: acme, subject: alice, scopes: [loop:start, loop:observe, secrets:inline]}
    - {token: full-tok, tenant: acme, subject: ops}
`

// secretsNet is a running end-to-end server for the loop-secret tests.
type secretsNet struct {
	base string            // the Connect server's base URL
	dir  string            // holds the durable store, the base dir and the log file
	home string            // the server's HOME
	gw   *recordingGateway // every model request the server made
	logs func() string     // everything the process logged while the server ran
	stop func()            // stops the server and waits for it; idempotent
	log  string            // the log file's path (inside dir)
}

// secretsNetServer stands up the production loop server over the real Connect
// edge with operatorYAML, a durable fsstore under dir, and every log sink the
// server and its loops write to redirected into one file under dir.
func secretsNetServer(t *testing.T, operatorYAML string) *secretsNet {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "server.log")
	f, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	// Redirect before the deps are built: the loop-server deps capture
	// os.Stderr (the MCP mint-audit writer) at construction. The restore is
	// registered first so it runs last, after the server and every httptest
	// server have stopped.
	prevLog, prevFlags := log.Writer(), log.Flags()
	prevSlog, prevStderr, prevIssuer := slog.Default(), os.Stderr, issuerLogWriter
	t.Cleanup(func() {
		log.SetOutput(prevLog)
		log.SetFlags(prevFlags)
		slog.SetDefault(prevSlog)
		os.Stderr = prevStderr
		issuerLogWriter = prevIssuer
		_ = f.Close()
	})
	log.SetOutput(f)
	slog.SetDefault(slog.New(slog.NewTextHandler(f, nil)))
	os.Stderr = f
	issuerLogWriter = f

	gw := &recordingGateway{}
	deps, host := harnessServerWith(t, operatorYAML, gw)
	store := fsstore.NewDurableFSStore(filepath.Join(dir, "store"))
	deps.DurableStore, deps.Registry = store, store
	deps.BaseDir = filepath.Join(dir, "base")
	rt := runtime.New(deps)

	v, _, err := buildLoopVerifier(host.operator.cfg)
	if err != nil {
		t.Fatalf("buildLoopVerifier: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	serveDone := make(chan error, 1)
	go func() { serveDone <- serveNet(ctx, ln, rt, v, store) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case <-serveDone:
			case <-time.After(5 * time.Second):
				t.Error("serveNet did not return after cancel")
			}
		})
	}
	t.Cleanup(stop)
	return &secretsNet{
		base: "http://" + ln.Addr().String(),
		dir:  dir,
		home: os.Getenv("HOME"),
		gw:   gw,
		log:  logPath,
		stop: stop,
		logs: func() string {
			b, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatal(err)
			}
			return string(b)
		},
	}
}

// secretsStart is a StartLoop request for loopSecretSpec(mcpURL) carrying one
// loop-scoped secret "my-token" = e2eValue, bound to hosts.
func secretsStart(mcpURL string, hosts ...string) *loopv1.StartLoopRequest {
	return &loopv1.StartLoopRequest{Task: "go", Harness: loopSecretSpec(mcpURL),
		Secrets: []*loopv1.LoopSecret{{Name: "my-token", Value: e2eValue, Hosts: hosts}}}
}

// sawBearer reports whether any recorded Authorization header is the bearer
// credential value.
func sawBearer(auths []string, value string) bool {
	for _, a := range auths {
		if a == "Bearer "+value {
			return true
		}
	}
	return false
}

// TestLoopSecretsScopeEndToEnd: over the real edge with the production
// verifier, sending secrets needs secrets:inline; a token that lists it and an
// unrestricted token both start the loop, and the value reaches the MCP server.
func TestLoopSecretsScopeEndToEnd(t *testing.T) {
	s := secretsNetServer(t, secretsOperator)
	srv, auths := mcpAuthFakeServer(t)

	_, err := bearerClient(s.base, "start-tok").StartLoop(context.Background(),
		connect.NewRequest(secretsStart(srv.URL, "127.0.0.1")))
	assertDenied(t, "start-tok with secrets", err, loopauth.ScopeSecretsInline)
	if n := len(auths()); n != 0 {
		t.Fatalf("a refused start reached the MCP server %d times", n)
	}
	if n := len(s.gw.snapshot()); n != 0 {
		t.Fatalf("a refused start reached the model %d times", n)
	}

	for _, tok := range []string{"inline-tok", "full-tok"} {
		before := len(auths())
		client := bearerClient(s.base, tok)
		resp, err := client.StartLoop(context.Background(), connect.NewRequest(secretsStart(srv.URL, "127.0.0.1")))
		if err != nil {
			t.Fatalf("%s StartLoop: %v", tok, err)
		}
		waitForTurnEndByObserve(t, client, resp.Msg.LoopId)
		if !sawBearer(auths()[before:], e2eValue) {
			t.Fatalf("%s: no MCP request carried the loop-scoped secret; saw %v", tok, auths()[before:])
		}
	}
	assertGatewayNeverSaw(t, s.gw, e2eValue)
}

// TestLoopSecretsLimitsEndToEnd: over the real edge, too many secrets or an
// oversized value is InvalidArgument, and the refusal never echoes a value.
func TestLoopSecretsLimitsEndToEnd(t *testing.T) {
	s := secretsNetServer(t, secretsOperator)
	srv, auths := mcpAuthFakeServer(t)
	client := bearerClient(s.base, "inline-tok")

	tooMany := secretsStart(srv.URL, "127.0.0.1")
	tooMany.Secrets = nil
	for i := 0; i < loopsecret.MaxSecrets+1; i++ {
		tooMany.Secrets = append(tooMany.Secrets, &loopv1.LoopSecret{
			Name: fmt.Sprintf("tok-%d", i), Value: e2eValue, Hosts: []string{"127.0.0.1"}})
	}
	oversized := secretsStart(srv.URL, "127.0.0.1")
	oversized.Secrets[0].Value = strings.Repeat("x", loopsecret.MaxValueBytes+1-len(e2eValue)) + e2eValue

	for name, req := range map[string]*loopv1.StartLoopRequest{"33 secrets": tooMany, "16385-byte value": oversized} {
		_, err := client.StartLoop(context.Background(), connect.NewRequest(req))
		if got := connect.CodeOf(err); got != connect.CodeInvalidArgument {
			t.Fatalf("%s: code = %v, want InvalidArgument (err=%v)", name, got, err)
		}
		if strings.Contains(err.Error(), e2eValue) {
			t.Fatalf("%s: the refusal echoes the secret value", name)
		}
	}
	if n := len(auths()); n != 0 {
		t.Fatalf("a refused start reached the MCP server %d times", n)
	}
	if n := len(s.gw.snapshot()); n != 0 {
		t.Fatalf("a refused start reached the model %d times", n)
	}
}

// TestLoopSecretValueIsNeverRecorded is the acceptance grep: after a loop that
// used a loop-scoped secret and a start refused for that secret's host rule,
// no file the server wrote (durable store, base dir, HOME), no log line, no
// error text or detail, and no model request holds the value, while the store
// did record the loop (so the grep is not vacuous).
func TestLoopSecretValueIsNeverRecorded(t *testing.T) {
	s := secretsNetServer(t, secretsOperator)
	srv, auths := mcpAuthFakeServer(t)
	client := bearerClient(s.base, "inline-tok")

	resp, err := client.StartLoop(context.Background(), connect.NewRequest(secretsStart(srv.URL, "127.0.0.1")))
	if err != nil {
		t.Fatalf("StartLoop: %v", err)
	}
	waitForTurnEndByObserve(t, client, resp.Msg.LoopId)
	if !sawBearer(auths(), e2eValue) {
		t.Fatalf("no MCP request carried the loop-scoped secret; saw %v", auths())
	}

	_, err = client.StartLoop(context.Background(), connect.NewRequest(secretsStart(srv.URL, "other.test")))
	if got := connect.CodeOf(err); got != connect.CodeInvalidArgument {
		t.Fatalf("host mismatch: code = %v, want InvalidArgument (err=%v)", got, err)
	}
	if strings.Contains(err.Error(), e2eValue) {
		t.Fatal("the host-rule refusal echoes the secret value")
	}
	var cerr *connect.Error
	if !errors.As(err, &cerr) {
		t.Fatalf("err = %T, want *connect.Error", err)
	}
	if len(cerr.Details()) == 0 {
		t.Fatal("the host-rule refusal carries no detail; the detail check would be vacuous")
	}
	for i, d := range cerr.Details() {
		if bytes.Contains(d.Bytes(), []byte(e2eValue)) {
			t.Fatalf("error detail %d (raw) holds the secret value", i)
		}
		msg, verr := d.Value()
		if verr != nil {
			t.Fatalf("error detail %d: %v", i, verr)
		}
		b, merr := protojson.Marshal(msg)
		if merr != nil {
			t.Fatalf("error detail %d: %v", i, merr)
		}
		if strings.Contains(string(b), e2eValue) {
			t.Fatalf("error detail %d holds the secret value: %s", i, b)
		}
		if !strings.Contains(string(b), "my-token") {
			t.Fatalf("error detail %d does not name the ref; the detail is not the host-rule violation: %s", i, b)
		}
	}

	// Stop the server so every buffered write has landed before the grep.
	s.stop()

	recorded := false
	for _, root := range []string{s.dir, s.home} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.Type().IsRegular() || path == s.log {
				return nil
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if strings.HasSuffix(path, ".gz") {
				zr, err := gzip.NewReader(bytes.NewReader(b))
				if err != nil {
					return fmt.Errorf("%s: %w", path, err)
				}
				if b, err = io.ReadAll(zr); err != nil {
					return fmt.Errorf("%s: %w", path, err)
				}
			}
			if bytes.Contains(b, []byte(e2eValue)) {
				t.Errorf("%s holds the secret value", path)
			}
			if strings.HasPrefix(path, filepath.Join(s.dir, "store")) && bytes.Contains(b, []byte("my-token")) {
				recorded = true
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	if !recorded {
		t.Fatal("no durable-store file names the secret_ref my-token; the store grep is vacuous")
	}
	if strings.Contains(s.logs(), e2eValue) {
		t.Fatal("the server log holds the secret value")
	}
	if len(s.gw.snapshot()) == 0 {
		t.Fatal("the gateway received no request; the model grep is vacuous")
	}
	assertGatewayNeverSaw(t, s.gw, e2eValue)
}

// TestLoopScopedSecretLoopCannotBeResumed: a loop whose spec depends on
// loop-scoped secrets cannot be resumed, because resume re-resolves the
// recorded spec without them. On the production loop-server deps with a
// durable store, an interactive loop started with a loop-scoped secret parks
// and is reaped (no longer live); a replica over the same store then fails to
// Resume it with the unresolved secret_ref at its own field: nothing dials the
// MCP server, and no error text holds the value.
func TestLoopScopedSecretLoopCannotBeResumed(t *testing.T) {
	const value = "cr-RESUME-SENTINEL"
	gw := &recordingGateway{script: []string{`{"choices":[{"message":{"role":"assistant","content":"done"}}]}`}}
	deps, _ := harnessServerWith(t, harnessOperator, gw)
	store := fsstore.NewDurableFSStore(t.TempDir())
	deps.DurableStore, deps.Registry = store, store
	deps.IdleTTL = 150 * time.Millisecond
	rt := runtime.New(deps)
	srv, auths := mcpAuthFakeServer(t)

	ctx := context.Background()
	h, err := rt.StartLoop(ctx, runtime.LoopConfig{
		Task: "go", Interactive: true, Harness: loopSecretSpec(srv.URL),
		Secrets: []loopsecret.Secret{{Name: "my-token", Value: value, Hosts: []string{"127.0.0.1"}}},
	})
	if err != nil {
		t.Fatalf("StartLoop: %v", err)
	}
	if !sawBearer(auths(), value) {
		t.Fatalf("the start did not send the loop-scoped secret; saw %v", auths())
	}
	// Force a non-live state: with nothing observing and no Send, the idle
	// reaper ends the parked loop and the runtime no longer holds it.
	done := make(chan struct{})
	go func() { _, _ = h.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the interactive loop was not reaped")
	}

	// A replica over the same store holds no live copy of the loop, so its
	// Resume rehydrates from the durable record.
	replica := runtime.New(deps)
	before := len(auths())
	_, err = replica.Resume(ctx, "", h.ID())
	var ve *harness.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("Resume err = %v, want a ValidationError", err)
	}
	if len(ve.Violations) != 1 {
		t.Fatalf("violations = %+v, want exactly one", ve.Violations)
	}
	if v := ve.Violations[0]; v.FieldPath != "mcp_servers[0].auth.client_secret_ref" || v.RuleID != harness.RuleSecretRef {
		t.Fatalf("violation = %s %s, want mcp_servers[0].auth.client_secret_ref %s", v.FieldPath, v.RuleID, harness.RuleSecretRef)
	}
	if strings.Contains(err.Error(), value) {
		t.Fatal("the resume refusal echoes the secret value")
	}
	if n := len(auths()) - before; n != 0 {
		t.Fatalf("the MCP server got %d requests for a refused resume", n)
	}
}
