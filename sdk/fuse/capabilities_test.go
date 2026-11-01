package fuse

import (
	"context"
	"errors"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	harnessv1 "github.com/ethanhinson/fuse/harness/v1"
	"github.com/ethanhinson/fuse/internal/capabilities"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/loopconnect"
	loopv1 "github.com/ethanhinson/fuse/internal/loopwire/v1"
	"github.com/ethanhinson/fuse/internal/loopwire/v1/loopv1connect"
)

// sdkCaps is a capability source that records the tenant it was asked for and
// answers with a fixed, fully populated capabilities.Capabilities.
type sdkCaps struct {
	mu  sync.Mutex
	saw event.TenantID
	ans capabilities.Capabilities
}

func (s *sdkCaps) Capabilities(_ context.Context, tenant event.TenantID) (capabilities.Capabilities, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saw = tenant
	return s.ans, nil
}

func (s *sdkCaps) tenant() event.TenantID {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saw
}

// capsServer mounts a Connect server whose handler advertises src, behind the
// same auth interceptor as remoteFakeServer.
func capsServer(t *testing.T, src loopconnect.CapabilitySource) string {
	t.Helper()
	handler := loopconnect.NewHandler(&fakeRuntime{}).WithKeepalive(time.Hour).WithCapabilities(src)
	_, h := loopv1connect.NewLoopServiceHandler(handler,
		connect.WithInterceptors(loopconnect.NewAuthInterceptor(devVerifier())))
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.URL
}

func fullInternalCaps() capabilities.Capabilities {
	return capabilities.Capabilities{
		Models: []capabilities.Model{{
			Alias: "fast", GatewayID: "cloud/fast", MaxOutputTokens: 4096, ContextWindow: 128000,
			ContextWindowIsDefault: true, Persona: "coder", IsDefault: true,
			Label: "Fast", Description: "quick answers", Tags: []string{"tools", "vision"},
		}, {
			Alias: "house", GatewayID: "cloud/house", ContextWindow: 64000,
		}},
		Skills: []capabilities.Skill{{Name: "notes", Description: "take notes"}},
		Ceilings: []capabilities.Ceiling{
			{FieldPath: "max_turns", OperatorSet: true, Value: 2.5, Direction: capabilities.DirectionMax},
			{FieldPath: "agents.queue_bound", Direction: capabilities.DirectionMax},
		},
		SecretRefs:       []string{"search-key"},
		SpecMCPAuthKinds: []harnessv1.McpAuthType{harnessv1.McpAuthType_MCP_AUTH_TYPE_BEARER},
		Server:           capabilities.Server{Version: "v1.2.3", DigestAlgorithm: "sha256"},
	}
}

// TestRemoteCapabilities drives Client.Capabilities over a real Connect server
// and checks the tenant reaches the source and every field maps back.
func TestRemoteCapabilities(t *testing.T) {
	src := &sdkCaps{ans: fullInternalCaps()}
	base := capsServer(t, src)

	c := NewRemote(base, Credentials{Token: "acme-tok", Tenant: "acme"})
	got, err := c.Capabilities(context.Background())
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if src.tenant() != "acme" {
		t.Errorf("source saw tenant %q, want acme", src.tenant())
	}
	want := Capabilities{
		Models: []ModelInfo{{
			Alias: "fast", GatewayID: "cloud/fast", MaxOutputTokens: 4096, ContextWindow: 128000,
			ContextWindowIsDefault: true, Persona: "coder", IsDefault: true,
			Label: "Fast", Description: "quick answers", Tags: []string{"tools", "vision"},
		}, {
			Alias: "house", GatewayID: "cloud/house", ContextWindow: 64000,
		}},
		Skills: []SkillInfo{{Name: "notes", Description: "take notes"}},
		Ceilings: []Ceiling{
			{FieldPath: "max_turns", OperatorSet: true, Value: 2.5, Direction: CeilingMax},
			{FieldPath: "agents.queue_bound", Value: 0, Direction: CeilingMax},
		},
		SecretRefs:       []string{"search-key"},
		SpecMCPAuthKinds: []harnessv1.McpAuthType{harnessv1.McpAuthType_MCP_AUTH_TYPE_BEARER},
		Server:           ServerInfo{Version: "v1.2.3", DigestAlgorithm: "sha256"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Capabilities =\n %+v\nwant\n %+v", got, want)
	}
}

// TestRemoteCapabilitiesErrors pins the sentinel each refusal maps to.
func TestRemoteCapabilitiesErrors(t *testing.T) {
	ctx := context.Background()
	base := capsServer(t, &sdkCaps{ans: fullInternalCaps()})

	if _, err := NewRemote(base, Credentials{Token: "bad-tok", Tenant: "acme"}).Capabilities(ctx); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("bad token: err = %v, want ErrUnauthenticated", err)
	}
	if _, err := NewRemote(base, Credentials{Token: "acme-tok", Tenant: "other"}).Capabilities(ctx); !errors.Is(err, ErrPermissionDenied) {
		t.Errorf("tenant spoof: err = %v, want ErrPermissionDenied", err)
	}

	unwired := capsServer(t, nil)
	if _, err := NewRemote(unwired, Credentials{Token: "acme-tok", Tenant: "acme"}).Capabilities(ctx); !errors.Is(err, ErrCapabilitiesUnsupported) {
		t.Errorf("no source: err = %v, want ErrCapabilitiesUnsupported", err)
	}
}

// TestLocalCapabilitiesUnsupported: an in-process runtime has no operator to
// advertise.
func TestLocalCapabilitiesUnsupported(t *testing.T) {
	c := NewLocal(&fakeRuntime{}, Credentials{Tenant: "acme"})
	if _, err := c.Capabilities(context.Background()); !errors.Is(err, ErrCapabilitiesUnsupported) {
		t.Errorf("local Capabilities err = %v, want ErrCapabilitiesUnsupported", err)
	}
}

// TestFromProtoCapabilitiesLimits: the tenant's loop limits map through, and an
// absent limits message maps to the zero value (unlimited).
func TestFromProtoCapabilitiesLimits(t *testing.T) {
	got := fromProtoCapabilities(&loopv1.GetCapabilitiesResponse{
		Limits: &loopv1.TenantLimits{MaxLiveLoops: 4, StartsPerMinute: 9},
	})
	if want := (Limits{MaxLiveLoops: 4, StartsPerMinute: 9}); got.Limits != want {
		t.Errorf("Limits = %+v, want %+v", got.Limits, want)
	}
	if got := fromProtoCapabilities(&loopv1.GetCapabilitiesResponse{}); got.Limits != (Limits{}) {
		t.Errorf("nil Limits = %+v, want zero", got.Limits)
	}
}
