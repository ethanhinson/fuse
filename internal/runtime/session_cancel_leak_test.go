package runtime

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/tools"
)

// The session context is a per-instance resource, and these are its LEAK tests.
//
// An INTERACTIVE (or resumed) launch derives its session context with
// context.WithCancel(context.WithoutCancel(loopCtx)): deliberately detached from the
// request ctx, so a client disconnect cannot unwind a parked run (change 0054 D2, and
// TestInteractiveLoopSurvivesRequestCtxCancel). Detachment is exactly what makes an
// early return dangerous — NOTHING upstream can ever cancel that context. On the happy
// path the cancel is handed to the idle reaper and the run-completion goroutine, which
// are the only things allowed to end a live session. But every early return BETWEEN the
// WithCancel and that handoff exits with the context still live and now unreferenced:
// leaked for the life of the process, not merely for the life of the request
// (learning per-instance-resource-needs-teardown-on-every-early-return, the same rule
// TestStartLoop_SandboxReleasedOnBuildAgentError enforces for the sandbox substrate).
//
// A happy-path test cannot see this and neither can -race: nothing is corrupted,
// something is merely never freed. So each failure path is driven directly, and each
// assertion is made against the PRE-teardown state as well as the post-teardown one —
// proving the context really was outstanding, so the test cannot pass vacuously.
//
// A one-shot loop has no session context at all (sessionCtx == loopCtx, sessionCancel
// nil), so the failure path must leave the caller's ctx untouched; that is asserted
// too, since over-cancelling is the opposite defect.

// sessionCapturingStore is an event.DurableStore that records the context the runtime
// built its durableSink with — which IS the session context (inproc.go: the sink is
// constructed with ctx: sessionCtx). Capturing it here is what lets a test observe a
// context that the production code otherwise never hands back to a caller.
//
// registerErr, when non-nil, fails Register so the launch takes the register early
// return; heartbeatErr and setLiveErr do the same for the two resume paths. The store
// doubles as the event.LoopRegistry so one type serves both seams.
type sessionCapturingStore struct {
	mu      sync.Mutex
	records map[event.StreamKey]event.LoopRecord

	registerErr  error
	heartbeatErr error
	setLiveErr   error
}

func newSessionCapturingStore() *sessionCapturingStore {
	return &sessionCapturingStore{records: map[event.StreamKey]event.LoopRecord{}}
}

func (s *sessionCapturingStore) Append(context.Context, event.StreamKey, event.Event) error {
	return nil
}

func (s *sessionCapturingStore) Subscribe(context.Context, event.StreamKey) (<-chan event.Event, func(), error) {
	ch := make(chan event.Event)
	return ch, func() {}, nil
}

func (s *sessionCapturingStore) Replay(context.Context, event.StreamKey, event.Seq) ([]event.Event, error) {
	return nil, nil
}

func (s *sessionCapturingStore) Register(_ context.Context, rec event.LoopRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.registerErr != nil {
		return s.registerErr
	}
	s.records[rec.Key] = rec
	return nil
}

func (s *sessionCapturingStore) SetLive(_ context.Context, key event.StreamKey, live bool, owner string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.setLiveErr != nil {
		return s.setLiveErr
	}
	rec, ok := s.records[key]
	if !ok {
		return event.ErrLoopUnknown
	}
	rec.Live, rec.OwnerNodeID = live, owner
	s.records[key] = rec
	return nil
}

func (s *sessionCapturingStore) Heartbeat(_ context.Context, key event.StreamKey, owner string, expiry time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.heartbeatErr != nil {
		return s.heartbeatErr
	}
	rec, ok := s.records[key]
	if !ok {
		return event.ErrLoopUnknown
	}
	rec.LeaseExpiry, rec.OwnerNodeID = expiry, owner
	s.records[key] = rec
	return nil
}

func (s *sessionCapturingStore) Resolve(_ context.Context, key event.StreamKey) (event.LoopRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.records[key]
	if !ok {
		return event.LoopRecord{}, event.ErrLoopUnknown
	}
	return rec, nil
}

func (s *sessionCapturingStore) List(_ context.Context, tenant event.TenantID) ([]event.LoopRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []event.LoopRecord
	for k, rec := range s.records {
		if k.Tenant == tenant {
			out = append(out, rec)
		}
	}
	return out, nil
}

