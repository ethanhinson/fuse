package tui

import (
	"strings"
	"sync"
	"time"
)

const (
	skillDebounce = 50 * time.Millisecond
)

// SlashRegistry aggregates CommandProviders and exposes a unified entry list
// plus a change notification channel.
type SlashRegistry struct {
	providers []CommandProvider
	mu        sync.RWMutex
	cached    []SlashEntry
	// cachedMaxWidth is the widest commandWidth across cached, memoized
	// because slashCompleter.View asks for it on every keystroke while the
	// overlay is open and an MCP server can push hundreds of entries. It is
	// written only in the same critical section that replaces cached, so it
	// can never be observed stale against the list it describes.
	cachedMaxWidth int
	reload         chan struct{}
	done           chan struct{}
}

// NewSlashRegistry starts the fan-out goroutine and returns a ready registry.
func NewSlashRegistry(providers ...CommandProvider) *SlashRegistry {
	r := &SlashRegistry{
		providers: providers,
		reload:    make(chan struct{}, 1),
		done:      make(chan struct{}),
	}
	r.snapshot()
	go r.fanout()
	return r
}

// fanout selects over every non-nil provider.Changes() channel; on any signal,
// applies a 50 ms debounce window then sends once on r.reload.
func (r *SlashRegistry) fanout() {
	// Build a merged set of cases dynamically via reflect would be messy;
	// instead spin one goroutine per provider that writes to a shared internal channel.
	dirty := make(chan struct{}, len(r.providers)+1)
	for _, p := range r.providers {
		ch := p.Changes()
		if ch == nil {
			continue
		}
		go func(ch <-chan struct{}) {
			for {
				select {
				case <-ch:
					dirty <- struct{}{}
				case <-r.done:
					return
				}
			}
		}(ch)
	}

	timer := time.NewTimer(0)
	timer.Stop()
	pending := false

	for {
		select {
		case <-dirty:
			if !pending {
				timer.Reset(skillDebounce)
				pending = true
			}
		case <-timer.C:
			pending = false
			r.snapshot()
			select {
			case r.reload <- struct{}{}:
			default:
			}
		case <-r.done:
			timer.Stop()
			return
		}
	}
}

// snapshot rebuilds the cached entry list from all providers, along with the
// memoized max command width. Both are published under one Lock: the width is
// derived from this exact list, so splitting them would let a reader see a
// width belonging to a different generation of entries.
func (r *SlashRegistry) snapshot() {
	var all []SlashEntry
	for _, p := range r.providers {
		all = append(all, p.Commands()...)
	}
	// Measured outside the lock — commandWidth is pure and takes no locks, but
	// there is no reason to hold the write lock across the whole scan.
	maxWidth := 0
	for _, e := range all {
		if w := commandWidth(e); w > maxWidth {
			maxWidth = w
		}
	}
	r.mu.Lock()
	r.cached = all
	r.cachedMaxWidth = maxWidth
	r.mu.Unlock()
}

// All returns the current union of all providers' entries.
func (r *SlashRegistry) All() []SlashEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]SlashEntry, len(r.cached))
	copy(out, r.cached)
	return out
}

// MaxCommandWidth returns the display-cell width of the widest command portion
// (Command plus " "+Syntax when present) across every entry. 0 when empty.
// Reads the value memoized by snapshot rather than rescanning: the completer
// calls this on every keystroke, and All() would copy the whole entry list.
func (r *SlashRegistry) MaxCommandWidth() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cachedMaxWidth
}

// Filter returns entries whose Command or Description contains f (case-insensitive).
// Empty f returns All().
func (r *SlashRegistry) Filter(f string) []SlashEntry {
	all := r.All()
	if f == "" {
		return all
	}
	lower := strings.ToLower(f)
	var out []SlashEntry
	for _, e := range all {
		if strings.Contains(strings.ToLower(e.Command), lower) ||
			strings.Contains(strings.ToLower(e.Description), lower) {
			out = append(out, e)
		}
	}
	return out
}

// Changes returns the unified reload channel.
func (r *SlashRegistry) Changes() <-chan struct{} { return r.reload }

// Close stops the fan-out goroutine and closes all providers.
func (r *SlashRegistry) Close() {
	close(r.done)
	for _, p := range r.providers {
		p.Close()
	}
}
