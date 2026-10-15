package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/ethanhinson/fuse/internal/model"
)

// models_discover.go: the "n" key in the models editor. It lists models the
// user does not have yet, from two sources, and turns the chosen one into a new
// alias through the normal add form. Shared by /models and the /config Models
// tab because both route through handleModelsEditorKey.
//
//   - gateway: what the gateway itself serves (its own config's model list);
//   - catalog: everything OpenRouter offers. The gateway can serve any of it
//     through its openrouter/* and private/* wildcard routes (model.Route*).
//
// Every row carries a privacy label: local, zdr (routed only to
// zero-data-retention providers) or may retain (the provider may keep or train
// on prompts). Tab switches source; typing filters; arrows move.

// discoverTimeout bounds a source lookup.
const discoverTimeout = 20 * time.Second

// ModelLister asks the provider gateway which model IDs it serves.
type ModelLister func(ctx context.Context) ([]string, error)

// CatalogLister fetches the OpenRouter catalogue.
type CatalogLister func(ctx context.Context) ([]model.CatalogModel, error)

// WithModelLister attaches the gateway lookup behind the models editor's
// "n" (discover new models) key. Nil leaves the key reporting "no gateway".
func (m ShellModel) WithModelLister(l ModelLister) ShellModel {
	m.lister = l
	return m
}

// WithCatalogLister attaches the OpenRouter catalogue lookup (Tab inside the
// discover view). Nil hides the catalogue source.
func (m ShellModel) WithCatalogLister(l CatalogLister) ShellModel {
	m.catalog = l
	return m
}

type discoverSource int

const (
	sourceGateway discoverSource = iota
	sourceCatalog
	sourceCount
)

func (s discoverSource) label() string {
	if s == sourceCatalog {
		return "OpenRouter catalog"
	}
	return "Gateway"
}

// discoverItem is one candidate model.
type discoverItem struct {
	id      string // the gateway ID that would be registered
	detail  string // context / price, already formatted
	privacy model.Privacy
}

// discoverList is one source's result.
type discoverList struct {
	loaded bool
	err    string
	items  []discoverItem // not yet registered
	total  int            // reported by the source, registered or not
}

// discoverState is the discover view.
type discoverState struct {
	source  discoverSource
	loading bool
	lists   [sourceCount]discoverList
	filter  string
	cursor  int
}

// modelsDiscoveredMsg carries a source's answer back into the shell.
type modelsDiscoveredMsg struct {
	source discoverSource
	items  []discoverItem
	err    error
}

// current is the list for the active source.
func (d *discoverState) current() *discoverList { return &d.lists[d.source] }

// visible is the active list narrowed by the filter.
func (d *discoverState) visible() []discoverItem {
	items := d.current().items
	if d.filter == "" {
		return items
	}
	f := strings.ToLower(d.filter)
	var out []discoverItem
	for _, it := range items {
		if strings.Contains(strings.ToLower(it.id), f) {
			out = append(out, it)
		}
	}
	return out
}

// startDiscover opens the view on the gateway source and fetches it.
func (m ShellModel) startDiscover() (bool, tea.Model, tea.Cmd) {
	st := m.modelsEdit
	if m.lister == nil && m.catalog == nil {
		st.status = errStyle("no gateway configured - set gateway.url in ~/.fuse/config.yml")
		return true, m, nil
	}
	d := &discoverState{source: sourceGateway}
	if m.lister == nil {
		d.source = sourceCatalog
	}
	st.discover = d
	st.discoverBoth = m.lister != nil && m.catalog != nil
	st.status = ""
	return true, m, m.fetchSource(d)
}

