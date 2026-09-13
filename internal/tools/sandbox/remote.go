package sandbox

// WHY THIS FILE IS IN PACKAGE sandbox AND NOT BESIDE ITS IMPLEMENTATION.
//
// The remote seam is the NETWORK contract — "fuse asks an orchestrator for the
// primitive" — as distinct from Handler/Runner, which is the IN-PROCESS contract
// — "fuse owns the primitive". ADR-0058 rule 1 keeps them separate and ADAPTS
// the first onto the second, so there stays exactly ONE bash-tool code path,
// which is what ADR-0044's "one substrate, one code path" was protecting.
//
// The adapter has to live HERE, in package sandbox, for a reason that is
// structural rather than stylistic: its Runner must satisfy the Pool's
// UNEXPORTED interfaces — principalScoped, mountScoped, containerIdentified —
// and EnvResetter. A Runner declared in another package can implement the
// exported ones and is then silently uncertifiable for warm reuse, which
// presents as "the pool never reuses a remote sandbox" rather than as a
// compile error.
//
// The seam itself is EXPORTED so an implementation (internal/tools/sandbox/
// kubernetes) can live outside this package and import it. The dependency runs
// one way only: the implementation imports sandbox, never the reverse. That is
// what keeps this package a leaf and what keeps client-go — or any other
// control-plane SDK — out of the package the bash tool depends on.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/ethanhinson/fuse/internal/loopauth"
)

// ErrSandboxGone reports that the remote sandbox behind a Runner no longer
// exists, so nothing can be executed through it.
//
// It is a distinct sentinel rather than a generic error because the CAUSE is
// something fuse did deliberately: a command exceeded its deadline and the
// sandbox was torn down to keep the run-`--rm` guarantee that a timed-out
// command leaves no process behind. A caller (and the Pool, on its next
// certification) must be able to tell "this context is spent, get a fresh one"
// apart from "the control plane is broken".
var ErrSandboxGone = errors.New("sandbox: the remote sandbox is gone")

// RemoteSubstrate is an isolation primitive fuse does NOT own and reaches over a
// control plane.
//
// Everything a substrate hands back is a CLAIM until fuse has checked it, which
// is the whole reason this seam is separate from Handler: on the local substrate
// containment is a flag fuse passes, and here it is an observation fuse must
// make. Provision's contract below is where that is pinned.
type RemoteSubstrate interface {
	// Name reports the substrate's bounded identifier ("kubernetes"), used as the
	// handler label on every sandbox event and metric. It is a closed enum value,
	// never free-form text derived from config or model output, and never "host".
	Name() string

	// Verify proves the substrate's network FLOOR actually holds — the canary
	// pair of ADR-0058 rule 3, where an unpoliced sandbox must reach a known
	// destination and a policed one must fail to reach the same. Only the pair
	// proves enforcement: a lone failure could be an absent destination.
	//
	// The adapter calls it ONCE, before the first Provision, and CACHES the
	// answer. A non-nil error makes every Acquire refuse for the handler's
	// lifetime — a cluster whose CNI does not enforce policy is disqualified,
	// exactly as a kvm-absent host is under ADR-0044's microVM rule, and it is
	// never re-probed in the hope of a different answer.
	Verify(ctx context.Context) error

	// Provision creates, CONFIRMS, and returns a sandbox for p.
	//
	// It returns ONLY a confirmed sandbox. An implementation must read back what
	// the substrate actually admitted, match it against the posture spec
	// demanded, and TEAR DOWN anything it cannot confirm before returning the
	// error (ADR-0058 rule 2). The adapter has no way to inspect the substrate's
	// object, so "confirmed" is a promise this method keeps or breaks; a
	// half-created sandbox that is merely forgotten is the failure mode the rule
	// exists to forbid.
	Provision(ctx context.Context, p loopauth.Principal, spec RemoteSpec) (RemoteSandbox, error)

	// Reap deletes ORPHANED sandboxes substrate-wide — those whose heartbeat is
	// older than staleAfter — and reports how many it deleted.
	//
	// It is substrate-wide and instance-agnostic BY DESIGN: an orphan exists
	// precisely when no fuse instance remembers it, so any live instance must be
	// able to collect any dead instance's leftovers. It must therefore be safe to
	// run concurrently from N instances, where "already deleted" is a success and
	// not an error.
	Reap(ctx context.Context, staleAfter time.Duration) (int, error)
}

