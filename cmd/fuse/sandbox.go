package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

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
// It also settles the EGRESS datapath (change 0064) and owns its lifetime. The
// returned func is the shutdown the caller MUST defer: it closes the host-side
// proxy, tearing down every per-principal socket and removing the fuse-owned
// directory they live in. It is never nil and is safe to call more than once, so
// every entry point can defer it unconditionally at the call site — which is also
// the right place for it, since defers run LIFO and the proxy must outlive the
// sandbox pool release that comes after it.
func newSandboxService(hosted bool, warnw io.Writer) (*sandbox.Service, func()) {
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
	proxy, forwarder := resolveEgressDatapath(root, warnw)

	opts := []sandbox.ServiceOption{sandbox.WithHostedPosture(hosted)}
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
// Returns (nil, "") for every non-wired outcome. A partial datapath is never
// returned: WithEgressProxy treats a nil proxy or empty path as "not supplied",
// and both halves are decided together here so that stays true.
func resolveEgressDatapath(root string, warnw io.Writer) (*sandbox.Proxy, string) {
	// Warnings are discarded on purpose: NewServiceFromRoot returns the SAME
	// warnings from its own load, and the caller logs those. Reporting them twice
	// would train an operator to skim them.
	cfg, _ := sandbox.LoadConfig(root)
	if cfg.Egress.Mode != sandbox.EgressEnforce {
		// Allow-all: no floor, no proxy, nothing to say, and no temp directory
		// created for the overwhelmingly common case.
		return nil, ""
	}

	candidates := egressForwarderCandidates()
	forwarder := firstForwarderArtifact(candidates)
	if forwarder == "" {
		warnEgressBlackout(warnw, root, cfg, candidates, "")
		return nil, ""
	}

	proxy, err := sandbox.NewProxy()
	if err != nil {
		warnEgressBlackout(warnw, root, cfg, candidates, err.Error())
		return nil, ""
	}

	// Enforcement is on AND reachable. Logged unconditionally for the same reason
	// the UNCONTAINED notice is: a change in what the sandbox can reach the network
	// through should never be quiet, in either direction.
	fmt.Fprintf(warnw, "sandbox: egress ENFORCED — %d declared destination(s); datapath via %s\n",
		len(cfg.Egress.Allow), forwarder)
	return proxy, forwarder
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
