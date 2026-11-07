// Package e2e is a black-box failure-injection suite for the fuse binary. Each
// test execs the real binary against a scripted fake OpenAI-compatible server it
// owns, in a scratch HOME and working directory, with an environment built from
// scratch so no test can ever reach a real model.
//
// A test that exposes a product defect calls defect() first: it skips by default
// and runs for real, failing, under E2E_SHOW_DEFECTS=1.
package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// fuseBin is the binary under test, built once by TestMain.
var fuseBin string

// selfBin is this test binary, re-execed as the fake MCP server.
var selfBin string

func TestMain(m *testing.M) {
	if mode := os.Getenv(mcpModeEnv); mode != "" {
		runMCPHelper(mode)
		return
	}
	dir, err := os.MkdirTemp("", "fuse-e2e-bin-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fuseBin = filepath.Join(dir, "fuse")
	build := exec.Command("go", "build", "-o", fuseBin, "./cmd/fuse")
	build.Dir = repoRoot()
	build.Env = scrubbed(os.Environ())
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build fuse: %v\n%s\n", err, out)
		os.RemoveAll(dir)
		os.Exit(1)
	}
	selfBin, err = os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.RemoveAll(dir)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// repoRoot walks up from the test's working directory to go.mod.
func repoRoot() string {
	dir, _ := os.Getwd()
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			panic("go.mod not found")
		}
		dir = parent
	}
}

// forbiddenEnv can route the binary at a live paid service.
var forbiddenEnv = []string{"LLM_GATEWAY_URL", "LLM_GATEWAY_KEY", "OPENROUTER_API_KEY", "TAVILY_API_KEY"}

func scrubbed(env []string) []string {
	out := make([]string, 0, len(env))
next:
	for _, kv := range env {
		for _, f := range forbiddenEnv {
			if strings.HasPrefix(kv, f+"=") {
				continue next
			}
		}
		out = append(out, kv)
	}
	return out
}

// defect marks a test exposing a confirmed product defect. It must be called
// before t.Parallel so the skip costs nothing.
func defect(t *testing.T, msg string) {
	t.Helper()
	if os.Getenv("E2E_SHOW_DEFECTS") == "" {
		t.Skip("DEFECT: " + msg)
	}
}

// --- fake gateway ---------------------------------------------------------

type wireFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type wireCall struct {
	ID       string   `json:"id"`
	Function wireFunc `json:"function"`
}

type wireMsg struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []wireCall `json:"tool_calls"`
	ToolCallID string     `json:"tool_call_id"`
}

type wireTool struct {
	Function struct {
		Name string `json:"name"`
	} `json:"function"`
}

// request is one recorded call to /chat/completions.
type request struct {
	N        int        `json:"-"`
	At       time.Time  `json:"-"`
	Auth     string     `json:"-"`
	Raw      []byte     `json:"-"`
	Model    string     `json:"model"`
	Messages []wireMsg  `json:"messages"`
	Tools    []wireTool `json:"tools"`
	MaxTok   int        `json:"max_tokens"`
}

// firstUser returns the opening user message: the agent's task.
func (r *request) firstUser() string {
	for _, m := range r.Messages {
		if m.Role == "user" {
			return m.Content
		}
	}
	return ""
}

// toolResult returns the content of the tool message answering id.
func (r *request) toolResult(id string) (string, bool) {
	for _, m := range r.Messages {
		if m.Role == "tool" && m.ToolCallID == id {
			return m.Content, true
		}
	}
	return "", false
}

func (r *request) toolResults() []string {
	var out []string
	for _, m := range r.Messages {
		if m.Role == "tool" {
			out = append(out, m.Content)
		}
	}
	return out
}

// step answers one request.
type step func(w http.ResponseWriter, r *http.Request)

type server struct {
	*httptest.Server
	mu      sync.Mutex
	reqs    []*request
	steps   []step
	route   func(*request) step
	release chan struct{}
	once    sync.Once
}

// newServer starts a gateway that answers with steps in order, then with
// "done" forever.
func newServer(t *testing.T, steps ...step) *server {
	t.Helper()
	s := &server{steps: steps, release: make(chan struct{})}
	s.Server = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(func() {
		s.unhang()
		s.CloseClientConnections()
		s.Close()
	})
	return s
}

// newRouter starts a gateway that chooses each answer from the request.
func newRouter(t *testing.T, route func(*request) step) *server {
	t.Helper()
	s := newServer(t)
	s.route = route
	return s
}

