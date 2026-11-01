// Package loopsecret carries loop-scoped secret values (change 0102) from the
// Connect edge to the harness resolver, in memory only.
//
// A loop-scoped secret is a credential value a client sends with StartLoop for one
// of the secret_ref names in its harness spec. Its value must never be logged,
// persisted, recorded in the event store, or put in an error: every rendering of a
// Secret (fmt, slog, JSON) redacts it. This package imports only the standard
// library so the runtime seam can carry it without importing anything auth-shaped
// (ADR-0030).
package loopsecret

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
)

// Secret is one loop-scoped credential value.
type Secret struct {
	// Name is the secret_ref name in the harness spec this value satisfies.
	Name string
	// Value is the credential itself. It is never rendered by any formatting,
	// logging, or encoding method on Secret.
	Value string
	// Hosts lists the destination hostnames the value may be sent to.
	Hosts []string
}

const redacted = "[REDACTED]"

// String renders the secret with its value redacted.
func (s Secret) String() string {
	return fmt.Sprintf("loopsecret.Secret{Name:%q, Hosts:%q, Value:%s}", s.Name, s.Hosts, redacted)
}

// GoString renders the secret with its value redacted, for %#v.
func (s Secret) GoString() string { return s.String() }

// Format renders the redacted String for every verb, so no fmt verb (including
// %+v and %#v, and a Secret nested in a slice or struct) can print the value.
func (s Secret) Format(f fmt.State, _ rune) {
	_, _ = io.WriteString(f, s.String())
}

// LogValue renders the secret for slog with its value redacted.
func (s Secret) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("name", s.Name),
		slog.Any("hosts", s.Hosts),
		slog.String("value", redacted),
	)
}

// MarshalJSON encodes the secret with its value redacted.
func (s Secret) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Name  string   `json:"name"`
		Hosts []string `json:"hosts"`
		Value string   `json:"value"`
	}{s.Name, s.Hosts, redacted})
}

// Wire limits on loop-scoped secrets in one StartLoop request.
const (
	// MaxSecrets is the most entries one request may carry.
	MaxSecrets = 32
	// MaxValueBytes is the longest value, in bytes, one entry may carry.
	MaxValueBytes = 16 << 10
)

// CheckLimits reports the first wire-limit violation in ss, or nil. Its message
// names only counts, indexes and byte lengths, never a value.
func CheckLimits(ss []Secret) error {
	if len(ss) > MaxSecrets {
		return fmt.Errorf("%d secrets exceeds the limit of %d", len(ss), MaxSecrets)
	}
	for i, s := range ss {
		if len(s.Value) > MaxValueBytes {
			return fmt.Errorf("secrets[%d].value is %d bytes, over the limit of %d", i, len(s.Value), MaxValueBytes)
		}
	}
	return nil
}

type ctxKey struct{}

// WithSecrets returns a context carrying a deep copy of ss. An empty ss returns
// ctx unchanged.
func WithSecrets(ctx context.Context, ss []Secret) context.Context {
	if len(ss) == 0 {
		return ctx
	}
	cp := make([]Secret, len(ss))
	for i, s := range ss {
		cp[i] = Secret{Name: s.Name, Value: s.Value, Hosts: append([]string(nil), s.Hosts...)}
	}
	return context.WithValue(ctx, ctxKey{}, cp)
}

// From returns the secrets WithSecrets stored on ctx, or nil.
func From(ctx context.Context) []Secret {
	ss, _ := ctx.Value(ctxKey{}).([]Secret)
	return ss
}
