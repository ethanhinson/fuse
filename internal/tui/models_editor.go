package tui

import (
	"fmt"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/model"
)

// Editor column styles. The /models listing (models_command.go) renders plain
// text via headerStyle; the editor colours its columns so the interactive
// modal reads as a distinct surface.
var (
	modelsIDStyle      = lipgloss.NewStyle().Foreground(colNormal)
	modelsPersonaStyle = lipgloss.NewStyle().Foreground(colMuted)
)

// models_editor.go: the interactive model-mapping editor, opened with
// `/models edit`. It mirrors the /queue editor's in-transcript overlay pattern
// (queue_view.go): a lightweight modal that owns the keyboard while open. It
// lists the registry's aliases and lets the human add, edit, or remove a
// mapping. A save both persists to ~/.fuse/config.yml (config.SetModel /
// RemoveModel) AND mutates the live *model.Registry in place, so the change
// takes effect for the next agent built this session without a restart.

// modelsEditState is the transient editor state, nil unless /models edit is open.
type modelsEditState struct {
	rows   []model.NamedModel
	cursor int
	// form is non-nil while adding or editing a mapping.
	form *modelForm
	// status is a one-line result/error banner shown under the list.
	status string
}

// modelFormField enumerates the editable fields, in Tab order.
type modelFormField int

const (
	fieldAlias modelFormField = iota
	fieldID
	fieldPersona
	fieldMaxTokens
	fieldContextWindow
	modelFormFieldCount
)

// modelForm is the add/edit form buffer.
type modelForm struct {
	editing   bool   // true = editing an existing alias (alias field is locked)
	origAlias string // the alias being edited, for the config/registry update
	alias     string
	id        string
	persona   string
	maxTokens string
	ctxWindow string
	field     modelFormField
}

func (f *modelForm) current() *string {
	switch f.field {
	case fieldAlias:
		return &f.alias
	case fieldID:
		return &f.id
	case fieldPersona:
		return &f.persona
	case fieldMaxTokens:
		return &f.maxTokens
	case fieldContextWindow:
		return &f.ctxWindow
	}
	return &f.alias
}

func (f *modelForm) nextField() {
	f.field = (f.field + 1) % modelFormFieldCount
	// The alias field is locked during edit; skip it.
	if f.editing && f.field == fieldAlias {
		f.field = fieldID
	}
}

func (f *modelForm) prevField() {
	f.field = (f.field - 1 + modelFormFieldCount) % modelFormFieldCount
	if f.editing && f.field == fieldAlias {
		f.field = fieldContextWindow
	}
}

// openModelsEditor builds the editor state from the current registry. Returns
// the model+cmd pair so it can be called directly from the slash switch.
func (m ShellModel) openModelsEditor() (tea.Model, tea.Cmd) {
	m.modelsEdit = &modelsEditState{}
	m.refreshModelsEditor()
	return m, nil
}

// refreshModelsEditor re-reads the registry after a mutation, clamping cursor.
func (m *ShellModel) refreshModelsEditor() {
	if m.modelsEdit == nil {
		return
	}
	if m.reg != nil {
		m.modelsEdit.rows = m.reg.Entries()
	}
	if m.modelsEdit.cursor >= len(m.modelsEdit.rows) {
		m.modelsEdit.cursor = len(m.modelsEdit.rows) - 1
	}
	if m.modelsEdit.cursor < 0 {
		m.modelsEdit.cursor = 0
	}
}

// handleModelsEditorKey drives the editor. Returns handled=false only when the
// editor is closed; while open it swallows every key.
func (m ShellModel) handleModelsEditorKey(msg tea.KeyMsg) (handled bool, model tea.Model, cmd tea.Cmd) {
	if m.modelsEdit == nil {
		return false, m, nil
	}
	st := m.modelsEdit

	if st.form != nil {
		return m.handleModelsFormKey(msg)
	}

	switch msg.String() {
	case "esc", "q":
		m.modelsEdit = nil
		return true, m, nil
	case "up", "k":
		if st.cursor > 0 {
			st.cursor--
		}
		return true, m, nil
	case "down", "j":
		if st.cursor < len(st.rows)-1 {
			st.cursor++
		}
		return true, m, nil
	case "a":
		st.form = &modelForm{}
		st.status = ""
		return true, m, nil
	case "e", "enter":
		if len(st.rows) > 0 {
			row := st.rows[st.cursor]
			st.form = &modelForm{
				editing:   true,
				origAlias: row.Alias,
				alias:     row.Alias,
				id:        row.Config.ID,
				persona:   row.Config.Persona,
				maxTokens: intField(row.Config.MaxTokens),
				ctxWindow: intField(row.Config.ContextWindow),
				field:     fieldID,
			}
			st.status = ""
		}
		return true, m, nil
	case "d", "x":
		return m.deleteSelectedModel()
	}
	return true, m, nil // editor swallows other keys while open
}

