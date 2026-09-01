package sandbox

import (
	"net"
	"strings"
)

// NOT BUILT HERE: remote/PaaS backend acceptance criterion (#75).
//
// This change (#0064) only enforces egress for the local container backend.
// A future remote/PaaS backend that wants to host `bash` (#75, tracked out of
// scope by this change's plan) is NOT automatically covered by this policy —
// a remote executor sits outside the host-side proxy and nftables floor this
// package builds, so it needs its own equivalent enforcement.
//
// The concrete, non-negotiable acceptance criterion for that future backend,
// carried over from ADR-0044: the cloud metadata endpoint, 169.254.169.254,
// MUST be null-routed (or otherwise unreachable) from the remote execution
// context by default, exactly as the container floor here denies it absent an
// explicit allowlist entry. A remote/PaaS backend that can reach the metadata
// endpoint without an operator having declared it in Egress.Allow does not
// meet the bar this package sets, and must not be considered a peer
// implementation of "egress control for bash" until it does.

// Match reports whether host:port is DECLARED in this policy's allowlist, and
// returns the matched entry so its optional #52 credential audience can be read
// off it.
//
// # The caller canonicalizes; Match does not
//
// host MUST ALREADY BE CANONICAL — the caller passes the result of
// reputation.CanonicalHost, applied ONCE at the proxy's entry point, before any
// decision is taken (ADR-0048 rule 3). Match deliberately does not lowercase,
// trim, or strip a trailing root dot: a second normalizer here could drift from
// the shared one, and one gate matching a raw spelling while another matches a
// normalized one is precisely the live bug ADR-0048 records. The declared side
// is canonicalized in parseAllowHost, also exactly once. If you find yourself
// wanting to normalize here, normalize at the entry point instead.
//
// # What matches
//
//   - An exact-host entry matches when the canonical hostnames are equal. There
//     is no suffix, wildcard, or subdomain matching: "*.example.com" does not
//     exist in v1 (plan Q4), and "sub.example.com" is not "example.com".
//   - A CIDR entry matches only when host is a literal IP address inside the
//     block. A hostname is NEVER resolved to test membership: DNS is
//     attacker-influenced, and resolving would make an authorization decision
//     depend on a lookup the policy does not own. A bare IP in the config is
//     stored as a full-mask block, so IP destinations are compared as values and
//     no alternate spelling of one address can miss its entry.
//   - Port must be EQUAL. There are no ranges and no "any port" wildcard.
//
// # What never matches
//
// An empty allowlist matches nothing. That is the deny-all state, and it is a
// legitimate resolved outcome: the operator declared no entries, or the loader
// refused to trust the ones they did declare, or the mode was unreadable. It is
// also what an allow-all Egress carries, since the loader leaves Allow nil there
// — under allow-all there is no proxy in the path to ask, and a matcher asked
// anyway must not invent a policy nobody wrote.
//
// Every unrecognized shape resolves to "not declared". This function's failure
// direction is denial, always.
func (e Egress) Match(host string, port int) (AllowEntry, bool) {
	if host == "" || len(e.Allow) == 0 {
		return AllowEntry{}, false
	}

	// Parsed once, outside the loop: the destination is either a literal
	// address or it is not, and that answer does not vary by entry.
	ip := literalIP(host)

	for _, entry := range e.Allow {
		if entry.Port != port {
			continue
		}
		switch {
		case entry.CIDR != nil:
			if ip != nil && entry.CIDR.Contains(ip) {
				return entry, true
			}
		case entry.Host != "":
			if entry.Host == host {
				return entry, true
			}
		}
		// An entry with neither a destination host nor a block is unusable.
		// The loader cannot produce one — it is the AllowEntry invariant that
		// exactly one is set — but a hand-built one must match nothing rather
		// than degrade into "any host on this port".
	}

	return AllowEntry{}, false
}

// literalIP parses host as an IP address literal, accepting the bracketed
// spelling an HTTP CONNECT line uses for IPv6 ("[2001:db8::1]") as well as the
// plain one, and returns nil for anything else — notably for a hostname, which
// is never resolved here.
//
// Stripping the brackets is address-literal SYNTAX, not host normalization:
// reputation.CanonicalHost neither adds nor removes them, so unwrapping here
// duplicates nothing it does and cannot drift from it. Both spellings must
// reach the same decision, and comparing IP values rather than strings is what
// makes that true for every other alternate spelling too.
func literalIP(host string) net.IP {
	if len(host) >= 2 && strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = host[1 : len(host)-1]
	}
	return net.ParseIP(host)
}
