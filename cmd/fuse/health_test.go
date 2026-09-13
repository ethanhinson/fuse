package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/loopauth"
)

// pingStore is a minimal event.DurableStore that also implements event.Pinger,
// counting Ping calls so the 2s probe cache is observable. Every stream method is
// a stub: readiness only ever calls Ping.
type pingStore struct {
	calls atomic.Int64
	err   error
}

func (s *pingStore) Append(context.Context, event.StreamKey, event.Event) error { return nil }
func (s *pingStore) Subscribe(context.Context, event.StreamKey) (<-chan event.Event, func(), error) {
	return nil, func() {}, nil
}
func (s *pingStore) Replay(context.Context, event.StreamKey, event.Seq) ([]event.Event, error) {
	return nil, nil
}
func (s *pingStore) Ping(context.Context) error {
	s.calls.Add(1)
	return s.err
}

// noPingStore is a DurableStore that deliberately does NOT implement event.Pinger:
// the optional-interface degrade path must read as ready.
type noPingStore struct{}

func (noPingStore) Append(context.Context, event.StreamKey, event.Event) error { return nil }
func (noPingStore) Subscribe(context.Context, event.StreamKey) (<-chan event.Event, func(), error) {
	return nil, func() {}, nil
}
func (noPingStore) Replay(context.Context, event.StreamKey, event.Seq) ([]event.Event, error) {
	return nil, nil
}

func testVerifier() loopauth.Verifier {
	return loopauth.NewStaticVerifier(map[string]loopauth.Principal{
		"tkn": {Tenant: event.DefaultTenant, Subject: "sub"},
	})
}

// get issues a GET against path on base and returns status + body.
func get(t *testing.T, base, path string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, base+path, nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, string(b)
}

func TestHealthzAlwaysOK(t *testing.T) {
	// A failing store and a nil verifier: liveness must STILL be 200. /healthz
	// answers "the process serves HTTP", nothing more — conflating it with
	// readiness makes Kubernetes restart a pod whose only problem is a
	// momentarily unreachable database.
	rd := newReadiness(&pingStore{err: errors.New("down")}, nil)
	base := healthServer(t, rd)
	if code, body := get(t, base, "/healthz"); code != http.StatusOK || strings.TrimSpace(body) != "ok" {
		t.Fatalf("/healthz = %d %q, want 200 \"ok\"", code, body)
	}
}

func TestReadyzHealthyPinger(t *testing.T) {
	rd := newReadiness(&pingStore{}, testVerifier())
	base := healthServer(t, rd)
	if code, body := get(t, base, "/readyz"); code != http.StatusOK || strings.TrimSpace(body) != "ok" {
		t.Fatalf("/readyz = %d %q, want 200 \"ok\"", code, body)
	}
}

func TestReadyzFailingPingerIs503WithEmptyBody(t *testing.T) {
	rd := newReadiness(&pingStore{err: errors.New("connection refused: secret-dsn-host:5432")}, testVerifier())
	base := healthServer(t, rd)
	code, body := get(t, base, "/readyz")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz = %d, want 503", code)
	}
	// The reason goes to the log, NEVER the response: an unauthenticated probe
	// endpoint that echoes a store error leaks infrastructure detail (here, a DSN
	// host) to anyone who can reach the port.
	if body != "" {
		t.Fatalf("/readyz 503 body = %q, want empty", body)
	}
}

func TestReadyzNilVerifierIs503(t *testing.T) {
	// No verifier means the auth surface is not up, so the instance must never be
	// advertised as ready: a Ready pod with no verifier would take traffic it
	// cannot authenticate.
	rd := newReadiness(&pingStore{}, nil)
	base := healthServer(t, rd)
	if code, body := get(t, base, "/readyz"); code != http.StatusServiceUnavailable || body != "" {
		t.Fatalf("/readyz = %d %q, want 503 with empty body", code, body)
	}
}

func TestReadyzStoreWithoutPingerIsReady(t *testing.T) {
	rd := newReadiness(noPingStore{}, testVerifier())
	base := healthServer(t, rd)
	if code := mustCode(t, base, "/readyz"); code != http.StatusOK {
		t.Fatalf("/readyz with a Pinger-less store = %d, want 200", code)
	}
}

func TestReadyzNilStoreIsReady(t *testing.T) {
	rd := newReadiness(nil, testVerifier())
	base := healthServer(t, rd)
	if code := mustCode(t, base, "/readyz"); code != http.StatusOK {
		t.Fatalf("/readyz with no store = %d, want 200", code)
	}
}

