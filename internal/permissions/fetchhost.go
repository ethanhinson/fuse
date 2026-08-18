package permissions

import (
	"net"
	"net/url"
	"path"
	"strings"

	"github.com/ethanhinson/fuse/internal/permissions/reputation"
)

// fetchFloorResult is the deciding-layer + verdict of the web_fetch host floor.
type fetchFloorResult struct {
	Verdict    Verdict // VerdictDeny, VerdictAsk, or a fallthrough sentinel (VerdictAllow == "fall through to classifier")
	DecidedBy  string  // "ssrf" | "config-deny" | "config-ask" | "blocklist" | "credentialed-url" | "known-good" | "malformed-url" | "fallthrough"
	Host       string
	AllowNudge bool // true when host is known-good (hardcoded seed OR reputation.KnownGood). On a "fallthrough" result this is only a classifier allow-bias hint, NOT a bypass; a "known-good" result is the floor's own auto-approve.
}

// The hardcoded known-good seed is split into two sets. Every entry in both is
// matched via hostMatchesSuffix, so a plain "x" requires an exact host and a
// "*.x" requires a real dot-boundary suffix — "notgithub.com" does NOT match
// "github.com" and "evil.gov.attacker.com" does NOT match "*.gov".
//
// strongKnownGoodSeed is the set eligible for a real auto-approve. The bar is
// not "the operator is known" — it is that THE NAMED OPERATOR CONTROLS THE
// HOSTNAME NAMESPACE the pattern spans: every host a pattern here matches must
// be one only that operator can bring into existence. A plain entry trivially
// qualifies (it names one host). A "*.x" entry qualifies only when x's operator
// alone creates the subdomains under it — "*.wikipedia.org" and
// "*.stackexchange.com" do, because Wikimedia and Stack Exchange mint their
// language/site subdomains themselves and no outsider can claim one.
//
// This is deliberately a question about the hostname, not about the prose. Who
// AUTHORED the page is a separate matter: Wikipedia articles and Stack Exchange
// answers are user-written, yet the host is still operator-run and moderated,
// which is the property the floor leans on. What disqualifies a pattern is an
// open-registration namespace, where an attacker picks the hostname — those
// live in nudgeOnlyKnownGoodSeed instead.
//
// The floor decides on the host alone, so anything listed here is a zero-review
// GET of any path and query string under it. Add an entry only if that is
// acceptable for every host the pattern can ever match.
var strongKnownGoodSeed = []string{
	// code / dev hosting
	"github.com",
	"raw.githubusercontent.com",
	"gitlab.com",
	"pkg.go.dev",
	"go.dev",
	"crates.io",
	"docs.rs",
	"pypi.org",
	"stackoverflow.com",
	"*.stackexchange.com",
	// official docs / references
	"developer.mozilla.org",
	"docs.python.org",
	"nodejs.org",
	"react.dev",
	"kubernetes.io",
	"docs.rust-lang.org",
	"learn.microsoft.com",
	"docs.aws.amazon.com",
	"cloud.google.com",
	"developer.apple.com",
	"*.wikipedia.org",
	"arxiv.org",
	"w3.org",
	"ietf.org",
	"rfc-editor.org",
}

// nudgeOnlyKnownGoodSeed holds mild positive signals that fail the
// hostname-namespace test above. Two kinds land here, for the same reason —
// somebody other than a vouching operator chooses the hostname:
//
//   - Broad TLD wildcards. "*.gov"/"*.edu"/"*.dev" span whole top-level
//     domains rather than one operator: anyone can register a .dev domain, and
//     .edu/.gov cover thousands of independently run sites.
//   - Open-registration user-content namespaces. "*.github.io" and
//     "*.readthedocs.io" name GitHub and Read the Docs as the HOSTING operator,
//     not as the party controlling the namespace — anyone can claim
//     attacker.github.io in under a minute. (Contrast "*.wikipedia.org" and
//     "*.stackexchange.com", where only the operator mints subdomains.)
//
// Because the floor decides on the host alone, promoting either kind would make
// https://attacker.github.io/x?d=<exfil> a zero-review GET. They must only ever
// set AllowNudge — a bias hint for the classifier, never a bypass.
// Deliberately excluded from strongKnownGoodSeed.
var nudgeOnlyKnownGoodSeed = []string{
	"*.gov",
	"*.edu",
	"*.dev",
	"*.github.io",
	"*.readthedocs.io",
}

