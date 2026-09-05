package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/toolidentity"
	"github.com/ethanhinson/fuse/internal/tools"
	"github.com/ethanhinson/fuse/internal/tools/sandbox"
)

// newSandboxService resolves the bash substrate for this process, ONCE.
//
// # Why once, and why here
//
// The off-switch is a file — <root>/.fuse/sandbox.local.yml — and the agent
// running inside the sandbox can write files. Resolving containment per bash
// call would let a model author its own escape on one call and use it on the
// next. So every entry point calls this exactly once, at startup, before any
// model has run, and hands the resulting frozen *sandbox.Service to the tool
// registry. Nothing downstream can re-read config, re-select a handler, or
// reach the filesystem (see sandbox.Service).
//
// root is the process working directory, which is the same trusted-local root
// internal/config already resolves .fuse.local.yml against. It is read here,
// at startup, rather than at Exec time — a tool's working_dir argument never
// reaches this decision.
//
// It settles TWO things, and the second is equally security-bearing: it is
// where the off-switch config is read from, AND it is the working tree the
// container substrate bind-mounts at /workspace. A model's working_dir selects
// a subdirectory of it and can never replace it (sandbox.WithTrustedRoot).
// When os.Getwd fails there is no trusted root, so nothing is mounted and any
// working_dir is refused — degraded, but never a mount the model chose.
//
// hosted declares the ADR-0034 posture: true when this binary executes
// workloads on behalf of REMOTE principals (the loop servers), false for the
// operator's own local CLI. It comes from how the binary was launched and from
// nowhere else — never from config, an environment variable, a wire field, or
// model output. Under the hosted posture the off-switch is structurally inert.
//
// The Warnings are the operator's only signal that their config file did not do
// what they thought, so they are logged unconditionally. Selection of the HOST
// substrate is logged too: uncontained execution should never be quiet.
//
// A construction error yields a NIL Service, which is fail-CLOSED — the bash
// tool then refuses every call. It is never an invitation to run uncontained;
// note that a missing container runtime is deliberately NOT an error here, it
// surfaces as a refusal at Acquire.
//
// appCfg is the process's own ~/.fuse/config.yml. It reaches this function for
// exactly one reason: the egress datapath's #52 delegated-identity seam is built
// from it (buildEgressCredentialSource), so an `egress.allow` entry declaring a
// `credential:` audience can actually be minted for. It NEVER influences
// containment — the substrate, the off-switch, and the egress posture are read
// only from the trusted-local <root>/.fuse/sandbox.local.yml, as before.
//
// It also settles the EGRESS datapath (change 0064) and owns its lifetime. The
// returned func is the shutdown the caller MUST defer: it closes the host-side
// proxy, tearing down every per-principal socket and removing the fuse-owned
// directory they live in. It is never nil and is safe to call more than once, so
// every entry point can defer it unconditionally at the call site — which is also
// the right place for it, since defers run LIFO and the proxy must outlive the
// sandbox pool release that comes after it.
func newSandboxService(appCfg config.Config, hosted bool, warnw io.Writer) (*sandbox.Service, func()) {
	root, err := os.Getwd()
	if err != nil {
		// LoadConfig treats an empty root as "no file was consulted" and
		// returns the contained default plus a warning — the safe direction.
		root = ""
	}

	// The egress DATAPATH (change 0064). Resolved BEFORE the Service, because
	// WithEgressProxy is a construction option and a Service is immutable once
	// built: there is no later moment at which a datapath could be added.
	//
	// Its absence is never permissive — see resolveEgressDatapath — and its
	// presence cannot turn enforcement ON: only the Service's own config load
	// decides the posture, and the container handler ignores a datapath unless
	// that load resolved EgressEnforce.
	proxy, forwarder, stopReporter := resolveEgressDatapath(appCfg, root, warnw)

	opts := []sandbox.ServiceOption{sandbox.WithHostedPosture(hosted)}

	// THE PER-TENANT BIND-MOUNT SOURCE (change 0065), gated on the HOSTED
	// posture — and gated on it for a reason worth stating, because this is the
	// design decision this composition root owns.
	//
	// # Why hosted, and only hosted
	//
	// `hosted` is the ADR-0034 posture signal: true exactly for the two loop
	// servers, which execute workloads on behalf of REMOTE principals under
	// ADR-0030's "one process hosts N loops", where `loop_server.auth` is a LIST
	// of token→principal entries each carrying its own tenant. That is the only
	// deployment in which two tenants' bash calls can meet inside one fuse
	// process, so it is the only deployment where sharing one bind-mount is a
	// cross-tenant disclosure.
	//
	// The local CLI bindings (one-shot, shell, research-probe, mcp-server) pass
	// hosted=false and keep WithTrustedRoot's single-root behaviour, which the
	// spec is explicit is not being replaced. That is not laziness: a local
	// operator has one tenant and one working tree, and moving their bash off
	// the repo they are standing in — onto some provisioned ~/.fuse/workspaces
	// box — would break the tool for the overwhelmingly common case while
	// isolating nothing from nobody. `hosted` comes from how the binary was
	// launched and from nowhere else — never config, an environment variable, a
	// wire field, or model output — so this gate is not something a model or a
	// remote caller can flip.
	//
	// # The host layout, and who owns it
	//
	// The sandbox package deliberately refuses to answer "which host directory
	// does tenant X get" — that is composition-root policy, and TenantRoots
	// takes only the PARENT the trees are children of. This binary's answer:
	//
	//	~/.fuse/workspaces/<tenant>/     0700, one direct child per tenant
	//
	// chosen because (a) it is a sibling of ~/.fuse/sessions, which is already
	// where this binary keeps per-deployment durable state, so an operator has
	// one directory to back up, quota, or put on a dedicated volume; (b) it is
	// OUTSIDE every tenant's own mounted tree and outside the repo root, so
	// nothing a contained command can write reaches the parent, the sibling
	// trees, or the off-switch file; and (c) it needs no new config surface —
	// and a config-supplied mount parent would be a new knob naming a host
	// directory fuse bind-mounts into a container a model drives, which is the
	// same hazard defaultEgressForwarderCandidates documents at length.
	//
	// create=true: fuse provisions a tenant's tree on first use, 0700, rather
	// than requiring out-of-band provisioning. An unprovisioned tenant would
	// otherwise resolve to NOTHING and every one of its bash calls would refuse
	// — technically safe, operationally a hosted fuse that does not work until
	// someone mkdirs by hand. The directory is empty and owner-only, so creating
	// it grants nothing that was not already this process's.
	//
	// # Degraded-safe, and NOT silent
	//
	// When the parent cannot be resolved, hostedWorkspaceParent returns "" and
	// says so loudly; NewTenantRoots over "" then resolves NOTHING for every
	// principal, so containers mount nothing and any working_dir is refused. It
	// never falls back to the process-wide trusted root, which under the hosted
	// posture is the shared tree this change exists to stop handing out. The
	// notice mirrors #64's "EGRESS ENFORCED with NO DATAPATH": a fail-closed
	// posture that nobody is told about is indistinguishable from a broken fuse.
	if hosted {
		parent := hostedWorkspaceParent(warnw)
		// Passed UNCONDITIONALLY, including when parent is "". A hosted Service
		// must be tenant-scoped whether or not provisioning succeeded: dropping
		// the option on failure would silently restore the shared-root
		// behaviour, which is the precise fallback the spec forbids.
		opts = append(opts, sandbox.WithTenantRoots(sandbox.NewTenantRoots(parent, true)))
	}

	closeFn := func() {}
	if proxy != nil {
		opts = append(opts, sandbox.WithEgressProxy(proxy, forwarder))
		// The composition root OWNS the Proxy: WithEgressProxy documents that the
		// Service never closes it, having no shutdown of its own. So the closer is
		// handed back for the entry point to defer — every entry point registers it
		// at THIS point, which (defers being LIFO) puts the proxy teardown after the
		// sandbox pool release, so no live container is left holding a socket whose
		// listener has gone. sync.Once because an entry point may run it on an early
		// return path as well as at the end.
		var once sync.Once
		closeFn = func() {
			once.Do(func() {
				if cerr := proxy.Close(); cerr != nil {
					fmt.Fprintf(warnw, "sandbox: egress proxy shutdown: %v\n", cerr)
				}
				// AFTER the proxy: Close waits for every connection goroutine and
				// accept loop, so by this point no further refusal can be reported
				// and the reporter can drain what it holds and print its summary.
				stopReporter()
			})
		}
	}

	svc, warns, serr := sandbox.NewServiceFromRoot(root, opts...)
	for _, w := range warns {
		fmt.Fprintf(warnw, "warning: %s\n", w.Error())
	}
	if serr != nil {
		fmt.Fprintf(warnw, "sandbox: substrate unavailable (%v); the bash tool will refuse to run commands\n", serr)
		return nil, closeFn
	}
	if svc != nil && !svc.Contained() && svc.Available() {
		fmt.Fprintf(warnw, "sandbox: UNCONTAINED — %s/.fuse/sandbox.local.yml authorizes running bash commands directly on this host\n", root)
		if proxy != nil {
			// The datapath notice above was resolved before the substrate was
			// known, and the off-switch then selected the host. Correct it rather
			// than leaving "egress ENFORCED" as the last word: the floor and the
			// allowlist are CONTAINER machinery, so on the host substrate they
			// enforce nothing at all.
			fmt.Fprintf(warnw, "sandbox: egress enforcement does NOT apply on the host substrate — the --network none floor and the egress.allow list are container-only, so this command's network access is this host's\n")
		}
	}
	return svc, closeFn
}

