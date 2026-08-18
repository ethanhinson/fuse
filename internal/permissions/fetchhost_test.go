package permissions

import "testing"

// (a) well-known hardcoded-seed hosts fall through with an allow nudge.
func TestClassifyFetchHost_WellKnownFallthroughNudge(t *testing.T) {
	cases := []string{
		"https://github.com/foo/bar",
		"https://docs.python.org/3/",
	}
	for _, raw := range cases {
		got := classifyFetchHost(raw, nil, nil)
		if got.Verdict != VerdictAllow {
			t.Errorf("%s: Verdict = %v, want VerdictAllow (fallthrough)", raw, got.Verdict)
		}
		if got.DecidedBy != "fallthrough" {
			t.Errorf("%s: DecidedBy = %q, want %q", raw, got.DecidedBy, "fallthrough")
		}
		if !got.AllowNudge {
			t.Errorf("%s: AllowNudge = false, want true", raw)
		}
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

// strongSeedMatch covers only the exact/suffix host entries — never the broad
// TLD wildcards, which must stay nudge-only and can never auto-approve.
func TestStrongSeedMatch(t *testing.T) {
	tests := []struct {
		host string
		want bool
	}{
		{"github.com", true},
		{"docs.python.org", true},
		{"user.github.io", true},
		{"agency.gov", false},
		{"mit.edu", false},
		{"foo.dev", false},
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
