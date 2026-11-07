package main

// loop_skills_drift_test.go is change 0134's acceptance-criterion-4 test: a
// loop started with an installed-skills digest on one host, resumed on a
// replica whose installed set differs, records a non-fatal skills_drift
// warning in the stream and still resolves (the existing RuleSkillRef refusal
// still applies only when a referenced skill is actually missing).

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/harness"
	harnessv1 "github.com/ethanhinson/fuse/harness/v1"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/event/fsstore"
	"github.com/ethanhinson/fuse/internal/runtime"
	"github.com/ethanhinson/fuse/internal/skills"
)

// hostWithSkills re-points a production loopServerHost's live skills at the
// given set, recomputing the digest. It is how a test forces one replica's
// installed-skills view independently of HOME.
func hostWithSkills(host *loopServerHost, installed []skills.Skill) *loopServerHost {
	live := &atomicSkills{}
	set := skills.NewSet(installed)
	live.Snapshot(set, skills.DigestOf(set))
	host.skills = live
	return host
}

// resilientDeps is a production deps pair sharing one durable store, with the
// host forced to `installed`. It wires CurrentSkillsDigest to the host's own
// method (it bypasses assembleLoopServerRuntimeDeps by re-pointing the host's
// live set after assembly), so the drift path runs through the real seam.
func resilientDeps(t *testing.T, operatorYAML string, gw *recordingGateway, store *fsstore.FSDurableStore, installed []skills.Skill) (runtime.Deps, *loopServerHost) {
	t.Helper()
	deps, host := harnessServerWith(t, operatorYAML, gw)
	deps.DurableStore, deps.Registry = store, store
	deps.CurrentSkillsDigest = host.CurrentSkillsDigest
	if installed != nil {
		hostWithSkills(host, installed)
	}
	return deps, host
}

// TestResumeOnDriftedSkillsRecordsSkillsDriftAndStillResolves proves a resume
// on a replica whose installed skill set differs from the one that started the
// loop records a skills_drift warning and still succeeds (the harness's skills
// resolve because they exist on the replica).
func TestResumeOnDriftedSkillsRecordsSkillsDriftAndStillResolves(t *testing.T) {
	gw := &recordingGateway{}
	store := fsstore.NewDurableFSStore(t.TempDir())

	// Host A's installed set: notes + review.
	startSkills := []skills.Skill{
		skills.NewCarried("notes", "take notes", "Take notes."),
		skills.NewCarried("review", "review work", "Review work."),
	}
	depsA, hostA := resilientDeps(t, harnessOperator, gw, store, startSkills)
	startDigest := hostA.CurrentSkillsDigest()
	if startDigest == "" {
		t.Fatal("host A must track a digest")
	}
	rtA := runtime.New(depsA)

	turns := uint32(1)
	spec := &harnessv1.HarnessSpec{MaxTurns: &turns, Skills: []*harnessv1.SkillRef{{Name: "notes"}}}
	h, err := rtA.StartLoop(context.Background(), runtime.LoopConfig{
		Task:    "go",
		Harness: spec,
	})
	if err != nil {
		t.Fatalf("StartLoop: %v", err)
	}
	_, _ = h.Wait() // the turn cap ends the run

	// Host B's installed set drifts: review is gone, a new skill appears.
	// "notes" — the skill the harness references — still exists.
	drifted := []skills.Skill{
		skills.NewCarried("notes", "take notes", "Take notes."),
		skills.NewCarried("brand-new", "brand new", "New."),
	}
	depsB, hostB := resilientDeps(t, harnessOperator, gw, store, drifted)
	if hostB.CurrentSkillsDigest() == startDigest {
		t.Fatal("test setup: replica digest must differ")
	}
	rtB := runtime.New(depsB)

	// Resume: resolves (notes exists) and records skills_drift.
	_, err = rtB.Resume(context.Background(), "", h.ID())
	if err != nil {
		t.Fatalf("Resume on drifted replica failed (should still resolve): %v", err)
	}

	hist, err := rtB.Attach(context.Background(), "", h.ID(), 0)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	found := false
	for _, e := range hist {
		if e.Kind == event.KindError && strings.Contains(string(e.Payload), "skills_drift") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no skills_drift event in stream (kinds: %v)", eventKinds(hist))
	}
}

