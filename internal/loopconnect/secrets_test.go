package loopconnect

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"connectrpc.com/connect"

	"github.com/ethanhinson/fuse/internal/loopauth"
	"github.com/ethanhinson/fuse/internal/loopsecret"
	loopv1 "github.com/ethanhinson/fuse/internal/loopwire/v1"
	"github.com/ethanhinson/fuse/internal/tenantlimit"
)

const edgeSentinel = "edge-SENTINEL"

func edgeSecrets() []*loopv1.LoopSecret {
	return []*loopv1.LoopSecret{{Name: "my-token", Value: edgeSentinel, Hosts: []string{"mcp.acme.test"}}}
}

// manySecrets is n well-formed entries, each carrying the sentinel in its value.
func manySecrets(n int) []*loopv1.LoopSecret {
	ss := make([]*loopv1.LoopSecret, n)
	for i := range ss {
		ss[i] = &loopv1.LoopSecret{Name: fmt.Sprintf("tok-%d", i), Value: edgeSentinel, Hosts: []string{"mcp.acme.test"}}
	}
	return ss
}

// oversizedSecret is one entry whose value is one byte over the limit and ends in
// the sentinel, so an error echoing any of the value would show it.
func oversizedSecret() []*loopv1.LoopSecret {
	v := strings.Repeat("x", loopsecret.MaxValueBytes+1-len(edgeSentinel)) + edgeSentinel
	return []*loopv1.LoopSecret{{Name: "my-token", Value: v, Hosts: []string{"mcp.acme.test"}}}
}

func wantInvalidArgument(t *testing.T, what string, err error) {
	t.Helper()
	if got := connect.CodeOf(err); got != connect.CodeInvalidArgument {
		t.Fatalf("%s: code = %v, want InvalidArgument (err=%v)", what, got, err)
	}
}

// wantNoSentinel asserts no rendering of a refusal (its error text, its connect
// message, its metadata) carries the secret value.
func wantNoSentinel(t *testing.T, what string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: want a refusal, got nil", what)
	}
	if strings.Contains(err.Error(), edgeSentinel) {
		t.Fatalf("%s: error echoes the secret value: %q", what, err)
	}
	var cerr *connect.Error
	if errors.As(err, &cerr) {
		if strings.Contains(cerr.Message(), edgeSentinel) {
			t.Fatalf("%s: connect message echoes the secret value", what)
		}
		for k, vs := range cerr.Meta() {
			for _, v := range vs {
				if strings.Contains(v, edgeSentinel) {
					t.Fatalf("%s: error metadata %s echoes the secret value", what, k)
				}
			}
		}
	}
}

func TestSecretsNeedSecretsInline(t *testing.T) {
	f := newScopeFixture(t)

	wantDenied(t, "loop:start + secrets", f.startWithSecrets("start", edgeSecrets()), loopauth.ScopeSecretsInline)
	if f.rt.startCalls != 0 {
		t.Fatalf("seam StartLoop called %d times for a refused request", f.rt.startCalls)
	}
	// The scope is checked before the limits: an unscoped oversized request is
	// refused for the scope.
	wantDenied(t, "loop:start + 33 secrets", f.startWithSecrets("start", manySecrets(33)), loopauth.ScopeSecretsInline)
	// The start scope is checked first.
	wantDenied(t, "secrets:inline only + secrets", f.startWithSecrets("inline-only", edgeSecrets()), loopauth.ScopeLoopStart)
	if f.rt.startCalls != 0 {
		t.Fatalf("seam StartLoop called %d times for refused requests", f.rt.startCalls)
	}

	wantOK(t, "loop:start + secrets:inline + secrets", f.startWithSecrets("start-inline", edgeSecrets()))
	wantOK(t, "unrestricted + secrets", f.startWithSecrets("none", edgeSecrets()))
	// No secrets, no new requirement.
	wantOK(t, "loop:start without secrets", f.start("start", ""))
	if f.rt.startCalls != 3 {
		t.Fatalf("seam StartLoop calls = %d, want 3", f.rt.startCalls)
	}
}

func TestSecretsReachTheRuntime(t *testing.T) {
	f := newScopeFixture(t)
	wantOK(t, "start-inline", f.startWithSecrets("start-inline", edgeSecrets()))
	want := []loopsecret.Secret{{Name: "my-token", Value: edgeSentinel, Hosts: []string{"mcp.acme.test"}}}
	if got := f.rt.sawStartCfg.Secrets; !reflect.DeepEqual(got, want) {
		t.Fatalf("LoopConfig.Secrets = %v, want %v", got, want)
	}

	// A start without secrets hands the runtime none.
	wantOK(t, "start", f.start("start", ""))
	if got := f.rt.sawStartCfg.Secrets; got != nil {
		t.Fatalf("LoopConfig.Secrets = %v, want nil without secrets", got)
	}
}

func TestLoopSecretsConversion(t *testing.T) {
	if got := loopSecrets(nil); got != nil {
		t.Fatalf("loopSecrets(nil) = %v, want nil", got)
	}
	if got := loopSecrets([]*loopv1.LoopSecret{}); got != nil {
		t.Fatalf("loopSecrets(empty) = %v, want nil", got)
	}
	in := []*loopv1.LoopSecret{{Name: "a", Value: "v", Hosts: []string{"h1", "h2"}}, nil}
	got := loopSecrets(in)
	want := []loopsecret.Secret{{Name: "a", Value: "v", Hosts: []string{"h1", "h2"}}, {}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("loopSecrets = %v, want %v", got, want)
	}
	in[0].Hosts[0] = "mutated"
	if got[0].Hosts[0] != "h1" {
		t.Fatalf("loopSecrets shares the wire Hosts slice: %v", got[0].Hosts)
	}
}

