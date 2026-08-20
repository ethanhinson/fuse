package tools

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/event/fsstore"
	"github.com/ethanhinson/fuse/internal/loopauth"
	"github.com/ethanhinson/fuse/internal/observe"
	"github.com/ethanhinson/fuse/internal/observe/metricspolicy"
	"github.com/ethanhinson/fuse/internal/observe/prometheus"
	"github.com/ethanhinson/fuse/internal/tools/sandbox"
	client "github.com/prometheus/client_golang/prometheus"
)

// containerCLIsForE2E mirrors sandbox.containerCLIs (unexported there) so this
// package-tools test can gate on a real runtime the same way T13 does.
var containerCLIsForE2E = [...]string{"docker", "nerdctl", "podman"}

// TestContainerLifecycleFeedsSandboxMetricsEndToEnd is the observability seam's
// end-to-end proof for change 0063, and it closes the gap the build left open:
// the emitter side (TestBashPoolEmitsSandboxLifecycleEvents) stops at the event
// stream, and the projection side (prometheus.sandbox_metrics_test.go) starts
// from hand-built observe.Records. Neither runs a real container and then checks
// that its lifecycle moved real fuse_sandbox_* series on a live /metrics scrape.
// This does — through the production chain and nothing rebuilt:
//
//	real container Acquire/Exec/Release/reap
//	  → sandbox.Pool hooks (WithPoolHooks)
//	    → SandboxEventHooks → event.EventStore.Append   [the production emitter]
//	      → FSEventStore.Replay                         [durable history, as an operator's shipper would read it]
//	        → observe.ProjectEvent                      [the production event→Record projection]
//	          → prometheus.Recorder.Project             [the production recorder]
//	            → GET /metrics                          [the real exposition handler]
//
// It is gated, not build-tagged: a box without docker/nerdctl/podman still sees
// this package green (t.Skip), exactly like container_integration_test.go.
func TestContainerLifecycleFeedsSandboxMetricsEndToEnd(t *testing.T) {
	if !containerRuntimeAvailable() {
		t.Skipf("skipping: none of %s found on PATH", strings.Join(containerCLIsForE2E[:], ", "))
	}

	ctx := context.Background()

	// A container substrate over a trusted root the test owns. This is the real
	// service the bash tool sits on — same constructor, same handler selection.
	root := t.TempDir()
	svc, err := sandbox.NewService(
		sandbox.Config{Contained: true, Handler: sandbox.HandlerContainer, Image: sandbox.DefaultContainerImage},
		sandbox.WithTrustedRoot(root),
	)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if !svc.Available() {
		t.Skipf("skipping: container service unavailable (handler %q not selectable here)", svc.HandlerName())
	}
	if svc.HandlerName() != sandbox.HandlerContainer {
		t.Skipf("skipping: selected handler is %q, not a container", svc.HandlerName())
	}

	// The loop's OWN durable event store, bound to one StreamKey — the exact
	// shape the runtime hands BuildAgent. tenant "admitted" is what the recorder
	// policy below lets through; the projection reads the tenant from this key,
	// never from a payload.
	key := event.StreamKey{Tenant: "admitted", Loop: "loop-e2e"}
	store, err := fsstore.NewFSEventStore(t.TempDir(), "sess-e2e")
	if err != nil {
		t.Fatalf("NewFSEventStore: %v", err)
	}
	defer func() { _ = store.Close() }()

	// The production pool over the production service, with the production
	// emitter installed. A short idle TTL keeps a leaked warm entry from
	// outliving the test; the reap we assert on is the deterministic Close path.
	pool := sandbox.NewPool(svc,
		sandbox.WithPoolIdleTTL(time.Minute),
		sandbox.WithPoolHooks(SandboxEventHooks(store, "node-root")),
	)

	principal := loopauth.Principal{Tenant: "t-e2e", Subject: "s-e2e"}

	// 1. COLD acquire: no warm entry exists, so this spawns a real container and
	//    the hook reports Reused=false with a measured cold start.
	r1, err := pool.Acquire(ctx, principal)
	if err != nil {
		t.Fatalf("cold Acquire: %v", err)
	}
	if out, err := r1.Exec(ctx, "echo hi", ""); err != nil || out.ExitCode != 0 {
		t.Fatalf("Exec(echo hi): err=%v exit=%d out=%q", err, out.ExitCode, out.Combined)
	}
	// 2. RELEASE it back to the pool (cause "released"): the container stays warm.
	if err := pool.Release(ctx, principal, r1, sandbox.CauseReleased); err != nil {
		t.Fatalf("Release: %v", err)
	}
	// 3. WARM acquire: the same principal checks out the warm container, so the
	//    hook reports Reused=true and no cold start.
	r2, err := pool.Acquire(ctx, principal)
	if err != nil {
		t.Fatalf("warm Acquire: %v", err)
	}
	if err := pool.Release(ctx, principal, r2, sandbox.CauseReleased); err != nil {
		t.Fatalf("second Release: %v", err)
	}
	// 4. REAP: Close tears the warm container down as the pool taking it away
	//    (cause "loop_end") — a KindSandboxReap, distinct from a release.
	if err := pool.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// The operator's shipper reads durable history; so do we. These are the loop's
	// OWN emitted events, marshalled and round-tripped through events.jsonl — not
	// hand-built records.
	evs, err := store.Replay(0)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(evs) == 0 {
		t.Fatal("no sandbox events on the durable stream — the pool's hooks never reached the emitter")
	}

	// Project each event through the PRODUCTION projection into the PRODUCTION
	// recorder. Nothing here is a test double: ProjectEvent and Recorder.Project
	// are what the live pipeline runs.
	rec := newAdmittedRecorder(t)
	for _, e := range evs {
		if err := rec.Project(ctx, observe.ProjectEvent(key, e)); err != nil {
			t.Fatalf("Project(%s): %v", e.Kind, err)
		}
	}

	body := scrapeMetrics(t, rec)

	// The families a real container lifecycle MUST have moved. Handler and runtime
	// are the real ones the container handler reported (container / docker|nerdctl|
	// podman), read back off the scrape rather than assumed.
	runtime := svc.Runtime()
	wantMetrics(t, body,
		// A cold spawn and a warm reuse each landed on acquire_total.
		`fuse_sandbox_acquire_total{reused="false",tenant_id="admitted"} 1`,
		`fuse_sandbox_acquire_total{reused="true",tenant_id="admitted"} 1`,
		// Exactly the cold spawn was timed — a warm reuse has no start to measure.
		`fuse_sandbox_cold_start_seconds_count{handler="container",runtime="`+runtime+`",tenant_id="admitted"} 1`,
		// The Close reap fired as loop_end, the pool taking the container away.
		`fuse_sandbox_reaped_total{cause="loop_end",handler="container",tenant_id="admitted"} 1`,
	)

	// The active gauge is the leak guard: every acquire raised it and every
	// hand-back/reap must have lowered it. After Close, nothing is held, so the
	// series must read exactly 0 — a non-zero here is a real gauge leak, the very
	// failure sandbox_metrics_test.go asserts against but only with synthetic
	// records. Here a real container proves it.
	wantMetrics(t, body,
		`fuse_sandbox_active{handler="container",runtime="`+runtime+`",tenant_id="admitted"} 0`,
	)

	// KindSandboxHealth has no emitter in the running system (recorded gap in the
	// 0063 close-out): fuse_sandbox_unhealthy_total is defined and its projection
	// is unit-tested, but no lifecycle path produces the event, so a real run can
	// never move it. Pin that here so the day an emitter is added, this guard
	// fails and forces the E2E assertion to be strengthened rather than silently
	// leaving the family unproven.
	if strings.Contains(body, "fuse_sandbox_unhealthy_total{") {
		t.Fatalf("fuse_sandbox_unhealthy_total gained a series from a real run — an emitter now exists; extend this test to drive an unhealthy transition and assert it end-to-end:\n%s", body)
	}
}

