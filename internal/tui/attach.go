package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/artifact"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/tools"
)

// attachStoreTimeout bounds the artifact-store writes made while loading images.
const attachStoreTimeout = 10 * time.Second

var placeholderRE = regexp.MustCompile(`\[Image #(\d+)\]`)

// trailingPlaceholderRE matches a placeholder at the end of the text.
var trailingPlaceholderRE = regexp.MustCompile(`\[Image #(\d+)\]$`)

// draftAttachment is an image queued for the next message, keyed by the number
// in its [Image #N] placeholder.
type draftAttachment struct {
	n   int
	att model.Attachment
}

// clipboardResultMsg is produced by readClipboardCmd. Paths and Data have already
// been loaded and stored inside the cmd goroutine.
type clipboardResultMsg struct {
	atts    []model.Attachment // from Paths (all images) or Data
	text    string             // Text fallback, or Paths joined with spaces when any was not an image
	loadErr error              // Data, or an all-image Paths set, sniffed as image but failed the loader
	err     error              // reader error (no tool, unsupported OS, timeout, other)
}

// attachCmdResultMsg is produced by attachFilesCmd (/attach).
type attachCmdResultMsg struct {
	atts []model.Attachment
	errs []string
}

func placeholder(n int) string { return fmt.Sprintf("[Image #%d]", n) }

func (m *ShellModel) notice(s string) { m.appendLine(headerStyle.Render(s)) }

// draftRefsN returns the queued images whose placeholders appear in line, in
// order of first appearance. Numbers with nothing queued are plain text.
func (m ShellModel) draftRefsN(line string) []draftAttachment {
	var out []draftAttachment
	seen := map[int]bool{}
	for _, sm := range placeholderRE.FindAllStringSubmatch(line, -1) {
		n, err := strconv.Atoi(sm[1])
		if err != nil || seen[n] {
			continue
		}
		for _, d := range m.draftAtts {
			if d.n == n {
				seen[n] = true
				out = append(out, d)
				break
			}
		}
	}
	return out
}

// draftRefs is the attachments a message with this text carries.
func (m ShellModel) draftRefs(line string) []model.Attachment {
	ds := m.draftRefsN(line)
	if len(ds) == 0 {
		return nil
	}
	out := make([]model.Attachment, len(ds))
	for i, d := range ds {
		out[i] = d.att
	}
	return out
}

// pruneDraft drops queued images whose placeholder is no longer in the input.
func (m *ShellModel) pruneDraft() {
	val := m.input.Value()
	kept := m.draftAtts[:0:0]
	for _, d := range m.draftAtts {
		if strings.Contains(val, placeholder(d.n)) {
			kept = append(kept, d)
		}
	}
	m.draftAtts = kept
}

// addAttachments numbers and queues atts, unless that would put more than
// agent.MaxLiveAttachments images on one message; then it prints the cap notice
// and queues nothing. It returns the new placeholder numbers.
func (m *ShellModel) addAttachments(atts []model.Attachment) ([]int, bool) {
	m.pruneDraft()
	if len(m.draftRefs(m.input.Value()))+len(atts) > agent.MaxLiveAttachments {
		m.notice(fmt.Sprintf("not attached: at most %d images per message; remove one first", agent.MaxLiveAttachments))
		return nil, false
	}
	ns := make([]int, len(atts))
	for i, a := range atts {
		m.attachSeq++
		ns[i] = m.attachSeq
		m.draftAtts = append(m.draftAtts, draftAttachment{n: m.attachSeq, att: a})
	}
	return ns, true
}

// refreshCompleter re-syncs the slash completer with the input after an edit.
func (m *ShellModel) refreshCompleter() {
	if m.completer != nil && !m.running {
		if val := m.input.Value(); strings.HasPrefix(val, "/") {
			m.completer.activate(val)
		} else {
			m.completer.deactivate()
		}
		m.refreshViewport(m.vp.AtBottom())
	}
}

