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
	loopv1 "github.com/ethanhinson/fuse/internal/loopwire/v1"
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
