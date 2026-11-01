package loopconnect

import (
	"context"
	"errors"
	"math"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

	harnessv1 "github.com/ethanhinson/fuse/harness/v1"
	"github.com/ethanhinson/fuse/internal/capabilities"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/loopauth"
	loopv1 "github.com/ethanhinson/fuse/internal/loopwire/v1"
	"github.com/ethanhinson/fuse/internal/loopwire/v1/loopv1connect"
)

// With no capability source wired, the RPC exists and answers Unimplemented,
// never an empty but plausible answer.
func TestGetCapabilitiesWithoutASourceIsUnimplemented(t *testing.T) {
	_, err := newTestServer(t, &fakeRuntime{}).GetCapabilities(context.Background(), connect.NewRequest(&loopv1.GetCapabilitiesRequest{}))
	if got := connect.CodeOf(err); got != connect.CodeUnimplemented {
		t.Fatalf("code = %v, want Unimplemented", got)
	}
}

type fakeCaps struct {
	saw    event.TenantID
	calls  int
	answer capabilities.Capabilities
	err    error
}

func (f *fakeCaps) Capabilities(_ context.Context, tenant event.TenantID) (capabilities.Capabilities, error) {
	f.calls++
	f.saw = tenant
	return f.answer, f.err
}

func capsServer(t *testing.T, src CapabilitySource, v loopauth.Verifier) loopv1connect.LoopServiceClient {
	t.Helper()
	var opts []connect.HandlerOption
	if v != nil {
		opts = append(opts, connect.WithInterceptors(NewAuthInterceptor(v)))
	}
	_, h := loopv1connect.NewLoopServiceHandler(NewHandler(&fakeRuntime{}).WithCapabilities(src), opts...)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return loopv1connect.NewLoopServiceClient(srv.Client(), srv.URL)
}

func TestGetCapabilitiesTenantSpoofIsPermissionDenied(t *testing.T) {
	src := &fakeCaps{}
	_, err := capsServer(t, src, testVerifier()).GetCapabilities(context.Background(),
		bearer(connect.NewRequest(&loopv1.GetCapabilitiesRequest{Tenant: "other"}), testToken))
	if connect.CodeOf(err) != connect.CodePermissionDenied || src.calls != 0 {
		t.Fatalf("code = %v, source calls = %d; want PermissionDenied and no call", connect.CodeOf(err), src.calls)
	}
}

func fullAnswer() capabilities.Capabilities {
	return capabilities.Capabilities{
		Models: []capabilities.Model{{
			Alias:                  "fast",
			GatewayID:              "openai/gpt-x",
			MaxOutputTokens:        4096,
			ContextWindow:          128000,
			ContextWindowIsDefault: true,
			Persona:                "terse",
			IsDefault:              true,
			Label:                  "Fast",
			Description:            "quick answers",
			Tags:                   []string{"tools", "vision"},
		}},
		Skills: []capabilities.Skill{{Name: "review", Description: "reviews code"}},
		Ceilings: []capabilities.Ceiling{
			{FieldPath: "limits.max_cost_usd", OperatorSet: true, Value: 2.5, Direction: capabilities.DirectionMax},
			{FieldPath: "limits.max_turns", OperatorSet: false, Value: 7, Direction: capabilities.DirectionMax},
		},
		SecretRefs: []string{"GITHUB_TOKEN"},
		SpecMCPAuthKinds: []harnessv1.McpAuthType{
			harnessv1.McpAuthType_MCP_AUTH_TYPE_BEARER,
			harnessv1.McpAuthType_MCP_AUTH_TYPE_STATIC,
		},
		SpecPolicy: capabilities.SpecPolicy{MCPHosts: []string{"*.example.com"}, DenyPrivateNetworks: true, ModelsRegistryOnly: true},
		Server:     capabilities.Server{Version: "v1.2.3", DigestAlgorithm: "sha256"},
	}
}

func TestGetCapabilitiesMapsEveryField(t *testing.T) {
	src := &fakeCaps{answer: fullAnswer()}
	resp, err := capsServer(t, src, testVerifier()).GetCapabilities(context.Background(),
		bearer(connect.NewRequest(&loopv1.GetCapabilitiesRequest{}), testToken))
	if err != nil {
		t.Fatalf("GetCapabilities: %v", err)
	}
	want := &loopv1.GetCapabilitiesResponse{
		Models: []*loopv1.ModelInfo{{
			Alias:                  "fast",
			GatewayId:              "openai/gpt-x",
			MaxOutputTokens:        4096,
			ContextWindow:          128000,
			ContextWindowIsDefault: true,
			Persona:                "terse",
			IsDefault:              true,
			Label:                  "Fast",
			Description:            "quick answers",
			Tags:                   []string{"tools", "vision"},
		}},
		Skills: []*loopv1.SkillInfo{{Name: "review", Description: "reviews code"}},
		Ceilings: []*loopv1.Ceiling{
			{FieldPath: "limits.max_cost_usd", OperatorSet: true, Value: structpb.NewNumberValue(2.5), Direction: loopv1.CeilingDirection_CEILING_DIRECTION_MAX},
			{FieldPath: "limits.max_turns", OperatorSet: false, Direction: loopv1.CeilingDirection_CEILING_DIRECTION_MAX},
		},
		SecretRefs: []string{"GITHUB_TOKEN"},
		SpecMcpAuthKinds: []harnessv1.McpAuthType{
			harnessv1.McpAuthType_MCP_AUTH_TYPE_BEARER,
			harnessv1.McpAuthType_MCP_AUTH_TYPE_STATIC,
		},
		SpecPolicy: &loopv1.SpecPolicy{McpHosts: []string{"*.example.com"}, DenyPrivateNetworks: true, ModelsRegistryOnly: true},
		Server:     &loopv1.ServerInfo{Version: "v1.2.3", DigestAlgorithm: "sha256"},
	}
	if !proto.Equal(resp.Msg, want) {
		t.Fatalf("response mismatch:\n got %v\nwant %v", resp.Msg, want)
	}
	if resp.Msg.Ceilings[1].Value != nil {
		t.Fatalf("unset ceiling carries a value: %v", resp.Msg.Ceilings[1].Value)
	}
}

