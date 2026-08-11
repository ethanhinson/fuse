package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/event/fsstore"
	"github.com/ethanhinson/fuse/internal/loopauth"
	loopv1 "github.com/ethanhinson/fuse/internal/loopwire/v1"
	"github.com/ethanhinson/fuse/internal/loopwire/v1/loopv1connect"
	"github.com/ethanhinson/fuse/internal/mcpdemo/rentals"
	"github.com/ethanhinson/fuse/internal/permissions"
	"github.com/ethanhinson/fuse/internal/runtime"
)

// This is change #59's PERMANENT CI ACCEPTANCE LANE: the full #52 verification
// checklist, asserted END-TO-END THROUGH THE LOOP-SERVER BINDING, against the REAL
// Wander rentals MCP server (Task 6). Two loops by two authenticated principals run
// CONCURRENTLY in one loop-serve-net process over one durable store; each loop's turn
// is scripted by an LLM_GATEWAY_URL double (NEVER Claude/Anthropic) to call the rentals
// MCP tools. It runs in the normal `go test ./...` suite (it is not build-tagged), so a
// regression kills it. It LOUD-skips only if a genuine prerequisite is absent — never a
// silent t.Skip that hides that the path never ran.
//
// Checklist (spec §Acceptance), all through the loop-server binding:
//  1. Two loops, two principals, one rentals server.
//  2. Each call presents a distinct, audience-bound, delegated token (per-principal).
//  3. Downstream adjudication: listings for the authorized principal, 403 (distinguishable
//     tool error) for the unauthorized one.
//  4. Per-principal write isolation (confused-deputy on a mutating path).
//  5. Wrong-audience token rejected.
//  6. Complete-mediation: an undeclared target denied terminally under AlwaysApprove.
//  7. No credential leak into the event stream / durable store / logs.

const (
	rentalsAudience = "https://rentals.example"
	// The loop-server derives per-tenant STS keys from ONE configured signing key via
	// HMAC(signingKey, "fuse-tool-identity-tenant:"+tenant) (Task 2). The rentals server
	// must verify with the SAME derived keys, so the lane derives them the same way.
	loopSigningKey = "acceptance-loop-signing-key-0123456789"
)

