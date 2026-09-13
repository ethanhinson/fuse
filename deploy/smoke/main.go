// Command smoke is the shared operator smoke client for the two deployment
// stacks in deploy/: the Compose dev stack (deploy/compose) and the Helm chart
// (deploy/charts/fuse). It is driven by `make compose-smoke` and
// `make helm-smoke`, never by CI — see the Makefile for why.
//
// It performs three checks against an ALREADY-RUNNING server, in order:
//
//  1. GET /readyz until 200, with bounded retries. This is the durable-store
//     readiness gate, so it is the check that proves the stack's Postgres wiring
//     came up, not just the process.
//  2. GET /metrics on the metrics port and assert at least one `fuse_` sample
//     line. A 200 with no fuse_ series means the Prometheus registry was never
//     populated — the endpoint answering is not the same as the server being
//     instrumented.
//  3. One loop.start over the PUBLIC Go SDK (sdk/fuse, remote backend), printing
//     the returned LoopID. This is the whole point: it exercises the authenticated
//     Connect wire end-to-end through the same client an operator would write,
//     rather than curling a handler.
//
// ON THE MODEL: loop.start returns the LoopID as soon as the loop is LAUNCHED —
// the first turn runs asynchronously under the loop-lifetime context — so this
// check proves the RPC without waiting on a model round-trip, and it passes -model
// "" so the server resolves its own default. The launched loop will still attempt
// one turn in the background. If you care about that turn succeeding, point the
// stack's config at a cheap model on your own gateway FIRST; this command never
// names a model and never asserts on the turn.
//
//	go run ./deploy/smoke -addr http://127.0.0.1:8787 -metrics http://127.0.0.1:9090 \
//	    -token fuse-dev-token -tenant _default
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	fuse "github.com/ethanhinson/fuse/sdk/fuse"
)

func main() {
	addr := flag.String("addr", "http://127.0.0.1:8787", "base URL of the Connect endpoint")
	metrics := flag.String("metrics", "http://127.0.0.1:9090", "base URL of the metrics endpoint")
	metricsPath := flag.String("metrics-path", "/metrics", "path of the metrics endpoint")
	token := flag.String("token", "fuse-dev-token", "bearer token")
	tenant := flag.String("tenant", "_default", "tenant the token acts as")
	task := flag.String("task", "deployment smoke: report readiness and stop", "loop.start task")
	attempts := flag.Int("attempts", 60, "readiness attempts before giving up")
	interval := flag.Duration("interval", 2*time.Second, "delay between readiness attempts")
	flag.Parse()

	if err := run(context.Background(), config{
		addr:        strings.TrimSuffix(*addr, "/"),
		metrics:     strings.TrimSuffix(*metrics, "/") + *metricsPath,
		token:       *token,
		tenant:      *tenant,
		task:        *task,
		attempts:    *attempts,
		interval:    *interval,
		httpTimeout: 10 * time.Second,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "SMOKE FAILED: %v\n", err)
		os.Exit(1)
	}
}

type config struct {
	addr        string
	metrics     string
	token       string
	tenant      string
	task        string
	attempts    int
	interval    time.Duration
	httpTimeout time.Duration
}

func run(ctx context.Context, cfg config) error {
	client := &http.Client{Timeout: cfg.httpTimeout}

	readyz := cfg.addr + "/readyz"
	if err := waitReady(ctx, client, readyz, cfg.attempts, cfg.interval); err != nil {
		return err
	}
	fmt.Printf("ok: %s returned 200\n", readyz)

	n, err := scrapeFuseMetrics(ctx, client, cfg.metrics)
	if err != nil {
		return err
	}
	fmt.Printf("ok: %s exposed %d fuse_ sample lines\n", cfg.metrics, n)

	c := fuse.NewRemote(cfg.addr, fuse.Credentials{Token: cfg.token, Tenant: cfg.tenant})
	startCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	// Model is deliberately empty: the server resolves its configured default, so
	// this command never embeds a model id of its own. See the package comment.
	id, err := c.StartLoop(startCtx, fuse.StartLoopConfig{Task: cfg.task})
	if err != nil {
		return fmt.Errorf("loop.start over the Go SDK: %w", err)
	}
	if id == "" {
		return errors.New("loop.start returned an empty loop id")
	}
	fmt.Printf("ok: loop.start over the Go SDK returned loop id %s\n", id)
	fmt.Println("SMOKE PASSED")
	return nil
}

// waitReady polls url until it answers 200. A connection error and a non-200 are
// treated identically — both mean "not ready yet" — because a stack coming up
// produces the first and a server whose store is not open produces the second.
func waitReady(ctx context.Context, client *http.Client, url string, attempts int, interval time.Duration) error {
	var last error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(interval):
			}
		}
		code, _, err := get(ctx, client, url)
		switch {
		case err != nil:
			last = err
		case code == http.StatusOK:
			return nil
		default:
			last = fmt.Errorf("status %d", code)
		}
	}
	return fmt.Errorf("%s never returned 200 after %d attempts: %w", url, attempts, last)
}

// scrapeFuseMetrics asserts the endpoint answers 200 AND carries at least one
// fuse_ sample line. Counting the lines rather than just finding the substring
// keeps a stray comment from satisfying the check.
func scrapeFuseMetrics(ctx context.Context, client *http.Client, url string) (int, error) {
	code, body, err := get(ctx, client, url)
	if err != nil {
		return 0, fmt.Errorf("scrape %s: %w", url, err)
	}
	if code != http.StatusOK {
		return 0, fmt.Errorf("scrape %s: status %d", url, code)
	}
	n := 0
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "fuse_") {
			n++
		}
	}
	if n == 0 {
		return 0, fmt.Errorf("scrape %s: 200 but no fuse_ sample lines (registry not populated?)", url)
	}
	return n, nil
}

func get(ctx context.Context, client *http.Client, url string) (int, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, "", err
	}
	return resp.StatusCode, string(body), nil
}
