package loopserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ethanhinson/fuse/internal/event"
)

// The thin stateless HTTP replay endpoint (binding #3, D3) exposes the durable
// catch-up path — runtime.Runtime.Attach(loop_id, from) — as a plain GET, distinct
// from the WS full-session transport. It carries NO live tail and holds NO
// connection state: every request is an independent, idempotent history read.
//
// Tenant is carried unenforced via the ?tenant= query param (identity is #0049),
// defaulting to empty when absent, exactly as the WS observe params carry it.

// getReplay issues a GET against the replay handler mounted on an httptest server and
// returns the status code, Content-Type, and raw body bytes.
func getReplay(t *testing.T, srv *httptest.Server, path string) (int, string, []byte) {
	t.Helper()
	resp, err := http.Get(srv.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, resp.Header.Get("Content-Type"), body
}

func TestReplayReturnsAttachHistory(t *testing.T) {
	hist := []event.Event{
		{Seq: 2, Kind: event.KindModelCallStart},
		{Seq: 3, Kind: event.KindTurnEnd},
	}
	fr := &fakeRuntime{attachHist: hist}
	srv := httptest.NewServer(NewReplayHandler(fr))
	t.Cleanup(srv.Close)

	// GET /loops/{id}/events?from=<seq>&tenant=<t> returns the durable history as a
	// JSON array equal to what rt.Attach returns, Content-Type application/json.
	status, ctype, body := getReplay(t, srv, "/loops/loop-http/events?from=1&tenant=acme")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, body)
	}
	if ctype != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ctype)
	}

	// Body equals exactly what rt.Attach(ctx, tenant, id, from) returns, marshaled.
	want, _ := json.Marshal(hist)
	var gotEvents []event.Event
	if err := json.Unmarshal(body, &gotEvents); err != nil {
		t.Fatalf("unmarshal body: %v (body=%s)", err, body)
	}
	var wantEvents []event.Event
	_ = json.Unmarshal(want, &wantEvents)
	if len(gotEvents) != len(wantEvents) {
		t.Fatalf("got %d events, want %d", len(gotEvents), len(wantEvents))
	}
	for i := range wantEvents {
		if gotEvents[i].Seq != wantEvents[i].Seq || gotEvents[i].Kind != wantEvents[i].Kind {
			t.Fatalf("event[%d] = %+v, want %+v", i, gotEvents[i], wantEvents[i])
		}
	}

	// The tenant query param reaches the seam unenforced.
	if fr.attachTenant != event.TenantID("acme") {
		t.Fatalf("attach tenant = %q, want acme", fr.attachTenant)
	}
}

func TestReplayIsStatelessAndIdempotent(t *testing.T) {
	fr := &fakeRuntime{attachHist: []event.Event{
		{Seq: 5, Kind: event.KindTurnStart},
	}}
	srv := httptest.NewServer(NewReplayHandler(fr))
	t.Cleanup(srv.Close)

	// Statelessness: a second identical request returns byte-identical output — no
	// live tail, no accumulated connection state.
	s1, _, b1 := getReplay(t, srv, "/loops/loop-http/events?from=0")
	s2, _, b2 := getReplay(t, srv, "/loops/loop-http/events?from=0")
	if s1 != http.StatusOK || s2 != http.StatusOK {
		t.Fatalf("statuses = %d, %d, want 200,200", s1, s2)
	}
	if string(b1) != string(b2) {
		t.Fatalf("non-idempotent: first=%s second=%s", b1, b2)
	}
}

func TestReplayDefaultsTenantEmptyAndFromZero(t *testing.T) {
	fr := &fakeRuntime{attachHist: []event.Event{}}
	srv := httptest.NewServer(NewReplayHandler(fr))
	t.Cleanup(srv.Close)

	// No ?tenant and no ?from: tenant defaults to empty, from defaults to 0.
	status, ctype, body := getReplay(t, srv, "/loops/loop-http/events")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, body)
	}
	if ctype != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ctype)
	}
	if fr.attachTenant != event.TenantID("") {
		t.Fatalf("default tenant = %q, want empty", fr.attachTenant)
	}
	// An empty history still marshals to a JSON array, never null.
	var arr []event.Event
	if err := json.Unmarshal(body, &arr); err != nil {
		t.Fatalf("empty history must be a JSON array: %v (body=%s)", err, body)
	}
}

func TestReplayUnknownLoopMapsTo404(t *testing.T) {
	fr := &fakeRuntime{attachErr: errors.New("loop not found: loop-x")}
	srv := httptest.NewServer(NewReplayHandler(fr))
	t.Cleanup(srv.Close)

	// Attach error (e.g. unknown loop) -> 404 with a JSON error body.
	status, ctype, body := getReplay(t, srv, "/loops/loop-x/events?from=0")
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", status, body)
	}
	if ctype != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ctype)
	}
	var errBody map[string]any
	if err := json.Unmarshal(body, &errBody); err != nil {
		t.Fatalf("404 body must be JSON: %v (body=%s)", err, body)
	}
	if _, ok := errBody["error"]; !ok {
		t.Fatalf("404 JSON body missing \"error\" field: %s", body)
	}
}

func TestReplayMalformedFromMapsTo400(t *testing.T) {
	fr := &fakeRuntime{attachHist: []event.Event{{Seq: 1, Kind: event.KindTurnStart}}}
	srv := httptest.NewServer(NewReplayHandler(fr))
	t.Cleanup(srv.Close)

	for _, bad := range []string{"abc", "-1", "1.5", "9999999999999999999999"} {
		status, ctype, body := getReplay(t, srv, "/loops/loop-http/events?from="+bad)
		if status != http.StatusBadRequest {
			t.Fatalf("from=%q status = %d, want 400; body=%s", bad, status, body)
		}
		if ctype != "application/json" {
			t.Fatalf("from=%q Content-Type = %q, want application/json", bad, ctype)
		}
	}
}

// No loop.start / loop.send on HTTP: the replay handler answers ONLY the GET events
// route (WS-only mutation, D3). Any other method/path is not the replay endpoint.
func TestReplayRejectsNonGetAndUnknownPaths(t *testing.T) {
	fr := &fakeRuntime{}
	srv := httptest.NewServer(NewReplayHandler(fr))
	t.Cleanup(srv.Close)

	// A POST to the events route is not served by the GET-only pattern.
	resp, err := http.Post(srv.URL+"/loops/loop-http/events", "application/json", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("POST to replay route returned 200; mutation must be WS-only (D3)")
	}

	// The handler never invoked Send (no mutation path exists on HTTP).
	_ = context.Background()
	if fr.sendLoopID != "" {
		t.Fatalf("HTTP handler must never call Send; got loopID=%q", fr.sendLoopID)
	}
}