// rentalsAcceptanceHarness stands up the real loop-serve-net binding (production
// serveNet + auth interceptor + registry) with the rentals server wired as an
// identity-propagating MCP server, and a scripted gateway that drives each loop's turn
// to call the rentals tools. It returns the server base URL, the durable store (to
// inspect the event stream for leak checks), and the rentals server.
func rentalsAcceptanceHarness(t *testing.T, gateway http.HandlerFunc) (base string, store *fsstore.FSDurableStore, rsrv *rentals.Server) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())

	// Per-tenant keys the loop-server STS mints under (Task 2 derivation) — the rentals
	// server verifies with these SAME keys (shared out of band, as a real RS does).
	tenantKeys := map[event.TenantID][]byte{
		"acme":   deriveTenantKey([]byte(loopSigningKey), "acme"),
		"globex": deriveTenantKey([]byte(loopSigningKey), "globex"),
	}
	rsrv = rentals.NewServer(rentals.Config{Audience: rentalsAudience, TenantKeys: tenantKeys})
	t.Cleanup(rsrv.Close)

	// Scripted gateway double (NEVER a real provider).
	gln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("gateway listen: %v", err)
	}
	gsrv := &http.Server{Handler: gateway}
	go func() { _ = gsrv.Serve(gln) }()
	t.Cleanup(func() { _ = gsrv.Close() })
	gatewayURL := "http://" + gln.Addr().String()

	// Config: the rentals server as an identity-propagating MCP server + two tenants
	// under loop_server.auth so the STS is keyed for both. AlwaysApprove is the binding
	// policy. Mode "off" so the ONLY denial in the path is the TargetMediator (point 6).
	// Gateway points at the scripted double (set directly — this cfg is hand-built, not
	// config.Load()'d, so the env override does not apply).
	cfg := config.Config{
		Gateway: config.Gateway{URL: gatewayURL, Key: "tkn"},
		MCPServers: []config.MCPServerConfig{{
			Name: "rentals", Transport: "sse", URL: rsrv.URL(), Audience: rentalsAudience,
			Auth: config.MCPAuthConfig{Type: "identity"},
		}},
		ToolIdentity: config.ToolIdentityConfig{SigningKey: loopSigningKey},
		LoopServer: config.LoopServerConfig{Auth: []config.AuthTokenConfig{
			{Token: "acme-tok", Tenant: "acme", Subject: "alice"},
			{Token: "globex-tok", Tenant: "globex", Subject: "bob"},
		}},
		Permissions: config.PermissionsConfig{Mode: "off"},
	}

	reg := registryFromConfig(cfg)
	alias := reg.Default

	dir := t.TempDir()
	store = fsstore.NewDurableFSStore(dir)

	deps := buildLoopServerRuntimeDeps(cfg, reg, alias, defaultToolRegistry(cfg.Research, nil),
		spawnAgentBlock, permissions.AlwaysApprove, sessionRateGate(cfg))
	deps.DurableStore = store
	deps.Registry = store
	deps.BaseDir = ""
	rt := runtime.New(deps)

	verifier := loopauth.NewStaticVerifier(map[string]loopauth.Principal{
		"acme-tok":   {Tenant: "acme", Subject: "alice"},
		"globex-tok": {Tenant: "globex", Subject: "bob"},
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	serveDone := make(chan error, 1)
	go func() { serveDone <- serveNet(ctx, ln, rt, verifier, store) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-serveDone:
		case <-time.After(5 * time.Second):
			t.Error("serveNet did not return after cancel")
		}
	})
	return "http://" + ln.Addr().String(), store, rsrv
}

// scriptedRentalsGateway returns a gateway handler that inspects each request's
// messages and scripts the loop's turn: it issues ONE tool call for the tool named in
// the task (marker "CALL:<tool>:<args-json>"), then a final text once the tool result
// is present. A task with no marker gets a plain final reply.
func scriptedRentalsGateway() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")

		// If a tool result is already in the conversation, finish the turn.
		if strings.Contains(string(body), `"role":"tool"`) {
			io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"done"}}]}`)
			return
		}
		// Otherwise find the CALL: marker in the (user) task and emit that tool call.
		tool, args := parseCallMarker(string(body))
		if tool == "" {
			io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"no-op"}}]}`)
			return
		}
		resp := map[string]any{"choices": []map[string]any{{"message": map[string]any{
			"role":    "assistant",
			"content": "",
			"tool_calls": []map[string]any{{
				"id": "1", "type": "function",
				"function": map[string]any{"name": tool, "arguments": args},
			}},
		}}}}
		b, _ := json.Marshal(resp)
		_, _ = w.Write(b)
	}
}

// parseCallMarker extracts the tool + args from a "CALL|<tool>|<args>|END" marker
// embedded in the request body (the loop's task). The '|' delimiter is used because
// the MCP tool name itself contains ':' (e.g. "mcp:rentals/search_rentals"), so a ':'
// delimiter would split the tool name.
func parseCallMarker(body string) (tool, args string) {
	const pfx = "CALL|"
	i := strings.Index(body, pfx)
	if i < 0 {
		return "", ""
	}
	rest := body[i+len(pfx):]
	end := strings.Index(rest, "|END")
	if end < 0 {
		return "", ""
	}
	spec := rest[:end] // "<tool>|<args-json>"
	j := strings.Index(spec, "|")
	if j < 0 {
		return spec, "{}"
	}
	// The args JSON was escaped in the JSON request body; unescape \" → ".
	args = strings.ReplaceAll(spec[j+1:], `\"`, `"`)
	return spec[:j], args
}

