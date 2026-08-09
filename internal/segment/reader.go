package segment

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/ethanhinson/fuse/internal/model"
)

// gzMagic is the two-byte gzip magic number (RFC 1952). Segments born after the
// change 0030 scope expansion are stored gzip-compressed as "<name>.md.gz"; old
// plaintext ".md" segments remain readable. LoadSegment sniffs these bytes so it
// stays transparent to both.
var gzMagic = [2]byte{0x1f, 0x8b}

// ReadIndex reads a session's index.json from its segments directory. A missing
// index is not an error — it returns an empty Index so callers treat "no
// segments yet" the same as an empty selection.
func ReadIndex(segDir string) (Index, error) {
	b, err := os.ReadFile(filepath.Join(segDir, IndexFileName))
	if err != nil {
		if os.IsNotExist(err) {
			return Index{}, nil
		}
		return Index{}, err
	}
	var idx Index
	if err := json.Unmarshal(b, &idx); err != nil {
		return Index{}, err
	}
	return idx, nil
}

// LoadSegment reads and parses a segment file at path, reconstructing its raw
// region messages (role + name + content + tool metadata) from the "## Raw
// region" section so a caller can filter by tool name or re-render the original
// transcript exactly.
func LoadSegment(path string) (Segment, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		// Back-compat / rename tolerance: an index Path may name a bare ".md"
		// whose on-disk file is now the compressed ".md.gz". Try that before
		// giving up.
		if os.IsNotExist(err) && !strings.HasSuffix(path, ".gz") {
			if gb, gerr := os.ReadFile(path + ".gz"); gerr == nil {
				b = gb
			} else {
				return Segment{}, err
			}
		} else {
			return Segment{}, err
		}
	}
	// Transparent decompression: new segments are gzip; old ones are plaintext.
	b, err = gunzipIfNeeded(b)
	if err != nil {
		return Segment{}, err
	}
	body := string(b)

	seg := Segment{}
	// Summary: between "## Summary" and "## Raw region".
	if s := sectionBetween(body, "## Summary", "## Raw region"); s != "" {
		seg.Summary = strings.TrimSpace(s)
	}
	raw := sectionAfter(body, "## Raw region")
	seg.Messages = parseRawRegion(raw)
	return seg, nil
}

// gunzipIfNeeded decompresses b when it begins with the gzip magic number, else
// returns b unchanged. A corrupt/truncated gzip stream yields an error (never a
// panic).
func gunzipIfNeeded(b []byte) ([]byte, error) {
	if len(b) < 2 || b[0] != gzMagic[0] || b[1] != gzMagic[1] {
		return b, nil
	}
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("segment gzip open: %w", err)
	}
	defer zr.Close()
	out, err := io.ReadAll(zr)
	if err != nil {
		return nil, fmt.Errorf("segment gzip read: %w", err)
	}
	return out, nil
}

// parseRawRegion parses the on-disk raw region (a JSON array of messages inside
// a ```json fenced code block, as renderRawRegionStore emits) back into the
// message slice. Because JSON is self-delimiting, every field round-trips
// exactly regardless of message content — no line-scanning heuristic that
// arbitrary tool output could spoof into a false message boundary. A missing or
// malformed block yields no messages (best-effort recovery, never a panic).
func parseRawRegion(raw string) []model.Message {
	start := strings.Index(raw, rawRegionFence)
	if start < 0 {
		return nil
	}
	rest := raw[start+len(rawRegionFence):]
	// The JSON begins after the fence line's newline.
	if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
		rest = rest[nl+1:]
	}
	end := strings.Index(rest, "\n```")
	if end < 0 {
		return nil
	}
	var msgs []model.Message
	if err := json.Unmarshal([]byte(rest[:end]), &msgs); err != nil {
		return nil
	}
	return msgs
}

// sectionBetween returns the text between the start marker and the end marker.
func sectionBetween(body, start, end string) string {
	i := strings.Index(body, start)
	if i < 0 {
		return ""
	}
	i += len(start)
	rest := body[i:]
	if j := strings.Index(rest, end); j >= 0 {
		return rest[:j]
	}
	return rest
}

// sectionAfter returns the text after the marker.
func sectionAfter(body, marker string) string {
	i := strings.Index(body, marker)
	if i < 0 {
		return ""
	}
	return body[i+len(marker):]
}
