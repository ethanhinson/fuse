package tools

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func numbered(n, width int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		s := fmt.Sprintf("line %d ", i)
		b.WriteString(s + strings.Repeat("x", max(0, width-len(s))) + "\n")
	}
	return b.String()
}

func read(t *testing.T, content string, a readArgs) string {
	t.Helper()
	res := renderRead(context.Background(), "f", strings.NewReader(content), int64(len(content)), a)
	if res.IsError {
		t.Fatal(res.Output)
	}
	return res.Output
}

// A start_line with no end_line is windowed like an unranged read, not run to
// the end of the file. Live, these reads came back over the spill budget and
// were cut middle-out: 8KB from start_line, then 8KB from the end of the file.
func TestOpenEndedReadIsWindowedFromStartLine(t *testing.T) {
	out := read(t, numbered(3000, 8), readArgs{StartLine: 100})
	if !strings.HasPrefix(out, "line 100 ") || !strings.Contains(out, "line 1099 ") || strings.Contains(out, "line 1100 ") {
		t.Fatalf("window wrong: head %q tail %q", out[:20], out[len(out)-120:])
	}
	if !strings.HasSuffix(out, "(Showing lines 100-1099 of 3001. Use start_line=1100 to continue.)") {
		t.Errorf("footer: %q", out[len(out)-120:])
	}
}

// A window also stops at readWindowBytes, on a line boundary, so it stays under
// the spill budget and never shows the file's tail out of place.
func TestWindowStopsAtByteBudgetOnALineBoundary(t *testing.T) {
	content := numbered(400, 99) // 100 bytes a line, 40KB
	for name, a := range map[string]readArgs{"unranged": {}, "start_only": {StartLine: 50}} {
		t.Run(name, func(t *testing.T) {
			out := read(t, content, a)
			body, footer, ok := strings.Cut(out, "\n(Showing lines ")
			if !ok {
				t.Fatalf("no footer: %q", out[len(out)-120:])
			}
			if len(body) > readWindowBytes || len(out) >= spillThreshold {
				t.Errorf("body %d bytes, output %d; window %d, spill %d", len(body), len(out), readWindowBytes, spillThreshold)
			}
			lines := strings.Split(body, "\n")
			last := lines[len(lines)-1]
			if len(last) != 99 || strings.Contains(out, "line 400 ") {
				t.Errorf("cut mid-line or showed the tail: last %q", last)
			}
			first := max(a.StartLine, 1)
			want := fmt.Sprintf("%d-%d of 401. Use start_line=%d to continue.)", first, first+len(lines)-1, first+len(lines))
			if footer != want {
				t.Errorf("footer %q, want %q", footer, want)
			}
		})
	}
}

// An explicit range is what the model asked for, so it is not windowed.
func TestExplicitRangeIsNotWindowed(t *testing.T) {
	out := read(t, numbered(400, 99), readArgs{StartLine: 1, EndLine: 300})
	if strings.Count(out, "\n") != 299 || strings.Contains(out, "Showing") {
		t.Errorf("got %d lines, footer present %v", strings.Count(out, "\n")+1, strings.Contains(out, "Showing"))
	}
}

// A window whose first line alone is over the budget shows that line's start
// and says how to get the rest.
func TestWindowWithAnOverlongFirstLine(t *testing.T) {
	content := "short\n" + strings.Repeat("y", 3*readWindowBytes) + "\nafter\n"
	out := read(t, content, readArgs{StartLine: 2})
	if !strings.HasPrefix(out, strings.Repeat("y", readWindowBytes)) || strings.Contains(out, "after") {
		t.Fatalf("head %q", out[:20])
	}
	if !strings.Contains(out, "Line 2 is longer than 16 KB") {
		t.Errorf("tail %q", out[len(out)-160:])
	}
}

// A small file read without end_line comes back whole, with no footer.
func TestSmallOpenEndedReadHasNoFooter(t *testing.T) {
	if out := read(t, "a\nb\nc\n", readArgs{StartLine: 2}); out != "b\nc\n" {
		t.Errorf("%q", out)
	}
}
