package tui

import (
	"fmt"
	"strings"

	"github.com/ethanhinson/fuse/internal/model"
)

// modelsActiveMarker is the selected-row glyph, matching the slash completer's
// cursor so the two listings read the same way.
const modelsActiveMarker = "▸ "

// modelsEmptyLine is what a nil or empty registry renders instead of a listing.
const modelsEmptyLine = "no models configured"

// modelsColGap separates the alias / ID / persona columns.
const modelsColGap = "  "

// renderModelsListing returns the /models output as display lines: a header
// followed by one aligned row per registry alias. Pure — no ShellModel, no I/O.
//
// Layout is delegated to the shared table primitive (table.go): alias / ID /
// persona columns plus the parenthesised tag, clamped to width. The
// blank-persona dash is declared as the persona column's Blank rather than
// substituted at the call site, so the column holds its position — and with it
// the tag offset — without the caller knowing why.
//
// width is the viewport content width the shell will wrap these lines to (0
// means unbounded, for callers with no pane). It is NOT cosmetic: /models
// output goes through appendLine -> refreshViewport -> hangWrap, whose
// wordwrap pass breaks at the last space before the limit. On an unbounded
// listing that space lands inside a padding run, so one long model ID folds
// the persona and the trailing tag onto a second line for EVERY row — change
// 0078's compositor failure. Clamping here is what keeps the rows one line.
func renderModelsListing(reg *model.Registry, active string, width int) []string {
	if reg == nil {
		return []string{modelsEmptyLine}
	}

	def := reg.DefaultAlias()
	var rows []Row
	for _, alias := range reg.Names() {
		mc, err := reg.Resolve(alias)
		if err != nil {
			// Defensive: Names() and Resolve() read the same map, so this
			// branch is unreachable through the exported API and therefore
			// untested. Skip rather than panic if that ever drifts.
			continue
		}
		rows = append(rows, Row{
			Cells:  []string{alias, mc.ID, mc.Persona},
			Active: alias == active,
			Tag:    modelsTag(alias == def, alias == active),
		})
	}
	if len(rows) == 0 {
		return []string{modelsEmptyLine}
	}

	cols := []Column{
		{},
		{},
		{Blank: modelsPersonaBlank},
	}
	// The header here spans the whole listing rather than labelling columns, so
	// it is prepended instead of going through TableOpts.ShowHeader.
	out := make([]string, 0, len(rows)+1)
	out = append(out, headerStyle.Render("Available models:"))
	return append(out, RenderTable(cols, rows, width, TableOpts{
		Gap:          modelsColGap,
		ActiveMarker: modelsActiveMarker,
	})...)
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

// modelAliasCompletions returns a supplier of "/model <alias>" completion
// entries filtered by prefix, reading the live registry each call so aliases
// added through the editor appear without rebuilding the completer. Entries are
// KindBuiltin so selection routes through the existing /model dispatch.
func modelAliasCompletions(reg *model.Registry) func(prefix string) []SlashEntry {
	return func(prefix string) []SlashEntry {
		if reg == nil {
			return nil
		}
		def := reg.DefaultAlias()
		var out []SlashEntry
		for _, e := range reg.Entries() {
			if prefix != "" && !strings.HasPrefix(e.Alias, prefix) {
				continue
			}
			alias := e.Alias // capture for the closure
			desc := e.Config.ID
			if e.Config.Persona != "" {
				desc += "  ·  " + e.Config.Persona
			}
			if alias == def {
				desc += "  (default)"
			}
			out = append(out, SlashEntry{
				Command:     "/model " + alias,
				Description: desc,
				Kind:        KindBuiltin,
				expand:      func() string { return "/model " + alias },
			})
		}
		return out
	}
}

// modelsPersonaBlank is what an empty persona renders as, so the persona column
// never collapses. It is the persona Column's Blank in the /models listing and
// personaCell's substitution in the editor.
const modelsPersonaBlank = "-"

// personaCell renders the persona column, substituting a dash for the empty
// persona so the column never collapses.
func personaCell(mc model.ModelConfig) string {
	if mc.Persona == "" {
		return modelsPersonaBlank
	}
	return mc.Persona
}

// limitsCell renders the max-tokens / context-window column. A zero max-tokens
// means "harness default"; a zero context window means the 128k harness
// default, so both are shown as "default" rather than "0".
func limitsCell(mc model.ModelConfig) string {
	maxTok := "default"
	if mc.MaxTokens > 0 {
		maxTok = fmt.Sprintf("%d out", mc.MaxTokens)
	}
	ctx := "128k ctx"
	if mc.ContextWindow > 0 {
		ctx = fmt.Sprintf("%s ctx", humanTokens(mc.ContextWindow))
	}
	return maxTok + " · " + ctx
}

// humanTokens formats a context-window size compactly. Context windows are
// conventionally powers of two, so it scales by 1024 (131072 -> "128k",
// 1048576 -> "1m") to match how those sizes are named. Values under 1024 are
// shown verbatim.
func humanTokens(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%dm", (n+(1<<19))>>20)
	case n >= 1<<10:
		return fmt.Sprintf("%dk", (n+(1<<9))>>10)
	default:
		return fmt.Sprintf("%d", n)
	}
}
