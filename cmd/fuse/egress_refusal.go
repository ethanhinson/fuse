package main

import (
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/ethanhinson/fuse/internal/tools/sandbox"
)

// egressRefusalReporter turns the egress proxy's observer seam
// (sandbox.ProxyHooks) into the operator notice that says WHY a contained
// command could not reach the network.
//
// # Why this channel
//
// It writes to the SAME writer the composition root prints its UNCONTAINED and
// EGRESS ENFORCED notices on, and for the same reason: a refusal is a statement
// about the operator's own config file, and the operator is the only party who
// can act on it. The alternative channel in this repo — a loop's event stream,
// via tools.SandboxEventHooks — is deliberately NOT used here, because the Proxy
// is a PROCESS resource built at startup by newSandboxService, before any loop
// (and therefore any loop-scoped EventStore) exists. Wiring it to an event store
// would mean either attributing every principal's refusals to whichever loop
// started last, or leaving the hook unconsumed on every non-loop-server binding —
// which is the defect being fixed. Routing egress refusals into the event stream
// as well is a follow-on that belongs with the projector/dashboard work, not here.
//
// # Why it is asynchronous
//
// sandbox.ProxyHooks documents that Refused may fire ON THE LISTENER'S ACCEPT
// LOOP — a capacity refusal has no goroutine of its own. A hook that wrote to
// the writer inline would put an arbitrary io.Writer (a pipe with nobody
// reading, a TUI's stderr) directly in the path of that principal's ability to
// accept connections, which is exactly the runaway the connection ceiling exists
// to bound. So the hook does one non-blocking channel send and returns; a single
// drain goroutine does the formatting and the writing, and a full buffer DROPS
// (counted, reported at shutdown) rather than waiting. Losing a notice is
// strictly better than stalling the accept loop: the refusal itself already
// happened, fail-closed, whether or not anyone hears about it.
//
// # What it may say
//
// ONLY the bounded fields of sandbox.RefusalInfo: the principal, the canonical
// destination, and the closed reason enum. No credential is reachable from here
// — RefusalInfo carries none, by design (a resolved toolidentity.Credential
// never leaves the proxy's delegatedHeader, and neither does the error from a
// failed resolution) — and nothing in this file formats, wraps, or echoes one.
type egressRefusalReporter struct {
	w  io.Writer
	ch chan sandbox.RefusalInfo

	stopc   chan struct{}
	done    chan struct{}
	once    sync.Once
	dropped atomic.Int64
}

const (
	// egressRefusalBuffer is how many refusals may be in flight to the drain
	// goroutine. It is sized above one principal's connection ceiling
	// (proxyMaxConnsPerPrincipal = 128) so an ordinary burst — a command opening
	// many connections to an undeclared host — is reported in full rather than
	// partly dropped, while staying a fixed, small allocation.
	egressRefusalBuffer = 256

	// egressRefusalKeyCeiling bounds the distinct (reason, destination) keys the
	// drain goroutine remembers. A command can name unboundedly many
	// destinations, and an unbounded map fed by model-authored traffic is a
	// memory-growth lever. Past the ceiling, refusals are still COUNTED — into a
	// single overflow bucket — so the summary never claims fewer refusals than
	// happened.
	egressRefusalKeyCeiling = 256

	// egressRefusalNoticeBudget bounds how many individual refusal notices are
	// written DURING a session. The rest are counted and reported by summarize at
	// teardown.
	//
	// The budget exists because of where these lines land. In the interactive
	// shell, stdout belongs to bubbletea's alt screen and stderr shares the same
	// terminal, so anything written asynchronously mid-session can shear the
	// render — that is the defect commit d7bddd1 fixed for the OTEL SDK's async
	// export errors, and this reporter has exactly that shape. Silencing egress
	// refusals is not an option (they are the operator's only answer to "why did
	// my command's network call fail?"), so the compromise is: the first few
	// DISTINCT refusals are said immediately, when they are diagnostic, and a
	// runaway's remainder waits for teardown, when the TUI has already exited and
	// nothing can be sheared.
	egressRefusalNoticeBudget = 20
)

// refusalKey is the identity of a repeated refusal: the same principal being
// told the same thing about the same destination. Repeats are counted rather
// than printed, so a `for i in $(seq 1000); do curl ...; done` produces one
// notice and one summary line instead of a thousand notices.
type refusalKey struct {
	tenant  string
	subject string
	host    string
	port    int
	reason  sandbox.RefusalReason
}

// newEgressRefusalReporter starts the drain goroutine. w must be safe for
// concurrent use with the composition root's own notices, since those are
// written from the startup goroutine while this one writes from the drain: every
// production caller passes os.Stderr, whose Write is a single syscall per line.
//
// The caller MUST call
// stop() — newSandboxService folds it into the closer it hands every entry
// point, after Proxy.Close, which is the point at which no further refusal can
// be reported.
func newEgressRefusalReporter(w io.Writer) *egressRefusalReporter {
	r := &egressRefusalReporter{
		w:     w,
		ch:    make(chan sandbox.RefusalInfo, egressRefusalBuffer),
		stopc: make(chan struct{}),
		done:  make(chan struct{}),
	}
	go r.drain()
	return r
}

// hooks is the seam handed to sandbox.WithProxyHooks.
//
// The Refused func is the whole non-blocking contract: a select with a default
// branch, one channel send, no lock, no I/O, no allocation on the hot path. It
// is safe to call from any goroutine, including the accept loop, and safe to
// call after stop() — the channel is never closed, so a late refusal is buffered
// or dropped, never a send on a closed channel.
func (r *egressRefusalReporter) hooks() sandbox.ProxyHooks {
	return sandbox.ProxyHooks{
		Refused: func(info sandbox.RefusalInfo) {
			select {
			case r.ch <- info:
			default:
				r.dropped.Add(1)
			}
		},
	}
}

