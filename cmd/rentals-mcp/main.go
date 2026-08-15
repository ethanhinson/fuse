// Command rentals-mcp serves the Wander demo's rentals MCP server (change #60) on a
// real address, so the consolidated demo app — and anything else speaking MCP over
// HTTP/SSE — can dial it like any other MCP server.
//
// It is the servable counterpart to internal/mcpdemo/rentals.NewServer (the test
// constructor): this binary takes the same rentals.Config, gets the routed handler from
// rentals.NewHandler, and serves it with graceful shutdown on SIGINT/SIGTERM.
//
// All wiring lives in runContext so it is testable end-to-end on 127.0.0.1:0 without a
// process; main is only signal plumbing and an exit code. Flags are documented in
// cmd/rentals-mcp/README.md.
package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/mcpdemo/rentals"
	"github.com/ethanhinson/fuse/internal/research"
)

const (
	envAddr         = "RENTALS_ADDR"
	envAudience     = "RENTALS_AUDIENCE"
	envSigningKey   = "RENTALS_SIGNING_KEY"
	envTenants      = "RENTALS_TENANTS"
	envFavoritesDir = "RENTALS_FAVORITES_DIR"
	envData         = "RENTALS_DATA"

	defaultShutdownGrace = 10 * time.Second
)

func main() {
	// SIGINT/SIGTERM cancel the run context, which triggers graceful shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Getenv); err != nil {
		fmt.Fprintf(os.Stderr, "rentals-mcp: %v\n", err)
		os.Exit(1)
	}
}

// run is the entrypoint's wiring: parse args/env, build the rentals config, serve until
// ctx is cancelled. Kept separate from main (which owns only signals and the exit code)
// so tests drive the whole binary in-process.
func run(ctx context.Context, args []string, env func(string) string) error {
	return runContext(ctx, runOpts{args: args, env: env, stderr: os.Stderr})
}

// runOpts are runContext's injectable seams. Only args/env are user-facing; stderr,
// ready and shutdownGrace exist so a test can capture logs, learn the ephemeral port
// the server actually bound, and bound its own teardown.
type runOpts struct {
	args   []string
	env    func(string) string
	stderr io.Writer
	// ready, when non-nil, is called with the bound address once the listener is
	// open and before the serve loop starts.
	ready func(addr string)
	// shutdownGrace bounds graceful shutdown before connections are forced closed.
	// ≤0 ⇒ defaultShutdownGrace.
	shutdownGrace time.Duration
}

// tenantKeyFlag collects repeated --tenant-key tenant=<hex> pairs.
type tenantKeyFlag []string

func (f *tenantKeyFlag) String() string     { return strings.Join(*f, ",") }
func (f *tenantKeyFlag) Set(v string) error { *f = append(*f, v); return nil }

