package sandbox

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/ethanhinson/fuse/internal/loopauth"
)

// The DATAPATH through the `--network none` floor (change 0064, plan Q1).
//
// `--network none` leaves the container with loopback and nothing else: `lo` is
// up, and there is no route off the box. Every constant below exists to open
// exactly ONE hole in that, and to make it the only one:
//
//	host                                  container
//	----------------------------------    ------------------------------------
//	per-principal UNIX socket (Proxy) --> /run/fuse/egress.sock      (mount, ro)
//	statically linked forwarder       --> /run/fuse/egress-forward   (mount, ro)
//	                                      127.0.0.1:3128  <-- the forwarder
//	                                      HTTP_PROXY=http://127.0.0.1:3128
//
// The forwarder is fuse's own binary, bind-mounted in, because the image is not
// asked to cooperate: the pinned default (alpine:3.20) has no socat, and any
// image an operator names is equally unlikely to. It listens on loopback and
// relays each accepted connection to the mounted socket, which is what turns a
// filesystem hole into something curl/git/pip can address — curl has no
// UNIX-socket PROXY support, only `--unix-socket` for the target host.
//
// Why not `--network container:<proxy>`: that hands the workload the proxy
// container's real NIC, re-opening general egress, which is the exact failure
// this change exists to prevent. Loopback plus one filesystem hole re-opens
// nothing.
//
// # These paths are TRUSTED-SIDE CONSTANTS, deliberately
//
// The in-container paths, the loopback address, and the injected variables are
// compile-time values, not configuration. Nothing a caller passes and nothing a
// model emits selects any of them, so there is no option to audit and no value
// to validate: the learning `trusted-root-never-model-selectable` is satisfied
// structurally rather than by a check. The only per-run value in the whole
// datapath is the HOST socket path, and that comes from Proxy.Listen keyed by
// the principal — see containerHandler.Acquire.
const (
	// containerEgressSocket is where the principal's host-side proxy socket is
	// mounted. Read-only: the container connects to it and never needs to write
	// the inode. (Linux exempts sockets from a read-only mount's write check —
	// sb_permission() rejects MAY_WRITE only for regular files, directories and
	// symlinks — so connect(2) works while the mount stays ro.)
	containerEgressSocket = "/run/fuse/egress.sock"

	// containerEgressForwarder is where fuse's statically linked forwarder is
	// mounted. Read-only so the command cannot replace the binary that is about
	// to be — or already is — its own parent process.
	containerEgressForwarder = "/run/fuse/egress-forward"

	// containerEgressListen is the loopback address the forwarder listens on
	// INSIDE the container. 3128 is the conventional proxy port; the value only
	// has to agree with containerEgressProxyURL, and both are fixed here.
	containerEgressListen = "127.0.0.1:3128"

	// containerEgressProxyURL is what the injected *_PROXY variables carry. It
	// is an `http://` URL because the proxy speaks HTTP CONNECT (plan Q2), which
	// is what curl/git/pip do with an http-scheme proxy for BOTH http and https
	// destinations.
	containerEgressProxyURL = "http://" + containerEgressListen
)

// proxyEnvKeys are every variable that decides whether — and through what — a
// process in the container uses a proxy. All eight are trusted-side territory
// under enforcement: they are stripped from the resolved passthrough environment
// and re-injected from egressProxyEnv, so the operator's env_passthrough list can
// neither redirect egress at a host of its choosing (HTTP_PROXY) nor exempt
// destinations from it (NO_PROXY).
//
// Both cases are the same defect wearing different clothes, which is why the list
// is one list: a variable this package injects is a variable this package owns.
var proxyEnvKeys = [...]string{
	"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY",
	"http_proxy", "https_proxy", "all_proxy", "no_proxy",
}

