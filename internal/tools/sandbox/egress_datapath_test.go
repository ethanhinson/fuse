package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ethanhinson/fuse/internal/loopauth"
)

// --- change 0064 task 6: the datapath through the floor ----------------------

// fakeForwarder writes a stand-in for the statically linked forwarder binary and
// returns its host path. argv is the boundary under test here, so what the file
// CONTAINS is irrelevant; that it is a real regular file is not, because the
// handler refuses to mount anything it cannot resolve to one.
func fakeForwarder(t *testing.T) string {
	t.Helper()
	path := filepath.Join(shortDir(t), "fuse-egress-forward")
	if err := os.WriteFile(path, []byte("#!/bin/true\n"), 0o755); err != nil {
		t.Fatalf("write fake forwarder: %v", err)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("EvalSymlinks(forwarder): %v", err)
	}
	return resolved
}

// datapathHandler builds a container handler carrying BOTH the trusted egress
// posture and the trusted datapath, applied exactly as NewService applies them.
func datapathHandler(t *testing.T, rec *recordingRun, e Egress, p *Proxy, forwarder string) *containerHandler {
	t.Helper()
	h, err := newContainerHandler(DefaultConfig(),
		withLookPath(fakeLookPath("docker")),
		withExecRunner(rec.run),
		withTrustedRoot(trustedTestRoot(t)),
		withEgress(e),
		withEgressDatapath(p, forwarder),
	)
	if err != nil {
		t.Fatalf("newContainerHandler: %v", err)
	}
	return h
}

// Under EgressEnforce with the datapath wired, argv carries the WHOLE hole: the
// principal's proxy socket mounted read-only at the fixed in-container path, the
// forwarder binary mounted read-only beside it, the injected proxy environment,
// and the forwarder as the container's entry command wrapping the shell.
//
// Asserted as a whole-argv golden because argv IS this handler's security
// boundary: a datapath that is "mostly right" is a hole in the wrong place.
func TestContainerArgvEgressDatapathUnderEnforce(t *testing.T) {
	rec := &recordingRun{}
	proxy := newTestProxy(t)
	forwarder := fakeForwarder(t)
	policy := Egress{Mode: EgressEnforce, Allow: []AllowEntry{{Host: "example.com", Port: 443}}}
	h := datapathHandler(t, rec, policy, proxy, forwarder)

	principal := loopauth.Principal{Tenant: "t1", Subject: "s1"}
	argv := func() []string {
		r, err := h.Acquire(context.Background(), principal, Env{Allow: map[string]string{"PATH": "/usr/bin", "HOME": "/root"}})
		if err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		if _, err := r.Exec(context.Background(), "true", ""); err != nil {
			t.Fatalf("Exec: %v", err)
		}
		return rec.args
	}()

	// Listen is idempotent per principal, so asking again yields the SAME socket
	// the Acquire above was handed. That is also the assertion that the path in
	// argv came from the proxy rather than from anything the caller chose.
	socket, err := proxy.Listen(principal, policy)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}

	want := []string{
		"run", "--rm", "-i",
		"--network", "none",
		"-v", socket + ":" + containerEgressSocket + ":ro",
		"-v", forwarder + ":" + containerEgressForwarder + ":ro",
		"--env", "HTTP_PROXY=" + containerEgressProxyURL,
		"--env", "HTTPS_PROXY=" + containerEgressProxyURL,
		"--env", "ALL_PROXY=" + containerEgressProxyURL,
		"--env", "NO_PROXY=",
		"--env", "http_proxy=" + containerEgressProxyURL,
		"--env", "https_proxy=" + containerEgressProxyURL,
		"--env", "all_proxy=" + containerEgressProxyURL,
		"--env", "no_proxy=",
		"--env", "HOME=/root",
		"--env", "PATH=/usr/bin",
		"-v", h.root + ":" + containerWorkspace,
		"-w", containerWorkspace,
		"--pull=never",
		DefaultContainerImage,
		containerEgressForwarder,
		"-listen", containerEgressListen,
		"-socket", containerEgressSocket,
		"--", "/bin/sh", "-c", "true",
	}
	if !reflect.DeepEqual(argv, want) {
		t.Fatalf("argv =\n%#v\nwant\n%#v", argv, want)
	}
}

