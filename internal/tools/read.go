package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"io"
	"os"
	"strings"

	"github.com/ethanhinson/fuse/internal/archive"
	"github.com/ethanhinson/fuse/internal/artifact"
	"github.com/ethanhinson/fuse/internal/model"
)

type readTool struct{}

// NewReadFile returns the read_file tool with optional 1-based line range.
func NewReadFile() Tool { return readTool{} }

func (readTool) Name() string { return "read_file" }

// readDefaultWindow bounds an unranged read. Whole-file dumps are the main
// driver of context ballooning; a windowed default with an explicit
// continuation footer teaches the model to page instead.
const readDefaultWindow = 1000

// readWindowBytes bounds a windowed read (no end_line) in bytes as well as
// lines. It sits under the 20KB spill budget, so a windowed read is never cut
// middle-out: it ends at a line boundary with a footer saying where to go on.
const readWindowBytes = 16 << 10

const (
	readChunkBytes = 64 << 10
	// readMaxOutputBytes caps what one read returns, whatever the range. Lines
	// can be any length, so a line count alone does not bound memory.
	readMaxOutputBytes = 16 << 20
	// readMaxScanBytes is how far an unranged read looks for the file's line count.
	readMaxScanBytes = 64 << 20
)

func (readTool) Description() string {
	return "Read a file's contents with 1-based inclusive line ranges. " +
		"Without end_line, up to 1000 lines (16 KB) from start_line (default 1) are returned; the footer says how to continue. " +
		"Prefer grep to locate, then read just the ranges you need. " +
		"path may also be an art:// artifact ref (e.g. from a spilled-output marker)." +
		" An image file (PNG, JPEG, GIF or WebP) is attached so you can see it, when this model accepts images."
}

func (readTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path":       map[string]any{"type": "string", "description": "File path or art:// artifact ref to read"},
			"start_line": map[string]any{"type": "integer", "description": "First line (1-based, inclusive)"},
			"end_line":   map[string]any{"type": "integer", "description": "Last line (1-based, inclusive)"},
		},
		"required": []string{"path"},
	}
}