func (s *server) unhang() { s.once.Do(func() { close(s.release) }) }

func (s *server) handle(w http.ResponseWriter, r *http.Request) {
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r.Body)
	req := &request{Raw: buf.Bytes(), At: time.Now(), Auth: r.Header.Get("Authorization")}
	_ = json.Unmarshal(req.Raw, req)
	s.mu.Lock()
	req.N = len(s.reqs)
	s.reqs = append(s.reqs, req)
	var st step
	switch {
	case s.route != nil:
		st = s.route(req)
	case req.N < len(s.steps):
		st = s.steps[req.N]
	default:
		st = text("done")
	}
	s.mu.Unlock()
	st(w, r)
}

func (s *server) requests() []*request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*request(nil), s.reqs...)
}

func (s *server) count() int { return len(s.requests()) }

// waitForRequests blocks until n requests arrived.
func (s *server) waitForRequests(t *testing.T, n int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if s.count() >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("server saw %d requests, wanted %d within %s", s.count(), n, within)
}

func sseHeader(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
}

func sse(w http.ResponseWriter, v any) {
	b, _ := json.Marshal(v)
	fmt.Fprintf(w, "data: %s\n\n", b)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func usageChunk(in, out int) map[string]any {
	return map[string]any{"choices": []any{}, "usage": map[string]int{"prompt_tokens": in, "completion_tokens": out}}
}

func contentChunk(s string) map[string]any {
	return map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"role": "assistant", "content": s}}}}
}

func finishChunk(reason string) map[string]any {
	return map[string]any{"choices": []any{map[string]any{"delta": map[string]any{}, "finish_reason": reason}}}
}

func done(w http.ResponseWriter) {
	fmt.Fprint(w, "data: [DONE]\n\n")
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// text streams a complete assistant message.
func text(s string) step {
	return func(w http.ResponseWriter, r *http.Request) {
		sseHeader(w)
		sse(w, contentChunk(s))
		sse(w, finishChunk("stop"))
		sse(w, usageChunk(10, 5))
		done(w)
	}
}

// call is one scripted tool call.
type call struct {
	id, name, args string
}

func toolCallChunk(i int, c call) map[string]any {
	return map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{
		map[string]any{"index": i, "id": c.id, "type": "function",
			"function": map[string]any{"name": c.name, "arguments": c.args}},
	}}}}}
}

// tools streams assistant tool calls with finish_reason tool_calls.
func tools(calls ...call) step {
	return func(w http.ResponseWriter, r *http.Request) {
		sseHeader(w)
		for i, c := range calls {
			sse(w, toolCallChunk(i, c))
		}
		sse(w, finishChunk("tool_calls"))
		sse(w, usageChunk(10, 5))
		done(w)
	}
}

// tool scripts a single call with id call_1. args is marshalled unless a string.
func tool(name string, args any) step {
	return tools(call{"call_1", name, mustArgs(args)})
}

func mustArgs(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func status(code int, body string, headers ...string) step {
	return func(w http.ResponseWriter, r *http.Request) {
		for i := 0; i+1 < len(headers); i += 2 {
			w.Header().Set(headers[i], headers[i+1])
		}
		w.WriteHeader(code)
		fmt.Fprint(w, body)
	}
}

// hang never answers, not even headers, until the client goes away.
func (s *server) hang() step {
	return func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-s.release:
		}
	}
}

// hangAfterHeaders sends stream headers and then no bytes at all.
func (s *server) hangAfterHeaders() step {
	return func(w http.ResponseWriter, r *http.Request) {
		sseHeader(w)
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-s.release:
		}
	}
}

// --- running the binary ---------------------------------------------------

type harness struct {
	t            *testing.T
	srv          *server
	home         string
	cwd          string
	key          string
	extra        string // appended to config.yml at the top level
	mextra       string // appended under the model entry, one "key: value" per line
	gatewayExtra string // appended under gateway:, one "key: value"
	env          map[string]string
}

var keySeq atomic.Int64

// newHarness builds a scratch HOME and cwd around srv. Config is written lazily
// by run, so a test can set extra first.
func newHarness(t *testing.T, srv *server) *harness {
	t.Helper()
	home := resolved(t, t.TempDir())
	cwd := filepath.Join(resolved(t, t.TempDir()), "work")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	return &harness{
		t:    t,
		srv:  srv,
		home: home,
		cwd:  cwd,
		key:  fmt.Sprintf("e2e-sentinel-key-%d-%d", os.Getpid(), keySeq.Add(1)),
		env:  map[string]string{},
	}
}

