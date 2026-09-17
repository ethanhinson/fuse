package main

import (
	"context"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/loopauth"
)

// The two probe paths. `/-/` prefixes are already the observability admin
// convention, but liveness/readiness deliberately use the unprefixed spellings
// every orchestrator (Kubernetes, compose, an LB health check) expects by default.
const (
	healthzPath = "/healthz"
	readyzPath  = "/readyz"
)

// readinessProbeTTL is how long one store probe result — success OR failure — is
// reused. Kubernetes probes every pod every few seconds and an LB may probe far
// more often; without a cache, N replicas × M probers becomes a connection storm
// against the one Postgres that is already the thing in trouble. 2s is short
// enough that a recovered store is advertised Ready within one probe period.
const readinessProbeTTL = 2 * time.Second

// readinessProbeTimeout bounds a single store probe so a hung database cannot
// pile up goroutines behind an unauthenticated endpoint.
const readinessProbeTimeout = 2 * time.Second

// readiness owns the three conditions that decide whether this instance should
// receive traffic, plus the probe cache.
//
// The conditions, and why each one is a readiness condition rather than a
// liveness condition:
//
//   - the durable store answers a cheap Ping — an instance whose event store is
//     unreachable can accept a loop and then lose it, so it must not be sent
//     traffic; but the process is fine, so restarting it (liveness) would not help.
//   - a verifier is present — without one the auth surface is not up, and a Ready
//     instance with no verifier would take requests it cannot authenticate.
//   - the process is not draining — set by the shutdown path (change 0076 Task 4)
//     so the load balancer stops sending new work BEFORE the listener stops
//     accepting it. Reversing that order is what makes a "graceful" drain drop
//     requests.
//
// The store field is typed event.DurableStore because that is what the composition
// root has; the Pinger capability is OPTIONAL and type-asserted (the
// CommittedDurableStore precedent). A nil store, or one that implements no Pinger,
// counts as ready: there is nothing to probe, and failing closed there would make
// every in-memory binding permanently un-Ready.
type readiness struct {
	store    event.DurableStore // may or may not implement event.Pinger
	verifier loopauth.Verifier
	draining atomic.Bool

	mu      sync.Mutex
	lastAt  time.Time
	lastErr error
	// hasLast distinguishes "no probe has ever completed" (cold start) from "a
	// probe completed and its result was nil". lastAt cannot carry that, because
	// an injected test clock may legitimately report the zero time.
	hasLast bool
	// inflight is non-nil exactly while one goroutine is inside Ping, and is
	// closed when that Ping returns. A concurrent caller uses it to WAIT only on
	// the cold-start path; once a result exists, callers read it and return
	// instead of waiting (see probe).
	inflight chan struct{}

	// now and logf are injected only by tests (a fake clock for the cache TTL, a
	// capturing sink for the reason log). Production uses the real ones.
	now  func() time.Time
	logf func(string, ...any)
}

func newReadiness(store event.DurableStore, verifier loopauth.Verifier) *readiness {
	return &readiness{
		store:    store,
		verifier: verifier,
		now:      time.Now,
		logf:     log.Printf,
	}
}

// register mounts the probe routes on mux. Both are mounted UNAUTHENTICATED by
// construction: the auth interceptor for this server is a connect.HandlerOption on
// the Connect handler (see serveNetObserved), never mux middleware, so a plain
// mux.Handle carries no credential requirement. That is deliberate — a kubelet
// presents no bearer token — and it is why neither handler may ever reveal
// anything about the instance beyond "ready" / "not ready".
func (r *readiness) register(mux *http.ServeMux) {
	mux.HandleFunc(healthzPath, r.handleHealthz)
	mux.HandleFunc(readyzPath, r.handleReadyz)
}

// handleHealthz is liveness ONLY: it answers 200 for as long as the process can
// serve HTTP. It deliberately consults nothing — no store, no verifier, not even
// the draining flag. A liveness probe that fails on a dependency outage gets the
// pod killed and restarted for a problem a restart cannot fix, and during a
// graceful drain it would kill the pod mid-drain.
func (r *readiness) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

