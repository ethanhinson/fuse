package secrets

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"
)

func TestEnvSecretsStore_Get(t *testing.T) {
	const key = "FUSE_TEST_SECRET_GET_XYZ"
	const val = "super-secret"

	os.Setenv(key, val)
	t.Cleanup(func() { os.Unsetenv(key) })

	store := &EnvSecretsStore{}
	ctx := context.Background()

	got, err := store.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get(%q) error: %v", key, err)
	}
	if got != val {
		t.Errorf("Get(%q) = %q, want %q", key, got, val)
	}

	_, err = store.Get(ctx, "FUSE_TEST_DEFINITELY_NOT_SET_12345")
	if !errors.Is(err, ErrSecretNotFound) {
		t.Errorf("expected ErrSecretNotFound for unknown key, got %v", err)
	}
}

func TestEnvSecretsStore_List(t *testing.T) {
	const key = "FUSE_TEST_SECRET_LIST_XYZ"
	os.Setenv(key, "anything")
	t.Cleanup(func() { os.Unsetenv(key) })

	store := &EnvSecretsStore{}
	names, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List error: %v", err)
	}

	found := false
	for _, n := range names {
		if n == key {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("List did not include %q", key)
	}
}

func TestEnvSecretsStore_ExportForContainer_Plain(t *testing.T) {
	const key = "FUSE_TEST_SECRET_EXPORT_XYZ"
	const val = "export-value"
	os.Setenv(key, val)
	t.Cleanup(func() { os.Unsetenv(key) })

	store := &EnvSecretsStore{}
	cs, err := store.ExportForContainer(context.Background(), []string{key}, "")
	if err != nil {
		t.Fatalf("ExportForContainer error: %v", err)
	}

	if cs.EncryptedBundle != nil {
		t.Error("EncryptedBundle should be nil when no public key is set")
	}
	if cs.Env == nil {
		t.Fatal("Env map should be populated")
	}
	if cs.Env[key] != val {
		t.Errorf("Env[%q] = %q, want %q", key, cs.Env[key], val)
	}
}

func TestPassthroughEncryption(t *testing.T) {
	p := &PassthroughEncryptionProvider{}
	ctx := context.Background()
	plaintext := []byte("hello, world")

	ciphertext, err := p.Encrypt(ctx, plaintext)
	if err != nil {
		t.Fatalf("Encrypt error: %v", err)
	}

	recovered, err := p.Decrypt(ctx, ciphertext)
	if err != nil {
		t.Fatalf("Decrypt error: %v", err)
	}

	if !bytes.Equal(recovered, plaintext) {
		t.Errorf("Decrypt(Encrypt(x)) = %q, want %q", recovered, plaintext)
	}
}
