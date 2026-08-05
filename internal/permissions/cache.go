package permissions

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sync"
)

// ApprovalCache stores session-scoped "allow for session" decisions keyed by
// (toolName, argsFingerprint). It is in-memory only and never persisted.
type ApprovalCache struct {
	mu      sync.RWMutex
	allowed map[string]struct{}
}

func newApprovalCache() *ApprovalCache {
	return &ApprovalCache{allowed: map[string]struct{}{}}
}

// Check returns true if this (tool, args) pair was previously session-allowed.
func (c *ApprovalCache) Check(toolName, args string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	_, ok := c.allowed[cacheKey(toolName, args)]
	return ok
}

// Allow records a session approval for (tool, args).
func (c *ApprovalCache) Allow(toolName, args string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.allowed[cacheKey(toolName, args)] = struct{}{}
}

// cacheKey builds a short stable key for (toolName, normalizedArgs).
func cacheKey(toolName, args string) string {
	normalized := normalizeArgs(args)
	h := sha256.Sum256([]byte(normalized))
	return fmt.Sprintf("%s:%x", toolName, h[:4])
}

// Clone returns a snapshot copy of the cache. Mutations to the clone do not
// propagate to the original and vice versa.
func (c *ApprovalCache) Clone() *ApprovalCache {
	c.mu.RLock()
	defer c.mu.RUnlock()
	clone := newApprovalCache()
	for k := range c.allowed {
		clone.allowed[k] = struct{}{}
	}
	return clone
}

// normalizeArgs round-trips JSON to produce a canonical representation.
// Falls back to the raw string on parse failure.
func normalizeArgs(args string) string {
	var v any
	if err := json.Unmarshal([]byte(args), &v); err != nil {
		return args
	}
	out, err := json.Marshal(v)
	if err != nil {
		return args
	}
	return string(out)
}
