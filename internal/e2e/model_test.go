package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestModel_Success_Exit0(t *testing.T) {
	t.Parallel()
	srv := newServer(t, text("all finished"))
	h := newHarness(t, srv)
	r := h.run()
	wantCode(t, r, 0)
	wantContains(t, r.stdout, "all finished", "stdout")
	if got := srv.requests()[0]; got.Auth != "Bearer "+h.key || got.Model != "test-model" {
		t.Fatalf("auth=%q model=%q", got.Auth, got.Model)
	}
}

func TestModel_PlainJSONResponseAccepted(t *testing.T) {
	t.Parallel()
	srv := newServer(t, status(200, `{"choices":[{"message":{"role":"assistant","content":"buffered hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`, "Content-Type", "application/json"))
	h := newHarness(t, srv)
	r := h.run()
	wantCode(t, r, 0)
	wantContains(t, r.stdout, "buffered hello", "stdout")
}

func Test429WithoutRetryAfter_RetriedThenSucceeds(t *testing.T) {
	t.Parallel()
	srv := newServer(t, status(429, `{"error":"slow"}`), status(429, `{"error":"slow"}`), text("recovered"))
	h := newHarness(t, srv)
	r := h.run()
	wantCode(t, r, 0)
	if srv.count() != 3 {
		t.Fatalf("requests = %d, want 3", srv.count())
	}
	wantContains(t, r.stdout, "recovered", "stdout")
}

func Test429RetryAfter_Honored(t *testing.T) {
	t.Parallel()
	srv := newServer(t, status(429, `{"error":"slow"}`, "Retry-After", "3"), text("recovered"))
	h := newHarness(t, srv)
	wantCode(t, h.run(), 0)
	reqs := srv.requests()
	if gap := reqs[1].At.Sub(reqs[0].At); gap < 2500*time.Millisecond {
		t.Fatalf("retried after %s despite Retry-After: 3", gap)
	}
}

func Test500ThenSuccess(t *testing.T) {
	t.Parallel()
	srv := newServer(t, status(500, "boom"), text("recovered"))
	h := newHarness(t, srv)
	wantCode(t, h.run(), 0)
	if srv.count() != 2 {
		t.Fatalf("requests = %d, want 2", srv.count())
	}
}

func Test500Persistent_BoundedNonzeroClear(t *testing.T) {
	t.Parallel()
	srv := newRouter(t, func(*request) step { return status(500, "upstream exploded") })
	h := newHarness(t, srv)
	r := h.run()
	wantCode(t, r, 1)
	if srv.count() != 3 {
		t.Fatalf("requests = %d, want 3 bounded attempts", srv.count())
	}
	wantContains(t, r.stderr, "500", "stderr")
	wantContains(t, r.stderr, "upstream exploded", "stderr")
	wantContains(t, r.stderr, "3 attempt", "stderr")
}

func Test401_NoRetryClearMessage(t *testing.T) {
	t.Parallel()
	srv := newRouter(t, func(*request) step { return status(401, `{"error":"invalid api key"}`) })
	h := newHarness(t, srv)
	r := h.run()
	wantCode(t, r, 1)
	if srv.count() != 1 {
		t.Fatalf("requests = %d, want 1", srv.count())
	}
	wantContains(t, r.stderr, "401", "stderr")
	wantContains(t, r.stderr, "invalid api key", "stderr")
}

func TestConnectionRefused(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	port := refusedPort(t)
	h.writeConfig()
	cfg := strings.Replace(readFile(t, h.home+"/.fuse/config.yml"), h.gatewayURL(), fmt.Sprintf("http://127.0.0.1:%d/v1", port), 1)
	h.write(h.home+"/.fuse/config.yml", cfg, 0o600)
	r := h.startNoConfig(defaultTimeout, "--model", "m", "e2e task").wait()
	wantCode(t, r, 1)
	wantContains(t, r.stderr, "connection refused", "stderr")
	wantContains(t, r.stderr, "3 attempt", "stderr")
	if r.dur > 15*time.Second {
		t.Fatalf("took %s", r.dur)
	}
}