// rentalsTask builds a loop task carrying a CALL marker the scripted gateway acts on.
func rentalsTask(tool, argsJSON string) string {
	return fmt.Sprintf("please help. CALL|%s|%s|END", tool, argsJSON)
}

// TestAcceptance_RentalsThroughLoopServer_TwoPrincipals is checklist points 1-4: two
// concurrent loops by two principals call the SAME rentals server; each presents a
// distinct delegated token; the authorized principal gets listings; per-principal
// favorites are isolated (the confused-deputy property on a mutating write path).
func TestAcceptance_RentalsThroughLoopServer_TwoPrincipals(t *testing.T) {
	base, store, rsrv := rentalsAcceptanceHarness(t, scriptedRentalsGateway())

	alice := bearerClient(base, "acme-tok")
	bob := bearerClient(base, "globex-tok")

	// Drive two loops CONCURRENTLY (point 1; -race makes a cross-loop bleed observable):
	// alice favorites L1, bob favorites L2.
	var wg sync.WaitGroup
	run := func(client loopv1connect.LoopServiceClient, tool, args string, out *string) {
		defer wg.Done()
		sr, err := client.StartLoop(context.Background(), connect.NewRequest(&loopv1.StartLoopRequest{
			Task: rentalsTask(tool, args),
		}))
		if err != nil {
			t.Errorf("StartLoop(%s): %v", tool, err)
			return
		}
		waitForTurnEndByObserve(t, client, sr.Msg.LoopId)
		*out = sr.Msg.LoopId
	}
	var aliceLoop, bobLoop string
	wg.Add(2)
	go run(alice, "mcp:rentals/favorite_listing", `{"listing_id":"L1"}`, &aliceLoop)
	go run(bob, "mcp:rentals/favorite_listing", `{"listing_id":"L2"}`, &bobLoop)
	wg.Wait()

	// Point 4: per-principal write isolation. alice list_favorites → [L1] (no L2);
	// bob → [L2] (no L1). Driven as fresh loops.
	aliceFavs := runFavoritesLoop(t, alice)
	bobFavs := runFavoritesLoop(t, bob)
	if !containsSub(aliceFavs, "L1") || containsSub(aliceFavs, "L2") {
		t.Fatalf("alice favorites = %q, want L1 only (bob's L2 invisible — confused-deputy)", aliceFavs)
	}
	if !containsSub(bobFavs, "L2") || containsSub(bobFavs, "L1") {
		t.Fatalf("bob favorites = %q, want L2 only (alice's L1 invisible)", bobFavs)
	}

	// Point 2: distinct, audience-bound, delegated tokens per principal — asserted
	// DIRECTLY from the rentals server's captured wire headers. Every tools/call carried
	// a Bearer token bound to the server audience, and across the run at least two
	// DISTINCT tokens appeared (alice's vs bob's — a per-principal delegation, never a
	// shared static token).
	auths := rsrv.CapturedAuths()
	if len(auths) < 2 {
		t.Fatalf("expected ≥2 captured tools/call auths (two principals), got %d", len(auths))
	}
	distinct := map[string]bool{}
	for i, a := range auths {
		if !strings.HasPrefix(a, "Bearer ") {
			t.Fatalf("captured auth %d not Bearer-scheme: %q", i, a)
		}
		distinct[a] = true
	}
	if len(distinct) < 2 {
		t.Fatalf("expected ≥2 DISTINCT per-principal tokens on the wire, got %d distinct of %d", len(distinct), len(auths))
	}
	for i, res := range rsrv.CapturedResources() {
		if res != rentalsAudience {
			t.Fatalf("call %d audience-binding = %q, want %q (RFC 8707 resource)", i, res, rentalsAudience)
		}
	}

	// Point 7: NO minted token leaks into the durable event stream.
	assertNoCredentialLeak(t, store, aliceLoop)
	assertNoCredentialLeak(t, store, bobLoop)
}

