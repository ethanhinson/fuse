package tui

import (
	"bytes"
	"image"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ethanhinson/fuse/internal/artifact"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/permissions"
)

func ptr[T any](v T) *T { return &v }

// attachRegistry has a vision model ("vis") and a text-only one ("txt").
func attachRegistry() *model.Registry {
	return model.NewRegistry("vis", map[string]model.ModelConfig{
		"vis": {ID: "prov/vis", Attachments: ptr(true)},
		"txt": {ID: "prov/txt"},
	})
}

// runUntil runs cmd, unwraps tea.BatchMsg recursively and returns the first
// message of type T.
func runUntil[T any](t *testing.T, cmd tea.Cmd) T {
	t.Helper()
	var find func(c tea.Cmd) (T, bool)
	find = func(c tea.Cmd) (T, bool) {
		var zero T
		if c == nil {
			return zero, false
		}
		switch v := c().(type) {
		case T:
			return v, true
		case tea.BatchMsg:
			for _, sub := range v {
				if got, ok := find(sub); ok {
					return got, true
				}
			}
		}
		return zero, false
	}
	got, ok := find(cmd)
	if !ok {
		t.Fatalf("no %T produced by cmd", got)
	}
	return got
}

func newAttachModel(t *testing.T) (ShellModel, *artifact.MemStore, string) {
	t.Helper()
	store := artifact.NewMemStore()
	dir := t.TempDir()
	m := sized(NewShellModel("vis", false, "dark", attachRegistry(), nil, nilBuilder, permissions.NewSessionMode(permissions.ModeSmart), true))
	m = m.WithArtifacts(artifact.Binding{Store: store, Tenant: "acme", Loop: "loop1"})
	m.goos = "darwin"
	m.getwd = func() (string, error) { return dir, nil }
	m.home = func() (string, error) { return dir, nil }
	return m, store, dir
}

func writeImage(t *testing.T, dir, name string) string {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func writeFile(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func paste(m ShellModel, s string) (ShellModel, tea.Cmd) {
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s), Paste: true})
	return next.(ShellModel), cmd
}

func pressKey(m ShellModel, k tea.KeyType) (ShellModel, tea.Cmd) {
	next, cmd := m.Update(tea.KeyMsg{Type: k})
	return next.(ShellModel), cmd
}

func typeText(m ShellModel, s string) ShellModel {
	for _, r := range s {
		next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = next.(ShellModel)
	}
	return m
}

func TestDropImagePathBecomesPlaceholder(t *testing.T) {
	m, _, dir := newAttachModel(t)
	m, cmd := paste(m, writeImage(t, dir, "a.png"))
	if cmd != nil {
		t.Errorf("a drop is resolved synchronously, got a cmd")
	}
	if got := m.input.Value(); got != "[Image #1] " {
		t.Fatalf("input = %q", got)
	}
	if len(m.draftAtts) != 1 || m.draftAtts[0].n != 1 {
		t.Fatalf("draftAtts = %+v", m.draftAtts)
	}
	if !strings.Contains(m.View(), "1 image attached") {
		t.Error("status should show 1 image attached")
	}
}

func TestDropVariantsAttachTheSameFile(t *testing.T) {
	dir := t.TempDir()
	p := writeImage(t, dir, "my shot.png")
	narrow := writeImage(t, dir, "Screenshot 10.12 AM.png")
	cases := map[string]string{
		"escaped space":  strings.ReplaceAll(p, " ", `\ `) + " ",
		"single quoted":  "'" + p + "'",
		"double quoted":  `"` + p + `"`,
		"file url":       "file://" + strings.ReplaceAll(p, " ", "%20"),
		"narrow nbsp":    strings.ReplaceAll(narrow, " ", `\ `),
		"escaped narrow": strings.ReplaceAll(narrow, " ", `\ `) + " ",
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			m, _, _ := newAttachModel(t)
			m, _ = paste(m, in)
			if m.input.Value() != "[Image #1] " || len(m.draftAtts) != 1 {
				t.Fatalf("input = %q drafts = %d", m.input.Value(), len(m.draftAtts))
			}
		})
	}
}

