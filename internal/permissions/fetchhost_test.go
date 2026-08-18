package permissions

import (
	"testing"

	"github.com/ethanhinson/fuse/internal/permissions/reputation"
)

// (a) strong-seed hosts and reputation top-sites are a real auto-approve:
// VerdictAllow decided by the "known-good" layer, never reaching the classifier.
func TestClassifyFetchHost_KnownGoodAutoApprove(t *testing.T) {
	cases := []string{
		// strongKnownGoodSeed entries (exact and dot-boundary suffix).
		"https://github.com/foo/bar",
		"https://docs.python.org/3/",
		// operator-minted subdomain namespaces (see a2b for the contrast with
		// open-registration ones like *.github.io).
		"https://en.wikipedia.org/wiki/X",
		// reputation.KnownGood top-sites that are NOT in the hardcoded strong
		// seed (data/popularity.csv).
		"https://google.com/search",
		"https://archive.org/details/x",
	}
	for _, raw := range cases {
		got := classifyFetchHost(raw, nil, nil)
		if got.Verdict != VerdictAllow {
			t.Errorf("%s: Verdict = %v, want VerdictAllow", raw, got.Verdict)
		}
		if got.DecidedBy != "known-good" {
			t.Errorf("%s: DecidedBy = %q, want %q", raw, got.DecidedBy, "known-good")
		}
		if !got.AllowNudge {
			t.Errorf("%s: AllowNudge = false, want true", raw)
		}
	}
}

// (a2) the broad TLD wildcards stay nudge-only: they must fall through to the
// classifier, never auto-approve.
func TestClassifyFetchHost_NudgeOnlyTLDsDoNotAutoApprove(t *testing.T) {
	cases := []string{
		"https://agency.gov/x",
		"https://mit.edu/x",
		"https://someproject.dev/x",
	}
	for _, raw := range cases {
		got := classifyFetchHost(raw, nil, nil)
		if got.Verdict != VerdictAllow {
			t.Errorf("%s: Verdict = %v, want VerdictAllow (fallthrough sentinel)", raw, got.Verdict)
		}
		if got.DecidedBy != "fallthrough" {
			t.Errorf("%s: DecidedBy = %q, want %q (TLD wildcards must not auto-approve)", raw, got.DecidedBy, "fallthrough")
		}
		if !got.AllowNudge {
			t.Errorf("%s: AllowNudge = false, want true (still a bias hint)", raw)
		}
	}
}

// (a2b) open-registration user-content namespaces stay nudge-only. Anyone can
// claim attacker.github.io or attacker.readthedocs.io in under a minute, so the
// named operator (GitHub, Read the Docs) controls the HOSTING, not the hostname
// namespace — and the floor decides on the host alone, which would make
// https://attacker.github.io/x?d=<data> a zero-review GET whose query string
// lands in an attacker's log. They must fall through to the classifier.
func TestClassifyFetchHost_UserContentNamespacesDoNotAutoApprove(t *testing.T) {
	cases := []string{
		"https://attacker.github.io/x",
		"https://attacker.readthedocs.io/en/latest/",
	}
	for _, raw := range cases {
		got := classifyFetchHost(raw, nil, nil)
		if got.Verdict != VerdictAllow {
			t.Errorf("%s: Verdict = %v, want VerdictAllow (fallthrough sentinel)", raw, got.Verdict)
		}
		if got.DecidedBy != "fallthrough" {
			t.Errorf("%s: DecidedBy = %q, want %q (open-registration user content must not auto-approve)",
				raw, got.DecidedBy, "fallthrough")
		}
		if !got.AllowNudge {
			t.Errorf("%s: AllowNudge = false, want true (still a bias hint)", raw)
		}
	}

	// they are out of the strong seed entirely, but stay in the union.
	for _, host := range []string{"attacker.github.io", "attacker.readthedocs.io"} {
		if strongSeedMatch(host) {
			t.Errorf("strongSeedMatch(%q) = true, want false", host)
		}
		if !knownGoodSeedMatch(host) {
			t.Errorf("knownGoodSeedMatch(%q) = false, want true (nudge preserved)", host)
		}
	}

	// operator-run hosts — the apex GitHub site, official docs, and the
	// operator-controlled subdomain namespaces of Wikipedia and Stack Exchange —
	// keep their real auto-approve.
	for _, raw := range []string{
		"https://github.com/foo/bar",
		"https://docs.python.org/3/",
		"https://en.wikipedia.org/wiki/X",
		"https://math.stackexchange.com/questions/1",
	} {
		got := classifyFetchHost(raw, nil, nil)
		if got.Verdict != VerdictAllow || got.DecidedBy != "known-good" || !got.AllowNudge {
			t.Errorf("%s: Verdict=%v DecidedBy=%q AllowNudge=%v, want VerdictAllow/known-good/true",
				raw, got.Verdict, got.DecidedBy, got.AllowNudge)
		}
	}
}

