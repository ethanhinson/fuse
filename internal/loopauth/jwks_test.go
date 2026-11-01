package loopauth

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/event"
)

func ecJWK(kid string, k *ecdsa.PrivateKey) JWK {
	x := k.X.FillBytes(make([]byte, 32))
	y := k.Y.FillBytes(make([]byte, 32))
	return JWK{Kid: kid, Kty: "EC", Crv: "P-256", X: base64.RawURLEncoding.EncodeToString(x), Y: base64.RawURLEncoding.EncodeToString(y)}
}

func edJWK(kid string, pub ed25519.PublicKey) JWK {
	return JWK{Kid: kid, Kty: "OKP", Crv: "Ed25519", X: base64.RawURLEncoding.EncodeToString(pub)}
}

func mintES256(t *testing.T, kid string, claims any, k *ecdsa.PrivateKey) string {
	t.Helper()
	input := b64(map[string]any{"alg": "ES256", "kid": kid}) + "." + b64(claims)
	sum := sha256.Sum256([]byte(input))
	r, s, err := ecdsa.Sign(rand.Reader, k, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	sig := append(r.FillBytes(make([]byte, 32)), s.FillBytes(make([]byte, 32))...)
	return input + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func mintEdDSA(kid string, claims any, k ed25519.PrivateKey) string {
	input := b64(map[string]any{"alg": "EdDSA", "kid": kid}) + "." + b64(claims)
	return input + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(k, []byte(input)))
}

func jwkIssuer(jwks ...JWK) Issuer {
	return Issuer{Name: "cms", JWKS: jwks, Audience: "fuse-loop", MaxTTL: time.Hour, TenantPrefix: "team-", Tenants: []event.TenantID{event.DefaultTenant}}
}

func TestJWKIssuerES256KidAndTenantPrefix(t *testing.T) {
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	v := newTestIssuerVerifier(t, jwkIssuer(ecJWK("a", k)))
	ok, err := v.verify(mintES256(t, "a", with(goodClaims(), "tenant", "team-42"), k))
	if err != nil || ok.Tenant != "team-42" {
		t.Fatalf("want team-42 accepted, got %v %v", ok, err)
	}
	for name, tok := range map[string]string{
		"other tenant": mintES256(t, "a", with(goodClaims(), "tenant", "other-42"), k),
		"upper tenant": mintES256(t, "a", with(goodClaims(), "tenant", "Team-42"), k),
		"unknown kid":  mintES256(t, "b", with(goodClaims(), "tenant", "team-42"), k),
	} {
		if _, err := v.Verify(t.Context(), tok); err != ErrInvalidToken {
			t.Errorf("%s: want ErrInvalidToken, got %v", name, err)
		}
	}
}

func TestJWKIssuerEdDSA(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	v := newTestIssuerVerifier(t, jwkIssuer(edJWK("e", pub)))
	if _, err := v.verify(mintEdDSA("e", goodClaims(), priv)); err != nil {
		t.Fatalf("EdDSA token refused: %v", err)
	}
}

func TestAlgConfusionRefused(t *testing.T) {
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	jwk := newTestIssuerVerifier(t, jwkIssuer(ecJWK("a", k)))
	hs := mint(map[string]any{"alg": "HS256", "kid": "a"}, goodClaims(), []byte("anything"))
	if _, err := jwk.Verify(t.Context(), hs); err != ErrInvalidToken {
		t.Errorf("HS256 on JWK issuer: %v", err)
	}
	none := b64(map[string]any{"alg": "none", "kid": "a"}) + "." + b64(goodClaims()) + "."
	if _, err := jwk.Verify(t.Context(), none); err != ErrInvalidToken {
		t.Errorf("alg none: %v", err)
	}
	// Missing kid on a JWK issuer.
	nokid := mintES256(t, "", goodClaims(), k)
	if _, err := jwk.verify(nokid); err == nil {
		t.Error("missing kid accepted")
	}
	// ES256 against a shared-secret issuer.
	shared := newTestIssuerVerifier(t)
	if _, err := shared.verify(mintES256(t, "a", goodClaims(), k)); err == nil {
		t.Error("ES256 on shared-secret issuer accepted")
	}
	// A kid naming a key of a different issuer.
	other := jwkIssuer(ecJWK("x", k))
	other.Name = "other"
	both := newTestIssuerVerifier(t, jwkIssuer(ecJWK("a", k)), other)
	if _, err := both.verify(mintES256(t, "x", goodClaims(), k)); err == nil {
		t.Error("kid of another issuer accepted")
	}
	// ES256 header over an Ed25519 key.
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	ed := newTestIssuerVerifier(t, jwkIssuer(edJWK("e", pub)))
	if _, err := ed.verify(mintES256(t, "e", goodClaims(), k)); err == nil {
		t.Error("alg/key-type mismatch accepted")
	}
}

func TestJWKSURLRefreshAndUnknownKidThrottle(t *testing.T) {
	k1, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	k2, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	var hits atomic.Int32
	var doc atomic.Value
	set := func(keys ...JWK) {
		b, _ := json.Marshal(map[string]any{"keys": keys})
		doc.Store(b)
	}
	set(ecJWK("a", k1))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write(doc.Load().([]byte))
	}))
	defer srv.Close()

	now := testNow
	iss := Issuer{Name: "cms", JWKSURL: srv.URL, Audience: "fuse-loop", MaxTTL: time.Hour, JWKSRefresh: 5 * time.Minute,
		Tenants: []event.TenantID{event.DefaultTenant}}
	v, err := NewIssuerVerifier([]Issuer{iss})
	if err != nil {
		t.Fatal(err)
	}
	v.now = func() time.Time { return now }
	// iat/exp in goodClaims are relative to testNow, which is the clock here.
	if _, err := v.verify(mintES256(t, "a", goodClaims(), k1)); err != nil {
		t.Fatalf("initial key: %v", err)
	}
	// New kid appears; first unknown-kid lookup refetches and accepts it.
	set(ecJWK("a", k1), ecJWK("b", k2))
	if _, err := v.verify(mintES256(t, "b", goodClaims(), k2)); err != nil {
		t.Fatalf("rotated key not picked up: %v", err)
	}
	// Repeated unknown kids inside a minute refetch at most once.
	before := hits.Load()
	for i := 0; i < 5; i++ {
		v.verify(mintES256(t, "zzz", goodClaims(), k1))
	}
	if d := hits.Load() - before; d > 1 {
		t.Fatalf("unknown kid caused %d refetches within a minute, want <= 1", d)
	}
	// After the refresh interval, a known kid triggers a refetch.
	now = now.Add(6 * time.Minute)
	before = hits.Load()
	v.verify(mintES256(t, "a", with(goodClaims(), "iat", now.Add(-time.Minute).Unix(), "exp", now.Add(time.Minute).Unix()), k1))
	if hits.Load() == before {
		t.Fatal("no refetch after refresh interval")
	}
}

func TestJWKSURLInitialFetchFailureFatal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", 500) }))
	defer srv.Close()
	_, err := NewIssuerVerifier([]Issuer{{Name: "cms", JWKSURL: srv.URL, Audience: "a", MaxTTL: time.Hour}})
	if err == nil {
		t.Fatal("want startup error when no document can be fetched")
	}
}