func TestReadyzDrainingIs503(t *testing.T) {
	store := &pingStore{}
	rd := newReadiness(store, testVerifier())
	base := healthServer(t, rd)
	if code := mustCode(t, base, "/readyz"); code != http.StatusOK {
		t.Fatalf("/readyz before draining = %d, want 200", code)
	}
	rd.setDraining()
	code, body := get(t, base, "/readyz")
	if code != http.StatusServiceUnavailable || body != "" {
		t.Fatalf("/readyz while draining = %d %q, want 503 with empty body", code, body)
	}
	// Draining short-circuits before the probe: once the answer is "no" for the
	// rest of this process's life, there is no reason to touch Postgres again.
	if got := store.calls.Load(); got != 1 {
		t.Fatalf("pinger calls = %d, want 1 (draining must not probe)", got)
	}
}

func TestReadyzCachesProbeResult(t *testing.T) {
	store := &pingStore{}
	rd := newReadiness(store, testVerifier())
	base := healthServer(t, rd)
	for i := 0; i < 5; i++ {
		if code := mustCode(t, base, "/readyz"); code != http.StatusOK {
			t.Fatalf("/readyz #%d = %d, want 200", i, code)
		}
	}
	if got := store.calls.Load(); got != 1 {
		t.Fatalf("pinger calls = %d across 5 probes inside the 2s window, want 1", got)
	}
}

func TestReadyzCachesProbeFailure(t *testing.T) {
	// The FAILURE path must cache too, or a liveness-probe storm against a
	// database that is down becomes a connection storm against that database.
	store := &pingStore{err: errors.New("down")}
	rd := newReadiness(store, testVerifier())
	base := healthServer(t, rd)
	for i := 0; i < 5; i++ {
		if code := mustCode(t, base, "/readyz"); code != http.StatusServiceUnavailable {
			t.Fatalf("/readyz #%d = %d, want 503", i, code)
		}
	}
	if got := store.calls.Load(); got != 1 {
		t.Fatalf("pinger calls = %d across 5 failing probes, want 1", got)
	}
}

func TestReadyzReprobesAfterCacheExpiry(t *testing.T) {
	// Mutation evidence for the cache TTL: with the clock moved past the window
	// the probe MUST run again — a cache that never expires is a readiness gate
	// that never recovers.
	store := &pingStore{}
	rd := newReadiness(store, testVerifier())
	now := time.Now()
	rd.now = func() time.Time { return now }
	base := healthServer(t, rd)
	if code := mustCode(t, base, "/readyz"); code != http.StatusOK {
		t.Fatalf("first probe = %d, want 200", code)
	}
	if got := store.calls.Load(); got != 1 {
		t.Fatalf("calls after first probe = %d, want 1", got)
	}
	now = now.Add(readinessProbeTTL + time.Millisecond)
	if code := mustCode(t, base, "/readyz"); code != http.StatusOK {
		t.Fatalf("post-expiry probe = %d, want 200", code)
	}
	if got := store.calls.Load(); got != 2 {
		t.Fatalf("calls after cache expiry = %d, want 2", got)
	}
}

func TestReadyzRecoversWhenStoreRecovers(t *testing.T) {
	store := &pingStore{err: errors.New("down")}
	rd := newReadiness(store, testVerifier())
	now := time.Now()
	rd.now = func() time.Time { return now }
	base := healthServer(t, rd)
	if code := mustCode(t, base, "/readyz"); code != http.StatusServiceUnavailable {
		t.Fatalf("while down = %d, want 503", code)
	}
	store.err = nil
	now = now.Add(readinessProbeTTL + time.Millisecond)
	if code := mustCode(t, base, "/readyz"); code != http.StatusOK {
		t.Fatalf("after recovery = %d, want 200", code)
	}
}

// TestReadyzProbeSurvivesRequestCancellation pins the interaction between the
// cache and a prober that hangs up. If the probe inherited the request context, a
// kubelet whose timeoutSeconds is shorter than a slow-but-healthy Ping would
// cancel it, and context.Canceled would be CACHED as a store failure — so the
// next probe, arriving inside the TTL, reads 503 on a healthy instance.
func TestReadyzProbeSurvivesRequestCancellation(t *testing.T) {
	release := make(chan struct{})
	store := &ctxPingStore{gate: release, entered: make(chan struct{})}
	rd := newReadiness(store, testVerifier())
	base := healthServer(t, rd)

	// A request that is abandoned while the Ping is in flight.
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/readyz", nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	abandoned := make(chan struct{})
	go func() {
		defer close(abandoned)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
		}
	}()
	store.awaitEntered(t)
	cancel()
	<-abandoned
	close(release)

	// The cached result must be the store's real answer (healthy), not the
	// client's cancellation.
	if err := rd.probe(context.Background()); err != nil {
		t.Fatalf("cached probe result = %v, want nil — a client disconnect poisoned the cache", err)
	}
}

