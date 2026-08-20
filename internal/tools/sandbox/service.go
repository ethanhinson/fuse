package sandbox

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ethanhinson/fuse/internal/loopauth"
)

// ErrRefusedUncontained is the refusal returned by Acquire when no contained
// substrate could be constructed and nothing authorized the host.
//
// It is a REFUSAL, not a degradation. The alternative — running the command on
// the host because the container runtime is missing — would mean that
// uninstalling docker silently turns containment off, which is precisely the
// fail-open direction ADR-0044 forbids. A caller that cannot get a Runner must
// report that the bash tool is unavailable; it must never answer this error by
// running the command somewhere less safe.
var ErrRefusedUncontained = errors.New("no container runtime available; refusing to run bash uncontained")

// Service owns substrate SELECTION: it decides, once, which Handler the bash
// tool runs on, and it is the only thing entitled to make that decision.
//
// # The attack this type exists to make unwritable
//
// The off-switch is a file — <root>/.fuse/sandbox.local.yml — and the agent
// running inside the sandbox can write files. If containment were resolved per
// bash call, a model could write its own .fuse/sandbox.local.yml on one call
// and be uncontained on the next: it would author its own escape and then use
// it, within a single loop. That is why the configuration is loaded exactly
// ONCE, at construction, against a repo root the COMPOSITION ROOT resolved and
// trusts, and why the resolved Config and the selected Handler are then frozen.
//
// Consequently this type deliberately exposes NO API that re-reads the config,
// re-resolves the root, or re-selects the handler. There is no Reload, no
// SetConfig, no per-call root or path parameter, and no method anywhere on
// Service that touches the filesystem. A structural test (see
// TestServiceMethodsNeverReloadConfig) fails the build if a method ever gains
// one. If a future need for a hot reload appears, it must be a NEW Service
// constructed from a freshly trusted root — never a mutation of this one.
//
// A Service is immutable after construction and is safe for concurrent use.
type Service struct {
	// cfg is the configuration as resolved at construction. Frozen; see above.
	cfg Config

	// handler is the selected substrate, or nil when selection refused. A nil
	// handler is the fail-closed state: Acquire returns refusal, and there is
	// no field a caller could consult to "use something else instead".
	handler Handler

	// refusal is the error Acquire returns while handler is nil. It is non-nil
	// exactly when handler is nil.
	refusal error

	// hosted records the posture the composition root declared, for
	// diagnostics only; selection already consumed it.
	hosted bool

	// warns are the diagnostics from the config load, retained so the
	// composition root can log them (and so events can report a degraded load).
	warns []Warning

	// lookup resolves environment values for the scrub allowlist. Nil means the
	// real process environment. This is the env-SCRUB seam — what a command may
	// OBSERVE — and has nothing to do with the off-switch, which is file-only.
	lookup func(string) (string, bool)
}

// serviceOptions are the knobs NewService accepts. They are a separate struct
// from Service so that nothing an option touches is reachable — or mutable —
// after construction returns.
type serviceOptions struct {
	hosted        bool
	lookup        func(string) (string, bool)
	containerOpts []containerOption
	hostHandler   Handler
	newContainer  func(Config, ...containerOption) (Handler, error)
}

// ServiceOption configures a Service at construction.
type ServiceOption func(*serviceOptions)

// WithHostedPosture declares whether this process is running the hosted /
// loop-server posture of ADR-0034 — that is, executing workloads on behalf of
// remote principals rather than for the operator sitting at this machine.
//
// SECURITY-CRITICAL: this flag is supplied by the COMPOSITION ROOT, from how
// the binary was launched. It must never be read from the config file, from an
// environment variable, from a wire field, from a tool argument, or from model
// output. Under the hosted posture the off-switch is structurally inert (see
// selectHandler): a local file cannot decide that someone else's workload runs
// uncontained.
func WithHostedPosture(hosted bool) ServiceOption {
	return func(o *serviceOptions) { o.hosted = hosted }
}

// WithEnvLookup overrides how allowlisted environment values are resolved.
// It affects only what a sandboxed command may observe; it can neither widen
// the allowlist nor influence containment.
func WithEnvLookup(fn func(string) (string, bool)) ServiceOption {
	return func(o *serviceOptions) {
		if fn != nil {
			o.lookup = fn
		}
	}
}

