package sandbox

import (
	"net"
	"testing"

	"github.com/ethanhinson/fuse/internal/permissions/reputation"
)

// loadEgress runs the declared entries through the REAL loader, so a matcher
// test is exercising the same AllowEntry values the proxy will hold — including
// the load-time canonicalization the matcher deliberately does not repeat.
func loadEgress(t *testing.T, entries ...string) Egress {
	t.Helper()
	root := t.TempDir()
	writeConfigFile(t, root, egressBody("enforce", entries...))
	cfg, warns := LoadConfig(root)
	if len(warns) != 0 {
		t.Fatalf("warnings = %v, want none for %v", warns, entries)
	}
	if cfg.Egress.Mode != EgressEnforce {
		t.Fatalf("Egress.Mode = %v, want EgressEnforce", cfg.Egress.Mode)
	}
	if len(cfg.Egress.Allow) != len(entries) {
		t.Fatalf("Egress.Allow = %+v, want %d entries", cfg.Egress.Allow, len(entries))
	}
	return cfg.Egress
}

// matchAtEntry is the proxy's whole decision path: canonicalize ONCE at the
// entry point (ADR-0048 rule 3), then match. Every spelling regression below
// goes through this, because that is where a spelling is normalized — never
// inside Match.
func matchAtEntry(e Egress, rawHost string, port int) (AllowEntry, bool) {
	return e.Match(reputation.CanonicalHost(rawHost), port)
}

// The deny-all state. An Egress with nothing declared matches NOTHING — not a
// hostname, not an IP, not any port. This is asserted explicitly rather than
// left implied, because it is the floor the whole change rests on.
func TestEgressMatchEmptyAllowlistNeverMatches(t *testing.T) {
	for name, e := range map[string]Egress{
		"nil allow under enforce":   {Mode: EgressEnforce},
		"empty allow under enforce": {Mode: EgressEnforce, Allow: []AllowEntry{}},
		"zero value":                {},
		// The loader leaves Allow nil under allow-all even when entries were
		// declared; the matcher must not invent a policy out of that.
		"allow-all": {Mode: EgressAllowAll},
	} {
		t.Run(name, func(t *testing.T) {
			for _, tc := range []struct {
				host string
				port int
			}{
				{"pkg.example.com", 443},
				{"192.0.2.7", 443},
				{"2001:db8::1", 443},
				{"", 443},
				{"pkg.example.com", 0},
			} {
				if entry, ok := e.Match(tc.host, tc.port); ok {
					t.Errorf("Match(%q, %d) = (%+v, true), want no match", tc.host, tc.port, entry)
				}
			}
		})
	}
}

// Exact-host entries: the host must be equal and the port must be EQUAL. No
// ranges, no wildcards, no suffix matching.
func TestEgressMatchExactHostAndPort(t *testing.T) {
	e := loadEgress(t,
		"    - host: pkg.example.com\n      port: 443\n",
		"    - host: api.internal\n      port: 8443\n      credential: internal-api\n",
	)

	for _, tc := range []struct {
		name string
		host string
		port int
		want bool
	}{
		{"declared host and port", "pkg.example.com", 443, true},
		{"second declared entry", "api.internal", 8443, true},
		{"declared host, undeclared port", "pkg.example.com", 8443, false},
		{"declared host, adjacent port", "pkg.example.com", 444, false},
		{"declared host, port zero", "pkg.example.com", 0, false},
		{"undeclared host, declared port", "evil.example.com", 443, false},
		{"subdomain of a declared host", "sub.pkg.example.com", 443, false},
		{"parent of a declared host", "example.com", 443, false},
		{"declared host as a suffix of the target", "notpkg.example.com", 443, false},
		{"empty host", "", 443, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entry, ok := e.Match(tc.host, tc.port)
			if ok != tc.want {
				t.Fatalf("Match(%q, %d) ok = %v, want %v", tc.host, tc.port, ok, tc.want)
			}
			if ok && (entry.Host != tc.host || entry.Port != tc.port) {
				t.Errorf("Match(%q, %d) = %+v, want the matching entry", tc.host, tc.port, entry)
			}
		})
	}
}