// TestResumeOnDriftedMissingSkillStillRefuses proves drift does not bypass the
// RuleSkillRef refusal: resuming a loop whose spec references a skill the
// replica lacks still fails validation even when digests differ.
func TestResumeOnDriftedMissingSkillStillRefuses(t *testing.T) {
	gw := &recordingGateway{}
	store := fsstore.NewDurableFSStore(t.TempDir())

	start := []skills.Skill{skills.NewCarried("notes", "take notes", "Take notes.")}
	depsA, _ := resilientDeps(t, harnessOperator, gw, store, start)
	rtA := runtime.New(depsA)

	turns := uint32(1)
	spec := &harnessv1.HarnessSpec{MaxTurns: &turns, Skills: []*harnessv1.SkillRef{{Name: "notes"}}}
	h, err := rtA.StartLoop(context.Background(), runtime.LoopConfig{
		Task:    "go",
		Harness: spec,
	})
	if err != nil {
		t.Fatalf("StartLoop: %v", err)
	}
	_, _ = h.Wait()

	// The replica lacks "notes" entirely (drift + missing).
	lacking := []skills.Skill{skills.NewCarried("other", "other", "Other.")}
	depsB, _ := resilientDeps(t, harnessOperator, gw, store, lacking)
	rtB := runtime.New(depsB)

	_, err = rtB.Resume(context.Background(), "", h.ID())
	var ve *harness.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("Resume err = %v, want a RuleSkillRef ValidationError", err)
	}
	for _, v := range ve.Violations {
		if v.FieldPath == "skills[0].name" && v.RuleID == harness.RuleSkillRef {
			return
		}
	}
	t.Fatalf("violations = %+v, want skills[0].name RuleSkillRef", ve.Violations)
}

// TestLoopHarnessRecordsLiveSkillsDigest proves the digest on the loop.harness
// record comes from the runtime's Deps.CurrentSkillsDigest (taken at loop
// start) rather than a caller-supplied field: a loop started with no digest
// passed in still records a non-empty one equal to the host's.
func TestLoopHarnessRecordsLiveSkillsDigest(t *testing.T) {
	gw := &recordingGateway{}
	store := fsstore.NewDurableFSStore(t.TempDir())

	installed := []skills.Skill{
		skills.NewCarried("notes", "take notes", "Take notes."),
	}
	deps, host := resilientDeps(t, harnessOperator, gw, store, installed)
	rt := runtime.New(deps)

	turns := uint32(1)
	spec := &harnessv1.HarnessSpec{MaxTurns: &turns, Skills: []*harnessv1.SkillRef{{Name: "notes"}}}
	h, err := rt.StartLoop(context.Background(), runtime.LoopConfig{
		Task:    "go",
		Harness: spec,
	})
	if err != nil {
		t.Fatalf("StartLoop: %v", err)
	}
	_, _ = h.Wait()

	hist, err := rt.Attach(context.Background(), "", h.ID(), 0)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	var found string
	for _, e := range hist {
		if e.Kind != event.KindLoopHarness {
			continue
		}
		var p event.LoopHarnessPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("decode loop.harness: %v", err)
		}
		found = p.InstalledSkillsDigest
	}
	if found == "" {
		t.Fatalf("loop.harness has no installed-skills digest (kinds: %v)", eventKinds(hist))
	}
	if want := host.CurrentSkillsDigest(); found != want {
		t.Fatalf("loop.harness digest = %q, want the host's %q", found, want)
	}
}

// waitParked waits for the loop to end (up to 10s) so a resume can rehydrate.
func waitParked(t *testing.T, h runtime.LoopHandle) {
	t.Helper()
	done := make(chan struct{})
	go func() { _, _ = h.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("loop did not end")
	}
}

func eventKinds(evs []event.Event) []event.Kind {
	out := make([]event.Kind, len(evs))
	for i, e := range evs {
		out[i] = e.Kind
	}
	return out
}