// RemoteSandbox is one confirmed remote execution context.
//
// Every method takes a context because every one of them is a network call. None
// of them may block indefinitely on a dead control plane: the adapter calls
// Teardown from a Release that callers invoke on every early-return path.
type RemoteSandbox interface {
	// ID is the sandbox's bounded identifier ("<namespace>/<pod>" for
	// Kubernetes), reported as ContainerID on every sandbox event. It must be
	// bounded and non-secret: it reaches metric labels and event payloads.
	ID() string

	// MountRoot is the IN-SANDBOX workspace root ("/workspace"). It is the root
	// the adapter contains a model-supplied working_dir against, so it must be
	// the substrate's own trusted answer and never anything derived from the
	// model.
	MountRoot() string

	// Principal is the identity this sandbox was provisioned for. It is fixed for
	// the sandbox's life; a sandbox is never re-pointed at another principal.
	Principal() loopauth.Principal

	// Exec runs cmd under env, rooted at workingDir.
	//
	// workingDir arrives ALREADY CONTAINMENT-CHECKED by the adapter (through
	// resolveWorkspace against MountRoot) and is an in-sandbox absolute path. An
	// implementation must not re-derive it from anything the model supplied.
	//
	// env is the COMPLETE environment the command may observe, passed per Exec
	// rather than baked into the sandbox at provision time. That is what makes
	// the Pool's reset-on-checkout meaningful on a warm sandbox whose container
	// environment cannot be changed after creation.
	//
	// A ctx deadline TEARS THE SANDBOX DOWN — see remoteRunner.Exec.
	Exec(ctx context.Context, env Env, cmd, workingDir string) (Output, error)

	// Heartbeat records that this sandbox is still owned by a live instance, so
	// another instance's Reap does not collect it.
	Heartbeat(ctx context.Context) error

	// Teardown destroys the sandbox. It must be idempotent and must treat "it is
	// already gone" as success.
	Teardown(ctx context.Context) error
}

// RemoteSpec is the POSTURE fuse demands of a provisioned sandbox.
//
// It is a demand, not a description: Provision must read back what the substrate
// admitted and refuse any drift from these values (ADR-0058 rule 2). It is
// assembled by the adapter from the trusted, load-once Config and carries
// nothing derived from model output.
type RemoteSpec struct {
	// Image is the workload image reference the sandbox must run.
	Image string

	// Limits are the resolved per-sandbox resource caps (change 0077), mapped by
	// the implementation onto whatever the substrate expresses. A cap the
	// substrate cannot express is reported at LOAD time
	// (WarnLimitNotEnforceable), never silently dropped here.
	Limits Limits

	// Egress is the resolved network posture. The floor an implementation builds
	// from it is never lower than the metadata-deny floor, in EITHER mode
	// (ADR-0058 rule 4).
	Egress Egress

	// Workspace is the in-sandbox workspace root the sandbox must expose. It is
	// the substrate's trusted root, and the adapter contains working_dir against
	// what the provisioned sandbox actually REPORTS (MountRoot), not against this
	// request — a substrate that mounted somewhere else must not have its drift
	// papered over by fuse's own demand.
	Workspace string
}

// RemoteOption configures the adapter at construction.
type RemoteOption func(*remoteHandler)

