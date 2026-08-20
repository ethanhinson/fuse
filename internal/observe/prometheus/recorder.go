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
	// Sandbox lifecycle (change 0063). Every label is a closed enum collapsed to
	// __overflow__ on anything else: the values arrive from a wire payload, so a
	// buggy or hostile writer must never be able to mint a series. The container
	// id is deliberately absent — it is a correlation token for logs and traces,
	// not a metric dimension — and so are the command and its environment.
	// loop/node stay off the histogram for the same cardinality reason.
	{"fuse_sandbox_active", "gauge", []string{"tenant_id", "handler", "runtime"}}, {"fuse_sandbox_acquire_total", "counter", []string{"tenant_id", "reused"}}, {"fuse_sandbox_cold_start_seconds", "histogram", []string{"tenant_id", "handler", "runtime"}}, {"fuse_sandbox_unhealthy_total", "counter", []string{"tenant_id", "handler", "reason"}}, {"fuse_sandbox_reaped_total", "counter", []string{"tenant_id", "handler", "cause"}},
}

func Catalog() []Descriptor { out := make([]Descriptor, len(catalog)); copy(out, catalog); return out }

var forbidden = map[string]bool{"loop_id": true, "node_id": true, "sequence": true, "trace_id": true, "span_id": true, "tenant_name": true, "display_name": true, "payload": true, "error": true, "url": true, "path": true, "container_id": true, "command": true, "env": true}

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
	sandboxAcquire, sandboxUnhealthy, sandboxReaped                                                         *client.CounterVec
	loopActive, overrides, admitted, budget, sandboxActive                                                  *client.GaugeVec
	loopDuration, modelDuration, attempts, toolDuration, spawnDuration, projectionDuration                  *client.HistogramVec
	sandboxColdStart                                                                                        *client.HistogramVec
	mu                                                                                                      sync.Mutex
	active                                                                                                  map[string]int
	// sandboxLive counts live Runners per fuse_sandbox_active series, and
	// sandboxHeld remembers the series each checkout was acquired on. Both are
	// bounded by the number of contexts actually checked out — the very quantity
	// the gauge reports — so neither can outgrow it.
	sandboxLive map[string]int
	sandboxHeld map[string]*sandboxCheckout
}

// sandboxCheckout carries an acquire's series forward to its release or reap.
// The release/reap payload deliberately carries no runtime (change 0063), so
// decrementing from the payload alone would target a DIFFERENT series than the
// acquire incremented and the gauge could never return to zero. Recovering the
// acquire's labels here fixes that without widening the wire payload.
type sandboxCheckout struct {
	tenant, handler, runtime string
	count                    int
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
	r := &Recorder{policy: cfg.Policy, gatherer: cfg.Gatherer, active: map[string]int{}, sandboxLive: map[string]int{}, sandboxHeld: map[string]*sandboxCheckout{}}
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
	r.sandboxActive = client.NewGaugeVec(client.GaugeOpts{Name: "fuse_sandbox_active", Help: "Live sandbox execution contexts by handler and container runtime — what is running where."}, []string{"tenant_id", "handler", "runtime"})
	r.sandboxAcquire = client.NewCounterVec(client.CounterOpts{Name: "fuse_sandbox_acquire_total", Help: "Sandbox checkouts by whether a warm context was reused — pool effectiveness."}, []string{"tenant_id", "reused"})
	r.sandboxColdStart = client.NewHistogramVec(client.HistogramOpts{Name: "fuse_sandbox_cold_start_seconds", Help: "Cold-start latency of a newly spawned sandbox. Observed only on a cold spawn: a warm reuse has no start to measure.", Buckets: b}, []string{"tenant_id", "handler", "runtime"})
	r.sandboxUnhealthy = client.NewCounterVec(client.CounterOpts{Name: "fuse_sandbox_unhealthy_total", Help: "Sandbox health transitions INTO unhealthy, by bounded reason (oom, runtime_exit, pull_failed, acquire_failed, unresponsive)."}, []string{"tenant_id", "handler", "reason"})
	r.sandboxReaped = client.NewCounterVec(client.CounterOpts{Name: "fuse_sandbox_reaped_total", Help: "Execution contexts that stopped being held, by bounded cause (released, loop_end, early_return, idle_ttl, stale_checkout). idle_ttl is the leak signal; stale_checkout firing at all means a pool invariant was violated."}, []string{"tenant_id", "handler", "cause"})
	r.overflow = client.NewCounterVec(client.CounterOpts{Name: "fuse_metrics_label_overflow_total", Help: "Collapsed label observations."}, []string{"dimension", "metric"})
	r.admitted = client.NewGaugeVec(client.GaugeOpts{Name: "fuse_metrics_label_admitted_values", Help: "Admitted non-overflow label values."}, []string{"dimension"})
	r.budget = client.NewGaugeVec(client.GaugeOpts{Name: "fuse_metrics_label_budget", Help: "Configured label cardinality limit, including __overflow__."}, []string{"dimension"})
	collectors := []client.Collector{r.loopTotal, r.loopDuration, r.loopActive, r.modelTotal, r.modelDuration, r.attempts, r.toolTotal, r.toolDuration, r.spawnTotal, r.spawnDuration, r.projectionTotal, r.projectionDuration, r.exportErrors, r.dropped, r.reopens, r.overrides, r.overflow, r.admitted, r.budget, r.decisions, r.classifierReplies, r.sandboxActive, r.sandboxAcquire, r.sandboxColdStart, r.sandboxUnhealthy, r.sandboxReaped}
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
	if rec.Operation == observe.OperationSandbox {
		r.recordSandbox(rec)
	}
	r.projectionDuration.WithLabelValues("success").Observe(time.Since(started).Seconds())
	return nil
}

