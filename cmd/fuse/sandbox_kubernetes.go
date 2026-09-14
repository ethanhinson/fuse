package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/tools"
	"github.com/ethanhinson/fuse/internal/tools/sandbox"
	"github.com/ethanhinson/fuse/internal/tools/sandbox/kubernetes"
)

// THE KUBERNETES COMPOSITION ROOT (change 0075, task 10).
//
// internal/tools/sandbox/kubernetes is a fully tested package that, without this
// file, the shipped binary never constructs. That is the exact shape of the
// `security-knob-inert-at-composition-root` learning, which this repository has
// now hit three times (#64's Proxy, #65's TenantRoots and health hooks, and this
// substrate), so the wiring lives in its own file with its own assertions in
// cmd/fuse/sandbox_kubernetes_wiring_test.go rather than as three lines buried
// in newSandboxService.
//
// WHY THE TWO HALVES MEET HERE AND NOWHERE ELSE.
//
// ADR-0058 rule 1: internal/tools/sandbox must never import a control-plane SDK,
// so the Handler seam and the Kubernetes client cannot be joined inside either
// package. sandbox.WithHandlerFactory is the one seam through which they meet,
// and it is reachable only from a composition root: the config FILE can only
// NAME a handler, never introduce one, so an operator can select a substrate the
// binary already carries and nothing more. That is what makes this file, and not
// the off-switch file, the authority on what "kubernetes" means.

// kubernetesSubstrateOptions is everything the substrate constructor needs,
// bundled so the seam below has one parameter rather than six.
//
// It mirrors kubernetes.Options deliberately — it is not an abstraction over it,
// it is the same information — because the seam exists to replace the CLUSTER in
// tests and must not become a second place where the substrate's inputs are
// decided.
type kubernetesSubstrateOptions struct {
	// Config is the frozen, load-once sandbox configuration. Its Kubernetes
	// block, Egress posture and Limits are the whole posture the Pod is built to.
	Config sandbox.Config
	// MaxPodsPerTenant sizes the cluster-side ResourceQuota from the same number
	// the in-process Gate enforces.
	MaxPodsPerTenant int64
	// InstanceID records which fuse instance owns a Pod. Legibility and the
	// heartbeat's owner, never the reaper's selection.
	InstanceID string
	// ProxyCredentials mints each sandbox sidecar's client certificate. REQUIRED
	// under enforce; the substrate refuses that combination itself rather than
	// discovering it per Pod.
	ProxyCredentials sandbox.SandboxCredentialSource
}

// newKubernetesSubstrate is the substrate-construction seam.
//
// It is a var for exactly one reason, and it is the same reason
// egressForwarderCandidates is: the REAL constructor opens a connection to an
// API server, so a test that required it would skip on every developer machine
// and therefore prove nothing at all about whether this binary wires the
// substrate. Nothing at runtime replaces it — no config file, no environment
// variable, and no model output reaches it — so it cannot be a door to a
// substitute substrate.
//
// What a test may replace is the CLUSTER. The registration below, the ordering
// of the refusals, and selectHandler's decision are never replaced: those are
// the things the wiring tests are about.
var newKubernetesSubstrate = func(opts kubernetesSubstrateOptions) (sandbox.RemoteSubstrate, error) {
	return kubernetes.New(kubernetes.Options{
		Config:           opts.Config,
		MaxPodsPerTenant: opts.MaxPodsPerTenant,
		InstanceID:       opts.InstanceID,
		// Arch is deliberately left unset: the kubernetes package defaults it to
		// its own runtime.GOARCH, which is this process's, and a value invented
		// here would be a second place the arch pin could be wrong.
		ProxyCredentials: opts.ProxyCredentials,
	})
}

