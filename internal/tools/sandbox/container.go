package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ethanhinson/fuse/internal/loopauth"
)

// DefaultContainerImage is the image used when the operator configured none.
//
// It is deliberately minimal (a shell, coreutils, and nothing else) and it is
// PINNED to a tag rather than floating on :latest, so that what a sandboxed
// command can reach does not change underneath an operator between two runs.
const DefaultContainerImage = "alpine:3.20"

// containerWorkspace is the fixed in-container mount point and working
// directory. It is a constant rather than a mirror of the host path so that
// nothing about the host's directory layout is disclosed to a command, and so
// argv is identical regardless of where the repo happens to live.
const containerWorkspace = "/workspace"

// containerCLIs are the supported OCI CLIs, in PREFERENCE ORDER. The order is
// load-bearing and deliberate: docker is the overwhelmingly common case, and
// nerdctl and podman are both argv-compatible with the subset used here
// (`run --rm -i --env -v -w`), which is why one argv builder serves all three.
var containerCLIs = [...]string{"docker", "nerdctl", "podman"}

// containerClientPassthrough are the variables the CLI BINARY needs to find and
// address a daemon. They are added on top of the base allowlist for the client
// process only, and never reach the container: the sandboxed command's
// environment is exactly what the caller resolved, passed as --env K=V.
var containerClientPassthrough = [...]string{
	"DOCKER_HOST",        // docker daemon socket / remote endpoint
	"DOCKER_CONFIG",      // docker client config dir (registry auth)
	"DOCKER_CONTEXT",     // selected docker context
	"CONTAINER_HOST",     // podman remote endpoint
	"CONTAINERD_ADDRESS", // nerdctl containerd socket
	"XDG_RUNTIME_DIR",    // rootless socket discovery for podman/nerdctl
}

// ErrNoContainerRuntime reports that none of the supported container CLIs is on
// PATH, so the container substrate cannot be constructed.
//
// This is returned rather than quietly degrading to the host handler. A silent
// fallback would mean that uninstalling docker turns containment off, which is
// exactly the fail-open direction ADR-0044 forbids. Selection (T5) decides what
// to do with this error; it is never this package's job to answer it by running
// the command somewhere less safe.
var ErrNoContainerRuntime = errors.New("no container runtime available")

// errPullFailed marks an Acquire that failed at the pre-pull. It is unexported
// and carries no caller-facing meaning: its only job is to let the pool tell a
// pull failure apart from every other cold-start failure, so ONE incident is
// reported under ONE reason (pull_failed, at the site that observed it) rather
// than being counted again as acquire_failed one frame up.
var errPullFailed = errors.New("sandbox: image pre-pull failed")

// ErrWorkingDirRefused reports that a model-supplied working_dir did not
// resolve to a directory INSIDE the trusted mount.
//
// ADR-0044 requires that "the model-supplied `working_dir` resolves *within*
// the mount and cannot escape it", and that the root of trust "comes from the
// authenticated loop-start context, never from model output (not the `command`,
// not `working_dir`)". Letting working_dir choose the bind-mount SOURCE would
// invert that: a model naming "/" or an operator's home directory would mount
// that subtree into a container it controls, recovering by filesystem exactly
// the credential access the env-scrub closed. So working_dir is a SUBPATH
// request against a mount it cannot influence, and a request that does not
// resolve inside the mount is refused before anything is executed.
var ErrWorkingDirRefused = errors.New("working_dir must resolve inside the sandbox workspace")

// ErrNoTrustedRoot reports that no trusted mount root was supplied, so there is
// no workspace for a working_dir to be relative TO.
//
// This is the degraded state a composition root reaches only when it could not
// resolve a repo root at all (see cmd/fuse's os.Getwd fallback, which also
// makes LoadConfig warn). The safe answer is to mount nothing and refuse the
// working_dir — never to promote the model's path to a mount source because no
// trusted one was available.
var ErrNoTrustedRoot = errors.New("no trusted workspace root is mounted")

// execRunner runs one command to completion and reports its combined output.
//
// It is injected so that argv construction — the part of this handler that
// carries the security-relevant decisions — is unit-testable with no container
// daemon anywhere in sight.
//
// Its contract mirrors the substrate contract one level down: the returned
// error is non-nil ONLY when the command could not be started at all. A command
// that started and exited non-zero returns its code with a nil error, and the
// implementation must never report a zero code alongside a non-nil error.
type execRunner func(ctx context.Context, name string, args ...string) ([]byte, int, error)

// containerOption configures a containerHandler at construction.
type containerOption func(*containerHandler)

// withLookPath overrides PATH probing (tests, and any caller that must resolve
// against something other than the ambient PATH).
func withLookPath(fn func(string) (string, error)) containerOption {
	return func(h *containerHandler) {
		if fn != nil {
			h.lookPath = fn
		}
	}
}

// withExecRunner overrides how the CLI is invoked.
func withExecRunner(fn execRunner) containerOption {
	return func(h *containerHandler) {
		if fn != nil {
			h.run = fn
		}
	}
}

// withContainerEnvLookup overrides how the container CLI CLIENT's own
// passthrough variables (containerClientPassthrough) are resolved. It is the
// same seam Service.WithEnvLookup governs for the sandboxed command's
// environment, plumbed through so ONE lookup governs both halves rather than
// the client half silently re-reading the real process environment on every
// Exec.
func withContainerEnvLookup(fn func(string) (string, bool)) containerOption {
	return func(h *containerHandler) {
		if fn != nil {
			h.envLookup = fn
		}
	}
}

// withLimits sets the per-container cgroup caps this handler renders into argv
// (change 0077).
//
// SECURITY-CRITICAL: like withTrustedRoot, the caps come from the COMPOSITION
// ROOT — resolved once from trusted operator config at Service construction,
// before any model has run, with posture defaults already applied. They must
// never be derived from a tool argument, a wire field, working_dir, or model
// output: a cap a model could influence is a cap a model could raise, and a cap
// a model can raise is not a cap. Applied once, at construction; no method
// changes them afterwards.
func withLimits(l Limits) containerOption {
	return func(h *containerHandler) { h.limits = l }
}