// handleModelsFormKey drives the add/edit form.
func (m ShellModel) handleModelsFormKey(msg tea.KeyMsg) (bool, tea.Model, tea.Cmd) {
	f := m.modelsEdit.form
	switch msg.Type {
	case tea.KeyEsc:
		m.modelsEdit.form = nil
		return true, m, nil
	case tea.KeyEnter:
		return m.saveModelForm()
	case tea.KeyTab, tea.KeyDown:
		f.nextField()
		return true, m, nil
	case tea.KeyShiftTab, tea.KeyUp:
		f.prevField()
		return true, m, nil
	case tea.KeyBackspace:
		cur := f.current()
		if n := len(*cur); n > 0 {
			*cur = (*cur)[:n-1]
		}
		return true, m, nil
	case tea.KeyRunes:
		cur := f.current()
		*cur += string(msg.Runes)
		return true, m, nil
	case tea.KeySpace:
		// Spaces are meaningless in every field (aliases, ids, personas, and
		// numbers are all whitespace-free), so ignore them rather than corrupt
		// a value.
		return true, m, nil
	}
	return true, m, nil
}

// saveModelForm validates the form, persists to config, and mutates the live
// registry. On any error it leaves the form open with a status banner.
func (m ShellModel) saveModelForm() (bool, tea.Model, tea.Cmd) {
	st := m.modelsEdit
	f := st.form

	alias := strings.TrimSpace(f.alias)
	id := strings.TrimSpace(f.id)
	if alias == "" {
		st.status = errStyle("alias is required")
		return true, m, nil
	}
	if id == "" {
		st.status = errStyle("model id is required")
		return true, m, nil
	}
	// Adding an alias that already exists would silently overwrite it; require
	// the user to edit it instead.
	if !f.editing && m.reg.Has(alias) {
		st.status = errStyle(fmt.Sprintf("alias %q already exists — edit it instead", alias))
		return true, m, nil
	}

	maxTok, err := parseNonNegative(f.maxTokens, "max tokens")
	if err != nil {
		st.status = errStyle(err.Error())
		return true, m, nil
	}
	ctxWin, err := parseNonNegative(f.ctxWindow, "context window")
	if err != nil {
		st.status = errStyle(err.Error())
		return true, m, nil
	}

	mc := model.ModelConfig{
		ID:            id,
		MaxTokens:     maxTok,
		ContextWindow: ctxWin,
		Persona:       strings.TrimSpace(f.persona),
	}
	// On edit, preserve the system prefix the form does not expose.
	if f.editing {
		if prev, err := m.reg.Resolve(f.origAlias); err == nil {
			mc.SystemPrefix = prev.SystemPrefix
		}
	}

	// Persist first; only touch the live registry if the file write succeeds,
	// so config and the running session never drift apart.
	if err := config.SetModel(alias, toConfigModel(mc)); err != nil {
		st.status = errStyle("save failed: " + err.Error())
		return true, m, nil
	}
	m.reg.Set(alias, mc)

	verb := "added"
	if f.editing {
		verb = "updated"
	}
	st.form = nil
	st.status = okStyle(fmt.Sprintf("%s %s", verb, alias))
	m.refreshModelsEditor()
	// Keep the cursor on the saved row.
	for i, r := range st.rows {
		if r.Alias == alias {
			st.cursor = i
			break
		}
	}
	return true, m, nil
}

// deleteSelectedModel removes the highlighted alias from config and the live
// registry. The registry refuses to remove the default alias, which is surfaced
// as a status banner rather than a silent no-op.
func (m ShellModel) deleteSelectedModel() (bool, tea.Model, tea.Cmd) {
	st := m.modelsEdit
	if len(st.rows) == 0 {
		return true, m, nil
	}
	alias := st.rows[st.cursor].Alias
	if alias == m.reg.DefaultAlias() {
		st.status = errStyle(fmt.Sprintf("%q is the default model — repoint models.default first", alias))
		return true, m, nil
	}
	if err := config.RemoveModel(alias); err != nil {
		st.status = errStyle("remove failed: " + err.Error())
		return true, m, nil
	}
	m.reg.Remove(alias)
	st.status = okStyle("removed " + alias)
	m.refreshModelsEditor()
	return true, m, nil
}

