package loopconnect

import (
	"context"
	"net/http"
	"testing"

	"connectrpc.com/connect"

	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/loopauth"
	loopv1 "github.com/ethanhinson/fuse/internal/loopwire/v1"
)

const (
	testToken = "sekret-token"
)

func testVerifier() loopauth.Verifier {
	return loopauth.NewStaticVerifier(map[string]loopauth.Principal{
		testToken: {Tenant: event.TenantID("acme"), Subject: "alice"},
	})
}

// fakeStreamConn is a minimal connect.StreamingHandlerConn: only RequestHeader
// carries behavior (the interceptor touches nothing else). The remaining methods
// exist solely to satisfy the interface and return zero values.
type fakeStreamConn struct {
	header http.Header
}

func (c *fakeStreamConn) RequestHeader() http.Header  { return c.header }
func (c *fakeStreamConn) Spec() connect.Spec          { return connect.Spec{} }
func (c *fakeStreamConn) Peer() connect.Peer          { return connect.Peer{} }
func (c *fakeStreamConn) Receive(any) error           { return nil }
func (c *fakeStreamConn) Send(any) error              { return nil }
func (c *fakeStreamConn) ResponseHeader() http.Header { return http.Header{} }
func (c *fakeStreamConn) ResponseTrailer() http.Header {
	return http.Header{}
}

func TestAuthInterceptor_Unary_ValidToken(t *testing.T) {
	interceptor := NewAuthInterceptor(testVerifier())

	var (
		called    bool
		gotPrin   loopauth.Principal
		gotOK     bool
	)
	next := connect.UnaryFunc(func(ctx context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		called = true
		gotPrin, gotOK = PrincipalFrom(ctx)
		return connect.NewResponse(&loopv1.SendResponse{}), nil
	})

	req := connect.NewRequest(&loopv1.SendRequest{})
	req.Header().Set("Authorization", "Bearer "+testToken)

	resp, err := interceptor.WrapUnary(next)(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called {
		t.Fatal("next handler was not called")
	}
	if resp == nil {
		t.Fatal("response passed through as nil")
	}
	if !gotOK {
		t.Fatal("PrincipalFrom(ctx) returned ok=false; principal not stashed")
	}
	if gotPrin.Tenant != event.TenantID("acme") || gotPrin.Subject != "alice" {
		t.Fatalf("wrong principal: %+v", gotPrin)
	}
}

func TestAuthInterceptor_Unary_MissingHeader(t *testing.T) {
	interceptor := NewAuthInterceptor(testVerifier())

	called := false
	next := connect.UnaryFunc(func(ctx context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		called = true
		return connect.NewResponse(&loopv1.SendResponse{}), nil
	})

	req := connect.NewRequest(&loopv1.SendRequest{})
	// no Authorization header

	_, err := interceptor.WrapUnary(next)(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for missing Authorization header")
	}
	if got := connect.CodeOf(err); got != connect.CodeUnauthenticated {
		t.Fatalf("code = %v, want %v", got, connect.CodeUnauthenticated)
	}
	if called {
		t.Fatal("next handler was called despite missing token")
	}
}

func TestAuthInterceptor_Unary_InvalidToken(t *testing.T) {
	interceptor := NewAuthInterceptor(testVerifier())

	called := false
	next := connect.UnaryFunc(func(ctx context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		called = true
		return connect.NewResponse(&loopv1.SendResponse{}), nil
	})

	req := connect.NewRequest(&loopv1.SendRequest{})
	req.Header().Set("Authorization", "Bearer not-a-real-token")

	_, err := interceptor.WrapUnary(next)(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for invalid token")
	}
	if got := connect.CodeOf(err); got != connect.CodeUnauthenticated {
		t.Fatalf("code = %v, want %v", got, connect.CodeUnauthenticated)
	}
	if called {
		t.Fatal("next handler was called despite invalid token")
	}
}

func TestAuthInterceptor_Streaming_ValidToken(t *testing.T) {
	interceptor := NewAuthInterceptor(testVerifier())

	var (
		called  bool
		gotPrin loopauth.Principal
		gotOK   bool
	)
	next := connect.StreamingHandlerFunc(func(ctx context.Context, _ connect.StreamingHandlerConn) error {
		called = true
		gotPrin, gotOK = PrincipalFrom(ctx)
		return nil
	})

	h := http.Header{}
	h.Set("Authorization", "Bearer "+testToken)
	conn := &fakeStreamConn{header: h}

	if err := interceptor.WrapStreamingHandler(next)(context.Background(), conn); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called {
		t.Fatal("next streaming handler was not called")
	}
	if !gotOK {
		t.Fatal("PrincipalFrom(ctx) returned ok=false; principal not stashed on streaming ctx")
	}
	if gotPrin.Tenant != event.TenantID("acme") || gotPrin.Subject != "alice" {
		t.Fatalf("wrong principal: %+v", gotPrin)
	}
}

func TestAuthInterceptor_Streaming_MissingHeader(t *testing.T) {
	interceptor := NewAuthInterceptor(testVerifier())

	called := false
	next := connect.StreamingHandlerFunc(func(ctx context.Context, _ connect.StreamingHandlerConn) error {
		called = true
		return nil
	})

	conn := &fakeStreamConn{header: http.Header{}} // no Authorization

	err := interceptor.WrapStreamingHandler(next)(context.Background(), conn)
	if err == nil {
		t.Fatal("expected error for missing Authorization header")
	}
	if got := connect.CodeOf(err); got != connect.CodeUnauthenticated {
		t.Fatalf("code = %v, want %v", got, connect.CodeUnauthenticated)
	}
	if called {
		t.Fatal("next streaming handler was called despite missing token")
	}
}

func TestAuthInterceptor_Streaming_InvalidToken(t *testing.T) {
	interceptor := NewAuthInterceptor(testVerifier())

	called := false
	next := connect.StreamingHandlerFunc(func(ctx context.Context, _ connect.StreamingHandlerConn) error {
		called = true
		return nil
	})

	h := http.Header{}
	h.Set("Authorization", "Bearer bogus")
	conn := &fakeStreamConn{header: h}

	err := interceptor.WrapStreamingHandler(next)(context.Background(), conn)
	if err == nil {
		t.Fatal("expected error for invalid token")
	}
	if got := connect.CodeOf(err); got != connect.CodeUnauthenticated {
		t.Fatalf("code = %v, want %v", got, connect.CodeUnauthenticated)
	}
	if called {
		t.Fatal("next streaming handler was called despite invalid token")
	}
}

// compile-time assertion that the interceptor satisfies connect.Interceptor.
var _ connect.Interceptor = authInterceptor{}
