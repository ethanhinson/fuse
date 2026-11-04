package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/artifact"
	artfs "github.com/ethanhinson/fuse/internal/artifact/fsstore"
	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/event/fsstore"
	"github.com/ethanhinson/fuse/internal/loopconnect"
	loopv1 "github.com/ethanhinson/fuse/internal/loopwire/v1"
	"github.com/ethanhinson/fuse/internal/loopwire/v1/loopv1connect"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/permissions"
	"github.com/ethanhinson/fuse/internal/runtime"
	"github.com/ethanhinson/fuse/internal/segment"
)

// spillRefRE matches the art:// ref a spill marker names; the marker ends the
// ref with a space.
var spillRefRE = regexp.MustCompile(`art://[^\s"\\\]]+`)

// TestSpillRefRecoveredOnAnotherRuntime: an oversized tool result on runtime A
// is spilled to the shared artifact store and the transcript carries only its
// art:// ref, never a filesystem path. After A reaps the loop, runtime B resumes
// it and the model's read_file on that ref recovers the elided middle, through
// B's own binding of the same (tenant, loop).
func TestSpillRefRecoveredOnAnotherRuntime(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// ~42 KiB fixture; the needle sits on line 300, outside the 8 KiB head and
	// tail the inline preview keeps.
	const needle = "NEEDLE-7f3a"
	fixtureDir := t.TempDir()
	fixture := filepath.Join(fixtureDir, "big.txt")
	var sb strings.Builder
	for i := 1; i <= 600; i++ {
		line := fmt.Sprintf("line %04d %s", i, strings.Repeat("x", 58))
		if i == 300 {
			line = fmt.Sprintf("line %04d %s %s", i, needle, strings.Repeat("y", 46))
		}
		sb.WriteString(line + "\n")
	}
	if err := os.WriteFile(fixture, []byte(sb.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	var (
		mu          sync.Mutex
		recoveryRef string
		bReadIssued bool
		needleSeen  bool
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var c gatewayCall
		_ = json.Unmarshal(body, &c)
		w.Header().Set("Content-Type", "application/json")
		mu.Lock()
		defer mu.Unlock()

		onB := false
		for _, m := range c.Messages {
			if m.Role == "user" && strings.Contains(m.Content, "recover it") {
				onB = true
			}
		}
		last := ""
		if n := len(c.Messages); n > 0 {
			last = c.Messages[n-1].Role
		}
		switch {
		case !onB && last == "user":
			io.WriteString(w, toolCallsReply(call{name: "read_file", args: map[string]any{"path": fixture}}))
		case !onB:
			io.WriteString(w, textReply("read it"))
		case last == "user":
			for _, m := range c.Messages {
				if m.Role == "tool" {
					if ref := spillRefRE.FindString(m.Content); ref != "" {
						recoveryRef = ref
						break
					}
				}
			}
			if recoveryRef == "" {
				io.WriteString(w, textReply("no ref in history"))
				return
			}
			bReadIssued = true
			io.WriteString(w, toolCallsReply(call{name: "read_file", args: map[string]any{
				"path": recoveryRef, "start_line": 295, "end_line": 305,
			}}))
		default:
			res := toolResults(c)
			if len(res) > 0 && strings.Contains(res[len(res)-1], needle) {
				needleSeen = true
			}
			io.WriteString(w, textReply("recovered"))
		}
	}))
	defer srv.Close()
	t.Setenv("LLM_GATEWAY_URL", srv.URL)
	t.Setenv("LLM_GATEWAY_KEY", "tkn")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config load: %v", err)
	}
	reg := registryFromConfig(cfg)
	alias := reg.Default

	store := fsstore.NewDurableFSStore(t.TempDir())
	artsDir := t.TempDir()
	sharedArts := artfs.New(artsDir)

	buildDeps := func(idleTTL time.Duration) runtime.Deps {
		d := buildLoopServerRuntimeDeps(nil, cfg, reg, alias, defaultToolRegistry(nil, cfg.Research, nil),
			spawnAgentBlock, permissions.AlwaysApprove, sessionRateGate(cfg))
		d.DurableStore = store
		d.Registry = store
		d.Artifacts = sharedArts
		d.BaseDir = ""
		d.IdleTTL = idleTTL
		return d
	}
	rtA := runtime.New(buildDeps(150 * time.Millisecond))
	rtB := runtime.New(buildDeps(time.Hour))

	serverFor := func(rt runtime.Runtime) (loopv1connect.LoopServiceClient, func()) {
		_, h := loopv1connect.NewLoopServiceHandler(loopconnect.NewHandler(rt).WithKeepalive(time.Hour))
		s := httptest.NewServer(h)
		return loopv1connect.NewLoopServiceClient(s.Client(), s.URL), s.Close
	}
	clientA, closeA := serverFor(rtA)
	defer closeA()
	clientB, closeB := serverFor(rtB)
	defer closeB()

	ctx := context.Background()
	startResp, err := clientA.StartLoop(ctx, connect.NewRequest(&loopv1.StartLoopRequest{
		Task: "read the big file", Model: alias, Interactive: true,
	}))
	if err != nil {
		t.Fatalf("A StartLoop: %v", err)
	}
	loopID := startResp.Msg.LoopId
	waitForKindInStore(t, rtA, loopID, event.KindLoopParked)

	// A's transcript carries the spill as an art:// ref and never a filesystem
	// path: not the fixture's directory, not the store's, not anything under HOME.
	hist, err := rtA.Attach(ctx, event.DefaultTenant, loopID, 0)
	if err != nil {
		t.Fatalf("A Attach: %v", err)
	}
	var spilled bool
	for _, e := range hist {
		if e.Kind != event.KindToolResult {
			continue
		}
		var p event.ToolResultPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("tool.result payload: %v", err)
		}
		for _, dir := range []string{fixtureDir, artsDir, home} {
			if strings.Contains(string(e.Payload), dir) {
				t.Fatalf("tool.result payload leaks absolute path %q:\n%s", dir, tailStr(p.Result, 400))
			}
		}
		wantPrefix := "art://" + string(event.DefaultTenant) + "/" + loopID + "/" + string(artifact.KindSpill) + "/"
		if strings.Contains(p.Result, wantPrefix) {
			spilled = true
		}
	}
	if !spilled {
		t.Fatalf("no tool.result on A carries an art:// spill ref for loop %s: %v", loopID, kindList(hist))
	}

	waitForNotLive(t, store, loopID)
	if _, err := clientB.Send(ctx, connect.NewRequest(&loopv1.SendRequest{LoopId: loopID, Input: "recover it"})); err != nil {
		t.Fatalf("B Send: %v", err)
	}
	waitForParkCount(t, rtB, loopID, 2)

	mu.Lock()
	defer mu.Unlock()
	if !bReadIssued {
		t.Fatalf("B never issued read_file on the spill ref (ref %q)", recoveryRef)
	}
	if !needleSeen {
		t.Fatalf("B's read_file on %s did not recover %s", recoveryRef, needle)
	}
}

