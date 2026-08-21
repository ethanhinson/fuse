package tui

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
)

// fakeProvider is a CommandProvider whose entries can be updated at runtime.
type fakeProvider struct {
	mu      sync.RWMutex
	entries []SlashEntry
	ch      chan struct{}
}

func newFakeProvider(entries []SlashEntry) *fakeProvider {
	return &fakeProvider{entries: entries, ch: make(chan struct{}, 1)}
}

func (p *fakeProvider) Commands() []SlashEntry {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]SlashEntry, len(p.entries))
	copy(out, p.entries)
	return out
}
func (p *fakeProvider) Changes() <-chan struct{} { return p.ch }
func (p *fakeProvider) Close()                   {}

func (p *fakeProvider) push(entries []SlashEntry) {
	p.mu.Lock()
	p.entries = entries
	p.mu.Unlock()
	select {
	case p.ch <- struct{}{}:
	default:
	}
}

func TestSlashRegistryAll(t *testing.T) {
	p1 := &staticProvider{entries: []SlashEntry{
		{Command: "/exit", Kind: KindBuiltin},
	}}
	p2 := &staticProvider{entries: []SlashEntry{
		{Command: "/code-review", Kind: KindSkill},
	}}
	reg := NewSlashRegistry(p1, p2)
	defer reg.Close()

	all := reg.All()
	if len(all) != 2 {
		t.Fatalf("want 2 entries, got %d", len(all))
	}
}

func TestSlashRegistryMaxCommandWidth(t *testing.T) {
	// "/cc NAME" is 8 cells and must beat "/bbbb" (5) — the " "+Syntax counts.
	p := &staticProvider{entries: []SlashEntry{
		{Command: "/a", Kind: KindBuiltin},
		{Command: "/bbbb", Kind: KindBuiltin},
		{Command: "/cc", Syntax: "NAME", Kind: KindBuiltin},
	}}
	reg := NewSlashRegistry(p)
	defer reg.Close()

	if got := reg.MaxCommandWidth(); got != 8 {
		t.Errorf("MaxCommandWidth() = %d, want 8", got)
	}
}

func TestSlashRegistryMaxCommandWidthEmpty(t *testing.T) {
	reg := NewSlashRegistry(&staticProvider{})
	defer reg.Close()

	if got := reg.MaxCommandWidth(); got != 0 {
		t.Errorf("empty registry MaxCommandWidth() = %d, want 0", got)
	}
}

func TestSlashRegistryMaxCommandWidthDisplayCells(t *testing.T) {
	// "世界" is 6 bytes but 4 display cells: "/x 世界" is 7 cells, not 9.
	syntax := "世界"
	p := &staticProvider{entries: []SlashEntry{
		{Command: "/x", Syntax: syntax, Kind: KindBuiltin},
	}}
	reg := NewSlashRegistry(p)
	defer reg.Close()

	got := reg.MaxCommandWidth()
	if byteLen := len("/x " + syntax); got == byteLen {
		t.Fatalf("MaxCommandWidth() = %d, over-counted by byte length", got)
	}
	if want := lipgloss.Width("/x " + syntax); got != want {
		t.Errorf("MaxCommandWidth() = %d, want %d display cells", got, want)
	}
}

func TestSlashRegistryMaxCommandWidthSpansAllEntries(t *testing.T) {
	// The widest entry lives in the second provider and matches no common
	// filter; the result must still reflect All(), not any subset.
	p1 := &staticProvider{entries: []SlashEntry{
		{Command: "/zz", Description: "match", Kind: KindBuiltin},
	}}
	p2 := &staticProvider{entries: []SlashEntry{
		{Command: "/a-very-long-command", Description: "unrelated", Kind: KindSkill},
	}}
	reg := NewSlashRegistry(p1, p2)
	defer reg.Close()

	if n := len(reg.Filter("match")); n != 1 {
		t.Fatalf("precondition: Filter should narrow to 1 entry, got %d", n)
	}
	if got, want := reg.MaxCommandWidth(), len("/a-very-long-command"); got != want {
		t.Errorf("MaxCommandWidth() = %d, want %d (widest across All())", got, want)
	}
}

// The max width must track the entry list across reloads, not just at
// construction. A value memoized outside the critical section that replaces
// r.cached survives a reload stale, which silently reintroduces the gutter
// misalignment the width exists to prevent.
func TestSlashRegistryMaxCommandWidthAfterReload(t *testing.T) {
	narrow := SlashEntry{Command: "/a", Kind: KindSkill}
	wide := SlashEntry{Command: "/a-much-wider-command", Kind: KindSkill}

	fp := newFakeProvider([]SlashEntry{narrow})
	reg := NewSlashRegistry(fp)
	defer reg.Close()

	if got, want := reg.MaxCommandWidth(), commandWidth(narrow); got != want {
		t.Fatalf("initial MaxCommandWidth() = %d, want %d", got, want)
	}

	// Grow: a wider entry arrives on reload.
	fp.push([]SlashEntry{narrow, wide})
	waitForMaxCommandWidth(t, reg, commandWidth(wide))

	// Shrink: the wide entry goes away again. A max that only ever grows is
	// just as stale as one that never updates at all.
	fp.push([]SlashEntry{narrow})
	waitForMaxCommandWidth(t, reg, commandWidth(narrow))
}

