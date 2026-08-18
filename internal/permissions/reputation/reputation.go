// Package reputation provides an embedded, offline host-reputation layer for
// the permissions gate. It answers two independent questions about a host:
//
//   - Blocked(host): is the host a known-bad domain (malware / phishing /
//     ads / trackers) that should be denied outright?
//   - KnownGood(host): is the host among a curated popularity allowlist,
//     usable as a "known-good" nudge in the classifier?
//
// All data is bundled as small pinned fixture snapshots via //go:embed. No
// network access occurs at build time or run time. See generate.go for the
// upstream sources and licensing.
//
// Licensing: MIT-only for code and data. The blocklist snapshots derive from
// StevenBlack/hosts (MIT) and Peter Lowe's list; the popularity allowlist
// derives from Majestic Million (CC BY 3.0). No GPL-licensed data is shipped.
package reputation

import (
	"bufio"
	"bytes"
	"embed"
	"encoding/csv"
	"strings"
	"sync"
)

// blocklistFS holds the two blocklist snapshots.
//
//go:embed data/blocklist-hosts.txt data/blocklist-domains.txt
var blocklistFS embed.FS

// popularityFS holds the Majestic-Million-style popularity snapshot.
//
//go:embed data/popularity.csv
var popularityFS embed.FS

// licensesFS holds the license / attribution texts that must ship with the
// bundled data.
//
//go:embed data/LICENSE-StevenBlack-MIT.txt data/LICENSE-PeterLowe.txt data/LICENSE-MajesticMillion-CCBY3.txt
var licensesFS embed.FS

var (
	blockOnce sync.Once
	blockSet  map[string]struct{}

	goodOnce sync.Once
	goodSet  map[string]struct{}
)

// CanonicalHost lowercases the host and strips a single trailing root dot so
// that "GitHub.Com." and "github.com" compare equal. It is the canonical host
// spelling used by every lookup in this package (Blocked, KnownGood, and the
// data loaders).
//
// It is exported because callers that gate on a host — notably the permissions
// web_fetch floor — MUST canonicalize with exactly this function before their
// own deny/ask matching. If a gate matches on a raw url.Hostname() while these
// lookups normalize, the two disagree on the FQDN spelling ("google.com.") and
// a denying layer can be skipped while KnownGood still answers true. Sharing
// one function makes that drift impossible; do not reimplement it.
//
// Note the empty result for "." (and for ""): callers must treat an empty
// canonical host as "no host", not as a host that matched nothing.
func CanonicalHost(host string) string {
	h := strings.ToLower(strings.TrimSpace(host))
	h = strings.TrimSuffix(h, ".")
	return h
}

// normalize is the internal alias for CanonicalHost.
func normalize(host string) string { return CanonicalHost(host) }

// loadBlocklist parses the union of both blocklist files into blockSet.
//
// The hosts-format file (StevenBlack style) has lines of the form
// "0.0.0.0 domain"; we take field[1]. The one-domain-per-line file (Peter
// Lowe style) has a bare domain per line. In both, blank lines and lines
// beginning with '#' are skipped.
func loadBlocklist() {
	blockSet = make(map[string]struct{})

	if b, err := blocklistFS.ReadFile("data/blocklist-hosts.txt"); err == nil {
		sc := bufio.NewScanner(bytes.NewReader(b))
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			if d := normalize(fields[1]); d != "" {
				blockSet[d] = struct{}{}
			}
		}
	}

	if b, err := blocklistFS.ReadFile("data/blocklist-domains.txt"); err == nil {
		sc := bufio.NewScanner(bytes.NewReader(b))
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			// Defensively take the first field in case of trailing tokens.
			fields := strings.Fields(line)
			if len(fields) == 0 {
				continue
			}
			if d := normalize(fields[0]); d != "" {
				blockSet[d] = struct{}{}
			}
		}
	}
}

// loadPopularity parses the Domain column (looked up by header name, not a
// fixed index) of the popularity CSV into goodSet.
func loadPopularity() {
	goodSet = make(map[string]struct{})

	b, err := popularityFS.ReadFile("data/popularity.csv")
	if err != nil {
		return
	}
	r := csv.NewReader(bytes.NewReader(b))
	r.FieldsPerRecord = -1 // tolerate ragged rows

	header, err := r.Read()
	if err != nil {
		return
	}
	domainCol := -1
	for i, col := range header {
		if strings.EqualFold(strings.TrimSpace(col), "Domain") {
			domainCol = i
			break
		}
	}
	if domainCol < 0 {
		return
	}

	for {
		rec, err := r.Read()
		if err != nil {
			break
		}
		if domainCol >= len(rec) {
			continue
		}
		if d := normalize(rec[domainCol]); d != "" {
			goodSet[d] = struct{}{}
		}
	}
}

// Blocked reports whether host is an exact member of the parsed blocklist
// (the union of both bundled files). The host is normalized (lowercased,
// trailing dot stripped) before lookup. Membership is exact — there is no
// substring or subdomain matching.
func Blocked(host string) bool {
	blockOnce.Do(loadBlocklist)
	h := normalize(host)
	if h == "" {
		return false
	}
	_, ok := blockSet[h]
	return ok
}

// KnownGood reports whether host is an exact member of the popularity
// allowlist (the Domain column of the bundled CSV). The host is normalized
// before lookup.
func KnownGood(host string) bool {
	goodOnce.Do(loadPopularity)
	h := normalize(host)
	if h == "" {
		return false
	}
	_, ok := goodSet[h]
	return ok
}
