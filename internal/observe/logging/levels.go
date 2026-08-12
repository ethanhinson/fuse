package logging

import (
	"errors"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Level is an ordered logging threshold.
type Level uint8

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)

func (l Level) String() string {
	switch l {
	case LevelDebug:
		return "debug"
	case LevelInfo:
		return "info"
	case LevelWarn:
		return "warn"
	case LevelError:
		return "error"
	default:
		return "unknown"
	}
}

func ParseLevel(s string) (Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return LevelDebug, nil
	case "info":
		return LevelInfo, nil
	case "warn", "warning":
		return LevelWarn, nil
	case "error":
		return LevelError, nil
	}
	return 0, errors.New("logging: invalid level")
}

type Scope string

const (
	ScopeGlobal Scope = "global"
	ScopeTenant Scope = "tenant"
	ScopeLoop   Scope = "loop"
)

type Override struct {
	Scope     Scope     `json:"scope"`
	TenantID  string    `json:"tenant_id,omitempty"`
	LoopID    string    `json:"loop_id,omitempty"`
	Level     Level     `json:"level"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
}
type State struct {
	Default   Level      `json:"default"`
	Global    *Level     `json:"global,omitempty"`
	Overrides []Override `json:"overrides"`
}
type scopedLevel struct {
	level  Level
	expiry time.Time
}
type levelSnapshot struct {
	configured Level
	global     *Level
	tenants    map[string]scopedLevel
	loops      map[loopKey]scopedLevel
}
type loopKey struct{ tenant, loop string }

// Levels publishes immutable snapshots, keeping steady-state resolution lock-free.
type Levels struct {
	snapshot  atomic.Pointer[levelSnapshot]
	mutate    sync.Mutex
	maxTTL    time.Duration
	now       func() time.Time
	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
}

func NewLevels(configured Level, maxTTL time.Duration, now func() time.Time) (*Levels, error) {
	if configured > LevelError || maxTTL <= 0 {
		return nil, errors.New("logging: invalid level configuration")
	}
	if now == nil {
		now = time.Now
	}
	l := &Levels{maxTTL: maxTTL, now: now, stop: make(chan struct{}), done: make(chan struct{})}
	l.snapshot.Store(&levelSnapshot{configured: configured, tenants: map[string]scopedLevel{}, loops: map[loopKey]scopedLevel{}})
	go l.expiryWorker()
	return l, nil
}

func (l *Levels) expiryWorker() {
	interval := l.maxTTL / 4
	if interval > time.Second {
		interval = time.Second
	}
	if interval < time.Millisecond {
		interval = time.Millisecond
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	defer close(l.done)
	for {
		select {
		case <-t.C:
			l.Expire()
		case <-l.stop:
			return
		}
	}
}

func (l *Levels) Effective(tenant, loop string) Level {
	s := l.snapshot.Load()
	now := l.now()
	if v, ok := s.loops[loopKey{tenant, loop}]; ok && now.Before(v.expiry) {
		return v.level
	}
	if v, ok := s.tenants[tenant]; ok && now.Before(v.expiry) {
		return v.level
	}
	if s.global != nil {
		return *s.global
	}
	return s.configured
}

func (l *Levels) SetDefault(v Level) error {
	if v > LevelError {
		return errors.New("logging: invalid level")
	}
	l.change(func(n *levelSnapshot) { n.configured = v })
	return nil
}
func (l *Levels) SetGlobal(v Level) error {
	if v > LevelError {
		return errors.New("logging: invalid level")
	}
	l.change(func(n *levelSnapshot) { x := v; n.global = &x })
	return nil
}
func (l *Levels) RemoveGlobal() { l.change(func(n *levelSnapshot) { n.global = nil }) }
func (l *Levels) SetTenant(tenant string, v Level, expiry time.Time) error {
	if tenant == "" {
		return errors.New("logging: tenant required")
	}
	if err := l.validScoped(v, expiry); err != nil {
		return err
	}
	l.change(func(n *levelSnapshot) { n.tenants[tenant] = scopedLevel{v, expiry} })
	return nil
}
func (l *Levels) RemoveTenant(tenant string) {
	l.change(func(n *levelSnapshot) { delete(n.tenants, tenant) })
}
func (l *Levels) SetLoop(tenant, loop string, v Level, expiry time.Time) error {
	if tenant == "" || loop == "" {
		return errors.New("logging: tenant and loop required")
	}
	if err := l.validScoped(v, expiry); err != nil {
		return err
	}
	l.change(func(n *levelSnapshot) { n.loops[loopKey{tenant, loop}] = scopedLevel{v, expiry} })
	return nil
}
func (l *Levels) RemoveLoop(tenant, loop string) {
	l.change(func(n *levelSnapshot) { delete(n.loops, loopKey{tenant, loop}) })
}
func (l *Levels) validScoped(v Level, expiry time.Time) error {
	now := l.now()
	if v > LevelError || expiry.IsZero() || !expiry.After(now) || expiry.After(now.Add(l.maxTTL)) {
		return errors.New("logging: scoped override expiry must be in the future and within maximum TTL")
	}
	return nil
}

func (l *Levels) Expire() {
	now := l.now()
	l.change(func(n *levelSnapshot) {
		for k, v := range n.tenants {
			if !now.Before(v.expiry) {
				delete(n.tenants, k)
			}
		}
		for k, v := range n.loops {
			if !now.Before(v.expiry) {
				delete(n.loops, k)
			}
		}
	})
}
func (l *Levels) Inspect() State {
	s := l.snapshot.Load()
	out := State{Default: s.configured, Overrides: make([]Override, 0, len(s.tenants)+len(s.loops))}
	if s.global != nil {
		x := *s.global
		out.Global = &x
	}
	for k, v := range s.tenants {
		out.Overrides = append(out.Overrides, Override{Scope: ScopeTenant, TenantID: k, Level: v.level, ExpiresAt: v.expiry})
	}
	for k, v := range s.loops {
		out.Overrides = append(out.Overrides, Override{Scope: ScopeLoop, TenantID: k.tenant, LoopID: k.loop, Level: v.level, ExpiresAt: v.expiry})
	}
	sort.Slice(out.Overrides, func(i, j int) bool {
		a, b := out.Overrides[i], out.Overrides[j]
		if a.Scope != b.Scope {
			return a.Scope < b.Scope
		}
		if a.TenantID != b.TenantID {
			return a.TenantID < b.TenantID
		}
		return a.LoopID < b.LoopID
	})
	return out
}
func (l *Levels) change(fn func(*levelSnapshot)) {
	l.mutate.Lock()
	defer l.mutate.Unlock()
	old := l.snapshot.Load()
	n := &levelSnapshot{configured: old.configured, global: old.global, tenants: make(map[string]scopedLevel, len(old.tenants)), loops: make(map[loopKey]scopedLevel, len(old.loops))}
	for k, v := range old.tenants {
		n.tenants[k] = v
	}
	for k, v := range old.loops {
		n.loops[k] = v
	}
	fn(n)
	l.snapshot.Store(n)
}
func (l *Levels) Close() error { l.closeOnce.Do(func() { close(l.stop); <-l.done }); return nil }