// withRemoteTrustedRoot declares the HOST tree whose containment the adapter
// resolves a working_dir against (tests).
//
// It is unexported deliberately. On a remote substrate the workspace is the
// sandbox's own filesystem, so the trusted root a working_dir is contained
// against is the sandbox's MountRoot and not a host path — there is nothing for a
// composition root to declare. The option exists so a test can exercise the
// containment path with a real directory tree, since resolveWorkspace
// canonicalises against a real filesystem.
func withRemoteTrustedRoot(root string) RemoteOption {
	return func(h *remoteHandler) { h.root = root }
}

// withRemoteHeartbeatInterval overrides the per-Runner heartbeat period (tests).
func withRemoteHeartbeatInterval(d time.Duration) RemoteOption {
	return func(h *remoteHandler) {
		if d > 0 {
			h.heartbeatEvery = d
		}
	}
}

// WithRemoteReapHook installs the observer the orphan reaper reports through.
//
// It is EXPORTED, unlike the other options, for the reason WithProxyHooks is: the
// decision that a reaped orphan becomes a `sandbox.reap` event belongs to the
// composition root, not to this package, which stays a leaf with respect to
// emission. The hook is the SAME ReleaseInfo the Pool's Reaped hook carries, so
// the existing translator and the existing event shape serve both — the cause
// (CauseOrphan vs CauseIdleTTL) is what tells them apart.
//
// A nil hook is valid and means the count is dropped, which is the state before a
// composition root wires one.
func WithRemoteReapHook(fn func(ReleaseInfo)) RemoteOption {
	return func(h *remoteHandler) { h.reaped = fn }
}

// withRemoteReapInterval overrides the per-handler orphan-reaper period (tests).
func withRemoteReapInterval(d time.Duration) RemoteOption {
	return func(h *remoteHandler) {
		if d > 0 {
			h.reapEvery = d
		}
	}
}

// remoteHandler adapts a RemoteSubstrate onto Handler.
type remoteHandler struct {
	sub RemoteSubstrate
	cfg Config

	// name is the substrate's bounded identifier, captured at construction so
	// Name() cannot start reporting something else mid-process.
	name string

	// root is the host tree used only by tests; see withRemoteTrustedRoot. In
	// production it is "" and containment is resolved against the sandbox's own
	// MountRoot.
	root string

	heartbeatEvery time.Duration
	reapEvery      time.Duration

	// verifyOnce + verifyErr are the STICKY floor verdict. The refusal is cached
	// for the handler's lifetime: a substrate whose floor is unproven is
	// disqualified, not retried (ADR-0058 rule 3).
	verifyOnce sync.Once
	verifyErr  error

	health HealthHooks

	// reaped is the orphan reaper's observer seam (WithRemoteReapHook). Nil means
	// the count is dropped.
	reaped func(ReleaseInfo)

	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}
}

var _ Handler = (*remoteHandler)(nil)

// NewRemoteHandler adapts s onto the Handler seam, and starts the per-handler
// orphan reaper.
//
// It returns an error only for a substrate that cannot be used at all — a nil
// one, or one with no bounded name. It does NOT probe the substrate: Verify runs
// lazily at the first Acquire, so a control plane that is briefly unreachable at
// startup does not turn into a permanent refusal, and so construction stays
// non-blocking.
//
// The returned Handler owns a goroutine and must be Closed by whatever owns it,
// or the reaper leaks. The composition root's Service is immutable and has no
// shutdown, so registering this through WithHandlerFactory ties the reaper's
// life to the process — which is correct for a process-scoped substrate.
func NewRemoteHandler(s RemoteSubstrate, cfg Config, opts ...RemoteOption) (Handler, error) {
	if s == nil {
		return nil, errors.New("sandbox: remote handler needs a substrate")
	}
	name := s.Name()
	if name == "" {
		return nil, errors.New("sandbox: remote substrate must report a bounded Name")
	}
	if name == HandlerHost {
		// A remote substrate calling itself "host" would make Service.Contained()
		// report false for a substrate that IS contained, and would file its
		// events under the one handler label that means "uncontained".
		return nil, fmt.Errorf("sandbox: remote substrate must not name itself %q", HandlerHost)
	}

	idleTTL := cfg.IdleTTL
	if idleTTL <= 0 {
		idleTTL = DefaultIdleTTL
	}

	h := &remoteHandler{
		sub:  s,
		cfg:  cfg,
		name: name,
		// A third of the idle TTL: two heartbeats must land inside one TTL, or a
		// live sandbox looks stale to another instance's reaper between beats.
		heartbeatEvery: idleTTL / 3,
		// The reaper is a backstop against leaked substrate, not a scheduler, so
		// it wakes on the same clamped cadence the Pool's reaper uses.
		reapEvery: clampDuration(idleTTL/4, minReapInterval, maxReapInterval),
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(h)
		}
	}
	if h.heartbeatEvery <= 0 {
		h.heartbeatEvery = DefaultIdleTTL / 3
	}

	go h.reapLoop(idleTTL)
	return h, nil
}

