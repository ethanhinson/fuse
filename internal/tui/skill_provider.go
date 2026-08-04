package tui

import (
	"log"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/ethanhinson/fuse/internal/skills"
)

const (
	missingDirRetryInterval = 30 * time.Second
)

// SkillProvider wraps skills.Set and watches skill dirs for live reloading.
type SkillProvider struct {
	dirs    []string
	mu      sync.RWMutex
	entries []SlashEntry
	ch      chan struct{}
	watcher *fsnotify.Watcher
	done    chan struct{}
	watched map[string]bool
}

// NewSkillProvider loads initial skills and starts fsnotify watches on dirs.
// Missing directories are skipped initially and retried every 30 s.
func NewSkillProvider(dirs []string) (*SkillProvider, error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	p := &SkillProvider{
		dirs:    dirs,
		ch:      make(chan struct{}, 1),
		watcher: w,
		done:    make(chan struct{}),
		watched: map[string]bool{},
	}
	p.reload()
	p.addWatches()
	go p.watch()
	return p, nil
}

// addWatches attempts to add fsnotify watches for any dirs not yet watched.
func (p *SkillProvider) addWatches() {
	for _, dir := range p.dirs {
		if p.watched[dir] {
			continue
		}
		if err := p.watcher.Add(dir); err == nil {
			p.watched[dir] = true
		}
	}
}

// reload re-runs skills.Load and updates entries under the write lock.
func (p *SkillProvider) reload() {
	set, err := skills.Load(p.dirs)
	if err != nil {
		log.Printf("[skill_provider] reload error: %v", err)
		return
	}
	entries := make([]SlashEntry, 0, len(set.All()))
	for _, sk := range set.All() {
		sk := sk // capture
		cmd := sk.SlashCommand
		if cmd == "" {
			cmd = "/" + sk.Name
		}
		body := sk.Body
		entries = append(entries, SlashEntry{
			Command:     cmd,
			Description: sk.Description,
			Kind:        KindSkill,
			expand:      func() string { return body },
		})
	}
	p.mu.Lock()
	p.entries = entries
	p.mu.Unlock()
}

// signal sends on ch without blocking.
func (p *SkillProvider) signal() {
	select {
	case p.ch <- struct{}{}:
	default:
	}
}

func (p *SkillProvider) watch() {
	retry := time.NewTicker(missingDirRetryInterval)
	defer retry.Stop()

	for {
		select {
		case ev, ok := <-p.watcher.Events:
			if !ok {
				return
			}
			_ = ev
			p.reload()
			p.signal()
		case err, ok := <-p.watcher.Errors:
			if !ok {
				return
			}
			log.Printf("[skill_provider] watcher error: %v", err)
		case <-retry.C:
			p.addWatches()
		case <-p.done:
			return
		}
	}
}

func (p *SkillProvider) Commands() []SlashEntry {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]SlashEntry, len(p.entries))
	copy(out, p.entries)
	return out
}

func (p *SkillProvider) Changes() <-chan struct{} { return p.ch }

func (p *SkillProvider) Close() {
	close(p.done)
	p.watcher.Close()
}
