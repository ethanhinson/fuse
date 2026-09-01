package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/loopauth"
	"github.com/ethanhinson/fuse/internal/toolidentity"
	"github.com/ethanhinson/fuse/internal/tools/sandbox"
)

// leakySource is a CredentialSource that fails in the two ways a real one can,
// carrying a secret in BOTH the credential it returns and the error it returns
// with it. Nothing downstream may reproduce that secret: Credential redacts
// itself and is reachable only through Header(), and the proxy discards the
// error rather than reporting it.
type leakySource struct {
	secret string
	empty  bool // return a successfully-resolved but EMPTY credential instead
}

func (s *leakySource) CredentialFor(_ context.Context, _ loopauth.Principal, t toolidentity.Target) (toolidentity.Credential, error) {
	if s.empty {
		// A success wearing a failure's clothes: an empty token is no identity at
		// all, and must be refused rather than presented.
		return toolidentity.NewCredential("Bearer", "", true), nil
	}
	return toolidentity.NewCredential("Bearer", s.secret, true),
		fmt.Errorf("sts: minting %q for audience %q failed with token %s", s.secret, t.Audience, s.secret)
}

// TestEgressRefusalReportNeverEchoesTheCredential is the D6 non-leak assertion
// for the NEW emission path: the operator notice this change added is derived
// only from the bounded fields of sandbox.RefusalInfo, so no failure mode of the
// credential seam can put token material into the diagnostic stream.
//
// It also pins the two fail-closed properties on the same path: a resolution
// ERROR is refused, and an EMPTY credential is refused — neither is downgraded
// to an unauthenticated allow-through now that a source is wired in production.
func TestEgressRefusalReportNeverEchoesTheCredential(t *testing.T) {
	const secret = "sk-live-do-not-log-me-4f8a2c"

	for name, src := range map[string]*leakySource{
		"resolution error": {secret: secret},
		"empty credential": {secret: secret, empty: true},
	} {
		t.Run(name, func(t *testing.T) {
			shortTempRoot(t)
			upstream := newUpstreamRecorder(t)
			host, port := upstream.hostPort(t)

			root := t.TempDir()
			writeEgressConfig(t, root, credentialEgressConfig(host, port))

			var buf syncBuffer
			reporter := newEgressRefusalReporter(&buf)
			proxy, err := sandbox.NewProxy(
				sandbox.WithProxyCredentialSource(src),
				sandbox.WithProxyHooks(reporter.hooks()),
			)
			if err != nil {
				t.Fatalf("NewProxy: %v", err)
			}

			sock := listenForTest(t, proxy, root, config.Config{})
			resp := forwardThroughProxy(t, sock, "http://"+net.JoinHostPort(host, port)+"/thing")
			if resp.StatusCode != http.StatusForbidden {
				t.Errorf("status = %d, want 403 — a credential that could not be supplied must never be served without one", resp.StatusCode)
			}
			if got := upstream.authorization(); got != "" {
				t.Errorf("upstream was reached with Authorization %q; the request must never have left", got)
			}

			_ = proxy.Close()
			reporter.stop()

			out := buf.String()
			if strings.Contains(out, secret) {
				t.Fatalf("the refusal report leaked the credential: %s", out)
			}
			if !strings.Contains(out, string(sandbox.RefusedCredentialUnavailable)) {
				t.Errorf("refusal report %q does not name the credential_unavailable reason", out)
			}
			if !strings.Contains(out, "signing_key") {
				t.Errorf("refusal report %q does not tell the operator what to fix", out)
			}
		})
	}
}

// blockingWriter blocks every Write until it is released. It stands in for the
// writer an operator actually has: a pipe nobody is reading, or a terminal that
// has stopped consuming.
type blockingWriter struct {
	release chan struct{}
}

func (w *blockingWriter) Write(p []byte) (int, error) {
	<-w.release
	return len(p), nil
}