// fetchSource starts the async lookup for the view's current source.
func (m ShellModel) fetchSource(d *discoverState) tea.Cmd {
	src := d.source
	d.loading = true
	switch src {
	case sourceCatalog:
		cat := m.catalog
		return func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), discoverTimeout)
			defer cancel()
			models, err := cat(ctx)
			items := make([]discoverItem, 0, len(models))
			for _, c := range models {
				p := model.PrivacyMayRetain
				if c.ZDR {
					p = model.PrivacyZDR
				}
				items = append(items, discoverItem{id: c.GatewayID(), detail: catalogDetail(c), privacy: p})
			}
			return modelsDiscoveredMsg{source: src, items: items, err: err}
		}
	default:
		lister := m.lister
		return func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), discoverTimeout)
			defer cancel()
			ids, err := lister(ctx)
			items := make([]discoverItem, 0, len(ids))
			for _, id := range ids {
				items = append(items, discoverItem{id: id, privacy: model.PrivacyOfRoute(id)})
			}
			return modelsDiscoveredMsg{source: src, items: items, err: err}
		}
	}
}

// catalogDetail formats a catalogue row's context size and price.
func catalogDetail(c model.CatalogModel) string {
	parts := []string{}
	if c.ContextLength > 0 {
		parts = append(parts, humanTokens(c.ContextLength)+" ctx")
	}
	if c.PromptPerM > 0 || c.CompletionPerM > 0 {
		parts = append(parts, fmt.Sprintf("$%s/$%s per M", priceCell(c.PromptPerM), priceCell(c.CompletionPerM)))
	} else {
		parts = append(parts, "free/varies")
	}
	return strings.Join(parts, " · ")
}

func priceCell(v float64) string {
	if v >= 100 {
		return fmt.Sprintf("%.0f", v)
	}
	return fmt.Sprintf("%.2f", v)
}

// applyModelsDiscovered stores a source's answer, dropping IDs already
// registered. A late reply after the user backed out is ignored.
func (m *ShellModel) applyModelsDiscovered(msg modelsDiscoveredMsg) {
	if m.modelsEdit == nil || m.modelsEdit.discover == nil {
		return
	}
	d := m.modelsEdit.discover
	l := &d.lists[msg.source]
	l.loaded = true
	if msg.source == d.source {
		d.loading = false
	}
	if msg.err != nil {
		l.err = msg.err.Error()
		return
	}
	known := map[string]bool{}
	if m.reg != nil {
		for _, e := range m.reg.Entries() {
			known[e.Config.ID] = true
		}
	}
	l.err, l.items, l.total = "", nil, len(msg.items)
	for _, it := range msg.items {
		if !known[it.id] {
			l.items = append(l.items, it)
		}
	}
}

// handleDiscoverKey drives the discover view.
func (m ShellModel) handleDiscoverKey(msg tea.KeyMsg) (bool, tea.Model, tea.Cmd) {
	st := m.modelsEdit
	d := st.discover
	items := d.visible()
	switch msg.Type {
	case tea.KeyCtrlC, tea.KeyCtrlD:
		return false, m, nil
	case tea.KeyEsc:
		if d.filter != "" {
			d.filter, d.cursor = "", 0
		} else {
			st.discover = nil
		}
	case tea.KeyUp:
		if d.cursor > 0 {
			d.cursor--
		}
	case tea.KeyDown:
		if d.cursor < len(items)-1 {
			d.cursor++
		}
	case tea.KeyTab, tea.KeyShiftTab:
		if (m.lister == nil || m.catalog == nil) || d.loading {
			return true, m, nil
		}
		d.source = (d.source + 1) % sourceCount
		d.cursor = 0
		if !d.current().loaded {
			return true, m, m.fetchSource(d)
		}
	case tea.KeyEnter:
		if d.cursor < len(items) {
			it := items[d.cursor]
			st.form = &modelForm{alias: suggestAlias(it.id), id: it.id, field: fieldAlias}
			st.discover = nil
			st.status = ""
			if it.privacy == model.PrivacyMayRetain {
				st.status = privacyBadge(it.privacy) + " " + modelsPersonaStyle.Render("the provider may keep or train on prompts sent to this model")
			}
		}
	case tea.KeyBackspace:
		if d.filter != "" {
			d.filter = d.filter[:len(d.filter)-1]
			d.cursor = 0
		}
	case tea.KeyRunes, tea.KeySpace:
		d.filter += strings.TrimSpace(string(msg.Runes))
		d.cursor = 0
	}
	return true, m, nil
}

