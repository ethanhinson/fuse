package mcp

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestGeneratePKCE(t *testing.T) {
	verifier, challenge, err := generatePKCE()
	if err != nil {
		t.Fatalf("generatePKCE() error: %v", err)
	}

	// Verifier: 32 random bytes → base64url (no padding) → 43 chars.
	if len(verifier) != 43 {
		t.Errorf("verifier length = %d, want 43", len(verifier))
	}

	// Verify it's valid base64url (no padding, no +/).
	if _, err := base64.RawURLEncoding.DecodeString(verifier); err != nil {
		t.Errorf("verifier is not valid base64url: %v", err)
	}

	// Challenge: SHA-256 of verifier (32 bytes) → base64url → 43 chars.
	if len(challenge) != 43 {
		t.Errorf("challenge length = %d, want 43", len(challenge))
	}
	if _, err := base64.RawURLEncoding.DecodeString(challenge); err != nil {
		t.Errorf("challenge is not valid base64url: %v", err)
	}

	// Each call should produce different values.
	v2, c2, err := generatePKCE()
	if err != nil {
		t.Fatalf("generatePKCE() second call error: %v", err)
	}
	if verifier == v2 {
		t.Error("two generatePKCE() calls returned the same verifier")
	}
	if challenge == c2 {
		t.Error("two generatePKCE() calls returned the same challenge")
	}
}

func TestTokenFileRoundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test-token.json")

	ts := &tokenSet{
		AccessToken:  "acc-token-123",
		RefreshToken: "ref-token-456",
		Expiry:       time.Now().Add(1 * time.Hour).Truncate(time.Second),
		ClientID:     "my-client",
		ClientSecret: "my-secret",
	}

	if err := saveTokens(path, ts); err != nil {
		t.Fatalf("saveTokens: %v", err)
	}

	// File should be written with mode 0600.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat token file: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("token file mode = %o, want 600", info.Mode().Perm())
	}

	loaded, err := loadTokens(path)
	if err != nil {
		t.Fatalf("loadTokens: %v", err)
	}
	if loaded == nil {
		t.Fatal("loadTokens returned nil for existing file")
	}

	if loaded.AccessToken != ts.AccessToken {
		t.Errorf("AccessToken = %q, want %q", loaded.AccessToken, ts.AccessToken)
	}
	if loaded.RefreshToken != ts.RefreshToken {
		t.Errorf("RefreshToken = %q, want %q", loaded.RefreshToken, ts.RefreshToken)
	}
	if !loaded.Expiry.Equal(ts.Expiry) {
		t.Errorf("Expiry = %v, want %v", loaded.Expiry, ts.Expiry)
	}
	if loaded.ClientID != ts.ClientID {
		t.Errorf("ClientID = %q, want %q", loaded.ClientID, ts.ClientID)
	}
	if loaded.ClientSecret != ts.ClientSecret {
		t.Errorf("ClientSecret = %q, want %q", loaded.ClientSecret, ts.ClientSecret)
	}
}

func TestLoadTokensMissingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nonexistent.json")

	ts, err := loadTokens(path)
	if err != nil {
		t.Fatalf("loadTokens on missing file: %v", err)
	}
	if ts != nil {
		t.Errorf("expected nil for missing file, got %+v", ts)
	}
}

func TestTokenFilePath_Default(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir available")
	}

	path, err := tokenFilePath("my-server", "")
	if err != nil {
		t.Fatalf("tokenFilePath: %v", err)
	}

	want := filepath.Join(home, ".fuse", "mcp-tokens", "my-server.json")
	if path != want {
		t.Errorf("tokenFilePath = %q, want %q", path, want)
	}
}

func TestTokenFilePath_Override(t *testing.T) {
	override := "/tmp/custom-token.json"
	path, err := tokenFilePath("any-server", override)
	if err != nil {
		t.Fatalf("tokenFilePath: %v", err)
	}
	if path != override {
		t.Errorf("tokenFilePath = %q, want %q", path, override)
	}
}

func TestSaveTokensMkdirAll(t *testing.T) {
	dir := t.TempDir()
	// Deep path that doesn't exist yet.
	path := filepath.Join(dir, "a", "b", "c", "token.json")

	ts := &tokenSet{AccessToken: "tok", Expiry: time.Now().Add(time.Hour)}
	if err := saveTokens(path, ts); err != nil {
		t.Fatalf("saveTokens with deep path: %v", err)
	}

	loaded, err := loadTokens(path)
	if err != nil {
		t.Fatalf("loadTokens after deep save: %v", err)
	}
	if loaded == nil || loaded.AccessToken != "tok" {
		t.Errorf("unexpected loaded token: %+v", loaded)
	}
}
