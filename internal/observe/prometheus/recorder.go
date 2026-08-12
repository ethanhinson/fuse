// Package prometheus projects safe Fuse observations into a fixed Prometheus catalog.
package prometheus

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/observe"
	"github.com/ethanhinson/fuse/internal/observe/metricspolicy"
	client "github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type Descriptor struct {
	Name, Type string
	Labels     []string
}

var catalog = []Descriptor{
	{"fuse_loop_operations_total", "counter", []string{"tenant_id", "operation", "outcome"}}, {"fuse_loop_operation_duration_seconds", "histogram", []string{"tenant_id", "operation", "outcome"}}, {"fuse_loop_active", "gauge", []string{"tenant_id", "operation"}},
	{"fuse_model_calls_total", "counter", []string{"tenant_id", "model", "outcome"}}, {"fuse_model_call_duration_seconds", "histogram", []string{"tenant_id", "model", "outcome"}}, {"fuse_model_call_attempts", "histogram", []string{"tenant_id", "model", "outcome"}},
	{"fuse_tool_calls_total", "counter", []string{"tenant_id", "tool", "outcome"}}, {"fuse_tool_call_duration_seconds", "histogram", []string{"tenant_id", "tool", "outcome"}}, {"fuse_spawn_operations_total", "counter", []string{"tenant_id", "outcome"}}, {"fuse_spawn_operation_duration_seconds", "histogram", []string{"tenant_id", "outcome"}},
	{"fuse_event_projection_total", "counter", []string{"event_kind", "outcome"}}, {"fuse_event_projection_duration_seconds", "histogram", []string{"outcome"}}, {"fuse_observability_export_errors_total", "counter", []string{"signal", "reason"}}, {"fuse_observability_dropped_total", "counter", []string{"signal", "reason"}}, {"fuse_observability_log_reopens_total", "counter", []string{"outcome"}}, {"fuse_observability_log_overrides", "gauge", []string{"scope"}},
	{"fuse_metrics_label_overflow_total", "counter", []string{"dimension", "metric"}}, {"fuse_metrics_label_admitted_values", "gauge", []string{"dimension"}}, {"fuse_metrics_label_budget", "gauge", []string{"dimension"}},
}

func Catalog() []Descriptor { out := make([]Descriptor, len(catalog)); copy(out, catalog); return out }

var forbidden = map[string]bool{"loop_id": true, "node_id": true, "sequence": true, "trace_id": true, "span_id": true, "tenant_name": true, "display_name": true, "payload": true, "error": true, "url": true, "path": true}

func forbiddenLabel(label string) bool { return forbidden[label] }
func ValidateLabels(name string, labels []string) error {
	for _, d := range catalog {
		if d.Name == name {
			if len(labels) != len(d.Labels) {
				return fmt.Errorf("labels for %s do not match declaration", name)
			}
			for i := range labels {
				if labels[i] != d.Labels[i] || forbiddenLabel(labels[i]) {
					return fmt.Errorf("label %q is undeclared or forbidden", labels[i])
				}
			}
			return nil
		}
	}
	return fmt.Errorf("undeclared metric %q", name)
}

type Config struct {
	Registerer       client.Registerer
	Gatherer         client.Gatherer
	Policy           *metricspolicy.Policy
	HistogramBuckets []float64
}
type Recorder struct {
	policy                                                                                                  *metricspolicy.Policy
	gatherer                                                                                                client.Gatherer
	loopTotal, modelTotal, toolTotal, spawnTotal, projectionTotal, exportErrors, dropped, reopens, overflow *client.CounterVec
	loopActive, overrides, admitted, budget                                                                 *client.GaugeVec
	loopDuration, modelDuration, attempts, toolDuration, spawnDuration, projectionDuration                  *client.HistogramVec
	mu                                                                                                      sync.Mutex
	active                                                                                                  map[string]int
}

