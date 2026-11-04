// Package fssink is the filesystem segment sink for the pre-compaction
// transcript archive: artsink over an artifact/fsstore, so the local shell and
// every other runtime share one archive code path. Like artsink it lives outside
// internal/segment so that package (the schema, reader and Source, imported by
// internal/tools for the segment_read tool) stays free of the internal/agent
// dependency a sink needs; otherwise tools -> segment -> agent -> tools would be
// an import cycle.
package fssink

import (
	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/artifact"
	artfs "github.com/ethanhinson/fuse/internal/artifact/fsstore"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/segment/artsink"
)

// New returns a sink archiving loopID's segments, under the default tenant, in
// a filesystem artifact store rooted at artifactsDir
// (<artifactsDir>/_default/<loopID>/segment/). Archive returns the segment's
// art:// ref as the recovery pointer. An empty loopID yields a nil sink, which
// the agent treats as its no-op.
func New(artifactsDir, loopID string) agent.SegmentSink {
	sink, _ := Open(artifactsDir, loopID)
	return sink
}

// Open is New plus the artifact binding the sink archives into, for a caller
// (the local shell) that must also bind its tool registry — spilled tool
// output, segment_read — and its segment reader to that same binding.
func Open(artifactsDir, loopID string) (agent.SegmentSink, artifact.Binding) {
	b := artifact.Binding{
		Store:  artfs.New(artifactsDir),
		Tenant: event.DefaultTenant,
		Loop:   event.LoopID(loopID),
	}
	return artsink.New(b), b
}