// (a2c) a credential-bearing URL never auto-approves, however strong the host.
// Userinfo ("https://<token>@github.com/x") is an exfiltration / credential-leak
// shape that lives in the URL, not the host — and the floor is the ONLY layer
// that can see it: the web_fetch classifier prompt is shown the host alone, so
// falling through would hand the decision to a judge that is blind to the very
// property that triggered it. The floor therefore decides: VerdictAsk with
// DecidedBy "credentialed-url", and no AllowNudge.
func TestClassifyFetchHost_CredentialedURLDoesNotAutoApprove(t *testing.T) {
	cases := []string{
		// token-only userinfo on a strong-seed host.
		"https://sometoken@github.com/x",
		// user:password form on a strong-seed host.
		"https://user:password@github.com/x",
		// dot-boundary subdomain of an operator-minted seed namespace.
		"https://tok@en.wikipedia.org/wiki/X",
		// a reputation.KnownGood top-site that is not in the hardcoded seed.
		"https://tok@google.com/search",
		"https://user:pw@archive.org/details/x",
	}
	for _, raw := range cases {
		got := classifyFetchHost(raw, nil, nil)
		if got.DecidedBy == "known-good" {
			t.Errorf("%s: DecidedBy = %q — a credential-bearing URL must never auto-approve", raw, got.DecidedBy)
		}
		if got.Verdict != VerdictAsk || got.DecidedBy != "credentialed-url" {
			t.Errorf("%s: Verdict=%v DecidedBy=%q, want VerdictAsk/credentialed-url",
				raw, got.Verdict, got.DecidedBy)
		}
		if got.AllowNudge {
			t.Errorf("%s: AllowNudge = true — a reputable host name says nothing about a URL carrying a secret", raw)
		}
	}

	// No regression: the same hosts, credential-free, still auto-approve.
	for _, raw := range []string{
		"https://github.com/x",
		"https://en.wikipedia.org/wiki/X",
		"https://google.com/search",
		"https://archive.org/details/x",
	} {
		got := classifyFetchHost(raw, nil, nil)
		if got.Verdict != VerdictAllow || got.DecidedBy != "known-good" || !got.AllowNudge {
			t.Errorf("%s: Verdict=%v DecidedBy=%q AllowNudge=%v, want VerdictAllow/known-good/true",
				raw, got.Verdict, got.DecidedBy, got.AllowNudge)
		}
	}
}

// (a2d) the credentialed-URL ask is an ASK, not a new top layer: every denying
// layer above it still decides first, and the canonical host is still reported.
func TestClassifyFetchHost_CredentialedURLDoesNotOutrankDenies(t *testing.T) {
	if got := classifyFetchHost("http://tok@127.0.0.1/x", nil, nil); got.DecidedBy != "ssrf" || got.Verdict != VerdictDeny {
		t.Errorf("credentialed SSRF: Verdict=%v DecidedBy=%q, want VerdictDeny/ssrf", got.Verdict, got.DecidedBy)
	}
	if got := classifyFetchHost("https://tok@github.com/x", []string{"github.com"}, nil); got.DecidedBy != "config-deny" {
		t.Errorf("credentialed fetch_deny host: DecidedBy = %q, want config-deny", got.DecidedBy)
	}
	if got := classifyFetchHost("https://tok@GitHub.com./x", nil, nil); got.Host != "github.com" {
		t.Errorf("credentialed ask should still carry the canonical host, got %q", got.Host)
	}
	// A credentialed URL to an otherwise-unremarkable host asks too: the shape is
	// host-independent, so it cannot be a property of the known-good set alone.
	if got := classifyFetchHost("https://tok@unknown-blog.example/post", nil, nil); got.Verdict != VerdictAsk || got.DecidedBy != "credentialed-url" {
		t.Errorf("credentialed unknown host: Verdict=%v DecidedBy=%q, want VerdictAsk/credentialed-url", got.Verdict, got.DecidedBy)
	}
}