// captureSessions installs the launchLoop test hook for the duration of one test and
// returns an accessor for the LAST session context a launch derived. The hook is the
// only way to observe a deliberately-detached context that production never returns.
//
// The accessor fails loudly when nothing was captured, and when the captured launch was
// not interactive — both would make an assertion vacuous.
func captureSessions(t *testing.T) func(*testing.T) context.Context {
	t.Helper()
	var mu sync.Mutex
	var last context.Context
	var lastInteractive bool

	onSessionContext = func(ctx context.Context, interactive bool) {
		mu.Lock()
		defer mu.Unlock()
		last, lastInteractive = ctx, interactive
	}
	t.Cleanup(func() { onSessionContext = nil })

	return func(t *testing.T) context.Context {
		t.Helper()
		mu.Lock()
		defer mu.Unlock()
		if last == nil {
			t.Fatal("no session context was ever derived — the test would pass vacuously")
		}
		if !lastInteractive {
			t.Fatal("the captured launch was not interactive, so it derived no cancelable session context — the test would pass vacuously")
		}
		return last
	}
}

// resetCapture drops an already-captured context so a follow-up launch in the same test
// is asserted on, not the seed launch before it.
func resetCapture(t *testing.T) func(*testing.T) context.Context {
	t.Helper()
	return captureSessions(t)
}

// failingBuildAgentDeps is the common Deps shape: a durable store/registry pair that
// captures the session context, and a BuildAgent that can be told to fail.
func leakDeps(s *sessionCapturingStore, buildErr error) Deps {
	return Deps{
		MaxConcurrent: 1,
		IdleTTL:       time.Hour, // long: the reaper must never mask a missing cancel
		DurableStore:  s,
		Registry:      s,
		BuildAgent: func(event.EventStore, *agent.AgentTree, string, *tools.Registry) (*agent.Agent, agent.ChildBuilder, string, error) {
			if buildErr != nil {
				return nil, nil, "", buildErr
			}
			return agent.New(newGatedCompleter(), execAll{tools.NewRegistry()}, nopRenderer{}, "cloud/x", "", 1, 0), nil, "cloud/x", nil
		},
	}
}

// assertCanceled is the post-teardown half of the assertion.
func assertCanceled(t *testing.T, ctx context.Context, path string) {
	t.Helper()
	select {
	case <-ctx.Done():
		if !errors.Is(ctx.Err(), context.Canceled) {
			t.Fatalf("%s: session ctx ended with %v, want context.Canceled", path, ctx.Err())
		}
	case <-time.After(time.Second):
		t.Fatalf("%s: session context LEAKED — it was never canceled on the early-return path", path)
	}
}

// TestStartLoop_SessionCtxCanceledOnRegisterError drives the register early return
// (inproc.go: "runtime: register loop"), the first failure that can strand a session
// context, since Register runs after the durableSink was built with it.
func TestStartLoop_SessionCtxCanceledOnRegisterError(t *testing.T) {
	s := newSessionCapturingStore()
	s.registerErr = errors.New("register boom")
	session := captureSessions(t)

	rt := New(leakDeps(s, nil))
	if _, err := rt.StartLoop(context.Background(), LoopConfig{
		Task: "hi", ModelID: "cloud/x", Interactive: true,
	}); err == nil {
		t.Fatal("StartLoop must return the Register error")
	}

	// PRE-teardown half: an interactive launch must actually have derived a cancelable
	// session context, or this test proves nothing. captureSessions fails on both.
	assertCanceled(t, session(t), "register error")
}

// TestStartLoop_SessionCtxCanceledOnBuildAgentError drives the LAST early return
// (inproc.go: "runtime: build agent") — the one furthest from the WithCancel, and the
// path the sandbox-teardown test already guards for the substrate. The session context
// must be released on exactly the same path.
func TestStartLoop_SessionCtxCanceledOnBuildAgentError(t *testing.T) {
	s := newSessionCapturingStore()
	session := captureSessions(t)

	rt := New(leakDeps(s, errors.New("build boom")))
	if _, err := rt.StartLoop(context.Background(), LoopConfig{
		Task: "hi", ModelID: "cloud/x", Interactive: true,
	}); err == nil {
		t.Fatal("StartLoop must return the BuildAgent error")
	}

	assertCanceled(t, session(t), "build agent error")
}