// A stream dropped mid tool call must not run the half-received call.
func TestStreamCutMidToolCall_RetriedNotExecuted(t *testing.T) {
	t.Parallel()
	var target string
	cut := func(w http.ResponseWriter, r *http.Request) {
		sseHeader(w)
		sse(w, toolCallChunk(0, call{"call_1", "write_file", fmt.Sprintf(`{"path":%q,"content":"par`, target)}))
		panic(http.ErrAbortHandler)
	}
	srv := newServer(t, cut, text("recovered"))
	h := newHarness(t, srv)
	target = h.path("partial.txt")
	r := h.run("--approve-all")
	wantCode(t, r, 0)
	mustNotExist(t, target)
	if srv.count() != 2 {
		t.Fatalf("requests = %d, want the cut attempt retried once", srv.count())
	}
}

func TestStreamCutPersistent_Nonzero(t *testing.T) {
	t.Parallel()
	cut := func(w http.ResponseWriter, r *http.Request) {
		sseHeader(w)
		sse(w, contentChunk("hello"))
		panic(http.ErrAbortHandler)
	}
	srv := newRouter(t, func(*request) step { return cut })
	h := newHarness(t, srv)
	r := h.run()
	wantCode(t, r, 1)
	if srv.count() != 3 {
		t.Fatalf("requests = %d, want 3", srv.count())
	}
	wantContains(t, r.stderr, "read gateway stream", "stderr")
}

// A proxy that closes the stream cleanly (terminating chunk, no [DONE], no
// finish_reason) is the same lost response as a dropped connection.
func TestStreamCleanEOFWithoutDone_NotTreatedAsComplete(t *testing.T) {
	t.Parallel()
	partial := func(w http.ResponseWriter, r *http.Request) {
		sseHeader(w)
		sse(w, contentChunk("The first half of the ans"))
		// Returning ends the chunked body cleanly: no error, no terminator event.
	}
	srv := newRouter(t, func(*request) step { return partial })
	h := newHarness(t, srv)
	r := h.run()
	if r.code == 0 {
		t.Fatalf("exit 0 on a truncated stream after %d request(s); stdout: %s", srv.count(), tail(r.stdout))
	}
}

func TestHang_NoHeaders_BoundedByRequestTimeout(t *testing.T) {
	t.Parallel()
	srv := newServer(t)
	srv.route = func(*request) step { return srv.hang() }
	h := newHarness(t, srv)
	h.gatewayExtra = "request_timeout: 500ms"
	r := h.run()
	wantCode(t, r, 1)
	if srv.count() != 3 {
		t.Fatalf("requests = %d, want 3", srv.count())
	}
	wantContains(t, r.stderr, "deadline exceeded", "stderr")
}

func TestHang_AfterHeaders_BoundedByRequestTimeout(t *testing.T) {
	t.Parallel()
	srv := newServer(t)
	srv.route = func(*request) step { return srv.hangAfterHeaders() }
	h := newHarness(t, srv)
	h.gatewayExtra = "request_timeout: 500ms"
	r := h.run()
	wantCode(t, r, 1)
	if srv.count() != 3 {
		t.Fatalf("requests = %d, want 3", srv.count())
	}
}

// With no request_timeout the only bound on a silent gateway is the 10 minute
// idle/header timeout per attempt (documented deliberate, adapter.go:32-37). This
// pins that the run is still in its first attempt after a few seconds. See the
// report for the resulting 30 minute worst case.
func TestHang_DefaultHasNoShortBound_Documented(t *testing.T) {
	t.Parallel()
	srv := newServer(t)
	srv.route = func(*request) step { return srv.hangAfterHeaders() }
	h := newHarness(t, srv)
	p := h.start(30*time.Second, "--model", "m", "e2e task")
	srv.waitForRequests(t, 1, 10*time.Second)
	time.Sleep(3 * time.Second)
	if srv.count() != 1 {
		t.Fatalf("requests = %d, want 1 still in flight", srv.count())
	}
	p.signal(syscall.SIGKILL)
	p.wait()
}

// Malformed JSON arguments: the model hears about it, the replayed transcript
// stays valid for strict providers, nothing runs.
func TestMalformedToolArguments_ReportedAndReplayedValid(t *testing.T) {
	t.Parallel()
	srv := newServer(t, tool("write_file", `{"path": "x.txt", "content": `), text("ok"))
	h := newHarness(t, srv)
	r := h.run("--approve-all")
	wantCode(t, r, 0)
	wantContains(t, h.toolOutput(), "bad arguments", "tool result")
	mustNotExist(t, h.path("x.txt"))
	for _, m := range h.lastRequest().Messages {
		for _, c := range m.ToolCalls {
			if !json.Valid([]byte(c.Function.Arguments)) {
				t.Fatalf("replayed invalid JSON arguments: %q", c.Function.Arguments)
			}
		}
	}
}