// withEgress sets the resolved egress posture this handler renders into argv
// (change 0064).
//
// SECURITY-CRITICAL: like withLimits and withTrustedRoot, the posture comes from
// the COMPOSITION ROOT — resolved once from trusted operator config at Service
// construction, before any model has run — and it is applied LAST in the options
// chain (see NewService) so no caller-supplied containerOption can downgrade
// EgressEnforce back to EgressAllowAll. It must never be derived from a tool
// argument, a wire field, working_dir, or model output: a floor the model can
// select is not a floor. Applied once, at construction; no method changes it
// afterwards.
func withEgress(e Egress) containerOption {
	return func(h *containerHandler) { h.egress = e }
}

// withEgressDatapath supplies the two halves of the hole through the
// `--network none` floor (change 0064, task 6): the source of the principal's
// host-side proxy socket, and the host path of the statically linked forwarder
// binary that is bind-mounted into the container to reach it.
//
// SECURITY-CRITICAL, and for a sharper reason than its neighbours. The floor is
// a subtraction — the worst a lost withEgress does is leave the container with
// the network it had before 0064. This is an ADDITION: it names a binary fuse
// bind-mounts into the container and then EXECUTES as the command's parent
// process, and a socket that is one principal's authenticated path off the box.
// A caller who could supply either would be choosing what runs inside the
// sandbox and whose policy it runs under. So, like withTrustedRoot and
// withEgress, both values come from the COMPOSITION ROOT and are applied LAST in
// the options chain (see NewService / WithEgressProxy), and neither this option
// nor the interface it takes is exported.
//
// Both halves are required: with either missing the datapath is not emitted at
// all and enforcement stays deny-all (see resolveForwarderBinary), which is the
// fail-closed direction.
func withEgressDatapath(src egressSocketSource, forwarderPath string) containerOption {
	return func(h *containerHandler) {
		h.egressSockets = src
		h.egressForwarder = forwarderPath
	}
}

// withTrustedRoot sets the host directory this handler bind-mounts.
//
// SECURITY-CRITICAL: the value comes from the COMPOSITION ROOT — the repo root
// resolved at startup, before any model has run (see Service.root and
// NewServiceFromRoot). It must never be derived from a tool argument, a wire
// field, or model output. It is applied once, at construction; there is no
// method that can change it afterwards.
func withTrustedRoot(root string) containerOption {
	return func(h *containerHandler) { h.root = root }
}

// tenantRootSource yields the HOST directory ONE principal's containers are
// allowed to see — the bind-mount source, chosen as a function of
// Principal.Tenant and nothing else (change 0065).
//
// The interface is UNEXPORTED, and so is the option that accepts one
// (withTenantRoots), for exactly the reason egressSocketSource is: a
// caller-suppliable root source IS a caller-suppliable bind-mount into the
// container, which is the hole this package exists to close. The exported seam
// lives at the composition root, where host layout policy belongs; the package
// itself stays layout-agnostic.
//
// The Principal handed in is the AUTHENTICATED one, established at the Connect
// edge and fixed at Acquire. It is never derived from command, working_dir, or
// any other tool argument: a tenant the model can select is not an isolation
// boundary.
//
// An implementation that cannot resolve a root returns ("", err) or ("", nil).
// Both are DEGRADED-SAFE and mean "mount nothing" — never a shared root, never
// a parent of some other tenant's tree.
//
// THE MICROVM BINDING. This seam is the container expression of a boundary
// ADR-0044's 2026-08-16 Update requires of every substrate, including the
// microVM handler that does not exist yet. The three conditions such a handler
// must satisfy — a per-tenant VM-native backing (a virtio-fs share OR a block
// image, one mechanism), a HOST-side canonicalise-then-compare working_dir
// check rather than a guest-side one, and warm/snapshot pools that stay
// strictly per-principal and reset — are recorded in full beside the seam-
// conformance stub, in microvm_conformance_test.go ("THE MICROVM FILESYSTEM
// CONTRACT"). They are written down there rather than rediscovered when the
// handler is built.
type tenantRootSource interface {
	Root(loopauth.Principal) (string, error)
}

// withTenantRoots supplies the per-tenant mount-root resolver (change 0065).
//
// SECURITY-CRITICAL, and for the same reason as withTrustedRoot: this names the
// host directory fuse bind-mounts into a container the model drives. The value
// comes from the COMPOSITION ROOT, resolved from trusted operator config before
// any model has run, and is applied LAST in the options chain so no
// caller-supplied containerOption can swap the isolation boundary out from
// under it. Applied once, at construction; no method changes it afterwards.
func withTenantRoots(src tenantRootSource) containerOption {
	return func(h *containerHandler) { h.tenantRoots = src }
}