func New(cfg Config) (*Recorder, error) {
	if cfg.Registerer == nil {
		reg := client.NewRegistry()
		cfg.Registerer, cfg.Gatherer = reg, reg
	}
	if cfg.Gatherer == nil {
		if g, ok := cfg.Registerer.(client.Gatherer); ok {
			cfg.Gatherer = g
		} else {
			return nil, fmt.Errorf("gatherer required")
		}
	}
	if cfg.Policy == nil {
		return nil, fmt.Errorf("cardinality policy required")
	}
	b := cfg.HistogramBuckets
	if len(b) == 0 {
		b = client.DefBuckets
	}
	r := &Recorder{policy: cfg.Policy, gatherer: cfg.Gatherer, active: map[string]int{}}
	r.loopTotal = client.NewCounterVec(client.CounterOpts{Name: "fuse_loop_operations_total", Help: "Completed loop operations."}, []string{"tenant_id", "operation", "outcome"})
	r.loopDuration = client.NewHistogramVec(client.HistogramOpts{Name: "fuse_loop_operation_duration_seconds", Help: "Loop operation duration.", Buckets: b}, []string{"tenant_id", "operation", "outcome"})
	r.loopActive = client.NewGaugeVec(client.GaugeOpts{Name: "fuse_loop_active", Help: "Active loop operations."}, []string{"tenant_id", "operation"})
	r.modelTotal = client.NewCounterVec(client.CounterOpts{Name: "fuse_model_calls_total", Help: "Model calls."}, []string{"tenant_id", "model", "outcome"})
	r.modelDuration = client.NewHistogramVec(client.HistogramOpts{Name: "fuse_model_call_duration_seconds", Help: "Model call duration.", Buckets: b}, []string{"tenant_id", "model", "outcome"})
	r.attempts = client.NewHistogramVec(client.HistogramOpts{Name: "fuse_model_call_attempts", Help: "Model call attempts.", Buckets: []float64{1, 2, 3, 4, 5, 8}}, []string{"tenant_id", "model", "outcome"})
	r.toolTotal = client.NewCounterVec(client.CounterOpts{Name: "fuse_tool_calls_total", Help: "Tool calls."}, []string{"tenant_id", "tool", "outcome"})
	r.toolDuration = client.NewHistogramVec(client.HistogramOpts{Name: "fuse_tool_call_duration_seconds", Help: "Tool call duration.", Buckets: b}, []string{"tenant_id", "tool", "outcome"})
	r.spawnTotal = client.NewCounterVec(client.CounterOpts{Name: "fuse_spawn_operations_total", Help: "Spawn operations."}, []string{"tenant_id", "outcome"})
	r.spawnDuration = client.NewHistogramVec(client.HistogramOpts{Name: "fuse_spawn_operation_duration_seconds", Help: "Spawn operation duration.", Buckets: b}, []string{"tenant_id", "outcome"})
	r.projectionTotal = client.NewCounterVec(client.CounterOpts{Name: "fuse_event_projection_total", Help: "Event projections."}, []string{"event_kind", "outcome"})
	r.projectionDuration = client.NewHistogramVec(client.HistogramOpts{Name: "fuse_event_projection_duration_seconds", Help: "Projection duration.", Buckets: b}, []string{"outcome"})
	r.exportErrors = client.NewCounterVec(client.CounterOpts{Name: "fuse_observability_export_errors_total", Help: "Export errors."}, []string{"signal", "reason"})
	r.dropped = client.NewCounterVec(client.CounterOpts{Name: "fuse_observability_dropped_total", Help: "Dropped observations."}, []string{"signal", "reason"})
	r.reopens = client.NewCounterVec(client.CounterOpts{Name: "fuse_observability_log_reopens_total", Help: "Log reopens."}, []string{"outcome"})
	r.overrides = client.NewGaugeVec(client.GaugeOpts{Name: "fuse_observability_log_overrides", Help: "Active log overrides."}, []string{"scope"})
	r.overflow = client.NewCounterVec(client.CounterOpts{Name: "fuse_metrics_label_overflow_total", Help: "Collapsed label observations."}, []string{"dimension", "metric"})
	r.admitted = client.NewGaugeVec(client.GaugeOpts{Name: "fuse_metrics_label_admitted_values", Help: "Admitted label values."}, []string{"dimension"})
	r.budget = client.NewGaugeVec(client.GaugeOpts{Name: "fuse_metrics_label_budget", Help: "Configured label budget."}, []string{"dimension"})
	collectors := []client.Collector{r.loopTotal, r.loopDuration, r.loopActive, r.modelTotal, r.modelDuration, r.attempts, r.toolTotal, r.toolDuration, r.spawnTotal, r.spawnDuration, r.projectionTotal, r.projectionDuration, r.exportErrors, r.dropped, r.reopens, r.overrides, r.overflow, r.admitted, r.budget}
	for _, c := range collectors {
		if err := cfg.Registerer.Register(c); err != nil {
			return nil, err
		}
	}
	for _, d := range []metricspolicy.Dimension{metricspolicy.TenantID, metricspolicy.Model, metricspolicy.Tool} {
		r.admitted.WithLabelValues(string(d)).Set(float64(cfg.Policy.AdmittedCount(d)))
		r.budget.WithLabelValues(string(d)).Set(float64(cfg.Policy.Budget(d)))
	}
	return r, nil
}
func (r *Recorder) Handler() http.Handler {
	return promhttp.HandlerFor(r.gatherer, promhttp.HandlerOpts{})
}
func (r *Recorder) label(d metricspolicy.Dimension, v, metric string) string {
	mapped := r.policy.Map(d, v)
	if mapped == metricspolicy.Overflow {
		r.overflow.WithLabelValues(string(d), metric).Inc()
	}
	return mapped
}
func (r *Recorder) Project(_ context.Context, rec observe.Record) error {
	started := time.Now()
	outcome := normalizedOutcome(rec.Outcome)
	if outcome == "" {
		outcome = "success"
	}
	r.projectionTotal.WithLabelValues(rec.EventName, "success").Inc()
	r.projectionDuration.WithLabelValues("success").Observe(time.Since(started).Seconds())
	if rec.EventName == string(event.KindTurnStart) {
		tenant := r.label(metricspolicy.TenantID, string(rec.TenantID), "fuse_loop_active")
		key := tenant + "\x00" + string(rec.Operation)
		r.mu.Lock()
		r.active[key]++
		n := r.active[key]
		r.mu.Unlock()
		r.loopActive.WithLabelValues(tenant, string(rec.Operation)).Set(float64(n))
		return nil
	}
	if rec.EventName == string(event.KindTurnEnd) || rec.EventName == string(event.KindLoopParked) || rec.EventName == string(event.KindError) {
		tenant := r.label(metricspolicy.TenantID, string(rec.TenantID), "fuse_loop_operations_total")
		r.loopTotal.WithLabelValues(tenant, string(rec.Operation), outcome).Inc()
		activeTenant := r.label(metricspolicy.TenantID, string(rec.TenantID), "fuse_loop_active")
		key := activeTenant + "\x00" + string(rec.Operation)
		r.mu.Lock()
		if r.active[key] > 0 {
			r.active[key]--
		}
		n := r.active[key]
		if n == 0 {
			delete(r.active, key)
		}
		r.mu.Unlock()
		r.loopActive.WithLabelValues(activeTenant, string(rec.Operation)).Set(float64(n))
	}
	if rec.EventName == string(event.KindModelCallEnd) {
		tenant := r.label(metricspolicy.TenantID, string(rec.TenantID), "fuse_model_calls_total")
		model := r.label(metricspolicy.Model, "", "fuse_model_calls_total")
		r.modelTotal.WithLabelValues(tenant, model, outcome).Inc()
	}
	if rec.EventName == string(event.KindToolResult) {
		tenant := r.label(metricspolicy.TenantID, string(rec.TenantID), "fuse_tool_calls_total")
		tool := r.label(metricspolicy.Tool, "", "fuse_tool_calls_total")
		r.toolTotal.WithLabelValues(tenant, tool, outcome).Inc()
	}
	if rec.EventName == string(event.KindSpawnDone) {
		tenant := r.label(metricspolicy.TenantID, string(rec.TenantID), "fuse_spawn_operations_total")
		r.spawnTotal.WithLabelValues(tenant, outcome).Inc()
	}
	return nil
}

