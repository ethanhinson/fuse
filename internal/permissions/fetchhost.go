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
	DecidedBy  string  // "ssrf" | "config-deny" | "config-ask" | "blocklist" | "known-good" | "malformed-url" | "fallthrough"
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
//	strong seed / reputation.KnownGood    => Allow, "known-good"  (a real auto-approve)
//	otherwise                             => VerdictAllow sentinel, "fallthrough"
//
// The order is load-bearing. SSRF is checked first so a private IP always
// denies. The known-good promotion (change 0069) sits strictly last among the
// deciding layers, so a configured fetch_deny/fetch_ask and the reputation
// blocklist all beat it: config always wins over the seed.
//
// Only the strong half of the hardcoded seed — the entries whose named
// operator controls the whole hostname namespace the pattern spans — and the
// exact reputation.KnownGood top-site set can auto-approve. The broad TLD
// wildcards (*.gov, *.edu, *.dev) and the open-registration user-content
// namespaces (*.github.io, *.readthedocs.io) are deliberately excluded — they
// still fall through with AllowNudge set, which is a classifier bias hint and
// never a bypass.
// AllowNudge on a "fallthrough" result comes from the full seed union OR
// reputation.KnownGood and never changes that Verdict.
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

	// Known-good auto-approve. Both membership tests are exact/dot-boundary:
	// strongSeedMatch uses hostMatchesSuffix, and reputation.KnownGood is an
	// exact lookup in the bundled popularity set with no subdomain widening.
	// This runs only after every denying layer above has declined.
	if strongSeedMatch(host) || reputation.KnownGood(host) {
		return fetchFloorResult{Verdict: VerdictAllow, DecidedBy: "known-good", Host: host, AllowNudge: true}
	}

	return fetchFloorResult{
		Verdict:    VerdictAllow,
		DecidedBy:  "fallthrough",
		Host:       host,
		AllowNudge: knownGoodSeedMatch(host) || reputation.KnownGood(host),
	}
}