// reapSink is the process-scoped indirection through which a reaped ORPHAN
// becomes a `sandbox.reap` event.
//
// # Why an indirection rather than the hook itself
//
// sandbox.WithRemoteReapHook is a CONSTRUCTION option on the handler, and the
// handler is built once, inside NewService, at startup. Event stores are
// PER-LOOP and do not exist yet at that moment. So the hook installed at
// construction cannot be the emitter; it has to be a cell whose target the
// per-loop wiring (installSandboxLoopHooks) fills in later.
//
// That inherits the same coarseness SandboxGateHooks already documents and
// accepts for the same reason: the reaper is process-scoped, so a process
// serving many loops attributes an orphan reap to whichever loop's store was
// installed last. An orphan is by definition a sandbox no Pool remembers, so
// there is no loop it "really" belongs to — the signal is a host-level one about
// instances dying without releasing their sandboxes, its tenant-free payload
// says so, and it is never used as a per-loop accounting record.
//
// A nil target DROPS the count, which is the honest state before any loop has a
// store (one-shot, shell, research-probe, mcp-server) — not an emission into a
// NoopStore that would make the wiring look live on a dashboard that then never
// moves.
type reapSink struct {
	mu sync.Mutex
	fn func(sandbox.ReleaseInfo)
}

// remoteReapSink is the one cell for this process. It is package-scoped because
// the substrate it serves is: one Service, one handler, one reaper.
var remoteReapSink = &reapSink{}

// observe is the function handed to sandbox.WithRemoteReapHook. It is installed
// at construction and stays installed; only its TARGET changes.
func (s *reapSink) observe(i sandbox.ReleaseInfo) {
	s.mu.Lock()
	fn := s.fn
	s.mu.Unlock()
	if fn != nil {
		fn(i)
	}
}

// set points the sink at a loop's emitter.
func (s *reapSink) set(fn func(sandbox.ReleaseInfo)) {
	s.mu.Lock()
	s.fn = fn
	s.mu.Unlock()
}

// target reads the current emitter (tests).
func (s *reapSink) target() func(sandbox.ReleaseInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fn
}

// swap installs fn and returns what was there (tests). It exists so a test can
// restore a process-scoped cell it disturbed.
func (s *reapSink) swap(fn func(sandbox.ReleaseInfo)) func(sandbox.ReleaseInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev := s.fn
	s.fn = fn
	return prev
}

// installRemoteReapObserver points the process's orphan-reap sink at one loop's
// event store. A nil store installs NOTHING and leaves the sink inert.
func installRemoteReapObserver(store event.EventStore, nodeID string) {
	if store == nil {
		return
	}
	// The SAME translator and the SAME event shape the Pool's own Reaped hook
	// uses: sandbox.ReleaseInfo in, KindSandboxReap out, and the CAUSE
	// (CauseOrphan vs CauseIdleTTL) is the only thing that distinguishes an
	// orphan sweep from an idle-TTL sweep. Building a second translator here
	// would be a second place the payload could drift.
	remoteReapSink.set(tools.SandboxEventHooks(store, nodeID).Reaped)
}

