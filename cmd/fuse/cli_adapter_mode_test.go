package main

import (
	"reflect"
	"strings"
	"testing"
	"unsafe"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/observe"
	"github.com/ethanhinson/fuse/internal/permissions"
	"github.com/ethanhinson/fuse/internal/tools"
)

// countModeEnv returns how many FUSE_HITL_MODE entries env holds and how many
// of them are exactly FUSE_HITL_MODE=off.
func countModeEnv(env []string) (all, off int) {
	for _, e := range env {
		if strings.HasPrefix(e, "FUSE_HITL_MODE=") {
			all++
			if e == "FUSE_HITL_MODE=off" {
				off++
			}
		}
	}
	return all, off
}

func assertRelayBasics(t *testing.T, env []string, sock string) {
	t.Helper()
	var gotSock, gotProv bool
	for _, e := range env {
		if e == "FUSE_HITL_SOCKET="+sock {
			gotSock = true
		}
		if e == "FUSE_HITL_PROVENANCE=policy" {
			gotProv = true
		}
	}
	if !gotSock || !gotProv {
		t.Fatalf("env missing socket/provenance (sock=%v prov=%v)", gotSock, gotProv)
	}
}

// TestCLIAdapterPassesModeOff asserts the adapter relays mode off to the tool
// subprocess exactly once, and never passes through an inherited stale value.
func TestCLIAdapterPassesModeOff(t *testing.T) {
	const sock = "/tmp/x.sock"
	t.Run("mode off relayed", func(t *testing.T) {
		env := newCLIAdapter("fuse", permissions.AlwaysApprove, true).subprocessEnv(sock)
		all, off := countModeEnv(env)
		if all != 1 || off != 1 {
			t.Fatalf("FUSE_HITL_MODE entries = %d (off=%d), want exactly one FUSE_HITL_MODE=off", all, off)
		}
		assertRelayBasics(t, env, sock)
	})
	t.Run("inherited stale value dropped", func(t *testing.T) {
		t.Setenv("FUSE_HITL_MODE", "off")
		env := newCLIAdapter("fuse", permissions.AlwaysApprove, false).subprocessEnv(sock)
		if all, _ := countModeEnv(env); all != 0 {
			t.Fatalf("FUSE_HITL_MODE entries = %d, want none: the relayed mode is this process's decision", all)
		}
		assertRelayBasics(t, env, sock)
	})
}

// completerOf reads the agent's unexported model Completer, mirroring
// observerOf. No production accessor exists for it.
func completerOf(t *testing.T, a *agent.Agent) agent.Completer {
	t.Helper()
	f := reflect.ValueOf(a).Elem().FieldByName("model")
	if !f.IsValid() {
		t.Fatal("agent.Agent has no model field; update completerOf")
	}
	c, ok := reflect.NewAt(f.Type(), unsafe.Pointer(f.UnsafeAddr())).Elem().Interface().(agent.Completer)
	if !ok {
		t.Fatalf("agent model field is %s, not an agent.Completer", f.Type())
	}
	return c
}

// TestBuildChildAgentCLIModeFollowsGate pins that both CLIAdapter construction
// sites relay mode off from the same expression that seeds the gate: the live
// SessionMode holder when present, else the configured mode. Complete is never
// called, so no claude process is spawned.
func TestBuildChildAgentCLIModeFollowsGate(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cases := []struct {
		name    string
		mode    string
		sm      func() *permissions.SessionMode
		wantOff bool
	}{
		{"config off", "off", func() *permissions.SessionMode { return nil }, true},
		{"config smart", "smart", func() *permissions.SessionMode { return nil }, false},
		{"session off over smart", "smart", func() *permissions.SessionMode { return permissions.NewSessionMode(permissions.ModeOff) }, true},
		{"session smart over off", "off", func() *permissions.SessionMode { return permissions.NewSessionMode(permissions.ModeSmart) }, false},
	}
	check := func(t *testing.T, a *agent.Agent, wantOff bool) {
		t.Helper()
		cli, ok := completerOf(t, a).(*CLIAdapter)
		if !ok {
			t.Fatalf("completer = %T, want *CLIAdapter", completerOf(t, a))
		}
		_, off := countModeEnv(cli.subprocessEnv("/tmp/x.sock"))
		if got := off == 1; got != wantOff {
			t.Fatalf("FUSE_HITL_MODE=off relayed = %v, want %v", got, wantOff)
		}
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A stale inherited value must not leak into either answer.
			t.Setenv("FUSE_HITL_MODE", "")
			var cfg config.Config
			cfg.Permissions.Mode = tc.mode
			reg := model.NewRegistry("c", map[string]model.ModelConfig{"c": {ID: "cli/claude"}})
			t.Run("buildChildAgent", func(t *testing.T) {
				a, err := buildChildAgent(cfg, reg, "c", discardRenderer{}, "", tools.NewRegistry(), permissions.AlwaysApprove, nil, "child", tc.sm(), false, nil, nil)
				if err != nil {
					t.Fatalf("buildChildAgent: %v", err)
				}
				check(t, a, tc.wantOff)
			})
			t.Run("buildAgentCore", func(t *testing.T) {
				a, _, err := buildAgentCore(cfg, reg, "c", discardRenderer{}, "", nil, "root", tools.NewRegistry(), permissions.AlwaysApprove, tc.sm(), false, nil, nil, observe.NoopObserver{})
				if err != nil {
					t.Fatalf("buildAgentCore: %v", err)
				}
				check(t, a, tc.wantOff)
			})
		})
	}
}
