package tui

import (
	"fmt"
	"sort"
	"strings"

	btable "github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/ethanhinson/fuse/internal/permissions"
)

// config_view.go: the `/config` screen — one overlay hosting the grouped
// settings surfaces behind the Tabs primitive (tabs.go).
//
// The Models tab is NOT a second model editor. It shares the SAME
// *modelsEditState and the SAME handlers as `/models edit`
// (openModelsEditor / handleModelsEditorKey / handleModelsFormKey /
// saveModelForm / deleteSelectedModel); `/config` is a second door onto that
// one room, so a fix to add/edit/remove lands on both routes at once. What
// differs is only the presentation: this tab renders through bubbles/table
// because it wants selection AND scroll over a registry that can outgrow the
// pane, where the static listing surfaces use table.go's primitive.
//
// Row actions dispatch on the selected ROW OBJECT (change 0010's finding): the
// handlers read st.rows[st.cursor] directly. Nothing here composes a
// "/model <alias>" string and re-parses it through handleSlash — handleSlash is
// for TYPED input only.
//
// Width invariant (change 0041): the panes and the tab bar are fitted, never
// bordered and never composed with lipgloss.JoinHorizontal. bubbles/table is
// given explicitly-fitted column widths and zero cell padding, and every line
// this file emits passes through fitTableLine as a final hard clamp.

const (
	// configTabModels, configTabPermissions and configTabMCP are the pane
	// indices, matching the 1/2/3 direct-index keys Tabs binds.
	configTabModels = iota
	configTabPermissions
	configTabMCP
)

// configOverlayHeight is how many rows of the viewport the overlay paints over:
// title, tab bar, rule, a body, and the key hint. It matches the models editor's
// rough footprint so the two surfaces feel the same size.
const configOverlayHeight = 16

// configState is the transient /config overlay state, nil unless the screen is
// open. It holds pointers rather than snapshots so the panes always render
// current data — the shell rebuilds its ShellModel value on every key.
type configState struct {
	tabs *Tabs
	// edit is the shared models-editor state — the same struct /models edit
	// drives. It is never a copy.
	edit *modelsEditState
	// mode is the session permission-mode holder, read-only here.
	mode *permissions.SessionMode
	// slashReg is where the configured MCP servers are read from: the MCP
	// provider publishes one entry per tool, each stamped with its server.
	slashReg *SlashRegistry
}

// newConfigState builds the three-pane container over live state.
func newConfigState(edit *modelsEditState, mode *permissions.SessionMode, slashReg *SlashRegistry) *configState {
	st := &configState{edit: edit, mode: mode, slashReg: slashReg}
	st.tabs = NewTabs(
		Pane{Title: "Models", View: st.modelsPaneView},
		Pane{Title: "Permissions", View: st.permissionsPaneView},
		Pane{Title: "MCP", View: st.mcpPaneView},
	)
	return st
}

// openConfig opens the /config screen. It opens the models editor state too —
// not a parallel copy of it — so the Models tab and `/models edit` are the same
// editor with two entry points.
func (m ShellModel) openConfig() (tea.Model, tea.Cmd) {
	m.modelsEdit = &modelsEditState{}
	m.refreshModelsEditor()
	m.config = newConfigState(m.modelsEdit, m.sessionMode, m.slashReg)
	return m, nil
}

// closeConfig tears down the overlay and releases the shared editor state.
func (m *ShellModel) closeConfig() {
	m.config = nil
	m.modelsEdit = nil
}

// handleConfigKey routes a key while /config is open. It is called from
// handleKey BELOW the approval and ask guards: /config changes nothing about
// that precedence — a pending approval or question still owns the keyboard.
//
// Order inside the overlay:
//  1. an open model form owns every key, including tab (which moves fields, not
//     panes) — otherwise the container would steal it mid-edit;
//  2. the Tabs container claims tab / shift+tab / 1..3;
//  3. esc closes the overlay (Tabs deliberately leaves esc to its host);
//  4. anything else goes to the active pane's handler.
func (m ShellModel) handleConfigKey(msg tea.KeyMsg) (handled bool, mdl tea.Model, cmd tea.Cmd) {
	if m.config == nil {
		return false, m, nil
	}
	st := m.config
	if st.tabs.Active() == configTabModels && m.modelsEdit != nil && m.modelsEdit.form != nil {
		return m.handleModelsFormKey(msg)
	}
	if ok, c := st.tabs.Update(msg); ok {
		return true, m, c
	}
	switch msg.String() {
	case "esc", "q":
		// "q" is the models editor's own second close key; honouring it here
		// keeps one meaning for it rather than letting it close the inner
		// editor and strand the overlay with no state.
		m.closeConfig()
		return true, m, nil
	}
	switch msg.Type {
	case tea.KeyCtrlC, tea.KeyCtrlD:
		// Let these fall through to the shell's global quit binding rather than
		// being swallowed here: /config has no key of its own for either, and
		// trapping them left the app unquittable while the settings screen was
		// open. Checked BEFORE dispatching into the shared models-editor
		// handler below — that handler is also reachable from /models edit's
		// own standalone entry point and is out of this fix's scope, so the
		// fall-through has to happen here, in /config's own routing, rather
		// than inside it.
		return false, m, nil
	}
	if st.tabs.Active() == configTabModels && m.modelsEdit != nil {
		return m.handleModelsEditorKey(msg)
	}
	// The read-only panes swallow everything else rather than leaking keys into
	// the shell's input behind the overlay.
	return true, m, nil
}

