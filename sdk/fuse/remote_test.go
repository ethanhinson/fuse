package fuse

import (
	"context"
	"errors"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/loopauth"
	"github.com/ethanhinson/fuse/internal/loopconnect"
	"github.com/ethanhinson/fuse/internal/loopwire/v1/loopv1connect"
)

// remoteFakeServer stands up a REAL connect server over the loopconnect handler wired
// to a fakeRuntime, with the auth interceptor seeded from verifier. It returns the base
// URL. This proves the wire (not the engine — that is Task 6): the SDK's remote backend
// dials a genuine Connect endpoint.
func remoteFakeServer(t *testing.T, f *fakeRuntime, verifier loopauth.Verifier) string {
	t.Helper()
	handler := loopconnect.NewHandler(f).WithKeepalive(time.Hour)
	path, h := loopv1connect.NewLoopServiceHandler(handler,
		connect.WithInterceptors(loopconnect.NewAuthInterceptor(verifier)))
	_ = path
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.URL
}

func devVerifier() loopauth.Verifier {
	return loopauth.NewStaticVerifier(map[string]loopauth.Principal{
		"good-tok": {Tenant: event.DefaultTenant, Subject: "alice"},
		"acme-tok": {Tenant: "acme", Subject: "bob"},
	})
}

// TestRemoteRoundTrip drives StartLoop -> Send -> Observe over a REAL connect server,
// asserting the bearer token + tenant reach the server and events round-trip from the
// client.
func TestRemoteRoundTrip(t *testing.T) {
	f := &fakeRuntime{startID: "loop-r", live: make(chan event.Event, 8)}
	f.hist = []event.Event{
		{Seq: 1, Kind: event.KindTurnStart},
		{Seq: 2, Kind: event.KindTurnEnd},
	}
	base := remoteFakeServer(t, f, devVerifier())

	c := NewRemote(base, Credentials{Token: "good-tok", Tenant: string(event.DefaultTenant)})
	ctx := context.Background()

	id, err := c.StartLoop(ctx, StartLoopConfig{Task: "hello", Model: "m", Interactive: true})
	if err != nil {
		t.Fatalf("StartLoop: %v", err)
	}
	if id != "loop-r" {
		t.Fatalf("id = %q, want loop-r", id)
	}
	if f.sawStartCfg.Task != "hello" || !f.sawStartCfg.Interactive {
		t.Fatalf("StartLoop cfg not threaded to server: %+v", f.sawStartCfg)
	}

	if err := c.Send(ctx, id, "again"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if f.sawSendLoop != "loop-r" || f.sawSendIn != "again" {
		t.Fatalf("Send not threaded: loop=%q in=%q", f.sawSendLoop, f.sawSendIn)
	}

	octx, cancel := context.WithCancel(ctx)
	defer cancel()
	ch, unsub, err := c.Observe(octx, id, 0)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	defer unsub()

	f.appendHist(event.Event{Seq: 3, Kind: event.KindLoopParked})
	f.live <- event.Event{Seq: 3, Kind: event.KindLoopParked}

	var last Event
	seqs := collectRemote(t, ch, 3, &last)
	assertNoLossNoDup(t, seqs, 0, 3, "remote round-trip")
	if !IsCompletion(last) {
		t.Fatalf("last event kind = %q, want completion (loop.parked)", last.Kind)
	}
}

// collectRemote drains ch through seq, capturing the last event seen.
func collectRemote(t *testing.T, ch <-chan Event, through Seq, last *Event) []Seq {
	t.Helper()
	var seqs []Seq
	deadline := time.After(5 * time.Second)
	for {
		select {
		case e, ok := <-ch:
			if !ok {
				return seqs
			}
			*last = e
			seqs = append(seqs, e.Seq)
			if e.Seq >= through {
				return seqs
			}
		case <-deadline:
			t.Fatalf("timeout at seq %d; saw %v", through, seqs)
			return seqs
		}
	}
}

// TestRemoteWrongTokenUnauthenticated proves a bad token maps to ErrUnauthenticated.
func TestRemoteWrongTokenUnauthenticated(t *testing.T) {
	f := &fakeRuntime{startID: "loop-r"}
	base := remoteFakeServer(t, f, devVerifier())

	c := NewRemote(base, Credentials{Token: "nope", Tenant: string(event.DefaultTenant)})
	_, err := c.StartLoop(context.Background(), StartLoopConfig{Task: "x"})
	if !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("StartLoop err = %v, want ErrUnauthenticated", err)
	}
}

// TestRemoteTenantSpoofPermissionDenied proves a wire tenant != principal tenant maps
// to ErrPermissionDenied.
func TestRemoteTenantSpoofPermissionDenied(t *testing.T) {
	f := &fakeRuntime{startID: "loop-r"}
	base := remoteFakeServer(t, f, devVerifier())

	// Token "acme-tok" is tenant "acme"; presenting tenant "globex" is a spoof.
	c := NewRemote(base, Credentials{Token: "acme-tok", Tenant: "globex"})
	_, err := c.StartLoop(context.Background(), StartLoopConfig{Task: "x"})
	if !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("StartLoop err = %v, want ErrPermissionDenied", err)
	}
}

// TestRemoteReconnectNoLossNoDup is the REQUIRED acceptance from the client, over the
// REAL wire. On reconnect Observe(fromSeq), the fake backend forces an append into the
// server's subscribe->replay window (an event that is ALSO in durable history), so a
// naive replay would double-deliver it. The client must see every seq exactly once.
func TestRemoteReconnectNoLossNoDup(t *testing.T) {
	live := make(chan event.Event, 16)
	f := &fakeRuntime{live: live}
	f.hist = []event.Event{
		{Seq: 1, Kind: event.KindTurnStart},
		{Seq: 2, Kind: event.KindModelCallStart},
	}

	// The server subscribes (rt.Observe) BEFORE it Attaches. observeGate runs inside
	// rt.Observe: it appends seq 3 to BOTH durable history and the live channel, landing
	// it in the subscribe->replay window. Attach then replays 1,2,3 and the live tail
	// also carries 3 — the server's watermark dedup must drop the live duplicate so the
	// CLIENT sees 3 exactly once.
	var once sync.Once
	f.observeGate = func() {
		once.Do(func() {
			f.appendHist(event.Event{Seq: 3, Kind: event.KindModelCallEnd})
			live <- event.Event{Seq: 3, Kind: event.KindModelCallEnd}
		})
	}

	base := remoteFakeServer(t, f, devVerifier())
	c := NewRemote(base, Credentials{Token: "good-tok", Tenant: string(event.DefaultTenant)})

	octx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, unsub, err := c.Observe(octx, "loop-x", 0)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	defer unsub()

	// A genuinely-new live event past the overlap so the client has a terminal seq.
	f.appendHist(event.Event{Seq: 4, Kind: event.KindTurnEnd})
	live <- event.Event{Seq: 4, Kind: event.KindTurnEnd}

	var last Event
	seqs := collectRemote(t, ch, 4, &last)
	assertNoLossNoDup(t, seqs, 0, 4, "remote reconnect")
}
