package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/loopauth"
	"github.com/ethanhinson/fuse/internal/observe"
	observabilitylogging "github.com/ethanhinson/fuse/internal/observe/logging"
	"github.com/ethanhinson/fuse/internal/observe/metricspolicy"
	observeotel "github.com/ethanhinson/fuse/internal/observe/otel"
	observeprom "github.com/ethanhinson/fuse/internal/observe/prometheus"
	"github.com/ethanhinson/fuse/internal/version"
	"github.com/go-logr/logr/funcr"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
)

const (
	observabilityAdminPath  = "/-/observability/logging"
	observabilityReloadPath = "/-/observability/logging/reload"
	observabilityReopenPath = "/-/observability/logging/reopen"
)

type observabilityService struct {
	cfg             config.ObservabilityConfig
	observer        observe.Observer
	projector       observe.Projector
	projection      *projectionDispatcher
	metrics         *observeprom.Recorder
	levels          *observabilitylogging.Levels
	logger          *observabilitylogging.Logger
	provider        *sdktrace.TracerProvider
	metricsServer   *http.Server
	metricsListener net.Listener
	reloadMu        sync.Mutex
	closeOnce       sync.Once
	closeErr        error
}

// metricsObserver is the sole source of operation metrics. It keeps the primary
// observer (normally OTEL) authoritative for context propagation while recording
// metrics from the same start/end boundary, where duration, terminal outcome,
// and bounded model/tool identity are still available.
type metricsObserver struct {
	primary observe.Observer
	metrics *observeprom.Recorder
}

type metricsTenantKey struct{}

// DecorateScope resolves an operation's tenant into the context WITHOUT starting the
// operation (observe.ScopeDecorator). Start establishes the same scope, but only work
// that actually starts a span can: a RESUMED interactive session deliberately starts
// no second fuse.loop.run (its session root already ended and exported), so without
// this every model, tool, store and turn metric for the rest of that session's life
// would record tenant="" and silently mis-attribute change 0051's per-tenant series.
func (o metricsObserver) DecorateScope(ctx context.Context, d observe.Descriptor) context.Context {
	for _, f := range d.Fields {
		if f.Key == "tenant" {
			ctx = context.WithValue(ctx, metricsTenantKey{}, f.Value)
			break
		}
	}
	// This wrapper contributes only the tenant; a wrapped observer may carry its own.
	return observe.DecorateScope(o.primary, ctx, d)
}

func (o metricsObserver) Start(ctx context.Context, d observe.Descriptor) (context.Context, observe.Handle) {
	ctx = o.DecorateScope(ctx, d)
	ctx, primary := o.primary.Start(ctx, d)
	tenant, _ := ctx.Value(metricsTenantKey{}).(string)
	return ctx, joinedHandle{primary: primary, metrics: o.metrics.ObserveOperation(tenant, d)}
}

func (o metricsObserver) TraceCarrier(ctx context.Context) *event.TraceCarrier {
	return observe.TraceCarrier(o.primary, ctx)
}

func (o metricsObserver) StartFromCarrier(ctx context.Context, c *event.TraceCarrier, delayed bool, d observe.Descriptor) (context.Context, observe.Handle) {
	ctx, primary := observe.StartFromCarrier(o.primary, ctx, c, delayed, d)
	tenant, _ := ctx.Value(metricsTenantKey{}).(string)
	return ctx, joinedHandle{primary: primary, metrics: o.metrics.ObserveOperation(tenant, d)}
}

type joinedHandle struct{ primary, metrics observe.Handle }

func (h joinedHandle) End(outcome observe.Outcome, fields ...observe.Field) {
	if h.primary != nil {
		h.primary.End(outcome, fields...)
	}
	if h.metrics != nil {
		h.metrics.End(outcome, fields...)
	}
}

