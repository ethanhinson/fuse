package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/runtime"
	"github.com/ethanhinson/fuse/internal/tools"
)

// Helpers for acceptance tests on the production one-shot wiring and a scripted
// gateway.

// treeCapture records the tree each loop's BuildAgent is handed, so a test can
// inspect the loop's nodes.
type treeCapture struct {
	mu    sync.Mutex
	trees []*agent.AgentTree
}

// wrap makes deps' BuildAgent record its tree here.
func (c *treeCapture) wrap(deps *runtime.Deps) {
	build := deps.BuildAgent
	deps.BuildAgent = func(store event.EventStore, tree *agent.AgentTree, modelID string, toolReg *tools.Registry) (*agent.Agent, error) {
		c.mu.Lock()
		c.trees = append(c.trees, tree)
		c.mu.Unlock()
		return build(store, tree, modelID, toolReg)
	}
}

func (c *treeCapture) last() *agent.AgentTree {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.trees) == 0 {
		return nil
	}
	return c.trees[len(c.trees)-1]
}

// rawCapture records every raw request body before handing it to next, for
// assertions on fields gatewayCall does not decode (tool schemas).
type rawCapture struct {
	next   http.Handler
	mu     sync.Mutex
	bodies [][]byte
}

func (r *rawCapture) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	r.bodies = append(r.bodies, body)
	r.mu.Unlock()
	req.Body = io.NopCloser(bytes.NewReader(body))
	r.next.ServeHTTP(w, req)
}

// firstFor returns the first raw body whose first user message contains marker.
func (r *rawCapture) firstFor(t *testing.T, marker string) []byte {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, b := range r.bodies {
		var c gatewayCall
		_ = json.Unmarshal(b, &c)
		for _, m := range c.Messages {
			if m.Role == "user" {
				if strings.Contains(m.Content, marker) {
					return b
				}
				break
			}
		}
	}
	t.Fatalf("no request for %q", marker)
	return nil
}

// hostedOperatorYAML is harnessOperator with room for the lead's turns.
const hostedOperatorYAML = `
models:
  default: fast
  fast: {id: cloud/fast}
  house: {id: cloud/house}
max_turns: 4
throughput: {requests_per_minute: 6000}
`

// hostedRun is one finished hosted loop: the gateway, the raw bodies, the
// loop's tree, and the runtime and loop id for reading its durable history.
type hostedRun struct {
	gw     *routedGateway
	raw    *rawCapture
	tree   *agent.AgentTree
	rt     runtime.Runtime
	loopID string
}

// runHostedConfig starts one loop on the production loop-server factory over the
// given operator config and a routed gateway, and waits for it.
func runHostedConfig(t *testing.T, operatorYAML string, route func(task string, turn int, c gatewayCall) string) hostedRun {
	t.Helper()
	gw := &routedGateway{route: route}
	raw := &rawCapture{next: gw}
	deps := harnessServerWith(t, operatorYAML, raw)
	capture := &treeCapture{}
	capture.wrap(&deps)
	rt := runtime.New(deps)
	h, err := rt.StartLoop(context.Background(), runtime.LoopConfig{Task: "ROOT-TASK"})
	if err != nil {
		t.Fatalf("StartLoop: %v", err)
	}
	_, _ = h.Wait()
	tree := capture.last()
	if tree == nil {
		t.Fatal("the loop's BuildAgent never ran")
	}
	return hostedRun{gw: gw, raw: raw, tree: tree, rt: rt, loopID: h.ID()}
}

// turnRequest returns the request for marker that carries exactly turn
// assistant messages.
func turnRequest(t *testing.T, gw *routedGateway, marker string, turn int) gatewayCall {
	t.Helper()
	for _, c := range gw.callsFor(marker) {
		n := 0
		for _, m := range c.Messages {
			if m.Role == "assistant" {
				n++
			}
		}
		if n == turn {
			return c
		}
	}
	t.Fatalf("no %s turn-%d request", marker, turn)
	return gatewayCall{}
}

// countContaining reports how many of results contain sub.
func countContaining(results []string, sub string) int {
	n := 0
	for _, r := range results {
		if strings.Contains(r, sub) {
			n++
		}
	}
	return n
}