// The matched entry is returned so the proxy can read its optional #52
// credential audience off it (task 5).
func TestEgressMatchReturnsTheMatchedEntry(t *testing.T) {
	e := loadEgress(t,
		"    - host: plain.example.com\n      port: 443\n",
		"    - host: api.internal\n      port: 8443\n      credential: internal-api\n",
	)

	if entry, ok := e.Match("api.internal", 8443); !ok || entry.Credential != "internal-api" {
		t.Errorf("Match(api.internal, 8443) = (%+v, %v), want the entry carrying credential internal-api", entry, ok)
	}
	if entry, ok := e.Match("plain.example.com", 443); !ok || entry.Credential != "" {
		t.Errorf("Match(plain.example.com, 443) = (%+v, %v), want a plain allow-through entry", entry, ok)
	}
}

// CIDR entries match a literal IP destination inside the block, on the exact
// declared port — and nothing else.
func TestEgressMatchCIDR(t *testing.T) {
	e := loadEgress(t,
		"    - host: 10.0.0.0/8\n      port: 5432\n",
		"    - host: 192.0.2.7\n      port: 443\n",
		"    - host: \"2001:db8::/32\"\n      port: 443\n",
	)

	for _, tc := range []struct {
		name string
		host string
		port int
		want bool
	}{
		{"inside the v4 block", "10.1.2.3", 5432, true},
		{"edge of the v4 block", "10.255.255.255", 5432, true},
		{"inside the block, wrong port", "10.1.2.3", 443, false},
		{"outside the v4 block", "11.1.2.3", 5432, false},
		{"bare IP literal, stored as a full mask", "192.0.2.7", 443, true},
		{"neighbour of the bare IP literal", "192.0.2.8", 443, false},
		{"inside the v6 block", "2001:db8::1", 443, true},
		{"alternate v6 spelling of the same address", "2001:0db8:0000:0000:0000:0000:0000:0001", 443, true},
		{"outside the v6 block", "2001:db9::1", 443, false},
		{"not an address at all", "not-an-ip", 5432, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := e.Match(tc.host, tc.port); ok != tc.want {
				t.Errorf("Match(%q, %d) ok = %v, want %v", tc.host, tc.port, ok, tc.want)
			}
		})
	}
}

// A hostname is NEVER resolved to test CIDR membership (plan Q4): DNS is
// attacker-influenced, and resolving would make the decision depend on a lookup
// the policy does not own. "localhost" resolves into 127.0.0.0/8 on every host
// this runs on, and must still be refused by a 127.0.0.0/8 entry.
func TestEgressMatchCIDRNeverResolvesAHostname(t *testing.T) {
	e := loadEgress(t,
		"    - host: 127.0.0.0/8\n      port: 8080\n",
		"    - host: \"::1\"\n      port: 8080\n",
	)

	for _, host := range []string{"localhost", "localhost.localdomain", "ip6-localhost"} {
		if entry, ok := e.Match(host, 8080); ok {
			t.Errorf("Match(%q, 8080) = (%+v, true); a CIDR entry must never match a hostname", host, entry)
		}
	}
	// The same block still matches the literal, so the test above is not
	// passing merely because the entry is inert.
	if _, ok := e.Match("127.0.0.1", 8080); !ok {
		t.Error("Match(127.0.0.1, 8080) = false, want the 127.0.0.0/8 entry to match the literal")
	}
}

