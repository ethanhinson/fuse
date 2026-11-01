package fuse

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"connectrpc.com/connect"

	"google.golang.org/protobuf/proto"

	"github.com/ethanhinson/fuse/harness"
	harnessv1 "github.com/ethanhinson/fuse/harness/v1"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/loopconnect"
	"github.com/ethanhinson/fuse/internal/loopsecret"
	loopv1 "github.com/ethanhinson/fuse/internal/loopwire/v1"
	"github.com/ethanhinson/fuse/internal/loopwire/v1/loopv1connect"
	"github.com/ethanhinson/fuse/internal/runtime"
)

var sdkViolations = &harness.ValidationError{Violations: []harness.Violation{
	{FieldPath: "skills[0].name", Message: `skill "x" is not installed on this server`, RuleID: harness.RuleSkillRef},
	{FieldPath: "throughput.requests_per_minute", Message: "may only tighten this server's limit of 100", RuleID: harness.RuleTightenOnly},
}}

func sdkSpec() *harnessv1.HarnessSpec {
	turns := uint32(3)
	return &harnessv1.HarnessSpec{MaxTurns: &turns, Models: &harnessv1.Models{DefaultAlias: "glm"}}
}

// TestStartLoopCarriesTheHarnessSpec drives both backends: the spec reaches the
// runtime seam, and a refused spec comes back as a *harness.ValidationError with
// every violation, so a caller handles local and remote identically.
func TestStartLoopCarriesTheHarnessSpec(t *testing.T) {
	backends := map[string]func(f *fakeRuntime) *Client{
		"local": func(f *fakeRuntime) *Client {
			return NewLocal(f, Credentials{Tenant: string(event.DefaultTenant)})
		},
		"remote": func(f *fakeRuntime) *Client {
			return NewRemote(remoteFakeServer(t, f, devVerifier()), Credentials{Token: "good-tok", Tenant: string(event.DefaultTenant)})
		},
	}
	for name, mk := range backends {
		t.Run(name, func(t *testing.T) {
			f := &fakeRuntime{startID: "loop-h"}
			c := mk(f)
			if _, err := c.StartLoop(context.Background(), StartLoopConfig{Task: "t", Harness: sdkSpec()}); err != nil {
				t.Fatalf("StartLoop: %v", err)
			}
			if !proto.Equal(f.sawStartCfg.Harness, sdkSpec()) {
				t.Fatalf("Harness = %v", f.sawStartCfg.Harness)
			}

			f.startErr = fmt.Errorf("runtime: harness: %w", sdkViolations)
			_, err := c.StartLoop(context.Background(), StartLoopConfig{Task: "t", Harness: sdkSpec()})
			var ve *harness.ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("err = %v, want *harness.ValidationError", err)
			}
			if len(ve.Violations) != 2 || ve.Violations[0] != sdkViolations.Violations[0] || ve.Violations[1] != sdkViolations.Violations[1] {
				t.Fatalf("violations = %+v", ve.Violations)
			}

			f.startErr = runtime.ErrHarnessUnsupported
			if _, err := c.StartLoop(context.Background(), StartLoopConfig{Task: "t", Harness: sdkSpec()}); !errors.Is(err, ErrHarnessUnsupported) {
				t.Fatalf("err = %v, want ErrHarnessUnsupported", err)
			}
		})
	}
}

// TestStartLoopCarriesLoopSecrets drives both backends: loop-scoped secrets set
// with WithSecrets reach the runtime seam's LoopConfig unchanged. The remote path
// goes through the real loopconnect.Handler, whose dev verifier principal is
// unrestricted and so holds secrets:inline.
func TestStartLoopCarriesLoopSecrets(t *testing.T) {
	backends := map[string]func(f *fakeRuntime) *Client{
		"local": func(f *fakeRuntime) *Client {
			return NewLocal(f, Credentials{Tenant: string(event.DefaultTenant)})
		},
		"remote": func(f *fakeRuntime) *Client {
			return NewRemote(remoteFakeServer(t, f, devVerifier()), Credentials{Token: "good-tok", Tenant: string(event.DefaultTenant)})
		},
	}
	for name, mk := range backends {
		t.Run(name, func(t *testing.T) {
			f := &fakeRuntime{startID: "loop-s"}
			c := mk(f)
			cfg := StartLoopConfig{Task: "t", Harness: sdkSpec()}.WithSecrets(LoopSecret{Name: "my-token", Value: "sdk-v", Hosts: []string{"mcp.acme.test"}})
			if _, err := c.StartLoop(context.Background(), cfg); err != nil {
				t.Fatalf("StartLoop: %v", err)
			}
			want := []loopsecret.Secret{{Name: "my-token", Value: "sdk-v", Hosts: []string{"mcp.acme.test"}}}
			if !reflect.DeepEqual(f.sawStartCfg.Secrets, want) {
				t.Fatalf("Secrets = %#v, want %#v", f.sawStartCfg.Secrets, want)
			}
		})
	}
}

