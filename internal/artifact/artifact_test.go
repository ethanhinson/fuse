package artifact_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sort"
	"strings"
	"testing"

	"github.com/ethanhinson/fuse/internal/artifact"
	"github.com/ethanhinson/fuse/internal/event"
)

func TestRefStringRoundTrip(t *testing.T) {
	ref := artifact.Ref{Tenant: "acme", Loop: "0190abc", Kind: artifact.KindSpill, ID: "x1"}
	if got, want := ref.String(), "art://acme/0190abc/spill/x1"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
	parsed, err := artifact.ParseRef(ref.String())
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}
	if parsed != ref {
		t.Fatalf("ParseRef = %+v, want %+v", parsed, ref)
	}
	if !artifact.IsRef(ref.String()) || artifact.IsRef("/tmp/x") {
		t.Fatal("IsRef misclassifies")
	}
}

func TestParseRefRejects(t *testing.T) {
	long := strings.Repeat("a", 129)
	cases := map[string]string{
		"missing scheme":      "acme/l1/spill/x1",
		"wrong scheme":        "file://acme/l1/spill/x1",
		"too few segments":    "art://acme/l1/spill",
		"too many segments":   "art://acme/l1/spill/x1/y",
		"empty segment":       "art://acme//spill/x1",
		"empty id":            "art://acme/l1/spill/",
		"unknown kind":        "art://acme/l1/bogus/x1",
		"dotdot segment":      "art://acme/../spill/x1",
		"embedded dotdot":     "art://acme/l1/spill/a..b",
		"leading dot":         "art://acme/l1/spill/.hidden",
		"overlong segment":    "art://acme/l1/spill/" + long,
		"space":               "art://acme/l1/spill/a b",
		"backslash":           "art://acme/l1/spill/a\\b",
		"unicode":             "art://acme/l1/spill/é",
		"percent":             "art://acme/l1/spill/a%2f",
		"slash smuggled kind": "art://acme/l1/spill//x1",
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := artifact.ParseRef(in); !errors.Is(err, artifact.ErrInvalidRef) {
				t.Fatalf("ParseRef(%q) err = %v, want ErrInvalidRef", in, err)
			}
		})
	}
	// A 128-byte segment is the boundary and must be accepted.
	ok := "art://acme/l1/spill/" + strings.Repeat("a", 128)
	if _, err := artifact.ParseRef(ok); err != nil {
		t.Fatalf("128-byte segment rejected: %v", err)
	}
}

func TestParsePrefix(t *testing.T) {
	cases := []struct {
		in   string
		want artifact.Ref
	}{
		{"art://t/l", artifact.Ref{Tenant: "t", Loop: "l"}},
		{"art://t/l/", artifact.Ref{Tenant: "t", Loop: "l"}},
		{"art://t/l/spill", artifact.Ref{Tenant: "t", Loop: "l", Kind: artifact.KindSpill}},
		{"art://t/l/spill/", artifact.Ref{Tenant: "t", Loop: "l", Kind: artifact.KindSpill}},
		{"art://t/l/spill/x1", artifact.Ref{Tenant: "t", Loop: "l", Kind: artifact.KindSpill, ID: "x1"}},
	}
	for _, c := range cases {
		got, err := artifact.ParsePrefix(c.in)
		if err != nil {
			t.Fatalf("ParsePrefix(%q): %v", c.in, err)
		}
		if got != c.want {
			t.Fatalf("ParsePrefix(%q) = %+v, want %+v", c.in, got, c.want)
		}
	}
	for _, bad := range []string{"t/l", "art://t", "art://t/", "art://t/l/bogus", "art://t/../spill", "art://t/l/spill/x/y"} {
		if _, err := artifact.ParsePrefix(bad); !errors.Is(err, artifact.ErrInvalidRef) {
			t.Fatalf("ParsePrefix(%q) err = %v, want ErrInvalidRef", bad, err)
		}
	}
}