// exfilShapeDenylist names host shapes that must NEVER receive the known-good
// auto-approve, however good their reputation is.
//
// Why this list exists. The known-good promotion decides on the host alone, so
// promoting a host authorizes a zero-review GET of any path and query string
// under it. That is safe for a documentation site and unsafe for a host whose
// whole purpose is to accept attacker-chosen content or to receive data in the
// URL. Two of the promotion's inputs can grow: the hardcoded seed above (which
// at least arrives as a reviewed code diff) and reputation.KnownGood, which is
// backed by internal/permissions/reputation/data/popularity.csv — a data file
// refreshed by hand from an upstream top-sites list. Paste services, link
// shorteners and webhook endpoints are genuinely popular, so a faithful refresh
// WILL surface them. This list is the code-side floor that keeps such a refresh
// from converting popularity into authorization.
//
// The four shapes, and what each one buys an attacker who can steer a fetch:
//
//   - Paste services — attacker-authored content served from a reputable host,
//     i.e. prompt injection with a trusted-looking origin.
//   - File upload / transfer hosts — the same, plus a drop point.
//   - Link shorteners — the fetched host is not the host that was approved;
//     the real destination is chosen after the decision.
//   - Webhook / request-capture endpoints — the request itself is the payload:
//     everything in the path and query is delivered to the attacker's log.
//
// This is a FLOOR, NOT AN EXHAUSTIVE LIST. New paste and tunnel services appear
// constantly and no static list can keep up. It is a backstop for the promotion
// path, not the system's only defense: a host that is missing here still faces
// the classifier, the deny shapes, and the configured fetch_deny/fetch_ask.
// Add to it freely — a false positive here costs one prompt, since a declined
// host falls through to the classifier rather than being denied.
//
// Entries are registrable domains or specific hosts; matching covers the entry
// and its subdomains at real dot boundaries (see exfilShapeMatch). List the
// narrowest thing that is actually the exfil shape: "hooks.slack.com", not
// "slack.com".
var exfilShapeDenylist = []string{
	// paste / snippet services
	"pastebin.com",
	"gist.github.com",
	"paste.ee",
	"hastebin.com",
	"dpaste.com",
	"dpaste.org",
	"ghostbin.com",
	"rentry.co",
	"controlc.com",
	"termbin.com",
	"ix.io",
	"sprunge.us",
	"0x0.st",
	"privatebin.net",
	"justpaste.it",
	"pastes.io",
	"codepad.org",
	"paste.rs",

	// file upload / transfer drops
	"transfer.sh",
	"file.io",
	"anonfiles.com",
	"gofile.io",
	"bashupload.com",
	"catbox.moe",
	"uguu.se",
	"oshi.at",
	"pixeldrain.com",
	"filebin.net",
	"wetransfer.com",
	"temp.sh",
	"tmpfiles.org",

	// link shorteners — the approved host is not the fetched destination
	"bit.ly",
	"t.co",
	"tinyurl.com",
	"goo.gl",
	"ow.ly",
	"is.gd",
	"v.gd",
	"buff.ly",
	"cutt.ly",
	"rebrand.ly",
	"shorturl.at",
	"rb.gy",
	"t.ly",
	"s.id",
	"lnkd.in",
	"tiny.cc",
	"shorte.st",

	// webhook / request-capture / tunnel endpoints — the request IS the payload
	"hooks.slack.com",
	"discord.com",
	"discordapp.com",
	"webhook.site",
	"hookbin.com",
	"requestbin.com",
	"requestcatcher.com",
	"beeceptor.com",
	"pipedream.net",
	"api.telegram.org",
	"hooks.zapier.com",
	"maker.ifttt.com",
	"ngrok.io",
	"ngrok-free.app",
	"ngrok.app",
	"trycloudflare.com",
	"localtunnel.me",
	"loca.lt",
	"serveo.net",
	"interact.sh",
	"oast.fun",
	"oast.pro",
	"oast.live",
	"burpcollaborator.net",
	"dnslog.cn",
	"canarytokens.com",
}

// exfilShapeMatch reports whether host is an exfil shape per
// exfilShapeDenylist. An entry matches the host itself and any subdomain of it,
// both at real dot boundaries via hostMatchesSuffix — so "pastebin.com" covers
// "www.pastebin.com" but not "notpastebin.com" and not
// "pastebin.com.evil.example". Substring matching is deliberately not used: it
// would both over-match unrelated hosts and be trivially evaded.
//
// The caller must pass an already-canonicalized host (reputation.CanonicalHost),
// as classifyFetchHost does; the entries are lowercase and dot-free of the root
// dot.
func exfilShapeMatch(host string) bool {
	for _, entry := range exfilShapeDenylist {
		// Tolerate an entry written in the seed's "*.x" style by reducing it to
		// its bare domain first; both forms are then checked below, so a
		// wildcard entry can never end up narrower than a plain one.
		bare := strings.TrimPrefix(entry, "*.")
		if hostMatchesSuffix(host, bare) || hostMatchesSuffix(host, "*."+bare) {
			return true
		}
	}
	return false
}