// modelsPaneView renders the shared editor's list — or its form — into the
// pane. The list is a bubbles/table so it scrolls with the cursor; the cursor
// itself stays in modelsEditState, which is what the shared handlers move.
func (st *configState) modelsPaneView(width, height int) string {
	if st.edit == nil {
		return headerStyle.Render("model editor unavailable")
	}
	if st.edit.form != nil {
		var lines []string
		renderModelForm(func(s string) { lines = append(lines, s) }, st.edit.form)
		return strings.Join(lines, "\n")
	}

	var footer []string
	if st.edit.status != "" {
		footer = append(footer, st.edit.status)
	}
	footer = append(footer, askKeysStyle.Render("j/k move · a add · e edit · d delete"))

	if len(st.edit.rows) == 0 {
		return strings.Join(append([]string{headerStyle.Render("no models registered")}, footer...), "\n")
	}

	// Measure in display cells over sanitized text, then fit the columns to the
	// pane width the same way table.go does — bubbles/table does not clamp its
	// own column widths against the render width.
	titles := []string{"alias", "model id", "persona"}
	rows := make([]btable.Row, len(st.edit.rows))
	widths := make([]int, len(titles))
	for i, t := range titles {
		widths[i] = lipgloss.Width(t)
	}
	for i, r := range st.edit.rows {
		cells := []string{tableCell(r.Alias), tableCell(r.Config.ID), tableCell(personaCell(r.Config))}
		for c, v := range cells {
			if w := lipgloss.Width(v); w > widths[c] {
				widths[c] = w
			}
		}
		rows[i] = cells
	}
	// One gap cell per column boundary is paid out of the same budget as the
	// content, so the gap is folded into each column's width rather than added
	// beside it.
	const configColGap = 1
	fitColumnWidths(widths, []int{3, 4, 3}, width-configColGap*len(titles))
	// Every btable.Row below carries exactly 3 cells (one per title), and
	// bubbles/table's renderRow indexes m.cols[i] against the ROW's cells — a
	// column dropped here without also dropping its cells would panic inside
	// View(). Keep all three columns; Width is widths[i]+configColGap, which is
	// never <= 0 (configColGap alone is 1), so there is nothing to drop.
	cols := make([]btable.Column, len(titles))
	for i, t := range titles {
		cols[i] = btable.Column{Title: t, Width: widths[i] + configColGap}
	}

	bodyH := height - len(footer) // WithHeight already deducts the table's own header row
	if bodyH < 1 {
		bodyH = 1
	}
	tbl := btable.New(
		btable.WithColumns(cols),
		btable.WithRows(rows),
		btable.WithHeight(bodyH),
		btable.WithFocused(true),
		// No padding anywhere: padding would add cells outside the fitted
		// column widths and blow the width budget.
		btable.WithStyles(btable.Styles{
			Header:   headerStyle,
			Cell:     lipgloss.NewStyle(),
			Selected: askSelectedStyle,
		}),
	)
	// Window the rows ourselves rather than trusting bubbles/table's offset
	// bookkeeping: the table is rebuilt on every render with viewport.YOffset 0,
	// and SetCursor only moves its internal start index — it never touches
	// YOffset (only MoveUp/MoveDown do, and nothing here calls them). So the
	// visible slice stays [0:Height) of the rendered content and the selection
	// falls off the bottom for any cursor at or past the pane height. Feeding
	// the table exactly the rows that fit, with a cursor rebased into that
	// slice, keeps the selected alias on screen at every position.
	//
	// h is read back off the table rather than derived from bodyH: WithHeight
	// already deducts the header row, so Height() is the true number of body
	// rows the pane will paint.
	h := tbl.Height()
	if h < 1 {
		h = 1
	}
	if len(rows) > h {
		cursor := st.edit.cursor
		if cursor < 0 {
			cursor = 0
		}
		if cursor > len(rows)-1 {
			cursor = len(rows) - 1
		}
		start := 0
		if cursor >= h {
			start = cursor - h + 1
		}
		tbl.SetRows(rows[start : start+h])
		tbl.SetCursor(cursor - start)
	} else {
		tbl.SetCursor(st.edit.cursor)
	}

	out := strings.Split(tbl.View(), "\n")
	return strings.Join(append(out, footer...), "\n")
}