// (a3) an unrecognized host is neither seeded nor a top-site: it still falls
// through to the classifier with no nudge.
func TestClassifyFetchHost_UnknownFallthrough(t *testing.T) {
	got := classifyFetchHost("https://unknown-blog.example/post", nil, nil)
	if got.Verdict != VerdictAllow {
		t.Errorf("Verdict = %v, want VerdictAllow (fallthrough sentinel)", got.Verdict)
	}
	if got.DecidedBy != "fallthrough" {
		t.Errorf("DecidedBy = %q, want %q", got.DecidedBy, "fallthrough")
	}
	if got.AllowNudge {
		t.Error("AllowNudge = true, want false")
	}
}

// (a4) config always beats the known-good promotion: a configured fetch_deny or
// fetch_ask of a strong-seed host still decides.
func TestClassifyFetchHost_ConfigBeatsKnownGood(t *testing.T) {
	deny := classifyFetchHost("https://github.com/x", []string{"github.com"}, nil)
	if deny.Verdict != VerdictDeny || deny.DecidedBy != "config-deny" {
		t.Errorf("fetch_deny of a seeded host: Verdict=%v DecidedBy=%q, want VerdictDeny/config-deny",
			deny.Verdict, deny.DecidedBy)
	}

	ask := classifyFetchHost("https://github.com/x", nil, []string{"github.com"})
	if ask.Verdict != VerdictAsk || ask.DecidedBy != "config-ask" {
		t.Errorf("fetch_ask of a seeded host: Verdict=%v DecidedBy=%q, want VerdictAsk/config-ask",
			ask.Verdict, ask.DecidedBy)
	}

	// Same for a reputation top-site that is not in the hardcoded seed.
	repDeny := classifyFetchHost("https://google.com/x", []string{"google.com"}, nil)
	if repDeny.Verdict != VerdictDeny || repDeny.DecidedBy != "config-deny" {
		t.Errorf("fetch_deny of a top-site: Verdict=%v DecidedBy=%q, want VerdictDeny/config-deny",
			repDeny.Verdict, repDeny.DecidedBy)
	}
}

// (b) SSRF targets deny with DecidedBy "ssrf".
func TestClassifyFetchHost_SSRFDeny(t *testing.T) {
	cases := []string{
		"http://127.0.0.1/x",
		"http://localhost/x",
		"http://10.1.2.3/",
		"http://192.168.0.5/",
		"http://172.16.9.9/",
		"http://169.254.1.1/",
		"http://[::1]/",
	}
	for _, raw := range cases {
		got := classifyFetchHost(raw, nil, nil)
		if got.Verdict != VerdictDeny {
			t.Errorf("%s: Verdict = %v, want VerdictDeny", raw, got.Verdict)
		}
		if got.DecidedBy != "ssrf" {
			t.Errorf("%s: DecidedBy = %q, want %q", raw, got.DecidedBy, "ssrf")
		}
	}
}

// (c) a reputation-blocklisted host denies with DecidedBy "blocklist".
// malware-test.example is in data/blocklist-hosts.txt (Task 3 fixture).
func TestClassifyFetchHost_BlocklistDeny(t *testing.T) {
	got := classifyFetchHost("https://malware-test.example/payload", nil, nil)
	if got.Verdict != VerdictDeny {
		t.Errorf("Verdict = %v, want VerdictDeny", got.Verdict)
	}
	if got.DecidedBy != "blocklist" {
		t.Errorf("DecidedBy = %q, want %q", got.DecidedBy, "blocklist")
	}
}

// (d) config deny/ask globs, and deny beats ask when both match.
func TestClassifyFetchHost_ConfigGlobs(t *testing.T) {
	deny := classifyFetchHost("http://sub.evil.example/", []string{"*.evil.example"}, nil)
	if deny.Verdict != VerdictDeny || deny.DecidedBy != "config-deny" {
		t.Errorf("deny glob: Verdict=%v DecidedBy=%q, want VerdictDeny/config-deny", deny.Verdict, deny.DecidedBy)
	}

	ask := classifyFetchHost("http://x.ask.example/", nil, []string{"*.ask.example"})
	if ask.Verdict != VerdictAsk || ask.DecidedBy != "config-ask" {
		t.Errorf("ask glob: Verdict=%v DecidedBy=%q, want VerdictAsk/config-ask", ask.Verdict, ask.DecidedBy)
	}

	// deny beats ask when a host matches both lists.
	both := classifyFetchHost("http://host.both.example/", []string{"*.both.example"}, []string{"*.both.example"})
	if both.Verdict != VerdictDeny || both.DecidedBy != "config-deny" {
		t.Errorf("deny-beats-ask: Verdict=%v DecidedBy=%q, want VerdictDeny/config-deny", both.Verdict, both.DecidedBy)
	}
}