// insertPasteText feeds the textinput a paste, so the cursor and scrolling behave
// as for any paste, then refreshes the completer.
func (m *ShellModel) insertPasteText(raw string) {
	m.input, _ = m.input.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(raw), Paste: true})
	m.refreshCompleter()
}

// insertPlaceholders puts the placeholders for ns in the input: at the cursor,
// separated by spaces with one trailing space (and a leading space when the rune
// before the cursor is not one), or, with replace, as the whole value.
func (m *ShellModel) insertPlaceholders(ns []int, replace bool) {
	parts := make([]string, len(ns))
	for i, n := range ns {
		parts[i] = placeholder(n)
	}
	text := strings.Join(parts, " ") + " "
	if replace {
		m.input.SetValue(text)
		m.input.CursorEnd()
		m.refreshCompleter()
		return
	}
	if pos := m.input.Position(); pos > 0 {
		if rs := []rune(m.input.Value()); pos <= len(rs) && rs[pos-1] != ' ' {
			text = " " + text
		}
	}
	m.insertPasteText(text)
}

// acceptsAttachments reports whether the active model takes image parts.
func (m ShellModel) acceptsAttachments() bool {
	if m.reg == nil {
		return false
	}
	mc, err := m.reg.Resolve(m.alias)
	return err == nil && mc.AcceptsAttachments()
}

// attachStore is the artifact binding to store new images in: none for a model
// that will not get them anyway.
func (m ShellModel) attachStore() artifact.Binding {
	if !m.acceptsAttachments() {
		return artifact.Binding{}
	}
	return m.artifacts
}

// attachSubmitBlock returns why a line carrying images cannot be sent now, or "".
func (m ShellModel) attachSubmitBlock(line string) string {
	if m.running {
		return "images go with a new prompt: wait for this turn to finish (or /interrupt it), then press Enter"
	}
	if m.humanBus != nil && m.handleReg != nil {
		r := classifyInput(line, m.tree, m.handleReg, m.running, len(m.asks) > 0, m.selectedNodeID())
		if r.Kind != routeNormal && r.Kind != routeRespond {
			return "images can only go with a plain prompt, not a routed message (@name, /btw)"
		}
	}
	if !m.acceptsAttachments() {
		return fmt.Sprintf("%s does not accept images. Switch with /model NAME, or delete the [Image #N] placeholders to send the text alone.", m.alias)
	}
	return ""
}

// backspacePlaceholder deletes a whole queued [Image #N] placeholder that ends
// right before the cursor. It reports whether it did.
func (m *ShellModel) backspacePlaceholder() bool {
	rs := []rune(m.input.Value())
	pos := m.input.Position()
	if pos > len(rs) || pos == 0 {
		return false
	}
	before := string(rs[:pos])
	loc := trailingPlaceholderRE.FindStringSubmatchIndex(before)
	if loc == nil {
		return false
	}
	n, _ := strconv.Atoi(before[loc[2]:loc[3]])
	queued := false
	for _, d := range m.draftAtts {
		if d.n == n {
			queued = true
		}
	}
	if !queued {
		return false
	}
	keep := before[:loc[0]]
	m.input.SetValue(keep + string(rs[pos:]))
	m.input.SetCursor(len([]rune(keep)))
	return true
}

// attachIndicator is the status-line fragment for images queued on the draft.
func (m ShellModel) attachIndicator() string {
	switch n := len(m.draftRefs(m.input.Value())); n {
	case 0:
		return ""
	case 1:
		return " " + headerStyle.Render("1 image attached")
	default:
		return " " + headerStyle.Render(fmt.Sprintf("%d images attached", n))
	}
}

// dropCandidate returns the absolute paths in a paste that may be a file drop,
// or nil. Ordinary text never reaches the disk: it must parse as 1..16 absolute
// paths, and a slash command being typed keeps pastes as text.
func (m ShellModel) dropCandidate(raw string) []string {
	if len(raw) > maxDropBytes || strings.HasPrefix(m.input.Value(), "/") {
		return nil
	}
	home, _ := m.home()
	paths, ok := parseDrop(raw, home, m.goos)
	if !ok || len(paths) > maxDropTokens {
		return nil
	}
	return paths
}

