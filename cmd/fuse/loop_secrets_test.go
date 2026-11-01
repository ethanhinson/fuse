package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ethanhinson/fuse/harness"
	harnessv1 "github.com/ethanhinson/fuse/harness/v1"
	"github.com/ethanhinson/fuse/internal/loopsecret"
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
