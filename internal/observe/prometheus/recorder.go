// Package prometheus projects safe Fuse observations into a fixed Prometheus catalog.
package prometheus

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

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
	{"fuse_permission_decisions_total", "counter", []string{"tenant_id", "verdict", "layer"}}, {"fuse_permission_classifier_replies_total", "counter", []string{"tenant_id", "outcome"}},
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
	decisions, classifierReplies                                                                            *client.CounterVec
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
	r.decisions = client.NewCounterVec(client.CounterOpts{Name: "fuse_permission_decisions_total", Help: "Permission-gate decisions by verdict and deciding layer."}, []string{"tenant_id", "verdict", "layer"})
	r.classifierReplies = client.NewCounterVec(client.CounterOpts{Name: "fuse_permission_classifier_replies_total", Help: "Auto-mode classifier replies by health outcome (ok, truncated, parse_error, cached)."}, []string{"tenant_id", "outcome"})
	r.overflow = client.NewCounterVec(client.CounterOpts{Name: "fuse_metrics_label_overflow_total", Help: "Collapsed label observations."}, []string{"dimension", "metric"})
	r.admitted = client.NewGaugeVec(client.GaugeOpts{Name: "fuse_metrics_label_admitted_values", Help: "Admitted non-overflow label values."}, []string{"dimension"})
	r.budget = client.NewGaugeVec(client.GaugeOpts{Name: "fuse_metrics_label_budget", Help: "Configured label cardinality limit, including __overflow__."}, []string{"dimension"})
	collectors := []client.Collector{r.loopTotal, r.loopDuration, r.loopActive, r.modelTotal, r.modelDuration, r.attempts, r.toolTotal, r.toolDuration, r.spawnTotal, r.spawnDuration, r.projectionTotal, r.projectionDuration, r.exportErrors, r.dropped, r.reopens, r.overrides, r.overflow, r.admitted, r.budget, r.decisions, r.classifierReplies}
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
	r.projectionTotal.WithLabelValues(rec.EventName, "success").Inc()
	// Permission decisions are counted from the projection rather than the
	// Observe* path deliberately: verdict and layer are bounded enums the event
	// payload faithfully carries, and counters (unlike durations) lose nothing
	// by being event-derived — so every binding that projects its event stream
	// gets decision metrics with no extra loop instrumentation.
	if rec.Verdict != "" && rec.DecisionLayer != "" {
		t := r.label(metricspolicy.TenantID, string(rec.TenantID), "fuse_permission_decisions_total")
		r.decisions.WithLabelValues(t, rec.Verdict, rec.DecisionLayer).Inc()
	}
	if rec.ClassifierOutcome != "" {
		t := r.label(metricspolicy.TenantID, string(rec.TenantID), "fuse_permission_classifier_replies_total")
		r.classifierReplies.WithLabelValues(t, rec.ClassifierOutcome).Inc()
	}
	r.projectionDuration.WithLabelValues("success").Observe(time.Since(started).Seconds())
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

// ObserveOperation is the authoritative path for operation metrics. Event
// projection intentionally records only projection health: durable events do
// not retain an operation's duration, model/tool identity, or terminal result.
func (r *Recorder) ObserveOperation(tenant string, d observe.Descriptor) observe.Handle {
	operation := string(d.Kind)
	mappedTenant := r.label(metricspolicy.TenantID, tenant, "fuse_loop_active")
	key := mappedTenant + "\x00" + operation
	r.mu.Lock()
	r.active[key]++
	active := r.active[key]
	r.mu.Unlock()
	r.loopActive.WithLabelValues(mappedTenant, operation).Set(float64(active))
	return &operationHandle{recorder: r, tenant: tenant, descriptor: d, started: time.Now(), activeKey: key, activeTenant: mappedTenant}
}

type operationHandle struct {
	recorder     *Recorder
	tenant       string
	descriptor   observe.Descriptor
	started      time.Time
	activeKey    string
	activeTenant string
	once         sync.Once
}

func (h *operationHandle) End(outcome observe.Outcome, _ ...observe.Field) {
	if h == nil || h.recorder == nil {
		return
	}
	h.once.Do(func() {
		r := h.recorder
		r.mu.Lock()
		if r.active[h.activeKey] > 1 {
			r.active[h.activeKey]--
		} else {
			delete(r.active, h.activeKey)
		}
		active := r.active[h.activeKey]
		r.mu.Unlock()
		r.loopActive.WithLabelValues(h.activeTenant, string(h.descriptor.Kind)).Set(float64(active))
		o := normalizedOutcome(outcome)
		if o == "" {
			o = string(observe.OutcomeSuccess)
		}
		operation := string(h.descriptor.Kind)
		tenant := r.label(metricspolicy.TenantID, h.tenant, "fuse_loop_operations_total")
		r.loopTotal.WithLabelValues(tenant, operation, o).Inc()
		tenant = r.label(metricspolicy.TenantID, h.tenant, "fuse_loop_operation_duration_seconds")
		r.loopDuration.WithLabelValues(tenant, operation, o).Observe(time.Since(h.started).Seconds())

		switch h.descriptor.Kind {
		case observe.OperationModelAttempt:
			r.ObserveModel(h.tenant, field(h.descriptor.Fields, "model"), observe.Outcome(o), time.Since(h.started), 1)
		case observe.OperationTool:
			r.ObserveTool(h.tenant, field(h.descriptor.Fields, "tool"), observe.Outcome(o), time.Since(h.started))
		case observe.OperationSpawn:
			tenant = r.label(metricspolicy.TenantID, h.tenant, "fuse_spawn_operations_total")
			r.spawnTotal.WithLabelValues(tenant, o).Inc()
			tenant = r.label(metricspolicy.TenantID, h.tenant, "fuse_spawn_operation_duration_seconds")
			r.spawnDuration.WithLabelValues(tenant, o).Observe(time.Since(h.started).Seconds())
		}
	})
}

func field(fields []observe.Field, key string) string {
	for _, f := range fields {
		if f.Key == key {
			return f.Value
		}
	}
	return ""
}

// ObserveExportError and ObserveDrop expose bounded health signals from the
// asynchronous projection path; neither operation is allowed to affect loops.
func (r *Recorder) ObserveExportError(signal, reason string) {
	r.exportErrors.WithLabelValues(signal, reason).Inc()
}
func (r *Recorder) ObserveDrop(signal, reason string) {
	r.dropped.WithLabelValues(signal, reason).Inc()
}
func (r *Recorder) ObserveReopen(outcome string) { r.reopens.WithLabelValues(outcome).Inc() }
func (r *Recorder) SetOverrides(scope string, value float64) {
	r.overrides.WithLabelValues(scope).Set(value)
}