// TestSegmentRecoveredOnAnotherRuntime: the loop server's production factory
// installs a segment sink on the agent it builds, so a compaction on runtime A
// archives the raw region under the loop's (tenant, loop) scope in the shared
// store. An independent runtime B graph bound to the same scope recovers it
// with segment_read and grep, and another tenant bound to the same loop id
// cannot.
func TestSegmentRecoveredOnAnotherRuntime(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	const cannedSummary = "Objective: verify cross-runtime\nState: compacted\nNext: proceed\nFiles: none"
	var (
		mu          sync.Mutex
		mainReqBody string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(string(b), `"tool_choice":"none"`) && strings.Contains(string(b), "Objective:") {
			io.WriteString(w, textReplyUsage(cannedSummary, 10, 5))
			return
		}
		mu.Lock()
		mainReqBody = string(b)
		mu.Unlock()
		io.WriteString(w, textReplyUsage("done", 10, 2))
	}))
	defer srv.Close()
	t.Setenv("LLM_GATEWAY_URL", srv.URL)
	t.Setenv("LLM_GATEWAY_KEY", "tkn")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config load: %v", err)
	}
	cfg.Context.Summarization.Enabled = true
	reg := registryFromConfig(cfg)
	alias := reg.Default

	shared := artfs.New(t.TempDir())
	const tenant, loopID = "acme", "loopseg"
	ctx := context.Background()

	// Runtime A: the production loop-server factory builds the agent (and so
	// its segment sink) from the loop's bound registry.
	depsA := buildLoopServerRuntimeDeps(nil, cfg, reg, alias, defaultToolRegistry(nil, cfg.Research, nil),
		spawnAgentBlock, permissions.AlwaysApprove, sessionRateGate(cfg))
	regA := depsA.NewToolRegistry()
	regA.WithArtifacts(shared, tenant, loopID)
	a, _, _, err := depsA.BuildAgent(event.NoopStore{}, agent.NewAgentTreeWithRootID(alias, alias, 2, loopID), alias, regA)
	if err != nil {
		t.Fatalf("A BuildAgent: %v", err)
	}
	if depsA.LoopTeardown != nil {
		defer depsA.LoopTeardown(regA)
	}
	a.ContextWindow = 4000

	big := strings.Repeat("RAWDATA-", 1000)
	history := []model.Message{
		{Role: "user", Content: "task"},
		{Role: "assistant", ToolCalls: []model.ToolCall{{ID: "1", Name: "read_file"}}},
		{Role: "tool", ToolCallID: "1", Name: "read_file", Content: big},
		{Role: "assistant", ToolCalls: []model.ToolCall{{ID: "2", Name: "read_file"}}},
		{Role: "tool", ToolCallID: "2", Name: "read_file", Content: big},
		{Role: "assistant", ToolCalls: []model.ToolCall{{ID: "3", Name: "grep"}}},
		{Role: "tool", ToolCallID: "3", Name: "grep", Content: big},
	}
	if _, err := a.Run(ctx, history); err != nil {
		t.Fatalf("A run: %v", err)
	}

	mu.Lock()
	body := mainReqBody
	mu.Unlock()
	wantPointer := "Recovery: grep your past at art://" + tenant + "/" + loopID + "/" + string(artifact.KindSegment) + "/"
	if !strings.Contains(body, wantPointer) {
		t.Fatalf("post-compaction main request missing %q:\n%s", wantPointer, tailStr(body, 600))
	}

	// Runtime B: an independent deps graph, bound to the same scope.
	depsB := buildLoopServerRuntimeDeps(nil, cfg, reg, alias, defaultToolRegistry(nil, cfg.Research, nil),
		spawnAgentBlock, permissions.AlwaysApprove, sessionRateGate(cfg))
	regB := depsB.NewToolRegistry()
	regB.WithArtifacts(shared, tenant, loopID)

	idx, err := segment.NewArtifactSource(regB.Artifacts()).ReadIndex(ctx)
	if err != nil || len(idx.Segments) != 1 {
		t.Fatalf("B index = %+v (err %v), want exactly one segment", idx, err)
	}
	e := idx.Segments[0]
	turnRange := fmt.Sprintf("%d-%d", e.TurnStart, e.TurnEnd)
	pointer := artifact.Ref{Tenant: tenant, Loop: loopID, Kind: artifact.KindSegment, ID: e.Path}.String()
	if !strings.Contains(body, pointer) {
		t.Fatalf("recovery pointer does not name the archived segment %q", pointer)
	}

	res := regB.Execute(ctx, "segment_read", argsJSON(map[string]any{"turn_range": turnRange}))
	if res.IsError || !strings.Contains(res.Output, "RAWDATA") {
		t.Fatalf("B segment_read %s: error=%v %s", turnRange, res.IsError, tailStr(res.Output, 200))
	}
	gres := regB.Execute(ctx, "grep", argsJSON(map[string]any{"pattern": "RAWDATA", "path": pointer}))
	if gres.IsError || !strings.Contains(gres.Output, "RAWDATA") {
		t.Fatalf("B grep on %s: error=%v %s", pointer, gres.IsError, tailStr(gres.Output, 200))
	}

	// Another tenant bound to the same loop id sees nothing of it.
	regC := depsB.NewToolRegistry()
	regC.WithArtifacts(shared, "other", loopID)
	cres := regC.Execute(ctx, "segment_read", argsJSON(map[string]any{"turn_range": turnRange}))
	if strings.Contains(cres.Output, "RAWDATA") {
		t.Fatalf("tenant other read tenant %s's segment: %s", tenant, tailStr(cres.Output, 200))
	}
	if !strings.Contains(cres.Output, "no segments") {
		t.Fatalf("tenant other segment_read = %q, want a no-segments result", cres.Output)
	}
	rres := regC.Execute(ctx, "read_file", argsJSON(map[string]any{"path": pointer}))
	if !rres.IsError || !strings.Contains(rres.Output, "artifact not found") {
		t.Fatalf("tenant other read_file %s = (error=%v) %q, want artifact not found", pointer, rres.IsError, rres.Output)
	}
}