// hostedWorkspaceParentName is the directory, under the fuse home, that the
// per-tenant workspace trees are children of. A CONSTANT, not a knob: it names
// a host directory this binary bind-mounts children of into containers a model
// drives, and the whole reason defaultEgressForwarderCandidates is anchored to
// the executable rather than to config applies here verbatim.
const hostedWorkspaceParentName = "workspaces"

// hostedWorkspaceParent resolves — and PROVISIONS — the parent directory the
// per-tenant bind-mount trees live under, returning "" when it cannot.
//
// # Why it must exist before NewTenantRoots sees it
//
// sandbox.NewTenantRoots canonicalises its parent through resolveMountRoot,
// which returns "" for anything it cannot prove is an existing directory — a
// bind-mount source is never invented, because a source the container daemon
// has to create is a root-owned empty box that merely LOOKS like a workspace.
// So a hosted fuse creates the parent itself, at startup, before any model has
// run, and hands over a path that already exists.
//
// 0700 on both the fuse home and the parent. The per-tenant children are 0700
// too (TenantRoots), but a 0755 parent would let any uid on the host enumerate
// the tenant list, and a per-tenant tree reachable by another uid is not an
// isolation boundary regardless of its own mode bits.
//
// Every failure returns "" plus a loud, operator-facing notice, never a
// degraded parent. "" is the fail-CLOSED direction: it resolves to no root for
// every principal, so containers mount nothing and any working_dir is refused,
// and it can never widen into a shared tree.
func hostedWorkspaceParent(warnw io.Writer) string {
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		warnHostedWorkspaceUnavailable(warnw, "(unresolved)", fmt.Sprintf("this process has no home directory (%v)", err))
		return ""
	}
	parent := filepath.Join(home, ".fuse", hostedWorkspaceParentName)
	if mkErr := os.MkdirAll(parent, 0o700); mkErr != nil {
		warnHostedWorkspaceUnavailable(warnw, parent, mkErr.Error())
		return ""
	}
	// Re-asserted on an EXISTING directory as well as a freshly created one:
	// MkdirAll leaves a pre-existing directory's mode alone, so a parent that
	// was created 0755 by an earlier fuse — or by an operator — would otherwise
	// keep leaking the tenant list to every uid on the host.
	if chErr := os.Chmod(parent, 0o700); chErr != nil {
		warnHostedWorkspaceUnavailable(warnw, parent, "cannot restrict it to owner-only ("+chErr.Error()+")")
		return ""
	}
	return parent
}

