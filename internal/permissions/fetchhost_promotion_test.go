package permissions

import (
	"sort"
	"strings"
	"testing"

	"github.com/ethanhinson/fuse/internal/permissions/reputation"
)

// knownGoodPromotedFromPopularity is the PINNED set of hosts that the bundled
// popularity list (internal/permissions/reputation/data/popularity.csv) is
// currently allowed to promote to a real web_fetch auto-approve — VerdictAllow
// decided by the "known-good" layer, with no classifier call and no prompt.
//
// Every entry here is an AUTHORIZATION GRANT, not a ranking datum: a zero-review
// GET of any path and query string under that host. Because the popularity list
// is refreshed by hand from an upstream top-sites CSV, a refresh would otherwise
// widen this authorization silently. This pin is what makes that widening
// visible: adding rows to the CSV turns the suite red until a human reviews the
// new hosts as grants and updates this list deliberately.
//
// Sorted, and exactly the promoted set — not merely a sample.
var knownGoodPromotedFromPopularity = []string{
	"amazon.com",
	"apple.com",
	"archive.org",
	"cloudflare.com",
	"facebook.com",
	"github.com",
	"google.com",
	"linkedin.com",
	"microsoft.com",
	"mozilla.org",
	"reddit.com",
	"stackoverflow.com",
	"twitter.com",
	"wikipedia.org",
	"youtube.com",
}

// TestKnownGoodPromotion_PinnedSet pins the exact set of popularity-list hosts
// that reach the "known-good" auto-approve. It derives the actual set by running
// every host in the bundled CSV through the real floor, so it measures the
// decision the gate makes rather than the contents of the data file: a CSV row
// that the exfil-shape denylist declines is correctly absent from the pin.
func TestKnownGoodPromotion_PinnedSet(t *testing.T) {
	var promoted []string
	for _, host := range reputation.KnownGoodHosts() {
		got := classifyFetchHost("https://"+host+"/x", nil, nil)
		if got.DecidedBy == "known-good" {
			promoted = append(promoted, host)
		}
	}
	sort.Strings(promoted)

	want := append([]string(nil), knownGoodPromotedFromPopularity...)
	sort.Strings(want)

	added, removed := setDiff(promoted, want)
	if len(added) == 0 && len(removed) == 0 {
		return
	}

	var b strings.Builder
	b.WriteString("the set of popularity-list hosts that AUTO-APPROVE web_fetch has changed.\n\n")
	b.WriteString("Each host below is an authorization grant: a zero-review, zero-prompt GET of\n")
	b.WriteString("any path and query string under that host, with the classifier never consulted.\n\n")
	if len(added) > 0 {
		b.WriteString("NEWLY AUTHORIZED (present now, not in the pinned set):\n")
		for _, h := range added {
			b.WriteString("  + " + h + "\n")
		}
	}
	if len(removed) > 0 {
		b.WriteString("NO LONGER AUTHORIZED (in the pinned set, not promoted now):\n")
		for _, h := range removed {
			b.WriteString("  - " + h + "\n")
		}
	}
	b.WriteString("\nWhat to do:\n")
	b.WriteString("  1. If you refreshed internal/permissions/reputation/data/popularity.csv,\n")
	b.WriteString("     review EVERY newly authorized host above as a security grant, not as a\n")
	b.WriteString("     popularity ranking. Ask whether an attacker-chosen URL under that host\n")
	b.WriteString("     could exfiltrate data or fetch attacker-controlled content.\n")
	b.WriteString("  2. Any host that is a paste/upload service, link shortener, webhook or\n")
	b.WriteString("     request-capture endpoint belongs in exfilShapeDenylist (fetchhost.go),\n")
	b.WriteString("     NOT in this pin — add it there and it will stop being promoted.\n")
	b.WriteString("  3. Only once you have accepted the remaining hosts, update\n")
	b.WriteString("     knownGoodPromotedFromPopularity in this file to match, deliberately.\n")
	b.WriteString("Do not update the pin to silence this failure without doing step 1.")
	t.Fatal(b.String())
}

// setDiff returns the elements of got missing from want, and of want missing
// from got. Both inputs must be sorted.
func setDiff(got, want []string) (added, removed []string) {
	inWant := make(map[string]struct{}, len(want))
	for _, h := range want {
		inWant[h] = struct{}{}
	}
	inGot := make(map[string]struct{}, len(got))
	for _, h := range got {
		inGot[h] = struct{}{}
	}
	for _, h := range got {
		if _, ok := inWant[h]; !ok {
			added = append(added, h)
		}
	}
	for _, h := range want {
		if _, ok := inGot[h]; !ok {
			removed = append(removed, h)
		}
	}
	return added, removed
}