// newAdmittedRecorder builds the production recorder with a policy that admits
// the "admitted" tenant — the same shape prometheus/recorder_test.go's
// testRecorder uses, rebuilt here because that helper is package-private.
func newAdmittedRecorder(t *testing.T) *prometheus.Recorder {
	t.Helper()
	p, err := metricspolicy.New(metricspolicy.Config{
		HashVersion: metricspolicy.HashV1,
		Dimensions: map[metricspolicy.Dimension]metricspolicy.DimensionConfig{
			metricspolicy.TenantID: {Budget: 2, Catalog: []string{"admitted"}},
			metricspolicy.Model:    {Budget: 2, Catalog: []string{"m"}},
			metricspolicy.Tool:     {Budget: 2, Catalog: []string{"tool"}},
		},
	})
	if err != nil {
		t.Fatalf("metricspolicy.New: %v", err)
	}
	rec, err := prometheus.New(prometheus.Config{Registerer: client.NewRegistry(), Policy: p})
	if err != nil {
		t.Fatalf("prometheus.New: %v", err)
	}
	return rec
}

// scrapeMetrics drives the recorder's real /metrics handler and returns the
// exposition text — the same bytes Prometheus would pull.
func scrapeMetrics(t *testing.T, rec *prometheus.Recorder) string {
	t.Helper()
	w := httptest.NewRecorder()
	rec.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("/metrics returned %d, want 200", w.Code)
	}
	return w.Body.String()
}

func wantMetrics(t *testing.T, body string, want ...string) {
	t.Helper()
	for _, s := range want {
		if !strings.Contains(body, s) {
			t.Errorf("missing exposition line:\n\t%s\nin /metrics scrape:\n%s", s, body)
		}
	}
}

func containerRuntimeAvailable() bool {
	for _, cli := range containerCLIsForE2E {
		if _, err := exec.LookPath(cli); err == nil {
			return true
		}
	}
	return false
}