// toConfigModel converts a registry ModelConfig into the config package's
// equivalent for persistence.
func toConfigModel(mc model.ModelConfig) config.ModelConfig {
	return config.ModelConfig{
		ID:            mc.ID,
		MaxTokens:     mc.MaxTokens,
		ContextWindow: mc.ContextWindow,
		Persona:       mc.Persona,
		SystemPrefix:  mc.SystemPrefix,
	}
}

// parseNonNegative parses an optional integer field: empty means zero (the
// "harness default" sentinel), and negatives/garbage are rejected with a
// field-named error.
func parseNonNegative(s, name string) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s must be a non-negative number", name)
	}
	return n, nil
}

// intField renders an int for the form, showing "" for zero so the field reads
// as empty (the harness-default sentinel) rather than a literal 0.
func intField(n int) string {
	if n == 0 {
		return ""
	}
	return strconv.Itoa(n)
}

func errStyle(s string) string { return errorStyle.Render(s) }
func okStyle(s string) string  { return okBannerStyle.Render(s) }

// renderModelsEditorOverlay paints the editor over the bottom of the viewport,
// mirroring renderQueueOverlay. In list mode it shows the aliases with a
// cursor; in form mode it shows the field form.
func renderModelsEditorOverlay(base string, st *modelsEditState, width int) string {
	if st == nil || width < 8 {
		return base
	}
	var lines []string
	add := func(s string) { lines = append(lines, s) }

	if st.form != nil {
		renderModelForm(add, st.form)
	} else {
		add(" " + askHeaderStyle.Render("⚙ Model mappings") + headerStyle.Render(fmt.Sprintf("  (%d)", len(st.rows))))
		add("")
		if len(st.rows) == 0 {
			add(" " + headerStyle.Render("no models registered"))
		}
		cols := []Column{
			{Style: humanMsgStyle},
			{Style: modelsIDStyle},
			{Style: modelsPersonaStyle},
		}
		rows := make([]Row, len(st.rows))
		for i, row := range st.rows {
			rows[i] = Row{
				Cells:  []string{row.Alias, truncate(row.Config.ID, 28), personaCell(row.Config)},
				Active: i == st.cursor,
			}
		}
		// The real render width, NOT 0 (unbounded): all three columns are padded
		// to their global max, so one over-wide alias would widen every row and
		// leave only fitLine below as the guard — and that truncates from the
		// RIGHT, eating the persona column on every row to pay for one entry.
		for _, line := range RenderTable(cols, rows, width, TableOpts{
			ActiveMarker: "❯ ",
			MarkerStyle:  askCursorStyle,
		}) {
			add(line)
		}
		add("")
		if st.status != "" {
			add(" " + st.status)
		}
		add(" " + askKeysStyle.Render("j/k move · a add · e edit · d delete · Esc close"))
	}

	overlay := lines
	baseLines := strings.Split(base, "\n")
	start := len(baseLines) - len(overlay)
	if start < 0 {
		start = 0
	}
	for i, ol := range overlay {
		if start+i < len(baseLines) {
			baseLines[start+i] = fitLine(ol, width)
		}
	}
	return strings.Join(baseLines, "\n")
}

// renderModelForm paints the add/edit form via the add callback.
func renderModelForm(add func(string), f *modelForm) {
	title := "Add model"
	if f.editing {
		title = "Edit " + f.origAlias
	}
	add(" " + askHeaderStyle.Render("⚙ "+title))
	add("")

	fields := []struct {
		which modelFormField
		label string
		value string
		hint  string
	}{
		{fieldAlias, "alias", f.alias, "short name you type after /model"},
		{fieldID, "model id", f.id, "gateway route, e.g. cloud/glm-5.2"},
		{fieldPersona, "persona", f.persona, "coding · research · general (optional)"},
		{fieldMaxTokens, "max tokens", f.maxTokens, "per-turn output ceiling (blank = default)"},
		{fieldContextWindow, "context window", f.ctxWindow, "context size (blank = 128k default)"},
	}
	for _, fl := range fields {
		cursor := "  "
		label := askKeysStyle.Render(fmt.Sprintf("%-15s", fl.label))
		val := fl.value
		locked := f.editing && fl.which == fieldAlias
		if fl.which == f.field {
			cursor = askCursorStyle.Render("❯ ")
			val = askSelectedStyle.Render(val + "▌")
		} else if locked {
			val = modelsPersonaStyle.Render(val + " (locked)")
		} else {
			val = askOptionStyle.Render(val)
		}
		line := cursor + label + " " + val
		if fl.which == f.field && fl.hint != "" {
			line += "  " + askDescStyle.Render(fl.hint)
		}
		add(line)
	}
	add("")
	add(" " + askKeysStyle.Render("Tab/↑↓ field · Enter save · Esc cancel"))
}
