package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	"github.com/ethanhinson/fuse/internal/loopconnect"
	"github.com/ethanhinson/fuse/internal/loopwire/v1/loopv1connect"
	"github.com/ethanhinson/fuse/internal/runtime"
)

// control_socket.go gives a run that has no server of its own - the one-shot CLI - a way
// to be steered while it works. It serves the same fuse.loop.v1 Connect service as
// loop-serve-net, over a unix socket. Anything that can reach a fuse server can then
// reach this run: `fuse steer`, `fuse orchestrate`, and the SDKs.
//
// There is no bearer token. The socket file is created 0600, so only the user who
// started the run can connect; the same trust the one-shot run already gives its own
// terminal.

// serveControlSocket listens on path and serves the loop service over rt until ctx is
// cancelled or the returned stop is called. A leftover socket file from a crashed run is
// replaced; any other file at path is an error.
func serveControlSocket(ctx context.Context, path string, rt runtime.Runtime) (stop func(), err error) {
	if fi, statErr := os.Lstat(path); statErr == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("control socket: %s exists and is not a socket", path)
		}
		if c, derr := net.DialTimeout("unix", path, time.Second); derr == nil {
			_ = c.Close()
			return nil, fmt.Errorf("control socket: %s is in use by another run", path)
		}
		_ = os.Remove(path)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("control socket: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		_ = os.Remove(path)
		return nil, fmt.Errorf("control socket: %w", err)
	}

	handler := loopconnect.NewHandler(rt).WithBaseContext(ctx)
	mux := http.NewServeMux()
	route, h := loopv1connect.NewLoopServiceHandler(handler)
	mux.Handle(route, h)
	srv := &http.Server{
		Handler:     h2c.NewHandler(mux, &http2.Server{}),
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	go func() {
		if serr := srv.Serve(ln); serr != nil && !errors.Is(serr, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "control socket: %v\n", serr)
		}
	}()
	stopped := make(chan struct{})
	stop = func() {
		select {
		case <-stopped:
			return
		default:
			close(stopped)
		}
		// Observe streams never finish by themselves, so close rather than wait.
		_ = srv.Close()
		_ = os.Remove(path)
	}
	go func() {
		<-ctx.Done()
		stop()
	}()
	return stop, nil
}
