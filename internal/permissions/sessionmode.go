package permissions

import "sync"

// SessionMode is the thread-safe holder for the session's active permission
// mode. It is owned by the session (not per-gate) and is the single source both
// the TUI (indicator, Shift+Tab, /mode) and per-turn gate construction read, so
// a mode switch is picked up by the next built gate.
//
// Both Get and Set are guarded by the same mutex: a lock on only one side is
// still a data race (mutex-test-double-concurrent-provider), because the
// TUI goroutine and per-turn gate construction cross goroutines.
type SessionMode struct {
	mu   sync.Mutex
	mode PermissionMode
}

// NewSessionMode returns a SessionMode seeded with the startup mode (typically
// ParseMode(cfg.Permissions.Mode)).
func NewSessionMode(mode PermissionMode) *SessionMode {
	return &SessionMode{mode: mode}
}

// Get returns the current session mode under the lock.
func (s *SessionMode) Get() PermissionMode {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mode
}

// Set replaces the session mode under the lock.
func (s *SessionMode) Set(mode PermissionMode) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mode = mode
}
