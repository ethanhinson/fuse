// Command fuse-egress-forward is the IN-CONTAINER half of fuse's egress control
// (change 0064).
//
// # What it is for
//
// A sandboxed bash container runs with `--network none`: loopback is up and
// there is no route off the box. The one hole fuse opens is a UNIX socket,
// bind-mounted read-only from the host, on which fuse's own egress proxy speaks
// HTTP CONNECT against the operator's allowlist. But nothing a command actually
// runs can USE a UNIX-socket proxy — curl's `--unix-socket` names the TARGET, not
// a proxy, and git/pip/go have no such notion at all. Every one of them can use
// an `http://host:port` proxy.
//
// So this program is the adapter: it listens on 127.0.0.1 inside the container
// and relays each accepted connection, byte for byte, to the mounted socket. It
// understands nothing about HTTP — the proxy on the far side does all the
// deciding — which is the point: no policy lives on the container side of the
// boundary, where the model's command could reach it.
//
// It is bind-mounted in from the host rather than installed in the image because
// the image is not asked to cooperate. The pinned default (alpine:3.20) has no
// socat, and an operator's own image is no likelier to. Built with CGO_ENABLED=0
// (`make egress-forwarder`) it is a static binary with no libc, no interpreter,
// and no runtime dependency on anything in the image.
//
// # Usage
//
//	fuse-egress-forward -listen 127.0.0.1:3128 -socket /run/fuse/egress.sock \
//	    -- /bin/sh -c "<command>"
//
// With a child command it becomes that command's PARENT: it binds the listener
// FIRST, then execs the child, so the very first thing the command does can be a
// network call with no readiness race. It exits with the child's exit status, and
// relays SIGINT/SIGTERM to it. With no child it serves until killed.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

// run is main's body, returning the process exit code so it is testable.
func run(argv []string) int {
	fs := flag.NewFlagSet("fuse-egress-forward", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	listen := fs.String("listen", "", "loopback address to listen on (e.g. 127.0.0.1:3128)")
	socket := fs.String("socket", "", "path of the mounted UNIX socket to relay to")
	if err := fs.Parse(argv); err != nil {
		return 2
	}
	if *listen == "" || *socket == "" {
		fmt.Fprintln(os.Stderr, "fuse-egress-forward: -listen and -socket are both required")
		return 2
	}

	// Bound BEFORE the child is started. This ordering is the whole reason the
	// forwarder wraps the command instead of being backgrounded next to it: when
	// the child's first instruction runs, the listener is already accepting.
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fuse-egress-forward: listen on %s: %v\n", *listen, err)
		return 1
	}
	defer func() { _ = ln.Close() }()

	go serve(ln, *socket)

	child := fs.Args()
	if len(child) == 0 {
		// No command to wrap: serve until the container is torn down. Nothing
		// else can end this process, which is correct — a forwarder that exited
		// on its own would silently close the only path out.
		select {}
	}
	return runChild(child)
}

// serve accepts loopback connections and relays each to the mounted socket.
//
// An accept error ends the loop rather than retrying: the only expected cause is
// the listener being closed at teardown, and spinning on a broken listener would
// burn a core inside a cgroup-capped container for the rest of the command's run.
func serve(ln net.Listener, socket string) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go relay(conn, socket)
	}
}

// relay joins one accepted loopback connection to a fresh connection on the
// mounted UNIX socket.
//
// One upstream connection PER accepted connection, never a shared or pooled one:
// the far side speaks HTTP CONNECT, and a CONNECT tunnel owns its connection for
// its whole life. Multiplexing would splice two commands' tunnels together.
//
// A dial failure closes the client connection with nothing written. There is no
// error page and no fallback path, because any fallback here would be a way out
// of the container that did not pass the proxy.
func relay(client net.Conn, socket string) {
	defer func() { _ = client.Close() }()

	upstream, err := net.Dial("unix", socket)
	if err != nil {
		return
	}
	defer func() { _ = upstream.Close() }()

	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(upstream, client)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(client, upstream)
		done <- struct{}{}
	}()

	// Closing both halves as soon as either direction ends is what unblocks the
	// other copy: a half-closed tunnel has no reader left to consume anything the
	// remaining half sends.
	<-done
	_ = client.Close()
	_ = upstream.Close()
	<-done
}

// runChild execs the wrapped command with this process's stdio and returns its
// exit status.
//
// Signals are relayed rather than ignored. This process is PID 1 in the
// container, and PID 1 has no default disposition for SIGTERM — a `docker stop`
// against an unhandled PID 1 waits out the full grace period and then SIGKILLs
// the whole container, which would look to a caller like every command hanging on
// cancellation.
func runChild(argv []string) int {
	cmd := exec.Command(argv[0], argv[1:]...) // #nosec G204 -- argv is fuse-supplied; see package doc
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "fuse-egress-forward: %v\n", err)
		return 127
	}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigs)
	relayed := make(chan struct{})
	go func() {
		for {
			select {
			case sig := <-sigs:
				_ = cmd.Process.Signal(sig)
			case <-relayed:
				return
			}
		}
	}()

	err := cmd.Wait()
	close(relayed)
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if code := exitErr.ExitCode(); code >= 0 {
			return code
		}
		// Killed by a signal: the shell convention, so a caller reading only the
		// status still sees a failure.
		return 128
	}
	fmt.Fprintf(os.Stderr, "fuse-egress-forward: %v\n", err)
	return 1
}
