package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/ethanhinson/fuse/internal/model"
)

// modelsFixture is the small fixed registry the table cases share: three
// aliases with deliberately different alias/ID/persona widths so the padding
// math has something to align.
func modelsFixture(def string) *model.Registry {
	return model.NewRegistry(def, map[string]model.ModelConfig{
		"glm":        {ID: "cloud/glm-5.2", Persona: "general"},
		"sonnet-5":   {ID: "claude/sonnet-5", Persona: "general"},
		"claude-max": {ID: "cli/claude-max", Persona: "coding"},
	})
}

// modelsRows strips styling and returns header + row lines.
func modelsRows(t *testing.T, lines []string) (string, []string) {
	t.Helper()
	if len(lines) == 0 {
		t.Fatal("renderModelsListing returned no lines")
	}
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		out = append(out, stripANSIString(l))
	}
	return out[0], out[1:]
}

func TestModelsListingHeaderAndOrder(t *testing.T) {
	header, rows := modelsRows(t, renderModelsListing(modelsFixture("glm"), "glm", 0))
	if header != "Available models:" {
		t.Fatalf("header = %q, want %q", header, "Available models:")
	}
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3: %#v", len(rows), rows)
	}
	// Names() order is sorted: claude-max, glm, sonnet-5.
	wantOrder := []string{"claude-max", "glm", "sonnet-5"}
	for i, alias := range wantOrder {
		field := strings.Fields(rows[i])
		// field[0] is the marker for the active row; drop it if present.
		if field[0] == "▸" {
			field = field[1:]
		}
		if field[0] != alias {
			t.Fatalf("row %d alias = %q, want %q (row %q)", i, field[0], alias, rows[i])
		}
	}
}

func TestModelsListingHeaderIsStyled(t *testing.T) {
	// lipgloss degrades to the no-color profile under `go test` (no TTY), which
	// would make headerStyle.Render a no-op and the assertion vacuous. Force a
	// color profile, as the rest of this package's render tests do.
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(prev)

	lines := renderModelsListing(modelsFixture("glm"), "glm", 0)
	if lines[0] == stripANSIString(lines[0]) {
		t.Fatalf("header line is not styled: %q", lines[0])
	}
	if want := headerStyle.Render("Available models:"); lines[0] != want {
		t.Fatalf("header = %q, want %q", lines[0], want)
	}
}

func rowFor(t *testing.T, rows []string, alias string) string {
	t.Helper()
	for _, r := range rows {
		f := strings.Fields(r)
		if f[0] == "▸" {
			f = f[1:]
		}
		if f[0] == alias {
			return r
		}
	}
	t.Fatalf("no row for alias %q in %#v", alias, rows)
	return ""
}

func TestModelsListingTagVocabulary(t *testing.T) {
	cases := []struct {
		name     string
		def      string
		active   string
		alias    string
		wantTag  string
		wantMark bool
	}{
		{"default and active", "glm", "glm", "glm", "(default, active)", true},
		{"default not active", "glm", "sonnet-5", "glm", "(default)", false},
		{"active not default", "glm", "sonnet-5", "sonnet-5", "(active)", true},
		{"plain", "glm", "sonnet-5", "claude-max", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, rows := modelsRows(t, renderModelsListing(modelsFixture(tc.def), tc.active, 0))
			row := rowFor(t, rows, tc.alias)
			if tc.wantTag == "" {
				if strings.Contains(row, "(") {
					t.Fatalf("row %q should carry no tag", row)
				}
			} else if !strings.HasSuffix(row, tc.wantTag) {
				t.Fatalf("row %q does not end with tag %q", row, tc.wantTag)
			}
			if tc.wantMark {
				if !strings.HasPrefix(row, "▸ ") {
					t.Fatalf("active row %q missing marker prefix", row)
				}
			} else {
				if !strings.HasPrefix(row, "  ") || strings.HasPrefix(row, "▸") {
					t.Fatalf("inactive row %q should be two-space indented", row)
				}
			}
		})
	}
}

// TestModelsListingColumnOffsets asserts the START OFFSET of each column is
// identical on every row — not the total line width, which is blind to a
// truncated suffix.
func TestModelsListingColumnOffsets(t *testing.T) {
	_, rows := modelsRows(t, renderModelsListing(modelsFixture("glm"), "glm", 0))
	type offsets struct{ alias, id, persona int }
	var first offsets
	for i, row := range rows {
		runes := []rune(row)
		// Column starts: scan for the run boundaries by finding each field's
		// rune index, measured in display cells from line start.
		var got offsets
		field := 0
		inField := false
		for j := 0; j < len(runes); j++ {
			isSpace := runes[j] == ' '
			// The marker glyph is part of the prefix, not a field.
			if !isSpace && runes[j] != '▸' && !inField {
				inField = true
				field++
				cells := lipgloss.Width(string(runes[:j]))
				switch field {
				case 1:
					got.alias = cells
				case 2:
					got.id = cells
				case 3:
					got.persona = cells
				}
			} else if isSpace {
				inField = false
			}
		}
		if i == 0 {
			first = got
			continue
		}
		if got != first {
			t.Fatalf("row %d offsets %+v != row 0 offsets %+v\nrow0=%q\nrowN=%q", i, got, first, rows[0], row)
		}
	}
}