func newObservability(ctx context.Context, cfg config.Config, stdout io.Writer) (*observabilityService, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	o := cfg.Observability
	s := &observabilityService{cfg: o, observer: observe.NoopObserver{}}
	fail := func(err error) (*observabilityService, error) { _ = s.Close(context.Background()); return nil, err }
	var projectors observe.Fanout
	if o.Metrics.Enabled {
		policy, err := metricspolicy.New(metricspolicy.Config{HashVersion: o.Cardinality.HashVersion, Salt: o.Cardinality.Salt, Dimensions: map[metricspolicy.Dimension]metricspolicy.DimensionConfig{
			metricspolicy.TenantID: {Budget: o.Cardinality.Tenant.Budget, Pinned: o.Cardinality.Tenant.Pinned, Catalog: o.Cardinality.Tenant.Catalog},
			metricspolicy.Model:    {Budget: o.Cardinality.Model.Budget, Pinned: o.Cardinality.Model.Pinned, Catalog: o.Cardinality.Model.Catalog},
			metricspolicy.Tool:     {Budget: o.Cardinality.Tool.Budget, Pinned: o.Cardinality.Tool.Pinned, Catalog: o.Cardinality.Tool.Catalog},
		}})
		if err != nil {
			return fail(fmt.Errorf("metrics policy: %w", err))
		}
		recorder, err := observeprom.New(observeprom.Config{Policy: policy, HistogramBuckets: o.Metrics.HistogramBuckets})
		if err != nil {
			return fail(fmt.Errorf("metrics adapter: %w", err))
		}
		s.metrics = recorder
		projectors = append(projectors, recorder)
	}
	if o.Logging.Enabled {
		level, _ := observabilitylogging.ParseLevel(o.Logging.Level)
		maxTTL, _ := time.ParseDuration(o.Logging.MaxOverrideTTL)
		levels, err := observabilitylogging.NewLevels(level, maxTTL, nil)
		if err != nil {
			return fail(err)
		}
		s.levels = levels
		var sink io.Writer = stdout
		if o.Logging.Output == "file" {
			file, err := observabilitylogging.OpenFile(o.Logging.File)
			if err != nil {
				return fail(fmt.Errorf("logging adapter: %w", err))
			}
			sink = file
		}
		s.logger = observabilitylogging.New(sink, levels, observabilitylogging.Identity{Service: "fuse", Instance: o.InstanceID})
		projectors = append(projectors, s.logger)
	}
	if o.Traces.Enabled {
		exporter, err := newOTLPExporter(ctx, o.Traces)
		if err != nil {
			return fail(fmt.Errorf("trace adapter: %w", err))
		}
		batch := observeotel.BatchConfig{QueueSize: o.Traces.QueueSize, BatchSize: o.Traces.BatchSize}
		if s.metrics != nil {
			batch.OnExportError = func() { s.metrics.ObserveExportError("traces", "export_failed") }
			batch.OnDrop = func() { s.metrics.ObserveDrop("traces", "queue_full") }
		}
		if o.Traces.ExportTimeout != "" {
			batch.ExportTimeout, _ = time.ParseDuration(o.Traces.ExportTimeout)
		}
		if o.Traces.BatchTimeout != "" {
			batch.BatchTimeout, _ = time.ParseDuration(o.Traces.BatchTimeout)
		}
		resourceAttributes := []attribute.KeyValue{semconv.ServiceName("fuse"), semconv.ServiceVersion(version.Version)}
		if o.InstanceID != "" {
			resourceAttributes = append(resourceAttributes, semconv.ServiceInstanceID(o.InstanceID))
		}
		s.provider = observeotel.NewProvider(exporter, batch,
			sdktrace.WithResource(resource.NewWithAttributes(semconv.SchemaURL, resourceAttributes...)),
			sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(o.Traces.SampleRatio))),
		)
		s.observer = observeotel.New(s.provider)
	}
	if s.metrics != nil {
		s.observer = metricsObserver{primary: s.observer, metrics: s.metrics}
	}
	if len(projectors) > 0 {
		s.projector = projectors
		s.projection = newProjectionDispatcher(projectors, 1024, s.metrics)
	}
	return s, nil
}

// installOTELDiagnostics routes the OpenTelemetry SDK's own diagnostic output —
// its global error handler (BatchSpanProcessor export failures, e.g. "traces
// export: processor export error: rpc timeout") and its internal logger (the
// OTLP exporter's connection-retry warnings) — to sink instead of the SDK
// default of os.Stderr.
//
// This matters for the TUI: runShell hands the terminal to bubbletea, and any
// async write to stderr from a background batch-export goroutine lands on the alt
// screen mid-render and shears the display. Export failures are non-fatal and are
// already counted via BatchConfig.OnExportError, so the shell passes a sink that
// never touches the terminal (the log file when logging to file, else io.Discard).
//
// Both are process-global OTEL registrations; call once per process. A nil sink
// falls back to io.Discard.
func installOTELDiagnostics(sink io.Writer) {
	if sink == nil {
		sink = io.Discard
	}
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		fmt.Fprintf(sink, "otel: %v\n", err)
	}))
	otel.SetLogger(funcr.New(func(prefix, args string) {
		if prefix != "" {
			fmt.Fprintf(sink, "otel: %s: %s\n", prefix, args)
		} else {
			fmt.Fprintf(sink, "otel: %s\n", args)
		}
	}, funcr.Options{}))
}

