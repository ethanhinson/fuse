package loopauth_test

import (
	"context"
	"errors"
	"testing"

	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/loopauth"
)

func TestStaticVerifierKnownToken(t *testing.T) {
	want := loopauth.Principal{Tenant: event.TenantID("acme"), Subject: "alice"}
	v := loopauth.NewStaticVerifier(map[string]loopauth.Principal{
		"tok-alice": want,
	})

	got, err := v.Verify(context.Background(), "tok-alice")
	if err != nil {
		t.Fatalf("Verify(known) returned error: %v", err)
	}
	if got != want {
		t.Fatalf("Verify(known) = %+v, want %+v", got, want)
	}
}

func TestStaticVerifierUnknownToken(t *testing.T) {
	v := loopauth.NewStaticVerifier(map[string]loopauth.Principal{
		"tok-alice": {Tenant: event.TenantID("acme"), Subject: "alice"},
	})

	got, err := v.Verify(context.Background(), "nope")
	if !errors.Is(err, loopauth.ErrInvalidToken) {
		t.Fatalf("Verify(unknown) err = %v, want ErrInvalidToken", err)
	}
	if got != (loopauth.Principal{}) {
		t.Fatalf("Verify(unknown) principal = %+v, want zero", got)
	}
}

func TestStaticVerifierEmptyToken(t *testing.T) {
	v := loopauth.NewStaticVerifier(map[string]loopauth.Principal{
		"tok-alice": {Tenant: event.TenantID("acme"), Subject: "alice"},
	})

	if _, err := v.Verify(context.Background(), ""); !errors.Is(err, loopauth.ErrInvalidToken) {
		t.Fatalf("Verify(empty) err = %v, want ErrInvalidToken", err)
	}
}

func TestStaticVerifierDefaultTenantRoundTrip(t *testing.T) {
	want := loopauth.Principal{Tenant: event.DefaultTenant, Subject: "dev"}
	v := loopauth.NewStaticVerifier(map[string]loopauth.Principal{
		"dev": want,
	})

	got, err := v.Verify(context.Background(), "dev")
	if err != nil {
		t.Fatalf("Verify(dev) returned error: %v", err)
	}
	if got != want {
		t.Fatalf("Verify(dev) = %+v, want %+v", got, want)
	}
	if got.Tenant != event.DefaultTenant {
		t.Fatalf("Verify(dev) tenant = %q, want DefaultTenant %q", got.Tenant, event.DefaultTenant)
	}
}
