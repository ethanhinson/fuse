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

// loopDetector trips when the recent turns' tool-call sets show a repeating
// cycle. It catches two shapes with the same `limit`:
//
//   - period-1 (AAAA…): the same set repeated `limit` turns in a row.
//   - period-2 (ABAB…): two distinct sets strictly alternating, once the
//     pattern has produced `limit` repeats of a period (2*limit keys).
//
// A history window is kept because the old last+count design reset on every
// change and so could never see ABAB — the alternation perpetually zeroed the
// counter. Legitimate alternating work (e.g. read/write across *different*
// targets) produces distinct fingerprints each turn, so it never collapses to
// just two alternating keys and does not false-trip.
type loopDetector struct {
	limit  int
	window []string
}

func newLoopDetector(limit int) *loopDetector {
	return &loopDetector{limit: limit}
}

// seen records one turn's set of fingerprints and reports whether a period-1 or
// period-2 loop has reached the limit. The set is order-independent.
func (d *loopDetector) seen(fps []string) bool {
	if d.limit <= 0 {
		return false
	}
	d.window = append(d.window, canonical(fps))
	// Cap the retained history at what the widest check needs (period-2 over
	// `limit` repeats = 2*limit keys); older entries can never matter.
	if max := 2 * d.limit; len(d.window) > max {
		d.window = d.window[len(d.window)-max:]
	}
	return d.hasPeriod1() || d.hasPeriod2()
}

// hasPeriod1 reports whether the last `limit` keys are all identical (AAAA…).
func (d *loopDetector) hasPeriod1() bool {
	if len(d.window) < d.limit {
		return false
	}
	tail := d.window[len(d.window)-d.limit:]
	for _, k := range tail[1:] {
		if k != tail[0] {
			return false
		}
	}
	return true
}

// hasPeriod2 reports whether the last 2*limit keys form a strict ABAB… cycle of
// two DISTINCT sets. Requiring A != B keeps period-1 out of this branch (it is
// handled by hasPeriod1) and requiring exactly two alternating keys avoids
// tripping on genuinely varied work.
func (d *loopDetector) hasPeriod2() bool {
	need := 2 * d.limit
	if len(d.window) < need {
		return false
	}
	tail := d.window[len(d.window)-need:]
	a, b := tail[0], tail[1]
	if a == b {
		return false // that's a period-1 run, not an alternation
	}
	for i, k := range tail {
		want := a
		if i%2 == 1 {
			want = b
		}
		if k != want {
			return false
		}
	}
	return true
}

// reset clears the history so the detector needs another full window before
// tripping again. Used after a human approves a "possible loop" force-through so
// the run continues without re-prompting every turn. See change 0038.
func (d *loopDetector) reset() {
	d.window = nil
}

func canonical(fps []string) string {
	cp := append([]string(nil), fps...)
	sort.Strings(cp)
	return strings.Join(cp, ",")
}