// waitForMaxCommandWidth polls until the registry's max width reaches want,
// covering the fan-out debounce, and fails with the last value it observed.
func waitForMaxCommandWidth(t *testing.T, reg *SlashRegistry, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	got := reg.MaxCommandWidth()
	for time.Now().Before(deadline) {
		if got == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
		got = reg.MaxCommandWidth()
	}
	t.Fatalf("MaxCommandWidth() = %d after reload, want %d", got, want)
}

func TestSlashRegistryFilterCaseInsensitive(t *testing.T) {
	p := &staticProvider{entries: []SlashEntry{
		{Command: "/code-review", Description: "Review code", Kind: KindSkill},
		{Command: "/model", Description: "Switch model", Kind: KindBuiltin},
	}}
	reg := NewSlashRegistry(p)
	defer reg.Close()

	hits := reg.Filter("CODE")
	if len(hits) != 1 || hits[0].Command != "/code-review" {
		t.Errorf("Filter('CODE') = %v", hits)
	}

	hits = reg.Filter("")
	if len(hits) != 2 {
		t.Errorf("empty filter should return all; got %d", len(hits))
	}
}

func TestSlashRegistryLiveReload(t *testing.T) {
	fp := newFakeProvider([]SlashEntry{{Command: "/a", Kind: KindSkill}})
	reg := NewSlashRegistry(fp)
	defer reg.Close()

	if len(reg.All()) != 1 {
		t.Fatalf("initial: want 1 entry")
	}

	// Push an update through the provider.
	fp.push([]SlashEntry{
		{Command: "/a", Kind: KindSkill},
		{Command: "/b", Kind: KindSkill},
	})

	// Wait for debounce + fan-out goroutine to update the registry.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if len(reg.All()) == 2 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("live reload: want 2 entries after push, got %d", len(reg.All()))
}

func TestSlashRegistryChangesSignal(t *testing.T) {
	fp := newFakeProvider(nil)
	reg := NewSlashRegistry(fp)
	defer reg.Close()

	fp.push([]SlashEntry{{Command: "/new", Kind: KindSkill}})

	select {
	case <-reg.Changes():
		// good
	case <-time.After(500 * time.Millisecond):
		t.Error("Changes() channel did not fire after provider signal")
	}
}

func TestSlashRegistryConcurrentFilter(t *testing.T) {
	entries := make([]SlashEntry, 100)
	for i := range entries {
		entries[i] = SlashEntry{Command: "/cmd", Kind: KindSkill}
	}
	fp := newFakeProvider(entries)
	reg := NewSlashRegistry(fp)
	defer reg.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 50; i++ {
			fp.push(entries)
			time.Sleep(2 * time.Millisecond)
		}
	}()

	for i := 0; i < 200; i++ {
		result := reg.Filter("cmd")
		if result == nil {
			t.Error("Filter returned nil slice")
		}
		time.Sleep(time.Millisecond)
	}
	<-done
}

func TestBuiltinProviderEntries(t *testing.T) {
	p := NewBuiltinProvider()
	entries := p.Commands()
	if len(entries) == 0 {
		t.Fatal("BuiltinProvider should have entries")
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Command, "/") {
			t.Errorf("builtin command should start with '/': %q", e.Command)
		}
		if e.Kind != KindBuiltin {
			t.Errorf("%s: expected KindBuiltin, got %q", e.Command, e.Kind)
		}
	}
	if p.Changes() != nil {
		t.Error("BuiltinProvider.Changes() should be nil (static)")
	}
}

func TestSlashEntryKindTag(t *testing.T) {
	cases := []struct {
		entry SlashEntry
		want  string
	}{
		{SlashEntry{Kind: KindBuiltin}, "[builtin]"},
		{SlashEntry{Kind: KindSkill}, "[skill]"},
		{SlashEntry{Kind: KindMCP, Server: "everything"}, "[mcp:everything]"},
	}
	for _, c := range cases {
		got := c.entry.KindTag()
		if got != c.want {
			t.Errorf("KindTag() = %q, want %q", got, c.want)
		}
	}
}

func TestSlashEntryExpansion(t *testing.T) {
	e := SlashEntry{Command: "/foo"}
	// No expand func set — default appends space.
	if got := e.Expansion(); got != "/foo " {
		t.Errorf("default expansion = %q, want %q", got, "/foo ")
	}

	e.expand = func() string { return "custom" }
	if got := e.Expansion(); got != "custom" {
		t.Errorf("custom expansion = %q, want %q", got, "custom")
	}
}
