package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/permissions"
	"github.com/ethanhinson/fuse/internal/probe"
	"github.com/ethanhinson/fuse/internal/runtime"
)

// TestTwoBindingsOneSeamEventParity proves spec Verification bullet 2: a CLI run
// (research-probe binding #1b) and a loop-server run (binding #2) for the SAME task
// emit the identical normalized event.Event stream. Each side is fed its OWN
// production source — its own deps builder, its own engine, its own fsstore
// (learning parity-test-feeds-each-side-its-own-production-source). The scripted
// httptest gateway is shared as the MODEL only (learning
// verify-tool-loop-at-gateway-seam); the two event streams are produced
// independently, never copied.
//
// The two bindings are driven SEQUENTIALLY here only because this test compares ONE
// run per side for byte-equivalence; each side already owns an isolated per-loop
// fsstore (change 0046 retired the process-global holder, so overlapping runs no
// longer clobber — that isolation is proven separately by the N-concurrent-loop
// tests). Sequential single-turn runs keep the comparison deterministic, read back
// via runtime.Attach.
func TestTwoBindingsOneSeamEventParity(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	const task = "PARITY-TASK-XYZZY"
	const reply = "PARITY-REPLY-QUUX"

	// One scripted gateway, shared as the MODEL only. A deterministic single-turn
	// assistant reply with no tool calls, so each engine runs exactly one turn.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"`+reply+`"}}]}`)
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
	// Derive the resolved gateway model id from the registry so the load-bearing guard
	// tracks a registry-default change instead of a brittle hardcoded literal.
	defMC, rerr := reg.Resolve(alias)
	if rerr != nil {
		t.Fatalf("resolve default model %q: %v", alias, rerr)
	}
	wantModelID := defMC.ID

	// --- CLI side (binding #1b: research-probe) — its OWN engine + fsstore. ---
	cliEvents := runResearchProbeSideForParity(t, cfg, reg, alias, task)

	// --- loop-server side (binding #2) — its OWN engine + fsstore. ---
	serverEvents := runLoopServerSideForParity(t, cfg, reg, alias, task)

	// Normalize both streams: drop volatile fields (Seq, TS, node IDs) and compare
	// the []Kind sequence plus the load-bearing payload fields.
	cliNorm := normalizeStream(t, cliEvents)
	serverNorm := normalizeStream(t, serverEvents)

	if len(cliNorm) == 0 {
		t.Fatalf("CLI side produced no events")
	}
	// Guard against a trivially-equal empty-payload stream: the run MUST have gone
	// through the gateway (a model.call.start with the resolved model id) and closed
	// the turn — otherwise "identical" would be vacuous.
	assertLoadBearingStream(t, cliNorm, wantModelID, reply)
	if !reflect.DeepEqual(cliNorm, serverNorm) {
		t.Fatalf("event streams diverge.\n CLI (%d): %+v\n SRV (%d): %+v", len(cliNorm), cliNorm, len(serverNorm), serverNorm)
	}
}

// runResearchProbeSideForParity constructs the research-probe binding's OWN deps
// (buildResearchProbeRuntimeDeps, act=nil freeform fan-out) but overrides BaseDir to
// a temp fsstore so the durable stream is readable via Attach. It drives the engine
// through the same runtime.StartLoop the production binding uses and returns the
// durable event stream.
func runResearchProbeSideForParity(t *testing.T, cfg config.Config, reg *model.Registry, alias, task string) []event.Event {
	t.Helper()
	tree := agent.NewAgentTreeWithConcurrency(alias, alias, cfg.Agents.MaxConcurrent)
	sched := tree.Scheduler()
	sched.SetMaxSpawns(cfg.Agents.MaxSpawns)
	sched.SetQueueBound(cfg.Agents.QueueBound)
	sched.SetSessionTokens(cfg.Throughput.SessionTokens)
	rootID := tree.RootID()
	logSink := probe.NewLog()

	deps := buildResearchProbeRuntimeDeps(researchProbeDepsInput{
		cfg: cfg, reg: reg, alias: alias, toolReg: defaultToolRegistry(cfg.Research, nil),
		tree: tree, act: nil, rootID: rootID, logSink: logSink, traceW: nil, rateGate: sessionRateGate(cfg),
	})
	// Override the production NoopStore with a real temp fsstore so the durable
	// stream is readable; the wiring under test is unchanged.
	deps.BaseDir = t.TempDir()

	return driveLoopAndAttach(t, deps, task, alias)
}

// runLoopServerSideForParity constructs binding #2's OWN deps
// (buildLoopServerRuntimeDeps) but overrides BaseDir to a temp fsstore so the run is
// isolated from the real session dir. It drives the same runtime.StartLoop and
// returns the durable event stream.
func runLoopServerSideForParity(t *testing.T, cfg config.Config, reg *model.Registry, alias, task string) []event.Event {
	t.Helper()
	deps := buildLoopServerRuntimeDeps(cfg, reg, alias, defaultToolRegistry(cfg.Research, nil),
		spawnAgentBlock, permissions.AlwaysApprove, sessionRateGate(cfg))
	deps.BaseDir = t.TempDir()
	return driveLoopAndAttach(t, deps, task, alias)
}

