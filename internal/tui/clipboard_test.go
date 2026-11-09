package tui

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/png"
	"reflect"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"golang.design/x/clipboard"
)

func pngFixture(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// fakeBackend scripts the clipboard library: what each format holds or the
// error reading it returns, and which reads were made.
type fakeBackend struct {
	initErr  error
	inits    int
	files    []string
	filesErr error
	data     map[clipboard.Format][]byte
	errs     map[clipboard.Format]error
	reads    []clipboard.Format
}

func (f *fakeBackend) init() error { f.inits++; return f.initErr }

func (f *fakeBackend) readFiles(context.Context) ([]string, error) {
	if f.filesErr != nil {
		return nil, f.filesErr
	}
	if len(f.files) == 0 {
		return nil, clipboard.ErrNoData
	}
	return f.files, nil
}

func (f *fakeBackend) read(_ context.Context, fm clipboard.Format) ([]byte, error) {
	f.reads = append(f.reads, fm)
	if err := f.errs[fm]; err != nil {
		return nil, err
	}
	if d := f.data[fm]; len(d) > 0 {
		return d, nil
	}
	return nil, clipboard.ErrNoData
}

func readClip(t *testing.T, f *fakeBackend) (clipboardContent, error) {
	t.Helper()
	return (&systemClipboard{backend: f}).Read(t.Context())
}

// A file-manager copy wins even when the clipboard also holds the file's icon.
func TestClipboardFilesComeFirst(t *testing.T) {
	f := &fakeBackend{files: []string{"/x/a.png"}, data: map[clipboard.Format][]byte{clipboard.FmtImage: pngFixture(t)}}
	got, err := readClip(t, f)
	if err != nil || !reflect.DeepEqual(got.Paths, []string{"/x/a.png"}) || got.Data != nil {
		t.Fatalf("got %+v err %v", got, err)
	}
	if len(f.reads) != 0 {
		t.Errorf("image or text read after files: %v", f.reads)
	}
}

func TestClipboardImage(t *testing.T) {
	data := pngFixture(t)
	f := &fakeBackend{data: map[clipboard.Format][]byte{clipboard.FmtImage: data, clipboard.FmtText: []byte("caption")}}
	got, err := readClip(t, f)
	if err != nil || !bytes.Equal(got.Data, data) || got.Text != "" {
		t.Fatalf("got %+v err %v", got, err)
	}
}

// Plain text stays text. Live, the old osascript reader turned "hello" into the
// file /hello, because AppleScript coerces text to a file URL.
func TestClipboardTextStaysText(t *testing.T) {
	f := &fakeBackend{data: map[clipboard.Format][]byte{clipboard.FmtText: []byte("hello from the clipboard")}}
	got, err := readClip(t, f)
	if err != nil || len(got.Paths) != 0 || got.Data != nil || got.Text != "hello from the clipboard" {
		t.Fatalf("got %+v err %v", got, err)
	}
}

func TestClipboardEmpty(t *testing.T) {
	got, err := readClip(t, &fakeBackend{})
	if err != nil || got.Paths != nil || got.Data != nil || got.Text != "" {
		t.Fatalf("got %+v err %v", got, err)
	}
}

// No display server (or no clipboard at all): a clear error, and the library
// is initialised once, not on every Ctrl+V.
func TestClipboardUnavailable(t *testing.T) {
	f := &fakeBackend{initErr: errors.New("no display")}
	c := &systemClipboard{backend: f}
	for range 2 {
		if _, err := c.Read(t.Context()); !errors.Is(err, errClipboardUnavailable) || !strings.Contains(err.Error(), "no display") {
			t.Fatalf("err = %v", err)
		}
	}
	if f.inits != 1 {
		t.Errorf("init called %d times", f.inits)
	}
	f2 := &fakeBackend{filesErr: clipboard.ErrUnavailable}
	if _, err := readClip(t, f2); !errors.Is(err, errClipboardUnavailable) {
		t.Errorf("ErrUnavailable from a read: err = %v", err)
	}
}

func TestClipboardTimeout(t *testing.T) {
	f := &fakeBackend{errs: map[clipboard.Format]error{clipboard.FmtImage: context.DeadlineExceeded}}
	if _, err := readClip(t, f); !errors.Is(err, errClipboardTimeout) {
		t.Fatalf("err = %v", err)
	}
}

func TestClipboardOtherErrorSurfaces(t *testing.T) {
	f := &fakeBackend{errs: map[clipboard.Format]error{clipboard.FmtText: errors.New("boom")}}
	if _, err := readClip(t, f); err == nil || err.Error() != "boom" {
		t.Fatalf("err = %v", err)
	}
}

// fakeReader is an injected clipboardReader.
type fakeReader struct {
	c    clipboardContent
	err  error
	read int
}

func (f *fakeReader) Read(context.Context) (clipboardContent, error) {
	f.read++
	return f.c, f.err
}

func ctrlV(t *testing.T, m ShellModel) ShellModel {
	t.Helper()
	m, cmd := pressKey(m, tea.KeyCtrlV)
	next, _ := m.Update(runUntil[clipboardResultMsg](t, cmd))
	return next.(ShellModel)
}

func TestCtrlVAttachesClipboardImage(t *testing.T) {
	m, _, _ := newAttachModel(t)
	m = m.WithClipboard(&fakeReader{c: clipboardContent{Data: pngFixture(t)}})
	m = ctrlV(t, m)
	if m.input.Value() != "[Image #1] " || len(m.draftAtts) != 1 || m.draftAtts[0].att.Name != "clipboard" {
		t.Fatalf("input = %q drafts = %+v", m.input.Value(), m.draftAtts)
	}
	if !strings.Contains(m.draftAtts[0].att.Ref, "/attachment/") {
		t.Errorf("not stored: %q", m.draftAtts[0].att.Ref)
	}
}

func TestCtrlVTextFallsBackToPaste(t *testing.T) {
	m, _, _ := newAttachModel(t)
	m = m.WithClipboard(&fakeReader{c: clipboardContent{Text: "hello"}})
	m = ctrlV(t, m)
	if m.input.Value() != "hello" {
		t.Fatalf("input = %q", m.input.Value())
	}
}

func TestCtrlVTextPathBecomesAttachment(t *testing.T) {
	m, _, dir := newAttachModel(t)
	m = m.WithClipboard(&fakeReader{c: clipboardContent{Text: writeImage(t, dir, "a.png")}})
	m = ctrlV(t, m)
	if m.input.Value() != "[Image #1] " {
		t.Fatalf("input = %q", m.input.Value())
	}
}

func TestCtrlVFinderFileAttaches(t *testing.T) {
	m, _, dir := newAttachModel(t)
	m = m.WithClipboard(&fakeReader{c: clipboardContent{Paths: []string{writeImage(t, dir, "a.png")}}})
	m = ctrlV(t, m)
	if m.input.Value() != "[Image #1] " {
		t.Fatalf("input = %q", m.input.Value())
	}
}

func TestCtrlVFinderNonImageFileIsText(t *testing.T) {
	m, _, dir := newAttachModel(t)
	p := writeFile(t, dir, "n.txt", []byte("x"))
	m = m.WithClipboard(&fakeReader{c: clipboardContent{Paths: []string{p}}})
	m = ctrlV(t, m)
	if m.input.Value() != p {
		t.Fatalf("input = %q", m.input.Value())
	}
}

func TestCtrlVEmptyClipboard(t *testing.T) {
	m, _, _ := newAttachModel(t)
	m = m.WithClipboard(&fakeReader{})
	m = ctrlV(t, m)
	if !strings.Contains(flatText(m), "no image on the clipboard") || m.input.Value() != "" {
		t.Errorf("transcript = %q", plainLines(m))
	}
}

func TestCtrlVTextOnlyModel(t *testing.T) {
	m, _, _ := newAttachModel(t)
	m.alias = "txt"
	m = m.WithClipboard(&fakeReader{c: clipboardContent{Data: pngFixture(t)}})
	m = ctrlV(t, m)
	if m.input.Value() != "" || len(m.draftAtts) != 0 {
		t.Fatalf("input = %q", m.input.Value())
	}
	if !strings.Contains(flatText(m), "txt does not accept images, so the clipboard image was not attached.") {
		t.Errorf("transcript = %q", plainLines(m))
	}
}

func TestCtrlVIgnoredWhileBusy(t *testing.T) {
	m, _, _ := newAttachModel(t)
	r := &fakeReader{c: clipboardContent{Text: "x"}}
	m = m.WithClipboard(r)
	m, cmd := pressKey(m, tea.KeyCtrlV)
	if cmd == nil {
		t.Fatal("first Ctrl+V must read")
	}
	m, cmd2 := pressKey(m, tea.KeyCtrlV)
	if cmd2 != nil {
		t.Error("second Ctrl+V while a read is in flight must be ignored")
	}
}

func TestCtrlVErrors(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{errClipboardUnavailable, "the clipboard is not available here; drag the file in or use /attach PATH"},
		{errClipboardTimeout, "reading the clipboard timed out after 5s"},
		{errors.New("boom"), "reading the clipboard failed: boom"},
	}
	for _, c := range cases {
		m, _, _ := newAttachModel(t)
		m = m.WithClipboard(&fakeReader{err: c.err})
		m = ctrlV(t, m)
		if !strings.Contains(flatText(m), c.want) {
			t.Errorf("%v: transcript = %q", c.err, plainLines(m))
		}
	}
}

func TestTextinputPasteBindingDisabled(t *testing.T) {
	m, _, _ := newAttachModel(t)
	if m.input.KeyMap.Paste.Enabled() {
		t.Error("textinput's own paste binding must be disabled")
	}
}
