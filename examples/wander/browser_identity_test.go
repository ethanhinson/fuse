//go:build browser

// Change 0060, Task 8 — the PER-PRINCIPAL ISOLATION lane.
//
// browser_test.go proves the SDK's transport promise (reconnect / no-loss / no-dup).
// THIS file proves the change's security-visible claim, end-to-end, in a real browser:
// two DIFFERENT demo principals, driven through the SAME page, see DIFFERENT favorites —
// and switching users FULLY RESETS the client, so the previous principal's observe stream
// cannot leak into the next one's session.
//
// Nothing here is faked at the boundary that matters. The lane stands up:
//
//   - the REAL rentals MCP server (cmd/rentals-mcp) on a real port, verifying per-tenant
//     delegation keys derived from the demo signing key;
//   - the REAL `fuse loop-serve-net` backend, configured from the CHECKED-IN
//     examples/wander/fuse.demo.yml (only the rentals `url:` is rewritten to the ephemeral
//     port) — so the demo tokens, tenants, audience and signing key this repo ships are the
//     ones actually exercised, and config rot fails the lane;
//   - the REAL Wander page in headless chromium, driven only through its UI.
//
// The model is a SCRIPTED LLM_GATEWAY_URL double (NEVER Claude/Anthropic; project policy):
// it turns "save listing L1" into a `mcp:rentals/favorite_listing` tool call and the app's
// own saved-panel refresh into `mcp:rentals/list_favorites`. Everything downstream of that
// tool call — the minted, audience-bound, per-principal delegation token and the rentals
// server's adjudication of WHOSE favorites set the write lands in — is real.
//
// LOUD ON TOOLCHAIN ABSENCE, like the reconnect lane: a missing toolchain is t.Fatal, never
// a green t.Skip. All teardown is t.Cleanup, client-before-server
// (httptest-defer-close-before-tcleanup-deadlock).
package wander_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/mxschmitt/playwright-go"
)

// demoUser is one entry of the checked-in demo config's `loop_server.auth` directory.
type demoUser struct {
	Token   string
	Tenant  string
	Subject string
}

// Label is how the UI's picker names the user — subject@tenant. The lane selects by this
// label, exactly as a human would.
func (u demoUser) Label() string { return u.Subject + "@" + u.Tenant }