func TestDropMultipleImages(t *testing.T) {
	m, _, dir := newAttachModel(t)
	m, _ = paste(m, writeImage(t, dir, "a.png")+" "+writeImage(t, dir, "b.png"))
	if got := m.input.Value(); got != "[Image #1] [Image #2] " {
		t.Fatalf("input = %q", got)
	}
}

func TestDropMixedImageAndTextFileIsText(t *testing.T) {
	m, _, dir := newAttachModel(t)
	raw := writeImage(t, dir, "a.png") + " " + writeFile(t, dir, "notes.txt", []byte("hello\n"))
	m, _ = paste(m, raw)
	if m.input.Value() != raw || len(m.draftAtts) != 0 {
		t.Fatalf("input = %q drafts = %d", m.input.Value(), len(m.draftAtts))
	}
}

func TestDropMissingFileIsText(t *testing.T) {
	m, _, dir := newAttachModel(t)
	raw := filepath.Join(dir, "gone.png")
	m, _ = paste(m, raw)
	if m.input.Value() != raw || len(m.draftAtts) != 0 {
		t.Fatalf("input = %q", m.input.Value())
	}
	if strings.Contains(plainLines(m), "not attached") {
		t.Error("a missing file is plain text and says nothing")
	}
}

func TestDropDirectoryIsText(t *testing.T) {
	m, _, dir := newAttachModel(t)
	m, _ = paste(m, dir)
	if m.input.Value() != dir || len(m.draftAtts) != 0 {
		t.Fatalf("input = %q", m.input.Value())
	}
}

func TestPasteOrdinaryTextIsSynchronous(t *testing.T) {
	m, _, _ := newAttachModel(t)
	m, cmd := paste(m, "hello world")
	if cmd != nil {
		t.Error("ordinary text must not produce a cmd")
	}
	if m.input.Value() != "hello world" {
		t.Fatalf("input = %q", m.input.Value())
	}
}

func TestPasteRelativePathIsText(t *testing.T) {
	m, _, dir := newAttachModel(t)
	writeImage(t, dir, "a.png")
	m, cmd := paste(m, "a.png")
	if cmd != nil || m.input.Value() != "a.png" || len(m.draftAtts) != 0 {
		t.Fatalf("input = %q cmd = %v", m.input.Value(), cmd != nil)
	}
}

func TestDropWhileTypingSlashIsText(t *testing.T) {
	m, _, dir := newAttachModel(t)
	p := writeImage(t, dir, "a.png")
	m = typeText(m, "/attach ")
	m, _ = paste(m, p)
	if got, want := m.input.Value(), "/attach "+p; got != want || len(m.draftAtts) != 0 {
		t.Fatalf("input = %q want %q", got, want)
	}
}

func TestDropInsertsAtCursorWithSpacing(t *testing.T) {
	m, _, dir := newAttachModel(t)
	m = typeText(m, "look at")
	m, _ = paste(m, writeImage(t, dir, "a.png"))
	if got := m.input.Value(); got != "look at [Image #1] " {
		t.Fatalf("input = %q", got)
	}
}

func TestDropBMPShowsLoadErrorAndKeepsText(t *testing.T) {
	m, _, dir := newAttachModel(t)
	raw := writeFile(t, dir, "a.bmp", append([]byte("BM"), make([]byte, 64)...))
	m, _ = paste(m, raw)
	out := plainLines(m)
	if m.input.Value() != raw || len(m.draftAtts) != 0 {
		t.Fatalf("input = %q", m.input.Value())
	}
	if !strings.Contains(out, "not attached:") || !strings.Contains(out, "cannot be attached") {
		t.Errorf("transcript = %q", out)
	}
}

func TestDropCapAtFour(t *testing.T) {
	m, _, dir := newAttachModel(t)
	var paths []string
	for _, n := range []string{"1", "2", "3", "4"} {
		paths = append(paths, writeImage(t, dir, n+".png"))
	}
	m, _ = paste(m, strings.Join(paths, " "))
	if len(m.draftAtts) != 4 {
		t.Fatalf("drafts = %d", len(m.draftAtts))
	}
	before := m.input.Value()
	extra := writeImage(t, dir, "5.png")
	m, _ = paste(m, extra)
	if len(m.draftAtts) != 4 {
		t.Errorf("drafts = %d, want 4", len(m.draftAtts))
	}
	// Reviewer change: a refused image keeps its path text, with the cap notice.
	if got, want := m.input.Value(), before+extra; got != want {
		t.Errorf("input = %q want %q", got, want)
	}
	if !strings.Contains(plainLines(m), "not attached: at most 4 images per message; remove one first") {
		t.Error("cap notice missing")
	}
}