// TestModelsListingFitsWidth is the regression for the unbounded layout: the
// listing is written into the shell transcript, which word-wraps it to the
// viewport width. Laid out unbounded, one long model ID pads every row past
// that width and wordwrap breaks inside the padding run, folding the persona
// and the tag onto a second line for EVERY entry.
//
// Fitting alone is the wrong witness (change 0066): a renderer that dropped the
// trailing tag would also "fit". So this asserts BOTH that every line fits and
// that the tag survives verbatim at that narrow width.
func TestModelsListingFitsWidth(t *testing.T) {
	reg := model.NewRegistry("glm", map[string]model.ModelConfig{
		"glm":      {ID: "cloud/glm-5.2-very-long-provider-model-identifier", Persona: "general"},
		"sonnet-5": {ID: "claude/sonnet-5", Persona: "general"},
	})
	const width = 40
	lines := renderModelsListing(reg, "glm", width)
	for i, l := range lines {
		if w := lipgloss.Width(stripANSIString(l)); w > width {
			t.Errorf("line %d width = %d, want <= %d: %q", i, w, width, l)
		}
	}
	_, rows := modelsRows(t, lines)
	var tagged string
	for _, r := range rows {
		if strings.HasPrefix(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(r), "▸")), "glm") {
			tagged = r
		}
	}
	if tagged == "" {
		t.Fatalf("no glm row in %#v", rows)
	}
	if !strings.HasSuffix(tagged, "(default, active)") {
		t.Errorf("tag did not survive the width clamp: %q", tagged)
	}
}

func TestModelsListingNilRegistry(t *testing.T) {
	lines := renderModelsListing(nil, "glm", 0)
	if len(lines) != 1 {
		t.Fatalf("nil registry -> %d lines, want 1: %#v", len(lines), lines)
	}
	if !strings.Contains(stripANSIString(lines[0]), "no models") {
		t.Fatalf("nil registry line = %q, want a \"no models\" line", lines[0])
	}
}

func TestModelsListingEmptyRegistry(t *testing.T) {
	lines := renderModelsListing(model.NewRegistry("", nil), "", 0)
	if len(lines) != 1 {
		t.Fatalf("empty registry -> %d lines, want 1: %#v", len(lines), lines)
	}
	if !strings.Contains(stripANSIString(lines[0]), "no models") {
		t.Fatalf("empty registry line = %q, want a \"no models\" line", lines[0])
	}
}

// TestModelsListingSingleEntryRendersOneRow asserts a registry with one
// entry renders exactly one row. The defensive `continue` in
// renderModelsListing (for an alias Names() lists but Resolve() rejects) is
// unreachable through the exported API and is therefore untested here.
func TestModelsListingSingleEntryRendersOneRow(t *testing.T) {
	entries := map[string]model.ModelConfig{
		"glm": {ID: "cloud/glm-5.2", Persona: "general"},
	}
	reg := model.NewRegistry("glm", entries)
	_, rows := modelsRows(t, renderModelsListing(reg, "glm", 0))
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1: %#v", len(rows), rows)
	}
	if !strings.Contains(rows[0], "glm") {
		t.Fatalf("row %q should list glm", rows[0])
	}
}

// TestModelsListingBlankPersonaHoldsColumn asserts an empty persona renders as
// the dash rather than collapsing, so the persona column — and the tag offset
// after it — hold their positions across rows.
func TestModelsListingBlankPersonaHoldsColumn(t *testing.T) {
	reg := model.NewRegistry("glm", map[string]model.ModelConfig{
		"glm":      {ID: "cloud/glm-5.2", Persona: "general"},
		"sonnet-5": {ID: "claude/sonnet-5"}, // no persona
	})
	_, rows := modelsRows(t, renderModelsListing(reg, "glm", 0))
	blank := rowFor(t, rows, "sonnet-5")
	if !strings.HasSuffix(blank, modelsPersonaBlank) {
		t.Fatalf("blank-persona row %q should end with %q", blank, modelsPersonaBlank)
	}
	// The tagged row's tag must start after the same persona column, so the
	// dash is genuinely occupying width rather than being appended loosely.
	tagged := rowFor(t, rows, "glm")
	if !strings.HasSuffix(tagged, "(default, active)") {
		t.Fatalf("row %q missing tag", tagged)
	}
	wantOffset := lipgloss.Width(blank) - lipgloss.Width(modelsPersonaBlank)
	gotOffset := lipgloss.Width(strings.TrimSuffix(tagged, "general (default, active)"))
	if gotOffset != wantOffset {
		t.Fatalf("persona column offset %d != %d\nblank=%q\ntagged=%q", gotOffset, wantOffset, blank, tagged)
	}
}

func TestLimitsCell(t *testing.T) {
	if got := limitsCell(model.ModelConfig{MaxTokens: 0, ContextWindow: 0}); got != "default · 128k ctx" {
		t.Errorf("zero limits = %q", got)
	}
	if got := limitsCell(model.ModelConfig{MaxTokens: 16384, ContextWindow: 131072}); got != "16384 out · 128k ctx" {
		t.Errorf("full limits = %q", got)
	}
}

func TestHumanTokens(t *testing.T) {
	cases := map[int]string{131072: "128k", 200704: "196k", 8192: "8k", 1048576: "1m", 500: "500"}
	for n, want := range cases {
		if got := humanTokens(n); got != want {
			t.Errorf("humanTokens(%d) = %q, want %q", n, got, want)
		}
	}
}
