package tools

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// endlessLines is an io.Reader that never ends: "line 1\nline 2\n...".
type endlessLines struct {
	n    int
	left []byte
	read int64
}

func (e *endlessLines) Read(p []byte) (int, error) {
	if len(e.left) == 0 {
		e.n++
		e.left = []byte("line " + strconv.Itoa(e.n) + "\n")
	}
	n := copy(p, e.left)
	e.left = e.left[n:]
	e.read += int64(n)
	return n, nil
}

// endlessLong is an io.Reader of one line that never ends.
type endlessLong struct{ read int64 }

func (e *endlessLong) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	e.read += int64(len(p))
	return len(p), nil
}

// Same answers as strings.Split would give, which is what callers have always seen.
func TestRenderRead_MatchesSplitSemantics(t *testing.T) {
	ctx := context.Background()
	for _, content := range []string{"", "a", "a\n", "a\nb", "a\nb\n", "\n", "\n\n", "a\n\nb\n"} {
		lines := strings.Split(content, "\n")
		total := len(lines)
		for start := 0; start <= total+1; start++ {
			for end := 0; end <= total+1; end++ {
				if end != 0 && end < max(start, 1) {
					continue
				}
				args := readArgs{StartLine: start, EndLine: end}
				res := renderRead(ctx, "f", strings.NewReader(content), int64(len(content)), args)
				first := max(start, 1)
				if first > total {
					if !res.IsError || !strings.Contains(res.Output, "beyond file length "+strconv.Itoa(total)) {
						t.Errorf("%q [%d,%d]: %+v, want beyond-length error", content, start, end, res)
					}
					continue
				}
				last := end
				if last == 0 || last > total {
					last = total
				}
				if want := strings.Join(lines[first-1:last], "\n"); res.IsError || res.Output != want {
					t.Errorf("%q [%d,%d] = %+v, want %q", content, start, end, res, want)
				}
			}
		}
	}
}

func TestRenderRead_LongLineAcrossChunks(t *testing.T) {
	long := strings.Repeat("y", 3*readChunkBytes)
	content := "a\n" + long + "\nc\n"
	res := renderRead(context.Background(), "f", strings.NewReader(content), -1, readArgs{StartLine: 2, EndLine: 3})
	if res.IsError || res.Output != long+"\nc" {
		t.Fatalf("len %d, err %v", len(res.Output), res.IsError)
	}
}

// A ranged read stops at end_line: the rest of the file is never read at all.
func TestRenderRead_RangedStopsEarly(t *testing.T) {
	src := &endlessLines{}
	res := renderRead(context.Background(), "f", src, -1, readArgs{StartLine: 100, EndLine: 101})
	if res.IsError || res.Output != "line 100\nline 101" {
		t.Fatalf("%+v", res)
	}
	if src.read > 256<<10 {
		t.Errorf("read %d bytes for a range in the first kilobyte", src.read)
	}
}

func TestRenderRead_UnrangedEndlessIsBounded(t *testing.T) {
	src := &endlessLines{}
	res := renderRead(context.Background(), "f", src, -1, readArgs{})
	if res.IsError {
		t.Fatal(res.Output)
	}
	if !strings.Contains(res.Output, "line 1000\n") || strings.Contains(res.Output, "line 1001\n") {
		t.Errorf("window wrong; tail %q", res.Output[len(res.Output)-120:])
	}
	if !strings.Contains(res.Output, "start_line and end_line") {
		t.Errorf("no guidance; tail %q", res.Output[len(res.Output)-120:])
	}
	if src.read > readMaxScanBytes+2*readChunkBytes {
		t.Errorf("scanned %d bytes, limit %d", src.read, readMaxScanBytes)
	}
}

func TestRenderRead_OutputCappedOnEndlessLine(t *testing.T) {
	for name, args := range map[string]readArgs{"unranged": {}, "ranged": {StartLine: 1, EndLine: 5}, "open_end": {StartLine: 1}} {
		t.Run(name, func(t *testing.T) {
			res := renderRead(context.Background(), "f", &endlessLong{}, -1, args)
			if res.IsError {
				t.Fatal(res.Output)
			}
			if len(res.Output) > readMaxOutputBytes+256 {
				t.Errorf("output %d bytes, cap %d", len(res.Output), readMaxOutputBytes)
			}
			if !strings.Contains(res.Output, "start_line and end_line") {
				t.Errorf("no guidance; tail %q", res.Output[len(res.Output)-120:])
			}
		})
	}
}