// containerHandler runs commands inside a throwaway OCI container.
//
// It is the DEFAULT substrate. Detection happens once, at construction, so that
// the answer to "is containment available" is settled before any command is
// eligible to run — never mid-Exec, where the only remaining options would be
// to fail late or to fall back.
type containerHandler struct {
	// runtime is the detected CLI name ("docker"|"nerdctl"|"podman"). It is a
	// bounded value drawn from containerCLIs, safe as an event/metric label.
	runtime string
	// image is the resolved image reference (config, or the pinned default).
	image string

	// root is the TRUSTED bind-mount source: the one host directory any
	// command run through this handler can see, mounted at containerWorkspace.
	// It is resolved once at construction from the composition root's repo root
	// and is never model-derived. "" means no trusted root was supplied, which
	// is the degraded-but-safe state: nothing is mounted and any model-supplied
	// working_dir is refused rather than becoming a mount source itself.
	root string

	// limits are the per-container cgroup caps (change 0077), resolved once at
	// construction from trusted operator config with posture defaults already
	// applied. Every field is optional: an unset field emits no flag. Never
	// model-derived; see withLimits.
	limits Limits

	// egress is the resolved egress posture (change 0064), settled once at
	// construction from trusted operator config. Its zero value is
	// EgressAllowAll, which renders no network flag at all — byte-for-byte the
	// pre-0064 argv. Never model-derived; see withEgress.
	egress Egress

	// egressSockets yields the per-principal host socket the container's
	// forwarder relays to, and egressForwarder is the canonicalised host path of
	// that forwarder binary. Both are resolved once at construction from the
	// composition root; either being absent means NO datapath is emitted and
	// EgressEnforce stays deny-all. Never model-derived; see withEgressDatapath.
	egressSockets   egressSocketSource
	egressForwarder string

	// tenantRoots resolves the per-principal bind-mount source (change 0065).
	// Nil — the default, and the whole local single-tenant path — means the
	// handler's single trusted root is used unchanged, so an operator who never
	// configured a resolver gets byte-for-byte today's argv. Never
	// model-derived; see withTenantRoots.
	tenantRoots tenantRootSource

	// pullOnce guards the single-flight pre-pull, and pullErr records its
	// outcome. A failed pull is retried on a later Acquire rather than cached as
	// a permanent failure, so pullOnce is reset on failure (see prePull).
	pullMu   sync.Mutex
	pulling  chan struct{} // non-nil while a pull is in flight; closed when it finishes
	pullDone bool          // true once a pull has SUCCEEDED; then never pulled again
	pullErr  error         // last pull's error, read under pullMu

	// envLookup resolves the CLI client's own passthrough variables
	// (containerClientPassthrough). Nil means the real process environment.
	// This governs the SAME seam as the sandboxed command's environment
	// (Service.lookup / WithEnvLookup) — see withContainerEnvLookup — so one
	// seam controls both halves of what "the environment is frozen" claims.
	envLookup func(string) (string, bool)

	// health is the substrate-health observer (change 0065, task 7). Installed
	// once at the composition root via Service.SetHealthHooks, before the
	// handler is used concurrently, and never written again — same discipline
	// as the gate's hooks. The zero value is inert.
	health HealthHooks

	lookPath func(string) (string, error)
	run      execRunner
}

// newContainerHandler detects a container CLI and returns the handler bound to
// it. It returns a nil handler and an ErrNoContainerRuntime-wrapping error when
// none of the supported CLIs is available: a nil handler on the error path is
// what makes "use it anyway" unwritable at the call site.
func newContainerHandler(cfg Config, opts ...containerOption) (*containerHandler, error) {
	h := &containerHandler{
		image:    strings.TrimSpace(cfg.Image),
		lookPath: exec.LookPath,
	}
	if h.image == "" {
		h.image = DefaultContainerImage
	}
	for _, opt := range opts {
		opt(h)
	}
	// Resolved ONCE, here, at construction — before any model has run — so that
	// the mount source is a settled fact for the life of the handler rather
	// than something re-derived per Exec from whatever the filesystem looks
	// like by then.
	h.root = resolveMountRoot(h.root)
	// Same discipline for the forwarder binary, and for the same reason: the
	// artifact fuse mounts and executes inside the container is proved to exist
	// ONCE, here, rather than being re-stat'ed per Exec — or, worse, handed to
	// the daemon unproven, which would have it invented as a root-owned
	// directory. "" means no datapath; see resolveForwarderBinary.
	h.egressForwarder = resolveForwarderBinary(h.egressForwarder)
	// Applied only if no test injected its own execRunner (withExecRunner sets
	// h.run directly), so the client-env lookup wiring never overrides an
	// explicit test double.
	if h.run == nil {
		h.run = h.runClientCommand
	}

	for _, cli := range containerCLIs {
		if _, err := h.lookPath(cli); err == nil {
			h.runtime = cli
			return h, nil
		}
	}
	return nil, fmt.Errorf("%w: looked for %s on PATH", ErrNoContainerRuntime, strings.Join(containerCLIs[:], ", "))
}

// resolveMountRoot canonicalises the trusted mount source.
//
// It returns "" — meaning "mount nothing" — for anything it cannot prove is an
// existing directory, because a bind-mount source the daemon has to invent is
// not a working tree; docker would silently create it (as root), and the model
// would get an empty workspace that LOOKS mounted. Symlinks are resolved here
// so that the later working_dir containment check compares two canonical paths:
// a root reached through a symlink would make an in-tree path look like an
// escape, and — worse — a root and a candidate canonicalised differently could
// make an escape look in-tree.
func resolveMountRoot(root string) string {
	root = strings.TrimSpace(root)
	if root == "" {
		return ""
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return ""
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return ""
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		return ""
	}
	return resolved
}

// Name reports the bounded handler identifier.
func (*containerHandler) Name() string { return HandlerContainer }

// Runtime reports which container CLI was detected at construction.
func (h *containerHandler) Runtime() string { return h.runtime }

// setHealthHooks satisfies healthObserved. Written once at the composition root
// before concurrent use, never afterwards — the same discipline every other
// field on this handler follows.
func (h *containerHandler) setHealthHooks(hooks HealthHooks) { h.health = hooks }

// tenantScoped satisfies the tenantScoped interface Service.TenantScoped reads
// through. It reports the substrate's ACTUAL posture — whether Acquire will
// resolve a per-principal mount source — rather than whether an option was once
// passed, so a resolver lost to the trusted-last ordering or to the
// nil-interface trap reports false. That is what makes the composition root's
// wiring assertion honest rather than a restatement of its own argument.
func (h *containerHandler) tenantScoped() bool { return h != nil && h.tenantRoots != nil }

