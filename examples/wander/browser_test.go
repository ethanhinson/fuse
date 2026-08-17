//go:build browser

// Package wander_test — the PERMANENT headless-browser reconnect lane for @fuse/sdk
// (change 0056, Task 5, D4). It converts the deferred manual real-browser proof (recorded
// as a checkbox in sdk/ts/README.md "Verify (human)" at #50/#55) into an enforced CI lane.
//
// It drives the REAL Wander page (examples/wander) — which imports the REAL @fuse/sdk over
// @connectrpc/connect-web — in headless chromium against a REAL `fuse loop-serve-net`
// backend with a SCRIPTED LLM_GATEWAY_URL double (NEVER Claude/Anthropic; project policy),
// then KILLS THE NETWORK mid-session (server.js's /__cut control destroys the open proxied
// stream socket) and asserts the concierge reply still completes after a transparent
// reconnect with NO LOSS / NO DUP.
//
// Run:  go test -tags browser ./...   (or `make browser-test`)
//
// LOUD ON TOOLCHAIN ABSENCE (smoke-over-fake-backend-proves-wire-not-system): missing node,
// esbuild, the go toolchain, or a chromium that playwright cannot install/launch is a hard
// t.Fatal, NEVER a green t.Skip — a passing suite can never hide an unexercised browser
// path. All teardown uses t.Cleanup (not defer srv.Close), ordered client-before-server
// (httptest-defer-close-before-tcleanup-deadlock).
//
// Network-kill approach (documented descope): Playwright's BrowserContext.SetOffline did not
// reliably sever an already-established, idle (parked) streaming socket, so the lane takes
// the plan's deterministic fallback — it lets turn 1 park (the observe stream is then
// live+open), then hits server.js's /__cut control, which forcibly DESTROYS every in-flight
// proxied Connect socket (a real mid-stream network kill of the open Observe stream, driving
// the SDK into `reconnecting`). The proxy is healthy again on the next request, so the SDK
// transparently re-observes from the watermark; turn 2 is then driven to completion. It
// asserts the SDK saw reconnecting→live again AND that the whole-session domain-event seq log
// is CONTIGUOUS (+1 each) through both parks — the exact no-loss/no-dup property, over a real
// browser, over a real drop.
package wander_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mxschmitt/playwright-go"
)

