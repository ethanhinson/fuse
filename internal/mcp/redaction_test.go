package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/loopauth"
	"github.com/ethanhinson/fuse/internal/toolidentity"
)

// authCapturingConn is a minimal mcpConn double that records the per-call auth threaded on
// ctx (proving the token DID leave via the transport) and returns a canned
// tools/call result — WITHOUT any credential in it. It lets the redaction test
// run deterministically with no live HTTP/SSE round-trip.
type authCapturingConn struct {
	gotAuthHeader string
	gotResource   string
}

func (f *authCapturingConn) call(ctx context.Context, _ string, _ any) (json.RawMessage, error) {
	// Mirror what the real transport does: pull the per-call credential/target off
	// ctx via the same applyCallAuth path the HTTP clients use.
	applyCallAuth(ctx, func(k, v string) {
		switch k {
		case "Authorization":
			f.gotAuthHeader = v
		case mcpResourceHeader:
			f.gotResource = v
		}
	}, func() {})
	// The downstream response never contains a credential.
	return json.RawMessage(`{"content":[{"type":"text","text":"downstream ok"}]}`), nil
}

func (f *authCapturingConn) notify(context.Context, string, any) error { return nil }
func (f *authCapturingConn) stop()                                     {}

// TestRedaction_TokenNeverInToolResult is the D6 guard: a real per-call minted
// credential reaches the transport (proving the identity path ran), but the
// minted TOKEN never appears in the tools.Result the loop turns into a
// tool.result event, nor in the model-supplied args.
func TestRedaction_TokenNeverInToolResult(t *testing.T) {
	const tenant = event.TenantID("acme")
	src := stsSource(t, tenant, []byte("redaction-signing-key"))
	target := toolidentity.Target{Name: "cap", Audience: "https://cap.example.test", Tier: toolidentity.TierOAuth}

	conn := &authCapturingConn{}
	tool := &MCPTool{client: conn, serverName: "cap", toolName: "do", source: src, target: target}

	ctx := toolidentity.WithPrincipal(context.Background(), loopauth.Principal{Tenant: tenant, Subject: "alice"})
	args := `{"q":"hello"}`
	res := tool.Execute(ctx, args)
	if res.IsError {
		t.Fatalf("unexpected tool error: %s", res.Output)
	}

	// The credential DID leave fuse on the wire (identity path ran).
	if !strings.HasPrefix(conn.gotAuthHeader, "Bearer ") {
		t.Fatalf("no per-call bearer reached the transport: %q", conn.gotAuthHeader)
	}
	rawToken := strings.TrimPrefix(conn.gotAuthHeader, "Bearer ")
	if rawToken == "" {
		t.Fatal("empty minted token")
	}
	// Audience binding rode alongside it.
	if conn.gotResource != target.Audience {
		t.Fatalf("resource header = %q, want %q", conn.gotResource, target.Audience)
	}

	// The token must NOT appear in the tool Result the loop persists as a
	// tool.result event, nor in the model-supplied args echoed as a tool.call.
	if strings.Contains(res.Output, rawToken) {
		t.Fatalf("D6 VIOLATION: minted token leaked into the tool result: %q", res.Output)
	}
	if strings.Contains(args, rawToken) {
		t.Fatal("test invariant broken: token cannot be in the model-supplied args")
	}
}

// TestRedaction_ResolveErrorCarriesNoToken: a resolution that DOES mint (a valid
// target) but whose result we inspect must never place the token in an error
// path. Here we assert the belt-and-suspenders Credential formatting guard: a
// Credential surfaced through fmt (a stray %v in a log or wrapped error) shows
// <redacted>, never the token.
func TestRedaction_CredentialFormatsRedacted(t *testing.T) {
	src := stsSource(t, "acme", []byte("k"))
	cred, err := src.CredentialFor(context.Background(),
		loopauth.Principal{Tenant: "acme", Subject: "alice"},
		toolidentity.Target{Name: "cap", Audience: "https://cap", Tier: toolidentity.TierOAuth})
	if err != nil {
		t.Fatalf("CredentialFor: %v", err)
	}
	h := cred.Header()
	rawToken := h[strings.IndexByte(h, ' ')+1:]
	for _, s := range []string{
		fmt.Sprintf("%v", cred),
		fmt.Sprintf("%#v", cred),
		fmt.Sprintf("%s", cred),
	} {
		if strings.Contains(s, rawToken) {
			t.Fatalf("credential format leaked the token: %q", s)
		}
	}
}