func TestDropTextOnlyModelPastesPathAndWarns(t *testing.T) {
	m, _, dir := newAttachModel(t)
	m.alias = "txt"
	p := writeImage(t, dir, "a.png")
	m, _ = paste(m, p)
	if m.input.Value() != p || len(m.draftAtts) != 0 {
		t.Fatalf("input = %q", m.input.Value())
	}
	out := plainLines(m)
	if !strings.Contains(out, "txt does not accept images, so the path was pasted as text.") {
		t.Errorf("transcript = %q", out)
	}
	m2, _, _ := newAttachModel(t)
	m2.alias = "txt"
	m2, _ = paste(m2, p+" "+writeImage(t, dir, "b.png"))
	if !strings.Contains(plainLines(m2), "the paths were pasted as text") {
		t.Error("plural notice missing")
	}
}

func TestBackspaceRemovesWholePlaceholder(t *testing.T) {
	m, _, _ := newAttachModel(t)
	m.draftAtts = []draftAttachment{{n: 1}}
	m.input.SetValue("x [Image #1]")
	m.input.CursorEnd()
	m, _ = pressKey(m, tea.KeyBackspace)
	if m.input.Value() != "x " {
		t.Fatalf("input = %q", m.input.Value())
	}
	m, _ = pressKey(m, tea.KeyBackspace)
	if m.input.Value() != "x" {
		t.Fatalf("second backspace: %q", m.input.Value())
	}
}

func TestBackspaceOnUnknownPlaceholderIsNormal(t *testing.T) {
	m, _, _ := newAttachModel(t)
	m.input.SetValue("[Image #9]")
	m.input.CursorEnd()
	m, _ = pressKey(m, tea.KeyBackspace)
	if m.input.Value() != "[Image #9" {
		t.Fatalf("input = %q", m.input.Value())
	}
}

func dropTwo(t *testing.T) (ShellModel, string) {
	t.Helper()
	m, _, dir := newAttachModel(t)
	m, _ = paste(m, writeImage(t, dir, "a.png")+" "+writeImage(t, dir, "b.png"))
	return m, dir
}

func TestSubmitCarriesOnlyReferencedImages(t *testing.T) {
	m, _ := dropTwo(t)
	second := m.draftAtts[1].att
	m.input.SetValue("[Image #2] describe")
	m, _ = pressKey(m, tea.KeyEnter)
	if len(m.history) != 1 || len(m.history[0].Attachments) != 1 || m.history[0].Attachments[0].SHA256 != second.SHA256 || m.history[0].Attachments[0].Name != second.Name {
		t.Fatalf("history = %+v", m.history)
	}
	if m.history[0].Content != "[Image #2] describe" {
		t.Errorf("content = %q", m.history[0].Content)
	}
	if m.draftAtts != nil {
		t.Error("draft not cleared")
	}
}

func TestSubmitOrderFollowsPlaceholders(t *testing.T) {
	m, _ := dropTwo(t)
	first, second := m.draftAtts[0].att, m.draftAtts[1].att
	m.input.SetValue("[Image #2] then [Image #1] and [Image #2] again")
	m, _ = pressKey(m, tea.KeyEnter)
	got := m.history[0].Attachments
	if len(got) != 2 || got[0].Name != second.Name || got[1].Name != first.Name {
		t.Fatalf("attachments = %+v", got)
	}
}

func TestSubmitTranscriptShowsImageLines(t *testing.T) {
	m, _, dir := newAttachModel(t)
	m, _ = paste(m, writeImage(t, dir, "a.png"))
	m, _ = pressKey(m, tea.KeyEnter)
	if out := plainLines(m); !strings.Contains(out, "⎿ [Image #1] a.png · PNG 2x2,") {
		t.Errorf("transcript = %q", out)
	}
}

