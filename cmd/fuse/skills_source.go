package main

// skills_source.go is the composition-root skill-source seam (change 0134).
// Installed skills come from a skills.Source — the filesystem adapter by
// default (config skills.dirs, falling back to skills.DefaultDirs()), or the
// Postgres adapter under the pgstore build when skills.source: postgres. The
// server bindings hold the loaded set in an atomicSkills and refresh it on a
// ticker so the hosted set follows the source; a failed refresh keeps the last
// good set and logs once per failure streak.

import (
	"context"
	"fmt"
	"log"
	"sync/atomic"
	"time"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/skills"
)

// skillSourceFromConfig resolves cfg.Skills.Source ("fs" or "postgres") into
// the concrete skills.Source the bindings load from. The postgres adapter is
// only compiled in under the pgstore build tag; an untagged binary that asks
// for it gets a clear error. Composition with embedded skills (ComposeEmbedded)
// is applied by startSkillsRefresh / ComposeSkills, not by this function.
func skillSourceFromConfig(cfg config.Config) (skills.Source, error) {
	switch cfg.Skills.Source {
	case "", "fs":
		return skills.FSSource{Dirs: skillDirsFromConfig(cfg)}, nil
	case "postgres":
		return newPgSkillSource(cfg)
	default:
		return nil, fmt.Errorf("skills.source %q is not a source (fs|postgres)", cfg.Skills.Source)
	}
}

// skillDirsFromConfig returns the configured skills.dirs, or the default
// directories when unset (change 0134: skills.DefaultDirs() is only a default).
func skillDirsFromConfig(cfg config.Config) []string {
	if len(cfg.Skills.Dirs) > 0 {
		return cfg.Skills.Dirs
	}
	return skills.DefaultDirs()
}

// atomicSkills is the server bindings' live skill set. It is safe for
// concurrent Load (a loop start, a capability query, a resume) and refresh
// (the ticker goroutine), and it carries the digest of the current set so the
// harness stream and resume drift share one authority. The set and its digest
// travel in ONE atomically-swapped snapshot: a reader can never observe a set
// paired with a stale digest from an earlier refresh (or a nil set with a
// non-empty digest).
type atomicSkills struct {
	cur atomic.Pointer[atomicSkillsSnapshot]
}

// atomicSkillsSnapshot is the immutable (set, digest) pair atomicSkills holds.
type atomicSkillsSnapshot struct {
	set    *skills.Set
	digest string
}

// Load returns the current set and its digest. An empty holder (never
// Snapshotted) returns nil and "".
func (a *atomicSkills) Load() (*skills.Set, string) {
	if s := a.cur.Load(); s != nil {
		return s.set, s.digest
	}
	return nil, ""
}

// Snapshot replaces the held set and digest together.
func (a *atomicSkills) Snapshot(set *skills.Set, digest string) {
	a.cur.Store(&atomicSkillsSnapshot{set: set, digest: digest})
}

// startSkillsRefresh loads the source immediately (failing the caller when the
// initial load fails — a server never starts advertising a guessed set), then
// refreshes on a ticker. It returns the live atomicSkills and a cancel func
// that stops the refresh loop. A refresh that fails keeps the last good set.
func startSkillsRefresh(ctx context.Context, src skills.Source, refresh time.Duration, onRefresh func()) (*atomicSkills, func(), error) {
	set, digest, err := ComposeSkills(src)
	if err != nil {
		return nil, nil, err
	}
	out := &atomicSkills{}
	out.Snapshot(set, digest)
	if onRefresh != nil {
		onRefresh()
	}
	if refresh <= 0 {
		refresh = config.DefaultSkillsRefresh
	}
	stop := make(chan struct{})
	go func() {
		t := time.NewTicker(refresh)
		defer t.Stop()
		// failed tracks the current failure streak so a long outage logs once
		// (the first miss) rather than once per tick, and the recovery logs
		// once when the streak ends.
		failed := false
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				next, nextDigest, err := ComposeSkills(src)
				if err != nil {
					if !failed {
						failed = true
						log.Printf("[skills] refresh failed, keeping last good set: %v", err)
					}
					continue
				}
				if failed {
					failed = false
					log.Printf("[skills] refresh recovered")
				}
				if nextDigest != digest {
					out.Snapshot(next, nextDigest)
					digest = nextDigest
					if onRefresh != nil {
						onRefresh()
					}
				}
			}
		}
	}()
	return out, func() { close(stop) }, nil
}

// ComposeSkills loads src and folds in the embedded skills for any name not
// already present, returning the system set and its digest. It is the skills
// package's ComposeEmbedded, re-exported for the composition root.
func ComposeSkills(src skills.Source) (*skills.Set, string, error) {
	return skills.ComposeEmbedded(context.Background(), src)
}