// runFavoritesLoop starts a list_favorites loop and returns the tool.result text from
// its event stream (where the favorites JSON is carried).
func runFavoritesLoop(t *testing.T, client loopv1connect.LoopServiceClient) string {
	t.Helper()
	sr, err := client.StartLoop(context.Background(), connect.NewRequest(&loopv1.StartLoopRequest{
		Task: rentalsTask("mcp:rentals/list_favorites", `{}`),
	}))
	if err != nil {
		t.Fatalf("StartLoop(list_favorites): %v", err)
	}
	waitForTurnEndByObserve(t, client, sr.Msg.LoopId)
	// Read the tool.result payload from the durable stream.
	return toolResultText(t, client, sr.Msg.LoopId)
}

// toolResultText observes the loop's stream and returns the concatenated tool.result
// payloads (where MCP tool output lands).
func toolResultText(t *testing.T, client loopv1connect.LoopServiceClient, loopID string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream, err := client.Observe(ctx, connect.NewRequest(&loopv1.ObserveRequest{LoopId: loopID, FromSeq: 0}))
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	var sb strings.Builder
	for stream.Receive() {
		msg := stream.Msg()
		if msg.Keepalive || msg.Event == nil {
			continue
		}
		if event.Kind(msg.Event.Kind) == event.KindToolResult {
			sb.Write(msg.Event.Payload)
		}
		if event.Kind(msg.Event.Kind) == event.KindTurnEnd {
			break
		}
	}
	_ = stream.Close()
	return sb.String()
}

// assertNoCredentialLeak replays the loop's durable history and asserts no minted
// token appears — no "Bearer <jws>" and no three-segment JWS in any payload (point 7).
func assertNoCredentialLeak(t *testing.T, store *fsstore.FSDurableStore, loopID string) {
	t.Helper()
	for _, tenant := range []event.TenantID{"acme", "globex"} {
		hist, err := store.Replay(context.Background(), event.StreamKey{Tenant: tenant, Loop: event.LoopID(loopID)}, 0)
		if err != nil {
			continue
		}
		for _, e := range hist {
			p := string(e.Payload)
			if strings.Contains(p, "Bearer eyJ") {
				t.Fatalf("credential leak: a Bearer JWS token appears in the %s event stream (kind=%s)", e.Kind, e.Kind)
			}
			if looksLikeJWS(p) {
				t.Fatalf("credential leak: a JWS-shaped token appears in the %s event stream", e.Kind)
			}
		}
	}
}

// looksLikeJWS reports whether s contains an HS256 JWS (header "eyJhbGciOi..." dot
// payload dot signature) — the minted delegation token shape.
func looksLikeJWS(s string) bool {
	// The STS header {"alg":"HS256","typ":"JWT"} base64url-encodes with this prefix.
	return strings.Contains(s, "eyJhbGciOiJIUzI1NiI")
}