func TestWanderBrowserReconnectNoLossNoDup(t *testing.T) {
	// --- toolchain gates: LOUD, never a green skip -------------------------------
	if _, err := exec.LookPath("go"); err != nil {
		t.Fatalf("go toolchain not on PATH: the browser lane builds cmd/fuse (%v)", err)
	}
	if _, err := exec.LookPath("node"); err != nil {
		t.Fatalf("node not on PATH: the browser lane serves Wander via server.js (%v)", err)
	}

	repoRoot := repoRootFromTest(t)

	// 1) Build the @fuse/sdk browser bundle (esbuild). A failure here is loud.
	buildWanderBundle(t, repoRoot)

	// 2) Scripted gateway double (NEVER a real provider): a fixed non-streamed reply so each
	//    concierge turn is deterministic and model-free.
	gwURL := newScriptedGateway(t)

	// 3) Build + start the REAL fuse loop-serve-net backend on a fixed ephemeral port.
	bin := buildFuseBinary(t, repoRoot)
	backendPort := freePort(t)
	backendAddr := net.JoinHostPort("127.0.0.1", backendPort)

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, bin, "loop-serve-net", "--addr", backendAddr)
	cmd.Dir = repoRoot
	cmd.Env = append(cmd.Environ(),
		"HOME="+t.TempDir(), // no user config → built-in dev token (tenant _default)
		"LLM_GATEWAY_URL="+gwURL,
		"LLM_GATEWAY_KEY=test-key",
	)
	var outBuf syncBuffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &outBuf
	if err := cmd.Start(); err != nil {
		t.Fatalf("start loop-serve-net: %v", err)
	}
	// Teardown ordering: the browser + static server stop first (registered later, so they
	// run first — Cleanup is LIFO), then the backend. Never defer srv.Close.
	t.Cleanup(func() {
		cancel()
		_ = cmd.Process.Kill()
		waitBounded(cmd, 10*time.Second)
	})
	waitForListen(t, backendAddr, 30*time.Second, &outBuf)

	// 4) Serve the Wander page via server.js, reverse-proxying the Connect path to the
	//    backend (same-origin, no CORS). node is the static+proxy server.
	staticPort := freePort(t)
	serveWander(t, repoRoot, staticPort, backendAddr)
	pageURL := fmt.Sprintf("http://127.0.0.1:%s/", staticPort)

	// 5) Headless chromium via playwright-go. Install + launch are LOUD on failure.
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

	// Surface page console messages + page errors into the test log for diagnosis.
	page.OnConsole(func(m playwright.ConsoleMessage) {
		t.Logf("[browser console %s] %s", m.Type(), m.Text())
	})
	page.OnPageError(func(err error) {
		t.Logf("[browser page error] %v", err)
	})

	if _, err := page.Goto(pageURL, playwright.PageGotoOptions{
		WaitUntil: playwright.WaitUntilStateNetworkidle,
		Timeout:   playwright.Float(30000),
	}); err != nil {
		t.Fatalf("goto %s: %v\n--- server output ---\n%s", pageURL, err, outBuf.String())
	}

	// --- turn 1: send a message, wait for the loop to park -----------------------
	sendConciergeMessage(t, page, "beachfront, sleeps 6, under $300/night")
	defer func() {
		if t.Failed() {
			t.Logf("--- backend loop-serve-net output ---\n%s", outBuf.String())
		}
	}()
	waitForParked(t, page, 1) // at least one loop.parked (kind seq) rendered

	// The observe stream is now live + open (parked). Confirm the SDK reached `live`.
	waitForState(t, page, "live")

	// --- kill the network mid-session (a REAL drop of the open observe stream) ----
	// server.js's /__cut control forcibly destroys every in-flight proxied Connect socket,
	// severing the OPEN Observe stream — a deterministic mid-stream network kill. The SDK
	// must classify this severed stream as TRANSIENT and enter `reconnecting` (not throw,
	// not hot-loop); no-loss/no-dup depends on it resuming from the watermark.
	cutStreams(t, staticPort)
	waitForState(t, page, "reconnecting")

	// The reverse proxy is healthy again immediately, so the SDK's next Observe succeeds and
	// transparently re-observes from the watermark (the server replays history since the
	// cursor — nothing new — and the SDK dedups any overlap).

	// --- turn 2: prove the session still works after the reconnect ---------------
	sendConciergeMessage(t, page, "actually, make it pet-friendly")
	waitForParked(t, page, 2) // a SECOND loop.parked after the reconnect

	// --- assertions: no-loss / no-dup over the whole session + reconnect states ---
	seqs := readNumberArray(t, page, "window.__wanderSeqs")
	if len(seqs) == 0 {
		t.Fatalf("no events observed in the browser\n--- server output ---\n%s", outBuf.String())
	}
	assertContiguousNoLossNoDup(t, seqs) // contiguous (+1 each) ⇒ no dup AND no loss.

	states := readStringArray(t, page, "window.__wanderStates")
	assertReconnectedThenLive(t, states)

	// A terminal error must NOT have fired — this was a transient drop.
	if term := readString(t, page, "window.__wanderTerminal"); term != "" && term != "<nil>" {
		t.Fatalf("a transient offline toggle surfaced a terminal error (%q); it must reconnect", term)
	}
}