func resolved(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (h *harness) gatewayURL() string {
	if h.srv != nil {
		return h.srv.URL + "/v1"
	}
	return "http://127.0.0.1:1/v1"
}

func (h *harness) writeConfig() {
	h.t.Helper()
	var b strings.Builder
	fmt.Fprintf(&b, "gateway:\n  url: %s\n  key: %s\n", h.gatewayURL(), h.key)
	if h.gatewayExtra != "" {
		b.WriteString("  " + h.gatewayExtra + "\n")
	}
	b.WriteString("models:\n  m:\n    id: test-model\n    max_tokens: 1000\n")
	for _, line := range strings.Split(strings.TrimSpace(h.mextra), "\n") {
		if line != "" {
			b.WriteString("    " + line + "\n")
		}
	}
	b.WriteString(h.extra)
	if !strings.HasSuffix(b.String(), "\n") {
		b.WriteString("\n")
	}
	h.write(filepath.Join(h.home, ".fuse", "config.yml"), b.String(), 0o600)
}

func (h *harness) write(path, content string, mode os.FileMode) {
	h.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		h.t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		h.t.Fatal(err)
	}
}

// hostSandbox authorizes bash to run directly on this machine for this run.
func (h *harness) hostSandbox() {
	h.write(filepath.Join(h.home, ".fuse", "sandbox.local.yml"), "handler: host\n", 0o600)
}

func (h *harness) path(rel string) string { return filepath.Join(h.cwd, rel) }

// outside returns a directory next to, not inside, the workspace.
func (h *harness) outside() string {
	h.t.Helper()
	d := filepath.Join(resolved(h.t, h.t.TempDir()), "outside")
	if err := os.MkdirAll(d, 0o755); err != nil {
		h.t.Fatal(err)
	}
	return d
}

func (h *harness) environ() []string {
	env := []string{
		"HOME=" + h.home,
		// /usr/local/bin deliberately absent: there is no docker on this PATH, so
		// the default container substrate can never start a real container.
		"PATH=/usr/bin:/bin:/usr/sbin:/sbin",
		"LANG=C.UTF-8",
		"TMPDIR=" + h.t.TempDir(),
	}
	for k, v := range h.env {
		for _, f := range forbiddenEnv {
			if k == f {
				h.t.Fatalf("test tried to set forbidden env %s", k)
			}
		}
		env = append(env, k+"="+v)
	}
	return env
}

type result struct {
	code     int
	signaled bool
	stdout   string
	stderr   string
	dur      time.Duration
	timedOut bool
	maxRSS   int64 // bytes
}

func (r result) all() string { return r.stdout + r.stderr }

type proc struct {
	h       *harness
	cmd     *exec.Cmd
	started time.Time
	outPath string
	errPath string
	waited  chan struct{}
	res     result
}

// start writes config.yml and launches the binary.
func (h *harness) start(timeout time.Duration, args ...string) *proc {
	h.t.Helper()
	h.writeConfig()
	return h.startNoConfig(timeout, args...)
}

// startNoConfig launches the binary in its own process group with stdin at
// /dev/null, over whatever config.yml is on disk. stdout and stderr go to files,
// so an orphan holding them can't wedge Wait.
func (h *harness) startNoConfig(timeout time.Duration, args ...string) *proc {
	h.t.Helper()
	dir := h.t.TempDir()
	p := &proc{h: h, outPath: filepath.Join(dir, "stdout"), errPath: filepath.Join(dir, "stderr"), waited: make(chan struct{})}
	stdout, err := os.Create(p.outPath)
	if err != nil {
		h.t.Fatal(err)
	}
	stderr, err := os.Create(p.errPath)
	if err != nil {
		h.t.Fatal(err)
	}
	p.cmd = exec.Command(fuseBin, args...)
	p.cmd.Dir = h.cwd
	p.cmd.Env = h.environ()
	p.cmd.Stdout = stdout
	p.cmd.Stderr = stderr
	p.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	p.started = time.Now()
	if err := p.cmd.Start(); err != nil {
		h.t.Fatal(err)
	}
	pgid := p.cmd.Process.Pid
	// Every child of the binary stays in this group unless it moves itself, so
	// the group kill reaches all of them, and only them.
	h.t.Cleanup(func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) })
	var timedOut atomic.Bool
	timer := time.AfterFunc(timeout, func() {
		timedOut.Store(true)
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
	})
	go func() {
		defer close(p.waited)
		_ = p.cmd.Wait()
		timer.Stop()
		stdout.Close()
		stderr.Close()
		p.res.timedOut = timedOut.Load()
		p.res.dur = time.Since(p.started)
		ps := p.cmd.ProcessState
		p.res.code = ps.ExitCode()
		if ws, ok := ps.Sys().(syscall.WaitStatus); ok {
			p.res.signaled = ws.Signaled()
		}
		if ru, ok := ps.SysUsage().(*syscall.Rusage); ok {
			p.res.maxRSS = int64(ru.Maxrss)
			if runtime.GOOS != "darwin" {
				p.res.maxRSS *= 1024
			}
		}
		o, _ := os.ReadFile(p.outPath)
		e, _ := os.ReadFile(p.errPath)
		p.res.stdout, p.res.stderr = string(o), string(e)
	}()
	h.t.Cleanup(func() { <-p.waited })
	return p
}

