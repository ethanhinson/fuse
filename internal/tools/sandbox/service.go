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

	// root is the TRUSTED filesystem root the composition root resolved: the
	// working tree a contained command is allowed to see, and the only
	// bind-mount source the container substrate will ever use. It is retained
	// here — rather than consumed and dropped by the config load — because the
	// substrate needs it at Exec time, and because the alternative (deriving a
	// mount from the model's working_dir) inverts ADR-0044's containment
	// constraint. Frozen at construction like everything else on this type.
	root string

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

	// gate is the process-scoped admission control on concurrent in-flight
	// Execs (change 0077). It lives HERE, on the process-scoped Service, and not
	// on the per-loop Pool: a per-loop gate would give every loop its own full
	// budget and bound the host by nothing. It is non-nil after construction and
	// is safe for concurrent use.
	gate *Gate
}

// serviceOptions are the knobs NewService accepts. They are a separate struct
// from Service so that nothing an option touches is reachable — or mutable —
// after construction returns.
type serviceOptions struct {
	root          string
	hosted        bool
	lookup        func(string) (string, bool)
	containerOpts []containerOption
	hostHandler   Handler
	newContainer  func(Config, ...containerOption) (Handler, error)

	// egressProxy and egressForwarder are the two halves of change 0064's
	// datapath, declared by the composition root through WithEgressProxy. They
	// are held here — rather than appended to containerOpts as they arrive — so
	// NewService can apply them LAST, after every caller-supplied option.
	egressProxy     *Proxy
	egressForwarder string

	// tenantRoots is change 0065's per-tenant mount-root resolver, declared by
	// the composition root through WithTenantRoots. Held here — rather than
	// appended to containerOpts as it arrives — so NewService can apply it
	// LAST, after every caller-supplied option, for the same reason
	// withTrustedRoot is applied last: this names the host tree fuse
	// bind-mounts into a container a model drives.
	tenantRoots *TenantRoots
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

// WithTrustedRoot declares the working tree a contained command may see.
//
// SECURITY-CRITICAL: this is the bind-mount SOURCE. Like WithHostedPosture it
// is supplied by the COMPOSITION ROOT, from a root it resolved and trusts at
// startup — never from a tool argument, a wire field, or model output. The
// model's `working_dir` is resolved as a subpath WITHIN this root (ADR-0044:
// it "resolves within the mount and cannot escape it"); it can never widen,
// replace, or escape what this option declares.
//
// NewServiceFromRoot applies it for you from the root it was given, so the
// ordinary composition path never calls this directly. It exists for the
// NewService form, which takes an already-loaded Config and therefore has no
// other way to say which tree the config was trusted from.
func WithTrustedRoot(root string) ServiceOption {
	return func(o *serviceOptions) { o.root = root }
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

// WithEgressProxy declares the egress DATAPATH: the host-side proxy whose
// per-principal UNIX socket a contained command reaches the network through, and
// the host path of the statically linked forwarder binary
// (cmd/fuse-egress-forward, built by `make egress-forwarder`) that is
// bind-mounted into the container to relay loopback traffic to it.
//
// SECURITY-CRITICAL: like WithHostedPosture and WithTrustedRoot, both values come
// from the COMPOSITION ROOT and from nowhere else. This is the one option that
// names a BINARY fuse mounts into the sandbox and runs as the command's parent
// process, so it is applied LAST — after every caller-supplied option — and takes
// a concrete *Proxy rather than an interface a caller could implement.
//
// It is OPTIONAL and its absence is not permissive. Without it, EgressEnforce
// emits the `--network none` floor and no hole at all: containment is total and
// every network call fails. Turning enforcement ON is `egress.mode: enforce` in
// the trusted config; this option only decides whether the declared allowlist has
// any way to be reached.
//
// A nil Proxy or an empty/unresolvable forwarder path is treated as "not
// supplied" — the fail-closed direction — never as a partially open datapath.
//
// Lifecycle: the caller OWNS the Proxy and must Close it at shutdown. The Service
// never closes it, because a Service is immutable after construction and has no
// shutdown of its own, and because one Proxy may outlive or precede any Service.
func WithEgressProxy(p *Proxy, forwarderPath string) ServiceOption {
	return func(o *serviceOptions) {
		o.egressProxy = p
		o.egressForwarder = forwarderPath
	}
}

// WithTenantRoots declares that the bind-mount source is per-TENANT: each
// authenticated principal's containers see the tree its Principal.Tenant maps
// to, and nothing else (change 0065).
//
// SECURITY-CRITICAL, and for exactly WithTrustedRoot's reason: this is the
// bind-mount SOURCE. Like WithHostedPosture, WithTrustedRoot and
// WithEgressProxy, the value comes from the COMPOSITION ROOT and from nowhere
// else — never a tool argument, a wire field, working_dir, or model output. It
// is applied LAST, after every caller-supplied option, and it takes the
// concrete *TenantRoots rather than an interface a caller could implement,
// because a caller-suppliable root source IS a caller-suppliable bind-mount
// into the sandbox.
//
// It is the HOSTED-PROFILE WIDENING, not a replacement. WithTrustedRoot's
// single-root behaviour remains the DEFAULT: with no resolver declared, the
// container substrate mounts the one trusted root exactly as it did before this
// change, and the assembled argv is byte-identical. That is deliberate — the
// local, single-tenant developer path has one tenant and one working tree, and
// making it pay for a boundary it does not have would be complexity with no
// isolation to show for it.
//
// A nil *TenantRoots is treated as "not supplied" and leaves the single-root
// default in place; it never becomes a non-nil resolver that resolves nothing
// (see the nil-interface dance in NewService).
//
// Where the resolver is configured, its answer is authoritative and never
// widened on failure: a principal whose root cannot be resolved gets NO mount
// and any working_dir is refused. It must never fall back to the trusted root
// declared by WithTrustedRoot, which under the hosted posture would be a tree
// shared across tenants.
func WithTenantRoots(t *TenantRoots) ServiceOption {
	return func(o *serviceOptions) { o.tenantRoots = t }
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
	// Appended LAST so it is the trusted root that wins: no test option and no
	// caller-supplied containerOption can substitute a different mount source.
	o.containerOpts = append(o.containerOpts, withTrustedRoot(o.root))
	// Plumbs the SAME lookup seam WithEnvLookup governs for the sandboxed
	// command's environment into the container CLI client's own passthrough
	// resolution (see runClientCommand), so one seam governs both halves.
	if o.lookup != nil {
		o.containerOpts = append(o.containerOpts, withContainerEnvLookup(o.lookup))
	}

	if cfg.IdleTTL <= 0 {
		// A hand-built Config (or the zero value) would otherwise mean "reap on
		// release", tearing down every warm Runner the instant it is returned.
		// LoadConfig already guarantees a positive TTL; this covers the callers
		// that did not go through it.
		cfg.IdleTTL = DefaultIdleTTL
	}

	// Fill every unset limit/concurrency field with its posture-appropriate
	// default. This is where the fail-safe posture decision is made — the caps
	// split hosted vs local, the concurrency backstop and pull timeout do not —
	// because o.hosted is known HERE and not in the (deliberately posture-free)
	// loader. cfg is a value, so the resolved caps become part of the frozen
	// Service config and reach the container handler below.
	cfg.resolveDefaults(o.hosted)

	// The container handler needs the resolved caps at construction — like the
	// mount root, they are settled once, before any model runs, with no method
	// that can change them afterwards. Appended after withTrustedRoot so caps and
	// root arrive together as trusted construction facts.
	o.containerOpts = append(o.containerOpts, withLimits(cfg.Limits))

	// The egress posture (change 0064) is the same kind of trusted construction
	// fact, and it is appended LAST for the same reason withTrustedRoot is: a
	// caller-supplied containerOption applied afterwards could otherwise put the
	// handler back on EgressAllowAll, and a floor the caller (or, through it, a
	// model) can lower is not a floor. Note that cfg.Egress is deliberately NOT
	// touched by resolveDefaults — the posture is chosen by the explicit
	// egress.mode knob only (see Config.Egress).
	o.containerOpts = append(o.containerOpts, withEgress(cfg.Egress))

	// And the datapath through that floor (change 0064, task 6), appended after
	// the posture so it is the very last word on what gets mounted into — and
	// executed inside — the container.
	//
	// It is appended UNCONDITIONALLY, including when the composition root
	// declared no datapath at all. That is deliberate: "the trusted side wins" has
	// to include the trusted side saying NOTHING, or a caller could open a hole
	// simply by being the only one who mentioned it. With nothing declared this
	// writes a nil source and an empty forwarder path, erasing anything an earlier
	// option set, and enforcement falls back to the datapath-less deny-all.
	//
	// The explicit nil-interface dance below is load-bearing: assigning a nil
	// *Proxy straight into an interface produces a NON-nil interface holding a
	// nil pointer, which egressDatapathWired would read as "wired" and then
	// dereference at Acquire.
	var socketSource egressSocketSource
	if o.egressProxy != nil {
		socketSource = o.egressProxy
	}
	o.containerOpts = append(o.containerOpts, withEgressDatapath(socketSource, o.egressForwarder))

	// The per-tenant mount-root resolver (change 0065), appended LAST — after
	// withTrustedRoot and after every caller-supplied option — for precisely the
	// reason withTrustedRoot itself is appended late: this decides what host
	// tree is bind-mounted into a container a model drives, and the trusted
	// side must have the last word on it.
	//
	// Appended UNCONDITIONALLY, including when the composition root declared no
	// resolver at all, on the same "the trusted side wins, including by saying
	// nothing" reasoning as the egress datapath above: with nothing declared
	// this writes a nil source, erasing anything an earlier option set, and the
	// handler falls back to the single trusted root — which is the pre-0065
	// behaviour and a byte-identical argv, NOT a degraded one.
	//
	// The nil-interface dance is load-bearing and is the same trap
	// egressDatapath documents: assigning a nil *TenantRoots straight into the
	// interface produces a NON-nil interface holding a nil pointer, which
	// Acquire's `h.tenantRoots != nil` would read as "a resolver is configured"
	// — turning every local, resolver-less deployment into one that mounts
	// nothing.
	var rootSource tenantRootSource
	if o.tenantRoots != nil {
		rootSource = o.tenantRoots
	}
	o.containerOpts = append(o.containerOpts, withTenantRoots(rootSource))

	s := &Service{
		cfg:    cfg,
		root:   o.root,
		hosted: o.hosted,
		lookup: o.lookup,
		gate:   newGate(cfg.Concurrency),
	}
	s.handler, s.refusal = selectHandler(cfg, o)
	// Stamp the selected substrate's bounded id onto the gate so a queued/refused
	// event names the handler that was full. Safe even on the refusal path
	// (HandlerName is "" then, and no Exec — hence no Admit — ever runs).
	s.gate.withHandlerName(s.HandlerName())
	return s, nil
}

// NewServiceFromRoot loads the off-switch config ONCE from root and selects the
// substrate for it.
//
// root must be a repo root the caller resolved and TRUSTS — never a path
// derived from a tool argument, a wire field, or model output, and never the
// process working directory of an agent that is free to chdir.
//
// It is used for TWO things, and the second is why it is retained rather than
// consumed: it locates the off-switch config, and it is the working tree the
// contained substrate bind-mounts (WithTrustedRoot). A model's `working_dir`
// selects a subdirectory of it and can never replace it.
//
// The returned Warnings are the operator's only signal that their config file
// did not do what they thought. The caller MUST log them; they are returned
// (rather than only stashed) so that ignoring them takes a deliberate `_`.
func NewServiceFromRoot(root string, opts ...ServiceOption) (*Service, []Warning, error) {
	cfg, warns := LoadConfig(root)

	// WithTrustedRoot goes LAST — after the caller's options — so the root this
	// function was trusted with is the one the substrate mounts, whatever else
	// was passed. Copied rather than appended in place: opts belongs to the
	// caller.
	withRoot := make([]ServiceOption, 0, len(opts)+1)
	withRoot = append(withRoot, opts...)
	withRoot = append(withRoot, WithTrustedRoot(root))

	svc, err := NewService(cfg, withRoot...)
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
		if s.refusal != nil {
			return nil, s.refusal
		}
		return nil, ErrRefusedUncontained
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

// TrustedRoot reports the working tree the composition root declared, which is
// the only host directory a contained command can be given. It is diagnostic:
// it reports the frozen decision, and nothing can be re-decided from it.
func (s *Service) TrustedRoot() string { return s.root }

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

// gate returns the process-scoped admission gate (change 0077). It satisfies the
// unexported PoolSource accessor, so a Pool built over this Service admits every
// Exec through the ONE host-wide gate rather than a per-loop one. Never nil after
// construction.
func (s *Service) gateFor() *Gate { return s.gate }

// NoteThreshold is the wait at or above which the model-visible backpressure
// note is attached, from the resolved config. Always positive.
func (s *Service) NoteThreshold() time.Duration {
	return durationOr(s.cfg.Concurrency.NoteThreshold, DefaultNoteThreshold)
}

// SetGateHooks installs the admission-emission observer on this Service's gate.
// It is called ONCE at the composition root, wherever the loop's EventStore is
// available, before the Service is used concurrently — mirroring how
// SandboxEventHooks is installed on the pool. A binding with no per-loop store
// installs nothing and the gate stays inert.
//
// A nil *Service is tolerated: NewBash(nil) is an explicitly supported
// fail-closed shape, and a composition root that wires the bash tool over a nil
// Service (no substrate selected) must be able to call this unconditionally, just
// as it can call NewBash(nil). A nil Service has no gate, so this is a no-op.
func (s *Service) SetGateHooks(h GateHooks) {
	if s == nil || s.gate == nil {
		return
	}
	s.gate.setHooks(h)
}

// Limits reports the resolved per-container cgroup caps, for diagnostics and for
// the composition root to log. The values are frozen at construction.
func (s *Service) Limits() Limits { return s.cfg.Limits }

// Warnings returns the diagnostics from the config load. The composition root
// MUST log them: they are how an operator learns their off-switch file was not
// honoured. Warning.Reason is a bounded enum, safe as a label.
func (s *Service) Warnings() []Warning {
	return append([]Warning(nil), s.warns...)
}