// TestWanderBrowserReloadRestoresSession is the change-0062 (D4) RELOAD-RESTORE lane: it
// proves the browser face of #54 — the page persists its loopId, and reopening the tab
// replays the durable event stream instead of minting a brand-new loop.
//
// It reuses the reconnect lane's harness verbatim (one real `fuse loop-serve-net`, one
// `node server.js`, the SCRIPTED gateway double — NEVER Claude/Anthropic; project policy)
// and leaves that lane's /__cut assertions untouched.
//
// *** DO NOT "SIMPLIFY" STEP 3 INTO browser.NewContext(). *** Playwright BrowserContexts
// are STORAGE-ISOLATED: a fresh context starts with an EMPTY localStorage, so the page
// would find no stored session, take the FRESH-SESSION path, and this test would go green
// while asserting nothing about restore — a false green. The faithful "I closed the tab and
// reopened it" gesture is page.Close() followed by a NEW Page in the SAME BrowserContext,
// which is the only shape that carries the persisted `wander.session.v1` entry across.
// (Copying StorageState() into a second context also works; it is strictly more machinery
// for the same assertion.)
func TestWanderBrowserReloadRestoresSession(t *testing.T) {
	pageURL, bctx, backendOut := startWanderBrowserStack(t)

	page1, err := bctx.NewPage()
	if err != nil {
		t.Fatalf("new page: %v", err)
	}
	instrumentPage(t, page1)
	if _, err := page1.Goto(pageURL, playwright.PageGotoOptions{
		WaitUntil: playwright.WaitUntilStateNetworkidle,
		Timeout:   playwright.Float(30000),
	}); err != nil {
		t.Fatalf("goto %s: %v\n--- server output ---\n%s", pageURL, err, backendOut.String())
	}
	defer func() {
		if t.Failed() {
			t.Logf("--- backend loop-serve-net output ---\n%s", backendOut.String())
		}
	}()

	// index.html ships a STATIC concierge greeting bubble, so every concierge count below is
	// measured against this baseline rather than against zero. It is the same static markup
	// on the reopened tab, which is why one reading serves both.
	greeting := readInt(t, page1, "document.querySelectorAll('.msg.concierge').length")

	// --- 1) turn 1 in the first tab, driven to a park -----------------------------
	const turn1 = "beachfront, sleeps 6, under $300/night"
	sendConciergeMessage(t, page1, turn1)
	// TWO parks: turn 1 itself, then the app's own QUIET Saved-panel refresh turn that the
	// favorite_listing call armed. Both land in the durable stream, so the fixture the
	// reopened tab replays contains a quiet turn — the case a replay can render wrong.
	waitForParked(t, page1, 2)
	if n := readInt(t, page1, "document.querySelectorAll('.msg.concierge').length"); n != greeting+1 {
		t.Fatalf("the LIVE tab rendered %d concierge bubble(s) after one visible turn (want %d); the quiet refresh turn must not be rendered at all; thread was %s",
			n, greeting+1, readString(t, page1, "JSON.stringify(Array.from(document.querySelectorAll('.msg')).map(e=>e.className+': '+e.textContent))"))
	}
	assertNoTerminal(t, page1, "after turn 1 in the first tab")

	// --- 2) the minted loop id must have been persisted ---------------------------
	loopID := readString(t, page1, "window.__wanderLoopId")
	if loopID == "" || loopID == "<nil>" {
		t.Fatalf("no window.__wanderLoopId after turn 1; the restore lane cannot assert anything without it")
	}

	// --- 3) close the TAB, reopen a tab in the SAME context (see the note above) ---
	if err := page1.Close(); err != nil {
		t.Fatalf("close page1: %v", err)
	}
	page2, err := bctx.NewPage() // SAME BrowserContext ⇒ localStorage survives.
	if err != nil {
		t.Fatalf("new page in the same context: %v", err)
	}
	instrumentPage(t, page2)

	// --- 4) the reopened tab restores the SAME loop -------------------------------
	// NOT `networkidle`: the restore opens its durable Observe stream during load and holds
	// it open, so the network never goes idle and the navigation would time out. `load` is
	// the honest wait here — every restore assertion below polls for its own condition.
	if _, err := page2.Goto(pageURL, playwright.PageGotoOptions{
		WaitUntil: playwright.WaitUntilStateLoad,
		Timeout:   playwright.Float(30000),
	}); err != nil {
		t.Fatalf("goto %s (reopened tab): %v\n--- server output ---\n%s", pageURL, err, backendOut.String())
	}
	waitForTrue(t, page2, "window.__wanderRestored === true",
		"the reopened tab did not restore a stored session (it took the fresh-session path)")
	if got := readString(t, page2, "window.__wanderLoopId"); got != loopID {
		t.Fatalf("restored loop id mismatch: reopened tab has %q, first tab minted %q", got, loopID)
	}

	// --- 5) the replayed transcript carries BOTH the question and the answer ------
	// The user-text assertion is the one that proves the `user.input` replay rendering:
	// without it a transcript of answers with no questions would pass. It must not be
	// dropped or weakened to a concierge-only check.
	waitForThreadText(t, page2, ".msg.user", turn1)
	waitForThreadText(t, page2, ".msg.concierge", "beachfront options")
	assertNoTerminal(t, page2, "after the restore")

	// --- 5b) …and NOT the app's quiet turn, on either side of it ------------------
	// The quiet Saved-panel refresh is a real turn on the real loop, so its user.input AND
	// its answer are both in the replayed stream. Suppressing only the question leaves an
	// orphan concierge bubble attached to nothing — the mirror image of the defect this
	// change exists to fix. Wait for BOTH replayed parks first, so the quiet turn's answer
	// has definitely been observed before we assert it was not rendered.
	waitForParked(t, page2, 2)
	if readBool(t, page2, fmt.Sprintf(
		"Array.from(document.querySelectorAll('.msg.concierge')).some(e => e.textContent.includes(%q))",
		savedStaysMarker)) {
		t.Fatalf("ORPHAN BUBBLE: the replayed quiet Saved-panel turn rendered its answer (%q) with no question above it; thread was %s",
			savedStaysMarker,
			readString(t, page2, "JSON.stringify(Array.from(document.querySelectorAll('.msg')).map(e=>e.className+': '+e.textContent))"))
	}
	// A replayed `send` arrives wrapped in the runtime's "[human message]" injection
	// envelope. That framing must never reach the transcript — and the quiet turn's
	// suppression is an exact-match comparison that the envelope would otherwise defeat.
	if readBool(t, page2, `Array.from(document.querySelectorAll('.msg')).some(e => e.textContent.includes("[human message]"))`) {
		t.Fatalf("the restored transcript leaked the runtime's injection envelope; thread was %s",
			readString(t, page2, "JSON.stringify(Array.from(document.querySelectorAll('.msg')).map(e=>e.className+': '+e.textContent))"))
	}
	if u, c := readInt(t, page2, "document.querySelectorAll('.msg.user').length"),
		readInt(t, page2, "document.querySelectorAll('.msg.concierge').length"); u != 1 || c != greeting+1 {
		t.Fatalf("restored transcript is unbalanced: %d user bubble(s), %d concierge bubble(s), want 1 and %d; thread was %s",
			u, c, greeting+1, readString(t, page2, "JSON.stringify(Array.from(document.querySelectorAll('.msg')).map(e=>e.className+': '+e.textContent))"))
	}

	// --- 6) the conversation genuinely continues on the restored loop -------------
	// The restore replayed TWO parks (turn 1 + the quiet refresh), so turn 2's park is the
	// THIRD one this page saw.
	sendConciergeMessage(t, page2, "actually, make it pet-friendly")
	waitForParked(t, page2, 3)
	waitForThreadText(t, page2, ".msg.user", "pet-friendly")
	if got := readString(t, page2, "window.__wanderLoopId"); got != loopID {
		t.Fatalf("turn 2 ran on a different loop: %q, want the restored %q", got, loopID)
	}
	if n := readInt(t, page2, "document.querySelectorAll('.msg.concierge').length"); n < 2 {
		t.Fatalf("expected the restored answer plus turn 2's reply (>=2 concierge bubbles), got %d", n)
	}

	// --- 7) a restore is not a terminal condition ---------------------------------
	assertNoTerminal(t, page2, "after turn 2 on the restored loop")
	if readBool(t, page2, "!!window.__wanderRestoreLost") {
		t.Fatalf("__wanderRestoreLost fired: the durable stream was reported gone on a live restore")
	}
	if readBool(t, page2, "!!window.__wanderPaused") {
		t.Fatalf("__wanderPaused fired: the restored loop was reported paused while it was still usable")
	}
}

