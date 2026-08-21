package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/ethanhinson/fuse/internal/model"
)

// modelsActiveMarker is the selected-row glyph, matching the slash completer's
// cursor so the two listings read the same way.
const modelsActiveMarker = "▸ "

// modelsIndent is the width-matched filler for non-active rows, so every
// column starts at the same display cell on every row.
const modelsIndent = "  "

// modelsEmptyLine is what a nil or empty registry renders instead of a listing.
const modelsEmptyLine = "no models configured"

// modelsColGap separates the alias / ID / persona columns.
const modelsColGap = "  "

// renderModelsListing returns the /models output as display lines: a header
// followed by one aligned row per registry alias. Pure — no ShellModel, no I/O.
//
// Two passes: measure the widest alias / ID / persona in DISPLAY CELLS
// (lipgloss.Width, never len — the same discipline as the slash completer),
// then emit prefix + padded columns + the parenthesised tag.
func renderModelsListing(reg *model.Registry, active string) []string {
	if reg == nil {
		return []string{modelsEmptyLine}
	}

	type row struct{ alias, id, persona string }
	var rows []row
	var wAlias, wID, wPersona int
	for _, alias := range reg.Names() {
		mc, err := reg.Resolve(alias)
		if err != nil {
			// Defensive: Names() and Resolve() read the same map, so this
			// branch is unreachable through the exported API and therefore
			// untested. Skip rather than panic if that ever drifts.
			continue
		}
		r := row{alias: alias, id: mc.ID, persona: mc.Persona}
		rows = append(rows, r)
		if w := lipgloss.Width(r.alias); w > wAlias {
			wAlias = w
		}
		if w := lipgloss.Width(r.id); w > wID {
			wID = w
		}
		if w := lipgloss.Width(r.persona); w > wPersona {
			wPersona = w
		}
	}
	if len(rows) == 0 {
		return []string{modelsEmptyLine}
	}

	def := reg.DefaultAlias()
	out := make([]string, 0, len(rows)+1)
	out = append(out, headerStyle.Render("Available models:"))
	for _, r := range rows {
		var b strings.Builder
		if r.alias == active {
			b.WriteString(modelsActiveMarker)
		} else {
			b.WriteString(modelsIndent)
		}
		b.WriteString(padCells(r.alias, wAlias))
		b.WriteString(modelsColGap)
		b.WriteString(padCells(r.id, wID))
		b.WriteString(modelsColGap)
		b.WriteString(padCells(r.persona, wPersona))
		if tag := modelsTag(r.alias == def, r.alias == active); tag != "" {
			b.WriteString(" " + tag)
		}
		// Trailing column padding is meaningless once nothing follows it; the
		// column START offsets, which are what alignment means here, are
		// unaffected by trimming the tail.
		out = append(out, strings.TrimRight(b.String(), " "))
	}
	return out
}

// modelsTag is the trailing parenthesised annotation. The vocabulary is closed:
// "(default, active)", "(default)", "(active)", or nothing.
func modelsTag(isDefault, isActive bool) string {
	switch {
	case isDefault && isActive:
		return "(default, active)"
	case isDefault:
		return "(default)"
	case isActive:
		return "(active)"
	default:
		return ""
	}
}

// padCells right-pads s to n DISPLAY CELLS. Never truncates: n is derived from
// the widest member of the column, so s can never exceed it.
func padCells(s string, n int) string {
	if pad := n - lipgloss.Width(s); pad > 0 {
		return s + strings.Repeat(" ", pad)
	}
	return s
}