func runContext(ctx context.Context, o runOpts) error {
	env := o.env
	if env == nil {
		env = func(string) string { return "" }
	}
	errOut := o.stderr
	if errOut == nil {
		errOut = io.Discard
	}
	log := slog.New(slog.NewTextHandler(errOut, &slog.HandlerOptions{Level: slog.LevelInfo}))

	fs := flag.NewFlagSet("rentals-mcp", flag.ContinueOnError)
	fs.SetOutput(errOut)
	addr := fs.String("addr", envOr(env, envAddr, "127.0.0.1:8091"), "listen address (host:port); port 0 picks an ephemeral port")
	audience := fs.String("audience", env(envAudience), "RFC 8707 resource id this server accepts (required)")
	signingKey := fs.String("signing-key", env(envSigningKey), "fuse tool-identity signing key; per-tenant verification keys are derived from it")
	tenants := fs.String("tenants", env(envTenants), "comma-separated tenant ids to key from the signing key (requires --signing-key); include _default for fuse's default tenant")
	favoritesDir := fs.String("favorites-dir", env(envFavoritesDir), "directory for durable per-principal favorites; empty ⇒ in-memory (lost on restart)")
	data := fs.String("data", envOr(env, envData, "auto"), `listing backend: "canned" (hermetic), "live" (web search, requires a credential), or "auto" (live if a credential is set, else canned)`)
	maxResults := fs.Int("max-results", 5, "maximum listings returned by the live backend")
	var tenantKeys tenantKeyFlag
	fs.Var(&tenantKeys, "tenant-key", "explicit verification key as tenant=<hex>; repeatable, and combinable with --signing-key")
	if err := fs.Parse(o.args); err != nil {
		return err
	}

	if strings.TrimSpace(*audience) == "" {
		return errors.New("--audience is required (or set " + envAudience + "): it is the RFC 8707 resource id this server accepts, and every delegated token is bound to it")
	}

	keys, err := buildTenantKeys(*signingKey, *tenants, tenantKeys)
	if err != nil {
		return err
	}
	if len(keys) == 0 {
		return errors.New("no tenant verification keys configured: pass --signing-key with --tenants (or " + envSigningKey + "/" + envTenants + "), or one or more --tenant-key tenant=<hex>. With none, every caller is unauthorized")
	}

	favorites, err := buildFavorites(*favoritesDir)
	if err != nil {
		return err
	}

	source, err := resolveDataSource(ctx, *data, *maxResults, env, log)
	if err != nil {
		return err
	}

	_, handler := rentals.NewHandler(rentals.Config{
		Data:       source,
		Audience:   *audience,
		TenantKeys: keys,
		Favorites:  favorites,
	})

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", *addr, err)
	}

	srv := &http.Server{
		Handler: handler,
		// Read side is bounded: the MCP surface is one long-lived GET /sse with no
		// body plus short JSON-RPC POSTs to /messages.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
		// WriteTimeout is deliberately UNSET: /sse is an unbounded server-push
		// stream, and any write deadline would sever every live MCP session at
		// exactly that interval. Shutdown, not a write deadline, bounds a stream.
		WriteTimeout: 0,
	}

	bound := ln.Addr().String()
	log.Info("rentals-mcp listening",
		"addr", bound, "audience", *audience, "tenants", len(keys),
		"favorites", favoritesMode(*favoritesDir), "data", fmt.Sprintf("%T", source))
	if o.ready != nil {
		o.ready(bound)
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	select {
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}

	grace := o.shutdownGrace
	if grace <= 0 {
		grace = defaultShutdownGrace
	}
	log.Info("rentals-mcp shutting down", "grace", grace)
	shutCtx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		// A still-connected SSE client cannot be drained politely; force it closed
		// so the process (or the test) always terminates.
		log.Warn("graceful shutdown timed out; forcing close", "err", err)
		_ = srv.Close()
	}
	<-serveErr
	return nil
}

// envOr returns the environment value for key, or def when it is unset/empty.
func envOr(env func(string) string, key, def string) string {
	if v := env(key); v != "" {
		return v
	}
	return def
}

// buildTenantKeys assembles the per-tenant HS256 verification keys from both supported
// forms: derivation from fuse's tool-identity signing key for a list of tenants, and
// explicit tenant=<hex> pairs (which win on collision). Every parse failure is loud and
// names the offending value.
//
// The keying MUST match cmd/fuse's toolIdentityTenantKeys tenant for tenant, including
// its event.DefaultTenant special case (raw signing key, no derivation) — see the loop
// below. A mismatch is silent on both sides and surfaces only as an unauthorized call.
func buildTenantKeys(signingKey, tenants string, explicit tenantKeyFlag) (map[event.TenantID][]byte, error) {
	keys := map[event.TenantID][]byte{}

	var names []string
	for _, t := range strings.Split(tenants, ",") {
		if t = strings.TrimSpace(t); t != "" {
			names = append(names, t)
		}
	}
	switch {
	case signingKey != "" && len(names) == 0:
		return nil, errors.New("--signing-key was given without --tenants: name at least one tenant id to derive a key for")
	case signingKey == "" && len(names) > 0:
		return nil, errors.New("--tenants was given without --signing-key: the per-tenant keys are derived from the signing key")
	}
	for _, t := range names {
		tenant := event.NormalizeTenant(event.TenantID(t))
		if tenant == event.DefaultTenant {
			// MIRRORS cmd/fuse/tool_identity.go's toolIdentityTenantKeys, which seeds
			// event.DefaultTenant with the RAW signing key and skips derivation for it
			// (keeping the single-user shell/one-shot paths byte-identical to pre-#59).
			// fuse therefore signs every `_default` token with the signing key VERBATIM,
			// so deriving here would verify nothing — and the demo's zero-config token
			// picker entry (examples/wander/app.js DEV_USER) is exactly this tenant.
			// Duplicating the derivation function is not enough: the CALLERS must agree.
			keys[tenant] = []byte(signingKey)
			continue
		}
		keys[tenant] = deriveTenantKey([]byte(signingKey), tenant)
	}

	for _, pair := range explicit {
		tenant, hexKey, ok := strings.Cut(pair, "=")
		if !ok || strings.TrimSpace(tenant) == "" || strings.TrimSpace(hexKey) == "" {
			return nil, fmt.Errorf("--tenant-key %q is not in tenant=<hex> form", pair)
		}
		raw, err := hex.DecodeString(strings.TrimSpace(hexKey))
		if err != nil {
			return nil, fmt.Errorf("--tenant-key %q: key is not valid hex: %w", pair, err)
		}
		if len(raw) == 0 {
			return nil, fmt.Errorf("--tenant-key %q: key is empty", pair)
		}
		keys[event.NormalizeTenant(event.TenantID(strings.TrimSpace(tenant)))] = raw
	}
	return keys, nil
}