// egressProxyEnv is the injected proxy environment, in the DOCUMENTED order:
// uppercase quartet then lowercase quartet, each in HTTP/HTTPS/ALL/NO order.
//
// Both cases are emitted because the ecosystem is split and the split is not
// principled — curl reads lowercase (and deliberately ignores uppercase
// `HTTP_PROXY` to avoid the CGI `Proxy:` header attack), Go's net/http and most
// language runtimes read either. Emitting one case would leave the other free
// for an inherited value to occupy.
//
// NO_PROXY is set EXPLICITLY EMPTY rather than omitted. Omitting it would leave
// whatever the image or a base layer baked in, and a single `NO_PROXY=*` is a
// complete bypass of everything above it.
func egressProxyEnv() []string {
	return []string{
		"HTTP_PROXY=" + containerEgressProxyURL,
		"HTTPS_PROXY=" + containerEgressProxyURL,
		"ALL_PROXY=" + containerEgressProxyURL,
		"NO_PROXY=",
		"http_proxy=" + containerEgressProxyURL,
		"https_proxy=" + containerEgressProxyURL,
		"all_proxy=" + containerEgressProxyURL,
		"no_proxy=",
	}
}

// stripProxyEnv removes every proxy-controlling key from an already-rendered
// `K=V` environment, returning a fresh slice.
//
// This runs under EgressEnforce whether or not a datapath was wired, because the
// two reasons are independent: with a datapath, a passthrough value must not
// compete with the injected one; without a datapath, an inherited HTTP_PROXY
// naming a host-side proxy is a description of an escape route that no longer
// exists, and handing it to the command only produces confusing failures.
//
// It is a STRIP and not a re-ordering. `--env K=V1 --env K=V2` resolves last-wins
// on docker, but this builder serves nerdctl and podman from the same argv, and
// making a trust boundary depend on how three runtimes each resolve a duplicate
// key is not a boundary. After this, no duplicate exists for them to resolve.
func stripProxyEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		key, _, _ := strings.Cut(kv, "=")
		if isProxyEnvKey(key) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func isProxyEnvKey(key string) bool {
	for _, k := range proxyEnvKeys {
		if key == k {
			return true
		}
	}
	return false
}

// egressForwarderCommand is the container's ENTRY COMMAND when the datapath is
// open: the forwarder, told where to listen and what to relay to, wrapping the
// shell invocation that would otherwise have been the entry command.
//
// It is an exec-wrap rather than a backgrounded `forwarder & cmd` for one
// reason: there is no race. The forwarder binds its listener BEFORE it spawns the
// child, so the first thing the command does can already be a `curl`. A
// backgrounded launch would need a readiness handshake through the filesystem to
// promise the same thing, and would get connection-refused flakes until someone
// wrote one.
//
// The shell is still `/bin/sh -c <cmd>`, unchanged and un-rewritten — cmd is
// passed through as one argument exactly as before, so nothing about how a
// command is quoted or interpreted changes when enforcement is on.
func egressForwarderCommand(cmd string) []string {
	return []string{
		containerEgressForwarder,
		"-listen", containerEgressListen,
		"-socket", containerEgressSocket,
		// `--` ends the forwarder's own flags: everything after it is the child
		// argv, so a command starting with a dash can never be read as a flag to
		// the forwarder.
		"--", "/bin/sh", "-c", cmd,
	}
}

// egressSocketSource yields the HOST path of the UNIX socket a principal's
// containers reach the network through. *Proxy is the implementation.
//
// The interface is UNEXPORTED on purpose, and so is the option that accepts one
// (withEgressDatapath): a caller-suppliable socket source is a caller-suppliable
// bind-mount into the container. The exported seam is WithEgressProxy, which
// takes a *Proxy and nothing else.
type egressSocketSource interface {
	Listen(loopauth.Principal, Egress) (string, error)
}

// resolveForwarderBinary canonicalises the host path of the forwarder binary,
// returning "" — meaning "no datapath" — for anything it cannot prove is an
// existing regular file.
//
// Failing to "" is the fail-CLOSED direction and is the whole reason this
// function exists. A bind-mount source that does not exist is not an error to
// docker: the daemon CREATES it, as a root-owned directory, and the container
// then holds a mount at the forwarder's fixed path containing nothing. The
// command would fail in a way that looks like a broken image rather than a
// missing artifact — and, worse, the injected proxy environment would be
// promising a listener that was never going to exist. With "" the datapath is
// simply not emitted: the floor stays on and enforcement is deny-all, which is
// exactly the state task 3 shipped.
//
// Symlinks are resolved so the path that reaches argv is the real file, for the
// same reason resolveMountRoot does it: the mount source must be the thing that
// was checked.
func resolveForwarderBinary(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return ""
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return ""
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.Mode().IsRegular() {
		return ""
	}
	return resolved
}