func TestSecretsLimits(t *testing.T) {
	f := newScopeFixture(t)

	wantInvalidArgument(t, "33 entries", f.startWithSecrets("start-inline", manySecrets(33)))
	err := f.startWithSecrets("start-inline", oversizedSecret())
	wantInvalidArgument(t, "oversized value", err)
	if strings.Contains(err.Error(), edgeSentinel) {
		t.Fatalf("oversized refusal echoes the value: %q", err)
	}
	if f.rt.startCalls != 0 {
		t.Fatalf("seam StartLoop called %d times for refused requests", f.rt.startCalls)
	}

	wantOK(t, "32 entries", f.startWithSecrets("start-inline", manySecrets(32)))
	atLimit := []*loopv1.LoopSecret{{Name: "my-token", Value: strings.Repeat("x", loopsecret.MaxValueBytes), Hosts: []string{"mcp.acme.test"}}}
	wantOK(t, "16384-byte value", f.startWithSecrets("start-inline", atLimit))
	if f.rt.startCalls != 2 {
		t.Fatalf("seam StartLoop calls = %d, want 2", f.rt.startCalls)
	}
}

// TestSecretsRefusedBeforeTenantLimits pins that a scope or limit refusal spends
// no start-rate token: with a budget of one start, a valid start still admits
// after both refusals.
func TestSecretsRefusedBeforeTenantLimits(t *testing.T) {
	clk := newLimitClock()
	reg := newFakeRegistry()
	rt := newRegisteringRuntime(reg, clk)
	lim := newTestLimiter(t, tenantlimit.Config{Default: tenantlimit.Limits{StartsPerMinute: 1}}, reg, clk)
	v := tokenVerifier{tokens: map[string]loopauth.Principal{
		"start":        {Tenant: "a", Subject: "alice", Scopes: loopauth.NewScopes(loopauth.ScopeLoopStart)},
		"start-inline": {Tenant: "a", Subject: "alice", Scopes: loopauth.NewScopes(loopauth.ScopeLoopStart, loopauth.ScopeSecretsInline)},
	}}
	client := newLimitServer(t, rt, reg, lim, v)
	start := func(token string, ss []*loopv1.LoopSecret) error {
		_, err := client.StartLoop(context.Background(), bearer(connect.NewRequest(&loopv1.StartLoopRequest{
			Task: "t", Secrets: ss,
		}), token))
		return err
	}

	wantDenied(t, "loop:start + secrets", start("start", edgeSecrets()), loopauth.ScopeSecretsInline)
	wantInvalidArgument(t, "oversized value", start("start-inline", oversizedSecret()))
	if rt.startCount() != 0 {
		t.Fatalf("seam StartLoop calls = %d, want 0", rt.startCount())
	}
	wantOK(t, "valid start after refusals", start("start-inline", edgeSecrets()))
	// The budget is now spent, proving the limiter is live in this fixture.
	refusal(t, start("start-inline", edgeSecrets()), "starts_per_minute")
}

func TestSecretsNoPrincipalPath(t *testing.T) {
	rt := &fakeRuntime{startID: "loop-1"}
	_, err := newTestServer(t, rt).StartLoop(context.Background(), connect.NewRequest(&loopv1.StartLoopRequest{
		Task: "t", Secrets: edgeSecrets(),
	}))
	if err != nil {
		t.Fatalf("StartLoop with secrets and no interceptor: %v", err)
	}
	want := []loopsecret.Secret{{Name: "my-token", Value: edgeSentinel, Hosts: []string{"mcp.acme.test"}}}
	if !reflect.DeepEqual(rt.sawStartCfg.Secrets, want) {
		t.Fatalf("LoopConfig.Secrets = %v, want %v", rt.sawStartCfg.Secrets, want)
	}
	// The limits still apply without a principal.
	_, err = newTestServer(t, rt).StartLoop(context.Background(), connect.NewRequest(&loopv1.StartLoopRequest{
		Task: "t", Secrets: oversizedSecret(),
	}))
	wantInvalidArgument(t, "oversized value without a principal", err)
}

func TestRefusalNeverEchoesASecretValue(t *testing.T) {
	f := newScopeFixture(t)
	refusals := map[string]error{
		"loop:start + secrets":    f.startWithSecrets("start", edgeSecrets()),
		"secrets:inline only":     f.startWithSecrets("inline-only", edgeSecrets()),
		"observe-only + secrets":  f.startWithSecrets("obs", edgeSecrets()),
		"33 entries":              f.startWithSecrets("start-inline", manySecrets(33)),
		"oversized value":         f.startWithSecrets("start-inline", oversizedSecret()),
		"loop:start + oversized":  f.startWithSecrets("start", oversizedSecret()),
		"unknown token + secrets": f.startWithSecrets("no-such-token", edgeSecrets()),
		"start-inline spoofed tenant": func() error {
			_, err := f.client.StartLoop(context.Background(), bearer(connect.NewRequest(&loopv1.StartLoopRequest{
				Task: "t", Tenant: "evilcorp", Secrets: edgeSecrets(),
			}), "start-inline"))
			return err
		}(),
	}
	for what, err := range refusals {
		wantNoSentinel(t, what, err)
	}
	if f.rt.startCalls != 0 {
		t.Fatalf("seam StartLoop called %d times for refused requests", f.rt.startCalls)
	}
}
