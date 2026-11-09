package tui

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/ethanhinson/fuse/internal/tools"
)

// clipboardCmdTimeout bounds each external command that reads the clipboard.
const clipboardCmdTimeout = 5 * time.Second

var (
	errClipboardNoTool      = errors.New("pasting images needs wl-paste (Wayland) or xclip (X11); install one, or drag the file in, or use /attach PATH")
	errClipboardUnsupported = errors.New("pasting images is not supported on this OS yet")
	errClipboardTimeout     = errors.New("reading the clipboard timed out after 5s")
)

// clipboardContent is what the clipboard holds. At most one field is set.
type clipboardContent struct {
	Paths []string // file references: a Finder copy (furl) or a text/uri-list
	Data  []byte   // raw image bytes
	Text  string   // plain text fallback
}

// clipboardReader reads the system clipboard. Tests inject a fake.
type clipboardReader interface {
	Read(ctx context.Context) (clipboardContent, error)
}

// cmdError is a failed external command.
type cmdError struct {
	Name, Stderr string
	Err          error
}

func (e *cmdError) Error() string {
	msg := strings.TrimSpace(e.Stderr)
	if msg == "" && e.Err != nil {
		msg = e.Err.Error()
	}
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		msg = msg[:i]
	}
	return e.Name + ": " + msg
}

func (e *cmdError) Unwrap() error { return e.Err }

// systemClipboard reads the clipboard through the platform's own tools. Every
// OS dependency is a field so tests can fake it.
type systemClipboard struct {
	goos     string
	getenv   func(string) string
	lookPath func(string) (string, error)
	run      func(ctx context.Context, name string, args ...string) ([]byte, error)
	tempDir  string
}

func newSystemClipboard() *systemClipboard {
	return &systemClipboard{
		goos:     runtime.GOOS,
		getenv:   os.Getenv,
		lookPath: exec.LookPath,
		run:      runCommand,
		tempDir:  os.TempDir(),
	}
}

// limitedBuffer keeps at most max bytes and silently discards the rest.
type limitedBuffer struct {
	buf bytes.Buffer
	max int
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if room := l.max - l.buf.Len(); room > 0 {
		if len(p) < room {
			room = len(p)
		}
		l.buf.Write(p[:room])
	}
	return len(p), nil
}

// runCommand runs name and returns its stdout, capped at one byte over the
// attachment limit so the loader reports the size error.
func runCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out := &limitedBuffer{max: tools.MaxAttachmentBytes + 1}
	var stderr bytes.Buffer
	cmd.Stdout = out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &cmdError{Name: name, Stderr: stderr.String(), Err: err}
	}
	return out.buf.Bytes(), nil
}

// runT runs one command under the per-command timeout.
func (c *systemClipboard) runT(ctx context.Context, name string, args ...string) ([]byte, error) {
	cctx, cancel := context.WithTimeout(ctx, clipboardCmdTimeout)
	defer cancel()
	out, err := c.run(cctx, name, args...)
	if errors.Is(err, context.DeadlineExceeded) {
		return nil, errClipboardTimeout
	}
	return out, err
}

// notThisType reports whether err is osascript's "not this type" (-1700).
func notThisType(err error) bool {
	var ce *cmdError
	return errors.As(err, &ce) && strings.Contains(ce.Stderr, "-1700")
}

func (c *systemClipboard) Read(ctx context.Context) (clipboardContent, error) {
	switch c.goos {
	case "darwin":
		return c.readDarwin(ctx)
	case "linux":
		return c.readLinux(ctx)
	}
	return clipboardContent{}, errClipboardUnsupported
}

// darwinFileTypes and darwinImageTypes are `clipboard info` entries that mark
// a copied file and image data. AppleScript coerces plain text to a file URL
// ("the clipboard as «class furl»" on "hello" gives /hello), so the reader asks
// which types are present before coercing anything.
var (
	darwinFileTypes  = []string{"«class furl»", "alias"}
	darwinImageTypes = []string{"«class PNGf»", "TIFF picture", "JPEG picture", "GIF picture"}
)

// clipboardHas reports whether a `clipboard info` listing ("«class utf8», 24,
// string, 24, ...") holds any of types as an entry name.
func clipboardHas(info string, types []string) bool {
	for _, entry := range strings.Split(info, ",") {
		entry = strings.TrimSpace(entry)
		for _, t := range types {
			if entry == t {
				return true
			}
		}
	}
	return false
}