// Acquire returns a Runner bound to p and to env.
//
// The environment is rendered here, once, from the allowlist the caller
// resolved. The Runner never consults the process environment afterwards, which
// is what makes it structurally impossible for an ambient variable to reach a
// container.
func (h *containerHandler) Acquire(ctx context.Context, p loopauth.Principal, env Env) (Runner, error) {
	// Bounded, single-flight pre-pull (change 0077). With --pull=never on the run
	// argv, the image must be acquired here or `run` fails; doing it under the
	// pull's own timeout is what keeps a cold image from hanging an Exec on the
	// command's own deadline. A failed pull is an ACQUIRE failure — it is
	// reported so selection surfaces "pull_failed" — and is retried on a later
	// Acquire rather than cached as permanent.
	if err := h.prePull(ctx); err != nil {
		// The one place pull_failed can be observed honestly: the image
		// acquisition itself failed, so this substrate can start no container
		// for anyone until a later pull succeeds. The error is NOT carried into
		// the hook — only the closed reason is (see HealthInfo).
		//
		// A caller-deadline expiry is excluded: prePull returns ctx.Err() when
		// the CALLER ran out of time while the pull continues in the background,
		// and that is the caller's bound firing, not the substrate failing.
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			h.health.fire(HealthInfo{
				Principal: p,
				Handler:   HandlerContainer,
				Runtime:   h.runtime,
				Reason:    HealthPullFailed,
			})
		}
		return nil, fmt.Errorf("%s pull %s: %w: %w", h.runtime, h.image, errPullFailed, err)
	}

	// The datapath's one per-run value (change 0064, task 6): the HOST path of
	// THIS principal's proxy socket.
	//
	// It is resolved HERE, at Acquire, and not in argv, because that is where the
	// principal is — argv is built per Exec from a Runner whose principal was
	// fixed at Acquire and is never reassigned, so a socket resolved here belongs
	// to exactly one principal for the Runner's whole life. Proxy.Listen is
	// idempotent per principal, so concurrent sandboxes for one principal share
	// one socket and a pooled Runner re-checked-out for its own principal
	// (pool.go's certifyPrincipal forbids any other) gets the same one back.
	//
	// A failure is an ACQUIRE failure, loudly. The alternative — degrading to the
	// datapath-less deny-all — would turn "the proxy could not open a socket"
	// into "every network call in this loop mysteriously fails", which is the
	// same containment but an unreadable one.
	socket := ""
	if h.egressDatapathWired() {
		var err error
		socket, err = h.egressSockets.Listen(p, h.egress)
		if err != nil {
			return nil, fmt.Errorf("sandbox: open egress socket: %w", err)
		}
	}

	// The mount source for THIS principal (change 0065).
	//
	// Resolved HERE, at Acquire, and for exactly the reason the egress socket
	// above is: this is where the AUTHENTICATED Principal is in hand. The
	// trust direction is the whole point — the tenant comes from
	// Principal.Tenant, established at the Connect edge before this package is
	// reached, and never from `command`, from `working_dir`, or from any other
	// tool argument. A tenant a model could name is a tenant a model could
	// switch, and a switchable tenant is not an isolation boundary. argv is
	// built per Exec from a Runner whose principal was fixed at Acquire and is
	// never reassigned, so a root resolved here belongs to exactly one
	// principal for that Runner's whole life and cannot drift between Execs.
	//
	// With no resolver configured — the default, and the whole local
	// single-tenant path — this is skipped entirely and the handler's single
	// trusted root is used unchanged, so argv stays byte-for-byte today's.
	root := h.root
	if h.tenantRoots != nil {
		resolved, err := h.tenantRoots.Root(p)
		if err != nil {
			// A resolver ERROR is an ACQUIRE failure, loudly — the same posture
			// the egress socket takes just above, and for the same reason. The
			// alternative, degrading to some other root, is the one outcome
			// forbidden here: it would run this tenant's commands against a
			// tree it was never granted, which is a cross-tenant disclosure
			// dressed up as resilience. Degrading to NO root would be safe but
			// unreadable ("my workspace is mysteriously empty"), so the error
			// is surfaced instead.
			return nil, fmt.Errorf("sandbox: resolve tenant mount root: %w", err)
		}
		// One canonicalisation, through the SAME function that canonicalises
		// the single trusted root at construction — never a second
		// canonicaliser. The later working_dir containment check compares a
		// canonical candidate against this root, and two paths canonicalised
		// differently is precisely how an escape comes to look in-tree.
		//
		// "" — whether the resolver returned it, or resolveMountRoot rejected
		// what it did return — is DEGRADED-SAFE and means "mount nothing". It
		// must never fall back to h.root, to a shared root, or to a parent of
		// some other tenant's tree: an unmounted container is a broken
		// workspace, a wrongly-mounted one is a disclosure, and only one of
		// those is recoverable.
		root = resolveMountRoot(resolved)
	}

	return &containerRunner{
		handler:      h,
		principal:    p,
		env:          renderEnv(env),
		egressSocket: socket,
		root:         root,
	}, nil
}

// egressDatapathWired reports whether BOTH halves of the hole are present under
// an enforcing posture. All three conditions are required, and the conjunction is
// the fail-closed statement: no mode, no source, or no forwarder binary means no
// datapath is emitted and enforcement is deny-all.
func (h *containerHandler) egressDatapathWired() bool {
	return h.egress.Mode == EgressEnforce && h.egressSockets != nil && h.egressForwarder != ""
}

