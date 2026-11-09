package tui

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"golang.design/x/clipboard"
)

// clipboardTimeout bounds one clipboard read.
const clipboardTimeout = 5 * time.Second

var (
	errClipboardUnavailable = errors.New("the clipboard is not available here; drag the file in or use /attach PATH")
	errClipboardTimeout     = errors.New("reading the clipboard timed out after 5s")
)

// clipboardContent is what the clipboard holds. At most one field is set.
type clipboardContent struct {
	Paths []string // copied files (a Finder or file-manager copy)
	Data  []byte   // image bytes, always PNG
	Text  string   // plain text fallback
}

// clipboardReader reads the system clipboard. Tests inject a fake.
type clipboardReader interface {
	Read(ctx context.Context) (clipboardContent, error)
}

// clipBackend is the system clipboard as golang.design/x/clipboard exposes it.
// That library reads the native clipboard on macOS, Windows and Linux
// (Wayland data-control or X11) without cgo or external tools, so fuse no
// longer shells out to osascript, pbpaste, wl-paste or xclip. Tests fake it.
type clipBackend interface {
	init() error
	readFiles(ctx context.Context) ([]string, error)
	read(ctx context.Context, f clipboard.Format) ([]byte, error)
}

type libClipboard struct{}

func (libClipboard) init() error { return clipboard.Init() }
func (libClipboard) readFiles(ctx context.Context) ([]string, error) {
	return clipboard.ReadFiles(ctx)
}
func (libClipboard) read(ctx context.Context, f clipboard.Format) ([]byte, error) {
	return clipboard.Read(ctx, f)
}

// systemClipboard reads, in order, copied files, then an image, then text: a
// file-manager copy can also carry the file's icon as image data, and an
// image copy can carry a text description.
type systemClipboard struct {
	backend clipBackend
	once    sync.Once
	initErr error
}

func newSystemClipboard() *systemClipboard { return &systemClipboard{backend: libClipboard{}} }

func (c *systemClipboard) Read(ctx context.Context) (clipboardContent, error) {
	c.once.Do(func() { c.initErr = c.backend.init() })
	if c.initErr != nil {
		return clipboardContent{}, fmt.Errorf("%w (%v)", errClipboardUnavailable, c.initErr)
	}
	ctx, cancel := context.WithTimeout(ctx, clipboardTimeout)
	defer cancel()

	files, err := c.backend.readFiles(ctx)
	if err = readErr(err); err != nil {
		return clipboardContent{}, err
	}
	if len(files) > 0 {
		return clipboardContent{Paths: files}, nil
	}
	img, err := c.backend.read(ctx, clipboard.FmtImage)
	if err = readErr(err); err != nil {
		return clipboardContent{}, err
	}
	if len(img) > 0 {
		return clipboardContent{Data: img}, nil
	}
	txt, err := c.backend.read(ctx, clipboard.FmtText)
	if err = readErr(err); err != nil {
		return clipboardContent{}, err
	}
	return clipboardContent{Text: string(txt)}, nil
}

// readErr maps a library read error: no data in that format is not a failure,
// a deadline is the timeout, and an unavailable clipboard says what to do.
func readErr(err error) error {
	switch {
	case err == nil, errors.Is(err, clipboard.ErrNoData):
		return nil
	case errors.Is(err, context.DeadlineExceeded):
		return errClipboardTimeout
	case errors.Is(err, clipboard.ErrUnavailable):
		return fmt.Errorf("%w (%v)", errClipboardUnavailable, err)
	}
	return err
}
