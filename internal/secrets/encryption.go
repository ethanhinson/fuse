package secrets

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"

	"filippo.io/age"
)

// EncryptionProvider handles encrypt/decrypt for secrets at rest and in transit.
type EncryptionProvider interface {
	Name() string
	Encrypt(ctx context.Context, plaintext []byte) (ciphertext []byte, err error)
	Decrypt(ctx context.Context, ciphertext []byte) (plaintext []byte, err error)
}

// ─── AgeEncryptionProvider ───────────────────────────────────────────────────

// AgeEncryptionProvider uses filippo.io/age for encryption.
type AgeEncryptionProvider struct {
	KeyFile  string
	identity *age.X25519Identity
}

// NewAgeEncryptionProvider loads the age identity from keyFile.
func NewAgeEncryptionProvider(keyFile string) (*AgeEncryptionProvider, error) {
	data, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, fmt.Errorf("age key file %s: %w", keyFile, err)
	}
	identities, err := age.ParseIdentities(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("parse age identity: %w", err)
	}
	if len(identities) == 0 {
		return nil, fmt.Errorf("no age identities found in %s", keyFile)
	}
	x25519, ok := identities[0].(*age.X25519Identity)
	if !ok {
		return nil, fmt.Errorf("expected X25519 identity in %s", keyFile)
	}
	return &AgeEncryptionProvider{KeyFile: keyFile, identity: x25519}, nil
}

func (p *AgeEncryptionProvider) Name() string { return "age" }

func (p *AgeEncryptionProvider) Encrypt(_ context.Context, plaintext []byte) ([]byte, error) {
	recipient := p.identity.Recipient()
	var buf bytes.Buffer
	w, err := age.Encrypt(&buf, recipient)
	if err != nil {
		return nil, fmt.Errorf("%w: age encrypt: %v", ErrEncryptionFailure, err)
	}
	if _, err := w.Write(plaintext); err != nil {
		return nil, fmt.Errorf("%w: age write: %v", ErrEncryptionFailure, err)
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("%w: age close: %v", ErrEncryptionFailure, err)
	}
	return buf.Bytes(), nil
}

func (p *AgeEncryptionProvider) Decrypt(_ context.Context, ciphertext []byte) ([]byte, error) {
	r, err := age.Decrypt(bytes.NewReader(ciphertext), p.identity)
	if err != nil {
		return nil, fmt.Errorf("%w: age decrypt: %v", ErrEncryptionFailure, err)
	}
	return io.ReadAll(r)
}

// ─── SopsEncryptionProvider ──────────────────────────────────────────────────

// SopsEncryptionProvider delegates encrypt/decrypt to the sops binary.
type SopsEncryptionProvider struct {
	SopsConfigPath string
}

func (p *SopsEncryptionProvider) Name() string { return "sops" }

func (p *SopsEncryptionProvider) Encrypt(ctx context.Context, plaintext []byte) ([]byte, error) {
	args := []string{"--encrypt", "--input-type", "json", "--output-type", "json"}
	if p.SopsConfigPath != "" {
		args = append(args, "--config", p.SopsConfigPath)
	}
	args = append(args, "/dev/stdin")
	cmd := exec.CommandContext(ctx, "sops", args...)
	cmd.Stdin = bytes.NewReader(plaintext)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%w: sops encrypt: %v", ErrEncryptionFailure, err)
	}
	return out, nil
}

func (p *SopsEncryptionProvider) Decrypt(ctx context.Context, ciphertext []byte) ([]byte, error) {
	args := []string{"--decrypt", "--input-type", "json", "--output-type", "json"}
	if p.SopsConfigPath != "" {
		args = append(args, "--config", p.SopsConfigPath)
	}
	args = append(args, "/dev/stdin")
	cmd := exec.CommandContext(ctx, "sops", args...)
	cmd.Stdin = bytes.NewReader(ciphertext)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%w: sops decrypt: %v", ErrEncryptionFailure, err)
	}
	return out, nil
}

// ─── PassthroughEncryptionProvider ──────────────────────────────────────────

// PassthroughEncryptionProvider performs no encryption. For dev/test only.
type PassthroughEncryptionProvider struct{}

func (p *PassthroughEncryptionProvider) Name() string { return "passthrough" }

func (p *PassthroughEncryptionProvider) Encrypt(_ context.Context, plaintext []byte) ([]byte, error) {
	return plaintext, nil
}

func (p *PassthroughEncryptionProvider) Decrypt(_ context.Context, ciphertext []byte) ([]byte, error) {
	return ciphertext, nil
}
