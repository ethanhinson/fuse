package loopconnect

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/ethanhinson/fuse/internal/event"
	loopv1 "github.com/ethanhinson/fuse/internal/loopwire/v1"
	"github.com/ethanhinson/fuse/internal/loopwire/v1/loopv1connect"
)

// TestObserveIdleKeepaliveSurvives asserts a stream held idle past a simulated
// gateway idle timeout survives via keepalive frames: with a short keepalive interval
// and NO events, the client still receives keepalive:true frames (no Seq), so a
// parked-but-alive loop's stream is not reaped.
func TestObserveIdleKeepaliveSurvives(t *testing.T) {
	f := newObserveFake()
	f.hist = func() []event.Event { return nil } // no history, no live events: pure idle.

	h := NewHandler(&f.fakeRuntime).WithKeepalive(20 * time.Millisecond)
	client := newObserveTestServer(t, h, f)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := client.Observe(ctx, connect.NewRequest(&loopv1.ObserveRequest{LoopId: "l"}))
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}

	// Over ~an idle window we must receive at least two keepalive frames and zero
	// event frames — proving the stream stays open with no traffic.
	keepalives := 0
	deadline := time.Now().Add(3 * time.Second)
	for keepalives < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("did not receive 2 keepalives in the idle window (got %d)", keepalives)
		}
		if !stream.Receive() {
			t.Fatalf("idle stream ended early (err=%v)", stream.Err())
		}
		msg := stream.Msg()
		if msg.Event != nil {
			t.Fatalf("unexpected event frame on an idle stream: seq %d", msg.Event.Seq)
		}
		if msg.Keepalive {
			keepalives++
		}
	}
	cancel()
}

// TestForwardedProtoDerivation asserts the handler derives the external scheme from
// X-Forwarded-Proto when present, rather than hard-assuming its own (plaintext)
// transport. The derived scheme is surfaced via ForwardedScheme so a binding can log
// or use it; it must NOT hard-assume TLS.
func TestForwardedProtoDerivation(t *testing.T) {
	// No forwarded header on a plaintext request → "http".
	reqPlain := connect.NewRequest(&loopv1.StartLoopRequest{})
	if got := ForwardedScheme(reqPlain.Header(), false); got != "http" {
		t.Fatalf("plaintext scheme = %q, want http", got)
	}
	// X-Forwarded-Proto: https on a plaintext hop → "https" (proxy terminated TLS).
	reqFwd := connect.NewRequest(&loopv1.StartLoopRequest{})
	reqFwd.Header().Set("X-Forwarded-Proto", "https")
	if got := ForwardedScheme(reqFwd.Header(), false); got != "https" {
		t.Fatalf("forwarded scheme = %q, want https (must not hard-assume own TLS)", got)
	}
	// Direct TLS with no forwarded header → "https".
	if got := ForwardedScheme(http.Header{}, true); got != "https" {
		t.Fatalf("direct-TLS scheme = %q, want https", got)
	}
}

// TestUnaryRetryRewindsBody asserts the client retry helper re-sends a NON-EMPTY
// request body on attempt 2 (rewind-request-body-on-manual-retry): a naive retry over
// a consumed body would send an empty body the second time.
func TestUnaryRetryRewindsBody(t *testing.T) {
	var attempts atomic.Int32
	var secondBodyLen atomic.Int64

	// A raw HTTP handler that fails attempt 1 with 503 and, on attempt 2, records the
	// received body length then returns a valid connect unary response.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		body, _ := io.ReadAll(r.Body)
		if n == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		secondBodyLen.Store(int64(len(body)))
		// Minimal valid connect unary (proto) response: echo an empty StartLoopResponse.
		w.Header().Set("Content-Type", r.Header.Get("Content-Type"))
		// A connect unary proto response body is the marshaled message; an empty
		// StartLoopResponse marshals to zero bytes, which is a valid response.
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := loopv1connect.NewLoopServiceClient(srv.Client(), srv.URL)

	resp, err := RetryUnary(context.Background(), 3, func(ctx context.Context) (*connect.Response[loopv1.StartLoopResponse], error) {
		// A fresh request per attempt is the correct rewind: the helper's contract is
		// that the closure rebuilds the request so the body is non-empty each time.
		return client.StartLoop(ctx, connect.NewRequest(&loopv1.StartLoopRequest{Task: "a-non-empty-task"}))
	})
	if err != nil {
		t.Fatalf("RetryUnary: %v", err)
	}
	_ = resp
	if attempts.Load() < 2 {
		t.Fatalf("expected a retry (>=2 attempts), got %d", attempts.Load())
	}
	if secondBodyLen.Load() == 0 {
		t.Fatal("attempt 2 sent an EMPTY body — request body was not rewound on retry")
	}
}