// driveLoopAndAttach runs one loop through runtime.StartLoop, waits for it, and
// reads the durable event stream via Attach(loopID, 0).
func driveLoopAndAttach(t *testing.T, deps runtime.Deps, task, alias string) []event.Event {
	t.Helper()
	rt := runtime.New(deps)
	h, err := rt.StartLoop(context.Background(), runtime.LoopConfig{Task: task, ModelID: alias})
	if err != nil {
		t.Fatalf("StartLoop: %v", err)
	}
	if _, werr := h.Wait(); werr != nil {
		t.Fatalf("loop run error: %v", werr)
	}
	evs, aerr := rt.Attach(h.ID(), 0)
	if aerr != nil {
		t.Fatalf("Attach: %v", aerr)
	}
	return evs
}

// normEvent is the volatile-field-stripped comparison shape: the Kind, the Turn, and
// the load-bearing payload fields (per kind). Seq, TS, and node IDs are volatile and
// omitted so two independently-produced streams for the same run compare equal.
type normEvent struct {
	Kind    event.Kind
	Turn    int
	Payload map[string]any
}

// normalizeStream reduces a raw event stream to the comparable normEvent slice: it
// keeps Kind + Turn and the kind-specific load-bearing payload fields (model id,
// content, tool names, error text), dropping Seq/TS/node-IDs (volatile per store).
func normalizeStream(t *testing.T, evs []event.Event) []normEvent {
	t.Helper()
	out := make([]normEvent, 0, len(evs))
	for _, e := range evs {
		out = append(out, normEvent{Kind: e.Kind, Turn: e.Turn, Payload: normalizePayload(t, e)})
	}
	return out
}

// normalizePayload extracts the load-bearing, non-volatile fields per kind. Volatile
// or run-specific values (msg counts, token counts, child node IDs) are dropped so a
// divergence in a LOAD-BEARING field (model id, content, error) still trips the test.
func normalizePayload(t *testing.T, e event.Event) map[string]any {
	t.Helper()
	if len(e.Payload) == 0 {
		return nil
	}
	m := map[string]any{}
	switch e.Kind {
	case event.KindModelCallStart:
		var p event.ModelCallStartPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("unmarshal %s: %v", e.Kind, err)
		}
		m["model"] = p.Model // load-bearing: same model id both sides
	case event.KindModelCallEnd:
		var p event.ModelCallEndPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("unmarshal %s: %v", e.Kind, err)
		}
		m["content"] = p.Content // load-bearing: same assistant reply both sides
		names := make([]string, 0, len(p.ToolCalls))
		for _, tc := range p.ToolCalls {
			names = append(names, tc.Name)
		}
		m["tool_calls"] = names
	case event.KindTurnStart:
		var p event.TurnStartPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("unmarshal %s: %v", e.Kind, err)
		}
		m["turn"] = p.Turn
	case event.KindTurnEnd:
		var p event.TurnEndPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("unmarshal %s: %v", e.Kind, err)
		}
		m["turn"] = p.Turn
	case event.KindError:
		var p event.ErrorPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("unmarshal %s: %v", e.Kind, err)
		}
		m["err"] = p.Err
		m["turn"] = p.Turn
	default:
		// Other kinds compare on Kind+Turn alone (payload dropped as volatile).
	}
	return m
}

// assertLoadBearingStream guards against a vacuous parity pass on two empty streams:
// it requires the normalized stream to contain a turn.start, a model.call.start
// carrying the resolved gateway model id (wantModelID, derived from the registry
// default so a registry change doesn't make this guard brittle), a model.call.end
// carrying the scripted reply, and a turn.end — the substance that makes "identical"
// meaningful.
func assertLoadBearingStream(t *testing.T, ns []normEvent, wantModelID, wantReply string) {
	t.Helper()
	var sawTurnStart, sawTurnEnd, sawModelID, sawReply bool
	for _, e := range ns {
		switch e.Kind {
		case event.KindTurnStart:
			sawTurnStart = true
		case event.KindTurnEnd:
			sawTurnEnd = true
		case event.KindModelCallStart:
			if e.Payload["model"] == wantModelID {
				sawModelID = true
			}
		case event.KindModelCallEnd:
			if e.Payload["content"] == wantReply {
				sawReply = true
			}
		}
	}
	if !sawTurnStart || !sawTurnEnd || !sawModelID || !sawReply {
		t.Fatalf("stream missing load-bearing events: turnStart=%v turnEnd=%v modelID=%v reply=%v\n%+v",
			sawTurnStart, sawTurnEnd, sawModelID, sawReply, ns)
	}
}
