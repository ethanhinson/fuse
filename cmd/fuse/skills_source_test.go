package main

// skills_source_test.go covers the composition-root skill-source seam (change
// 0134): the atomicSkills snapshot pair (a reader always sees a set with its
// own digest, never a set paired with a stale one) and the refresh loop's
// once-per-failure-streak logging.

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/skills"
)

// ---- atomicSkills ----

// TestAtomicSkillsEmptyLoadReturnsNilAndEmptyDigest proves a fresh holder (never
// Snapshotted) loads as (nil, "") rather than panicking on a nil interface.
func TestAtomicSkillsEmptyLoadReturnsNilAndEmptyDigest(t *testing.T) {
	var a atomicSkills
	set, digest := a.Load()
	if set != nil {
		t.Fatalf("empty Load set = %v, want nil", set)
	}
	if digest != "" {
		t.Fatalf("empty Load digest = %q, want empty", digest)
	}
}

// TestAtomicSkillsLoadAndSnapshotAreConcurrentSafe is the fix-2 race test: one
// goroutine flips between two distinct sets and their digests while the main
// goroutine repeatedly Loads and checks that the returned digest is the digest
// of the returned set. Under the old two-atomics holder this pairs a new set
// with a stale digest; the single-snapshot holder cannot. Run with -race.
func TestAtomicSkillsLoadAndSnapshotAreConcurrentSafe(t *testing.T) {
	var a atomicSkills
	aSet := skills.NewSet([]skills.Skill{skills.NewCarried("alpha", "does alpha", "Alpha body.")})
	bSet := skills.NewSet([]skills.Skill{skills.NewCarried("beta", "does beta", "Beta body.")})
	a.Snapshot(aSet, skills.DigestOf(aSet))
	a.Snapshot(bSet, skills.DigestOf(bSet))

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 20000; i++ {
			if i%2 == 0 {
				a.Snapshot(aSet, skills.DigestOf(aSet))
			} else {
				a.Snapshot(bSet, skills.DigestOf(bSet))
			}
		}
	}()
	for i := 0; i < 20000; i++ {
		set, digest := a.Load()
		if set == nil {
			t.Fatalf("Load returned a nil set after Snapshot")
		}
		if got := skills.DigestOf(set); got != digest {
			t.Fatalf("Load returned set+digest out of pair: digest=%q, DigestOf(set)=%q", digest, got)
		}
	}
	<-done
}

// ---- refresh streak logging ----

// fakeStreakSource is a skills.Source whose Load fails until succeedsAfter
// calls have elapsed, then returns the given single-skill set.
type fakeStreakSource struct {
	mu     sync.Mutex
	fail   bool
	loaded int
}

func (f *fakeStreakSource) Load(ctx context.Context) (*skills.Set, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loaded++
	if f.fail {
		return nil, "", errors.New("streak source down")
	}
	set := skills.NewSet([]skills.Skill{skills.NewCarried("only", "the only skill", "Body.")})
	return set, skills.DigestOf(set), nil
}

func (f *fakeStreakSource) setFailing(v bool) {
	f.mu.Lock()
	f.fail = v
	f.mu.Unlock()
}

func (f *fakeStreakSource) loads() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.loaded
}

// syncedBuffer is a bytes.Buffer safe for the refresh goroutine to log into
// while the test goroutine reads, so the race detector sees no unsynchronized
// pair (the log package itself does not lock the writer).
type syncedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncedBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncedBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// TestSkillsRefreshLogsOncePerFailureStreak is the fix-3 test: a source that
// fails across many 1ms ticks produces exactly one failure line for the
// streak, and the recovery after it produces one recovered line.
func TestSkillsRefreshLogsOncePerFailureStreak(t *testing.T) {
	buf := &syncedBuffer{}
	log.SetOutput(buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	src := &fakeStreakSource{} // healthy for the initial load
	_, stop, err := startSkillsRefresh(context.Background(), src, time.Millisecond, nil)
	if err != nil {
		t.Fatalf("startSkillsRefresh: %v", err)
	}
	t.Cleanup(stop)

	// Begin the failure streak: many ticks now fail.
	src.setFailing(true)
	deadline := time.Now().Add(2 * time.Second)
	for src.loads() < 12 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	// End the streak: the next successful ComposeSkills recovers and logs once.
	src.setFailing(false)
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		out := buf.String()
		if strings.Contains(out, "recovered") {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}

	out := buf.String()
	failures := strings.Count(out, "refresh failed")
	if failures != 1 {
		t.Fatalf("failure lines = %d, want exactly 1 for the whole streak; log:\n%s", failures, out)
	}
	if rec := strings.Count(out, "recovered"); rec != 1 {
		t.Fatalf("recovered lines = %d, want exactly 1; log:\n%s", rec, out)
	}
}