// kubernetesHandlerFactory builds the factory registered for
// `handler: kubernetes`, or returns nil when this process must not offer it.
//
// # The refusals it does NOT make
//
// It makes exactly ONE decision — whether to hand back a factory at all — and
// makes it on information that is settled before any model has run: the resolved
// Config. Every substantive refusal (a discarded config block, an unset
// advertise address under enforce, an unparsable listen address, a missing
// credential minter) belongs to kubernetes.newSubstrate and is left there, so
// there is one authority on what a buildable posture is and a diagnostic an
// operator reads names the field they typed.
//
// The factory closes over the proxy's credential source rather than the *Proxy,
// so nothing about the listener, the CA, or the policy reaches the substrate
// (sandbox.SandboxCredentialSource documents why that seam is narrow).
//
// nil is returned only for a posture that names a different handler. That is not
// a silent refusal: with no factory registered, selectHandler refuses the name
// loudly ("this binary has no factory for it"), which is the correct answer for
// a name this process deliberately did not offer — and it is a state this
// function only produces when the name is not kubernetes at all.
func kubernetesHandlerFactory(credentials sandbox.SandboxCredentialSource, instanceID string) func(sandbox.Config) (sandbox.Handler, error) {
	return func(cfg sandbox.Config) (sandbox.Handler, error) {
		// THE LOAD-TIME REFUSAL, checked HERE as well as inside the substrate.
		//
		// Config.Kubernetes.Refused is a positive assertion that the operator's
		// kubernetes: block was DISCARDED (WarnBadKubernetes), which a factory
		// cannot tell from "the operator wrote nothing" by looking at nil fields —
		// and those two must resolve differently. An absent block means "use the
		// substrate's defaults"; a refused one means build NOTHING, because the
		// handler was NAMED and the posture it was to be built with is unknown.
		//
		// It is checked at BOTH ends deliberately. The substrate's own check is the
		// authority for every caller; this one is what makes the composition root's
		// behaviour independent of the substrate seam, so a test that substitutes
		// the cluster still exercises the refusal — and so a future substrate
		// registered under another name inherits the discipline rather than having
		// to remember it.
		if cfg.Kubernetes.Refused {
			return nil, fmt.Errorf("the kubernetes: config block was discarded at load (bad_kubernetes); " +
				"refusing to build a substrate from defaults the operator did not choose — fix the reported " +
				"values in .fuse/sandbox.local.yml")
		}

		maxPods := int64(0)
		if cfg.Concurrency.MaxInflightPerTenant != nil {
			maxPods = *cfg.Concurrency.MaxInflightPerTenant
		}

		sub, err := newKubernetesSubstrate(kubernetesSubstrateOptions{
			Config:           cfg,
			MaxPodsPerTenant: maxPods,
			InstanceID:       instanceID,
			ProxyCredentials: credentials,
		})
		if err != nil {
			// Returned as-is. selectHandler wraps it in ErrRefusedUncontained and
			// does NOT fall through to the container or the host handler, which is
			// the whole of ADR-0058 rule 2; adding a fallback here would be the
			// silent substitution that rule forbids.
			return nil, err
		}

		// THE REAPER'S LIFETIME, stated deliberately because the seam invites the
		// opposite conclusion. NewRemoteHandler starts a goroutine and returns a
		// sandbox.Handler, so Close() is reachable only through a type assertion —
		// and this composition root does NOT perform one.
		//
		// That is the CORRECT choice for a process-scoped substrate, not an
		// oversight. sandbox.Service is immutable and has no shutdown of its own,
		// so there is no moment between "the Service exists" and "the process
		// exits" at which closing the reaper would be right. Closing it earlier
		// would stop collecting orphans while sandboxes were still live, which is
		// precisely the leak the reaper exists to bound. The goroutine's life is
		// therefore the process's, ends with it, and leaks nothing that outlives
		// the binary.
		return sandbox.NewRemoteHandler(sub, cfg,
			// The observability wiring. Without it, `sandbox.reap` with cause
			// `orphan` can never be emitted by a real fuse and
			// fuse_sandbox_reap_total{cause="orphan"} stays permanently at zero —
			// which reads exactly like a fleet that never leaks a sandbox.
			sandbox.WithRemoteReapHook(remoteReapSink.observe),
		)
	}
}

// instanceIDForSandbox is this fuse process's identifier on every sandbox Pod it
// creates.
//
// It is for LEGIBILITY and for the heartbeat's owner, and explicitly NOT for the
// reaper's selection: an orphan is precisely a Pod whose owning instance is gone,
// so a reaper that filtered on this label could never collect one. That is why a
// weak identifier is acceptable here in a way it would not be for an identity.
//
// hostname-pid, because in the shipped posture the hostname IS the Pod name
// (unique per replica) and the pid disambiguates two fuse processes sharing a
// host — a developer's laptop, or a container running more than one. A hostname
// that cannot be read degrades to the pid alone rather than to "", so two
// instances never look like one instance to an operator reading labels.
//
// It is truncated to a DNS-1123 label budget by the kubernetes package, which
// owns that constraint.
func instanceIDForSandbox() string {
	host, err := os.Hostname()
	if err != nil || strings.TrimSpace(host) == "" {
		return fmt.Sprintf("pid-%d", os.Getpid())
	}
	return fmt.Sprintf("%s-%d", host, os.Getpid())
}