// containerRunner is one principal's container execution context.
type containerRunner struct {
	handler *containerHandler
	// principal is fixed at Acquire and never reassigned: a Runner belongs to
	// exactly one principal for its whole life, which is what lets the warm
	// pool re-assert ownership on checkout (see acquiredFor).
	principal loopauth.Principal

	// egressSocket is the HOST path of this principal's proxy socket, resolved
	// once at Acquire alongside principal and equally immutable. "" means no
	// datapath was wired, in which case argv emits none of it.
	//
	// Acquire took a LEASE on that listener and this Runner holds it until
	// Release. It is deliberately not a close: the listener is per-PRINCIPAL and
	// several sandboxes may share it, so closing it here would cut a live tunnel
	// belonging to another Exec. Dropping the lease lets the proxy tear it down
	// when the LAST holder lets go — see Release below and Proxy.Release.
	egressSocket string

	// root is the HOST directory THIS Runner's containers bind-mount, resolved
	// once at Acquire alongside principal and equally immutable (change 0065).
	// With no tenant resolver configured it is the handler's single trusted
	// root; with one, it is the tree that resolver granted this principal's
	// tenant.
	//
	// It lives on the Runner rather than being re-derived per Exec so that it
	// is a settled fact for the Runner's whole life: a root re-resolved inside
	// argv could drift between two Execs of one checked-out Runner, and a mount
	// that changes underneath a principal is a mount nobody can reason about.
	// "" means mount nothing — the degraded-safe state — and is never a
	// substitute root; see workspace.
	root string

	// releaseOnce makes the lease drop happen at most once however many times
	// Release is called. Callers legitimately release twice (bash.go's explicit
	// call plus its defer), and a second drop would be a lease this Runner never
	// held — which, if some other sandbox had since taken one, would tear down a
	// listener that is in use.
	releaseOnce sync.Once

	// mu guards env. A pooled Runner is re-environed on checkout (ResetEnv)
	// while a previous Exec may still be unwinding, so the environment is not
	// write-once even though it is set-once per checkout.
	mu sync.Mutex
	// env is the COMPLETE environment, pre-rendered as sorted K=V strings by
	// renderEnv. Sorting is what makes argv deterministic and golden-testable.
	env []string
}

// acquiredFor reports the principal this Runner was acquired for, so a pool can
// verify — against the Runner itself, not only against its own bookkeeping —
// that it is about to hand the right context to the right principal.
func (r *containerRunner) acquiredFor() loopauth.Principal { return r.principal }

// mountRoot reports the HOST directory this Runner's containers bind-mount, so
// a pool can verify — against the Runner itself, not only against its own
// bookkeeping — that a warm checkout still mounts the tree it was certified
// with (change 0065; see pool.go's certifyEntry).
//
// It reads the immutable field set at Acquire and takes no lock, because there
// is nothing to lock: root is written once, before this Runner is visible to
// any other goroutine, and never reassigned. If that ever stops being true,
// this accessor is where the guard belongs.
func (r *containerRunner) mountRoot() string { return r.root }

// ResetEnv re-applies a freshly resolved environment to a warm Runner.
//
// It exists for the pool's reset-on-checkout: a Runner that outlives one
// checkout must observe the CURRENT allowlist on the next, not a snapshot taken
// when it was first acquired, or a rotated credential would linger in argv.
func (r *containerRunner) ResetEnv(env Env) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.env = renderEnv(env)
	return nil
}

// currentEnv snapshots the rendered environment. It never returns nil.
func (r *containerRunner) currentEnv() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.env == nil {
		return []string{}
	}
	return append([]string(nil), r.env...)
}

// workspace resolves the bind-mount SOURCE and the in-container working
// directory for one Exec.
//
// This is where ADR-0044's containment constraint is enforced, and the split it
// makes is the whole point:
//
//   - The mount source is ALWAYS the trusted root passed in as `root`. It is
//     not a function of workingDir, and there is no branch on which model
//     output can reach it. This is what makes "mount my home directory"
//     unwritable.
//   - The model-supplied workingDir is a SUBPATH REQUEST resolved against that
//     root. It moves -w and nothing else, so the worst a hostile value can do
//     is name a directory the container was already going to be able to see.
//   - Anything that does not resolve inside the root is REFUSED, not clamped.
//     Silently rewriting an escape to the root would run a command somewhere
//     the caller did not ask for, which is how "cd /etc && rm -rf ." becomes a
//     surprise in the repo.
//
// Note that the returned workdir is a CONTAINER path. The runtime resolves it
// inside the container's own namespace, so a symlink the model plants in the
// tree after this check can only redirect -w to another path inside the mount —
// there is no TOCTOU window between here and the mount that reaches the host.
// The host-side canonicalisation below is what closes the window that DOES
// exist: a symlink already in the tree pointing out of it.
//
// The root is a PARAMETER rather than a field read (change 0065). That is the
// entire per-tenant change to this function: WHICH root is mounted is settled
// at Acquire, from the authenticated Principal.Tenant, and handed in already
// canonicalised (see resolveMountRoot). Everything below — the canonical
// comparison, EvalSymlinks, the filepath.Rel + ".." rejection, the
// non-directory refusal, the refusal to disclose host paths — is UNCHANGED and
// must stay that way: the containment algorithm is not reimplemented in order
// to be made tenant-aware, it is simply pointed at a narrower root.
//
// Consequently a caller MUST pass an already-canonicalised root. Both call
// paths do: h.root is canonicalised at construction, and the per-tenant root at
// Acquire, through that same one function.
func (h *containerHandler) workspace(root string, workingDir string) (mount string, workdir string, err error) {
	workingDir = strings.TrimSpace(workingDir)

	if root == "" {
		if workingDir != "" {
			// The one thing we must not do here is fall back to mounting the
			// model's path because we have no trusted one.
			return "", "", fmt.Errorf("%w: cannot place working_dir %q", ErrNoTrustedRoot, workingDir)
		}
		return "", containerWorkspace, nil
	}

	// THE DEFAULT, and the common case: no working_dir at all still mounts the
	// working tree. An unmounted container is an empty box the agent cannot
	// work in (ADR-0044: "The working tree must be mounted in for the model to
	// see the repo it edits").
	if workingDir == "" {
		return root, containerWorkspace, nil
	}

	// A relative working_dir is relative to the workspace — the only root the
	// model has any business naming a path against.
	candidate := workingDir
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(root, candidate)
	}

	// Canonicalise before comparing. A prefix test against an uncanonicalised
	// path is defeated by "..", by a doubled separator, and by a symlink; both
	// sides here are fully resolved, so the comparison is between real paths.
	resolved, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		// Includes "does not exist", which is a refusal rather than a silent
		// fallback to the root: the caller asked to run somewhere specific.
		// The host path is deliberately NOT echoed back — the container never
		// discloses this host's directory layout (see containerWorkspace).
		return "", "", fmt.Errorf("%w: %q could not be resolved", ErrWorkingDirRefused, workingDir)
	}

	rel, err := filepath.Rel(root, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", "", fmt.Errorf("%w: %q escapes it", ErrWorkingDirRefused, workingDir)
	}
	if info, err := os.Stat(resolved); err != nil || !info.IsDir() {
		// In-tree but not a directory. Refused here so the caller gets the
		// reason, rather than at the daemon, where it surfaces as an opaque
		// runtime failure of a container that was already created.
		return "", "", fmt.Errorf("%w: %q is not a directory", ErrWorkingDirRefused, workingDir)
	}
	if rel == "." {
		return root, containerWorkspace, nil
	}
	return root, containerWorkspace + "/" + filepath.ToSlash(rel), nil
}

