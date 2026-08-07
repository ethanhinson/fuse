package tui

import (
	"fmt"
	"sort"
	"strings"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/ratelimit"
)

// scheduler_summary renders the change 0036 observability counters — per-pool
// slots/queue/token spend and per-provider rate-gate utilization — into the
// fixed-width machine-built strings the status bar and agents overlay show. No
// model bytes reach these strings; every field is an integer counter, so the
// output is stable and short (no sanitization beyond omitting unset axes).

// compactTokens renders a token count as a short human figure: exact under 1000,
// then "Nk" for thousands and "N.Mm" for millions. It keeps the segment narrow
// (e.g. "310k") so several pools fit a status line.
func compactTokens(n int) string {
	switch {
	case n < 1000:
		return fmt.Sprintf("%d", n)
	case n < 1_000_000:
		return fmt.Sprintf("%dk", n/1000)
	default:
		return fmt.Sprintf("%.1fm", float64(n)/1_000_000)
	}
}

// poolSummaryLine renders one pool's counters in the spec shape
// `research 3/5 slots · 2 queued · 310k/500k tokens`, omitting any axis that is
// unset (SlotTotal 0 ⇒ no slots segment, Queued 0 ⇒ no queued segment,
// TokenQuota 0 ⇒ no tokens segment) so an idle or unlimited axis adds no noise.
// The pool label is the workflow name, or "session" for the implicit pool.
func poolSummaryLine(p agent.PoolSnapshot) string {
	label := p.Workflow
	if label == "" {
		label = "session"
	}
	var segs []string
	if p.SlotTotal > 0 {
		segs = append(segs, fmt.Sprintf("%d/%d slots", p.SlotsInUse, p.SlotTotal))
	}
	if p.Queued > 0 {
		segs = append(segs, fmt.Sprintf("%d queued", p.Queued))
	}
	if p.TokenQuota > 0 {
		segs = append(segs, fmt.Sprintf("%s/%s tokens", compactTokens(p.TokenSpend), compactTokens(p.TokenQuota)))
	}
	if len(segs) == 0 {
		return ""
	}
	return label + " " + strings.Join(segs, " · ")
}

// rateGateLine renders one provider's rate-gate remaining/cap in the shape
// `deepseek 5/6 rpm · 500/600 tpm`, omitting an unlimited (cap 0) axis. The
// provider label is the resolved key, or "global" for the pure-global axes.
func rateGateLine(u ratelimit.ProviderUtilization) string {
	label := u.Provider
	if label == "" {
		label = "global"
	}
	var segs []string
	if u.RPMCap > 0 {
		segs = append(segs, fmt.Sprintf("%d/%d rpm", u.RPMRemaining, u.RPMCap))
	}
	if u.TPMCap > 0 {
		segs = append(segs, fmt.Sprintf("%s/%s tpm", compactTokens(u.TPMRemaining), compactTokens(u.TPMCap)))
	}
	if len(segs) == 0 {
		return ""
	}
	return label + " " + strings.Join(segs, " · ")
}

// schedulerSummaryLines renders the full multi-line observability block for the
// agents overlay: one line per non-empty pool (workflow pools first by name, the
// implicit session pool last), then one line per rate-gate provider. Empty when
// nothing is engaged. util may be nil (no rate gate configured).
func schedulerSummaryLines(snap agent.SchedulerSnapshot, util []ratelimit.ProviderUtilization) []string {
	pools := append([]agent.PoolSnapshot(nil), snap.Pools...)
	// Workflow pools sorted by name; the implicit session pool ("") sorts last.
	sort.SliceStable(pools, func(i, j int) bool {
		if (pools[i].Workflow == "") != (pools[j].Workflow == "") {
			return pools[j].Workflow == "" // non-implicit before implicit
		}
		return pools[i].Workflow < pools[j].Workflow
	})
	var out []string
	for _, p := range pools {
		if line := poolSummaryLine(p); line != "" {
			out = append(out, line)
		}
	}
	provs := append([]ratelimit.ProviderUtilization(nil), util...)
	sort.SliceStable(provs, func(i, j int) bool { return provs[i].Provider < provs[j].Provider })
	for _, u := range provs {
		if line := rateGateLine(u); line != "" {
			out = append(out, line)
		}
	}
	return out
}

// schedulerStatusSegment renders the compact single-segment scheduler summary for
// the status bar: the busiest engaged pool's line. "Busiest" is the pool with the
// most SlotsInUse, ties broken by more Queued, then by name (the implicit session
// pool sorts last, matching the overlay). Only pools with a renderable axis
// (slots/queue/token) are considered; empty when none is engaged. Kept to one
// pool so the status line stays narrow — the full per-pool block lives in the
// agents overlay.
func schedulerStatusSegment(snap agent.SchedulerSnapshot) string {
	best := -1
	for i, p := range snap.Pools {
		if poolSummaryLine(p) == "" {
			continue // not engaged: nothing to render
		}
		if best < 0 || busierPool(p, snap.Pools[best]) {
			best = i
		}
	}
	if best < 0 {
		return ""
	}
	return poolSummaryLine(snap.Pools[best])
}

// busierPool reports whether a is the busier pool to surface than b: more
// SlotsInUse wins; on a tie, more Queued; on a further tie, the earlier name
// (with the implicit session pool "" sorting last, mirroring the overlay order).
func busierPool(a, b agent.PoolSnapshot) bool {
	if a.SlotsInUse != b.SlotsInUse {
		return a.SlotsInUse > b.SlotsInUse
	}
	if a.Queued != b.Queued {
		return a.Queued > b.Queued
	}
	// Name tiebreak: implicit session pool ("") sorts last; otherwise lexical.
	if (a.Workflow == "") != (b.Workflow == "") {
		return b.Workflow == "" // a non-implicit beats the implicit pool
	}
	return a.Workflow < b.Workflow
}