// TestWanderBrowserUnknownStoredPrincipalStartsFresh pins the degrade path of the
// principal-aware restore (change 0062): the page re-selects the stored principal from the
// demo directory before restoring, so a stored entry naming a principal that is NOT in that
// directory (removed from the demo config, a failed directory fetch, or a pasted credential
// that was deliberately never persisted) has no credential to restore under.
//
// That case must land on a CLEAN FRESH SESSION — the stored entry forgotten, no terminal
// error, the composer usable — and must never fall through into an Observe issued under
// whatever credential happens to be in hand, which is precisely the cross-owner call the
// server answers with PermissionDenied.
func TestWanderBrowserUnknownStoredPrincipalStartsFresh(t *testing.T) {
	pageURL, bctx, backendOut := startWanderBrowserStack(t)

	page1, err := bctx.NewPage()
	if err != nil {
		t.Fatalf("new page: %v", err)
	}
	instrumentPage(t, page1)
	if _, err := page1.Goto(pageURL, playwright.PageGotoOptions{
		WaitUntil: playwright.WaitUntilStateNetworkidle,
		Timeout:   playwright.Float(30000),
	}); err != nil {
		t.Fatalf("goto %s: %v\n--- server output ---\n%s", pageURL, err, backendOut.String())
	}
	defer func() {
		if t.Failed() {
			t.Logf("--- backend loop-serve-net output ---\n%s", backendOut.String())
		}
	}()

	// Plant a stored session owned by a principal this deployment has never heard of. Writing
	// the entry directly is the point: it is the shape left behind by a demo config that
	// dropped a user, and no UI gesture can produce it on THIS page.
	const ghostLoop = "ghost-loop-that-must-never-be-observed"
	if _, err := page1.Evaluate(fmt.Sprintf(
		`localStorage.setItem("wander.session.v1", JSON.stringify({loopId:%q,tenant:"ghost-tenant",subject:"ghost-subject"}))`,
		ghostLoop)); err != nil {
		t.Fatalf("plant stored session: %v", err)
	}

	if err := page1.Close(); err != nil {
		t.Fatalf("close page1: %v", err)
	}
	page2, err := bctx.NewPage() // SAME BrowserContext ⇒ the planted entry survives.
	if err != nil {
		t.Fatalf("new page in the same context: %v", err)
	}
	instrumentPage(t, page2)
	if _, err := page2.Goto(pageURL, playwright.PageGotoOptions{
		WaitUntil: playwright.WaitUntilStateLoad,
		Timeout:   playwright.Float(30000),
	}); err != nil {
		t.Fatalf("goto %s (reopened tab): %v\n--- server output ---\n%s", pageURL, err, backendOut.String())
	}

	// The entry being dropped is the positive signal that the lookup ran and failed to find
	// an owner — so waiting on it also settles the async directory fetch deterministically.
	waitForTrue(t, page2, `localStorage.getItem("wander.session.v1") === null`,
		"an unrestorable stored session was left in localStorage; it must be forgotten, not kept to fail again on every load")
	if readBool(t, page2, "window.__wanderRestored === true") {
		t.Fatalf("the page claimed a restore for a principal that is not in the demo directory")
	}
	if got := readString(t, page2, "window.__wanderLoopId"); got == ghostLoop {
		t.Fatalf("the page adopted the stored loop %q despite having no credential for its owner", got)
	}
	assertNoTerminal(t, page2, "after declining to restore an unknown principal's session")

	// …and the fresh session is a WORKING one, not a broken page.
	sendConciergeMessage(t, page2, "beachfront, sleeps 6, under $300/night")
	waitForParked(t, page2, 1)
	waitForThreadText(t, page2, ".msg.concierge", "beachfront options")
	if got := readString(t, page2, "window.__wanderLoopId"); got == "" || got == "<nil>" || got == ghostLoop {
		t.Fatalf("the fresh session did not mint its own loop (window.__wanderLoopId=%q)", got)
	}
}