// withContainerLookPath overrides container CLI probing (tests).
func withContainerLookPath(fn func(string) (string, error)) ServiceOption {
	return func(o *serviceOptions) {
		o.containerOpts = append(o.containerOpts, withLookPath(fn))
	}
}

// withContainerExec overrides how the container CLI is invoked (tests).
func withContainerExec(fn execRunner) ServiceOption {
	return func(o *serviceOptions) {
		o.containerOpts = append(o.containerOpts, withExecRunner(fn))
	}
}

// withHostHandler substitutes the host substrate (tests). It exists so a test
// can prove the refusal path never touches the host at all, which a real
// hostHandler — which records nothing — cannot show.
func withHostHandler(h Handler) ServiceOption {
	return func(o *serviceOptions) {
		if h != nil {
			o.hostHandler = h
		}
	}
}

// NewService selects the substrate for an already-loaded Config.
//
// This is the composition-root form: the caller loaded the Config once from a
// trusted root (see NewServiceFromRoot, which does both) and passes the hosted
// posture explicitly.
//
// It does not return an error for a missing container runtime, and that is
// deliberate. A construction error at this exact boundary invites the caller to
// write `svc, err := NewService(...); if err != nil { /* fall back */ }`, and
// the only available fallback is uncontained execution. Instead an
// unconstructable substrate yields a Service in the refusing state, so
// "unavailable" can be observed (Available) and reported, but never rescued.
// The error return is reserved for a future substrate whose construction can
// fail for a reason that is NOT a containment refusal; it is nil today.
func NewService(cfg Config, opts ...ServiceOption) (*Service, error) {
	o := &serviceOptions{newContainer: defaultContainerFactory}
	for _, opt := range opts {
		if opt != nil {
			opt(o)
		}
	}
	if o.hostHandler == nil {
		o.hostHandler = newHostHandler()
	}

	if cfg.IdleTTL <= 0 {
		// A hand-built Config (or the zero value) would otherwise mean "reap on
		// release", tearing down every warm Runner the instant it is returned.
		// LoadConfig already guarantees a positive TTL; this covers the callers
		// that did not go through it.
		cfg.IdleTTL = DefaultIdleTTL
	}

	s := &Service{
		cfg:    cfg,
		hosted: o.hosted,
		lookup: o.lookup,
	}
	s.handler, s.refusal = selectHandler(cfg, o)
	return s, nil
}

// NewServiceFromRoot loads the off-switch config ONCE from root and selects the
// substrate for it.
//
// root must be a repo root the caller resolved and TRUSTS — never a path
// derived from a tool argument, a wire field, or model output, and never the
// process working directory of an agent that is free to chdir.
//
// The returned Warnings are the operator's only signal that their config file
// did not do what they thought. The caller MUST log them; they are returned
// (rather than only stashed) so that ignoring them takes a deliberate `_`.
func NewServiceFromRoot(root string, opts ...ServiceOption) (*Service, []Warning, error) {
	cfg, warns := LoadConfig(root)

	svc, err := NewService(cfg, opts...)
	if err != nil {
		return nil, warns, err
	}
	// Assigned before the Service escapes to the caller, so the "immutable
	// after construction" property holds: nothing else ever writes a field.
	svc.warns = warns
	return svc, warns, nil
}

// defaultContainerFactory adapts newContainerHandler to the factory signature.
// It returns a nil Handler INTERFACE on error — not a typed nil pointer — so
// that a caller checking `handler == nil` is never fooled by a non-nil
// interface wrapping a nil *containerHandler.
func defaultContainerFactory(cfg Config, opts ...containerOption) (Handler, error) {
	h, err := newContainerHandler(cfg, opts...)
	if err != nil {
		return nil, err
	}
	return h, nil
}

