package fuse

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/ethanhinson/fuse/harness"
	harnessv1 "github.com/ethanhinson/fuse/harness/v1"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/runtime"
)

var sdkViolations = &harness.ValidationError{Violations: []harness.Violation{
	{FieldPath: "skills[0].name", Message: `skill "x" is not installed on this server`, RuleID: harness.RuleSkillRef},
	{FieldPath: "throughput.requests_per_minute", Message: "may only tighten this server's limit of 100", RuleID: harness.RuleTightenOnly},
}}

func sdkSpec() *harnessv1.HarnessSpec {
	turns := uint32(3)
	return &harnessv1.HarnessSpec{MaxTurns: &turns, Models: &harnessv1.Models{DefaultAlias: "glm"}}
}

// TestStartLoopCarriesTheHarnessSpec drives both backends: the spec reaches the
// runtime seam, and a refused spec comes back as a *harness.ValidationError with
// every violation, so a caller handles local and remote identically.
func TestStartLoopCarriesTheHarnessSpec(t *testing.T) {
	backends := map[string]func(f *fakeRuntime) *Client{
		"local": func(f *fakeRuntime) *Client {
			return NewLocal(f, Credentials{Tenant: string(event.DefaultTenant)})
		},
		"remote": func(f *fakeRuntime) *Client {
			return NewRemote(remoteFakeServer(t, f, devVerifier()), Credentials{Token: "good-tok", Tenant: string(event.DefaultTenant)})
		},
	}
	for name, mk := range backends {
		t.Run(name, func(t *testing.T) {
			f := &fakeRuntime{startID: "loop-h"}
			c := mk(f)
			if _, err := c.StartLoop(context.Background(), StartLoopConfig{Task: "t", Harness: sdkSpec()}); err != nil {
				t.Fatalf("StartLoop: %v", err)
			}
			if !proto.Equal(f.sawStartCfg.Harness, sdkSpec()) {
				t.Fatalf("Harness = %v", f.sawStartCfg.Harness)
			}

			f.startErr = fmt.Errorf("runtime: harness: %w", sdkViolations)
			_, err := c.StartLoop(context.Background(), StartLoopConfig{Task: "t", Harness: sdkSpec()})
			var ve *harness.ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("err = %v, want *harness.ValidationError", err)
			}
			if len(ve.Violations) != 2 || ve.Violations[0] != sdkViolations.Violations[0] || ve.Violations[1] != sdkViolations.Violations[1] {
				t.Fatalf("violations = %+v", ve.Violations)
			}

			f.startErr = runtime.ErrHarnessUnsupported
			if _, err := c.StartLoop(context.Background(), StartLoopConfig{Task: "t", Harness: sdkSpec()}); !errors.Is(err, ErrHarnessUnsupported) {
				t.Fatalf("err = %v, want ErrHarnessUnsupported", err)
			}
		})
	}
}