// --- browser helpers ------------------------------------------------------------

// startWanderBrowserStack stands up the whole lane's harness — scripted gateway double,
// real `fuse loop-serve-net`, `node server.js`, headless chromium — and returns the page
// URL, a BrowserContext to open pages in, and the backend's captured output. Toolchain
// absence is LOUD (t.Fatal), never a green skip; all teardown is t.Cleanup, LIFO-ordered
// browser-before-servers.
func startWanderBrowserStack(t *testing.T) (string, playwright.BrowserContext, *syncBuffer) {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Fatalf("go toolchain not on PATH: the browser lane builds cmd/fuse (%v)", err)
	}
	if _, err := exec.LookPath("node"); err != nil {
		t.Fatalf("node not on PATH: the browser lane serves Wander via server.js (%v)", err)
	}

	repoRoot := repoRootFromTest(t)
	buildWanderBundle(t, repoRoot)
	// The restore lane's fixture MUST contain a favorited listing: favoriting is what arms
	// the app's quiet Saved-panel refresh turn, and a quiet turn is the case a replay can
	// get wrong (see newSavedRefreshGateway).
	gwURL := newSavedRefreshGateway(t)
	bin := buildFuseBinary(t, repoRoot)
	backendPort := freePort(t)
	backendAddr := net.JoinHostPort("127.0.0.1", backendPort)

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, bin, "loop-serve-net", "--addr", backendAddr)
	cmd.Dir = repoRoot
	cmd.Env = append(cmd.Environ(),
		"HOME="+t.TempDir(), // no user config → built-in dev token (tenant _default)
		"LLM_GATEWAY_URL="+gwURL,
		"LLM_GATEWAY_KEY=test-key",
	)
	out := &syncBuffer{}
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start loop-serve-net: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		_ = cmd.Process.Kill()
		waitBounded(cmd, 10*time.Second)
	})
	waitForListen(t, backendAddr, 30*time.Second, out)

	staticPort := freePort(t)
	serveWander(t, repoRoot, staticPort, backendAddr)
	pageURL := fmt.Sprintf("http://127.0.0.1:%s/", staticPort)

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
	return pageURL, bctx, out
}