func TestUnknownTool_ReportedToModel(t *testing.T) {
	t.Parallel()
	srv := newServer(t, tool("does_not_exist", `{}`), text("ok"))
	h := newHarness(t, srv)
	r := h.run("--approve-all")
	wantCode(t, r, 0)
	wantContains(t, h.toolOutput(), `unknown tool "does_not_exist"`, "tool result")
}

// Under the default gate an unknown tool is denied by policy rather than named as
// unknown; the run still survives.
func TestUnknownTool_DefaultGate_DeniedNotCrash(t *testing.T) {
	t.Parallel()
	srv := newServer(t, tool("does_not_exist", `{}`), text("ok"))
	h := newHarness(t, srv)
	wantCode(t, h.run(), 0)
	wantContains(t, h.toolOutput(), "requires approval", "tool result")
}

func TestFinishReasonLength_NoToolCall_BoundedNonzero(t *testing.T) {
	t.Parallel()
	cutoff := func(w http.ResponseWriter, r *http.Request) {
		sseHeader(w)
		sse(w, contentChunk("cut off mid sent"))
		sse(w, finishChunk("length"))
		sse(w, usageChunk(10, 1000))
		done(w)
	}
	srv := newRouter(t, func(*request) step { return cutoff })
	h := newHarness(t, srv)
	h.mextra = "max_tokens_ceiling: 4000"
	r := h.run()
	wantCode(t, r, 1)
	reqs := srv.requests()
	if len(reqs) > 4 {
		t.Fatalf("requests = %d, want a short climb to the ceiling", len(reqs))
	}
	if reqs[len(reqs)-1].MaxTok <= reqs[0].MaxTok {
		t.Fatalf("output cap did not rise: %d -> %d", reqs[0].MaxTok, reqs[len(reqs)-1].MaxTok)
	}
	wantContains(t, r.stderr, "truncated", "stderr")
	wantContains(t, r.stdout, "cut off mid sent", "stdout keeps the partial text")
}

// finish_reason length with a cut-off tool call must never execute the call.
func TestFinishReasonLength_CutToolCall_NotExecuted(t *testing.T) {
	t.Parallel()
	var target string
	cutoff := func(w http.ResponseWriter, r *http.Request) {
		sseHeader(w)
		sse(w, toolCallChunk(0, call{"call_1", "write_file", fmt.Sprintf(`{"path":%q,"content":"par`, target)}))
		sse(w, finishChunk("length"))
		sse(w, usageChunk(10, 1000))
		done(w)
	}
	srv := newServer(t, cutoff, text("ok"))
	h := newHarness(t, srv)
	target = h.path("cut.txt")
	wantCode(t, h.run("--approve-all"), 0)
	mustNotExist(t, target)
	if len(h.lastRequest().toolResults()) != 0 {
		t.Fatalf("cut-off call reached history: %v", h.lastRequest().toolResults())
	}
}

func TestEmptyReply_FailsRun(t *testing.T) {
	t.Parallel()
	for name, st := range map[string]step{
		"finish_stop": func(w http.ResponseWriter, r *http.Request) {
			sseHeader(w)
			sse(w, finishChunk("stop"))
			sse(w, usageChunk(10, 0))
			done(w)
		},
		"done_only": func(w http.ResponseWriter, r *http.Request) {
			sseHeader(w)
			done(w)
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := newRouter(t, func(*request) step { return st })
			h := newHarness(t, srv)
			r := h.run()
			if r.code == 0 {
				t.Fatalf("exit 0 for an empty model reply after %d request(s); stdout %q stderr %q", srv.count(), r.stdout, r.stderr)
			}
		})
	}
}

func TestMalformedSSEChunk_NonzeroNoRetry(t *testing.T) {
	t.Parallel()
	bad := func(w http.ResponseWriter, r *http.Request) {
		sseHeader(w)
		fmt.Fprint(w, "data: {not json\n\n")
	}
	srv := newRouter(t, func(*request) step { return bad })
	h := newHarness(t, srv)
	r := h.run()
	wantCode(t, r, 1)
	wantContains(t, r.stderr, "decode stream chunk", "stderr")
}

func TestHTMLErrorPageWith200_Nonzero(t *testing.T) {
	t.Parallel()
	srv := newRouter(t, func(*request) step {
		return status(200, "<html>gateway login</html>", "Content-Type", "text/html")
	})
	h := newHarness(t, srv)
	r := h.run()
	wantCode(t, r, 1)
	wantContains(t, r.stderr, "decode gateway response", "stderr")
}