// argv builds the exact command line for one Exec.
//
// It is separated from Exec because argv IS the security boundary of this
// handler: every containment property (the scrubbed environment, the mount, the
// workdir) is expressed here and nowhere else, so it must be assertable without
// running anything.
//
// It returns an error rather than a best-effort command line: a working_dir
// that cannot be contained is a REFUSAL, and a refusal must be unable to
// produce argv at all, so there is nothing for Exec to accidentally run.
func (r *containerRunner) argv(cmd string, workingDir string) ([]string, error) {
	// The Runner's own root — fixed at Acquire for its authenticated principal
	// (change 0065) — is the ONLY root argv ever mounts. Never h.root here: on
	// the per-tenant path that is a wider tree this principal was not granted.
	mount, workdir, err := r.handler.workspace(r.root, workingDir)
	if err != nil {
		return nil, err
	}

	env := r.currentEnv()

	args := make([]string, 0, 20+2*len(env))
	args = append(args,
		"run",
		// --rm: the container is torn down when the command exits, so no
		// state — including anything the command wrote outside the mount —
		// survives into the next principal's execution.
		"--rm",
		// -i: keeps stdin open. No -t: there is no terminal here, and
		// allocating one would corrupt the combined-output capture.
		"-i",
	)

	// Per-container cgroup caps (change 0077). Emitted ONLY for fields the
	// resolved config set; an unset field renders nothing — never a sentinel,
	// never `--memory 0` (which some runtimes read as "unlimited" and others
	// reject). Placement is deliberate: after `--rm -i` and BEFORE the egress
	// floor below (change 0064), which is where network posture lands.
	args = append(args, r.handler.limits.argv()...)

	// The egress FLOOR (change 0064). Under EgressEnforce the container gets
	// `--network none`: loopback only, no route off the box, so nothing the
	// command runs can reach the network except through the hole the trusted
	// side opens for it. Under EgressAllowAll — the default, and the zero value
	// — nothing is emitted at all, so an operator who never configured egress
	// gets byte-for-byte the pre-0064 argv.
	//
	// Three things about this site a reader must not have to rediscover:
	//
	//   - It is decided by the MODE alone, never by the allowlist being
	//     non-empty. Enforcing with nothing declared is the deny-all state and
	//     still gets the floor; that is the fail-safe direction.
	//   - It is the PAIR form (`--network`, `none`), which docker, nerdctl and
	//     podman all accept — preserving the one-argv-builder-serves-all-three
	//     property.
	//   - It is emitted HERE, in the run builder, and nowhere else. The pre-pull
	//     (prePull/runPull) builds its own `pull <image>` argv straight to h.run
	//     and must keep doing so: a pull under --network none cannot reach a
	//     registry, and with --pull=never below, a floored pull would take the
	//     whole substrate down on every cold image.
	//
	// The DATAPATH through the floor (change 0064, task 6) lands here, between
	// the floor and the --env pairs: the two read-only mounts, then the injected
	// proxy environment. See egress_forwarder.go for the shape and the rejected
	// alternatives.
	if r.handler.egress.Mode == EgressEnforce {
		args = append(args, "--network", "none")

		// Stripped whether or not a datapath follows. Under enforcement these
		// eight variables are the TRUSTED SIDE's, and an operator's
		// env_passthrough naming one of them must not be able to redirect egress
		// (HTTP_PROXY) or exempt destinations from it (NO_PROXY). Doing it before
		// the injection below means no duplicate key ever reaches the runtime, so
		// which of docker/nerdctl/podman is in use cannot change the answer.
		env = stripProxyEnv(env)

		if r.egressSocket != "" {
			// Read-only, both of them. The socket only needs connect(2), which a
			// read-only mount permits for a socket inode; the forwarder only
			// needs execute, and must not be replaceable by the very command it
			// is about to parent.
			args = append(args,
				"-v", r.egressSocket+":"+containerEgressSocket+":ro",
				"-v", r.handler.egressForwarder+":"+containerEgressForwarder+":ro",
			)
			for _, kv := range egressProxyEnv() {
				args = append(args, "--env", kv)
			}
		}
	}

	for _, kv := range env {
		// ALWAYS the `--env K=V` pair form, NEVER a bare `--env K`.
		//
		// A bare `--env K` tells docker/nerdctl/podman to copy the HOST's
		// current value of K into the container. That would re-open the exact
		// ambient-inheritance hole this package exists to close, and it would
		// do so invisibly: argv would still look like it named only allowed
		// keys. renderEnv already produced K=V strings; pass them whole.
		args = append(args, "--env", kv)
	}

	// Per-TENANT subdivision of this mount landed in change 0065: the source is
	// the root resolved for this Runner's authenticated Principal.Tenant at
	// Acquire (r.root), not a process-wide one.
	//
	// The INVARIANT this site has always carried is unchanged by that, and is
	// what a reader must not lose: the source is the trusted root and only ever
	// the trusted root. mount is "" only when no trusted root was resolved for
	// this principal at all, in which case nothing is mounted — never a
	// substitute derived from the command's arguments, and never a wider root
	// borrowed because this principal's own could not be resolved.
	if mount != "" {
		args = append(args, "-v", mount+":"+containerWorkspace)
	}
	args = append(args, "-w", workdir)

	// --pull=never (change 0077): the image is acquired by the explicit,
	// separately-timed pre-pull (see prePull), so `run` must never trigger an
	// unbounded pull under the command's own deadline. Supported by all three
	// detected CLIs (docker, nerdctl, podman), preserving the
	// one-argv-builder-serves-all-three property.
	args = append(args, "--pull=never")

	args = append(args, r.handler.image)
	if r.egressSocket != "" {
		// The forwarder EXEC-WRAPS the shell rather than being backgrounded
		// beside it, so the loopback listener is already bound when the command's
		// first byte runs. cmd itself is untouched and still reaches
		// `/bin/sh -c` as one argument. See egressForwarderCommand.
		args = append(args, egressForwarderCommand(cmd)...)
	} else {
		args = append(args, "/bin/sh", "-c", cmd)
	}
	return args, nil
}