// warnHostedWorkspaceUnavailable is the operator's only signal that a hosted
// fuse came up unable to give any tenant a workspace.
//
// It mirrors warnEgressBlackout deliberately, because the failure has the same
// shape: the posture is correct and fail-closed, every bash call refuses, and
// without this notice that is indistinguishable from the sandbox simply being
// broken. Naming the directory and the cause is what turns a support ticket
// into a chmod.
func warnHostedWorkspaceUnavailable(warnw io.Writer, parent, cause string) {
	fmt.Fprintf(warnw,
		"sandbox: HOSTED with NO PER-TENANT WORKSPACE — this binary serves remote principals, so each tenant's bash "+
			"must run against its own tree under %s, but that directory is unusable (%s). "+
			"No tenant will be given ANY workspace: every contained command runs with nothing mounted and every "+
			"working_dir is refused. This is deliberate — sharing one tree across tenants is a cross-tenant "+
			"disclosure — but it is not a working deployment. Make %s creatable and owner-only, then restart.\n",
		parent, cause, parent)
}

// installSandboxLoopHooks installs the two per-LOOP observers on the
// process-scoped sandbox Service: the admission gate's (change 0077) and the
// substrate-health emitter's (change 0065).
//
// # Why both live here, together
//
// Both seams have the same awkward shape and it is better stated once than
// discovered twice: the Service is PROCESS-scoped (one substrate, one gate, one
// handler, shared by every loop) while an event store is PER-LOOP. So a process
// serving many loops attributes an admission — or a health transition — to
// whichever loop's store was installed last. That coarseness is acceptable for
// both, and for the same reason SandboxGateHooks already documents: these are
// HOST-level signals whose tenant rides on the event and whose metrics are
// host-level, so the operator-facing rate stays correct even where a single
// event's loop attribution is approximate. Neither is a per-loop accounting
// record, and neither is used as one.
//
// A nil store installs NOTHING — the honest shape for the bindings that have no
// per-loop store (one-shot, shell, research-probe, mcp-server) — rather than
// emitting into a NoopStore, which would make the wiring look active on a
// dashboard that would then never move. A nil *Service is tolerated because
// NewBash(nil) is a supported fail-closed shape and this must be callable
// unconditionally beside it.
//
// It is called from the per-loop construction factory, at the first point that
// holds BOTH the frozen Service and the loop's store, and before the loop's
// agent exists — so no command can have run, and no hook is replaced while it
// is firing.
func installSandboxLoopHooks(sb *sandbox.Service, store event.EventStore, nodeID string) {
	if sb == nil || store == nil {
		return
	}
	sb.SetGateHooks(tools.SandboxGateHooks(store, nodeID))
	// THE HEALTH EMITTER (change 0065, task 9). This is its ONLY non-test
	// caller, and without it fuse_sandbox_unhealthy_total can never gain a
	// series in a real fuse — an always-zero failure counter that reads exactly
	// like a healthy fleet. See the security-knob-inert-at-composition-root
	// learning; cmd/fuse's own tests fail if this line is removed.
	sb.SetHealthHooks(tools.SandboxHealthHooks(store, nodeID))
}