func TestNewIDUniqueAndSortable(t *testing.T) {
	const n = 10000
	ids := make([]string, n)
	seen := make(map[string]bool, n)
	for i := range ids {
		id := artifact.NewID()
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
		ids[i] = id
		if _, err := artifact.ParseRef("art://t/l/spill/" + id); err != nil {
			t.Fatalf("NewID %q is not a valid ref segment: %v", id, err)
		}
	}
	if !sort.StringsAreSorted(ids) {
		t.Fatal("ids from successive NewID calls are not ascending")
	}
}

// spyStore records whether its Open was reached.
type spyStore struct {
	artifact.Store
	opened int
}

func (s *spyStore) Open(ctx context.Context, ref artifact.Ref) (io.ReadCloser, artifact.Meta, error) {
	s.opened++
	return s.Store.Open(ctx, ref)
}

func TestBindingScopesOpen(t *testing.T) {
	ctx := context.Background()
	spy := &spyStore{Store: artifact.NewMemStore()}
	b := artifact.Binding{Store: spy, Tenant: "a", Loop: "l1"}
	if !b.Valid() {
		t.Fatal("binding should be valid")
	}
	meta, err := b.Put(ctx, artifact.KindSpill, bytes.NewReader([]byte("sentinel")), artifact.Meta{Tool: "bash"})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	// The same artifact reached through a foreign tenant or loop must not resolve,
	// and the store must never be consulted.
	for _, foreign := range []artifact.Ref{
		{Tenant: "b", Loop: "l1", Kind: meta.Ref.Kind, ID: meta.Ref.ID},
		{Tenant: "a", Loop: "l2", Kind: meta.Ref.Kind, ID: meta.Ref.ID},
	} {
		if _, _, err := b.Open(ctx, foreign); !errors.Is(err, artifact.ErrNotFound) {
			t.Fatalf("Open(%s) err = %v, want ErrNotFound", foreign, err)
		}
	}
	if spy.opened != 0 {
		t.Fatalf("store Open reached %d times for foreign refs", spy.opened)
	}
	rc, _, err := b.Open(ctx, meta.Ref)
	if err != nil {
		t.Fatalf("Open own ref: %v", err)
	}
	body, _ := io.ReadAll(rc)
	rc.Close()
	if string(body) != "sentinel" || spy.opened != 1 {
		t.Fatalf("own Open body=%q opened=%d", body, spy.opened)
	}

	// The empty tenant and DefaultTenant are the same tenant.
	db := artifact.Binding{Store: spy, Loop: "l9"}
	m2, err := db.PutID(ctx, artifact.KindSegmentIndex, "index", strings.NewReader("i"), artifact.Meta{})
	if err != nil {
		t.Fatalf("PutID: %v", err)
	}
	if m2.Ref.Tenant != event.DefaultTenant {
		t.Fatalf("tenant not normalized: %q", m2.Ref.Tenant)
	}
	if _, _, err := db.Open(ctx, m2.Ref); err != nil {
		t.Fatalf("Open via default tenant: %v", err)
	}
	list, err := db.List(ctx, artifact.KindSegmentIndex)
	if err != nil || len(list) != 1 {
		t.Fatalf("List = %v, %v", list, err)
	}

	if (artifact.Binding{Store: spy}).Valid() || (artifact.Binding{Loop: "l"}).Valid() {
		t.Fatal("incomplete binding reported valid")
	}
}

func TestBindingContext(t *testing.T) {
	if _, ok := artifact.BindingFrom(context.Background()); ok {
		t.Fatal("empty context carries a binding")
	}
	b := artifact.Binding{Store: artifact.NewMemStore(), Tenant: "a", Loop: "l1"}
	got, ok := artifact.BindingFrom(artifact.WithBinding(context.Background(), b))
	if !ok || got.Tenant != "a" || got.Loop != "l1" || got.Store != b.Store {
		t.Fatalf("BindingFrom = %+v, %v", got, ok)
	}
}