func newOTLPExporter(ctx context.Context, c config.TracesObservabilityConfig) (sdktrace.SpanExporter, error) {
	if c.Protocol == "grpc" {
		opts := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(c.Endpoint), otlptracegrpc.WithHeaders(c.Headers)}
		if c.Insecure {
			opts = append(opts, otlptracegrpc.WithInsecure())
		}
		return otlptracegrpc.New(ctx, opts...)
	}
	opts := []otlptracehttp.Option{otlptracehttp.WithEndpoint(c.Endpoint), otlptracehttp.WithHeaders(c.Headers)}
	if c.Insecure {
		opts = append(opts, otlptracehttp.WithInsecure())
	}
	return otlptracehttp.New(ctx, opts...)
}

func (s *observabilityService) Close(ctx context.Context) error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		s.reloadMu.Lock()
		defer s.reloadMu.Unlock()
		var errs []error
		projectionStopped := true
		if s.projection != nil {
			if err := s.projection.Close(ctx); err != nil {
				projectionStopped = false
				errs = append(errs, err)
			}
		}
		if s.metricsServer != nil {
			errs = append(errs, s.metricsServer.Shutdown(ctx))
		}
		if s.provider != nil {
			errs = append(errs, s.provider.Shutdown(ctx))
		}
		// A projector stuck inside the logger owns its locks. Do not turn a
		// context-bounded dispatcher shutdown into an unbounded logger close.
		if s.logger != nil && projectionStopped {
			errs = append(errs, s.logger.Close())
		}
		if s.levels != nil {
			errs = append(errs, s.levels.Close())
		}
		s.closeErr = errors.Join(errs...)
	})
	return s.closeErr
}

func (s *observabilityService) startMetricsEndpoint(ctx context.Context, verifier loopauth.Verifier) error {
	if s == nil || s.metrics == nil || s.cfg.Metrics.Bind == "" {
		return nil
	}
	ln, err := net.Listen("tcp", s.cfg.Metrics.Bind)
	if err != nil {
		return fmt.Errorf("metrics listen %s: %w", s.cfg.Metrics.Bind, err)
	}
	h := s.metrics.Handler()
	if s.cfg.Metrics.Access == "authenticated" {
		h = authenticatedHTTP(verifier, h)
	}
	mux := http.NewServeMux()
	mux.Handle(s.cfg.Metrics.Path, h)
	s.metricsListener = ln
	s.metricsServer = &http.Server{Handler: mux, BaseContext: func(net.Listener) context.Context { return ctx }}
	go func() { _ = s.metricsServer.Serve(ln) }()
	return nil
}

func (s *observabilityService) Reopen(ctx context.Context, actor string) error {
	if s == nil || s.logger == nil {
		return errors.New("logging is disabled")
	}
	err := s.logger.Reopen()
	outcome := "success"
	if err != nil {
		outcome = "error"
	}
	if s.metrics != nil {
		s.metrics.ObserveReopen(outcome)
	}
	_ = s.logger.Audit(ctx, observabilitylogging.Audit{Actor: actor, Action: "reopen", Scope: "logging", Outcome: outcome})
	return err
}

func (s *observabilityService) handleSIGHUP(ctx context.Context, logging config.LoggingObservabilityConfig, stderr io.Writer) {
	if s == nil || s.logger == nil {
		return
	}
	if err := s.Reload(ctx, logging, "signal:SIGHUP"); err != nil {
		fmt.Fprintf(stderr, "loop-serve-net: logging reload: %v\n", err)
	}
	if logging.Output == "file" {
		if err := s.Reopen(ctx, "signal:SIGHUP"); err != nil {
			fmt.Fprintf(stderr, "loop-serve-net: logging reopen: %v\n", err)
		}
	}
}