// instrumentPage surfaces a page's console messages and uncaught errors into the test log.
func instrumentPage(t *testing.T, page playwright.Page) {
	t.Helper()
	page.OnConsole(func(m playwright.ConsoleMessage) {
		t.Logf("[browser console %s] %s", m.Type(), m.Text())
	})
	page.OnPageError(func(err error) {
		t.Logf("[browser page error] %v", err)
	})
}

// assertNoTerminal fails if the page recorded a terminal stream error. A restore (like a
// transient drop) must never surface one.
func assertNoTerminal(t *testing.T, page playwright.Page, when string) {
	t.Helper()
	if term := readString(t, page, "window.__wanderTerminal"); term != "" && term != "<nil>" {
		t.Fatalf("terminal error %q surfaced %s; a restore is not a terminal condition", term, when)
	}
}

// waitForTrue polls a boolean page expression until it holds, or fails with `msg`.
func waitForTrue(t *testing.T, page playwright.Page, expr, msg string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if readBool(t, page, expr) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("%s (timed out waiting for %s; __wanderTerminal=%s errors=%s)", msg, expr,
		readString(t, page, "window.__wanderTerminal"),
		readString(t, page, "JSON.stringify(Array.from(document.querySelectorAll('.msg.error')).map(e=>e.textContent))"))
}