// deriveTenantKey mirrors fuse's built-in STS per-tenant key derivation
// (cmd/fuse/tool_identity.go): HMAC-SHA256(signingKey, "fuse-tool-identity-tenant:"+tenant).
// It is duplicated rather than imported because that helper lives in another `main`
// package; the two MUST stay byte-identical or no token this server sees will verify.
func deriveTenantKey(signingKey []byte, tenant event.TenantID) []byte {
	mac := hmac.New(sha256.New, signingKey)
	mac.Write([]byte("fuse-tool-identity-tenant:" + string(tenant)))
	return mac.Sum(nil)
}

// buildFavorites selects the favorites store: a durable filesystem store under dir, or
// the in-memory store when dir is empty. The empty ⇒ in-memory branch is decided HERE,
// before NewFileFavorites is called (it rejects an empty root).
func buildFavorites(dir string) (rentals.FavoritesStore, error) {
	if strings.TrimSpace(dir) == "" {
		return rentals.NewMemoryFavorites(), nil
	}
	store, err := rentals.NewFileFavorites(strings.TrimSpace(dir))
	if err != nil {
		return nil, fmt.Errorf("--favorites-dir %q: %w", dir, err)
	}
	return store, nil
}

func favoritesMode(dir string) string {
	if strings.TrimSpace(dir) == "" {
		return "memory"
	}
	return "file:" + dir
}

// resolveDataSource picks the listing backend. Credential discovery goes through
// research.Resolve (the single owner of the provider/credential rules) rather than
// re-reading TAVILY_API_KEY ad hoc.
//
//	"canned" — the hermetic in-repo listings. Never touches the network.
//	"live"   — the search-backed source; a missing credential is a LOUD failure.
//	"auto"   — live when a credential is configured, otherwise canned. This is the
//	           default, which makes canned the behaviour with no key present.
func resolveDataSource(ctx context.Context, mode string, maxResults int, env func(string) string, log *slog.Logger) (rentals.DataSource, error) {
	if log == nil {
		log = slog.Default()
	}
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "canned":
		return rentals.CannedData{}, nil
	case "live", "auto":
	default:
		return nil, fmt.Errorf("--data %q is not valid; use \"canned\", \"live\", or \"auto\"", mode)
	}

	provider, err := research.Resolve(config.ResearchConfig{MaxResults: maxResults}, env)
	if err != nil {
		if strings.EqualFold(strings.TrimSpace(mode), "live") {
			return nil, fmt.Errorf("--data live: %w", err)
		}
		log.Info("no search credential configured; serving canned listings", "err", err)
		return rentals.CannedData{}, nil
	}
	log.Info("serving live listings", "provider", provider.Name())
	return rentals.NewLiveData(rentals.LiveDataConfig{
		Searcher:   provider,
		Ctx:        ctx,
		MaxResults: maxResults,
		Logger:     log,
	}), nil
}