// suggestAlias derives a short alias from a gateway ID: the part after the
// last "/" ("private/z-ai/glm-5.2" -> "glm-5.2"). The user can change it.
func suggestAlias(id string) string {
	if i := strings.LastIndex(id, "/"); i >= 0 && i < len(id)-1 {
		return id[i+1:]
	}
	return id
}

// privacyLabel is the plain-text privacy word for a route.
func privacyLabel(p model.Privacy) string {
	switch p {
	case model.PrivacyLocal:
		return "local"
	case model.PrivacyZDR:
		return "zdr"
	case model.PrivacyMayRetain:
		return "may retain"
	}
	return ""
}

// privacyBadge is privacyLabel styled so the three outcomes read apart at a
// glance: green for no retention, muted for local, amber for may retain.
func privacyBadge(p model.Privacy) string {
	label := privacyLabel(p)
	switch p {
	case model.PrivacyZDR:
		return okBannerStyle.Render(label)
	case model.PrivacyMayRetain:
		return approvalHeaderStyle.Render(label)
	case model.PrivacyLocal:
		return modelsPersonaStyle.Render(label)
	}
	return label
}

// discoverHint is the key hint for the view.
func discoverHint(d *discoverState, both bool) string {
	h := "↑/↓ move · type to filter · Enter add · Esc back"
	if both {
		h = "Tab source · " + h
	}
	return h
}

// discoverLines renders the discover view body, windowed around the cursor.
func discoverLines(d *discoverState, both bool, width, height int) []string {
	var head []string
	tabs := " " + askHeaderStyle.Render(d.source.label())
	if both {
		other := sourceGateway
		if d.source == sourceGateway {
			other = sourceCatalog
		}
		tabs += headerStyle.Render("  (Tab: " + other.label() + ")")
	}
	head = append(head, tabs)
	l := d.current()
	switch {
	case d.loading:
		return append(head, " "+headerStyle.Render("fetching..."))
	case l.err != "":
		// Wrap rather than clip: the useful part of a transport error is often last.
		wrapped := lipgloss.NewStyle().Width(max(width-2, 10)).Render("could not list models: " + l.err)
		for _, ln := range strings.Split(wrapped, "\n") {
			head = append(head, " "+errStyle(strings.TrimRight(ln, " ")))
		}
		return head
	case len(l.items) == 0:
		return append(head, " "+headerStyle.Render(fmt.Sprintf("nothing new - all %d models are already registered", l.total)))
	}

	items := d.visible()
	summary := fmt.Sprintf("%d new of %d", len(l.items), l.total)
	if d.filter != "" {
		summary = fmt.Sprintf("%d match of %d new", len(items), len(l.items))
	}
	head = append(head, " "+headerStyle.Render(summary)+"  "+askKeysStyle.Render("filter: ")+d.filter+"▌", "")
	if len(items) == 0 {
		return append(head, " "+headerStyle.Render("no match"))
	}

	room := height - len(head)
	if room < 1 {
		room = 1
	}
	start := 0
	if d.cursor >= room {
		start = d.cursor - room + 1
	}
	idW := 0
	for _, it := range items {
		idW = max(idW, lipgloss.Width(it.id))
	}
	idW = min(idW, max(width/2, 12))
	lines := head
	for i := start; i < len(items) && i < start+room; i++ {
		it := items[i]
		prefix, style := "  ", humanMsgStyle
		if i == d.cursor {
			prefix, style = askCursorStyle.Render("❯ "), askSelectedStyle
		}
		id := truncateCells(tableCell(it.id), idW)
		pad := strings.Repeat(" ", max(idW-lipgloss.Width(id), 0))
		row := " " + prefix + style.Render(id) + pad + "  " + padRight(privacyBadge(it.privacy), privacyLabel(it.privacy), 10)
		if it.detail != "" {
			row += "  " + modelsPersonaStyle.Render(it.detail)
		}
		lines = append(lines, row)
	}
	return lines
}

// padRight pads a styled string out to w cells, measuring the plain text.
func padRight(styled, plain string, w int) string {
	return styled + strings.Repeat(" ", max(w-lipgloss.Width(plain), 0))
}