// A scan for a far-off line over a source with no end stops when the tool's
// context does, and keeps nothing while it runs.
func TestRenderRead_CancelledScanStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	src := &cancelAfter{cancel: cancel, after: 8 << 20}
	res := renderRead(ctx, "f", src, -1, readArgs{StartLine: 1 << 40})
	if !res.IsError || !strings.Contains(res.Output, "canceled") {
		t.Fatalf("%+v", res)
	}
}

type cancelAfter struct {
	endlessLines
	cancel func()
	after  int64
}

func (c *cancelAfter) Read(p []byte) (int, error) {
	if c.read > c.after {
		c.cancel()
	}
	return c.endlessLines.Read(p)
}

func TestReadFile_DevZeroRefusedWithoutGrowing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no /dev/zero")
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	res := NewReadFile().Execute(context.Background(), `{"path":"/dev/zero"}`)
	runtime.ReadMemStats(&after)
	if !res.IsError || !strings.Contains(res.Output, "binary") {
		t.Fatalf("%+v", res)
	}
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 1<<20 {
		t.Errorf("allocated %d bytes refusing /dev/zero", grew)
	}
}

func TestReadFile_HugeTextFileRangeAndWindow(t *testing.T) {
	p := filepath.Join(t.TempDir(), "big.log")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.Repeat("z", 1000) + "\n"
	for range readMaxScanBytes/len(line) + 100 {
		if _, err := io.WriteString(f, line); err != nil {
			t.Fatal(err)
		}
	}
	f.Close()
	res := NewReadFile().Execute(context.Background(), `{"path":"`+p+`"}`)
	if res.IsError || !strings.Contains(res.Output, "of a file over 64 MiB") {
		t.Fatalf("err %v, tail %q", res.IsError, res.Output[max(0, len(res.Output)-120):])
	}
	res = NewReadFile().Execute(context.Background(), `{"path":"`+p+`","start_line":60000,"end_line":60001}`)
	if res.IsError || len(res.Output) != 2001 {
		t.Fatalf("err %v, len %d", res.IsError, len(res.Output))
	}
}

func TestReadFile_GzipRangesAndFallback(t *testing.T) {
	dir := t.TempDir()
	var b bytes.Buffer
	for i := 1; i <= 2000; i++ {
		fmt.Fprintf(&b, "row %d\n", i)
	}
	gzWrite(t, filepath.Join(dir, "x.log.gz"), b.Bytes())

	// Asked for by the original name, found by its archived one.
	res := NewReadFile().Execute(context.Background(), `{"path":"`+filepath.Join(dir, "x.log")+`","start_line":1500,"end_line":1501}`)
	if res.IsError || res.Output != "row 1500\nrow 1501" {
		t.Fatalf("%+v", res)
	}
	res = NewReadFile().Execute(context.Background(), `{"path":"`+filepath.Join(dir, "x.log.gz")+`"}`)
	if res.IsError || !strings.Contains(res.Output, "Showing lines 1-1000 of 2001") {
		t.Fatalf("err %v, tail %q", res.IsError, res.Output[len(res.Output)-80:])
	}

	gzWrite(t, filepath.Join(dir, "bin.gz"), []byte("a\x00b"))
	res = NewReadFile().Execute(context.Background(), `{"path":"`+filepath.Join(dir, "bin.gz")+`"}`)
	if !res.IsError || !strings.Contains(res.Output, "binary") {
		t.Fatalf("%+v", res)
	}
}

func TestReadFile_CorruptGzipReportsError(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "g.gz")
	gzWrite(t, good, bytes.Repeat([]byte("hello world\n"), 5000))
	data, err := os.ReadFile(good)
	if err != nil {
		t.Fatal(err)
	}
	truncated := filepath.Join(dir, "t.gz")
	if err := os.WriteFile(truncated, data[:len(data)/2], 0o600); err != nil {
		t.Fatal(err)
	}
	res := NewReadFile().Execute(context.Background(), `{"path":"`+truncated+`"}`)
	if !res.IsError || !strings.Contains(res.Output, "gzip") {
		t.Fatalf("%+v", res)
	}
	headerOnly := filepath.Join(dir, "h.gz")
	if err := os.WriteFile(headerOnly, data[:4], 0o600); err != nil {
		t.Fatal(err)
	}
	res = NewReadFile().Execute(context.Background(), `{"path":"`+headerOnly+`"}`)
	if !res.IsError || !strings.Contains(res.Output, "gzip") {
		t.Fatalf("%+v", res)
	}
}