func TestPlaceholderNumbersAreSessionWide(t *testing.T) {
	m, _, dir := newAttachModel(t)
	p := writeImage(t, dir, "a.png")
	m, _ = paste(m, p)
	m, _ = pressKey(m, tea.KeyEnter)
	m.running = false
	m, _ = paste(m, p)
	if m.input.Value() != "[Image #2] " {
		t.Fatalf("input = %q", m.input.Value())
	}
	m.input.Reset()
	m = typeAndEnter(m, "/clear")
	m, _ = paste(m, p)
	if m.input.Value() != "[Image #1] " {
		t.Fatalf("after /clear input = %q", m.input.Value())
	}
}

func TestSubmitBlockedWhileRunning(t *testing.T) {
	m, _, dir := newAttachModel(t)
	m, _ = paste(m, writeImage(t, dir, "a.png"))
	m.running = true
	m, _ = pressKey(m, tea.KeyEnter)
	if m.input.Value() != "[Image #1] " || len(m.history) != 0 {
		t.Fatalf("input = %q history = %d", m.input.Value(), len(m.history))
	}
	if !strings.Contains(plainLines(m), "images go with a new prompt: wait for this turn to finish") {
		t.Error("notice missing")
	}
}

func TestSubmitBlockedOnTextOnlyModel(t *testing.T) {
	m, _, dir := newAttachModel(t)
	m, _ = paste(m, writeImage(t, dir, "a.png"))
	m.alias = "txt" // as after a /config switch
	m, _ = pressKey(m, tea.KeyEnter)
	if m.input.Value() != "[Image #1] " || len(m.history) != 0 {
		t.Fatalf("input = %q", m.input.Value())
	}
	if !strings.Contains(plainLines(m), "txt does not accept images. Switch with /model NAME, or delete the [Image #N] placeholders to send the text alone.") {
		t.Errorf("transcript = %q", plainLines(m))
	}
}

func TestSubmitBlockedForRoutedMessage(t *testing.T) {
	m, bus, _ := shellWithHumanMessaging(t)
	m.running = false
	m.reg = attachRegistry()
	m.alias = "vis"
	m.draftAtts = []draftAttachment{{n: 1, att: model.Attachment{Name: "a.png"}}}
	m.input.SetValue("@coder see [Image #1]")
	m, _ = pressKey(m, tea.KeyEnter)
	if m.input.Value() != "@coder see [Image #1]" {
		t.Fatalf("input = %q", m.input.Value())
	}
	if !strings.Contains(plainLines(m), "images can only go with a plain prompt, not a routed message") {
		t.Errorf("transcript = %q", plainLines(m))
	}
	if len(bus.Pending("coder-1")) != 0 {
		t.Error("message must not be routed")
	}
}

func TestSkillCommandCarriesImages(t *testing.T) {
	m, _, dir := newAttachModel(t)
	m.slashReg = registryWith(skillEntry("/review", "review body", "review"))
	m, _ = paste(m, writeImage(t, dir, "a.png"))
	m.input.SetValue("/review [Image #1]")
	m, _ = pressKey(m, tea.KeyEnter)
	if len(m.history) != 1 {
		t.Fatalf("history = %+v (transcript %q)", m.history, plainLines(m))
	}
	h := m.history[0]
	if !strings.HasSuffix(h.Content, "ARGUMENTS: [Image #1]") || len(h.Attachments) != 1 {
		t.Fatalf("message = %+v", h)
	}
}

func TestModelSwitchWarnsAboutHistoryImages(t *testing.T) {
	m, _, _ := newAttachModel(t)
	m.history = []model.Message{{Role: "user", Content: "x", Attachments: []model.Attachment{{Name: "a.png"}}}}
	m = typeAndEnter(m, "/model txt")
	if !strings.Contains(plainLines(m), "note: txt does not accept images; earlier images in this conversation reach it as text notes") {
		t.Errorf("transcript = %q", plainLines(m))
	}
	m = typeAndEnter(m, "/model vis")
	if strings.Count(plainLines(m), "note: ") != 1 {
		t.Error("/model vis must not warn")
	}
}

func TestAttachCommandRelativePath(t *testing.T) {
	m, _, dir := newAttachModel(t)
	writeImage(t, dir, "a.png")
	m.input.SetValue("/attach a.png")
	m, cmd := pressKey(m, tea.KeyEnter)
	res := runUntil[attachCmdResultMsg](t, cmd)
	next, _ := m.Update(res)
	m = next.(ShellModel)
	if m.input.Value() != "[Image #1] " {
		t.Fatalf("input = %q", m.input.Value())
	}
}