type readArgs struct {
	Path      string `json:"path"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
}

func (readTool) Execute(ctx context.Context, args string) Result {
	var a readArgs
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		return Result{IsError: true, Output: fmt.Sprintf("bad arguments: %v", err)}
	}
	// An art:// ref resolves through the loop's artifact binding.
	data, handled, err := readArtifact(ctx, a.Path)
	if err != nil {
		return Result{IsError: true, Output: err.Error()}
	}
	if handled {
		br := bufio.NewReaderSize(bytes.NewReader(data), readChunkBytes)
		head, _ := br.Peek(512)
		if mime, _ := imageType(head); mime != "" {
			return readImage(ctx, a.Path, br, int64(len(data)), mime, true)
		}
		return renderRead(ctx, a.Path, br, int64(len(data)), a)
	}
	// Transparent decompression: a ".gz" (or a bare path whose file is now a
	// "<path>.gz" archive) is gunzipped so the model's generic read_file keeps
	// working on archived spill/log files. Plaintext files pass through unchanged.
	// A truncated/corrupt gzip returns an error, never a panic.
	r, size, err := archive.OpenStream(a.Path)
	if err != nil {
		return Result{IsError: true, Output: err.Error()}
	}
	defer r.Close()
	br := bufio.NewReaderSize(r, readChunkBytes)
	var res Result
	head, _ := br.Peek(512)
	if mime, _ := imageType(head); mime != "" {
		res = readImage(ctx, a.Path, br, size, mime, false)
	} else {
		res = renderRead(ctx, a.Path, br, size, a)
	}

	// Track the read in the file tracker for filesystem paths (not art:// refs).
	// Only track successful reads: errors mean the content was not delivered.
	if !handled && !res.IsError {
		if ft := FileTrackerFrom(ctx); ft != nil {
			// A read is "full" when no range was specified. A partial read does NOT
			// authorize overwrite but DOES count for the edit_file staleness check.
			full := a.StartLine == 0 && a.EndLine == 0 || len(res.Attachments) > 0
			if canonical, err := resolvePath(a.Path); err == nil {
				if fi, statErr := os.Stat(canonical); statErr == nil {
					ft.MarkRead(canonical, full, fi.ModTime(), fi.Size())
				}
			}
		}
	}

	return res
}

// renderRead applies the binary refusal and line-range windowing shared by
// filesystem paths and artifact refs. It reads r as a stream and stops as soon as
// it has the answer. The file is whatever the model named, which can be far larger
// than memory or, for a device or a pipe, have no end at all. size is r's length
// when known, else -1.
func renderRead(ctx context.Context, path string, r io.Reader, size int64, a readArgs) Result {
	br := bufio.NewReaderSize(r, readChunkBytes)

	// Refuse binaries: raw executable bytes poison the model context and the
	// control characters corrupt terminal rendering. The check runs on the
	// DECOMPRESSED bytes so a gzipped binary is still refused.
	head, err := br.Peek(8192)
	if err != nil && err != io.EOF {
		return Result{IsError: true, Output: err.Error()}
	}
	if bytes.IndexByte(head, 0) >= 0 {
		what := ""
		if size >= 0 {
			what = fmt.Sprintf(" (%d bytes)", size)
		}
		return Result{IsError: true, Output: fmt.Sprintf(
			"%s appears to be a binary file%s — not readable as text. Use bash (file, strings, otool, xxd) for binary inspection.",
			path, what)}
	}

	start := a.StartLine
	if start < 1 {
		start = 1
	}
	// Inverted or negative ranges (end_line < start_line) select nothing, so tell
	// the model instead of scanning the file for them.
	if a.EndLine != 0 && a.EndLine < start {
		return Result{IsError: true, Output: fmt.Sprintf(
			"invalid range: end_line %d is before start_line %d", a.EndLine, a.StartLine)}
	}
	// A read without end_line, with or without start_line, is windowed: at most
	// readDefaultWindow lines and readWindowBytes from start, cut at a line
	// boundary, with a footer saying where to continue. Only an explicit end_line
	// asks for more. An open-ended start_line read used to run to the end of the
	// file, and anything over the spill budget then came back cut in the middle:
	// 8KB from start_line, 8KB from the file's end, nothing between.
	windowed := a.EndLine == 0
	end := a.EndLine
	limit := readMaxOutputBytes
	if windowed {
		end = start + readDefaultWindow - 1
		limit = readWindowBytes
	}

	var kept, cur bytes.Buffer
	line := 1  // the line being read; as with strings.Split, "a\n" has lines "a" and ""
	shown := 0 // last line kept whole
	keeping := true
	var scanned int64
	capped, lineCut, overScan := false, false, false

	// finish settles the line just read into kept. It reports false when the read
	// should stop: an explicit range hit the hard cap, or a window's first line
	// alone is over the window.
	over := false // cur holds only part of this line
	finish := func() bool {
		defer func() { cur.Reset(); over = false }()
		if !keeping || line < start || line > end {
			return true
		}
		sep := 0
		if shown > 0 {
			sep = 1
		}
		switch {
		case !over && kept.Len()+sep+cur.Len() <= limit:
			if sep == 1 {
				kept.WriteByte('\n')
			}
			kept.Write(cur.Bytes())
			shown = line
		case !windowed:
			if sep == 1 {
				kept.WriteByte('\n')
			}
			kept.Write(cur.Bytes())
			capped = true
			return false
		case shown == 0:
			kept.Write(cur.Bytes())
			shown, lineCut = line, true
			return false
		default:
			keeping = false
		}
		return true
	}

	chunkBuf := make([]byte, readChunkBytes)
scan:
	for {
		// A pipe that never ends has to be stoppable, and a tool timeout cancels ctx.
		if err := ctx.Err(); err != nil {
			return Result{IsError: true, Output: fmt.Sprintf("reading %s: %v", path, err)}
		}
		n, rerr := br.Read(chunkBuf)
		chunk := chunkBuf[:n]
		scanned += int64(n)
		for len(chunk) > 0 {
			seg := chunk
			nl := bytes.IndexByte(chunk, '\n')
			if nl >= 0 {
				seg = chunk[:nl]
			}
			if keeping && line >= start && line <= end && !over {
				room := limit - kept.Len() - cur.Len()
				if shown > 0 {
					room--
				}
				if len(seg) > room {
					cur.Write(seg[:max(room, 0)])
					over = true
					if !windowed || shown == 0 {
						// Nothing after this point can be shown: settle now
						// rather than walk the rest of a possibly endless line.
						finish()
						break scan
					}
				} else {
					cur.Write(seg)
				}
			}
			if nl < 0 {
				break
			}
			if !finish() {
				break scan
			}
			line++
			chunk = chunk[nl+1:]
			if !windowed && line > end {
				break scan
			}
		}
		if rerr == io.EOF {
			if !finish() {
				break scan
			}
			break
		}
		if rerr != nil {
			return Result{IsError: true, Output: rerr.Error()}
		}
		// Past the window the scan goes on only to report a total. That number is
		// not worth walking a huge file or an endless device for.
		if windowed && (!keeping || line > end) && scanned >= readMaxScanBytes {
			overScan = true
			break
		}
	}

	if start > line {
		return Result{IsError: true, Output: fmt.Sprintf("start_line %d beyond file length %d", start, line)}
	}

	out := kept.String()
	switch {
	case capped:
		out += fmt.Sprintf("\n(Output stopped at the %d MiB limit, in line %d. Use start_line and end_line to read a smaller range.)", readMaxOutputBytes>>20, line)
	case lineCut:
		out += fmt.Sprintf("\n(Line %d is longer than %d KB; only its start is shown. Use bash (cut -c, fold) for the rest of it, or start_line and end_line for other lines.)", shown, readWindowBytes>>10)
	case overScan:
		out += fmt.Sprintf("\n(Showing lines %d-%d of a file over %d MiB. Use start_line=%d to continue, or start_line and end_line to read further.)", start, shown, readMaxScanBytes>>20, shown+1)
	case windowed && shown < line:
		out += fmt.Sprintf("\n(Showing lines %d-%d of %d. Use start_line=%d to continue.)", start, shown, line, shown+1)
	}
	return Result{Output: out}
}

// readImage answers a read_file of an image: the picture as an attachment when
// the model accepts images, else a text result saying why not. fromArtifact marks
// a source that is already a stored attachment, which is not stored twice.
func readImage(ctx context.Context, path string, br *bufio.Reader, size int64, mime string, fromArtifact bool) Result {
	if _, ok := imageSupported(mime); !ok {
		return Result{IsError: true, Output: unsupportedImageErr(path, mime).Error()}
	}
	if !AttachmentsAllowed(ctx) {
		head, _ := br.Peek(512)
		detail := ""
		if mime != "image/webp" {
			if cfg, _, err := image.DecodeConfig(bytes.NewReader(head)); err == nil {
				detail = fmt.Sprintf("%dx%d pixels, ", cfg.Width, cfg.Height)
			}
		}
		if size >= 0 {
			detail += formatSize(int(size))
		} else {
			detail = strings.TrimSuffix(detail, ", ")
		}
		desc := imageKind(mime) + " image"
		if detail != "" {
			desc += " (" + detail + ")"
		}
		return Result{IsError: true, Output: fmt.Sprintf(
			"%s is a %s. This model cannot view images, so it was not attached; inspect it with bash instead (e.g. python3 with PIL), or use a model that accepts attachments.",
			path, desc)}
	}
	att, err := LoadAttachment(path, br, mime)
	if err != nil {
		return Result{IsError: true, Output: err.Error()}
	}
	if fromArtifact {
		att.Ref = path
	} else if b, ok := artifact.BindingFrom(ctx); ok {
		StoreAttachment(ctx, b, &att, "read_file")
	}
	dims := ""
	if att.Width > 0 {
		dims = fmt.Sprintf("%dx%d pixels, ", att.Width, att.Height)
	}
	out := fmt.Sprintf("%s: %s image, %s%s. Attached for you to view.", path, imageKind(mime), dims, formatSize(att.Size))
	if att.Ref != "" {
		out += " Saved as " + att.Ref + "."
	}
	return Result{Output: out, Attachments: []model.Attachment{att}}
}