func TestWanderBrowserUserSwitchIsolatesFavorites(t *testing.T) {
	// --- toolchain gates: LOUD, never a green skip -------------------------------
	if _, err := exec.LookPath("go"); err != nil {
		t.Fatalf("go toolchain not on PATH: the identity lane builds cmd/fuse + cmd/rentals-mcp (%v)", err)
	}
	if _, err := exec.LookPath("node"); err != nil {
		t.Fatalf("node not on PATH: the identity lane serves Wander via server.js (%v)", err)
	}

	repoRoot := repoRootFromTest(t)

	// 1) The checked-in demo config is the SOURCE OF TRUTH for every credential in this
	//    lane: the two demo users, the audience, and the tool-identity signing key.
	demoCfgPath := filepath.Join(repoRoot, "examples", "wander", "fuse.demo.yml")
	demoCfg := readFileString(t, demoCfgPath)
	users := parseDemoUsers(t, demoCfg)
	if len(users) < 2 {
		t.Fatalf("examples/wander/fuse.demo.yml must declare ≥2 demo principals for this lane; parsed %d", len(users))
	}
	alice, bob := users[0], users[1]
	if alice.Tenant == bob.Tenant && alice.Subject == bob.Subject {
		t.Fatalf("the two demo principals are identical (%v); isolation cannot be demonstrated", alice)
	}
	audience := scalarFromYAML(t, demoCfg, "audience")
	signingKey := scalarFromYAML(t, demoCfg, "signing_key")

	// 2) Build the @fuse/sdk browser bundle (esbuild). A failure here is loud.
	buildWanderBundle(t, repoRoot)

	// 3) The REAL rentals MCP server on an ephemeral port, verifying per-tenant keys
	//    derived from the demo signing key for BOTH demo tenants. Favorites stay
	//    in-memory: the lane asserts isolation, not durability (task 3 owns that).
	rentalsPort := freePort(t)
	rentalsAddr := net.JoinHostPort("127.0.0.1", rentalsPort)
	startRentalsMCP(t, repoRoot, rentalsAddr, audience, signingKey, []string{alice.Tenant, bob.Tenant})

	// 4) The demo config, verbatim except for the rentals URL (ephemeral port), as the
	//    backend's ~/.fuse/config.yml. loop_server.auth + tool_identity are home-only
	//    credential surfaces (ADR-0006), which is exactly where this lands them.
	home := t.TempDir()
	cfgPath := writeBackendConfig(t, home, demoCfg, rentalsAddr)

	// 5) Scripted gateway double (NEVER a real provider).
	gwURL := newFavoritesGateway(t)

	// 6) The REAL loop-serve-net backend on that config.
	bin := buildFuseBinary(t, repoRoot)
	backendPort := freePort(t)
	backendAddr := net.JoinHostPort("127.0.0.1", backendPort)
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, bin, "loop-serve-net", "--addr", backendAddr)
	cmd.Dir = repoRoot
	cmd.Env = append(cmd.Environ(),
		"HOME="+home, // the demo config IS the server's trusted config
		"LLM_GATEWAY_URL="+gwURL,
		"LLM_GATEWAY_KEY=test-key",
	)
	var outBuf syncBuffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &outBuf
	if err := cmd.Start(); err != nil {
		t.Fatalf("start loop-serve-net: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		_ = cmd.Process.Kill()
		waitBounded(cmd, 10*time.Second)
	})
	waitForListen(t, backendAddr, 30*time.Second, &outBuf)

	// 7) Serve the page, pointing server.js's demo-user directory at the SAME config the
	//    backend authenticates against — so the picker offers exactly the tokens that work.
	staticPort := freePort(t)
	serveWanderWithDemoConfig(t, repoRoot, staticPort, backendAddr, cfgPath)
	pageURL := fmt.Sprintf("http://127.0.0.1:%s/", staticPort)

	// 8) Headless chromium.
	if err := playwright.Install(&playwright.RunOptions{Browsers: []string{"chromium"}}); err != nil {
		t.Fatalf("playwright install chromium failed (browser lane requires chromium): %v", err)
	}
	pw, err := playwright.Run()
	if err != nil {
		t.Fatalf("playwright run failed: %v", err)
	}
	t.Cleanup(func() { _ = pw.Stop() })
	browser, err := pw.Chromium.Launch(playwright.BrowserTypeLaunchOptions{Headless: playwright.Bool(true)})
	if err != nil {
		t.Fatalf("chromium launch failed: %v", err)
	}
	t.Cleanup(func() { _ = browser.Close() })
	bctx, err := browser.NewContext()
	if err != nil {
		t.Fatalf("new browser context: %v", err)
	}
	page, err := bctx.NewPage()
	if err != nil {
		t.Fatalf("new page: %v", err)
	}
	page.OnConsole(func(m playwright.ConsoleMessage) { t.Logf("[browser console %s] %s", m.Type(), m.Text()) })
	page.OnPageError(func(err error) { t.Logf("[browser page error] %v", err) })

	if _, err := page.Goto(pageURL, playwright.PageGotoOptions{
		WaitUntil: playwright.WaitUntilStateNetworkidle,
		Timeout:   playwright.Float(30000),
	}); err != nil {
		t.Fatalf("goto %s: %v\n--- server output ---\n%s", pageURL, err, outBuf.String())
	}
	defer func() {
		if t.Failed() {
			t.Logf("--- backend loop-serve-net output ---\n%s", outBuf.String())
		}
	}()

	// --- principal A: pick alice, favorite L1, see it in the saved panel ---------
	selectDemoUser(t, page, alice)
	waitForWhoami(t, page, alice.Label())
	// Switching principals refreshes the saved panel through the agent (list_favorites);
	// alice starts with nothing saved.
	waitForSavedSettled(t, page, &outBuf)

	sendConciergeMessage(t, page, "save listing L1")
	waitForSavedContains(t, page, "L1", &outBuf)

	// --- switch to principal B: FULL client-side reset ---------------------------
	selectDemoUser(t, page, bob)
	waitForWhoami(t, page, bob.Label())

	// Wait for bob's own session to be live and delivering (his refresh turn completed).
	// Doing this BEFORE the stream count is what makes the count meaningful: one live
	// stream now can only be bob's, so it also proves alice's is gone.
	waitForSavedSettled(t, page, &outBuf)

	// The reset is not cosmetic: the previous principal's observe stream must be torn
	// down. Exactly ONE live observe stream may remain — if the old one leaked, this
	// settles at 2 and the lane fails.
	waitForOpenStreams(t, page, 1, &outBuf)

	// The transcript is cleared by the reset: no carried-over user message.
	if n := readInt(t, page, "document.querySelectorAll('#thread .msg.user').length"); n != 0 {
		t.Fatalf("switching users left %d transcript message(s) from the previous principal; the reset must clear it", n)
	}

	// --- THE CLAIM: bob's saved panel must NOT contain alice's listing -----------
	if got := savedPanelText(t, page); strings.Contains(got, "L1") {
		t.Fatalf("PER-PRINCIPAL ISOLATION VIOLATED: %s's saved panel shows %s's listing L1 (panel=%q)",
			bob.Label(), alice.Label(), got)
	}

	// And positively: bob's own favorite lands in bob's set, still without alice's.
	sendConciergeMessage(t, page, "save listing L2")
	waitForSavedContains(t, page, "L2", &outBuf)
	if got := savedPanelText(t, page); strings.Contains(got, "L1") {
		t.Fatalf("PER-PRINCIPAL ISOLATION VIOLATED: %s's saved panel shows %s's listing L1 after saving L2 (panel=%q)",
			bob.Label(), alice.Label(), got)
	}

	// Switching back to alice must show HER listing again (L1) and not bob's (L2) —
	// proving the panel reflects the calling principal, not a client-side cache.
	selectDemoUser(t, page, alice)
	waitForWhoami(t, page, alice.Label())
	waitForSavedContains(t, page, "L1", &outBuf)
	if got := savedPanelText(t, page); strings.Contains(got, "L2") {
		t.Fatalf("PER-PRINCIPAL ISOLATION VIOLATED: %s's saved panel shows %s's listing L2 (panel=%q)",
			alice.Label(), bob.Label(), got)
	}

	// No terminal error may have fired: every switch was an authorized principal.
	if term := readString(t, page, "window.__wanderTerminal"); term != "" && term != "<nil>" {
		t.Fatalf("a user switch surfaced a terminal stream error (%q); both demo tokens are valid\n--- server output ---\n%s",
			term, outBuf.String())
	}
}

