package loopserver

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/ethanhinson/fuse/harness"
	harnessv1 "github.com/ethanhinson/fuse/harness/v1"
)

func TestLoopStartThreadsTheHarnessSpec(t *testing.T) {
	fr := &fakeRuntime{startID: "loop-h"}
	s := NewServer(nil, nil, fr)
	params := `{"task":"hi","harness":{"maxTurns":1,"skills":[{"name":"research"}],"models":{"default_alias":"glm"}}}`
	got := s.dispatch(context.Background(), req{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "loop.start", Params: json.RawMessage(params)})
	if got.Error != nil {
		t.Fatalf("error: %+v", got.Error)
	}
	turns := uint32(1)
	want := &harnessv1.HarnessSpec{MaxTurns: &turns, Skills: []*harnessv1.SkillRef{{Name: "research"}}, Models: &harnessv1.Models{DefaultAlias: "glm"}}
	if !proto.Equal(fr.startCfg.Harness, want) {
		t.Fatalf("Harness = %v, want %v", fr.startCfg.Harness, want)
	}
}

func TestLoopStartMalformedHarnessIsInvalidParams(t *testing.T) {
	fr := &fakeRuntime{startID: "loop-h"}
	s := NewServer(nil, nil, fr)
	got := s.dispatch(context.Background(), req{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "loop.start", Params: json.RawMessage(`{"task":"hi","harness":{"nope":1}}`)})
	if got.Error == nil || got.Error.Code != codeInvalidParams {
		t.Fatalf("got %+v, want invalid params", got.Error)
	}
	if fr.startCfg.Task != "" {
		t.Fatal("a malformed spec still started a loop")
	}
}

func TestLoopStartInvalidHarnessCarriesViolations(t *testing.T) {
	ve := &harness.ValidationError{Violations: []harness.Violation{{FieldPath: "skills[0].name", Message: "not installed", RuleID: harness.RuleSkillRef}}}
	fr := &fakeRuntime{startErr: fmt.Errorf("runtime: harness: %w", ve)}
	s := NewServer(nil, nil, fr)
	got := s.dispatch(context.Background(), req{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "loop.start", Params: json.RawMessage(`{"task":"hi","harness":{}}`)})
	if got.Error == nil || got.Error.Code != codeInvalidParams {
		t.Fatalf("got %+v, want invalid params", got.Error)
	}
	raw, err := json.Marshal(got.Error)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"code":-32602,"message":"harness spec: 1 violation: skills[0].name: not installed (fuse.skill_ref)","data":{"violations":[{"field_path":"skills[0].name","message":"not installed","rule_id":"fuse.skill_ref"}]}}`
	if string(raw) != want {
		t.Fatalf("error frame =\n %s\nwant\n %s", raw, want)
	}
}