// egressForwarderName is the artifact `make egress-forwarder` produces for one
// architecture. GOOS is always linux — the binary runs inside the CONTAINER, not
// on this host — so only the arch varies.
const egressForwarderName = "fuse-egress-forward-linux-"

// egressForwarderCandidates is the artifact-discovery seam. It is a var only so
// tests can substitute paths; nothing at runtime replaces it, and in particular
// no config file, environment variable, or model output reaches it (see
// defaultEgressForwarderCandidates for why that matters).
var egressForwarderCandidates = defaultEgressForwarderCandidates

// defaultEgressForwarderCandidates is where a fuse binary looks for the
// in-container forwarder, in order.
//
// # Why the candidates are anchored to the EXECUTABLE, and to nothing else
//
// This path becomes a read-only bind-mount SOURCE inside the sandbox, and a
// mount source is containment-relevant: whoever names it names a host file the
// contained command's namespace can see. So it is derived the same way the
// hosted posture is — from how this binary was installed — and never from the
// config file, an environment variable, the working directory, or a tool
// argument. `<root>/.fuse/sandbox.local.yml` is exactly the file the agent
// running inside the sandbox can author, so a forwarder path read from config
// would let a model choose a host file to mount. There is no such knob.
//
//	<exeDir>/dist/fuse-egress-forward-linux-<arch>  — a repo checkout: `make
//	    build` writes ./fuse at the repo root and `make egress-forwarder` writes
//	    dist/ beside it, so a developer needs no extra step.
//	<exeDir>/fuse-egress-forward-linux-<arch>       — a `go install`ed fuse
//	    (~/go/bin/fuse): the operator drops the artifact next to the binary.
//	    This is the documented answer for a distributed fuse, and it is a COPY
//	    rather than a download: fuse fetches nothing at startup.
//
// # The architecture
//
// Strictly, the arch that matters is the CONTAINER IMAGE's, not this host's, and
// fuse does not know the image's arch without asking the runtime. runtime.GOARCH
// is used because every container runtime defaults to the host platform, so it is
// right for every case except an operator deliberately running a foreign-arch
// image under emulation. That case is not silent and is not fail-open: the wrong
// binary fails to exec as the container's entry command, so the bash call fails
// loudly with an exec-format error and nothing reaches the network.
//
// The symlink is resolved so that a fuse reached through a symlink (a package
// manager's bin/ shim) looks beside the REAL binary, which is where the artifact
// was installed.
func defaultEgressForwarderCandidates() []string {
	exe, err := os.Executable()
	if err != nil {
		return nil
	}
	if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
		exe = resolved
	}
	dir := filepath.Dir(exe)
	name := egressForwarderName + runtime.GOARCH
	return []string{
		filepath.Join(dir, "dist", name),
		filepath.Join(dir, name),
	}
}

