package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/ethanhinson/fuse/internal/archive"
)

type readTool struct{}

// NewReadFile returns the read_file tool with optional 1-based line range.
func NewReadFile() Tool { return readTool{} }

func (readTool) Name() string { return "read_file" }

// readDefaultWindow bounds an unranged read. Whole-file dumps are the main
// driver of context ballooning; a windowed default with an explicit
// continuation footer teaches the model to page instead.
const readDefaultWindow = 1000

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
		"Without a range, the first 1000 lines are returned; the footer says how to continue. " +
		"Prefer grep to locate, then read just the ranges you need. " +
		"path may also be an art:// artifact ref (e.g. from a spilled-output marker)."
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
		return renderRead(ctx, a.Path, bytes.NewReader(data), int64(len(data)), a)
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
	return renderRead(ctx, a.Path, r, size, a)
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
	// Lines start..end are kept. end == 0 means through the end of the file.
	end := a.EndLine
	unranged := a.StartLine == 0 && a.EndLine == 0
	if unranged {
		// Unranged read of a large file: return the first window with an
		// explicit continuation affordance instead of the whole file.
		end = readDefaultWindow
	}

	var kept bytes.Buffer
	line := 1 // the line being read; as with strings.Split, "a\n" has lines "a" and ""
	var scanned int64
	capped, overScan := false, false
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
			if line >= start && (end == 0 || line <= end) {
				if room := readMaxOutputBytes - kept.Len(); len(seg) >= room {
					kept.Write(seg[:room])
					capped = true
					break scan
				}
				kept.Write(seg)
			}
			if nl < 0 {
				break
			}
			// A newline separates two kept lines, so it is kept only if the next is.
			if line >= start && (end == 0 || line < end) {
				kept.WriteByte('\n')
			}
			line++
			chunk = chunk[nl+1:]
			if !unranged && end != 0 && line > end {
				break scan
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return Result{IsError: true, Output: rerr.Error()}
		}
		// An unranged read scans on only to report a total. That number is not worth
		// walking a huge file or an endless device for.
		if unranged && scanned >= readMaxScanBytes {
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
	case overScan:
		// The output cap is lower than the scan limit, so reaching the limit means
		// the whole window fitted.
		out += fmt.Sprintf("\n(Showing lines 1-%d of a file over %d MiB. Use start_line and end_line to read further.)", readDefaultWindow, readMaxScanBytes>>20)
	case unranged && line > readDefaultWindow:
		out += fmt.Sprintf("\n(Showing lines %d-%d of %d. Use start_line=%d to continue.)", start, end, line, end+1)
	}
	return Result{Output: out}
}