// ctxPingStore blocks inside Ping until released, and reports the context error it
// observed, so a test can abandon the request mid-probe.
type ctxPingStore struct {
	gate    chan struct{}
	entered chan struct{}
}

func (s *ctxPingStore) Append(context.Context, event.StreamKey, event.Event) error { return nil }
func (s *ctxPingStore) Subscribe(context.Context, event.StreamKey) (<-chan event.Event, func(), error) {
	return nil, func() {}, nil
}
func (s *ctxPingStore) Replay(context.Context, event.StreamKey, event.Seq) ([]event.Event, error) {
	return nil, nil
}
func (s *ctxPingStore) awaitEntered(t *testing.T) {
	t.Helper()
	select {
	case <-s.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("Ping was never entered")
	}
}

// Ping is called exactly once in this test, so closing entered unconditionally is
// safe.
func (s *ctxPingStore) Ping(ctx context.Context) error {
	close(s.entered)
	select {
	case <-s.gate:
		return ctx.Err()
	case <-time.After(5 * time.Second):
		return errors.New("gate never released")
	}
}

// TestReadyzConcurrentProbesCollapseOntoOneCall is the concurrency assertion the
// cache exists for: N simultaneous probes that all miss the cache must still make
// ONE Ping, not N. Run under -race, it also pins that the cache fields are only
// touched under the mutex.
func TestReadyzConcurrentProbesCollapseOntoOneCall(t *testing.T) {
	store := &pingStore{}
	rd := newReadiness(store, testVerifier())
	base := healthServer(t, rd)

	const n = 24
	var wg sync.WaitGroup
	start := make(chan struct{})
	codes := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			codes[i] = mustCode(t, base, "/readyz")
		}(i)
	}
	close(start)
	wg.Wait()

	for i, c := range codes {
		if c != http.StatusOK {
			t.Fatalf("concurrent probe %d = %d, want 200", i, c)
		}
	}
	if got := store.calls.Load(); got != 1 {
		t.Fatalf("pinger calls = %d across %d simultaneous probes, want 1 — the cache must collapse a storm, not merely dedupe sequential hits", got, n)
	}
}

// TestHealthRoutesNeedNoBearerTokenWhileConnectStillDoes is the security-boundary
// assertion: the probes are reachable unauthenticated (a kubelet presents no
// credential) while the Connect RPC surface on the SAME mux still rejects an
// unauthenticated call. If a future change moves the auth interceptor from a
// connect.HandlerOption onto mux middleware, the first half of this test goes red.
func TestHealthRoutesNeedNoBearerTokenWhileConnectStillDoes(t *testing.T) {
	verifier := testVerifier()
	store := &pingStore{}

	fr := &netFakeRuntime{startID: "loop-health"}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serveNetObserved(ctx, ln, fr, verifier, nil, store, nil) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("serveNetObserved did not return after cancel")
		}
	})
	base := "http://" + ln.Addr().String()
	waitServing(t, base+"/healthz")

	for _, path := range []string{"/healthz", "/readyz"} {
		if code := mustCode(t, base, path); code != http.StatusOK {
			t.Fatalf("unauthenticated %s = %d, want 200", path, code)
		}
	}

	// Same server, same mux: a Connect unary with no Authorization header is
	// rejected. This proves the probes are an explicit, narrow exemption rather
	// than a hole in the auth surface.
	resp, err := http.Post(base+"/fuse.loop.v1.LoopService/StartLoop", "application/json", strings.NewReader(`{"task":"hi"}`))
	if err != nil {
		t.Fatalf("StartLoop: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatal("unauthenticated StartLoop succeeded — the Connect surface must still require a bearer token")
	}
}

// healthServer mounts just the probe routes on a loopback listener, so the probe
// semantics are tested without a runtime.
func healthServer(t *testing.T, rd *readiness) string {
	t.Helper()
	mux := http.NewServeMux()
	rd.register(mux)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return "http://" + ln.Addr().String()
}

func mustCode(t *testing.T, base, path string) int {
	t.Helper()
	code, _ := get(t, base, path)
	return code
}

func waitServing(t *testing.T, url string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("server never came up at %s", url)
}