// waitForThreadText polls until some message bubble matching `sel` contains `want`.
func waitForThreadText(t *testing.T, page playwright.Page, sel, want string) {
	t.Helper()
	expr := fmt.Sprintf("Array.from(document.querySelectorAll(%q)).some(e => e.textContent.includes(%q))",
		sel, want)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if readBool(t, page, expr) {
			return
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatalf("no %s bubble containing %q; thread was %s", sel, want,
		readString(t, page, "JSON.stringify(Array.from(document.querySelectorAll('.msg')).map(e=>e.className+': '+e.textContent))"))
}

func readBool(t *testing.T, page playwright.Page, expr string) bool {
	t.Helper()
	v, err := page.Evaluate(expr)
	if err != nil {
		t.Fatalf("evaluate %q: %v", expr, err)
	}
	b, _ := v.(bool)
	return b
}

// cutStreams hits the Wander static server's /__cut control to forcibly drop every
// in-flight proxied Connect stream — the deterministic mid-stream network kill.
func cutStreams(t *testing.T, staticPort string) {
	t.Helper()
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%s/__cut", staticPort))
	if err != nil {
		t.Fatalf("cut streams: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	t.Logf("[__cut] %s", string(body))
}

func sendConciergeMessage(t *testing.T, page playwright.Page, msg string) {
	t.Helper()
	if err := page.Fill("#input", msg); err != nil {
		t.Fatalf("fill input: %v", err)
	}
	if err := page.Click("#send"); err != nil {
		t.Fatalf("click send: %v", err)
	}
}

// waitForParked polls until at least `n` loop.parked completions have been rendered. Wander
// pushes every observed seq to __wanderSeqs and the terminal answer to a .concierge bubble;
// we count parked completions by watching the seq log grow past each turn's terminal event.
// Simpler + robust: count how many times the composer re-enabled (parked ⇒ input enabled).
func waitForParked(t *testing.T, page playwright.Page, n int) {
	t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		if readInt(t, page, "window.__wanderParks") >= n {
			return
		}
		time.Sleep(150 * time.Millisecond)
	}
	seqs := readNumberArray(t, page, "window.__wanderSeqs")
	rawSeqs := readString(t, page, "JSON.stringify(window.__wanderSeqs)")
	bubbles := readInt(t, page, "document.querySelectorAll('.msg.concierge').length")
	errs := readString(t, page, "JSON.stringify(Array.from(document.querySelectorAll('.msg.error')).map(e=>e.textContent))")
	t.Fatalf("timed out waiting for %d parked concierge replies; seqs=%v rawSeqs=%s conciergeBubbles=%d errors=%s states=%v",
		n, seqs, rawSeqs, bubbles, errs, readStringArray(t, page, "window.__wanderStates"))
}

func waitForState(t *testing.T, page playwright.Page, state string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		states := readStringArray(t, page, "window.__wanderStates")
		for _, s := range states {
			if s == state {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for connection state %q; saw %v", state,
		readStringArray(t, page, "window.__wanderStates"))
}

// assertContiguousNoLossNoDup proves the full no-loss/no-dup property, not merely
// monotonicity. The loop emits domain events with a CONTIGUOUS per-loop `event.Seq`
// counter (+1 each; keepalive frames carry no seq and are filtered by the SDK before they
// reach __wanderSeqs), so the browser's observed seq log must increase by exactly 1 at
// every step across BOTH parks and the mid-stream cut:
//   - a DUPLICATE (a re-delivered overlap frame after the reconnect) would show a step of
//     0 or a repeat — caught;
//   - a LOSS (an event dropped in the subscribe→replay gap and never re-delivered) would
//     show a step > 1 — caught. A plain strictly-increasing check would MISS this, so the
//     contiguity check is what makes the browser lane actually assert no-loss.
func assertContiguousNoLossNoDup(t *testing.T, seqs []int) {
	t.Helper()
	for i := 1; i < len(seqs); i++ {
		if seqs[i] != seqs[i-1]+1 {
			t.Fatalf("no-loss/no-dup violated: seq log not contiguous at index %d (%d after %d): %v",
				i, seqs[i], seqs[i-1], seqs)
		}
	}
}

// assertReconnectedThenLive requires the state log to show a `reconnecting` that is followed
// by a later `live` — proof the SDK resumed the stream after the offline drop rather than
// throwing or stalling.
func assertReconnectedThenLive(t *testing.T, states []string) {
	t.Helper()
	recon := -1
	for i, s := range states {
		if s == "reconnecting" {
			recon = i
			break
		}
	}
	if recon < 0 {
		t.Fatalf("no `reconnecting` state after the offline drop: %v", states)
	}
	for i := recon + 1; i < len(states); i++ {
		if states[i] == "live" {
			return
		}
	}
	t.Fatalf("no `live` state after `reconnecting` (stream did not resume): %v", states)
}

// --- page-value readers ---------------------------------------------------------

func readNumberArray(t *testing.T, page playwright.Page, expr string) []int {
	t.Helper()
	v, err := page.Evaluate(expr)
	if err != nil {
		t.Fatalf("evaluate %q: %v", expr, err)
	}
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]int, 0, len(arr))
	for _, e := range arr {
		if n, ok := toInt(e); ok {
			out = append(out, n)
		}
	}
	return out
}

// toInt coerces a playwright-returned JSON number (which may arrive as int, int64, or
// float64 depending on the value) into a Go int.
func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	default:
		return 0, false
	}
}

func readStringArray(t *testing.T, page playwright.Page, expr string) []string {
	t.Helper()
	v, err := page.Evaluate(expr)
	if err != nil {
		t.Fatalf("evaluate %q: %v", expr, err)
	}
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func readInt(t *testing.T, page playwright.Page, expr string) int {
	t.Helper()
	v, err := page.Evaluate(expr)
	if err != nil {
		t.Fatalf("evaluate %q: %v", expr, err)
	}
	if n, ok := toInt(v); ok {
		return n
	}
	return 0
}

func readString(t *testing.T, page playwright.Page, expr string) string {
	t.Helper()
	v, err := page.Evaluate(expr)
	if err != nil {
		t.Fatalf("evaluate %q: %v", expr, err)
	}
	return fmt.Sprintf("%v", v)
}

// --- process / infra helpers (mirrors sdk/fuse/acceptance_test.go) ---------------

// buildWanderBundle runs examples/wander/build.sh (esbuild) to produce vendor/fuse-sdk.js.
// A failure is loud: the browser lane cannot run without the bundled SDK.
func buildWanderBundle(t *testing.T, repoRoot string) {
	t.Helper()
	build := exec.Command("bash", filepath.Join(repoRoot, "examples", "wander", "build.sh"))
	build.Dir = repoRoot
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build @fuse/sdk browser bundle (examples/wander/build.sh): %v\n%s", err, out)
	}
}