// stop drains what is buffered, writes the suppression summary, and joins the
// drain goroutine. It is idempotent.
func (r *egressRefusalReporter) stop() {
	r.once.Do(func() { close(r.stopc) })
	<-r.done
}

func (r *egressRefusalReporter) drain() {
	defer close(r.done)
	tally := &refusalTally{
		seen:    make(map[refusalKey]int),
		printed: make(map[refusalKey]bool),
	}
	for {
		select {
		case info := <-r.ch:
			r.record(tally, info)
		case <-r.stopc:
			// Everything already buffered is still the operator's to see. Drained
			// without blocking: nothing can be added once the proxy is closed, and
			// a stop must not be able to wait on a producer.
			for {
				select {
				case info := <-r.ch:
					r.record(tally, info)
				default:
					r.summarize(tally)
					return
				}
			}
		}
	}
}

// refusalTally is the drain goroutine's whole state. It is owned by that one
// goroutine and never shared, which is why none of it is synchronized.
type refusalTally struct {
	// seen counts every refusal of each kind, printed or not.
	seen map[refusalKey]int
	// printed records which kinds got an individual notice, so the shutdown
	// summary can report the remainder exactly rather than assuming one was shown.
	printed map[refusalKey]bool
	// notices is how many individual notices have been written this session.
	notices int
	// overflow counts refusals past the distinct-key ceiling.
	overflow int
}

// record prints the FIRST refusal of each kind, within the session notice
// budget, and counts everything else for the shutdown summary.
func (r *egressRefusalReporter) record(t *refusalTally, info sandbox.RefusalInfo) {
	key := refusalKey{
		tenant:  string(info.Principal.Tenant),
		subject: info.Principal.Subject,
		host:    info.Host,
		port:    info.Port,
		reason:  info.Reason,
	}
	n, known := t.seen[key]
	if !known && len(t.seen) >= egressRefusalKeyCeiling {
		t.overflow++
		return
	}
	t.seen[key] = n + 1
	if known || t.notices >= egressRefusalNoticeBudget {
		return
	}
	t.notices++
	t.printed[key] = true
	fmt.Fprintf(r.w, "sandbox: egress REFUSED — %s → %s (%s); %s\n",
		principalLabel(info), destinationLabel(info), string(info.Reason), refusalAdvice(info.Reason))
	if t.notices == egressRefusalNoticeBudget {
		fmt.Fprintf(r.w, "sandbox: egress REFUSED — notice budget reached; further refusals are counted and summarized at shutdown\n")
	}
}

// summarize reports, at teardown, everything that was counted but not printed —
// so an operator can tell a single stray call from a storm without the storm
// having filled their terminal mid-session.
func (r *egressRefusalReporter) summarize(t *refusalTally) {
	for key, n := range t.seen {
		remaining := n
		if t.printed[key] {
			remaining--
		}
		if remaining == 0 {
			continue
		}
		fmt.Fprintf(r.w, "sandbox: egress REFUSED (repeat) — %s → %s (%s): %d further refusal(s) not shown individually\n",
			principalLabelOf(key.tenant, key.subject), destinationLabelOf(key.host, key.port), string(key.reason), remaining)
	}
	if t.overflow > 0 {
		fmt.Fprintf(r.w, "sandbox: egress REFUSED — %d further refusal(s) to additional destinations were counted but not detailed\n", t.overflow)
	}
	if dropped := r.dropped.Load(); dropped > 0 {
		fmt.Fprintf(r.w, "sandbox: egress REFUSED — %d refusal notice(s) were dropped rather than delay the proxy; the refusals themselves still happened\n", dropped)
	}
}

func principalLabel(info sandbox.RefusalInfo) string {
	return principalLabelOf(string(info.Principal.Tenant), info.Principal.Subject)
}

func principalLabelOf(tenant, subject string) string {
	if tenant == "" && subject == "" {
		return "principal (unattributed)"
	}
	return "principal " + tenant + "/" + subject
}

func destinationLabel(info sandbox.RefusalInfo) string {
	return destinationLabelOf(info.Host, info.Port)
}

// destinationLabelOf renders the destination the proxy refused. An empty host is
// the honest answer for a refusal taken before the client named one (a capacity
// refusal at accept, or a request that could not be read).
func destinationLabelOf(host string, port int) string {
	if host == "" {
		return "(no destination named)"
	}
	if port == 0 {
		return host
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}

// refusalAdvice maps the proxy's closed reason enum onto what the operator would
// have to change. The switch is exhaustive over sandbox's RefusalReason
// constants; an unrecognised one gets no advice rather than a guess.
func refusalAdvice(reason sandbox.RefusalReason) string {
	switch reason {
	case sandbox.RefusedNotDeclared:
		return "declare it under egress.allow in .fuse/sandbox.local.yml to permit it"
	case sandbox.RefusedCredentialUnavailable:
		return "the entry declares a `credential:` audience that could not be minted — check tool_identity.signing_key in ~/.fuse/config.yml"
	case sandbox.RefusedCredentialTunnel:
		return "a `credential:` destination is reachable only as plaintext http through the proxy; an https CONNECT tunnel cannot carry the delegated identity"
	case sandbox.RefusedUpstreamUnreachable:
		return "the destination is declared but could not be reached — a network fault, not a policy denial"
	case sandbox.RefusedMalformedTarget, sandbox.RefusedNonConnect:
		return "the request named no destination the allowlist could be consulted about"
	case sandbox.RefusedPrincipalConnLimit:
		return "this principal already holds the maximum concurrent egress connections"
	case sandbox.RefusedProxyConnLimit:
		return "the host-wide egress connection ceiling is full"
	default:
		return "refused by egress policy"
	}
}