// handleReadyz answers 200 "ok" when all three conditions hold, and 503 with an
// EMPTY body otherwise. The reason goes to the log and never to the response:
// this endpoint is reachable without a credential, and a store error commonly
// carries infrastructure detail (a DSN host, a port, an internal hostname) that
// an unauthenticated caller must not learn.
func (r *readiness) handleReadyz(w http.ResponseWriter, req *http.Request) {
	if reason := r.notReady(req.Context()); reason != "" {
		r.logf("readyz: not ready: %s", reason)
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

// notReady returns the reason this instance is not ready, or "" when it is. The
// reason string is for the log only.
func (r *readiness) notReady(ctx context.Context) string {
	// Draining is checked FIRST and short-circuits: once the answer is "no" for
	// the rest of this process's life there is no reason to keep probing the
	// store, and the shutdown path wants this answer to be instant.
	if r.draining.Load() {
		return "draining"
	}
	if r.verifier == nil {
		return "no token verifier configured"
	}
	if err := r.probe(ctx); err != nil {
		return "durable store ping: " + err.Error()
	}
	return ""
}

// probe pings the durable store, reusing a result younger than readinessProbeTTL.
// Both outcomes are cached, so a probe storm against a store that is DOWN cannot
// become a connection storm against it either.
//
// Concurrent probes collapse onto ONE Ping rather than all missing the cache
// together and each opening a connection — the exact storm the cache exists to
// prevent. They collapse singleflight-style, NOT by queueing: the mutex is
// released before Ping runs, and a caller that arrives while a probe is in flight
// returns the previous result immediately instead of waiting for the new one. An
// earlier shape held the lock across Ping, which collapsed the storm but made
// every queued probe pay the full readinessProbeTimeout — and since that budget
// equals the chart's probes.readiness.timeoutSeconds, a store that was merely SLOW
// timed out every kubelet probe rather than one, and failureThreshold evicted a
// healthy pod on latency alone. A concurrent probe is now O(1), not O(wait).
//
// Cold start is the one case where a caller does wait: until some probe has
// completed there is no previous result, and the alternatives are both wrong —
// answering "ready" would advertise an instance whose store may be dead (exactly
// what the startup probe exists to catch), and answering "not ready" would burn
// kubelet failures on a store nobody has asked yet. So the first caller, and any
// caller concurrent with that first Ping, waits for the real answer; that wait is
// still bounded by readinessProbeTimeout. From the second probe onward nobody
// waits.
//
// The probe context is DETACHED from the request context and carries only the
// timeout. That is deliberate and load-bearing with the cache: a prober that
// gives up and closes the connection (a kubelet whose own timeoutSeconds is
// shorter than a slow-but-healthy Ping) would otherwise cancel the in-flight
// Ping, and the resulting context.Canceled would be CACHED as a store failure —
// marking a healthy instance not-ready for the whole TTL on the strength of the
// client's impatience. A caller that has hung up gets no answer either way, so
// there is nothing to gain by propagating its cancellation inward.
func (r *readiness) probe(_ context.Context) error {
	pinger, ok := r.store.(event.Pinger)
	if !ok || pinger == nil {
		// No store, or a store that implements no Pinger: nothing to probe, so
		// ready. The optional-interface degrade (event.Pinger's contract).
		return nil
	}

	for {
		r.mu.Lock()
		// A result younger than the TTL is served as-is, in flight or not.
		if r.hasLast && r.now().Sub(r.lastAt) < readinessProbeTTL {
			err := r.lastErr
			r.mu.Unlock()
			return err
		}
		if wait := r.inflight; wait != nil {
			// Someone is already probing. With a previous result in hand, return
			// it and do NOT queue — stale by at most one probe interval is the
			// right trade against spending the caller's whole timeout budget.
			if r.hasLast {
				err := r.lastErr
				r.mu.Unlock()
				return err
			}
			// Cold start: nothing to report yet, so wait for that probe and loop
			// to read whatever it recorded.
			r.mu.Unlock()
			select {
			case <-wait:
			case <-time.After(readinessProbeTimeout):
				// The owner is itself bounded by readinessProbeTimeout, so this
				// fires only if it is wedged past its own deadline. Report
				// not-ready rather than wait unbounded; nothing is cached, so the
				// next probe re-evaluates.
				return context.DeadlineExceeded
			}
			continue
		}
		// This goroutine owns the probe. Publish inflight, drop the lock, Ping.
		done := make(chan struct{})
		r.inflight = done
		startedAt := r.now()
		r.mu.Unlock()

		pctx, cancel := context.WithTimeout(context.Background(), readinessProbeTimeout)
		err := pinger.Ping(pctx)
		cancel()

		r.mu.Lock()
		r.lastErr = err
		r.lastAt = startedAt
		r.hasLast = true
		r.inflight = nil
		r.mu.Unlock()
		close(done)
		return err
	}
}

// setDraining flips this instance to permanently not-ready. It is the seam the
// graceful-shutdown path (change 0076 Task 4) calls BEFORE it stops the listener,
// so the load balancer removes this instance from rotation while it can still
// finish the requests already in flight. It is one-way on purpose: a process that
// has begun shutting down never re-advertises itself as ready.
func (r *readiness) setDraining() { r.draining.Store(true) }
