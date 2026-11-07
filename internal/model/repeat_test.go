package model

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRepeatsAtTail(t *testing.T) {
	runaway := "Here is the explanation. " + strings.Repeat("of the value ", 2000)
	prose := "Go slices grow by doubling. " + strings.Repeat("a different sentence each time ", 300) +
		strings.Repeat("of the value ", 100) // 1300 bytes of repetition: under the bar

	// A markdown table with many similar rows. Column values differ, so no
	// short unit repeats 2KB back to back.
	var table strings.Builder
	table.WriteString("| Name | Kind | Status |\n|---|---|---|\n")
	for i := 0; i < 80; i++ {
		fmt.Fprintf(&table, "| item-%03d | widget | active |\n", i)
	}

	// A long numbered list: each line differs by its number.
	var list strings.Builder
	for i := 1; i <= 300; i++ {
		fmt.Fprintf(&list, "%d. an entry in the list with some shared wording\n", i)
	}

	// Repeated log lines that each differ by a timestamp.
	var logs strings.Builder
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&logs, "2026-11-07T12:%02d:%02d.000Z INFO request handled path=/v1/chat status=200\n", i/60, i%60)
	}

	// 1.9KB of one repeated unit is under the 2KB bar and must not trigger.
	underBar := strings.Repeat("of the value ", 146) // 1898 bytes

	for name, tc := range map[string]struct {
		s    string
		want bool
	}{
		"empty":             {"", false},
		"short":             {"ababababab", false},
		"plain_prose":       {prose, false},
		"markdown_table":    {table.String(), false},
		"numbered_list":     {list.String(), false},
		"timestamped_logs":  {logs.String(), false},
		"under_2kb_of_unit": {underBar, false},
		"runaway_phrase":    {runaway, true},
		"runaway_one_char":  {strings.Repeat("x", 5000), true},
		"runaway_newline":   {"done.\n" + strings.Repeat("\n", 3000), true},
		"runaway_256_bytes": {strings.Repeat(strings.Repeat("q", 255)+" ", 12), true},
		"unit_257_bytes":    {strings.Repeat(strings.Repeat("q", 256)+" ", 10), false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := repeatsAtTail(tc.s); got != tc.want {
				t.Errorf("repeatsAtTail(%d bytes) = %v, want %v", len(tc.s), got, tc.want)
			}
		})
	}
}

// A stream whose content repeats forever is cut off long before the provider
// would stop, the cancellation reaches the server, and the retry returns the
// good second reply.
func TestRunawayStreamCutOffEarlyAndRetried(t *testing.T) {
	var calls, firstBytes atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		if n > 1 {
			fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"clean answer"},"finish_reason":"stop"}]}`+"\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		fl, _ := w.(http.Flusher)
		line := `data: {"choices":[{"delta":{"content":"of the value "}}]}` + "\n\n"
		for {
			m, err := io.WriteString(w, line)
			if err != nil {
				return // the client hung up: the cancellation worked
			}
			firstBytes.Add(int64(m))
			if fl != nil {
				fl.Flush()
			}
			// Yield to the Go scheduler: in a busy container this loop can
			// otherwise fill a large kernel buffer before the transport gets
			// cycles to deliver the client's cancellation.
			time.Sleep(time.Millisecond)
			select {
			case <-r.Context().Done():
				return
			default:
			}
		}
	}))
	defer srv.Close()

	a := NewAdapter(srv.URL, "k", srv.Client())
	a.MaxAttempts = 3
	a.RetryBackoff = 0
	start := time.Now()
	resp, err := a.Complete(context.Background(), CompletionReq{Model: "m", Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("retry should have recovered: %v", err)
	}
	if resp.Content != "clean answer" {
		t.Fatalf("content = %q, want the second reply", resp.Content)
	}
	if calls.Load() != 2 {
		t.Fatalf("server calls = %d, want 2 (runaway cut, then retry)", calls.Load())
	}
	// The runaway reply is detected after a few KB; without the watchdog the
	// server would stream until the client's output cap (256KB+).
	if got := firstBytes.Load(); got > 64*1024 {
		t.Errorf("server sent %d bytes of the runaway stream; the watchdog should have cut it far earlier", got)
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Errorf("took %v; the stream was not cut early", elapsed)
	}
}

// When every attempt degenerates, the failure is clear and bounded.
func TestRunawayStreamRetriesExhausted(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		line := `data: {"choices":[{"delta":{"content":"of the value "}}]}` + "\n\n"
		for {
			if _, err := io.WriteString(w, line); err != nil {
				return
			}
			if fl != nil {
				fl.Flush()
			}
			select {
			case <-r.Context().Done():
				return
			default:
			}
		}
	}))
	defer srv.Close()

	a := NewAdapter(srv.URL, "k", srv.Client())
	a.MaxAttempts = 2
	a.RetryBackoff = 0
	_, err := a.Complete(context.Background(), CompletionReq{Model: "m"})
	if err == nil || !strings.Contains(err.Error(), "degenerated into repetition") {
		t.Fatalf("err = %v, want a clear repetition failure", err)
	}
	if calls.Load() != 2 {
		t.Errorf("calls = %d, want both attempts tried", calls.Load())
	}
}

// Repetitive tool-call arguments (a model writing a repetitive file) must not
// trip the watchdog: only visible content is watched.
func TestRepetitiveToolArgumentsNotCut(t *testing.T) {
	repetitiveArgs := `{"path":"out.txt","content":"` + strings.Repeat("of the value ", 2000) + `"}`
	srv := sseServer(t, []string{
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"write_file","arguments":` + mustJSON(repetitiveArgs) + `}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":5,"completion_tokens":5}}`,
	})
	defer srv.Close()

	a := NewAdapter(srv.URL, "k", srv.Client())
	resp, err := a.Complete(context.Background(), CompletionReq{Model: "m"})
	if err != nil {
		t.Fatalf("repetitive tool arguments must stream fine: %v", err)
	}
	if len(resp.ToolCalls) != 1 || !strings.Contains(resp.ToolCalls[0].Arguments, "of the value ") {
		t.Fatalf("tool call = %+v", resp.ToolCalls)
	}
}

func mustJSON(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