// Under allow-all NOTHING of the datapath appears, even with a proxy and a
// forwarder wired. The mode is the only switch: an operator who never turned
// egress control on gets byte-for-byte the pre-0064 argv, with no mounts, no
// injected proxy variables, and the plain `/bin/sh -c` entry command.
func TestContainerArgvNoEgressDatapathUnderAllowAll(t *testing.T) {
	rec := &recordingRun{}
	forwarder := fakeForwarder(t)
	h := datapathHandler(t, rec, Egress{Mode: EgressAllowAll}, newTestProxy(t), forwarder)

	r, err := h.Acquire(context.Background(), loopauth.Principal{Tenant: "t1", Subject: "s1"}, Env{Allow: map[string]string{"PATH": "/usr/bin"}})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if _, err := r.Exec(context.Background(), "true", ""); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	argv := rec.args

	want := []string{
		"run", "--rm", "-i",
		"--env", "PATH=/usr/bin",
		"-v", h.root + ":" + containerWorkspace,
		"-w", containerWorkspace,
		"--pull=never",
		DefaultContainerImage, "/bin/sh", "-c", "true",
	}
	if !reflect.DeepEqual(argv, want) {
		t.Fatalf("allow-all argv =\n%#v\nwant\n%#v", argv, want)
	}
	for _, banned := range []string{containerEgressSocket, containerEgressForwarder, containerEgressProxyURL, forwarder} {
		if strings.Contains(strings.Join(argv, " "), banned) {
			t.Fatalf("allow-all argv leaked datapath fragment %q: %#v", banned, argv)
		}
	}
}

// THE REGRESSION the plan names (learning `trusted-root-never-model-selectable`).
//
// The proxy variables are injected by the TRUSTED side. An operator's
// env_passthrough list naming HTTP_PROXY — or any of its seven siblings — must
// not be able to redirect the container's egress at a host of the passthrough
// value's choosing, which under enforce is the difference between "all traffic is
// policed" and "all traffic goes wherever that value says".
//
// Note WHY this is a strip rather than an ordering trick: `--env K=V1 --env K=V2`
// resolves last-wins on docker, and this builder serves nerdctl and podman from
// the same argv. Relying on any of the three to resolve a duplicate our way would
// make the trust boundary a property of the runtime. There is no duplicate.
func TestContainerEgressProxyEnvIsNotOverridableByPassthrough(t *testing.T) {
	const rogue = "http://attacker.example:8080"

	rec := &recordingRun{}
	policy := Egress{Mode: EgressEnforce}
	h := datapathHandler(t, rec, policy, newTestProxy(t), fakeForwarder(t))

	// Exactly what an operator writes as `env_passthrough: [HTTP_PROXY, ...]`
	// plus a host environment supplying each one — the full resolved shape, not a
	// hand-poked map.
	passthrough := []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "all_proxy", "no_proxy"}
	env := ResolveEnv(passthrough, func(key string) (string, bool) {
		switch key {
		case "PATH":
			return "/usr/bin", true
		case "NO_PROXY", "no_proxy":
			// The hole-punching shape: an inherited NO_PROXY that would exempt
			// every destination from the proxy entirely.
			return "*", true
		case "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy":
			return rogue, true
		}
		return "", false
	})

	r, err := h.Acquire(context.Background(), loopauth.Principal{Tenant: "t1", Subject: "s1"}, env)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if _, err := r.Exec(context.Background(), "true", ""); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	argv := rec.args

	if strings.Contains(strings.Join(argv, " "), rogue) {
		t.Fatalf("passthrough value reached argv — the trusted proxy env was overridable: %#v", argv)
	}
	if containsPair(argv, "--env", "NO_PROXY=*") || containsPair(argv, "--env", "no_proxy=*") {
		t.Fatalf("inherited NO_PROXY punched a hole through the proxy: %#v", argv)
	}
	for _, want := range []string{
		"HTTP_PROXY=" + containerEgressProxyURL,
		"HTTPS_PROXY=" + containerEgressProxyURL,
		"ALL_PROXY=" + containerEgressProxyURL,
		"NO_PROXY=",
		"http_proxy=" + containerEgressProxyURL,
		"https_proxy=" + containerEgressProxyURL,
		"all_proxy=" + containerEgressProxyURL,
		"no_proxy=",
	} {
		if !containsPair(argv, "--env", want) {
			t.Fatalf("argv missing the trusted %q: %#v", want, argv)
		}
		// Exactly ONE occurrence: a second `--env HTTP_PROXY=...` anywhere would
		// hand the duplicate-resolution decision to the container runtime.
		if n := countPair(argv, "--env", strings.SplitN(want, "=", 2)[0]); n != 1 {
			t.Fatalf("argv carries %d --env entries for %q, want exactly 1: %#v", n, strings.SplitN(want, "=", 2)[0], argv)
		}
	}
}