// TestResume_SessionCtxCanceledOnHeartbeatError and its set-live sibling cover the two
// resume-only early returns. A resume is ALWAYS interactive (it re-parks to await the
// next Send), so it always derives a session context and always has something to leak.
func TestResume_SessionCtxCanceledOnRegistryErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		arm  func(*sessionCapturingStore)
		path string
	}{
		{
			name: "heartbeat",
			arm:  func(s *sessionCapturingStore) { s.heartbeatErr = errors.New("heartbeat boom") },
			path: "resume heartbeat error",
		},
		{
			name: "setlive",
			arm:  func(s *sessionCapturingStore) { s.setLiveErr = errors.New("setlive boom") },
			path: "resume set-live error",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newSessionCapturingStore()

			// Instance A seeds a durable, resumable loop, then goes away. Its record must
			// be left NOT live and lease-free, so the cold instance below treats the loop
			// as abandoned and takes the rehydrate path (Resume case 2) rather than
			// short-circuiting on a local cache hit (case 1) or refusing a loop owned by a
			// live instance (case 3).
			rtA := New(leakDeps(s, nil))
			hA, err := rtA.StartLoop(context.Background(), LoopConfig{
				Task: "hi", ModelID: "cloud/x", Interactive: true,
			})
			if err != nil {
				t.Fatalf("seed StartLoop: %v", err)
			}
			loopID := hA.ID()

			s.mu.Lock()
			rec := s.records[event.StreamKey{Tenant: event.DefaultTenant, Loop: event.LoopID(loopID)}]
			rec.Live = false
			rec.LeaseExpiry = time.Time{}
			s.records[event.StreamKey{Tenant: event.DefaultTenant, Loop: event.LoopID(loopID)}] = rec
			s.mu.Unlock()

			// Instance B is COLD: a separate runtime over the same durable store, so its
			// local cache is empty and Resume must rehydrate. Arm the failure first, and
			// capture only from here so the seed launch above is not what gets asserted.
			rtB := New(leakDeps(s, nil))
			tc.arm(s)
			session := captureSessions(t)

			if _, err := rtB.Resume(context.Background(), event.DefaultTenant, loopID); err == nil {
				t.Fatalf("Resume must return the %s error", tc.name)
			}

			assertCanceled(t, session(t), tc.path)
		})
	}
}

// TestStartLoop_OneShotFailureLeavesCallerCtxAlive is the over-cancellation guard, and
// the reason failLaunch is not simply folded into end(). A ONE-SHOT loop derives no
// session context (sessionCtx == loopCtx, sessionCancel nil), so a failed launch must
// cancel NOTHING the caller still owns — the opposite defect from the leak, and just as
// real: a shared caller context killed by one failed launch would take unrelated work
// with it.
func TestStartLoop_OneShotFailureLeavesCallerCtxAlive(t *testing.T) {
	s := newSessionCapturingStore()

	// A raw capture: this launch is deliberately NOT interactive, so the
	// interactive-only guard in captureSessions does not apply here.
	var mu sync.Mutex
	var derived context.Context
	var derivedInteractive bool
	onSessionContext = func(ctx context.Context, interactive bool) {
		mu.Lock()
		defer mu.Unlock()
		derived, derivedInteractive = ctx, interactive
	}
	t.Cleanup(func() { onSessionContext = nil })

	rt := New(leakDeps(s, errors.New("build boom")))

	callerCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if _, err := rt.StartLoop(callerCtx, LoopConfig{
		Task: "hi", ModelID: "cloud/x", // Interactive: false — one-shot
	}); err == nil {
		t.Fatal("StartLoop must return the BuildAgent error")
	}

	mu.Lock()
	sess, wasInteractive := derived, derivedInteractive
	mu.Unlock()
	if sess == nil {
		t.Fatal("no context was ever derived — the test would pass vacuously")
	}
	if wasInteractive {
		t.Fatal("this launch must be one-shot for the guard to mean anything")
	}

	// The derived ctx IS the caller's for a one-shot; it must still be live.
	select {
	case <-sess.Done():
		t.Fatal("one-shot failure canceled the CALLER's context — failLaunch must only release a session context it created")
	default:
	}
	if err := callerCtx.Err(); err != nil {
		t.Fatalf("caller ctx must remain live after a one-shot launch failure, got %v", err)
	}
}

// TestInteractiveHappyPathSessionOutlivesLaunch is the counterweight to every test
// above: failLaunch must not fire on the SUCCESS path. A launched interactive loop's
// session context stays live after StartLoop returns — that is the whole point of the
// detachment, and a failLaunch wired into end() (which the success path also calls)
// would silently kill every interactive session at launch.
func TestInteractiveHappyPathSessionOutlivesLaunch(t *testing.T) {
	s := newSessionCapturingStore()
	session := captureSessions(t)

	rt := New(leakDeps(s, nil))
	if _, err := rt.StartLoop(context.Background(), LoopConfig{
		Task: "hi", ModelID: "cloud/x", Interactive: true,
	}); err != nil {
		t.Fatalf("StartLoop: %v", err)
	}

	select {
	case <-session(t).Done():
		t.Fatal("session context was canceled on the SUCCESS path — a launched interactive loop must stay live")
	case <-time.After(100 * time.Millisecond):
	}
}
