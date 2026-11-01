package fuse

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"

	loopv1 "github.com/ethanhinson/fuse/internal/loopwire/v1"
	"github.com/ethanhinson/fuse/internal/loopwire/v1/loopv1connect"
)

// headerRecorder is a minimal loop service that records the Authorization header
// of every GetCapabilities, StartLoop and Observe request, in arrival order.
type headerRecorder struct {
	loopv1connect.UnimplementedLoopServiceHandler
	mu   sync.Mutex
	seen []string
}

func (h *headerRecorder) record(v string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seen = append(h.seen, v)
}

func (h *headerRecorder) got() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.seen...)
}

func (h *headerRecorder) GetCapabilities(_ context.Context, req *connect.Request[loopv1.GetCapabilitiesRequest]) (*connect.Response[loopv1.GetCapabilitiesResponse], error) {
	h.record(req.Header().Get("Authorization"))
	return connect.NewResponse(&loopv1.GetCapabilitiesResponse{}), nil
}

func (h *headerRecorder) StartLoop(_ context.Context, req *connect.Request[loopv1.StartLoopRequest]) (*connect.Response[loopv1.StartLoopResponse], error) {
	h.record(req.Header().Get("Authorization"))
	return connect.NewResponse(&loopv1.StartLoopResponse{LoopId: "loop-1"}), nil
}

func (h *headerRecorder) Observe(_ context.Context, req *connect.Request[loopv1.ObserveRequest], stream *connect.ServerStream[loopv1.ObserveEvent]) error {
	h.record(req.Header().Get("Authorization"))
	return stream.Send(&loopv1.ObserveEvent{Event: &loopv1.Event{Seq: 1, Kind: "turn.start"}})
}

func newHeaderRecorderServer(t *testing.T) (*headerRecorder, string) {
	t.Helper()
	rec := &headerRecorder{}
	_, h := loopv1connect.NewLoopServiceHandler(rec)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return rec, srv.URL
}

// drain reads ch until it closes or the deadline passes.
func drain(t *testing.T, ch <-chan Event) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("observe stream did not end")
		}
	}
}

func TestTokenProviderCalledPerRequest(t *testing.T) {
	rec, base := newHeaderRecorderServer(t)
	var mu sync.Mutex
	tokens := []string{"tok-1", "tok-2", "tok-3"}
	calls := 0
	provider := func(context.Context) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		tok := tokens[calls]
		calls++
		return tok, nil
	}
	c := NewRemote(base, Credentials{Token: "static"}, WithTokenProvider(provider))
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		if _, err := c.Capabilities(ctx); err != nil {
			t.Fatalf("Capabilities #%d: %v", i+1, err)
		}
	}
	ch, unsub, err := c.Observe(ctx, "loop-1", 0)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	defer unsub()
	drain(t, ch)

	got := rec.got()
	want := []string{"Bearer tok-1", "Bearer tok-2", "Bearer tok-3"}
	if len(got) != len(want) {
		t.Fatalf("recorded %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("recorded %q, want %q", got, want)
		}
	}
	for _, v := range got {
		if v == "Bearer static" {
			t.Fatalf("static Token was sent although WithTokenProvider is set: %q", got)
		}
	}
}

func TestTokenProviderErrorFailsRequest(t *testing.T) {
	rec, base := newHeaderRecorderServer(t)
	errBoom := errors.New("boom")
	c := NewRemote(base, Credentials{}, WithTokenProvider(func(context.Context) (string, error) {
		return "", errBoom
	}))
	ctx := context.Background()

	if _, err := c.Capabilities(ctx); !errors.Is(err, errBoom) {
		t.Fatalf("Capabilities err = %v, want errors.Is errBoom", err)
	}
	if _, err := c.StartLoop(ctx, StartLoopConfig{Task: "x"}); !errors.Is(err, errBoom) {
		t.Fatalf("StartLoop err = %v, want errors.Is errBoom", err)
	}
	ch, unsub, err := c.Observe(ctx, "loop-1", 0)
	if !errors.Is(err, errBoom) {
		if unsub != nil {
			unsub()
		}
		if ch != nil {
			drain(t, ch)
		}
		t.Fatalf("Observe err = %v, want errors.Is errBoom", err)
	}
	if got := rec.got(); len(got) != 0 {
		t.Fatalf("server recorded %d requests %q, want none", len(got), got)
	}
}

func TestTokenProviderEmptySendsNoHeader(t *testing.T) {
	rec, base := newHeaderRecorderServer(t)
	c := NewRemote(base, Credentials{Token: "static"}, WithTokenProvider(func(context.Context) (string, error) {
		return "", nil
	}))
	if _, err := c.Capabilities(context.Background()); err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	got := rec.got()
	if len(got) != 1 || got[0] != "" {
		t.Fatalf("recorded %q, want one empty Authorization", got)
	}
}

func TestStaticTokenUnchanged(t *testing.T) {
	rec, base := newHeaderRecorderServer(t)
	c := NewRemote(base, Credentials{Token: "t"})
	ctx := context.Background()
	if _, err := c.Capabilities(ctx); err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	ch, unsub, err := c.Observe(ctx, "loop-1", 0)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	defer unsub()
	drain(t, ch)
	got := rec.got()
	if len(got) != 2 || got[0] != "Bearer t" || got[1] != "Bearer t" {
		t.Fatalf("recorded %q, want [Bearer t Bearer t]", got)
	}
}

// A provider error on a stream must not put anything on the wire: no HTTP
// request at all, not even an unauthenticated one with an empty body. The server
// handler is wrapped so raw hits are counted below connect's own decoding.
func TestTokenProviderErrorSendsNoHTTPForStream(t *testing.T) {
	rec := &headerRecorder{}
	_, h := loopv1connect.NewLoopServiceHandler(rec)
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	errBoom := errors.New("boom")
	c := NewRemote(srv.URL, Credentials{}, WithTokenProvider(func(context.Context) (string, error) {
		return "", errBoom
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ch, unsub, err := c.Observe(ctx, "loop-1", 0)
	if unsub != nil {
		defer unsub()
	}
	if !errors.Is(err, errBoom) {
		if ch != nil {
			drain(t, ch)
		}
		t.Fatalf("Observe err = %v, want errors.Is errBoom", err)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("server saw %d raw HTTP requests on a provider error, want 0", n)
	}
}

// Credentials must stay comparable (usable as a map key, ==): the token provider
// is an Option, never a Credentials field.
func TestCredentialsComparable(t *testing.T) {
	m := map[Credentials]int{{Token: "a", Tenant: "t"}: 1}
	if m[Credentials{Token: "a", Tenant: "t"}] != 1 {
		t.Fatal("Credentials is not usable as a map key")
	}
}