// firstForwarderArtifact returns the first candidate that is an EXECUTABLE
// REGULAR FILE, or "" when none is.
//
// Both conditions are load-bearing and both fail to "". A missing or
// non-regular source is not an error to docker — the daemon CREATES a
// root-owned directory at the mount source — which would leave the container
// holding an empty directory where its entry command should be. A present but
// non-executable file is worse than no datapath at all: with no datapath,
// commands still run and only the network fails; with an unexecutable forwarder
// every bash call dies before the shell starts. So neither is treated as a
// datapath, and the deny-all floor (plus the diagnostic below) stands instead.
//
// This check is deliberately no LAXER than sandbox.resolveForwarderBinary's, so
// a path this function blesses is never silently dropped downstream — which
// would put the "datapath wired" notice and reality out of step.
func firstForwarderArtifact(candidates []string) string {
	for _, p := range candidates {
		if p == "" {
			continue
		}
		info, err := os.Stat(p)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			continue
		}
		return p
	}
	return ""
}

// resolveEgressDatapath decides whether this process can offer the egress
// datapath, and SAYS SO EITHER WAY.
//
// # The defect this function exists to prevent
//
// `egress.mode: enforce` emits the `--network none` floor whether or not a
// datapath was wired. Without one the result is a TOTAL network blackout — every
// call in every contained command fails and the operator's `egress.allow` list
// has no effect at all. That state is correct (deny-all is the fail-CLOSED
// direction, and this function never degrades it to allow) but it must never be
// SILENT: an advertised knob whose allowlist is unreachable, with nothing
// printed, is indistinguishable from a broken network. So the blackout is
// announced here, beside the UNCONTAINED notice, in the same unconditional way.
//
// The config is read here in ADDITION to the Service's own load, not instead of
// it. This read decides only whether a datapath is OFFERED; the Service's read is
// the sole authority on the posture. The two cannot disagree in practice — both
// happen at startup, before any model has run — and if they somehow did, both
// directions are safe: "enforce" here with allow-all there leaves an unused proxy
// (the container handler ignores a datapath outside enforcement), and allow-all
// here with "enforce" there is the deny-all floor.
//
// Returns a nil proxy and an empty forwarder for every non-wired outcome. A
// partial datapath is never returned: WithEgressProxy treats a nil proxy or
// empty path as "not supplied", and both halves are decided together here so
// that stays true.
//
// The third return is the refusal reporter's shutdown, which newSandboxService
// folds into the closer it hands the entry point. It is never nil, and it is
// safe to call whether or not a proxy was built.
func resolveEgressDatapath(appCfg config.Config, root string, warnw io.Writer) (*sandbox.Proxy, string, func()) {
	noop := func() {}
	// Warnings are discarded on purpose: NewServiceFromRoot returns the SAME
	// warnings from its own load, and the caller logs those. Reporting them twice
	// would train an operator to skim them.
	cfg, _ := sandbox.LoadConfig(root)
	if cfg.Egress.Mode != sandbox.EgressEnforce {
		// Allow-all: no floor, no proxy, nothing to say, and no temp directory
		// created for the overwhelmingly common case.
		return nil, "", noop
	}

	candidates := egressForwarderCandidates()
	forwarder := firstForwarderArtifact(candidates)
	if forwarder == "" {
		warnEgressBlackout(warnw, root, cfg, candidates, "")
		return nil, "", noop
	}

	// THE #52 DELEGATED-IDENTITY SEAM. Resolved here, at the composition root,
	// because sandbox.WithProxyCredentialSource is a CONSTRUCTION option: as with
	// the datapath itself, there is no later moment at which a source could be
	// added to a live Proxy. A nil source is fail-CLOSED, not permissive — a
	// `credential:` entry with no source is refused (Proxy.delegatedHeader) — and
	// the reason it is nil is printed, so an operator whose entry can never be
	// satisfied learns that at startup rather than from an unexplained 403.
	credentials, credReason := buildEgressCredentialSource(appCfg, cfg.Egress)
	if credReason != "" {
		fmt.Fprintf(warnw, "sandbox: egress identity — %s\n", credReason)
	}

	// THE REFUSAL REPORTER. ProxyHooks.Refused is the only egress observability
	// this package offers, and an unconsumed hook means an operator can see THAT
	// a command's network call failed but never WHY. It is non-blocking by
	// construction: a capacity refusal fires on the listener's accept loop, so a
	// hook that wrote to warnw directly could stall a principal's ability to
	// accept behind a slow pipe (see egressRefusalReporter).
	reporter := newEgressRefusalReporter(warnw)

	proxy, err := sandbox.NewProxy(
		sandbox.WithProxyCredentialSource(credentials),
		sandbox.WithProxyHooks(reporter.hooks()),
	)
	if err != nil {
		reporter.stop()
		warnEgressBlackout(warnw, root, cfg, candidates, err.Error())
		return nil, "", noop
	}

	// Enforcement is on AND reachable. Logged unconditionally for the same reason
	// the UNCONTAINED notice is: a change in what the sandbox can reach the network
	// through should never be quiet, in either direction.
	fmt.Fprintf(warnw, "sandbox: egress ENFORCED — %d declared destination(s); datapath via %s\n",
		len(cfg.Egress.Allow), forwarder)
	return proxy, forwarder, reporter.stop
}

