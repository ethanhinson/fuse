package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/ethanhinson/fuse/internal/event"
	loopv1 "github.com/ethanhinson/fuse/internal/loopwire/v1"
	"github.com/ethanhinson/fuse/internal/loopwire/v1/loopv1connect"
)

// holdingGateway is a scripted model gateway. A request whose body contains
// HOLD blocks until its request ctx ends (the loop's model call was cancelled);
// every other request answers "done" at once.
type holdingGateway struct {
	holding   atomic.Bool
	sawCancel atomic.Bool
	release   chan struct{}
	url       string
}

func newHoldingGateway(t *testing.T) *holdingGateway {
	t.Helper()
	g := &holdingGateway{release: make(chan struct{})}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if bytes.Contains(body, []byte("HOLD")) {
			g.holding.Store(true)
			select {
			case <-r.Context().Done():
				g.sawCancel.Store(true)
			case <-g.release:
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"done"}}]}`))
	}))
	// Cleanups run last-in first-out: release any held handler before Close,
	// which waits for outstanding requests.
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(g.release) })
	g.url = srv.URL
	return g
}

type cancelServer struct {
	gw   *holdingGateway
	base string
	logs *lockedBuffer
}

func (s *cancelServer) client(tok string) loopv1connect.LoopServiceClient {
	return loopv1connect.NewLoopServiceClient(h2cClient(), s.base,
		connect.WithInterceptors(bearerClientInterceptor{token: tok}))
}

// startCancelServer runs the real `fuse loop-serve-net` command against a
// holding gateway, with three principals in two tenants, metrics and debug
// logging on. extra is appended under loop_server.
func startCancelServer(t *testing.T, extra string) *cancelServer {
	t.Helper()
	gw := newHoldingGateway(t)

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("LLM_GATEWAY_URL", gw.url)
	t.Setenv("LLM_GATEWAY_KEY", "test-key")
	if err := os.Mkdir(filepath.Join(home, ".fuse"), 0o755); err != nil {
		t.Fatal(err)
	}
	configYAML := fmt.Sprintf(`gateway:
  url: %q
  key: test-key
models:
  default: fast
  fast: {id: cloud/fast}
throughput: {requests_per_minute: 6000}
loop_server:
  auth:
    - {token: owner-tok, tenant: acme, subject: owner}
    - {token: peer-tok, tenant: acme, subject: peer}
    - {token: other-tok, tenant: globex, subject: other}
%sobservability:
  instance_id: cancel-acceptance
  metrics:
    enabled: true
    path: /metrics
    access: public
  logging:
    enabled: true
    output: stdout
    level: debug
    max_override_ttl: 1m
  cardinality:
    hash_version: sha256-64-v1
    salt: cancel-acceptance
    tenant:
      budget: 3
      catalog: [acme, globex]
    model:
      budget: 2
    tool:
      budget: 2
