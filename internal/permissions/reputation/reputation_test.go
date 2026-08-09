package reputation

import (
	"io/fs"
	"testing"
)

func TestBlockedParsesNonEmpty(t *testing.T) {
	// Loading the blocklist must yield a non-empty set. We probe indirectly
	// through Blocked by asserting at least one known fixture is present.
	if !Blocked("malware-test.example") {
		t.Fatal("expected blocklist to be non-empty and contain malware-test.example")
	}
}

func TestBlockedKnownBad(t *testing.T) {
	bad := []string{
		"malware-test.example",   // from hosts-format file
		"phishing-test.example",  // from hosts-format file
		"telemetry-collector.example", // hosts file, after a comment block
		"ad-broker.example",      // from one-domain-per-line file
		"data-broker.example",    // from one-domain-per-line file
	}
	for _, h := range bad {
		if !Blocked(h) {
			t.Errorf("Blocked(%q) = false, want true", h)
		}
	}
}

func TestBlockedUnknownHostFalse(t *testing.T) {
	if Blocked("some-random-unlisted-host.invalid") {
		t.Error("Blocked returned true for an unlisted host")
	}
}

func TestBlockedNoSubstringMatching(t *testing.T) {
	// A subdomain / superstring of a blocked host must NOT match: positive
	// membership only, no substring matching.
	if Blocked("sub.malware-test.example") {
		t.Error("Blocked matched a subdomain of a blocked host; must be exact-membership only")
	}
	if Blocked("malware-test.example.evil.invalid") {
		t.Error("Blocked matched a superstring of a blocked host; must be exact-membership only")
	}
}

func TestKnownGoodParsesNonEmpty(t *testing.T) {
	if !KnownGood("google.com") {
		t.Fatal("expected popularity list to be non-empty and contain google.com")
	}
}

func TestKnownGoodKnownFixtures(t *testing.T) {
	good := []string{"github.com", "wikipedia.org", "stackoverflow.com", "cloudflare.com"}
	for _, h := range good {
		if !KnownGood(h) {
			t.Errorf("KnownGood(%q) = false, want true", h)
		}
	}
}

func TestKnownGoodUnknownFalse(t *testing.T) {
	if KnownGood("some-random-unlisted-host.invalid") {
		t.Error("KnownGood returned true for an unlisted host")
	}
}

func TestNormalizationBlocked(t *testing.T) {
	variants := []string{
		"MALWARE-TEST.EXAMPLE",  // uppercase
		"malware-test.example.", // trailing dot
		"MalWare-Test.Example.", // mixed case + trailing dot
	}
	for _, h := range variants {
		if !Blocked(h) {
			t.Errorf("Blocked(%q) = false; normalization should resolve to malware-test.example", h)
		}
	}
}

func TestNormalizationKnownGood(t *testing.T) {
	variants := []string{"GITHUB.COM", "github.com.", "GitHub.Com."}
	for _, h := range variants {
		if !KnownGood(h) {
			t.Errorf("KnownGood(%q) = false; normalization should resolve to github.com", h)
		}
	}
}

func TestLicenseFilesEmbeddedNonEmpty(t *testing.T) {
	names := []string{
		"data/LICENSE-StevenBlack-MIT.txt",
		"data/LICENSE-PeterLowe.txt",
		"data/LICENSE-MajesticMillion-CCBY3.txt",
	}
	for _, name := range names {
		b, err := fs.ReadFile(licensesFS, name)
		if err != nil {
			t.Errorf("license %q not embedded: %v", name, err)
			continue
		}
		if len(b) == 0 {
			t.Errorf("license %q is empty", name)
		}
	}
}