// loadDropped stats and loads each path. notDrop means some path is missing, not
// a regular file, or not an image, so the paste is text. loadErr is the first
// loader failure when every path was an image. Loaded images are stored in b.
func loadDropped(paths []string, b artifact.Binding) (atts []model.Attachment, notDrop bool, loadErr error) {
	for _, p := range paths {
		fi, err := os.Stat(p)
		if err != nil || !fi.Mode().IsRegular() {
			return nil, true, nil
		}
		att, err := tools.LoadAttachmentFile(p)
		var ni *tools.NotImageError
		switch {
		case errors.As(err, &ni):
			return nil, true, nil
		case err != nil:
			if loadErr == nil {
				loadErr = err
			}
		default:
			atts = append(atts, att)
		}
	}
	if loadErr != nil {
		return nil, false, loadErr
	}
	storeAll(b, atts)
	return atts, false, nil
}

// storeAll puts each attachment in the session artifact store, setting its Ref.
func storeAll(b artifact.Binding, atts []model.Attachment) {
	ctx, cancel := context.WithTimeout(context.Background(), attachStoreTimeout)
	defer cancel()
	for i := range atts {
		tools.StoreAttachment(ctx, b, &atts[i], "shell")
	}
}

// handlePastedRunes is the one paste entry point (typed paste and the clipboard
// text fallback). If the paste is entirely image files, they are loaded here,
// synchronously, and become [Image #N] placeholders. Otherwise, and whenever an
// image is refused (not accepted by the model, over the cap, failed the loader),
// the pasted text goes in unchanged, with a notice for a refused image.
func (m *ShellModel) handlePastedRunes(raw string) {
	paths := m.dropCandidate(raw)
	if paths == nil {
		m.insertPasteText(raw)
		return
	}
	atts, notDrop, loadErr := loadDropped(paths, m.attachStore())
	switch {
	case notDrop:
		m.insertPasteText(raw)
	case loadErr != nil:
		m.insertPasteText(raw)
		m.notice("not attached: " + loadErr.Error())
	case !m.acceptsAttachments():
		m.insertPasteText(raw)
		what := "the path was"
		if len(paths) > 1 {
			what = "the paths were"
		}
		m.notice(fmt.Sprintf("%s does not accept images, so %s pasted as text. Switch with /model NAME, or set attachments: true on its config entry if it does.", m.alias, what))
	default:
		ns, ok := m.addAttachments(atts)
		if !ok {
			m.insertPasteText(raw)
			return
		}
		m.insertPlaceholders(ns, false)
	}
}

// attachFilesCmd loads and stores each path for /attach, independently.
func attachFilesCmd(paths []string, b artifact.Binding) tea.Cmd {
	return func() tea.Msg {
		var res attachCmdResultMsg
		for _, p := range paths {
			att, err := tools.LoadAttachmentFile(p)
			if err != nil {
				res.errs = append(res.errs, err.Error())
				continue
			}
			res.atts = append(res.atts, att)
		}
		storeAll(b, res.atts)
		return res
	}
}

// readClipboardCmd reads the clipboard off the Update goroutine and loads and
// stores whatever image it holds. No temp file outlives the read.
func readClipboardCmd(r clipboardReader, b artifact.Binding) tea.Cmd {
	return func() tea.Msg {
		c, err := r.Read(context.Background())
		if err != nil {
			return clipboardResultMsg{err: err}
		}
		var res clipboardResultMsg
		switch {
		case len(c.Paths) > 0:
			var atts []model.Attachment
			var firstErr error
			notImage := false
			for _, p := range c.Paths {
				att, err := tools.LoadAttachmentFile(p)
				var ni *tools.NotImageError
				switch {
				case errors.As(err, &ni):
					notImage = true
				case err != nil:
					if firstErr == nil {
						firstErr = err
					}
				default:
					atts = append(atts, att)
				}
			}
			switch {
			case notImage:
				res.text = strings.Join(c.Paths, " ")
			case firstErr != nil:
				res.loadErr = firstErr
			default:
				storeAll(b, atts)
				res.atts = atts
			}
		case len(c.Data) > 0:
			att, err := tools.LoadAttachmentBytes("clipboard", c.Data)
			var ni *tools.NotImageError
			switch {
			case errors.As(err, &ni):
				res.text = c.Text
			case err != nil:
				res.loadErr = err
			default:
				atts := []model.Attachment{att}
				storeAll(b, atts)
				res.atts = atts
			}
		default:
			res.text = c.Text
		}
		return res
	}
}