func normalizedOutcome(outcome observe.Outcome) string {
	if outcome == observe.OutcomeCanceled {
		return "cancelled"
	}
	return string(outcome)
}
func (r *Recorder) ObserveModel(tenant, model string, outcome observe.Outcome, d time.Duration, attempts int) {
	o := normalizedOutcome(outcome)
	labels := func(metric string) (string, string) {
		return r.label(metricspolicy.TenantID, tenant, metric), r.label(metricspolicy.Model, model, metric)
	}
	t, m := labels("fuse_model_calls_total")
	r.modelTotal.WithLabelValues(t, m, o).Inc()
	t, m = labels("fuse_model_call_duration_seconds")
	r.modelDuration.WithLabelValues(t, m, o).Observe(d.Seconds())
	t, m = labels("fuse_model_call_attempts")
	r.attempts.WithLabelValues(t, m, o).Observe(float64(attempts))
}
func (r *Recorder) ObserveTool(tenant, tool string, outcome observe.Outcome, d time.Duration) {
	o := normalizedOutcome(outcome)
	t := r.label(metricspolicy.TenantID, tenant, "fuse_tool_calls_total")
	v := r.label(metricspolicy.Tool, tool, "fuse_tool_calls_total")
	r.toolTotal.WithLabelValues(t, v, o).Inc()
	t = r.label(metricspolicy.TenantID, tenant, "fuse_tool_call_duration_seconds")
	v = r.label(metricspolicy.Tool, tool, "fuse_tool_call_duration_seconds")
	r.toolDuration.WithLabelValues(t, v, o).Observe(d.Seconds())
}