// serveWander starts `node server.js` for the Wander page on staticPort, proxying the
// Connect path to backendAddr. It waits until the static port accepts, and tears the
// process down via t.Cleanup.
func serveWander(t *testing.T, repoRoot, staticPort, backendAddr string) {
	t.Helper()
	serverJS := filepath.Join(repoRoot, "examples", "wander", "server.js")
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "node", serverJS)
	cmd.Dir = filepath.Join(repoRoot, "examples", "wander")
	cmd.Env = append(cmd.Environ(),
		"PORT="+staticPort,
		"FUSE_NET_ADDR="+backendAddr,
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

func buildFuseBinary(t *testing.T, repoRoot string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "fuse-wander-browser")
	build := exec.Command("go", "build", "-o", bin, "./cmd/fuse")
	build.Dir = repoRoot
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build ./cmd/fuse: %v\n%s", err, out)
	}
	return bin
}

func waitBounded(cmd *exec.Cmd, d time.Duration) {
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
	}
}

// newScriptedGateway starts an httptest-style gateway returning a fixed non-streamed
// assistant reply. NEVER a real provider (project policy). Torn down via t.Cleanup.
func newScriptedGateway(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("gateway listen: %v", err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"Here are a few beachfront options that sleep 6 under $300/night."}}]}`)
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return "http://" + ln.Addr().String()
}

// savedStaysMarker is the quiet turn's answer text. It exists ONLY to be assertable: if it
// is ever visible in the transcript, a quiet turn's answer was rendered without its
// question — the orphan-bubble defect the restore lane guards.
const savedStaysMarker = "Here are your saved stays."

// newSavedRefreshGateway is the restore lane's scripted double (NEVER a real provider). It
// scripts a conversation that ends in a QUIET turn, which is the fixture the restore lane
// needs:
//
//	turn 1  user asks for beachfront  → call favorite_listing (arms pendingSavedRefresh)
//	        tool result               → the beachfront answer, park
//	quiet   app sends SAVED_REFRESH_PROMPT ("Show me my saved stays.")
//	                                  → savedStaysMarker, park
//
// Both of those turns are real turns on the real loop, so BOTH are in the durable stream a
// reopened tab replays — the quiet one included. Keying on the LAST message (not "does the
// body contain a tool role") is what makes this work for a persistent interactive loop,
// whose later turns still carry earlier turns' tool messages.
//
// favorite_listing is emitted MCP-qualified and is deliberately not registered in this lane
// (no rentals server here): the runtime answers an unknown tool with an is_error tool
// result and carries on, and the app arms its refresh off the `tool.call` event, which is
// all this fixture needs. The identity lane owns the real-tool path.
func newSavedRefreshGateway(t *testing.T) string {
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
			b, _ := json.Marshal(map[string]any{"choices": []map[string]any{{
				"message": map[string]any{"role": "assistant", "content": text},
			}}})
			_, _ = w.Write(b)
		}
		const beachfront = "Here are a few beachfront options that sleep 6 under $300/night."

		if len(req.Messages) == 0 {
			reply(beachfront)
			return
		}
		last := req.Messages[len(req.Messages)-1]
		if last.Role == "tool" {
			reply(beachfront)
			return
		}
		text := strings.ToLower(string(last.Content))
		if strings.Contains(text, "saved stays") {
			reply(savedStaysMarker)
			return
		}
		if strings.Contains(text, "beachfront") {
			// Tool-call arguments are PLAIN JSON — pre-escaping them double-escapes and
			// hangs the loop with no failing assertion.
			args, _ := json.Marshal(map[string]any{"listing_id": "L1"})
			b, _ := json.Marshal(map[string]any{"choices": []map[string]any{{"message": map[string]any{
				"role":    "assistant",
				"content": "",
				"tool_calls": []map[string]any{{
					"id": "1", "type": "function",
					"function": map[string]any{"name": "mcp:rentals/favorite_listing", "arguments": string(args)},
				}},
			}}}})
			_, _ = w.Write(b)
			return
		}
		reply(beachfront)
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return "http://" + ln.Addr().String()
}

func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("freePort split: %v", err)
	}
	_ = ln.Close()
	return port
}

func waitForListen(t *testing.T, addr string, timeout time.Duration, out *syncBuffer) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("nothing accepted on %s within %s\n--- output ---\n%s", addr, timeout, out.String())
}

func repoRootFromTest(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate test file")
	}
	// …/examples/wander/browser_test.go → repo root is two dirs up.
	return filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
}

// syncBuffer is a thread-safe buffer for concurrent subprocess stdout/stderr writes.
type syncBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	return len(p), nil
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}
