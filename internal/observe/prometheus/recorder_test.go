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

func testRecorder(t *testing.T) *Recorder {
	t.Helper()
	p, err := metricspolicy.New(metricspolicy.Config{HashVersion: metricspolicy.HashV1, Dimensions: map[metricspolicy.Dimension]metricspolicy.DimensionConfig{
		metricspolicy.TenantID: {Budget: 1, Catalog: []string{"admitted"}}, metricspolicy.Model: {Budget: 1, Catalog: []string{"m"}}, metricspolicy.Tool: {Budget: 1, Catalog: []string{"tool"}},
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

func TestCatalogLabelsAreExactAndSafe(t *testing.T) {
	for _, d := range Catalog() {
		for _, label := range d.Labels {
			if forbiddenLabel(label) {
				t.Fatalf("%s has forbidden label %q", d.Name, label)
			}
		}
	}
	if err := ValidateLabels("fuse_loop_active", []string{"tenant_id", "operation", "loop_id"}); err == nil {
		t.Fatal("undeclared label accepted")
	}
	for _, bad := range []string{"loop_id", "node_id", "sequence", "trace_id", "span_id", "tenant_name", "payload", "error", "url", "path"} {
		if !forbiddenLabel(bad) {
			t.Fatalf("%q not forbidden", bad)
		}
	}
	if len(Catalog()) != 19 {
		t.Fatalf("catalog size=%d", len(Catalog()))
	}
}

func TestProjectionActiveCleanupOverflowAndExposition(t *testing.T) {
	r := testRecorder(t)
	ctx := context.Background()
	if err := r.Project(ctx, observe.Record{Timestamp: time.Now(), EventName: string(event.KindTurnStart)}); err != nil {
		t.Fatal(err)
	}
	loop := r.ObserveOperation("unknown", observe.Descriptor{Kind: observe.OperationLoop})
	loop.End(observe.OutcomeSuccess)
	r.ObserveModel("admitted", "m", observe.OutcomeSuccess, 20*time.Millisecond, 2)
	r.ObserveTool("admitted", "unknown-tool", observe.OutcomeError, time.Millisecond)
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/metrics", nil)
	r.Handler().ServeHTTP(w, req)
	body := w.Body.String()
	for _, want := range []string{
		`fuse_loop_operations_total{operation="loop",outcome="success",tenant_id="__overflow__"} 1`,
		`fuse_loop_active{operation="loop",tenant_id="__overflow__"} 0`,
		`fuse_metrics_label_overflow_total{dimension="tenant_id",metric="fuse_loop_operations_total"} 1`,
		`fuse_metrics_label_admitted_values{dimension="tenant_id"} 1`,
		`fuse_metrics_label_budget{dimension="tenant_id"} 1`,
		`fuse_tool_calls_total{outcome="error",tenant_id="admitted",tool="__overflow__"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s\n%s", want, body)
		}
	}
}

func TestOperationObservationUsesTerminalDataAndDescriptorIdentity(t *testing.T) {
	r := testRecorder(t)
	h := r.ObserveOperation("admitted", observe.Descriptor{Kind: observe.OperationModelAttempt, Name: "complete", Fields: []observe.Field{{Key: "model", Value: "m"}}})
	time.Sleep(time.Millisecond)
	h.End(observe.OutcomeTimeout)

	w := httptest.NewRecorder()
	r.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	body := w.Body.String()
	for _, want := range []string{
		`fuse_loop_operations_total{operation="model.attempt",outcome="timeout",tenant_id="admitted"} 1`,
		`fuse_loop_operation_duration_seconds_count{operation="model.attempt",outcome="timeout",tenant_id="admitted"} 1`,
		`fuse_model_calls_total{model="m",outcome="timeout",tenant_id="admitted"} 1`,
		`fuse_model_call_duration_seconds_count{model="m",outcome="timeout",tenant_id="admitted"} 1`,
		`fuse_model_call_attempts_count{model="m",outcome="timeout",tenant_id="admitted"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s\n%s", want, body)
		}
	}
}
