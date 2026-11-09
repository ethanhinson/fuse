package tui

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func pngFixture(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// fakeRun scripts external commands: each call is recorded as "name args...",
// and the first rule whose key is a prefix of the call decides the outcome.
type fakeRun struct {
	calls []string
	rules []fakeRule
}

type fakeRule struct {
	prefix string
	out    string
	err    error
	write  []byte // written to the last argv, as the osascript PNGf script does
}

func (f *fakeRun) run(_ context.Context, name string, args ...string) ([]byte, error) {
	call := strings.Join(append([]string{name}, args...), " ")
	f.calls = append(f.calls, call)
	for _, r := range f.rules {
		if strings.HasPrefix(call, r.prefix) {
			if r.write != nil {
				if err := os.WriteFile(args[len(args)-1], r.write, 0o644); err != nil {
					return nil, err
				}
			}
			return []byte(r.out), r.err
		}
	}
	return nil, &cmdError{Name: name, Stderr: "unscripted"}
}

func (f *fakeRun) called(prefix string) bool {
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func newFakeClipboard(goos string, env map[string]string, tools []string, f *fakeRun) (*systemClipboard, string) {
	dir, _ := os.MkdirTemp("", "clip-test-")
	return &systemClipboard{
		goos:   goos,
		getenv: func(k string) string { return env[k] },
		lookPath: func(n string) (string, error) {
			for _, t := range tools {
				if t == n {
					return "/bin/" + n, nil
				}
			}
			return "", errors.New("not found")
		},
		run:     f.run,
		tempDir: dir,
	}, dir
}

var errNotType = &cmdError{Name: "osascript", Stderr: "execution error: Can't make some data into the expected type. (-1700)"}

func noClipTemp(t *testing.T, dir string) {
	t.Helper()
	left, _ := filepath.Glob(filepath.Join(dir, "fuse-clip-*"))
	if len(left) != 0 {
		t.Errorf("temp dirs left behind: %v", left)
	}
}

func TestClipboardDarwinScreenshot(t *testing.T) {
	data := pngFixture(t)
	f := &fakeRun{rules: []fakeRule{
		{prefix: "osascript -e POSIX path", err: errNotType},
		{prefix: "osascript -e on run argv", write: data},
	}}
	c, dir := newFakeClipboard("darwin", nil, nil, f)
	defer os.RemoveAll(dir)
	got, err := c.Read(t.Context())
	if err != nil || !bytes.Equal(got.Data, data) {
		t.Fatalf("got %+v err %v", got, err)
	}
	noClipTemp(t, dir)
}

func TestClipboardDarwinFinderFile(t *testing.T) {
	f := &fakeRun{rules: []fakeRule{{prefix: "osascript -e POSIX path", out: "/x/a.png\n"}}}
	c, dir := newFakeClipboard("darwin", nil, nil, f)
	defer os.RemoveAll(dir)
	got, err := c.Read(t.Context())
	if err != nil || !reflect.DeepEqual(got.Paths, []string{"/x/a.png"}) {
		t.Fatalf("got %+v err %v", got, err)
	}
	if f.called("osascript -e on run") {
		t.Error("PNGf must not be tried after a Finder file")
	}
}

func TestClipboardDarwinTextOnly(t *testing.T) {
	f := &fakeRun{rules: []fakeRule{
		{prefix: "osascript", err: errNotType},
		{prefix: "pbpaste", out: "hi"},
	}}
	c, dir := newFakeClipboard("darwin", nil, nil, f)
	defer os.RemoveAll(dir)
	got, err := c.Read(t.Context())
	if err != nil || got.Text != "hi" {
		t.Fatalf("got %+v err %v", got, err)
	}
	noClipTemp(t, dir)
}

func TestClipboardDarwinOtherErrorSurfaces(t *testing.T) {
	f := &fakeRun{rules: []fakeRule{{prefix: "osascript", err: &cmdError{Name: "osascript", Stderr: "not authorized (-1743)"}}}}
	c, dir := newFakeClipboard("darwin", nil, nil, f)
	defer os.RemoveAll(dir)
	if _, err := c.Read(t.Context()); err == nil || !strings.Contains(err.Error(), "-1743") {
		t.Fatalf("err = %v", err)
	}
}

func TestClipboardWaylandImage(t *testing.T) {
	data := pngFixture(t)
	f := &fakeRun{rules: []fakeRule{
		{prefix: "wl-paste --list-types", out: "text/plain\nimage/png\n"},
		{prefix: "wl-paste --no-newline --type image/png", out: string(data)},
	}}
	c, dir := newFakeClipboard("linux", map[string]string{"WAYLAND_DISPLAY": "wayland-0"}, []string{"wl-paste"}, f)
	defer os.RemoveAll(dir)
	got, err := c.Read(t.Context())
	if err != nil || !bytes.Equal(got.Data, data) || !f.called("wl-paste --no-newline --type image/png") {
		t.Fatalf("got %+v err %v calls %v", got, err, f.calls)
	}
}

func TestClipboardWaylandURIList(t *testing.T) {
	f := &fakeRun{rules: []fakeRule{
		{prefix: "wl-paste --list-types", out: "text/uri-list\ntext/plain\n"},
		{prefix: "wl-paste --no-newline --type text/uri-list", out: "file:///x/a%20b.png\r\n"},
	}}
	c, dir := newFakeClipboard("linux", map[string]string{"WAYLAND_DISPLAY": "w"}, []string{"wl-paste"}, f)
	defer os.RemoveAll(dir)
	got, err := c.Read(t.Context())
	if err != nil || !reflect.DeepEqual(got.Paths, []string{"/x/a b.png"}) {
		t.Fatalf("got %+v err %v", got, err)
	}
}

func TestClipboardX11Image(t *testing.T) {
	data := pngFixture(t)
	f := &fakeRun{rules: []fakeRule{
		{prefix: "xclip -selection clipboard -t TARGETS -o", out: "TARGETS\nimage/png\n"},
		{prefix: "xclip -selection clipboard -t image/png -o", out: string(data)},
	}}
	c, dir := newFakeClipboard("linux", map[string]string{"DISPLAY": ":0"}, []string{"xclip"}, f)
	defer os.RemoveAll(dir)
	got, err := c.Read(t.Context())
	if err != nil || !bytes.Equal(got.Data, data) {
		t.Fatalf("got %+v err %v", got, err)
	}
}

func TestClipboardLinuxTextFallback(t *testing.T) {
	f := &fakeRun{rules: []fakeRule{
		{prefix: "xclip -selection clipboard -t TARGETS -o", out: "TARGETS\nUTF8_STRING\n"},
		{prefix: "xclip -selection clipboard -o", out: "plain"},
	}}
	c, dir := newFakeClipboard("linux", map[string]string{"DISPLAY": ":0"}, []string{"xclip"}, f)
	defer os.RemoveAll(dir)
	got, err := c.Read(t.Context())
	if err != nil || got.Text != "plain" {
		t.Fatalf("got %+v err %v", got, err)
	}
}

func TestClipboardLinuxNoTool(t *testing.T) {
	c, dir := newFakeClipboard("linux", map[string]string{"WAYLAND_DISPLAY": "w", "DISPLAY": ":0"}, nil, &fakeRun{})
	defer os.RemoveAll(dir)
	if _, err := c.Read(t.Context()); !errors.Is(err, errClipboardNoTool) {
		t.Fatalf("err = %v", err)
	}
}

func TestClipboardWindowsUnsupported(t *testing.T) {
	c, dir := newFakeClipboard("windows", nil, nil, &fakeRun{})
	defer os.RemoveAll(dir)
	if _, err := c.Read(t.Context()); !errors.Is(err, errClipboardUnsupported) {
		t.Fatalf("err = %v", err)
	}
}

func TestClipboardTimeout(t *testing.T) {
	f := &fakeRun{rules: []fakeRule{{prefix: "osascript", err: context.DeadlineExceeded}}}
	c, dir := newFakeClipboard("darwin", nil, nil, f)
	defer os.RemoveAll(dir)
	_, err := c.Read(t.Context())
	if err == nil || err.Error() != "reading the clipboard timed out after 5s" {
		t.Fatalf("err = %v", err)
	}
}

func TestRunCommandCapsStdout(t *testing.T) {
	out, err := runCommand(t.Context(), "sh", "-c", "head -c 4000000 /dev/zero")
	if err != nil {
		t.Skipf("sh unavailable: %v", err)
	}
	if len(out) != 7<<19+1 {
		t.Errorf("len = %d", len(out))
	}
	_, err = runCommand(t.Context(), "sh", "-c", "echo boom >&2; exit 3")
	var ce *cmdError
	if !errors.As(err, &ce) || !strings.Contains(ce.Stderr, "boom") {
		t.Errorf("err = %v", err)
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
	if !strings.Contains(plainLines(m), "no image on the clipboard") || m.input.Value() != "" {
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
	if !strings.Contains(plainLines(m), "txt does not accept images, so the clipboard image was not attached.") {
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
		{errClipboardNoTool, "pasting images needs wl-paste (Wayland) or xclip (X11)"},
		{errClipboardTimeout, "reading the clipboard timed out after 5s"},
		{errors.New("boom"), "reading the clipboard failed: boom"},
	}
	for _, c := range cases {
		m, _, _ := newAttachModel(t)
		m = m.WithClipboard(&fakeReader{err: c.err})
		m = ctrlV(t, m)
		if !strings.Contains(plainLines(m), c.want) {
			t.Errorf("%v: transcript = %q", c.err, plainLines(m))
		}
	}
}

func TestCtrlVUnsupportedOSFallsBackToTextPaste(t *testing.T) {
	m, _, _ := newAttachModel(t)
	m.goos = "windows"
	m = m.WithClipboard(&fakeReader{err: errClipboardUnsupported})
	m, cmd := pressKey(m, tea.KeyCtrlV)
	next, c2 := m.Update(runUntil[clipboardResultMsg](t, cmd))
	m = next.(ShellModel)
	if !strings.Contains(plainLines(m), "pasting images is not supported on windows yet; drag the file in or use /attach PATH") {
		t.Errorf("transcript = %q", plainLines(m))
	}
	if c2 == nil {
		t.Error("text paste fallback cmd expected")
	}
}

func TestTextinputPasteBindingDisabled(t *testing.T) {
	m, _, _ := newAttachModel(t)
	if m.input.KeyMap.Paste.Enabled() {
		t.Error("textinput's own paste binding must be disabled")
	}
}
