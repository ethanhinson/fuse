// Package agent runs the stateless model/tool loop with loop detection.
package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
)

// fingerprint hashes a tool call (name + raw args) into a stable identifier.
func fingerprint(name, args string) string {
	sum := sha256.Sum256([]byte(name + "\x00" + args))
	return hex.EncodeToString(sum[:8])
}

// loopDetector tracks consecutive repeats of an identical set of tool-call
// fingerprints and trips once the same set has occurred `limit` times in a row.
type loopDetector struct {
	limit int
	last  string
	count int
}

func newLoopDetector(limit int) *loopDetector {
	return &loopDetector{limit: limit}
}

// seen records one turn's set of fingerprints and reports whether the loop
// limit has been reached. The set is order-independent.
func (d *loopDetector) seen(fps []string) bool {
	key := canonical(fps)
	if key == d.last {
		d.count++
	} else {
		d.last = key
		d.count = 1
	}
	return d.count >= d.limit
}

// reset clears the consecutive-repeat state so the detector needs another full
// `limit` run of identical calls before tripping again. Used after a human
// approves a "possible loop" force-through so the run continues without
// re-prompting every turn. See change 0038.
func (d *loopDetector) reset() {
	d.last = ""
	d.count = 0
}

func canonical(fps []string) string {
	cp := append([]string(nil), fps...)
	sort.Strings(cp)
	return strings.Join(cp, ",")
}