// hostMatchesSuffix reports whether host matches pattern with real
// dot-boundary semantics. A "*.x" pattern matches a host that ends in ".x"
// (a genuine label boundary) but not the bare apex "x". A plain "x" pattern
// requires exact equality. Both host and pattern are compared case-sensitively;
// callers lowercase the host and the seed patterns are already lowercase.
func hostMatchesSuffix(host, pattern string) bool {
	if strings.HasPrefix(pattern, "*.") {
		suffix := pattern[1:] // ".x"
		return strings.HasSuffix(host, suffix) && len(host) > len(suffix)
	}
	return host == pattern
}

// strongSeedMatch reports whether host is in the strong
// (operator-controlled-namespace) part of the hardcoded known-good seed. This
// is the only half eligible to back a real auto-approve; the broad TLD
// wildcards and the open-registration user-content namespaces are excluded by
// construction.
func strongSeedMatch(host string) bool {
	return seedMatch(host, strongKnownGoodSeed)
}

// knownGoodSeedMatch reports whether host is in the hardcoded known-good seed,
// i.e. the union of the strong and nudge-only sets. Membership only sets
// AllowNudge; it never changes the Verdict.
func knownGoodSeedMatch(host string) bool {
	return strongSeedMatch(host) || seedMatch(host, nudgeOnlyKnownGoodSeed)
}

// seedMatch reports whether host matches any pattern in seed under
// hostMatchesSuffix's dot-boundary semantics.
func seedMatch(host string, seed []string) bool {
	for _, pat := range seed {
		if hostMatchesSuffix(host, pat) {
			return true
		}
	}
	return false
}

// isSSRFHost reports whether host targets a loopback, private, or link-local
// address (or the literal "localhost"). It uses net.ParseIP rather than string
// prefixes so it cannot be fooled by textual tricks, and covers the RFC-1918
// v4 ranges plus fc00::/7 via IsPrivate.
func isSSRFHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsPrivate()
}

// matchesAnyGlob reports whether host matches any of the given path.Match
// patterns. path.Match treats '.' as a literal and '*' as a run not crossing
// '/', which suits host globs ("*.evil.com" matches "sub.evil.com"). An exact
// host string is a degenerate pattern and matches itself. A malformed pattern
// (path.ErrBadPattern) is treated as a non-match.
func matchesAnyGlob(host string, patterns []string) bool {
	for _, pat := range patterns {
		if ok, err := path.Match(pat, host); err == nil && ok {
			return true
		}
	}
	return false
}

