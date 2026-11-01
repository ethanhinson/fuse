package loopconnect

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	"github.com/ethanhinson/fuse/harness"
	harnessv1 "github.com/ethanhinson/fuse/harness/v1"
	"github.com/ethanhinson/fuse/internal/loopauth"
	loopv1 "github.com/ethanhinson/fuse/internal/loopwire/v1"
	"github.com/ethanhinson/fuse/internal/loopwire/v1/loopv1connect"
	"github.com/ethanhinson/fuse/internal/runtime"
)

func TestStartLoopThreadsTheHarnessSpec(t *testing.T) {
	rt := &fakeRuntime{startID: "loop-h"}
	client := newTestServer(t, rt)
	turns := uint32(1)
	spec := &harnessv1.HarnessSpec{MaxTurns: &turns, Skills: []*harnessv1.SkillRef{{Name: "research"}}}
	resp, err := client.StartLoop(context.Background(), connect.NewRequest(&loopv1.StartLoopRequest{Task: "t", Harness: spec}))
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(rt.sawStartCfg.Harness, spec) {
		t.Fatalf("Harness = %v, want %v", rt.sawStartCfg.Harness, spec)
	}
	if resp.Msg.HarnessDigest != harness.Digest(spec) {
		t.Fatalf("harness_digest = %q, want %q", resp.Msg.HarnessDigest, harness.Digest(spec))
	}
	resp, err = client.StartLoop(context.Background(), connect.NewRequest(&loopv1.StartLoopRequest{Task: "t"}))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Msg.HarnessDigest != "" {
		t.Fatalf("no-spec StartLoop answered harness_digest %q", resp.Msg.HarnessDigest)
	}
	if rt.sawStartCfg.Harness != nil {
		t.Fatalf("an absent spec reached the seam as %v", rt.sawStartCfg.Harness)
	}
}

func TestStartLoopInvalidHarnessIsInvalidArgumentWithViolations(t *testing.T) {
	ve := &harness.ValidationError{Violations: []harness.Violation{
		{FieldPath: "skills[0].name", Message: `skill "x" is not installed on this server`, RuleID: harness.RuleSkillRef},
		{FieldPath: "throughput.requests_per_minute", Message: "may only tighten this server's limit of 100", RuleID: harness.RuleTightenOnly},
	}}
	rt := &fakeRuntime{startErr: fmt.Errorf("runtime: harness: %w", ve)}
	client := newTestServer(t, rt)
	_, err := client.StartLoop(context.Background(), connect.NewRequest(&loopv1.StartLoopRequest{Task: "t", Harness: &harnessv1.HarnessSpec{}}))
	var cerr *connect.Error
	if !errors.As(err, &cerr) || cerr.Code() != connect.CodeInvalidArgument {
		t.Fatalf("err = %v, want InvalidArgument", err)
	}
	details := cerr.Details()
	if len(details) != 1 {
		t.Fatalf("details = %d, want 1", len(details))
	}
	msg, derr := details[0].Value()
	if derr != nil {
		t.Fatal(derr)
	}
	resp, ok := msg.(*harnessv1.ValidateHarnessResponse)
	if !ok || !proto.Equal(resp, ve.Proto()) {
		t.Fatalf("detail = %v, want %v", msg, ve.Proto())
	}
}

func TestStartLoopHarnessUnsupportedIsUnimplemented(t *testing.T) {
	rt := &fakeRuntime{startErr: runtime.ErrHarnessUnsupported}
	client := newTestServer(t, rt)
	_, err := client.StartLoop(context.Background(), connect.NewRequest(&loopv1.StartLoopRequest{Task: "t", Harness: &harnessv1.HarnessSpec{}}))
	if got := connect.CodeOf(err); got != connect.CodeUnimplemented {
		t.Fatalf("code = %v, want Unimplemented", got)
	}
}

// --- harness digest pin: a token pinned to one spec digest starts only that spec ---

// digestPinFixture stands up the real auth interceptor over a registry-backed
// handler with a pinned token (alice, pinned to harness.Digest(spec)) and an
// unpinned one, and returns the spec plus a variant that differs in one field.
type digestPinFixture struct {
	rt      *fakeRuntime
	reg     *fakeRegistry
	client  loopv1connect.LoopServiceClient
	spec    *harnessv1.HarnessSpec
	variant *harnessv1.HarnessSpec
}