// Reload validates the complete replacement before publishing its live level.
// Output/file and every SDK/metrics setting are startup-only because changing
// them requires reconstructing exporters and sinks.
func (s *observabilityService) Reload(ctx context.Context, next config.LoggingObservabilityConfig, actor string) error {
	if s == nil || s.levels == nil {
		return errors.New("logging is disabled")
	}
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()
	probe := config.Config{Observability: config.ObservabilityConfig{Logging: next}}
	if err := probe.Validate(); err != nil {
		return err
	}
	if next.Output != s.cfg.Logging.Output || next.File != s.cfg.Logging.File || next.MaxOverrideTTL != s.cfg.Logging.MaxOverrideTTL {
		return errors.New("logging output, file, and maximum override TTL are startup-only")
	}
	level, _ := observabilitylogging.ParseLevel(next.Level)
	previous := s.levels.Inspect().Default.String()
	if err := s.levels.SetDefault(level); err != nil {
		return err
	}
	s.cfg.Logging = next
	return s.logger.Audit(ctx, observabilitylogging.Audit{Actor: actor, Action: "reload", Scope: "logging", Previous: previous, New: level.String(), Outcome: "success"})
}

func authenticateHTTP(r *http.Request, verifier loopauth.Verifier) (loopauth.Principal, error) {
	if verifier == nil {
		return loopauth.Principal{}, loopauth.ErrInvalidToken
	}
	h := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(h) < 8 || !strings.EqualFold(h[:7], "Bearer ") {
		return loopauth.Principal{}, loopauth.ErrInvalidToken
	}
	return verifier.Verify(r.Context(), strings.TrimSpace(h[7:]))
}

func authenticatedHTTP(verifier loopauth.Verifier, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := authenticateHTTP(r, verifier)
		if err != nil {
			http.Error(w, "unauthenticated", http.StatusUnauthorized)
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), principalContextKey{}, p))
		next.ServeHTTP(w, r)
	})
}

type principalContextKey struct{}

type loggingMutation struct {
	Scope    string `json:"scope"`
	TenantID string `json:"tenant_id"`
	LoopID   string `json:"loop_id"`
	Level    string `json:"level"`
	TTL      string `json:"ttl"`
}

func (s *observabilityService) adminHandler(verifier loopauth.Verifier) http.Handler {
	return authenticatedHTTP(verifier, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.levels == nil {
			http.Error(w, "logging disabled", http.StatusNotFound)
			return
		}
		p := r.Context().Value(principalContextKey{}).(loopauth.Principal)
		tenant := string(event.NormalizeTenant(p.Tenant))
		if r.Method == http.MethodGet {
			state := s.levels.Inspect()
			filtered := state.Overrides[:0]
			for _, v := range state.Overrides {
				if v.TenantID == tenant {
					filtered = append(filtered, v)
				}
			}
			state.Overrides = filtered
			_ = json.NewEncoder(w).Encode(struct {
				InstanceID string                     `json:"instance_id"`
				State      observabilitylogging.State `json:"state"`
			}{s.cfg.InstanceID, state})
			return
		}
		var m loggingMutation
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&m); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		if m.TenantID == "" {
			m.TenantID = tenant
		}
		if m.TenantID != tenant || m.Scope == "global" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		actor := p.Subject
		var err error
		switch r.Method {
		case http.MethodPut:
			level, e := observabilitylogging.ParseLevel(m.Level)
			if e != nil {
				err = e
				break
			}
			ttl, e := time.ParseDuration(m.TTL)
			if e != nil {
				err = e
				break
			}
			expiry := time.Now().Add(ttl)
			if m.Scope == "tenant" {
				err = s.levels.SetTenant(tenant, level, expiry)
			} else if m.Scope == "loop" {
				err = s.levels.SetLoop(tenant, m.LoopID, level, expiry)
			} else {
				err = errors.New("invalid scope")
			}
		case http.MethodDelete:
			if m.Scope == "tenant" {
				s.levels.RemoveTenant(tenant)
			} else if m.Scope == "loop" {
				s.levels.RemoveLoop(tenant, m.LoopID)
			} else {
				err = errors.New("invalid scope")
			}
		default:
			w.Header().Set("Allow", "GET, PUT, DELETE")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		outcome := "success"
		if err != nil {
			outcome = "error"
		}
		_ = s.logger.Audit(r.Context(), observabilitylogging.Audit{Actor: actor, Action: strings.ToLower(r.Method), Scope: m.Scope, New: m.Level, Outcome: outcome})
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err == nil {
			s.updateOverrideMetrics()
		}
		w.WriteHeader(http.StatusNoContent)
	}))
}