func (c *systemClipboard) readDarwin(ctx context.Context) (clipboardContent, error) {
	out, err := c.runT(ctx, "osascript", "-e", "clipboard info")
	if err != nil {
		return clipboardContent{}, err
	}
	info := string(out)

	// A Finder copy comes first: it can also carry the file's icon as image data.
	if clipboardHas(info, darwinFileTypes) {
		out, err := c.runT(ctx, "osascript", "-e", "POSIX path of (the clipboard as «class furl»)")
		switch {
		case err == nil:
			if p := strings.TrimSpace(string(out)); p != "" {
				return clipboardContent{Paths: []string{p}}, nil
			}
		case !notThisType(err):
			return clipboardContent{}, err
		}
	}
	if !clipboardHas(info, darwinImageTypes) {
		return c.darwinText(ctx)
	}

	dir, err := os.MkdirTemp(c.tempDir, "fuse-clip-*")
	if err != nil {
		return clipboardContent{}, err
	}
	defer os.RemoveAll(dir)
	file := dir + string(os.PathSeparator) + "clip.png"
	_, err = c.runT(ctx, "osascript",
		"-e", "on run argv",
		"-e", "set f to open for access (POSIX file (item 1 of argv)) with write permission",
		"-e", "set eof f to 0",
		"-e", "write (the clipboard as «class PNGf») to f",
		"-e", "close access f",
		"-e", "end run", file)
	switch {
	case err == nil:
		if data, rerr := os.ReadFile(file); rerr == nil && len(data) > 0 {
			return clipboardContent{Data: data}, nil
		}
	case !notThisType(err):
		return clipboardContent{}, err
	}

	return c.darwinText(ctx)
}

func (c *systemClipboard) darwinText(ctx context.Context) (clipboardContent, error) {
	out, err := c.runT(ctx, "pbpaste")
	if err != nil {
		return clipboardContent{}, err
	}
	return clipboardContent{Text: string(out)}, nil
}

// imageTargets is the order in which clipboard image types are preferred.
var imageTargets = []string{"image/png", "image/jpeg", "image/gif", "image/webp"}

func (c *systemClipboard) readLinux(ctx context.Context) (clipboardContent, error) {
	var list, get func(typ string) []string
	var text []string
	switch {
	case c.getenv("WAYLAND_DISPLAY") != "" && c.hasTool("wl-paste"):
		list = func(string) []string { return []string{"wl-paste", "--list-types"} }
		get = func(typ string) []string { return []string{"wl-paste", "--no-newline", "--type", typ} }
		text = []string{"wl-paste", "--no-newline"}
	case c.getenv("DISPLAY") != "" && c.hasTool("xclip"):
		list = func(string) []string { return []string{"xclip", "-selection", "clipboard", "-t", "TARGETS", "-o"} }
		get = func(typ string) []string { return []string{"xclip", "-selection", "clipboard", "-t", typ, "-o"} }
		text = []string{"xclip", "-selection", "clipboard", "-o"}
	default:
		return clipboardContent{}, errClipboardNoTool
	}

	lc := list("")
	out, err := c.runT(ctx, lc[0], lc[1:]...)
	if errors.Is(err, errClipboardTimeout) {
		return clipboardContent{}, err
	}
	var types []string
	if err == nil {
		for _, l := range strings.Split(string(out), "\n") {
			if l = strings.TrimSpace(l); l != "" {
				types = append(types, l)
			}
		}
	}
	has := func(t string) bool {
		for _, x := range types {
			if x == t {
				return true
			}
		}
		return false
	}

	if has("text/uri-list") {
		gc := get("text/uri-list")
		out, err := c.runT(ctx, gc[0], gc[1:]...)
		if err != nil {
			return clipboardContent{}, err
		}
		var paths []string
		for _, l := range strings.Split(string(out), "\n") {
			l = strings.TrimSpace(l)
			if l == "" || strings.HasPrefix(l, "#") {
				continue
			}
			if p, ok := resolveDroppedPath(l, "", c.goos); ok {
				paths = append(paths, p)
			}
		}
		if len(paths) > 0 {
			return clipboardContent{Paths: paths}, nil
		}
	}
	for _, t := range imageTargets {
		if has(t) {
			gc := get(t)
			out, err := c.runT(ctx, gc[0], gc[1:]...)
			if err != nil {
				return clipboardContent{}, err
			}
			return clipboardContent{Data: out}, nil
		}
	}
	out, err = c.runT(ctx, text[0], text[1:]...)
	if errors.Is(err, errClipboardTimeout) {
		return clipboardContent{}, err
	}
	if err != nil { // an error here means an empty clipboard
		return clipboardContent{}, nil
	}
	return clipboardContent{Text: string(out)}, nil
}

func (c *systemClipboard) hasTool(name string) bool {
	_, err := c.lookPath(name)
	return err == nil
}