func TestStreamErrorEvent_TerminalNotRetried(t *testing.T) {
	t.Parallel()
	bad := func(w http.ResponseWriter, r *http.Request) {
		sseHeader(w)
		sse(w, map[string]any{"error": map[string]any{"message": "AuthenticationError: bad key", "type": "AuthenticationError", "code": "401"}})
	}
	srv := newRouter(t, func(*request) step { return bad })
	h := newHarness(t, srv)
	r := h.run()
	wantCode(t, r, 1)
	if srv.count() != 1 {
		t.Fatalf("requests = %d, want 1", srv.count())
	}
}

func TestContextOverflow_NoToolHistory_FailsOnce(t *testing.T) {
	t.Parallel()
	srv := newRouter(t, func(*request) step {
		return status(400, `{"error":{"message":"This model's maximum context length is 8192 tokens"}}`)
	})
	h := newHarness(t, srv)
	r := h.run()
	wantCode(t, r, 1)
	if srv.count() != 1 {
		t.Fatalf("requests = %d, want 1 (nothing to prune)", srv.count())
	}
	wantContains(t, r.stderr, "maximum context length", "stderr")
}

func TestContextOverflow_StreamedErrorEvent(t *testing.T) {
	t.Parallel()
	bad := func(w http.ResponseWriter, r *http.Request) {
		sseHeader(w)
		sse(w, map[string]any{"error": map[string]any{"message": "context_length_exceeded", "type": "ContextWindowExceededError", "code": "400"}})
	}
	srv := newRouter(t, func(*request) step { return bad })
	h := newHarness(t, srv)
	wantCode(t, h.run(), 1)
	if srv.count() != 1 {
		t.Fatalf("requests = %d, want 1", srv.count())
	}
}

func TestToolCallsAsQwenXMLText_Recovered(t *testing.T) {
	t.Parallel()
	var target string
	srv := newRouter(t, func(r *request) step {
		if r.N == 0 {
			return text(fmt.Sprintf("<function=write_file>\n<parameter=path>\n%s\n</parameter>\n<parameter=content>\nhi\n</parameter>\n</function>\n</tool_call>", target))
		}
		return text("done")
	})
	h := newHarness(t, srv)
	target = h.path("recovered.txt")
	wantCode(t, h.run("--approve-all"), 0)
	if got := readFile(t, target); !strings.Contains(got, "hi") {
		t.Fatalf("recovered write produced %q", got)
	}
	if srv.count() != 2 {
		t.Fatalf("requests = %d, want the recovered call answered", srv.count())
	}
}

// Other text dialects are deliberately not recovered (internal/model/recover.go
// names only the Qwen XML form), so the reply reads as a final answer. Pinned here
// because the run exits 0 with the work undone; see the report.
func TestToolCallsAsOtherTextDialects_EndRunUnexecuted_Documented(t *testing.T) {
	t.Parallel()
	for name, format := range map[string]string{
		"hermes_json": `<tool_call>{"name":"write_file","arguments":{"path":%q,"content":"x"}}</tool_call>`,
		"bare_json":   `{"name":"write_file","arguments":{"path":%q,"content":"x"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var target string
			srv := newRouter(t, func(r *request) step { return text(fmt.Sprintf(format, target)) })
			h := newHarness(t, srv)
			target = h.path("never.txt")
			wantCode(t, h.run("--approve-all"), 0)
			mustNotExist(t, target)
			if srv.count() != 1 {
				t.Fatalf("requests = %d", srv.count())
			}
		})
	}
}

// A reasoning model's empty reply must not end the run when asking again works.
func TestEmptyReply_RecoversOnRetry(t *testing.T) {
	t.Parallel()
	empty := func(w http.ResponseWriter, r *http.Request) {
		sseHeader(w)
		sse(w, finishChunk("stop"))
		sse(w, usageChunk(10, 0))
		done(w)
	}
	var mu sync.Mutex
	n := 0
	srv := newRouter(t, func(*request) step {
		mu.Lock()
		defer mu.Unlock()
		n++
		if n == 1 {
			return empty
		}
		return text("recovered")
	})
	h := newHarness(t, srv)
	r := h.run()
	wantCode(t, r, 0)
	wantContains(t, r.stdout, "recovered", "stdout")
}
