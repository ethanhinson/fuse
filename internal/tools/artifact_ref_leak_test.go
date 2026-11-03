package tools

import (
	"context"
	"io"
	"io/fs"
	"regexp"
	"strings"
	"syscall"
	"testing"

	"github.com/ethanhinson/fuse/internal/artifact"
	"github.com/ethanhinson/fuse/internal/event"
)

// hostPathLeak is a deliberately path-bearing error, as a filesystem store
// would return for EACCES/EIO on its tenant tree.
const hostPathLeak = "/home/operator/.fuse/artifacts/acme/l1/spill/x1"

// leakyStore fails every read with a raw *fs.PathError naming a host path, so
// the tool layer is checked independently of the store's own scrubbing.
type leakyStore struct{}

func (leakyStore) Put(context.Context, artifact.Ref, io.Reader, artifact.Meta) (artifact.Meta, error) {
	return artifact.Meta{}, &fs.PathError{Op: "mkdir", Path: hostPathLeak, Err: syscall.EACCES}
}

func (leakyStore) Open(context.Context, artifact.Ref) (io.ReadCloser, artifact.Meta, error) {
	return nil, artifact.Meta{}, &fs.PathError{Op: "open", Path: hostPathLeak, Err: syscall.EIO}
}

func (leakyStore) List(context.Context, event.TenantID, event.LoopID, artifact.Kind) ([]artifact.Meta, error) {
	return nil, &fs.PathError{Op: "open", Path: hostPathLeak, Err: syscall.EACCES}
}

// rootedHostPath matches an absolute path; an "art://" ref's slashes follow a
// ':' and are not rooted.
var rootedHostPath = regexp.MustCompile(`(^|[\s"'(=])/[^/\s]`)

// TestArtifactToolsDoNotLeakStoreErrors: a store I/O failure reaches the
// durable tool result only as a fixed message naming the ref, never the
// underlying error text (which may name the host filesystem).
func TestArtifactToolsDoNotLeakStoreErrors(t *testing.T) {
	b := artifact.Binding{Store: leakyStore{}, Tenant: "acme", Loop: "l1"}
	ctx := artifact.WithBinding(t.Context(), b)
	ref := "art://acme/l1/spill/x1"

	cases := map[string]Result{
		"read_file":      NewReadFile().Execute(ctx, toolArgs(t, readArgs{Path: ref})),
		"grep":           NewGrep().Execute(ctx, toolArgs(t, map[string]any{"pattern": "x", "path": ref})),
		"list_directory": NewListDirectory().Execute(ctx, toolArgs(t, listArgs{Path: "art://acme/l1"})),
		"segment_read":   NewSegmentRead().Execute(ctx, toolArgs(t, segmentReadArgs{TurnRange: "1"})),
	}
	for name, res := range cases {
		if !res.IsError {
			t.Errorf("%s: want an error result, got %q", name, res.Output)
		}
		if strings.Contains(res.Output, ".fuse") || strings.Contains(res.Output, "operator") ||
			rootedHostPath.MatchString(res.Output) {
			t.Errorf("%s: tool output names a host path: %q", name, res.Output)
		}
		if strings.Contains(res.Output, "input/output error") || strings.Contains(res.Output, "permission denied") {
			t.Errorf("%s: tool output interpolates the store error: %q", name, res.Output)
		}
	}
	if got := cases["read_file"].Output; got != "artifact read failed: "+ref {
		t.Errorf("read_file output = %q, want the fixed ref-only message", got)
	}
}