func (s *observabilityService) updateOverrideMetrics() {
	if s == nil || s.metrics == nil || s.levels == nil {
		return
	}
	counts := map[string]float64{"tenant": 0, "loop": 0}
	for _, override := range s.levels.Inspect().Overrides {
		counts[string(override.Scope)]++
	}
	for scope, count := range counts {
		s.metrics.SetOverrides(scope, count)
	}
}

func (s *observabilityService) reloadHandler(verifier loopauth.Verifier) http.Handler {
	return authenticatedHTTP(verifier, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.Context().Value(principalContextKey{}).(loopauth.Principal)
		if !p.ObservabilityOperator {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var next config.LoggingObservabilityConfig
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&next); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		if err := s.Reload(r.Context(), next, p.Subject); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
}

func (s *observabilityService) reopenHandler(verifier loopauth.Verifier) http.Handler {
	return authenticatedHTTP(verifier, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.Context().Value(principalContextKey{}).(loopauth.Principal)
		if !p.ObservabilityOperator {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if err := s.Reopen(r.Context(), p.Subject); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
}

// projectingDurableStore observes the append only after durable acceptance.
type projectingDurableStore struct {
	event.CommittedDurableStore
	projection *projectionDispatcher
}

func (s projectingDurableStore) Append(ctx context.Context, key event.StreamKey, e event.Event) error {
	committed, err := s.AppendCommitted(ctx, key, e)
	if err != nil {
		return err
	}
	if s.projection != nil {
		s.projection.Enqueue(observe.ProjectEvent(key, committed))
	}
	return nil
}

type projectionStats struct {
	Dropped uint64
	Errors  uint64
}

// projectionDispatcher keeps observability sinks outside the durable append
// path. Its finite queue deliberately sheds telemetry during an outage rather
// than applying backpressure to the loop.
type projectionDispatcher struct {
	projector observe.Projector
	metrics   *observeprom.Recorder
	queue     chan observe.Record
	done      chan struct{}
	ctx       context.Context
	cancel    context.CancelFunc
	mu        sync.Mutex
	closed    bool
	dropped   atomic.Uint64
	errors    atomic.Uint64
}

func newProjectionDispatcher(projector observe.Projector, capacity int, metrics ...*observeprom.Recorder) *projectionDispatcher {
	if capacity < 1 {
		capacity = 1
	}
	ctx, cancel := context.WithCancel(context.Background())
	d := &projectionDispatcher{projector: projector, queue: make(chan observe.Record, capacity), done: make(chan struct{}), ctx: ctx, cancel: cancel}
	if len(metrics) > 0 {
		d.metrics = metrics[0]
	}
	go d.run()
	return d
}

func (d *projectionDispatcher) Enqueue(record observe.Record) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		d.dropped.Add(1)
		if d.metrics != nil {
			d.metrics.ObserveDrop("projection", "closed")
		}
		return
	}
	select {
	case d.queue <- record:
	default:
		d.dropped.Add(1)
		if d.metrics != nil {
			d.metrics.ObserveDrop("projection", "queue_full")
		}
	}
}

func (d *projectionDispatcher) Stats() projectionStats {
	if d == nil {
		return projectionStats{}
	}
	return projectionStats{Dropped: d.dropped.Load(), Errors: d.errors.Load()}
}

func (d *projectionDispatcher) Close(ctx context.Context) error {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	if !d.closed {
		d.closed = true
		d.cancel()
		close(d.queue)
	}
	d.mu.Unlock()
	select {
	case <-d.done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("projection shutdown: %w", ctx.Err())
	}
}

func (d *projectionDispatcher) run() {
	defer close(d.done)
	for record := range d.queue {
		if d.projector != nil {
			if err := d.projector.Project(d.ctx, record); err != nil && !errors.Is(err, context.Canceled) {
				d.errors.Add(1)
				if d.metrics != nil {
					d.metrics.ObserveExportError("projection", "sink_error")
				}
			}
		}
		if d.ctx.Err() != nil {
			return
		}
	}
}