func TestAttachCommandQuotedAndMultiple(t *testing.T) {
	m, _, dir := newAttachModel(t)
	writeImage(t, dir, "my shot.png")
	writeImage(t, dir, "b.png")
	m.input.SetValue("/attach 'my shot.png' b.png")
	m, cmd := pressKey(m, tea.KeyEnter)
	next, _ := m.Update(runUntil[attachCmdResultMsg](t, cmd))
	m = next.(ShellModel)
	if m.input.Value() != "[Image #1] [Image #2] " {
		t.Fatalf("input = %q", m.input.Value())
	}
}

func TestAttachCommandErrors(t *testing.T) {
	m, _, dir := newAttachModel(t)
	writeFile(t, dir, "notes.txt", []byte("hi"))
	m.input.SetValue("/attach gone.png notes.txt")
	m, cmd := pressKey(m, tea.KeyEnter)
	next, _ := m.Update(runUntil[attachCmdResultMsg](t, cmd))
	m = next.(ShellModel)
	out := plainLines(m)
	if !strings.Contains(out, "not attached: open ") || !strings.Contains(out, "is not a PNG, JPEG, GIF or WebP image") {
		t.Errorf("transcript = %q", out)
	}
	if m.input.Value() != "" || len(m.draftAtts) != 0 {
		t.Errorf("input = %q", m.input.Value())
	}
}

func TestAttachCommandTextOnlyModel(t *testing.T) {
	m, _, dir := newAttachModel(t)
	m.alias = "txt"
	writeImage(t, dir, "a.png")
	m.input.SetValue("/attach a.png")
	m, cmd := pressKey(m, tea.KeyEnter)
	if cmd != nil {
		t.Error("no cmd expected")
	}
	if !strings.Contains(plainLines(m), "txt does not accept images, so nothing was attached.") {
		t.Errorf("transcript = %q", plainLines(m))
	}
}

func TestAttachCommandUsage(t *testing.T) {
	m, _, _ := newAttachModel(t)
	m.input.SetValue("/attach")
	m, _ = pressKey(m, tea.KeyEnter)
	if !strings.Contains(plainLines(m), "usage: /attach PATH... - attach images (PNG, JPEG, GIF or WebP) to your next message") {
		t.Errorf("transcript = %q", plainLines(m))
	}
}

func TestAttachCommandParseFailure(t *testing.T) {
	m, _, _ := newAttachModel(t)
	m.input.SetValue("/attach 'unterminated")
	m, _ = pressKey(m, tea.KeyEnter)
	if !strings.Contains(plainLines(m), "could not parse the path; quote paths that contain spaces") {
		t.Errorf("transcript = %q", plainLines(m))
	}
}

func TestAttachmentsStoredInArtifactStore(t *testing.T) {
	m, store, dir := newAttachModel(t)
	p := writeImage(t, dir, "a.png")
	want, _ := os.ReadFile(p)
	m, _ = paste(m, p)
	ref := m.draftAtts[0].att.Ref
	if !strings.Contains(ref, "/attachment/") {
		t.Fatalf("ref = %q", ref)
	}
	parsed, err := artifact.ParseRef(ref)
	if err != nil {
		t.Fatal(err)
	}
	rc, _, err := store.Open(t.Context(), parsed)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, want) {
		t.Error("stored bytes differ")
	}
}

func TestTextOnlyModelDoesNotStoreImages(t *testing.T) {
	m, store, dir := newAttachModel(t)
	m.alias = "txt"
	m, _ = paste(m, writeImage(t, dir, "a.png"))
	metas, _ := artifact.Binding{Store: store, Tenant: "acme", Loop: "loop1"}.List(t.Context(), artifact.KindAttachment)
	if len(metas) != 0 {
		t.Errorf("stored %d artifacts for a text-only model", len(metas))
	}
}

func TestAttachBuiltinEntry(t *testing.T) {
	for _, e := range NewBuiltinProvider().Commands() {
		if e.Command == "/attach" {
			if e.Expansion() != "/attach " || e.Kind != KindBuiltin {
				t.Errorf("entry = %+v", e)
			}
			return
		}
	}
	t.Error("/attach builtin missing")
}
