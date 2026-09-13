package main

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"testing"
	"time"
)

// TestDrainShutsDownMetricsListener proves the SEPARATE metrics listener is part
// of the graceful drain.
//
// When observability.metrics.bind is set — BOTH shipped configs set it
// (deploy/compose/fuse.compose.yml and deploy/charts/fuse/values.yaml pin
// 0.0.0.0:9090) — startMetricsEndpoint runs a second http.Server on its own
// listener and goroutine. The drain path touched only the Connect server, so a
// scrape in flight at SIGTERM was severed by process exit rather than drained,
// while the chart publishes terminationGracePeriodSeconds = drainTimeout + 10 as
// if the whole server drains.
//
// The assertion is deterministic rather than a sleep-race because the drain is
// awaited (TestDrainCompletesBeforeServeReturns): by the time
// serveNetWithOptions returns, the drain goroutine has run to completion, so the
// metrics endpoint MUST already be refusing new connections at that instant.
// Before the fix it keeps serving, because nothing ever shuts it down.
func TestDrainShutsDownMetricsListener(t *testing.T) {
	var out, errb bytes.Buffer
	obs, _, code, ok := setupLocalObservability(context.Background(),
		enabledMetricsShellConfig("127.0.0.1:0"), &out, &errb, "drain-test")
	if !ok {
		t.Fatalf("observability setup failed with code %d: %s", code, errb.String())
	}
	t.Cleanup(func() { _ = obs.Close(context.Background()) })
	if obs.metricsListener == nil {
		t.Fatal("test precondition: a bound metrics endpoint must be listening")
	}
	metricsURL := "http://" + obs.metricsListener.Addr().String() + "/metrics"
	waitServing(t, metricsURL)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	served := make(chan error, 1)
	go func() {
		served <- serveNetWithOptions(ctx, ln, &netFakeRuntime{startID: "loop-drain-metrics"}, testVerifier(), nil, &pingStore{}, obs,
			serveNetOptions{drainTimeout: 2 * time.Second})
	}()
	waitServing(t, "http://"+ln.Addr().String()+"/healthz")

	cancel()
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("serveNetWithOptions: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serveNetWithOptions never returned")
	}

	// The drain is complete by construction, so this needs no retry loop: a
	// successful scrape here means the metrics listener outlived the drain.
	resp, err := http.Get(metricsURL)
	if err == nil {
		resp.Body.Close()
		t.Fatalf("metrics endpoint still served a scrape (status=%d) after the drain completed; it is not part of the drain", resp.StatusCode)
	}
}
