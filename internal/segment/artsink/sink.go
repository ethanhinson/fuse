// Package artsink is the artifact-store-backed agent.SegmentSink. It lives
// outside internal/segment so that package (the schema, reader and Source,
// imported by internal/tools) stays free of the internal/agent dependency a
// sink needs; otherwise tools -> segment -> agent -> tools would cycle.
package artsink

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/artifact"
	"github.com/ethanhinson/fuse/internal/segment"
)

// archiveTimeout bounds one Archive. SegmentSink.Archive takes no context and
// archiving is best-effort, so the sink supplies its own deadline.
const archiveTimeout = 10 * time.Second

// Sink archives each compacted region as a KindSegment artifact and appends an
// entry to the loop's KindSegmentIndex artifact. The index is always re-read
// from the store before appending, so sinks in different runtimes serving the
// same loop extend one index rather than overwriting each other.
type Sink struct {
	b  artifact.Binding
	mu sync.Mutex
}

// New returns a sink over b, or a nil interface when b is not valid, so callers
// pass it straight to the agent and get its no-op default.
func New(b artifact.Binding) agent.SegmentSink {
	if !b.Valid() {
		return nil
	}
	return &Sink{b: b}
}

// Archive persists r and returns the segment's art:// ref as the recovery pointer.
func (s *Sink) Archive(r agent.SegmentRegion) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), archiveTimeout)
	defer cancel()

	ts := time.Now().UTC()
	seg := segment.Segment{
		TurnStart:    r.TurnStart,
		TurnEnd:      r.TurnEnd,
		Tools:        r.ToolNames,
		TokensBefore: r.TokensBefore,
		TokensAfter:  r.TokensAfter,
		TS:           ts,
		Summary:      r.Summary,
		Messages:     r.Messages,
	}
	// Plaintext here: the store compresses at rest.
	body := segment.RenderSegment(seg)
	meta, err := s.b.Put(ctx, artifact.KindSegment, strings.NewReader(body), artifact.Meta{
		Turn:   r.TurnStart,
		Labels: map[string]string{"turns": strconv.Itoa(r.TurnStart) + "-" + strconv.Itoa(r.TurnEnd)},
	})
	if err != nil {
		return "", fmt.Errorf("segment put: %w", err)
	}

	// Store state, never memory: a loop that moved runtimes appends correctly.
	idx, err := segment.NewArtifactSource(s.b).ReadIndex(ctx)
	if err != nil {
		return "", fmt.Errorf("segment index read: %w", err)
	}
	idx.SessionID = string(s.b.Loop)
	idx.Segments = append(idx.Segments, segment.IndexEntry{
		TurnStart:    r.TurnStart,
		TurnEnd:      r.TurnEnd,
		Tools:        r.ToolNames,
		TokensBefore: r.TokensBefore,
		TokensAfter:  r.TokensAfter,
		Path:         meta.Ref.ID,
		TS:           ts,
	})
	out, err := json.Marshal(idx)
	if err != nil {
		return "", fmt.Errorf("segment index encode: %w", err)
	}
	if _, err := s.b.PutID(ctx, artifact.KindSegmentIndex, segment.IndexArtifactID, bytes.NewReader(out), artifact.Meta{}); err != nil {
		return "", fmt.Errorf("segment index write: %w", err)
	}
	return meta.Ref.String(), nil
}