func (p *proc) wait() result {
	<-p.waited
	return p.res
}

// signal sends sig to the binary alone, not to its process group.
func (p *proc) signal(sig syscall.Signal) {
	p.h.t.Helper()
	if err := p.cmd.Process.Signal(sig); err != nil {
		p.h.t.Fatalf("signal: %v", err)
	}
}

const defaultTimeout = 45 * time.Second

// run execs a one-shot task to completion with the given flags. A wedged binary
// is killed and fails the test.
func (h *harness) run(flags ...string) result {
	h.t.Helper()
	args := append([]string{"--model", "m"}, flags...)
	args = append(args, "e2e task")
	r := h.start(defaultTimeout, args...).wait()
	if r.timedOut {
		h.t.Fatalf("binary did not exit within %s\nstdout: %s\nstderr: %s", defaultTimeout, tail(r.stdout), tail(r.stderr))
	}
	return r
}

func tail(s string) string {
	if len(s) > 3000 {
		return "..." + s[len(s)-3000:]
	}
	return s
}

// lastRequest fails the test when the server never saw a call.
func (h *harness) lastRequest() *request {
	h.t.Helper()
	reqs := h.srv.requests()
	if len(reqs) == 0 {
		h.t.Fatal("server saw no requests")
	}
	return reqs[len(reqs)-1]
}

// toolOutput returns what the model was told about call_1.
func (h *harness) toolOutput() string {
	h.t.Helper()
	out, ok := h.lastRequest().toolResult("call_1")
	if !ok {
		h.t.Fatalf("no tool result for call_1 reached the model\nraw: %s", tail(string(h.lastRequest().Raw)))
	}
	return out
}

func wantCode(t *testing.T, r result, want int) {
	t.Helper()
	if r.code != want {
		t.Fatalf("exit code = %d, want %d\nstdout: %s\nstderr: %s", r.code, want, tail(r.stdout), tail(r.stderr))
	}
}

func wantContains(t *testing.T, haystack, needle, what string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Fatalf("%s does not contain %q:\n%s", what, needle, tail(haystack))
	}
}

func wantNotContains(t *testing.T, haystack, needle, what string) {
	t.Helper()
	if strings.Contains(haystack, needle) {
		t.Fatalf("%s contains %q:\n%s", what, needle, tail(haystack))
	}
}

// refusedPort returns a loopback port nothing is listening on.
func refusedPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port
}

var markerSeq atomic.Int64

// sleepMarker returns a unique, harmless, long `sleep` argument to find a child
// by in the process table.
func sleepMarker() string {
	return fmt.Sprintf("7%03d.%d7", os.Getpid()%1000, markerSeq.Add(1))
}

// processesMatching lists process-table lines containing marker.
func processesMatching(t *testing.T, marker string) []string {
	t.Helper()
	out, err := exec.Command("/bin/ps", "-axo", "pid=,pgid=,command=").Output()
	if err != nil {
		t.Fatalf("ps: %v", err)
	}
	var hits []string
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, marker) {
			hits = append(hits, strings.TrimSpace(line))
		}
	}
	return hits
}

// waitNoProcess waits briefly for marker to leave the process table.
func waitNoProcess(t *testing.T, marker string, within time.Duration) []string {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		hits := processesMatching(t, marker)
		if len(hits) == 0 || time.Now().After(deadline) {
			return hits
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func mustNotExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); err == nil {
		t.Fatalf("%s exists, want it absent", path)
	}
}

// walkFiles visits every entry under root.
func walkFiles(root string, fn func(path string, info os.FileInfo)) {
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err == nil && info != nil {
			fn(p, info)
		}
		return nil
	})
}