// Name reports the substrate's bounded identifier.
func (h *remoteHandler) Name() string { return h.name }

// setHealthHooks satisfies healthObserved so SetHealthHooks reaches this
// handler exactly as it reaches the container one.
func (h *remoteHandler) setHealthHooks(hooks HealthHooks) { h.health = hooks }

// Close stops the orphan reaper. It is idempotent.
func (h *remoteHandler) Close() error {
	h.stopOnce.Do(func() {
		close(h.stop)
		<-h.done
	})
	return nil
}

// Acquire verifies the floor once, provisions a confirmed sandbox, and wraps it
// in a Runner.
//
// Every failure here is a REFUSAL wrapped in ErrRefusedUncontained. That
// wrapping is load-bearing: it is the error the bash tool already reports as
// "unavailable", and a caller that answers it by running the command another way
// is the one thing this package exists to make unwritable. Note what is NOT on
// any path below — any consideration of another substrate.
func (h *remoteHandler) Acquire(ctx context.Context, p loopauth.Principal, env Env) (Runner, error) {
	if err := h.verify(ctx); err != nil {
		// THE FLOOR IS UNPROVEN, so this substrate is disqualified. The health
		// event fires on EVERY refused Acquire rather than once at the first,
		// deliberately: the verdict is sticky and there is no recovery, so an
		// operator who missed the first event must still be able to see that a
		// configured Kubernetes handler is refusing everything and why. The reason
		// is its own closed value, not acquire_failed — a cluster whose CNI does
		// not enforce policy is a permanent, human-sized problem, and burying it in
		// the transient bucket is how it goes unnoticed.
		//
		// A caller-deadline expiry is excluded for the same reason the provision
		// path excludes it: that is the caller's bound firing, not the substrate
		// failing. Verify's own verdict is cached, so a deadline that fell during
		// the FIRST verify is the one case where the error is the caller's.
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			h.health.fire(HealthInfo{
				Principal: p,
				Handler:   h.name,
				Reason:    HealthFloorUnverified,
			})
		}
		return nil, fmt.Errorf("%w: %s floor unverified: %w", ErrRefusedUncontained, h.name, err)
	}

	sb, err := h.sub.Provision(ctx, p, h.spec())
	if err != nil {
		// acquire_failed is the honest reason: the substrate could not produce a
		// sandbox at all, so nothing this principal asked for can run. A
		// caller-deadline expiry is excluded for the same stated reason the
		// container handler excludes it — that is the caller's bound firing, not
		// the substrate failing.
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			h.health.fire(HealthInfo{
				Principal: p,
				Handler:   h.name,
				Reason:    HealthAcquireFailed,
			})
		}
		return nil, fmt.Errorf("%w: %s provision: %w", ErrRefusedUncontained, h.name, err)
	}
	if sb == nil {
		// A substrate returning (nil, nil) would otherwise become a nil-deref on
		// the first Exec. Fail closed instead.
		return nil, fmt.Errorf("%w: %s provision returned no sandbox", ErrRefusedUncontained, h.name)
	}

	r := &remoteRunner{
		handler:   h,
		sandbox:   sb,
		principal: p,
		// mount and id are SNAPSHOT at Acquire, not read through to the sandbox
		// on demand, for the reason containerRunner.root is: the Pool pins them
		// at the cold start and re-asserts them on every hit (certifyEntry), and
		// a value that can drift between two Execs of one checkout is a value
		// nobody can reason about.
		mount:         sb.MountRoot(),
		id:            sb.ID(),
		env:           env,
		heartbeatStop: make(chan struct{}),
	}
	go r.heartbeatLoop(h.heartbeatEvery)
	return r, nil
}