// selectHandler is THE decision. The rules, in order:
//
//  1. The config authorized the host AND the hosted posture is not active
//     ⇒ host. This is the operator's local off-switch, honoured.
//  2. Otherwise ⇒ container. This is the default, and it is what an absent,
//     unreadable, malformed, or un-understood config resolves to (T3).
//  3. The container substrate could not be constructed and the host was not
//     authorized ⇒ REFUSE. Never a host fallback.
//
// Rule 4 has no branch of its own, and that is the point: under the hosted
// posture the off-switch is not overridden, it is never consulted. `contained:
// false` and `handler: host` are structurally inert because hostAuthorized is
// unreachable while hosted is true, so there is no path — including the
// container-unavailable path — on which a hosted process reaches the host.
func selectHandler(cfg Config, o *serviceOptions) (Handler, error) {
	if hostAuthorized(cfg, o.hosted) {
		return o.hostHandler, nil
	}

	handler, err := o.newContainer(cfg, o.containerOpts...)
	if err != nil {
		// Fail closed. Note what is NOT here: any consideration of
		// o.hostHandler. The host is unreachable from this branch by
		// construction, not by a check that a later edit could invert.
		return nil, fmt.Errorf("%w: %w", ErrRefusedUncontained, err)
	}
	return handler, nil
}

// hostAuthorized reports whether uncontained execution on this machine was
// explicitly authorized.
//
// Both containment fields must agree before the host is reachable. The loader
// guarantees the invariant Contained == (Handler != HandlerHost), so requiring
// both costs nothing for a loaded config — but Config is an exported struct a
// caller can build by hand, and its ZERO VALUE has Contained: false. Demanding
// the explicit HandlerHost as well means a zero or half-built Config can never
// be misread as an off-switch.
func hostAuthorized(cfg Config, hosted bool) bool {
	if hosted {
		// The hosted posture ends the question before the file is consulted.
		return false
	}
	return cfg.Handler == HandlerHost && !cfg.Contained
}

// Acquire returns an execution context for p on the selected substrate.
//
// It refuses — with ErrRefusedUncontained — when no substrate was selected.
// Nothing is executed on that path: no probe, no fallback, no host Runner.
//
// The environment is re-resolved per Acquire from the passthrough list frozen
// at construction, so a Runner always carries current values (which the warm
// pool's reset-on-checkout relies on) while the SET of permitted keys stays
// exactly what the trusted config declared.
func (s *Service) Acquire(ctx context.Context, p loopauth.Principal) (Runner, error) {
	if s.handler == nil {
		return nil, s.refusal
	}
	return s.handler.Acquire(ctx, p, s.resolveEnv())
}

// resolveEnv builds the complete environment a command may observe.
func (s *Service) resolveEnv() Env {
	if s.lookup == nil {
		return ResolveEnvFromOS(s.cfg.EnvPassthrough)
	}
	return ResolveEnv(s.cfg.EnvPassthrough, s.lookup)
}

// Available reports whether a substrate was selected. When it is false every
// Acquire refuses, and the correct response is to report the bash tool as
// unavailable — never to run the command another way.
func (s *Service) Available() bool { return s.handler != nil }

// HandlerName reports the bounded substrate identifier ("container" or "host"),
// or "" when selection refused. It is safe as an event or metric label.
func (s *Service) HandlerName() string {
	if s.handler == nil {
		return ""
	}
	return s.handler.Name()
}

// Contained reports whether the selected substrate isolates the command from
// this host. It is false both for the host substrate and when nothing was
// selected — in the latter case nothing runs at all, so "not contained" never
// describes something that executed.
func (s *Service) Contained() bool {
	return s.handler != nil && s.handler.Name() != HandlerHost
}

// Hosted reports the posture the composition root declared. It is diagnostic
// only: selection has already consumed it, and no caller may re-derive
// containment from it.
func (s *Service) Hosted() bool { return s.hosted }

// Runtime reports which container CLI was detected, or "" on any other
// substrate. It is a bounded value ("docker"|"nerdctl"|"podman"), safe as a
// label.
func (s *Service) Runtime() string {
	if h, ok := s.handler.(*containerHandler); ok {
		return h.Runtime()
	}
	return ""
}

// IdleTTL is the warm-pool reaper's idle backstop from the resolved config. It
// is always positive.
func (s *Service) IdleTTL() time.Duration { return s.cfg.IdleTTL }

// Warnings returns the diagnostics from the config load. The composition root
// MUST log them: they are how an operator learns their off-switch file was not
// honoured. Warning.Reason is a bounded enum, safe as a label.
func (s *Service) Warnings() []Warning {
	return append([]Warning(nil), s.warns...)
}
