package segment

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/ethanhinson/fuse/internal/model"
)

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

// rawHeaderRE matches a raw-region message header line "<role> [<name>]:" or
// "<role>:". The name group is optional.
var rawHeaderRE = regexp.MustCompile(`^(\S+?)(?: \[(.+)\])?:$`)

// LoadSegment reads and parses a segment file at path, reconstructing its raw
// region messages (role + name + content) from the "## Raw region" section so a
// caller can filter by tool name.
func LoadSegment(path string) (Segment, error) {
	b, err := os.ReadFile(path)
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

// parseRawRegion parses the block format RenderRawRegion emits back into
// messages. Content between a header and the next header (or EOF) is the
// message body, with the trailing blank separator trimmed.
func parseRawRegion(raw string) []model.Message {
	lines := strings.Split(raw, "\n")
	var out []model.Message
	var cur *model.Message
	var buf []string
	flush := func() {
		if cur == nil {
			return
		}
		cur.Content = strings.TrimRight(strings.Join(buf, "\n"), "\n")
		out = append(out, *cur)
		cur = nil
		buf = nil
	}
	for _, ln := range lines {
		if m := rawHeaderRE.FindStringSubmatch(ln); m != nil {
			flush()
			cur = &model.Message{Role: m[1], Name: m[2]}
			continue
		}
		if cur != nil {
			buf = append(buf, ln)
		}
	}
	flush()
	return out
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