// TestEgressRefusalEmissionNeverBlocksTheCaller is the concurrency guard the
// eaf4a6a fix demands: ProxyHooks.Refused can fire ON THE LISTENER'S ACCEPT
// LOOP, so an emission that waits on a slow writer stalls that principal's
// ability to accept connections at all — converting an observability feature
// into the denial-of-service the connection ceiling exists to prevent.
//
// With the writer blocked indefinitely, every hook call must still return
// promptly, and the overflow must be DROPPED and accounted for rather than
// queued without bound.
func TestEgressRefusalEmissionNeverBlocksTheCaller(t *testing.T) {
	release := make(chan struct{})
	w := &blockingWriter{release: release}
	reporter := newEgressRefusalReporter(w)
	hooks := reporter.hooks()

	// Every refusal is a DISTINCT destination, so each one is a first-of-its-kind
	// that the drain goroutine would try to write — nothing is deduplicated away.
	fired := make(chan struct{})
	go func() {
		defer close(fired)
		for i := 0; i < egressRefusalBuffer*2; i++ {
			hooks.Refused(sandbox.RefusalInfo{
				Principal: loopauth.Principal{Tenant: "t", Subject: "s"},
				Host:      fmt.Sprintf("h%d.example.com", i),
				Port:      80,
				Reason:    sandbox.RefusedNotDeclared,
			})
		}
	}()

	select {
	case <-fired:
	case <-time.After(5 * time.Second):
		close(release) // unblock the drain so the test can exit
		t.Fatal("Refused blocked on a stalled writer — a capacity refusal fires on the accept loop, which must never wait on I/O")
	}

	if dropped := reporter.dropped.Load(); dropped == 0 {
		t.Errorf("no refusal notices were dropped; the emission queued %d notices without bound", egressRefusalBuffer*2)
	}

	close(release)
	reporter.stop()
	if out := fmt.Sprint(reporter.dropped.Load()); out == "" {
		t.Fatal("unreachable")
	}
}

// TestEgressRefusalReporterStopIsIdempotent: newSandboxService folds stop() into
// the closer every entry point defers, and an entry point may run that closer on
// an early-return path as well as at the end.
func TestEgressRefusalReporterStopIsIdempotent(t *testing.T) {
	var buf syncBuffer
	reporter := newEgressRefusalReporter(&buf)
	reporter.hooks().Refused(sandbox.RefusalInfo{
		Principal: loopauth.Principal{Tenant: "t", Subject: "s"},
		Host:      "example.com", Port: 443, Reason: sandbox.RefusedCredentialTunnel,
	})
	reporter.stop()
	reporter.stop()

	if out := buf.String(); !strings.Contains(out, "example.com:443") || !strings.Contains(out, string(sandbox.RefusedCredentialTunnel)) {
		t.Errorf("report %q lost the buffered refusal", out)
	}
}

// TestEgressRefusalNoticeBudgetDefersTheRemainderToTeardown: the interactive
// shell hands stdout to bubbletea and shares the terminal with stderr, so an
// unbounded stream of asynchronous notices is the shearing defect d7bddd1 fixed
// for the OTEL SDK. Distinct refusals past the budget are counted and reported
// at teardown — after the TUI is gone — never dropped from the accounting.
func TestEgressRefusalNoticeBudgetDefersTheRemainderToTeardown(t *testing.T) {
	var buf syncBuffer
	reporter := newEgressRefusalReporter(&buf)
	h := reporter.hooks()
	const distinct = egressRefusalNoticeBudget + 10
	for i := 0; i < distinct; i++ {
		h.Refused(sandbox.RefusalInfo{
			Principal: loopauth.Principal{Tenant: "t", Subject: "s"},
			Host:      fmt.Sprintf("d%d.example.com", i), Port: 80, Reason: sandbox.RefusedNotDeclared,
		})
	}
	reporter.stop()

	out := buf.String()
	if n := strings.Count(out, "egress REFUSED — principal"); n != egressRefusalNoticeBudget {
		t.Errorf("got %d individual notices, want the budget of %d", n, egressRefusalNoticeBudget)
	}
	if !strings.Contains(out, "notice budget reached") {
		t.Errorf("report %q does not say the budget was reached", out)
	}
	if n := strings.Count(out, "egress REFUSED (repeat)"); n != distinct-egressRefusalNoticeBudget {
		t.Errorf("got %d deferred summary lines, want %d — refusals past the budget must still be accounted for", n, distinct-egressRefusalNoticeBudget)
	}
}

// TestEgressRefusalRepeatsAreSummarized: the client is a shell command the model
// wrote, so one loop can produce thousands of identical refusals. The operator
// gets one notice plus a count, not a filled terminal.
func TestEgressRefusalRepeatsAreSummarized(t *testing.T) {
	var buf syncBuffer
	reporter := newEgressRefusalReporter(&buf)
	h := reporter.hooks()
	for i := 0; i < 50; i++ {
		h.Refused(sandbox.RefusalInfo{
			Principal: loopauth.Principal{Tenant: "t", Subject: "s"},
			Host:      "denied.example.com", Port: 80, Reason: sandbox.RefusedNotDeclared,
		})
	}
	reporter.stop()

	out := buf.String()
	if n := strings.Count(out, "egress REFUSED — principal"); n != 1 {
		t.Errorf("got %d individual refusal notices, want exactly 1: %s", n, out)
	}
	if !strings.Contains(out, "49 further refusal(s)") {
		t.Errorf("report %q does not account for the suppressed repeats", out)
	}
}