// classifyFetchHost extracts the host from rawURL, canonicalizes it with
// reputation.CanonicalHost, and applies the static web_fetch floor to that one
// canonical value. The layers, in order:
//
//	malformed/opaque URL (no host)        => Ask,  "malformed-url"
//	SSRF (loopback/RFC-1918/link-local/…) => Deny, "ssrf"
//	fetchDeny glob match                  => Deny, "config-deny"  (deny beats ask)
//	fetchAsk glob match                   => Ask,  "config-ask"
//	reputation.Blocked(host)              => Deny, "blocklist"
//	URL carries userinfo (u.User != nil)  => Ask,  "credentialed-url"
//	strong seed / reputation.KnownGood    => Allow, "known-good"  (a real auto-approve),
//	                                        unless exfilShapeMatch withholds it
//	otherwise                             => VerdictAllow sentinel, "fallthrough"
//
// The order is load-bearing. SSRF is checked first so a private IP always
// denies. The known-good promotion (change 0069) sits strictly last among the
// deciding layers, so a configured fetch_deny/fetch_ask and the reputation
// blocklist all beat it: config always wins over the seed. The exfil-shape
// subtraction is evaluated after all of those and immediately before the
// promotion — it withholds an auto-approve but never creates a decision, so it
// cannot reorder anything above it.
//
// The credentialed-URL layer sits between the denying layers and the promotion
// for the same reason: it is an ASK, so every Deny above it still decides
// first, and it never reorders them. It is decided HERE rather than delegated
// downward because userinfo is the one deny shape in this file that lives in
// the URL instead of the host — and the classifier is shown the host alone (see
// webFetchPendingPrompt), so falling through would hand the judgement to a
// layer structurally blind to the property that triggered it. The floor already
// holds the parsed URL, so it is the only place the shape is decidable at all.
//
// Only the strong half of the hardcoded seed — the entries whose named operator
// controls the whole hostname namespace the pattern spans — and the exact
// reputation.KnownGood top-site set can auto-approve, and only when the host is
// not an exfil shape (see exfilShapeDenylist) — the popularity list is a
// refreshable data file, so that subtraction is what stops a routine refresh
// from silently authorizing a paste host or a link shortener.
// The broad TLD
// wildcards (*.gov, *.edu, *.dev) and the open-registration user-content
// namespaces (*.github.io, *.readthedocs.io) are deliberately excluded — they
// still fall through with AllowNudge set, which is a classifier bias hint and
// never a bypass.
// AllowNudge on a "fallthrough" result comes from the full seed union OR
// reputation.KnownGood and never changes that Verdict.
//
// LIMITATION — this floor authorizes the REQUESTED host only, not the host
// actually contacted. The HTTP client used downstream (internal/research)
// follows redirects with no CheckRedirect hook set anywhere in this tree, so
// a known-good host that 302s elsewhere (e.g. to a private/link-local address
// or an attacker-controlled endpoint) is fetched without this floor
// re-deciding on the redirect target. Callers must not read a "known-good" or
// "fallthrough" verdict here as a guarantee about the host the request
// ultimately lands on. A per-hop re-check belongs in internal/research, where
// the actual HTTP client lives, not in this classifier.
func classifyFetchHost(rawURL string, fetchDeny, fetchAsk []string) fetchFloorResult {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fetchFloorResult{Verdict: VerdictAsk, DecidedBy: "malformed-url"}
	}
	// Canonicalize ONCE, before every layer, and derive all of them from this
	// single value. url.Hostname preserves the root dot, so the raw host of
	// "https://google.com./q" is "google.com." — which path.Match and
	// hostMatchesSuffix both miss while reputation.Blocked/KnownGood match it
	// anyway (they canonicalize internally). Matching on the raw host would
	// therefore let the FQDN spelling skip config-deny/config-ask and land on
	// the known-good auto-approve, breaking "config always beats the seed" with
	// a one-character mutation; it would likewise walk "localhost." past the
	// SSRF check. Using reputation.CanonicalHost — the very function those
	// lookups normalize with — is what keeps the layers in agreement.
	host := reputation.CanonicalHost(u.Hostname())
	if host == "" {
		return fetchFloorResult{Verdict: VerdictAsk, DecidedBy: "malformed-url"}
	}

	if isSSRFHost(host) {
		return fetchFloorResult{Verdict: VerdictDeny, DecidedBy: "ssrf", Host: host}
	}
	if matchesAnyGlob(host, fetchDeny) {
		return fetchFloorResult{Verdict: VerdictDeny, DecidedBy: "config-deny", Host: host}
	}
	if matchesAnyGlob(host, fetchAsk) {
		return fetchFloorResult{Verdict: VerdictAsk, DecidedBy: "config-ask", Host: host}
	}
	if reputation.Blocked(host) {
		return fetchFloorResult{Verdict: VerdictDeny, DecidedBy: "blocklist", Host: host}
	}

	// Credential-bearing URL. "https://<token>@github.com/x" has
	// Hostname() == "github.com", so without this layer a leaked token rides a
	// strong-seed host straight through the promotion below with zero review.
	// Userinfo is a deterministic, host-independent, credential-leak shape the
	// floor already holds in hand — and the ONLY layer that can see it: the
	// web_fetch classifier is shown the canonical HOST and nothing else, so
	// falling through here would be close to a no-op. So the floor decides, and
	// it decides Ask rather than Deny: a credentialed URL is sometimes a
	// legitimate private-registry or basic-auth fetch, and the human is the
	// judge with the context the classifier lacks.
	//
	// This also settles the AllowNudge question for the shape: the result below
	// is never reached for a credentialed URL, so no nudge is ever emitted for
	// one. That is deliberate and must stay true — a reputable host name says
	// nothing about a URL carrying a secret, so anyone converting this Ask into
	// a fallthrough has to withhold AllowNudge explicitly.
	if u.User != nil {
		return fetchFloorResult{Verdict: VerdictAsk, DecidedBy: "credentialed-url", Host: host}
	}

	// Exfil-shape subtraction. Checked BEFORE the promotion below, and it
	// subtracts from every input to that promotion — the hardcoded seed as well
	// as the refreshable popularity list — so the two can never disagree about
	// a host. A match here is not a decision: it only withholds the promotion
	// (and the allow-bias nudge), leaving the host to the classifier.
	exfilShape := exfilShapeMatch(host)

	// Known-good auto-approve. Both membership tests are exact/dot-boundary:
	// strongSeedMatch uses hostMatchesSuffix, and reputation.KnownGood is an
	// exact lookup in the bundled popularity set with no subdomain widening.
	// This runs only after every denying layer above has declined.
	if !exfilShape && (strongSeedMatch(host) || reputation.KnownGood(host)) {
		return fetchFloorResult{Verdict: VerdictAllow, DecidedBy: "known-good", Host: host, AllowNudge: true}
	}

	return fetchFloorResult{
		Verdict:   VerdictAllow,
		DecidedBy: "fallthrough",
		Host:      host,
		// An exfil shape gets no nudge either. The nudge biases the classifier
		// toward allow, and these are precisely the hosts where a reputable
		// name says nothing about the attacker-chosen URL under it.
		AllowNudge: !exfilShape && (knownGoodSeedMatch(host) || reputation.KnownGood(host)),
	}
}