func (m ShellModel) handleClipboardResult(msg clipboardResultMsg) (tea.Model, tea.Cmd) {
	m.clipBusy = false
	if msg.err != nil {
		switch {
		case errors.Is(msg.err, errClipboardUnsupported):
			m.notice(fmt.Sprintf("pasting images is not supported on %s yet; drag the file in or use /attach PATH", m.goos))
			return m, textinput.Paste
		case errors.Is(msg.err, errClipboardNoTool), errors.Is(msg.err, errClipboardTimeout):
			m.notice(msg.err.Error())
		default:
			m.notice("reading the clipboard failed: " + msg.err.Error())
		}
		return m, nil
	}
	switch {
	case len(msg.atts) > 0:
		if !m.acceptsAttachments() {
			m.notice(fmt.Sprintf("%s does not accept images, so the clipboard image was not attached. Switch with /model NAME, or set attachments: true on its config entry if it does.", m.alias))
			return m, nil
		}
		if ns, ok := m.addAttachments(msg.atts); ok {
			m.insertPlaceholders(ns, false)
		}
	case msg.loadErr != nil:
		m.notice("not attached: " + msg.loadErr.Error())
	case msg.text != "":
		m.handlePastedRunes(msg.text)
	default:
		m.notice("no image on the clipboard")
	}
	return m, nil
}

func (m ShellModel) handleAttachCmdResult(msg attachCmdResultMsg) (tea.Model, tea.Cmd) {
	for _, e := range msg.errs {
		m.notice("not attached: " + e)
	}
	if len(msg.atts) > 0 {
		if ns, ok := m.addAttachments(msg.atts); ok {
			m.insertPlaceholders(ns, true)
		}
	}
	return m, nil
}

// attachCommand handles /attach PATH...
func (m ShellModel) attachCommand(line string) (tea.Model, tea.Cmd) {
	rest := strings.TrimSpace(strings.TrimPrefix(line, "/attach"))
	if rest == "" {
		m.notice("usage: /attach PATH... - attach images (PNG, JPEG, GIF or WebP) to your next message")
		return m, nil
	}
	if !m.acceptsAttachments() {
		m.notice(fmt.Sprintf("%s does not accept images, so nothing was attached. Switch with /model NAME, or set attachments: true on its config entry if it does.", m.alias))
		return m, nil
	}
	home, _ := m.home()
	cwd, _ := m.getwd()
	paths, ok := parseAttachArgs(rest, home, cwd, m.goos)
	if !ok {
		m.notice("could not parse the path; quote paths that contain spaces")
		return m, nil
	}
	return m, attachFilesCmd(paths, m.artifacts)
}

// imageEchoLines is the transcript line for each image sent with a prompt.
func imageEchoLines(ds []draftAttachment) []string {
	out := make([]string, len(ds))
	for i, d := range ds {
		name := sanitizeDisplay(filepath.Base(d.att.Name))
		out[i] = headerStyle.Render(fmt.Sprintf("  ⎿ %s %s · %s", placeholder(d.n), name, sanitizeDisplay(tools.AttachmentSummary(d.att))))
	}
	return out
}

// historyHasAttachments reports whether any message in history carries an image.
func historyHasAttachments(h []model.Message) bool {
	for _, msg := range h {
		if len(msg.Attachments) > 0 {
			return true
		}
	}
	return false
}