// The sandbox label vocabularies. Each is closed at the wire contract
// (event.SandboxAcquirePayload, event.SandboxCause, event.SandboxHealthPayload)
// and pinned here rather than imported so that widening the wire enum is a
// deliberate, reviewed edit to the metric catalog too. Anything else collapses
// to __overflow__: these values come off a payload, and an unrecognized one is
// far likelier to be a bug or an injection than a series worth minting.
var (
	sandboxHandlers      = map[string]bool{"container": true, "host": true, "microvm": true}
	sandboxRuntimes      = map[string]bool{"docker": true, "nerdctl": true, "podman": true}
	sandboxCauses        = map[string]bool{"released": true, "loop_end": true, "early_return": true, "idle_ttl": true, "stale_checkout": true}
	sandboxHealthReasons = map[string]bool{"oom": true, "runtime_exit": true, "pull_failed": true, "acquire_failed": true, "unresponsive": true, "recovered": true}
)

// sandboxRuntimeNone is the explicit label for a handler that has no container
// runtime at all — the host handler. It is a real, bounded fact and is kept
// distinct from __overflow__ (an unrecognized value) and from "" (which would
// read as "unknown" and silently aggregate unrelated series together).
const sandboxRuntimeNone = "none"

func boundedLabel(allowed map[string]bool, value string) string {
	if allowed[value] {
		return value
	}
	return metricspolicy.Overflow
}

func sandboxRuntimeLabel(runtime string) string {
	if runtime == "" {
		return sandboxRuntimeNone
	}
	return boundedLabel(sandboxRuntimes, runtime)
}

// recordSandbox drives the sandbox families off a projected Record.
//
// Every branch keys off the event name, not off a field's zero value: Reused is
// a plain bool, so treating false as "cold spawn" without first confirming this
// is an acquire would count every unrelated event as a container start.
func (r *Recorder) recordSandbox(rec observe.Record) {
	tenant := r.label(metricspolicy.TenantID, string(rec.TenantID), "fuse_sandbox_active")
	handler := boundedLabel(sandboxHandlers, rec.Handler)

	switch rec.EventName {
	case "sandbox.acquire":
		runtime := sandboxRuntimeLabel(rec.Runtime)
		reused := "false"
		if rec.Reused {
			reused = "true"
		}
		r.sandboxAcquire.WithLabelValues(tenant, reused).Inc()
		if !rec.Reused {
			// Only a cold spawn has a start to measure; observing 0 for a warm
			// reuse would drag the latency distribution toward a lie.
			r.sandboxColdStart.WithLabelValues(tenant, handler, runtime).Observe(float64(rec.ColdStartMS) / 1000)
		}
		r.sandboxHold(rec.ContainerID, tenant, handler, runtime)
	case "sandbox.release", "sandbox.reap":
		cause := boundedLabel(sandboxCauses, rec.Cause)
		if rec.EventName == "sandbox.reap" {
			r.sandboxReaped.WithLabelValues(tenant, handler, cause).Inc()
		}
		// Release AND reap both end a checkout. A reap that did not decrement
		// would leak the gauge upward forever, which is exactly the leak this
		// family exists to expose.
		r.sandboxRelease(rec.ContainerID, handler)
	case "sandbox.health":
		// Only a transition INTO unhealthy counts. The projector already decided
		// that (a healthy or recovered transition projects success), so reading
		// the outcome avoids re-deriving health from the reason enum here.
		if rec.Outcome == observe.OutcomeError {
			r.sandboxUnhealthy.WithLabelValues(tenant, handler, boundedLabel(sandboxHealthReasons, rec.Reason)).Inc()
		}
	}
}

// sandboxKey identifies a checkout for carry-forward. The container id is used
// ONLY as this internal correlation key and never reaches a label. The handler
// is folded in so the host handler — which has no container id at all — groups
// its checkouts under its own refcount instead of colliding with anything else
// that reports an empty id.
func sandboxKey(containerID, handler string) string { return handler + "\x00" + containerID }

func (r *Recorder) sandboxHold(containerID, tenant, handler, runtime string) {
	key := sandboxKey(containerID, handler)
	series := tenant + "\x00" + handler + "\x00" + runtime
	r.mu.Lock()
	held, ok := r.sandboxHeld[key]
	if !ok {
		held = &sandboxCheckout{tenant: tenant, handler: handler, runtime: runtime}
		r.sandboxHeld[key] = held
	}
	held.count++
	r.sandboxLive[series]++
	live := r.sandboxLive[series]
	r.mu.Unlock()
	r.sandboxActive.WithLabelValues(tenant, handler, runtime).Set(float64(live))
}

func (r *Recorder) sandboxRelease(containerID, handler string) {
	key := sandboxKey(containerID, handler)
	r.mu.Lock()
	held, ok := r.sandboxHeld[key]
	if !ok {
		// A checkout this recorder never saw acquired — a process that started
		// mid-stream, or a replay from an offset. Decrementing would drive the
		// gauge negative and misreport a leak as negative capacity, so the
		// counter families still record it and the gauge stays untouched.
		r.mu.Unlock()
		return
	}
	series := held.tenant + "\x00" + held.handler + "\x00" + held.runtime
	held.count--
	if held.count <= 0 {
		delete(r.sandboxHeld, key)
	}
	if r.sandboxLive[series] > 1 {
		r.sandboxLive[series]--
	} else {
		delete(r.sandboxLive, series)
	}
	live := r.sandboxLive[series]
	tenant, h, runtime := held.tenant, held.handler, held.runtime
	r.mu.Unlock()
	r.sandboxActive.WithLabelValues(tenant, h, runtime).Set(float64(live))
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
