package loopauth

import (
	"sort"
	"strings"
)

// The scope vocabulary. A token's scopes narrow which RPCs it may call at the
// Connect edge; a token with no scopes is unrestricted.
const (
	// ScopeLoopStart authorizes StartLoop.
	ScopeLoopStart = "loop:start"
	// ScopeLoopSend authorizes Send to a loop the caller owns.
	ScopeLoopSend = "loop:send"
	// ScopeLoopObserve authorizes Observe of a loop the caller owns.
	ScopeLoopObserve = "loop:observe"
	// ScopeLoopAdmin widens owner authorization for Send and Observe to any loop
	// within the caller's tenant. It is honoured only when explicitly granted.
	ScopeLoopAdmin = "loop:admin"
	// ScopeCapabilitiesRead authorizes reading the server's capabilities.
	ScopeCapabilitiesRead = "capabilities:read"
	// ScopeSecretsInline authorizes inline secrets on a request. The loopconnect
	// StartLoop handler enforces it for StartLoopRequest.secrets.
	ScopeSecretsInline = "secrets:inline"
)

// Scopes is a canonical scope set: the distinct non-empty scope strings,
// sorted, joined by one space (the RFC 8693 section 4.2 wire form). It is a
// string, not a slice, so Principal stays comparable: the sandbox pool keys
// warm runners by Principal. The empty set means unrestricted.
type Scopes string

// NewScopes canonicalizes items: each is trimmed, empty items are dropped,
// duplicates are removed, and the rest are sorted and joined by one space.
func NewScopes(items ...string) Scopes {
	seen := make(map[string]struct{}, len(items))
	out := make([]string, 0, len(items))
	for _, item := range items {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if _, dup := seen[item]; dup {
			continue
		}
		seen[item] = struct{}{}
		out = append(out, item)
	}
	sort.Strings(out)
	return Scopes(strings.Join(out, " "))
}

// ParseScopes canonicalizes a whitespace-separated scope claim.
func ParseScopes(claim string) Scopes {
	return NewScopes(strings.Fields(claim)...)
}

// List returns the scopes as a slice. It is never nil: the empty set yields
// an empty slice.
func (s Scopes) List() []string {
	if s == "" {
		return []string{}
	}
	return strings.Split(string(s), " ")
}

// Contains reports whether scope is in the set.
func (s Scopes) Contains(scope string) bool {
	if scope == "" || s == "" {
		return false
	}
	for _, item := range strings.Split(string(s), " ") {
		if item == scope {
			return true
		}
	}
	return false
}

// Empty reports whether the set is empty, which means unrestricted.
func (s Scopes) Empty() bool { return s == "" }

// Allows reports whether p may perform an operation that requires scope:
// an unrestricted principal (no scopes) allows everything.
func (p Principal) Allows(scope string) bool {
	return p.Scopes.Empty() || p.Scopes.Contains(scope)
}

// Grants reports whether scope was EXPLICITLY granted. It is used for widening
// grants (loop:admin), which an unrestricted principal never receives implicitly.
func (p Principal) Grants(scope string) bool { return p.Scopes.Contains(scope) }