// verify runs the substrate's floor check at most once and caches the verdict.
func (h *remoteHandler) verify(ctx context.Context) error {
	h.verifyOnce.Do(func() { h.verifyErr = h.sub.Verify(ctx) })
	return h.verifyErr
}

// spec assembles the posture to demand from the trusted, frozen Config. Nothing
// here is derived from model output, from a wire field, or from a tool argument.
func (h *remoteHandler) spec() RemoteSpec {
	return RemoteSpec{
		Image:     h.cfg.Image,
		Limits:    h.cfg.Limits,
		Egress:    h.cfg.Egress,
		Workspace: containerWorkspace,
	}
}

// reapLoop collects orphans substrate-wide until Close.
//
// It lives on the HANDLER and not on the Pool, deliberately: a Pool's reaper
// tears down the entries that Pool remembers, and an orphan is by definition a
// sandbox no Pool remembers — the instance that owned it is gone. staleAfter is
// 2*IdleTTL so a sandbox whose owner is merely slow to beat is never collected
// out from under a live Exec.
func (h *remoteHandler) reapLoop(idleTTL time.Duration) {
	defer close(h.done)

	t := time.NewTicker(h.reapEvery)
	defer t.Stop()
	for {
		select {
		case <-h.stop:
			return
		case <-t.C:
			// A bounded context: a reap that hangs on a wedged control plane must
			// not wedge the reaper with it.
			ctx, cancel := context.WithTimeout(context.Background(), h.reapEvery)
			n, err := h.sub.Reap(ctx, 2*idleTTL)
			cancel()

			// The ERROR is still dropped: this is a backstop, the sandboxes it
			// collects are already unowned, and the substrate has its own
			// (activeDeadlineSeconds-class) backstop underneath it — so there is
			// nothing an operator would do with "one sweep failed" that the next
			// sweep does not do for them, and no honest reason for it in the closed
			// health enum.
			//
			// The COUNT is reported, once per collected orphan, with cause
			// `orphan`. That is the only signal there is that fuse instances are
			// dying without releasing their sandboxes: an orphan is by definition a
			// sandbox no Pool remembers, so the Pool's own reaper cannot see it and
			// idle_ttl never fires for it. ContainerID is deliberately EMPTY — Reap
			// reports how many it deleted, not which, and inventing an id would be
			// fabricating an observation (ADR-0056).
			if err == nil && n > 0 && h.reaped != nil {
				for range n {
					h.reaped(ReleaseInfo{
						Handler: h.name,
						Cause:   CauseOrphan,
					})
				}
			}
		}
	}
}