// TestExfilShape_NotPromotedDespitePopularity is the containment proof for the
// pin above: a host can be BOTH a genuine top site (in the popularity CSV) and
// an exfil shape. bit.ly and pastebin.com are exactly that — they are in the
// bundled CSV precisely so this case is exercised — and the promotion path must
// decline them, leaving them to the classifier instead of auto-approving.
func TestExfilShape_NotPromotedDespitePopularity(t *testing.T) {
	for _, host := range []string{"bit.ly", "pastebin.com"} {
		if !reputation.KnownGood(host) {
			t.Fatalf("fixture broken: %q must be in data/popularity.csv for this test to prove anything", host)
		}
		got := classifyFetchHost("https://"+host+"/abc?d=secret", nil, nil)
		if got.DecidedBy == "known-good" {
			t.Errorf("%s: DecidedBy = %q — an exfil-shape host must never auto-approve, even when it is a top site",
				host, got.DecidedBy)
		}
		if got.DecidedBy != "fallthrough" {
			t.Errorf("%s: DecidedBy = %q, want %q (must reach the classifier)", host, got.DecidedBy, "fallthrough")
		}
		if got.AllowNudge {
			t.Errorf("%s: AllowNudge = true — an exfil-shape host must not even bias the classifier toward allow", host)
		}
	}
}

// TestExfilShape_SeedAndDenylistDisjoint holds the invariant that makes the
// subtraction's placement safe to reason about: because the check gates the
// WHOLE promotion (hardcoded seed included, not just the refreshable CSV), a
// host present in both lists would be silently un-seeded. No such host exists
// today, and this test keeps it that way — if a future entry needs to be in
// both, that is a contradiction to resolve deliberately, not to discover in
// production.
func TestExfilShape_SeedAndDenylistDisjoint(t *testing.T) {
	for _, pat := range strongKnownGoodSeed {
		host := strings.TrimPrefix(pat, "*.")
		if exfilShapeMatch(host) {
			t.Errorf("strong seed entry %q is also an exfil shape; the denylist silently revokes it — resolve which list it belongs in", pat)
		}
	}

	// A paste shape under a seeded operator is the near-miss case: the apex is
	// promoted, the paste host must not be, and neither must shade the other.
	if got := classifyFetchHost("https://gist.github.com/x/deadbeef", nil, nil); got.DecidedBy == "known-good" {
		t.Error("gist.github.com: DecidedBy = known-good, want the exfil-shape decline")
	}
	if apex := classifyFetchHost("https://github.com/foo/bar", nil, nil); apex.DecidedBy != "known-good" {
		t.Errorf("github.com: DecidedBy = %q, want known-good (the denylist must not over-reach to the apex)", apex.DecidedBy)
	}
}

// TestExfilShape_OrderingUnchanged proves the subtraction did not disturb the
// layer order: SSRF, config-deny, config-ask and the blocklist all still decide
// before the promotion, and they decide for exfil-shape hosts too.
func TestExfilShape_OrderingUnchanged(t *testing.T) {
	if got := classifyFetchHost("https://bit.ly/x", []string{"bit.ly"}, nil); got.DecidedBy != "config-deny" {
		t.Errorf("DecidedBy = %q, want config-deny (config still decides above the exfil check)", got.DecidedBy)
	}
	if got := classifyFetchHost("https://bit.ly/x", nil, []string{"bit.ly"}); got.DecidedBy != "config-ask" {
		t.Errorf("DecidedBy = %q, want config-ask", got.DecidedBy)
	}
	if got := classifyFetchHost("https://localhost/x", nil, nil); got.DecidedBy != "ssrf" {
		t.Errorf("DecidedBy = %q, want ssrf", got.DecidedBy)
	}
}

// TestExfilShapeMatch_DotBoundary proves the denylist uses dot-boundary
// matching, not substrings: it covers a listed domain and its subdomains, and
// nothing that merely contains the string.
func TestExfilShapeMatch_DotBoundary(t *testing.T) {
	tests := []struct {
		host string
		want bool
	}{
		{"pastebin.com", true},
		{"www.pastebin.com", true},           // subdomain of a listed domain
		{"notpastebin.com", false},           // superstring label, not a boundary
		{"pastebin.com.evil.example", false}, // listed domain as a left label
		{"hooks.slack.com", true},
		{"slack.com", false}, // only the webhook host is listed, not the apex
		{"bit.ly", true},
		{"github.com", false},
		{"gist.github.com", true},
		{"unknown-blog.example", false},
	}
	for _, tt := range tests {
		if got := exfilShapeMatch(tt.host); got != tt.want {
			t.Errorf("exfilShapeMatch(%q) = %v, want %v", tt.host, got, tt.want)
		}
	}
}