// ADR-0048's recorded bug class: two spellings of one destination must reach
// the SAME decision. The declared side is canonicalized at load; the requesting
// side is canonicalized once at the proxy's entry (matchAtEntry). Neither is
// done twice.
func TestEgressMatchSpellingsReachTheSameDecision(t *testing.T) {
	e := loadEgress(t,
		"    - host: PKG.Example.COM.\n      port: 443\n",
		"    - host: \"2001:db8::1\"\n      port: 443\n",
	)

	for _, tc := range []struct {
		name string
		host string
		want bool
	}{
		{"plain", "pkg.example.com", true},
		{"trailing dot", "pkg.example.com.", true},
		{"uppercase", "PKG.EXAMPLE.COM", true},
		{"mixed case and trailing dot", "Pkg.Example.Com.", true},
		{"surrounding space", "  pkg.example.com  ", true},
		{"plain v6 literal", "2001:db8::1", true},
		// The bracketed form is how a CONNECT line spells an IPv6 literal.
		{"bracketed v6 literal", "[2001:db8::1]", true},
		{"bracketed uppercase v6 literal", "[2001:DB8::1]", true},
		{"bracketed alternate v6 spelling", "[2001:0db8:0:0:0:0:0:1]", true},
		{"a different host entirely", "evil.example.com", false},
		{"a different v6 literal", "[2001:db8::2]", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := matchAtEntry(e, tc.host, 443); ok != tc.want {
				t.Errorf("matchAtEntry(%q, 443) ok = %v, want %v", tc.host, ok, tc.want)
			}
		})
	}
}

// Match's contract is that the CALLER already canonicalized (one normalizer,
// at the entry point). This pins that boundary: Match itself does not lowercase
// or strip a trailing dot, so nobody adds a second normalizer here that can
// later drift from reputation.CanonicalHost — which is the ADR-0048 bug.
// Bracket stripping is address-literal syntax, not host normalization, and
// CanonicalHost never touches brackets, so there is nothing to drift.
func TestEgressMatchRequiresTheCallerToCanonicalize(t *testing.T) {
	e := loadEgress(t, "    - host: pkg.example.com\n      port: 443\n")

	for _, host := range []string{"PKG.EXAMPLE.COM", "pkg.example.com.", "Pkg.Example.Com."} {
		if _, ok := e.Match(host, 443); ok {
			t.Errorf("Match(%q, 443) matched; Match must not normalize — the caller canonicalizes once, at the entry", host)
		}
	}
}

// A malformed entry can never reach Match through the loader, but the matcher
// must still refuse to guess if one is constructed directly: an entry with
// neither a Host nor a CIDR matches nothing, and an empty requested host does
// not slot into it.
func TestEgressMatchIgnoresEntriesWithNoDestination(t *testing.T) {
	e := Egress{Mode: EgressEnforce, Allow: []AllowEntry{{Port: 443}}}

	for _, host := range []string{"", "pkg.example.com", "192.0.2.7"} {
		if entry, ok := e.Match(host, 443); ok {
			t.Errorf("Match(%q, 443) = (%+v, true), want no match for an entry with no destination", host, entry)
		}
	}
}

// Every AllowEntry the loader produces holds the invariant Match relies on:
// exactly one of Host or CIDR is set. If that ever stops being true, the
// matcher's two-branch shape is reading a value nobody set.
func TestLoadedAllowEntriesSetExactlyOneDestination(t *testing.T) {
	e := loadEgress(t,
		"    - host: pkg.example.com\n      port: 443\n",
		"    - host: 10.0.0.0/8\n      port: 5432\n",
		"    - host: 192.0.2.7\n      port: 443\n",
		"    - host: \"2001:db8::1\"\n      port: 443\n",
	)

	for i, entry := range e.Allow {
		if (entry.Host == "") == (entry.CIDR == nil) {
			t.Errorf("Allow[%d] = %+v, want exactly one of Host or CIDR set", i, entry)
		}
		if entry.Host != "" && net.ParseIP(entry.Host) != nil {
			t.Errorf("Allow[%d] = %+v, want an IP literal stored as a CIDR, not as a Host", i, entry)
		}
	}
}
