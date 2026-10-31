package main

import (
	"context"
	"strings"
	"testing"

	"github.com/ethanhinson/fuse/internal/permissions"
)

// TestCLIAdapterPassesApproverProvenance asserts the subprocess env carries the
// approver's provenance label next to the relay socket, so `fuse mcp-server`
// labels its socket client the same way as the parent's approver.
func TestCLIAdapterPassesApproverProvenance(t *testing.T) {
	noop := func(context.Context, permissions.ApprovalRequest) (bool, bool, error) { return true, false, nil }
	cases := []struct {
		name string
		appr permissions.Approver
		want string
	}{
		{"policy", permissions.Policy(noop), "FUSE_HITL_PROVENANCE=policy"},
		{"human", permissions.Human(noop), "FUSE_HITL_PROVENANCE=human"},
		{"always approve", permissions.AlwaysApprove, "FUSE_HITL_PROVENANCE=policy"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newCLIAdapter("fuse", tc.appr).subprocessEnv("/tmp/x.sock")
			var gotSock, gotProv bool
			for _, e := range env {
				if e == "FUSE_HITL_SOCKET=/tmp/x.sock" {
					gotSock = true
				}
				if e == tc.want {
					gotProv = true
				}
				if strings.HasPrefix(e, "FUSE_HITL_PROVENANCE=") && e != tc.want {
					t.Fatalf("unexpected %q, want %q", e, tc.want)
				}
			}
			if !gotSock || !gotProv {
				t.Fatalf("env missing socket/provenance (sock=%v prov=%v)", gotSock, gotProv)
			}
		})
	}
}