// kubernetesProxyCredentials brings up the TLS listener a sandbox's egress
// sidecar reaches this instance's proxy on, and binds enrollment to the resolved
// egress policy.
//
// # Why the listener is brought up HERE and not in resolveEgressDatapath
//
// resolveEgressDatapath serves the CONTAINER substrate, whose datapath is a UNIX
// socket bind-mounted into the container — no TCP, no TLS, no certificates. A
// remote sandbox has no filesystem in common with this process, so its relay has
// to cross the network, which is what Proxy.ListenTLS and Proxy.Enroll exist
// for. Binding that listener for every local `fuse shell` would open a TCP port
// nobody asked for, so it happens only when the loaded config NAMES a remote
// handler and the posture is enforcing.
//
// Both failures below return a nil source, which is the fail-CLOSED direction
// and NOT a degradation: the substrate refuses to build at all without a
// credential minter under enforce (a sidecar with no certificate is closed by
// the proxy on an unknown serial and presents inside the sandbox as a network
// fault), so a nil source here becomes a loud refusal at selection rather than a
// quietly broken datapath.
func kubernetesProxyCredentials(proxy *sandbox.Proxy, cfg sandbox.Config, warnw io.Writer) sandbox.SandboxCredentialSource {
	if proxy == nil {
		// enforce with no forwarder artifact: resolveEgressDatapath already said
		// so, loudly, and there is no proxy to enroll against.
		return nil
	}
	listen := kubernetesProxyListen(cfg)
	ca, err := sandbox.NewCA()
	if err != nil {
		fmt.Fprintf(warnw, "sandbox: kubernetes egress — the in-process CA could not be minted (%v); "+
			"no sandbox can be issued a client certificate, so the kubernetes handler will REFUSE rather than "+
			"run Pods with no datapath\n", err)
		return nil
	}
	if err := proxy.ListenTLS(listen, ca); err != nil {
		fmt.Fprintf(warnw, "sandbox: kubernetes egress — the proxy's TLS listener could not bind %s (%v); "+
			"a sandbox Pod has no other way to reach this instance's proxy, so the kubernetes handler will "+
			"REFUSE rather than run Pods with no datapath\n", listen, err)
		return nil
	}
	fmt.Fprintf(warnw, "sandbox: kubernetes egress ENFORCED — sandbox Pods reach this instance's proxy over TLS on %s\n", listen)
	// The SAME resolved Config.Egress the substrate is built from, so the policy
	// a sandbox is served under and the posture its Pod was rendered to are one
	// value (sandbox.NewProxyCredentialSource documents why it is bound here
	// rather than passed per Enroll).
	return sandbox.NewProxyCredentialSource(proxy, cfg.Egress)
}

// kubernetesProxyListen is the address the TLS listener binds.
//
// It reads kubernetes.proxy.listen — the same value the substrate derives the
// per-Pod NetworkPolicy's permitted port and the sidecar's -upstream port from —
// so the port fuse listens on and the port a Pod is allowed to reach cannot
// drift. The default is spelled in ONE place, the kubernetes package's
// defaultProxyListen, and re-spelled here only because it is unexported there;
// TestKubernetesProxyListenDefaultAgreesWithTheSubstrate pins the agreement.
func kubernetesProxyListen(cfg sandbox.Config) string {
	if l := cfg.Kubernetes.ProxyListen; l != nil && *l != "" {
		return *l
	}
	return kubernetes.DefaultProxyListen
}