// remoteRunner is one acquired remote execution context.
//
// It implements Runner AND the four seams the Pool reaches for — EnvResetter,
// principalScoped, mountScoped, and containerIdentified. The last one had no
// implementor at all before this change (`docker run --rm` leaves no durable
// container), so a warm remote sandbox is the first thing to make the Pool's
// container-id path live.
type remoteRunner struct {
	handler *remoteHandler
	sandbox RemoteSandbox

	// principal is fixed at Acquire and never reassigned: a Runner belongs to
	// exactly one principal for its whole life, which is what lets the warm pool
	// re-assert ownership on checkout (acquiredFor).
	principal loopauth.Principal

	// id is the sandbox's bounded identifier, snapshot at Acquire.
	id string

	releaseOnce   sync.Once
	heartbeatStop chan struct{}

	// mu guards env, mount, and gone. env is not write-once (a pooled Runner is
	// re-environed on checkout while a previous Exec may still be unwinding), and
	// gone is written by whichever Exec hits a deadline.
	mu sync.Mutex
	// env is the COMPLETE environment the next Exec renders. It is kept as an Env
	// rather than pre-rendered strings because the substrate renders it itself
	// (there is no argv here to golden-test).
	env Env
	// mount is the in-sandbox workspace root, snapshot at Acquire. It is the root
	// a working_dir is contained against and the value the Pool re-asserts.
	mount string
	// gone records that the sandbox was torn down mid-life (a deadline). Once
	// set, every Exec refuses and Release is a no-op teardown.
	gone bool
}

var (
	_ Runner              = (*remoteRunner)(nil)
	_ EnvResetter         = (*remoteRunner)(nil)
	_ principalScoped     = (*remoteRunner)(nil)
	_ mountScoped         = (*remoteRunner)(nil)
	_ containerIdentified = (*remoteRunner)(nil)
)

// acquiredFor reports the principal this Runner was acquired for, so the Pool
// can verify against the RUNNER — not only against its own bookkeeping — that it
// is about to hand the right context to the right principal.
func (r *remoteRunner) acquiredFor() loopauth.Principal { return r.principal }

// mountRoot reports the workspace root this Runner's sandbox exposes, so the
// Pool can refuse a warm checkout whose mount no longer matches what it was
// certified with (change 0065's defence, carried onto a remote substrate).
func (r *remoteRunner) mountRoot() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.mount
}

// ContainerID reports the sandbox's bounded id. This is containerIdentified's
// FIRST real implementor: a warm remote sandbox outlives the command it ran, so
// unlike a `run --rm` container it has a durable identity worth reporting.
func (r *remoteRunner) ContainerID() string { return r.id }

// ResetEnv re-applies a freshly resolved environment to a warm Runner.
//
// On a remote sandbox the container's own environment cannot be changed after
// creation, so this is what makes the Pool's reset-on-checkout MEAN anything:
// the fresh allowlist is stored here and the next Exec renders it. Without it a
// warm sandbox would keep serving whatever it was first acquired with — a
// rotated credential lingering in every later command.
func (r *remoteRunner) ResetEnv(env Env) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.gone {
		// Reporting failure here is what makes the Pool DISCARD the entry rather
		// than hand out a Runner whose sandbox no longer exists.
		return ErrSandboxGone
	}
	r.env = env
	return nil
}

