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
	DecidedBy  string  // "ssrf" | "config-deny" | "config-ask" | "blocklist" | "malformed-url" | "fallthrough"
	Host       string
	AllowNudge bool // true when host is known-good (hardcoded seed OR reputation.KnownGood) — a classifier allow-bias hint, NOT a bypass
}

// knownGoodSeed is a hardcoded suffix/exact set of code/dev, official-docs, and
// reference hosts plus mild positive TLD signals. Each entry is matched via
// hostMatchesSuffix so a plain "x" requires an exact host and a "*.x" requires a
// real dot-boundary suffix — "notgithub.com" does NOT match "github.com" and
// "evil.gov.attacker.com" does NOT match "*.gov". Membership only sets
// AllowNudge; it never changes the Verdict.
var knownGoodSeed = []string{
	// code / dev hosting
	"github.com",
	"*.github.io",
	"raw.githubusercontent.com",
	"gitlab.com",
	"pkg.go.dev",
	"go.dev",
	"crates.io",
	"docs.rs",
	"pypi.org",
	"*.readthedocs.io",
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
	// mild positive TLD signals
	"*.gov",
	"*.edu",
	"*.dev",
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

// knownGoodSeedMatch reports whether host is in the hardcoded known-good seed.
func knownGoodSeedMatch(host string) bool {
	for _, pat := range knownGoodSeed {
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

// classifyFetchHost extracts the host from rawURL and applies the static
// web_fetch floor. The layers, in order:
//
//	malformed/opaque URL (no host)        => Ask,  "malformed-url"
//	SSRF (loopback/RFC-1918/link-local/…) => Deny, "ssrf"
//	fetchDeny glob match                  => Deny, "config-deny"  (deny beats ask)
//	fetchAsk glob match                   => Ask,  "config-ask"
//	reputation.Blocked(host)              => Deny, "blocklist"
//	otherwise                             => VerdictAllow sentinel, "fallthrough"
//
// SSRF is checked before config/blocklist so a private IP always denies.
// AllowNudge is set on the fallthrough result from the hardcoded seed OR
// reputation.KnownGood; it never changes the Verdict.
func classifyFetchHost(rawURL string, fetchDeny, fetchAsk []string) fetchFloorResult {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fetchFloorResult{Verdict: VerdictAsk, DecidedBy: "malformed-url"}
	}
	host := strings.ToLower(u.Hostname())
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

	return fetchFloorResult{
		Verdict:    VerdictAllow,
		DecidedBy:  "fallthrough",
		Host:       host,
		AllowNudge: knownGoodSeedMatch(host) || reputation.KnownGood(host),
	}
}