`, gw.url, extra)
	if err := os.WriteFile(filepath.Join(home, ".fuse", "config.yml"), []byte(configYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	oldListen, oldContext := netListen, serveNetContext
	netListen = func(string, string) (net.Listener, error) { return ln, nil }
	serveNetContext = func() (context.Context, context.CancelFunc) { return ctx, func() {} }
	logs := &lockedBuffer{}
	commandDone := make(chan int, 1)
	go func() {
		commandDone <- run([]string{"loop-serve-net", "--addr", ln.Addr().String()}, logs, &bytes.Buffer{})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-commandDone:
		case <-time.After(10 * time.Second):
			t.Errorf("loop-serve-net did not exit within 10s; logs:\n%s", logs.String())
		}
		netListen, serveNetContext = oldListen, oldContext
	})

	s := &cancelServer{gw: gw, base: "http://" + ln.Addr().String(), logs: logs}
	// Readiness: retry the first call until the server answers.
	owner := s.client("owner-tok")
	deadline := time.Now().Add(3 * time.Second)
	for {
		_, err = owner.GetCapabilities(context.Background(), connect.NewRequest(&loopv1.GetCapabilitiesRequest{}))
		if err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("loop-serve-net not ready: %v; logs:\n%s", err, logs.String())
	}
	return s
}

// observeStream reads one Observe stream in the background and hands its
// events (keepalives dropped) to a channel, so a test can wait with timeouts.
type observeStream struct {
	events chan *loopv1.Event
	cancel context.CancelFunc
}

func openObserve(t *testing.T, cl loopv1connect.LoopServiceClient, loopID string) *observeStream {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	stream, err := cl.Observe(ctx, connect.NewRequest(&loopv1.ObserveRequest{LoopId: loopID}))
	if err != nil {
		cancel()
		t.Fatalf("Observe: %v", err)
	}
	o := &observeStream{events: make(chan *loopv1.Event, 256), cancel: cancel}
	go func() {
		defer close(o.events)
		for stream.Receive() {
			msg := stream.Msg()
			if msg.Keepalive || msg.Event == nil {
				continue
			}
			select {
			case o.events <- msg.Event:
			case <-ctx.Done():
				return
			}
		}
	}()
	t.Cleanup(func() { cancel(); _ = stream.Close() })
	return o
}

// until reads events until one of kind arrives, failing after d.
func (o *observeStream) until(t *testing.T, kind event.Kind, d time.Duration) *loopv1.Event {
	t.Helper()
	timer := time.NewTimer(d)
	defer timer.Stop()
	for {
		select {
		case e, ok := <-o.events:
			if !ok {
				t.Fatalf("Observe stream ended before %s", kind)
			}
			if event.Kind(e.Kind) == kind {
				return e
			}
		case <-timer.C:
			t.Fatalf("no %s within %s", kind, d)
		}
	}
}

func startHold(t *testing.T, cl loopv1connect.LoopServiceClient) string {
	t.Helper()
	resp, err := cl.StartLoop(context.Background(), connect.NewRequest(&loopv1.StartLoopRequest{Task: "HOLD please"}))
	if err != nil {
		t.Fatalf("StartLoop HOLD: %v", err)
	}
	return resp.Msg.LoopId
}

func cancelLoop(cl loopv1connect.LoopServiceClient, req *loopv1.CancelLoopRequest) (*connect.Response[loopv1.CancelLoopResponse], error) {
	return cl.CancelLoop(context.Background(), connect.NewRequest(req))
}

func wantCode(t *testing.T, name string, err error, code connect.Code) *connect.Error {
	t.Helper()
	var cerr *connect.Error
	if !errors.As(err, &cerr) || cerr.Code() != code {
		t.Fatalf("%s: err = %v, want %s", name, err, code)
	}
	return cerr
}

func eventually(t *testing.T, name string, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not true within %s", name, d)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestCancelLoopOverTheWire drives CancelLoop through the real loop-serve-net
// command: the held model call is cancelled, loop.cancelled is the last event
// on the live Observe stream, Send is refused as cancelled, a repeat cancel is
// idempotent, and the cancel shows up in /metrics and the projected logs.
func TestCancelLoopOverTheWire(t *testing.T) {
	s := startCancelServer(t, "")
	owner := s.client("owner-tok")

	loopID := startHold(t, owner)
	obs := openObserve(t, owner, loopID)
	obs.until(t, event.KindModelCallStart, 5*time.Second)

	resp, err := cancelLoop(owner, &loopv1.CancelLoopRequest{LoopId: loopID, Reason: "operator stop"})
	if err != nil {
		t.Fatalf("CancelLoop: %v; logs:\n%s", err, s.logs.String())
	}
	if got := resp.Msg.GetOutcome(); got != loopv1.CancelOutcome_CANCEL_OUTCOME_CANCELLED {
		t.Fatalf("outcome = %s, want CANCELLED", got)
	}
	if !resp.Msg.GetWasLive() {
		t.Error("was_live = false, want true")
	}
	terminal := resp.Msg.GetTerminalSeq()
	if terminal == 0 {
		t.Error("terminal_seq = 0, want the seq of loop.cancelled")
	}

	ev := obs.until(t, event.KindLoopCancelled, 5*time.Second)
	var p event.LoopCancelledPayload
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		t.Fatalf("decode loop.cancelled: %v", err)
	}
	if p.Reason != "operator stop" || !p.WasLive || p.RequestedBy != "owner" {
		t.Errorf("loop.cancelled payload = %+v, want reason %q, was_live, requested_by owner", p, "operator stop")
	}
	if ev.Seq != terminal {
		t.Errorf("loop.cancelled seq = %d, want terminal_seq %d", ev.Seq, terminal)
	}
	select {
	case e, ok := <-obs.events:
		if ok {
			t.Errorf("event after loop.cancelled: seq %d kind %s", e.Seq, e.Kind)
		}
	case <-time.After(200 * time.Millisecond):
	}

	eventually(t, "gateway saw its request ctx cancelled", 5*time.Second, s.gw.sawCancel.Load)

	_, err = owner.Send(context.Background(), connect.NewRequest(&loopv1.SendRequest{LoopId: loopID, Input: "hi"}))
	cerr := wantCode(t, "Send after cancel", err, connect.CodeFailedPrecondition)
	if !strings.Contains(cerr.Message(), "cancelled") || strings.Contains(cerr.Message(), "finished") {
		t.Errorf("Send message = %q, want it to say cancelled, not finished", cerr.Message())
	}

	again, err := cancelLoop(owner, &loopv1.CancelLoopRequest{LoopId: loopID, Reason: "second"})
	if err != nil {
		t.Fatalf("second CancelLoop: %v", err)
	}
	if got := again.Msg.GetOutcome(); got != loopv1.CancelOutcome_CANCEL_OUTCOME_ALREADY_CANCELLED {
		t.Errorf("second outcome = %s, want ALREADY_CANCELLED", got)
	}
	if got := again.Msg.GetReason(); got != "operator stop" {
		t.Errorf("second reason = %q, want the first cancel's %q", got, "operator stop")
	}

	const metricLine = `fuse_loops_cancelled_total{tenant_id="acme",was_live="true"} 1`
	var body string
	eventually(t, "metrics show the cancel", 2*time.Second, func() bool {
		r, err := http.Get(s.base + "/metrics")
		if err != nil {
			return false
		}
		b, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()
		body = string(b)
		return strings.Contains(body, metricLine)
	})
	if strings.Contains(body, "operator stop") {
		t.Error("the cancel reason leaked into /metrics")
	}

	// The projected log line: payload-free, so the reason never appears.
	var found bool
	eventually(t, "projected loop.cancelled log line", 2*time.Second, func() bool {
		for _, line := range strings.Split(s.logs.String(), "\n") {
			var rec struct {
				EventName string `json:"event_name"`
				Operation string `json:"operation"`
				Outcome   string `json:"outcome"`
				TenantID  string `json:"tenant_id"`
			}
			if json.Unmarshal([]byte(line), &rec) != nil {
				continue
			}
			if rec.EventName == string(event.KindLoopCancelled) {
				found = rec.Operation == "loop" && rec.Outcome == "canceled" && rec.TenantID == "acme"
				return true
			}
		}
		return false
	})
	if !found {
		t.Errorf("loop.cancelled log line lacks operation=loop outcome=canceled tenant acme; logs:\n%s", s.logs.String())
	}
	if strings.Contains(s.logs.String(), "operator stop") {
		t.Error("the cancel reason leaked into the structured logs")
	}
}

// TestCancelLoopTenantIsolationOverTheWire: another tenant cannot find the
// loop, a same-tenant non-owner is refused, and a spoofed wire tenant is
// refused; none of them disturbs the running loop.
func TestCancelLoopTenantIsolationOverTheWire(t *testing.T) {
	s := startCancelServer(t, "")
	owner := s.client("owner-tok")

	loopID := startHold(t, owner)
	eventually(t, "loop holding its model call", 5*time.Second, s.gw.holding.Load)

	_, err := cancelLoop(s.client("other-tok"), &loopv1.CancelLoopRequest{LoopId: loopID, Reason: "x"})
	wantCode(t, "other tenant", err, connect.CodeNotFound)
	_, err = cancelLoop(s.client("peer-tok"), &loopv1.CancelLoopRequest{LoopId: loopID, Reason: "x"})
	wantCode(t, "same-tenant non-owner", err, connect.CodePermissionDenied)
	_, err = cancelLoop(owner, &loopv1.CancelLoopRequest{LoopId: loopID, Tenant: "globex", Reason: "x"})
	wantCode(t, "spoofed wire tenant", err, connect.CodePermissionDenied)

	// The loop is intact: still holding, and its stream has no loop.cancelled.
	if s.gw.sawCancel.Load() {
		t.Fatal("a refused cancel reached the running model call")
	}
	obs := openObserve(t, owner, loopID)
	obs.until(t, event.KindModelCallStart, 5*time.Second)
	timer := time.NewTimer(200 * time.Millisecond)
drain:
	for {
		select {
		case e, ok := <-obs.events:
			if !ok {
				break drain
			}
			if event.Kind(e.Kind) == event.KindLoopCancelled {
				t.Fatal("a refused cancel appended loop.cancelled")
			}
		case <-timer.C:
			break drain
		}
	}

	resp, err := cancelLoop(owner, &loopv1.CancelLoopRequest{LoopId: loopID, Reason: "cleanup"})
	if err != nil {
		t.Fatalf("owner CancelLoop: %v", err)
	}
	if got := resp.Msg.GetOutcome(); got != loopv1.CancelOutcome_CANCEL_OUTCOME_CANCELLED || !resp.Msg.GetWasLive() {
		t.Errorf("owner cancel = %s was_live=%v, want CANCELLED was_live=true", got, resp.Msg.GetWasLive())
	}
}

// TestCancelLoopFreesLiveLoopSlot: under max_live_loops: 1, a StartLoop issued
// right after CANCELLED succeeds, because CANCELLED means the loop is no
// longer live in the registry.
func TestCancelLoopFreesLiveLoopSlot(t *testing.T) {
	s := startCancelServer(t, "  limits:\n    default: {max_live_loops: 1}\n")
	owner := s.client("owner-tok")

	loopID := startHold(t, owner)
	_, err := owner.StartLoop(context.Background(), connect.NewRequest(&loopv1.StartLoopRequest{Task: "second"}))
	wantCode(t, "second start under the cap", err, connect.CodeResourceExhausted)

	resp, err := cancelLoop(owner, &loopv1.CancelLoopRequest{LoopId: loopID, Reason: "free the slot"})
	if err != nil {
		t.Fatalf("CancelLoop: %v", err)
	}
	if got := resp.Msg.GetOutcome(); got != loopv1.CancelOutcome_CANCEL_OUTCOME_CANCELLED {
		t.Fatalf("outcome = %s, want CANCELLED", got)
	}
	if _, err := owner.StartLoop(context.Background(), connect.NewRequest(&loopv1.StartLoopRequest{Task: "after cancel"})); err != nil {
		t.Fatalf("StartLoop right after CANCELLED: %v", err)
	}
}