// (e) malformed / hostless URLs ask with DecidedBy "malformed-url".
func TestClassifyFetchHost_MalformedAsk(t *testing.T) {
	cases := []string{
		"http://%zz",
		"::::",
		"",
	}
	for _, raw := range cases {
		got := classifyFetchHost(raw, nil, nil)
		if got.Verdict != VerdictAsk {
			t.Errorf("%q: Verdict = %v, want VerdictAsk", raw, got.Verdict)
		}
		if got.DecidedBy != "malformed-url" {
			t.Errorf("%q: DecidedBy = %q, want %q", raw, got.DecidedBy, "malformed-url")
		}
	}
}

// (f) spoof hosts must NOT be nudged and must fall through (not deny).
func TestClassifyFetchHost_SpoofNoNudge(t *testing.T) {
	cases := []string{
		"https://notgithub.com/x",
		"https://github.com.evil.example/x",
	}
	for _, raw := range cases {
		got := classifyFetchHost(raw, nil, nil)
		if got.Verdict != VerdictAllow {
			t.Errorf("%s: Verdict = %v, want VerdictAllow (fallthrough)", raw, got.Verdict)
		}
		if got.DecidedBy != "fallthrough" {
			t.Errorf("%s: DecidedBy = %q, want %q", raw, got.DecidedBy, "fallthrough")
		}
		if got.AllowNudge {
			t.Errorf("%s: AllowNudge = true, want false (spoof must not be nudged)", raw)
		}
	}
}

// (g) the FQDN (trailing-dot) spelling of a host must be canonicalized before
// every layer. url.Hostname preserves the root dot, so "google.com." would
// otherwise miss path.Match/strongSeedMatch in the denying layers while still
// matching reputation.KnownGood (which normalizes) — turning a one-character
// mutation into a zero-classifier auto-approve past a configured fetch_deny.
func TestClassifyFetchHost_TrailingDotCanonicalized(t *testing.T) {
	// config-deny still decides for the FQDN spelling.
	deny := classifyFetchHost("https://google.com./?q=secret", []string{"google.com"}, nil)
	if deny.Verdict != VerdictDeny || deny.DecidedBy != "config-deny" {
		t.Errorf("fetch_deny vs FQDN spelling: Verdict=%v DecidedBy=%q, want VerdictDeny/config-deny",
			deny.Verdict, deny.DecidedBy)
	}
	if deny.Host != "google.com" {
		t.Errorf("deny.Host = %q, want canonical %q", deny.Host, "google.com")
	}

	// same for a strong-seed host under fetch_deny.
	seedDeny := classifyFetchHost("https://github.com./x", []string{"github.com"}, nil)
	if seedDeny.Verdict != VerdictDeny || seedDeny.DecidedBy != "config-deny" {
		t.Errorf("fetch_deny vs seeded FQDN: Verdict=%v DecidedBy=%q, want VerdictDeny/config-deny",
			seedDeny.Verdict, seedDeny.DecidedBy)
	}

	// config-ask still decides for the FQDN spelling.
	ask := classifyFetchHost("https://google.com./x", nil, []string{"google.com"})
	if ask.Verdict != VerdictAsk || ask.DecidedBy != "config-ask" {
		t.Errorf("fetch_ask vs FQDN spelling: Verdict=%v DecidedBy=%q, want VerdictAsk/config-ask",
			ask.Verdict, ask.DecidedBy)
	}

	// SSRF sees through the root dot too.
	ssrf := []string{"http://localhost./admin", "http://127.0.0.1./x"}
	for _, raw := range ssrf {
		got := classifyFetchHost(raw, nil, nil)
		if got.Verdict != VerdictDeny || got.DecidedBy != "ssrf" {
			t.Errorf("%s: Verdict=%v DecidedBy=%q, want VerdictDeny/ssrf", raw, got.Verdict, got.DecidedBy)
		}
	}

	// blocklist sees through the root dot.
	blocked := classifyFetchHost("https://malware-test.example./payload", nil, nil)
	if blocked.Verdict != VerdictDeny || blocked.DecidedBy != "blocklist" {
		t.Errorf("blocklist vs FQDN spelling: Verdict=%v DecidedBy=%q, want VerdictDeny/blocklist",
			blocked.Verdict, blocked.DecidedBy)
	}

	// canonicalization must not break the happy path: an unconfigured
	// known-good host still auto-approves in its FQDN spelling.
	for _, raw := range []string{"https://github.com./foo", "https://google.com./search"} {
		good := classifyFetchHost(raw, nil, nil)
		if good.Verdict != VerdictAllow || good.DecidedBy != "known-good" || !good.AllowNudge {
			t.Errorf("%s: Verdict=%v DecidedBy=%q AllowNudge=%v, want VerdictAllow/known-good/true",
				raw, good.Verdict, good.DecidedBy, good.AllowNudge)
		}
	}

	// a host that is nothing but the root dot canonicalizes to empty and must
	// be treated as hostless, not as a non-empty host.
	dot := classifyFetchHost("https://./x", nil, nil)
	if dot.Verdict != VerdictAsk || dot.DecidedBy != "malformed-url" {
		t.Errorf("bare-dot host: Verdict=%v DecidedBy=%q, want VerdictAsk/malformed-url", dot.Verdict, dot.DecidedBy)
	}
	if dot.Host != "" {
		t.Errorf("bare-dot host: Host = %q, want empty", dot.Host)
	}
}