// buildEgressCredentialSource resolves the CredentialSource the egress proxy
// mints an `egress.allow` entry's declared `credential:` audience through.
//
// # Why it is gated on the EGRESS config, not the MCP config
//
// buildToolIdentitySource turns the seam on when an MCP SERVER declares an
// identity-propagating auth type. That is the right trigger for the MCP manager
// and the wrong one here: the two seams are opted into by different config. An
// operator who writes `credential: internal-api` on an egress entry and runs no
// identity-propagating MCP server has opted in just as explicitly, and gating on
// the MCP config would leave their entry permanently refused — the defect this
// fix exists to close. So the trigger is the egress allowlist itself, and the
// CONSTRUCTION (newToolIdentityBroker) is shared rather than duplicated, so both
// seams mint under the same tenant keys from the same trusted signing material.
//
// It stays INERT for every deployment that declared no `credential:` entry:
// (nil, "") — no STS, no broker, no notice, byte-identical to before.
//
// Every failure returns a NIL source plus a reason, never a degraded one. A nil
// source is refused at request time by the proxy, so the fail-closed property is
// structural: there is no value this function can return that turns a declared
// identity into an unauthenticated allow-through.
func buildEgressCredentialSource(appCfg config.Config, egress sandbox.Egress) (toolidentity.CredentialSource, string) {
	declared := 0
	for _, entry := range egress.Allow {
		if entry.Credential != "" {
			declared++
		}
	}
	if declared == 0 {
		return nil, ""
	}
	if appCfg.ToolIdentity.SigningKey == "" {
		return nil, fmt.Sprintf("%d egress.allow entr%s declare a `credential:` audience but tool_identity.signing_key is unset in ~/.fuse/config.yml — no delegated token can be minted, so every request to those destinations is REFUSED",
			declared, plural(declared))
	}
	src, reason := newToolIdentityBroker(appCfg)
	if reason != "" {
		return nil, reason + " — egress entries declaring a `credential:` audience are REFUSED"
	}
	return src, ""
}

// warnEgressBlackout is the operator's only signal that enforcement is on with
// no way through. It names the count of entries that are now unreachable, every
// path that was searched, and the exact command that produces the artifact.
func warnEgressBlackout(warnw io.Writer, root string, cfg sandbox.Config, candidates []string, cause string) {
	where := strings.Join(candidates, ", ")
	if where == "" {
		where = "(this binary's location could not be determined)"
	}
	detail := "no forwarder binary was found"
	if cause != "" {
		detail = "the host-side egress proxy could not be started (" + cause + ")"
	}
	fmt.Fprintf(warnw,
		"sandbox: EGRESS ENFORCED with NO DATAPATH — %s/.fuse/sandbox.local.yml declares egress.mode: enforce, but %s, "+
			"so containers run with --network none and EVERY network call from EVERY bash command will fail; "+
			"the %d declared egress.allow entr%s cannot be reached. "+
			"Build the forwarder with `make egress-forwarder` and place %s%s beside the fuse binary (searched: %s).\n",
		root, detail, len(cfg.Egress.Allow), plural(len(cfg.Egress.Allow)), egressForwarderName, runtime.GOARCH, where)
}

func plural(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}
