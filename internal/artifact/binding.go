package artifact

import (
	"context"
	"io"

	"github.com/ethanhinson/fuse/internal/event"
)

// Binding scopes a Store to one (tenant, loop). Tools resolve refs through a
// binding, never through the bare store, so a ref naming another tenant or loop
// is refused before the store is consulted.
type Binding struct {
	Store  Store
	Tenant event.TenantID
	Loop   event.LoopID
}

// Valid reports whether the binding names a store and a loop.
func (b Binding) Valid() bool { return b.Store != nil && b.Loop != "" }

// Put writes r as a new artifact of kind with a store-allocated id.
func (b Binding) Put(ctx context.Context, kind Kind, r io.Reader, meta Meta) (Meta, error) {
	return b.PutID(ctx, kind, "", r, meta)
}

// PutID writes r under an explicit id, replacing any artifact already there.
func (b Binding) PutID(ctx context.Context, kind Kind, id string, r io.Reader, meta Meta) (Meta, error) {
	return b.Store.Put(ctx, Ref{Tenant: b.Tenant, Loop: b.Loop, Kind: kind, ID: id}, r, meta)
}

// Open resolves ref only when it belongs to this binding's (tenant, loop); any
// other ref is ErrNotFound. The full key is re-asserted on every call.
func (b Binding) Open(ctx context.Context, ref Ref) (io.ReadCloser, Meta, error) {
	if b.Store == nil ||
		event.NormalizeTenant(ref.Tenant) != event.NormalizeTenant(b.Tenant) ||
		ref.Loop != b.Loop {
		return nil, Meta{}, ErrNotFound
	}
	return b.Store.Open(ctx, ref)
}

// List returns this loop's artifacts of kind.
func (b Binding) List(ctx context.Context, kind Kind) ([]Meta, error) {
	return b.Store.List(ctx, b.Tenant, b.Loop, kind)
}

type bindingKey struct{}

// WithBinding returns ctx carrying b.
func WithBinding(ctx context.Context, b Binding) context.Context {
	return context.WithValue(ctx, bindingKey{}, b)
}

// BindingFrom returns the binding ctx carries, if any.
func BindingFrom(ctx context.Context) (Binding, bool) {
	b, ok := ctx.Value(bindingKey{}).(Binding)
	return b, ok
}