func TestStartLoopConfigWithSecretsAppendsWithoutMutating(t *testing.T) {
	a := LoopSecret{Name: "a", Value: "va", Hosts: []string{"a.test"}}
	b := LoopSecret{Name: "b", Value: "vb", Hosts: []string{"b.test"}}
	base := StartLoopConfig{Task: "t"}.WithSecrets(a)
	two := base.WithSecrets(b)
	if !reflect.DeepEqual(two.Secrets, []LoopSecret{a, b}) {
		t.Fatalf("two.Secrets = %#v", two.Secrets)
	}
	if !reflect.DeepEqual(base.Secrets, []LoopSecret{a}) {
		t.Fatalf("receiver mutated: base.Secrets = %#v", base.Secrets)
	}
	// A sibling built from the same receiver must not share backing storage.
	other := base.WithSecrets(LoopSecret{Name: "c", Value: "vc", Hosts: []string{"c.test"}})
	if two.Secrets[1].Name != "b" || other.Secrets[1].Name != "c" {
		t.Fatalf("siblings share storage: two=%#v other=%#v", two.Secrets, other.Secrets)
	}
}

// predatingHandler is a server from before harness specs: proto3 drops the
// unknown harness field, so it starts the loop on its own config and answers
// without a harness digest.
type predatingHandler struct{ *loopconnect.Handler }

func (p predatingHandler) StartLoop(ctx context.Context, req *connect.Request[loopv1.StartLoopRequest]) (*connect.Response[loopv1.StartLoopResponse], error) {
	req.Msg.Harness = nil
	return p.Handler.StartLoop(ctx, req)
}

func TestRemoteStartLoopRefusesAServerThatIgnoredTheSpec(t *testing.T) {
	f := &fakeRuntime{startID: "loop-old"}
	_, h := loopv1connect.NewLoopServiceHandler(predatingHandler{loopconnect.NewHandler(f)})
	srv := httptest.NewServer(h)
	defer srv.Close()
	c := NewRemote(srv.URL, Credentials{Tenant: string(event.DefaultTenant)})
	if _, err := c.StartLoop(context.Background(), StartLoopConfig{Task: "t", Harness: sdkSpec()}); !errors.Is(err, ErrHarnessUnsupported) {
		t.Fatalf("err = %v, want ErrHarnessUnsupported", err)
	}
	if id, err := c.StartLoop(context.Background(), StartLoopConfig{Task: "t"}); err != nil || id != "loop-old" {
		t.Fatalf("no-spec StartLoop = %q, %v", id, err)
	}
}

// TestRemoteUnimplementedIsNotAHarnessError keeps a misrouted server's
// Unimplemented (404) visible as itself on every call that carried no spec.
func TestRemoteUnimplementedIsNotAHarnessError(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	c := NewRemote(srv.URL, Credentials{Tenant: string(event.DefaultTenant)})
	ctx := context.Background()
	if _, err := c.StartLoop(ctx, StartLoopConfig{Task: "t"}); err == nil || errors.Is(err, ErrHarnessUnsupported) {
		t.Fatalf("StartLoop err = %v", err)
	}
	if err := c.Send(ctx, "x", "hi"); err == nil || errors.Is(err, ErrHarnessUnsupported) {
		t.Fatalf("Send err = %v", err)
	}
	if _, err := c.StartLoop(ctx, StartLoopConfig{Task: "t", Harness: sdkSpec()}); !errors.Is(err, ErrHarnessUnsupported) {
		t.Fatalf("spec StartLoop err = %v, want ErrHarnessUnsupported", err)
	}
}