// argv renders the per-container cgroup caps as OCI run flags, in a fixed order
// (change 0077). Only set fields render; an unset field emits nothing.
//
// Three details a reader must not have to rediscover:
//
//   - --memory-swap is pinned EQUAL to --memory. Docker's default when --memory
//     is set and --memory-swap is not is TWICE the memory limit, so a lone
//     --memory 2g actually permits 4 GB of memory+swap. Pinning them equal is
//     what makes the number mean what it says.
//   - --ulimit fsize bounds a SINGLE FILE, not the mount. It is real protection
//     against `dd if=/dev/zero of=big`; it is not a disk quota (that is #0065).
//   - --cpus is rendered from the config's already-canonicalised decimal string,
//     never a float formatted by %v — argv is golden-tested, so rendering must
//     be deterministic and locale-independent.
func (l Limits) argv() []string {
	var a []string
	if l.MemoryBytes != nil {
		m := strconv.FormatInt(*l.MemoryBytes, 10)
		// Pinned equal — see the doc comment.
		a = append(a, "--memory", m, "--memory-swap", m)
	}
	if l.CPUs != nil {
		a = append(a, "--cpus", *l.CPUs)
	}
	if l.Pids != nil {
		a = append(a, "--pids-limit", strconv.FormatInt(*l.Pids, 10))
	}
	if l.NoFile != nil {
		n := strconv.FormatInt(*l.NoFile, 10)
		a = append(a, "--ulimit", "nofile="+n+":"+n)
	}
	if l.FsizeBytes != nil {
		a = append(a, "--ulimit", "fsize="+strconv.FormatInt(*l.FsizeBytes, 10))
	}
	return a
}

// prePull performs the bounded, single-flight image acquisition (change 0077).
//
// The pull runs under a context derived from context.Background() and the
// configured pull_timeout — deliberately NOT from the caller's context, so a
// short-timeout bash call cannot cancel a pull a later call would have benefited
// from. The caller waits on the shared in-flight pull only up to ITS OWN
// deadline: if the caller's ctx fires first it returns that error while the pull
// completes in the background and the next Acquire finds the image warm.
//
// Concurrent callers JOIN the in-flight pull rather than each starting one. A
// SUCCEEDED pull is remembered and never repeated; a FAILED pull is not cached —
// it is retried on a later Acquire, because a transient registry blip must not
// permanently break the substrate.
func (h *containerHandler) prePull(ctx context.Context) error {
	h.pullMu.Lock()
	if h.pullDone {
		h.pullMu.Unlock()
		return nil
	}
	wait := h.pulling
	if wait == nil {
		// We start the pull. A fresh channel marks it in-flight; a background
		// goroutine runs it under the pull_timeout, independent of any caller.
		wait = make(chan struct{})
		h.pulling = wait
		timeout := DefaultPullTimeout
		if h.limits.PullTimeout != nil {
			timeout = *h.limits.PullTimeout
		}
		go h.runPull(wait, timeout)
	}
	h.pullMu.Unlock()

	// Wait on the shared pull only up to the CALLER's own deadline.
	select {
	case <-wait:
		h.pullMu.Lock()
		err := h.pullErr
		h.pullMu.Unlock()
		return err
	case <-ctx.Done():
		// The caller ran out of time; the pull continues in the background.
		return ctx.Err()
	}
}

// runPull executes the actual pull under its own timeout and records the
// outcome, then closes done to release every joined caller. On failure it clears
// the in-flight marker so a later Acquire retries; on success it latches
// pullDone so the image is never pulled again.
func (h *containerHandler) runPull(done chan struct{}, timeout time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	_, _, err := h.run(ctx, h.runtime, "pull", h.image)

	h.pullMu.Lock()
	h.pullErr = err
	if err == nil {
		h.pullDone = true
	}
	h.pulling = nil // allow a retry on failure; harmless on success (pullDone gates)
	h.pullMu.Unlock()

	close(done)
}