// countPair counts `flag <value>` pairs whose value has the given `KEY=` prefix.
func countPair(args []string, flag, key string) int {
	n := 0
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && strings.HasPrefix(args[i+1], key+"=") {
			n++
		}
	}
	return n
}

// The datapath is only ever as wired as the trusted side made it. A forwarder
// path that does not resolve to a real regular file yields NO datapath — the
// floor stays on and the container gets no hole at all.
//
// The direction matters: mounting an unresolvable path would have the daemon
// invent it (as a root-owned directory), and the container would then hold a
// mount at the forwarder's fixed path that fuse never put anything in.
func TestContainerArgvEgressDatapathInertWhenForwarderMissing(t *testing.T) {
	for name, forwarder := range map[string]string{
		"absent":       filepath.Join(shortDir(t), "not-there"),
		"a directory":  shortDir(t),
		"unset":        "",
		"only spaces ": "   ",
	} {
		t.Run(name, func(t *testing.T) {
			rec := &recordingRun{}
			h := datapathHandler(t, rec, Egress{Mode: EgressEnforce}, newTestProxy(t), forwarder)
			r, err := h.Acquire(context.Background(), loopauth.Principal{Tenant: "t1", Subject: "s1"}, Env{})
			if err != nil {
				t.Fatalf("Acquire: %v", err)
			}
			if _, err := r.Exec(context.Background(), "true", ""); err != nil {
				t.Fatalf("Exec: %v", err)
			}
			if !containsPair(rec.args, "--network", "none") {
				t.Fatalf("floor missing: %#v", rec.args)
			}
			if strings.Contains(strings.Join(rec.args, " "), containerEgressSocket) {
				t.Fatalf("argv opened a datapath with no forwarder binary: %#v", rec.args)
			}
			if strings.Contains(strings.Join(rec.args, " "), containerEgressProxyURL) {
				t.Fatalf("argv injected proxy env with no datapath behind it: %#v", rec.args)
			}
		})
	}
}

// Two principals on one Proxy get two DIFFERENT sockets in argv. The socket path
// is the principal's identity at the proxy (see Proxy.Listen), so a datapath that
// handed the same path to both would erase the whole per-principal policy split
// upstream of any matching.
func TestContainerArgvEgressSocketIsPerPrincipal(t *testing.T) {
	proxy := newTestProxy(t)
	forwarder := fakeForwarder(t)
	policy := Egress{Mode: EgressEnforce}

	socketFor := func(p loopauth.Principal) string {
		rec := &recordingRun{}
		h := datapathHandler(t, rec, policy, proxy, forwarder)
		r, err := h.Acquire(context.Background(), p, Env{})
		if err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		if _, err := r.Exec(context.Background(), "true", ""); err != nil {
			t.Fatalf("Exec: %v", err)
		}
		for i := 0; i+1 < len(rec.args); i++ {
			if rec.args[i] == "-v" && strings.HasSuffix(rec.args[i+1], ":"+containerEgressSocket+":ro") {
				return strings.TrimSuffix(rec.args[i+1], ":"+containerEgressSocket+":ro")
			}
		}
		t.Fatalf("no egress socket mount in argv: %#v", rec.args)
		return ""
	}

	a := socketFor(loopauth.Principal{Tenant: "t1", Subject: "alice"})
	b := socketFor(loopauth.Principal{Tenant: "t1", Subject: "bob"})
	if a == b {
		t.Fatalf("both principals were handed the same egress socket %q", a)
	}
}