func TestGetCapabilitiesFiltersByThePrincipalsTenant(t *testing.T) {
	src := &fakeCaps{}
	client := capsServer(t, src, testVerifier())
	if _, err := client.GetCapabilities(context.Background(),
		bearer(connect.NewRequest(&loopv1.GetCapabilitiesRequest{}), testToken)); err != nil {
		t.Fatalf("no wire tenant: %v", err)
	}
	if src.saw != "acme" {
		t.Fatalf("source saw tenant %q, want the principal's acme", src.saw)
	}
	src.saw = ""
	if _, err := client.GetCapabilities(context.Background(),
		bearer(connect.NewRequest(&loopv1.GetCapabilitiesRequest{Tenant: "acme"}), testToken)); err != nil {
		t.Fatalf("matching wire tenant: %v", err)
	}
	if src.saw != "acme" || src.calls != 2 {
		t.Fatalf("source saw %q after %d calls, want acme after 2", src.saw, src.calls)
	}
}

func TestGetCapabilitiesUnauthenticated(t *testing.T) {
	src := &fakeCaps{answer: fullAnswer()}
	client := capsServer(t, src, testVerifier())
	for name, req := range map[string]*connect.Request[loopv1.GetCapabilitiesRequest]{
		"missing token": connect.NewRequest(&loopv1.GetCapabilitiesRequest{}),
		"invalid token": bearer(connect.NewRequest(&loopv1.GetCapabilitiesRequest{}), "not-the-token"),
	} {
		resp, err := client.GetCapabilities(context.Background(), req)
		if got := connect.CodeOf(err); got != connect.CodeUnauthenticated || resp != nil {
			t.Fatalf("%s: code = %v, resp = %v; want Unauthenticated and no answer", name, got, resp)
		}
	}
	if src.calls != 0 {
		t.Fatalf("source called %d times for unauthenticated requests", src.calls)
	}
}

func TestGetCapabilitiesSourceErrorIsInternal(t *testing.T) {
	src := &fakeCaps{answer: fullAnswer(), err: errors.New("registry unavailable")}
	resp, err := capsServer(t, src, testVerifier()).GetCapabilities(context.Background(),
		bearer(connect.NewRequest(&loopv1.GetCapabilitiesRequest{}), testToken))
	if got := connect.CodeOf(err); got != connect.CodeInternal || resp != nil {
		t.Fatalf("code = %v, resp = %v; want Internal and no answer", got, resp)
	}
}

// The mapper clamps counts into uint32, maps an unknown direction to
// UNSPECIFIED, and never aliases the caller's slices.
func TestToProtoCapabilitiesClampsAndCopies(t *testing.T) {
	c := capabilities.Capabilities{
		Models: []capabilities.Model{
			{Alias: "neg", MaxOutputTokens: -1, ContextWindow: -5, Tags: []string{"tools"}},
			{Alias: "big", MaxOutputTokens: math.MaxUint32 + 1, ContextWindow: math.MaxInt},
		},
		Ceilings: []capabilities.Ceiling{
			{FieldPath: "a", OperatorSet: true, Value: 1, Direction: capabilities.DirectionMin},
			{FieldPath: "b", Direction: capabilities.Direction(99)},
		},
		SecretRefs:       []string{"S"},
		SpecMCPAuthKinds: []harnessv1.McpAuthType{harnessv1.McpAuthType_MCP_AUTH_TYPE_BEARER},
	}
	got := toProtoCapabilities(c)
	if m := got.Models[0]; m.MaxOutputTokens != 0 || m.ContextWindow != 0 {
		t.Fatalf("negative counts = %d/%d, want 0/0", m.MaxOutputTokens, m.ContextWindow)
	}
	if m := got.Models[1]; m.MaxOutputTokens != math.MaxUint32 || m.ContextWindow != math.MaxUint32 {
		t.Fatalf("oversized counts = %d/%d, want max uint32", m.MaxOutputTokens, m.ContextWindow)
	}
	if d := got.Ceilings[0].Direction; d != loopv1.CeilingDirection_CEILING_DIRECTION_MIN {
		t.Fatalf("DirectionMin mapped to %v", d)
	}
	if d := got.Ceilings[1].Direction; d != loopv1.CeilingDirection_CEILING_DIRECTION_UNSPECIFIED {
		t.Fatalf("unknown direction mapped to %v", d)
	}
	c.Models[0].Tags[0] = "mutated"
	c.SecretRefs[0] = "mutated"
	c.SpecMCPAuthKinds[0] = harnessv1.McpAuthType_MCP_AUTH_TYPE_OAUTH2
	if got.Models[0].Tags[0] != "tools" || got.SecretRefs[0] != "S" ||
		got.SpecMcpAuthKinds[0] != harnessv1.McpAuthType_MCP_AUTH_TYPE_BEARER {
		t.Fatalf("mapped answer aliases the caller's slices: %v", got)
	}
}