// Exec runs cmd inside a fresh container.
//
// Error semantics are identical to the host handler's, by contract: a command
// that RAN and exited non-zero, or that was killed by the context deadline, is
// a normal result reported through Output (ExitCode / TimedOut) with a nil
// error. A non-nil error means the SUBSTRATE could not start the command at
// all, and ExitCode is -1 on that path so a caller that reads only ExitCode
// still fails closed. ExitCode is never left 0 on any failure path.
func (r *containerRunner) Exec(ctx context.Context, cmd string, workingDir string) (Output, error) {
	args, err := r.argv(cmd, workingDir)
	if err != nil {
		// A containment refusal, decided BEFORE the CLI is touched: no
		// container is created, no mount is established, nothing runs. It is
		// reported as a substrate failure (ExitCode -1, non-nil error) because
		// that is exactly what it is — the command could not be started — and
		// because an ExitCode-only caller must fail closed on it.
		return Output{ExitCode: -1}, err
	}

	combined, code, err := r.handler.run(ctx, r.handler.runtime, args...)

	out := Output{
		Combined: combined,
		ExitCode: code,
		// Checked on the caller's context: the CLI is killed when the deadline
		// fires, and the resulting status is indistinguishable from any other
		// signal death without this.
		TimedOut: errors.Is(ctx.Err(), context.DeadlineExceeded),
	}

	// Substrate-health classification (change 0065, task 7), decided BEFORE the
	// error branch below rewrites ExitCode to -1 — classifyExit needs the raw
	// status the runtime actually reported. It fires for a signal death or a
	// failure to start, and for NOTHING else: an ordinary non-zero command exit
	// is the command reporting a result, not the sandbox being unhealthy.
	if reason, unhealthy := classifyExit(code, err, out.TimedOut); unhealthy {
		r.handler.health.fire(HealthInfo{
			Principal: r.principal,
			Handler:   HandlerContainer,
			Runtime:   r.handler.runtime,
			Reason:    reason,
		})
	}

	if err != nil {
		// Could not start: no daemon, an unpullable image, a rejected mount.
		out.ExitCode = -1
		return out, fmt.Errorf("%s: %w", r.handler.runtime, err)
	}
	if out.TimedOut && out.ExitCode == 0 {
		// A killed command reporting success is not a result we will pass on.
		out.ExitCode = -1
	}
	return out, nil
}

// Release drops this Runner's EGRESS LEASE and does nothing else.
//
// There is no container to stop: `run --rm` already tears one down when the
// command exits, and warm reuse is the pool's concern. What there IS, under an
// enforcing posture, is the per-principal listener this Runner's Acquire leased
// — a goroutine, a listening FD, a 0700 directory and a socket inode on the
// host. This is where the lease goes back.
//
// It is the seam that makes those reclaimable at all. A Runner's life is exactly
// the span in which its container can reach the socket, and the warm pool
// already calls this on every teardown it performs (the idle-TTL reaper,
// Pool.Close, a failed reuse certification, a principal mismatch), so a
// long-lived multi-tenant process reclaims a principal's listener when that
// principal's last sandbox goes rather than at process exit. Dropping a lease is
// not closing a listener: the proxy closes it only when the last holder lets go,
// so this can never cut a tunnel belonging to another Exec.
//
// It stays idempotent (releaseOnce) and non-blocking on every ordinary path, so
// callers may release unconditionally on every early return without branching on
// which substrate they hold. The error is reported rather than swallowed — a
// socket directory that could not be removed is worth surfacing — but every
// current caller treats teardown as best-effort, which is right: a failed
// reclaim must not deny anyone a fresh sandbox.
func (r *containerRunner) Release(context.Context) error {
	var err error
	r.releaseOnce.Do(func() {
		if r.egressSocket == "" || r.handler == nil || r.handler.egressSockets == nil {
			return
		}
		err = r.handler.egressSockets.Release(r.principal)
	})
	return err
}

// runClientCommand is the default execRunner, bound to h so that the CLI
// client's own environment is resolved through the SAME lookup seam
// (h.envLookup) that governs the sandboxed command's environment, rather than
// re-reading the real process environment on every Exec regardless of what
// WithEnvLookup / withContainerEnvLookup declared.
func (h *containerHandler) runClientCommand(ctx context.Context, name string, args ...string) ([]byte, int, error) {
	return runCommand(ctx, name, h.envLookup, args...)
}

// runCommand is the default execRunner implementation: a real subprocess.
//
// It maps os/exec's error model onto the execRunner contract — an ExitError is
// a RESULT (the CLI ran and reported a status), anything else is a start
// failure — and it never returns a zero exit code alongside a failure.
func runCommand(ctx context.Context, name string, lookup func(string) (string, bool), args ...string) ([]byte, int, error) {
	c := exec.CommandContext(ctx, name, args...)

	// SECURITY-CRITICAL: a nil exec.Cmd.Env means "inherit the parent process
	// environment", so this is set explicitly and is never left nil.
	//
	// Note what this env is and is not. It is the CLIENT process's environment —
	// what the docker/nerdctl/podman binary itself needs to locate a daemon and
	// read its own config. It has NO bearing on what the sandboxed command
	// observes: the container's environment is carried entirely in argv as
	// --env K=V. Inheriting here would still hand this process's API keys to
	// the CLI binary, so it is scrubbed to the same discipline, with the small
	// set of daemon-addressing variables the CLIs genuinely require added.
	//
	// lookup is the SAME seam (Service.lookup / WithEnvLookup) that governs the
	// sandboxed command's environment; nil means the real process environment.
	if lookup != nil {
		c.Env = renderEnv(ResolveEnv(containerClientPassthrough[:], lookup))
	} else {
		c.Env = renderEnv(ResolveEnvFromOS(containerClientPassthrough[:]))
	}

	combined, err := c.CombinedOutput()
	if err == nil {
		return combined, 0, nil
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		code := exitErr.ExitCode()
		if code == 0 {
			// Signalled-but-zero cannot normally happen; refuse to report it as
			// success regardless.
			code = -1
		}
		return combined, code, nil
	}

	return combined, -1, err
}