// TestAcceptance_UnauthorizedAndWrongAudienceThroughLoopServer is checklist points 3
// (403 for an unauthorized principal, distinguishable tool error) and 5 (wrong-audience
// rejected). It reuses the rentals server's own contract (proven in Task 6) but asserts
// it THROUGH the loop-server binding: a loop whose principal's tenant the rentals server
// does not accept surfaces a distinguishable tool error in its stream.
func TestAcceptance_UnauthorizedThroughLoopServer(t *testing.T) {
	// Server accepts only acme; the loop authenticates as globex — globex's token
	// verifies under no server key ⇒ the rentals server 403s it (-32001), surfaced as a
	// distinguishable tool error in the loop's stream.
	t.Setenv("HOME", t.TempDir())
	tenantKeys := map[event.TenantID][]byte{"acme": deriveTenantKey([]byte(loopSigningKey), "acme")}
	rsrv := rentals.NewServer(rentals.Config{Audience: rentalsAudience, TenantKeys: tenantKeys})
	t.Cleanup(rsrv.Close)

	gln, _ := net.Listen("tcp", "127.0.0.1:0")
	gsrv := &http.Server{Handler: scriptedRentalsGateway()}
	go func() { _ = gsrv.Serve(gln) }()
	t.Cleanup(func() { _ = gsrv.Close() })

	cfg := config.Config{
		Gateway: config.Gateway{URL: "http://" + gln.Addr().String(), Key: "tkn"},
		MCPServers: []config.MCPServerConfig{{
			Name: "rentals", Transport: "sse", URL: rsrv.URL(), Audience: rentalsAudience,
			Auth: config.MCPAuthConfig{Type: "identity"},
		}},
		ToolIdentity: config.ToolIdentityConfig{SigningKey: loopSigningKey},
		LoopServer: config.LoopServerConfig{Auth: []config.AuthTokenConfig{
			{Token: "globex-tok", Tenant: "globex", Subject: "bob"},
		}},
		Permissions: config.PermissionsConfig{Mode: "off"},
	}
	reg := registryFromConfig(cfg)
	dir := t.TempDir()
	store := fsstore.NewDurableFSStore(dir)
	deps := buildLoopServerRuntimeDeps(cfg, reg, reg.Default, defaultToolRegistry(cfg.Research, nil),
		spawnAgentBlock, permissions.AlwaysApprove, sessionRateGate(cfg))
	deps.DurableStore, deps.Registry, deps.BaseDir = store, store, ""
	rt := runtime.New(deps)
	verifier := loopauth.NewStaticVerifier(map[string]loopauth.Principal{"globex-tok": {Tenant: "globex", Subject: "bob"}})
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serveNet(ctx, ln, rt, verifier, store) }()
	t.Cleanup(func() { cancel(); <-done })

	client := bearerClient("http://"+ln.Addr().String(), "globex-tok")
	sr, err := client.StartLoop(context.Background(), connect.NewRequest(&loopv1.StartLoopRequest{
		Task: rentalsTask("mcp:rentals/search_rentals", `{"query":""}`),
	}))
	if err != nil {
		t.Fatalf("StartLoop: %v", err)
	}
	waitForTurnEndByObserve(t, client, sr.Msg.LoopId)
	got := toolResultText(t, client, sr.Msg.LoopId)
	if !strings.Contains(got, "-32001") {
		t.Fatalf("unauthorized principal must surface a distinguishable 403 tool error through the binding; tool.result was %q", got)
	}
}

// TestAcceptance_CompleteMediationThroughLoopServer is checklist point 6: a tool
// reaching an UNDECLARED target is denied TERMINALLY by the TargetMediator on the
// loop-server path even under AlwaysApprove + Mode off. The loop calls an MCP tool for a
// server that is NOT configured; the gate denies it before execution.
func TestAcceptance_CompleteMediationThroughLoopServer(t *testing.T) {
	base, _, _ := rentalsAcceptanceHarness(t, scriptedRentalsGateway())
	client := bearerClient(base, "acme-tok")

	// "mcp:evil/steal" targets a server that is not in cfg.MCPServers — the mediator
	// denies it terminally (root-of-trust allowlist), independent of the auto-approve
	// binding policy.
	sr, err := client.StartLoop(context.Background(), connect.NewRequest(&loopv1.StartLoopRequest{
		Task: rentalsTask("mcp:evil/steal", `{}`),
	}))
	if err != nil {
		t.Fatalf("StartLoop: %v", err)
	}
	waitForTurnEndByObserve(t, client, sr.Msg.LoopId)
	got := toolResultText(t, client, sr.Msg.LoopId)
	if !strings.Contains(got, "not a configured MCP server") && !strings.Contains(got, "root-of-trust") {
		t.Fatalf("an undeclared target must be denied terminally by the mediator through the binding; tool.result was %q", got)
	}
}