// --- UI drivers -----------------------------------------------------------------

// selectDemoUser picks a principal from the page's token picker by its subject@tenant
// label — the same gesture a human makes.
func selectDemoUser(t *testing.T, page playwright.Page, u demoUser) {
	t.Helper()
	label := u.Label()
	if _, err := page.SelectOption("#user", playwright.SelectOptionValues{
		Labels: playwright.StringSlice(label),
	}); err != nil {
		opts := readString(t, page,
			"JSON.stringify(Array.from(document.querySelectorAll('#user option')).map(o=>o.textContent))")
		t.Fatalf("select demo user %q in #user: %v (options=%s)", label, err, opts)
	}
}

func waitForWhoami(t *testing.T, page playwright.Page, want string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var got string
	for time.Now().Before(deadline) {
		got = strings.TrimSpace(readString(t, page, "document.getElementById('whoami').textContent"))
		if got == want {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("identity label never became %q (last %q)", want, got)
}

func savedPanelText(t *testing.T, page playwright.Page) string {
	t.Helper()
	return readString(t, page, "document.getElementById('saved').textContent")
}

// savedListingIDs reads the listing ids the saved panel is currently showing.
func savedListingIDs(t *testing.T, page playwright.Page) []string {
	t.Helper()
	return readStringArray(t, page,
		"Array.from(document.querySelectorAll('#saved .saved-item')).map(e=>e.dataset.listing)")
}

// waitForSavedContains waits until the saved panel shows a listing id.
func waitForSavedContains(t *testing.T, page playwright.Page, id string, out *syncBuffer) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		for _, got := range savedListingIDs(t, page) {
			if got == id {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("saved panel never showed listing %q; panel=%q errors=%s\n--- server output ---\n%s",
		id, savedPanelText(t, page),
		readString(t, page, "JSON.stringify(Array.from(document.querySelectorAll('.msg.error')).map(e=>e.textContent))"),
		out.String())
}

// waitForSavedSettled waits until the app has finished the refresh turn it fires on a
// user switch — i.e. the saved panel reflects a completed list_favorites for the CURRENT
// principal (the app records the refresh generation it last rendered).
func waitForSavedSettled(t *testing.T, page playwright.Page, out *syncBuffer) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if readInt(t, page, "window.__wanderSavedRefreshes") >= 1 {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("the saved panel never refreshed for the current principal; panel=%q errors=%s\n--- server output ---\n%s",
		savedPanelText(t, page),
		readString(t, page, "JSON.stringify(Array.from(document.querySelectorAll('.msg.error')).map(e=>e.textContent))"),
		out.String())
}

// waitForOpenStreams waits until exactly `want` observe streams are live in the page.
// This is the anti-leak assertion: after a user switch the OLD principal's stream must
// have been torn down through the SDK's idempotent abort, leaving one.
func waitForOpenStreams(t *testing.T, page playwright.Page, want int, out *syncBuffer) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var got int
	for time.Now().Before(deadline) {
		got = readInt(t, page, "window.__wanderOpenStreams")
		if got == want {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("expected exactly %d live observe stream(s) after the user switch, saw %d — the previous principal's stream leaked\n--- server output ---\n%s",
		want, got, out.String())
}

// --- backend / config fixtures --------------------------------------------------

var rentalsURLRe = regexp.MustCompile(`(?m)^(\s*url:\s*)http://[^\s]+$`)

// writeBackendConfig writes the CHECKED-IN demo config to <home>/.fuse/config.yml with
// only the rentals `url:` rewritten to the lane's ephemeral address. Everything the claim
// depends on — the demo tokens, tenants, audience and signing key — is used verbatim, so
// a drifted demo config fails this lane instead of silently demoing something else.
func writeBackendConfig(t *testing.T, home, demoCfg, rentalsAddr string) string {
	t.Helper()
	if n := len(rentalsURLRe.FindAllString(demoCfg, -1)); n != 1 {
		t.Fatalf("expected exactly one rewritable `url:` line in examples/wander/fuse.demo.yml, found %d", n)
	}
	// BASE url only: the MCP HTTP transport appends /sse and /messages itself.
	out := rentalsURLRe.ReplaceAllString(demoCfg, "${1}http://"+rentalsAddr)
	dir := filepath.Join(home, ".fuse")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	path := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(path, []byte(out), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// startRentalsMCP builds and runs cmd/rentals-mcp on addr, verifying delegation tokens
// for the given tenants under keys derived from signingKey.
func startRentalsMCP(t *testing.T, repoRoot, addr, audience, signingKey string, tenants []string) {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "rentals-mcp")
	build := exec.Command("go", "build", "-o", bin, "./cmd/rentals-mcp")
	build.Dir = repoRoot
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build ./cmd/rentals-mcp: %v\n%s", err, out)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, bin,
		"--addr", addr,
		"--audience", audience,
		"--signing-key", signingKey,
		"--tenants", strings.Join(tenants, ","),
		"--data", "canned", // hermetic: no network, no TAVILY_API_KEY
	)
	cmd.Dir = repoRoot
	var out syncBuffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start rentals-mcp: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		_ = cmd.Process.Kill()
		waitBounded(cmd, 10*time.Second)
	})
	waitForListen(t, addr, 20*time.Second, &out)
}

// serveWanderWithDemoConfig starts server.js with FUSE_DEMO_CONFIG pointed at the config
// the backend authenticates against, so the page's picker offers those exact principals.
func serveWanderWithDemoConfig(t *testing.T, repoRoot, staticPort, backendAddr, demoConfig string) {
	t.Helper()
	serverJS := filepath.Join(repoRoot, "examples", "wander", "server.js")
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "node", serverJS)
	cmd.Dir = filepath.Join(repoRoot, "examples", "wander")
	cmd.Env = append(cmd.Environ(),
		"PORT="+staticPort,
		"FUSE_NET_ADDR="+backendAddr,
		"FUSE_DEMO_CONFIG="+demoConfig,
	)
	var out syncBuffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start wander server.js: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		_ = cmd.Process.Kill()
		waitBounded(cmd, 10*time.Second)
	})
	waitForListen(t, net.JoinHostPort("127.0.0.1", staticPort), 15*time.Second, &out)
}