func newDigestPinFixture(t *testing.T) digestPinFixture {
	t.Helper()
	one, two := uint32(1), uint32(2)
	spec := &harnessv1.HarnessSpec{MaxTurns: &one, Skills: []*harnessv1.SkillRef{{Name: "research"}}}
	variant := &harnessv1.HarnessSpec{MaxTurns: &two, Skills: []*harnessv1.SkillRef{{Name: "research"}}}
	if harness.Digest(spec) == harness.Digest(variant) {
		t.Fatal("fixture specs share a digest")
	}
	reg := newFakeRegistry()
	rt := &fakeRuntime{startID: "loop-p"}
	verifier := tokenVerifier{tokens: map[string]loopauth.Principal{
		"pinned":   {Tenant: "acme", Subject: "alice", HarnessDigest: harness.Digest(spec)},
		"unpinned": {Tenant: "acme", Subject: "alice"},
	}}
	return digestPinFixture{rt: rt, reg: reg, client: newAuthzServer(t, rt, reg, verifier), spec: spec, variant: variant}
}

func TestDigestPinMatchingSpecStarts(t *testing.T) {
	f := newDigestPinFixture(t)
	f.reg.seed("acme", "loop-p", "", "node-1")
	resp, err := f.client.StartLoop(context.Background(),
		bearer(connect.NewRequest(&loopv1.StartLoopRequest{Task: "t", Harness: f.spec}), "pinned"))
	if err != nil {
		t.Fatalf("pinned token with its own spec refused: %v", err)
	}
	if resp.Msg.HarnessDigest != harness.Digest(f.spec) {
		t.Fatalf("harness_digest = %q, want %q", resp.Msg.HarnessDigest, harness.Digest(f.spec))
	}
	if f.rt.startCalls != 1 {
		t.Fatalf("seam StartLoop calls = %d, want 1", f.rt.startCalls)
	}
}

func TestDigestPinMismatchRefused(t *testing.T) {
	f := newDigestPinFixture(t)
	_, err := f.client.StartLoop(context.Background(),
		bearer(connect.NewRequest(&loopv1.StartLoopRequest{Task: "t", Harness: f.variant}), "pinned"))
	if got := connect.CodeOf(err); got != connect.CodePermissionDenied {
		t.Fatalf("code = %v (err %v), want PermissionDenied", got, err)
	}
	if f.rt.startCalls != 0 {
		t.Fatalf("seam StartLoop called %d times for a mismatched pin", f.rt.startCalls)
	}
	recs, lerr := f.reg.List(context.Background(), "acme")
	if lerr != nil {
		t.Fatal(lerr)
	}
	if len(recs) != 0 {
		t.Fatalf("a refused StartLoop left %d registry records", len(recs))
	}
}

func TestDigestPinMissingSpecRefused(t *testing.T) {
	f := newDigestPinFixture(t)
	_, err := f.client.StartLoop(context.Background(),
		bearer(connect.NewRequest(&loopv1.StartLoopRequest{Task: "t"}), "pinned"))
	if got := connect.CodeOf(err); got != connect.CodePermissionDenied {
		t.Fatalf("code = %v (err %v), want PermissionDenied", got, err)
	}
	if f.rt.startCalls != 0 {
		t.Fatalf("seam StartLoop called %d times for a pinned token with no spec", f.rt.startCalls)
	}
}

func TestNoPinAnySpecStarts(t *testing.T) {
	f := newDigestPinFixture(t)
	f.reg.seed("acme", "loop-p", "", "node-1")
	resp, err := f.client.StartLoop(context.Background(),
		bearer(connect.NewRequest(&loopv1.StartLoopRequest{Task: "t", Harness: f.variant}), "unpinned"))
	if err != nil {
		t.Fatalf("unpinned token refused: %v", err)
	}
	if resp.Msg.HarnessDigest != harness.Digest(f.variant) || f.rt.startCalls != 1 {
		t.Fatalf("digest = %q, seam calls = %d", resp.Msg.HarnessDigest, f.rt.startCalls)
	}
}