// the floor's canonicalization is reputation.CanonicalHost itself — the same
// function reputation.Blocked/KnownGood normalize with — so the deciding layers
// and the reputation lookups cannot drift apart again.
func TestFetchFloorUsesReputationCanonicalHost(t *testing.T) {
	tests := []struct{ in, want string }{
		{"GitHub.Com.", "github.com"},
		{"github.com", "github.com"},
		{"LOCALHOST.", "localhost"},
		{".", ""},
		{"", ""},
	}
	for _, tt := range tests {
		if got := reputation.CanonicalHost(tt.in); got != tt.want {
			t.Errorf("reputation.CanonicalHost(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// strongSeedMatch covers only entries whose named operator controls the whole
// hostname namespace the pattern spans — never the broad TLD wildcards, and
// never the open-registration user-content namespaces. Both stay nudge-only and
// can never auto-approve.
func TestStrongSeedMatch(t *testing.T) {
	tests := []struct {
		host string
		want bool
	}{
		{"github.com", true},
		{"docs.python.org", true},
		{"en.wikipedia.org", true},
		{"math.stackexchange.com", true},
		{"agency.gov", false},
		{"mit.edu", false},
		{"foo.dev", false},
		// open-registration user-content namespaces are nudge-only, not strong.
		{"user.github.io", false},
		{"someproject.readthedocs.io", false},
		// dot-boundary spoofs match nothing.
		{"notgithub.com", false},
		{"github.com.evil.example", false},
	}
	for _, tt := range tests {
		if got := strongSeedMatch(tt.host); got != tt.want {
			t.Errorf("strongSeedMatch(%q) = %v, want %v", tt.host, got, tt.want)
		}
	}
}

// knownGoodSeedMatch stays the union of both sets, so the AllowNudge surface
// is unchanged by the split.
func TestKnownGoodSeedMatch_UnionUnchanged(t *testing.T) {
	tests := []struct {
		host string
		want bool
	}{
		{"github.com", true},
		{"docs.python.org", true},
		{"agency.gov", true},
		{"mit.edu", true},
		{"foo.dev", true},
		{"user.github.io", true},
		{"someproject.readthedocs.io", true},
		{"notgithub.com", false},
		{"github.com.evil.example", false},
		{"evil.gov.attacker.com", false},
	}
	for _, tt := range tests {
		if got := knownGoodSeedMatch(tt.host); got != tt.want {
			t.Errorf("knownGoodSeedMatch(%q) = %v, want %v", tt.host, got, tt.want)
		}
	}
}

// hostMatchesSuffix enforces real dot-boundary suffix semantics.
func TestHostMatchesSuffix(t *testing.T) {
	tests := []struct {
		host, pattern string
		want          bool
	}{
		{"github.com", "github.com", true},
		{"notgithub.com", "github.com", false},
		{"user.github.io", "*.github.io", true},
		{"github.io", "*.github.io", false}, // bare apex does not match "*.github.io"
		{"agency.gov", "*.gov", true},
		{"evil.gov.attacker.com", "*.gov", false},
		{"github.com.evil.example", "github.com", false},
	}
	for _, tt := range tests {
		if got := hostMatchesSuffix(tt.host, tt.pattern); got != tt.want {
			t.Errorf("hostMatchesSuffix(%q, %q) = %v, want %v", tt.host, tt.pattern, got, tt.want)
		}
	}
}