// --- the scripted gateway double ------------------------------------------------

var saveListingRe = regexp.MustCompile(`(?i)save listing ([A-Za-z0-9_-]+)`)

// newFavoritesGateway scripts each turn from the LAST message in the conversation:
//
//   - last message is a tool result  → finish the turn with a plain assistant reply;
//   - last user message asks for saved stays → call mcp:rentals/list_favorites;
//   - last user message says "save listing X"  → call mcp:rentals/favorite_listing{X};
//   - anything else → a plain reply.
//
// Keying on the LAST message (not "does the body contain a tool role") is what makes it
// work for a PERSISTENT interactive loop, whose later turns still carry earlier turns'
// tool messages. Tool-call arguments are passed as PLAIN JSON — pre-escaping them
// double-escapes and hangs the loop with no failing assertion
// (scripted-gateway-double-double-escapes-tool-args).
func newFavoritesGateway(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("gateway listen: %v", err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")

		var req struct {
			Messages []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(body, &req)

		reply := func(text string) {
			resp := map[string]any{"choices": []map[string]any{{
				"message": map[string]any{"role": "assistant", "content": text},
			}}}
			b, _ := json.Marshal(resp)
			_, _ = w.Write(b)
		}
		call := func(name string, args map[string]any) {
			argJSON, _ := json.Marshal(args)
			resp := map[string]any{"choices": []map[string]any{{"message": map[string]any{
				"role":    "assistant",
				"content": "",
				"tool_calls": []map[string]any{{
					"id": "1", "type": "function",
					"function": map[string]any{"name": name, "arguments": string(argJSON)},
				}},
			}}}}
			b, _ := json.Marshal(resp)
			_, _ = w.Write(b)
		}

		if len(req.Messages) == 0 {
			reply("Hi! Where would you like to go?")
			return
		}
		last := req.Messages[len(req.Messages)-1]
		if last.Role == "tool" {
			reply("Done — your saved stays are up to date.")
			return
		}
		text := string(last.Content)
		if strings.Contains(strings.ToLower(text), "saved stays") {
			call("mcp:rentals/list_favorites", map[string]any{})
			return
		}
		if m := saveListingRe.FindStringSubmatch(text); m != nil {
			call("mcp:rentals/favorite_listing", map[string]any{"listing_id": m[1]})
			return
		}
		reply("Here are a few options that might fit.")
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return "http://" + ln.Addr().String()
}

// --- demo-config parsing --------------------------------------------------------

var (
	commentLineRe = regexp.MustCompile(`(?m)^\s*#.*$`)
	authEntryRe   = regexp.MustCompile(`(?m)^\s*-\s*token:\s*(\S+)\s*$\n\s*tenant:\s*(\S+)\s*$\n\s*subject:\s*(\S+)\s*$`)
)

// parseDemoUsers reads the `loop_server.auth` directory out of the demo config. It is a
// deliberately literal reader: if the checked-in config stops declaring token/tenant/
// subject triples, this lane fails loudly rather than testing a fiction.
func parseDemoUsers(t *testing.T, yml string) []demoUser {
	t.Helper()
	stripped := commentLineRe.ReplaceAllString(yml, "")
	var users []demoUser
	for _, m := range authEntryRe.FindAllStringSubmatch(stripped, -1) {
		users = append(users, demoUser{Token: m[1], Tenant: m[2], Subject: m[3]})
	}
	return users
}

// scalarFromYAML pulls a single `key: value` scalar out of the demo config (comments
// stripped first). It fatals unless exactly one occurrence exists, so an ambiguous config
// can never silently select the wrong credential.
func scalarFromYAML(t *testing.T, yml, key string) string {
	t.Helper()
	re := regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(key) + `:\s*(\S+)\s*$`)
	ms := re.FindAllStringSubmatch(commentLineRe.ReplaceAllString(yml, ""), -1)
	if len(ms) != 1 {
		t.Fatalf("expected exactly one %q entry in examples/wander/fuse.demo.yml, found %d", key, len(ms))
	}
	return ms[0][1]
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}