// permissionsPaneView reports the session permission mode — the same
// information /mode prints. Read-only in this change; edit actions are an
// explicit follow-up.
func (st *configState) permissionsPaneView(width, height int) string {
	cur := "(unknown)"
	if st.mode != nil {
		cur = st.mode.Get().String()
	}
	modes := []struct{ name, desc string }{
		{"smart", "classify each call; prompt only when it looks risky"},
		{"auto", "approve routine calls without prompting"},
		{"prompt-all", "prompt for every gated call"},
		{"off", "no gating"},
	}
	rows := make([]Row, len(modes))
	for i, md := range modes {
		rows[i] = Row{
			Cells:  []string{md.name, md.desc},
			Active: md.name == cur,
		}
	}
	lines := []string{headerStyle.Render("permission mode: ") + statusModelStyle.Render(cur), ""}
	lines = append(lines, RenderTable(
		[]Column{{Style: humanMsgStyle}, {Style: modelsPersonaStyle}},
		rows, width, TableOpts{ActiveMarker: "❯ ", MarkerStyle: askCursorStyle},
	)...)
	lines = append(lines, "", askKeysStyle.Render("read-only · change with /mode NAME or Shift+Tab"))
	return strings.Join(lines, "\n")
}

// mcpPaneView lists the configured MCP servers, read-only. There is no /mcp
// builtin today, so this is the first surface for them: the server set is read
// off the slash registry, where the MCP provider publishes one entry per tool
// stamped with its server name.
func (st *configState) mcpPaneView(width, height int) string {
	servers := st.mcpServers()
	if len(servers) == 0 {
		return strings.Join([]string{
			headerStyle.Render("no MCP servers configured"),
			"",
			askKeysStyle.Render("read-only · declare servers under mcp_servers in ~/.fuse/config.yml"),
		}, "\n")
	}
	rows := make([]Row, len(servers))
	for i, s := range servers {
		rows[i] = Row{Cells: []string{s.name, fmt.Sprintf("%d tool(s)", s.tools)}}
	}
	lines := []string{headerStyle.Render(fmt.Sprintf("MCP servers (%d)", len(servers))), ""}
	lines = append(lines, RenderTable(
		[]Column{{Style: humanMsgStyle}, {Style: modelsPersonaStyle}},
		rows, width, TableOpts{},
	)...)
	lines = append(lines, "", askKeysStyle.Render("read-only · edit actions are a follow-up"))
	return strings.Join(lines, "\n")
}

// mcpServer is one configured server projected out of the slash registry.
type mcpServer struct {
	name  string
	tools int
}

// mcpServers folds the registry's KindMCP entries into a stable, name-sorted
// server list with tool counts.
func (st *configState) mcpServers() []mcpServer {
	if st.slashReg == nil {
		return nil
	}
	counts := map[string]int{}
	for _, e := range st.slashReg.All() {
		if e.Kind == KindMCP && e.Server != "" {
			counts[e.Server]++
		}
	}
	out := make([]mcpServer, 0, len(counts))
	for name, n := range counts {
		out = append(out, mcpServer{name: name, tools: n})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// configOverlayLines renders the whole overlay — title, tab bar, active pane,
// key hint — as display lines, each guaranteed to be at most width cells.
func configOverlayLines(st *configState, width, height int) []string {
	if st == nil || width < 8 || height < 4 {
		return nil
	}
	title := fitTableLine(" "+askHeaderStyle.Render("⚙ Config"), width)
	hint := fitTableLine(" "+askKeysStyle.Render("tab/shift+tab or 1-3 switch · Esc close"), width)

	lines := []string{title}
	// The tab bar and pane get everything the title and hint do not: a glyph
	// spends a row from the same budget, exactly as it spends a cell.
	body := st.tabs.View(width, height-len(lines)-1)
	if body != "" {
		lines = append(lines, strings.Split(body, "\n")...)
	}
	for len(lines) < height-1 {
		lines = append(lines, "")
	}
	return append(lines[:height-1], hint)
}

// renderConfigOverlay paints the /config screen over the bottom of the
// viewport, mirroring renderModelsEditorOverlay and renderQueueOverlay.
func renderConfigOverlay(base string, st *configState, width int) string {
	if st == nil || width < 8 {
		return base
	}
	baseLines := strings.Split(base, "\n")
	height := configOverlayHeight
	if height > len(baseLines) {
		height = len(baseLines)
	}
	overlay := configOverlayLines(st, width, height)
	start := len(baseLines) - len(overlay)
	if start < 0 {
		start = 0
	}
	for i, ol := range overlay {
		if start+i < len(baseLines) {
			baseLines[start+i] = fitTableLine(ol, width)
		}
	}
	return strings.Join(baseLines, "\n")
}