// Exec contains the working directory, then runs the command in the sandbox.
//
// ExitCode is -1 on every path where nothing ran, so a caller that reads only
// ExitCode still fails closed.
func (r *remoteRunner) Exec(ctx context.Context, cmd string, workingDir string) (Output, error) {
	r.mu.Lock()
	gone, env, mount := r.gone, r.env, r.mount
	r.mu.Unlock()

	if gone {
		return Output{ExitCode: -1}, fmt.Errorf("%w: %s", ErrSandboxGone, r.id)
	}

	// ONE containment implementation, shared with the container handler (change
	// 0075, task 1). The root and the mount point are the SAME value here: the
	// sandbox's workspace is its own filesystem, so the trusted root a
	// working_dir resolves against IS the in-sandbox mount root. h.root is
	// non-empty only in tests, which need a real directory tree because
	// resolveWorkspace canonicalises against a real filesystem.
	root := mount
	if r.handler != nil && r.handler.root != "" {
		root = r.handler.root
	}
	_, workdir, err := resolveWorkspace(root, workingDir, mount)
	if err != nil {
		// A containment refusal, decided BEFORE the control plane is touched: no
		// exec is issued, so nothing runs. Reported as a substrate failure
		// (ExitCode -1, non-nil error) because that is what it is.
		return Output{ExitCode: -1}, err
	}

	out, execErr := r.sandbox.Exec(ctx, env, cmd, workdir)
	timedOut := errors.Is(ctx.Err(), context.DeadlineExceeded)

	if timedOut {
		// THE RUN-`--rm` GUARANTEE, CARRIED ACROSS A NETWORK.
		//
		// A timed-out command must never leave a process behind. On the local
		// substrate `--rm` gives that for free. Here the sandbox OUTLIVES the
		// command by design (it is warm), so the only way to keep the guarantee
		// is to destroy the sandbox — and to do it by construction rather than by
		// trusting an in-image `timeout` binary the model's own command could
		// interfere with. The cost is one lost warm sandbox.
		//
		// The teardown context is deliberately NOT ctx: ctx is the expired one,
		// and a teardown issued on it would be cancelled before it left the
		// process, leaking exactly the sandbox this branch exists to destroy.
		r.markGone()
		tctx, cancel := context.WithTimeout(context.Background(), remoteTeardownTimeout)
		_ = r.sandbox.Teardown(tctx)
		cancel()

		out.TimedOut = true
		if out.ExitCode == 0 {
			// A killed command reporting success is not a result we pass on.
			out.ExitCode = -1
		}
		// The deadline is the CALLER's bound firing, already reported as
		// Output.TimedOut, so it is not an error and not a health event — see
		// classifyExit's note on the same decision for the container handler.
		return out, nil
	}

	if execErr != nil {
		out.ExitCode = -1
		return out, fmt.Errorf("%s: %w", r.handler.name, execErr)
	}
	return out, nil
}

// remoteTeardownTimeout bounds a teardown issued on a context of our own making
// (the deadline and release paths). It is generous enough for a control-plane
// delete and short enough that a wedged control plane cannot hold a caller.
const remoteTeardownTimeout = 15 * time.Second

// markGone records that the sandbox no longer exists.
func (r *remoteRunner) markGone() {
	r.mu.Lock()
	r.gone = true
	r.mu.Unlock()
}

// Release stops the heartbeat and tears the sandbox down.
//
// It is idempotent (callers legitimately release twice — an explicit call plus a
// defer), and a Runner already marked gone tears down NOTHING: the sandbox is
// already deleted, and a second delete would target a name that may by then
// belong to someone else.
func (r *remoteRunner) Release(ctx context.Context) error {
	var err error
	r.releaseOnce.Do(func() {
		close(r.heartbeatStop)

		r.mu.Lock()
		gone := r.gone
		r.gone = true
		r.mu.Unlock()
		if gone {
			return
		}

		// A teardown must not be denied by a caller whose context is already
		// cancelled — Release is called from error paths and defers, where an
		// expired ctx is the norm rather than the exception, and a teardown
		// skipped there is a leaked sandbox.
		tctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), remoteTeardownTimeout)
		defer cancel()
		err = r.sandbox.Teardown(tctx)
	})
	return err
}

// heartbeatLoop keeps the sandbox looking owned until Release.
//
// It exists because Reap is instance-agnostic: without a beat, another
// instance's reaper would collect a sandbox that is serving a live Exec. It stops
// at Release, which is equally load-bearing in the other direction — a heartbeat
// that outlived its sandbox would keep an orphan looking alive forever.
func (r *remoteRunner) heartbeatLoop(every time.Duration) {
	if every <= 0 {
		return
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-r.heartbeatStop:
			return
		case <-t.C:
			r.mu.Lock()
			gone := r.gone
			r.mu.Unlock()
			if gone {
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), every)
			// Errors are dropped: a missed beat is recoverable (the next one
			// lands well inside 2*IdleTTL), and there is no honest health reason
			// for it in the closed enum.
			_ = r.sandbox.Heartbeat(ctx)
			cancel()
		}
	}
}