// withRogueEgressDatapath is a caller-supplied containerOption applied BEFORE the
// ones NewService appends, standing in for any seam a caller could reach.
func withRogueEgressDatapath(src egressSocketSource, forwarder string) ServiceOption {
	return func(o *serviceOptions) {
		o.containerOpts = append(o.containerOpts, withEgressDatapath(src, forwarder))
	}
}

// Trust ordering, the same rule the floor itself follows: the datapath is
// established by the trusted side and applied LAST, so a caller-supplied option
// cannot substitute its own socket source or its own forwarder binary — either of
// which would put a fuse-mounted, fuse-launched binary of someone else's choosing
// inside the container.
func TestServiceAppliesTrustedEgressDatapathLast(t *testing.T) {
	rec := &recordingRun{}
	trusted := newTestProxy(t)
	forwarder := fakeForwarder(t)
	rogueForwarder := fakeForwarder(t)

	cfg := DefaultConfig()
	cfg.Egress = Egress{Mode: EgressEnforce}

	svc, err := NewService(cfg,
		withRogueEgressDatapath(newTestProxy(t), rogueForwarder),
		withContainerLookPath(fakeLookPath("docker")),
		withContainerExec(rec.run),
		WithTrustedRoot(trustedTestRoot(t)),
		WithEgressProxy(trusted, forwarder),
	)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	h, ok := svc.handler.(*containerHandler)
	if !ok {
		t.Fatalf("handler = %T, want *containerHandler", svc.handler)
	}
	if h.egressForwarder != forwarder {
		t.Fatalf("handler forwarder = %q, want the trusted %q — a caller option replaced the mounted binary", h.egressForwarder, forwarder)
	}

	r, err := svc.Acquire(context.Background(), loopauth.Principal{Tenant: "t1", Subject: "s1"})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if _, err := r.Exec(context.Background(), "true", ""); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	joined := strings.Join(rec.args, " ")
	if !strings.Contains(joined, trusted.Root()) {
		t.Fatalf("argv does not mount a socket from the TRUSTED proxy root %q: %#v", trusted.Root(), rec.args)
	}
	if strings.Contains(joined, rogueForwarder) {
		t.Fatalf("argv mounts the caller-supplied forwarder: %#v", rec.args)
	}
}

// "The trusted side wins" has to include the trusted side saying NOTHING.
//
// With no WithEgressProxy at all, a caller-supplied datapath must be ERASED, not
// merely un-overridden — otherwise a caller could open the hole simply by being
// the only one who mentioned it, and a composition root that never opted in would
// be running an enforcing sandbox with someone else's socket and someone else's
// binary mounted inside it. Enforcement falls back to the datapath-less deny-all.
func TestServiceWithNoDeclaredDatapathErasesACallerSupplied(t *testing.T) {
	rec := &recordingRun{}
	rogueForwarder := fakeForwarder(t)

	cfg := DefaultConfig()
	cfg.Egress = Egress{Mode: EgressEnforce}

	svc, err := NewService(cfg,
		withRogueEgressDatapath(newTestProxy(t), rogueForwarder),
		withContainerLookPath(fakeLookPath("docker")),
		withContainerExec(rec.run),
		WithTrustedRoot(trustedTestRoot(t)),
		// Deliberately NO WithEgressProxy.
	)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	r, err := svc.Acquire(context.Background(), loopauth.Principal{Tenant: "t1", Subject: "s1"})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if _, err := r.Exec(context.Background(), "true", ""); err != nil {
		t.Fatalf("Exec: %v", err)
	}

	if !containsPair(rec.args, "--network", "none") {
		t.Fatalf("floor missing: %#v", rec.args)
	}
	joined := strings.Join(rec.args, " ")
	if strings.Contains(joined, rogueForwarder) || strings.Contains(joined, containerEgressSocket) {
		t.Fatalf("an undeclared datapath survived into argv: %#v", rec.args)
	}
}
