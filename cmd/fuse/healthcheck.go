package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// defaultHealthcheckAddr mirrors the `loop-serve-net --addr` default. The
// container healthcheck is invoked with NO arguments (`["/fuse","healthcheck"]`),
// so these defaults must describe the server the same image starts.
const defaultHealthcheckAddr = "127.0.0.1:8787"

// runHealthcheck GETs http://<addr><path> and returns a process exit code: 0 on
// any 2xx, 1 on everything else — a non-2xx status, a connection refusal, a
// timeout, a malformed flag. It prints NOTHING on success: this is a container
// healthcheck, run every few seconds for the life of the container, and a
// chatty probe is the thing that fills a log.
//
// Readiness (/readyz) is the default probe path rather than liveness: the thing
// a `docker compose`/orchestrator healthcheck gates on is "should this instance
// receive traffic", which is exactly /readyz's question.
func runHealthcheck(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", defaultHealthcheckAddr, "TCP address of the fuse server to probe")
	path := fs.String("path", readyzPath, "probe path to GET")
	timeout := fs.Duration("timeout", 2*time.Second, "total time budget for the probe")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: fuse healthcheck [--addr host:port] [--path /readyz] [--timeout 2s]")
		return 1
	}

	url := "http://" + *addr + *path
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		fmt.Fprintf(stderr, "healthcheck: %v\n", err)
		return 1
	}
	// A dedicated client with no keep-alives: this process makes exactly one
	// request and exits, so a pooled connection would only leave the server
	// holding an idle socket per probe.
	client := &http.Client{
		Transport: &http.Transport{
			DisableKeepAlives: true,
			DialContext:       (&net.Dialer{}).DialContext,
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(stderr, "healthcheck: %v\n", err)
		return 1
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		fmt.Fprintf(stderr, "healthcheck: %s: %s\n", url, resp.Status)
		return 1
	}
	return 0
}
