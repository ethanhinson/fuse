package secrets

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"

	"filippo.io/age"
	"github.com/fsnotify/fsnotify"
)

// Sentinel errors for secrets operations.
var (
	ErrSecretNotFound      = errors.New("secret not found")
	ErrSecretsStoreFailure = errors.New("secrets store failure")
	ErrEncryptionFailure   = errors.New("encryption failure")
)

// ContainerSecrets is the result of ExportForContainer.
type ContainerSecrets struct {
	// Env holds resolved plaintext values when no public key is set.
	Env map[string]string
	// EncryptedBundle holds an age-encrypted bundle when a public key is set.
	EncryptedBundle []byte
	// BundleAlgorithm is "age" when EncryptedBundle is set, "" otherwise.
	BundleAlgorithm string
}

// SecretsStore resolves named secrets and exports them for container injection.
type SecretsStore interface {
	// Get returns the plaintext value for the named secret.
	Get(ctx context.Context, name string) (string, error)
	// List returns all known secret names (values not returned).
	List(ctx context.Context) ([]string, error)
	// ExportForContainer resolves named secrets for container injection.
	// If publicKey is non-empty (an age X25519 public key), the export is
	// age-encrypted; otherwise plaintext Env is returned.
	ExportForContainer(ctx context.Context, names []string, publicKey string) (*ContainerSecrets, error)
}

// exportForContainer is a shared helper used by all store implementations.
func exportForContainer(ctx context.Context, store SecretsStore, names []string, publicKey string) (*ContainerSecrets, error) {
	env := make(map[string]string, len(names))
	for _, name := range names {
		val, err := store.Get(ctx, name)
		if err != nil {
			return nil, err
		}
		env[name] = val
	}

	if publicKey == "" {
		return &ContainerSecrets{Env: env}, nil
	}

	// Encrypt with age.
	recipient, err := age.ParseX25519Recipient(publicKey)
	if err != nil {
		return nil, fmt.Errorf("%w: parse age recipient: %v", ErrEncryptionFailure, err)
	}
	plaintext, err := json.Marshal(env)
	if err != nil {
		return nil, fmt.Errorf("%w: marshal secrets: %v", ErrEncryptionFailure, err)
	}
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
	return &ContainerSecrets{
		EncryptedBundle: buf.Bytes(),
		BundleAlgorithm: "age",
	}, nil
}

// ─── EnvSecretsStore ─────────────────────────────────────────────────────────

// EnvSecretsStore reads secrets from environment variables.
// Secret names map 1:1 to env var names (case-sensitive).
type EnvSecretsStore struct{}

func (s *EnvSecretsStore) Get(_ context.Context, name string) (string, error) {
	val, ok := os.LookupEnv(name)
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrSecretNotFound, name)
	}
	return val, nil
}

func (s *EnvSecretsStore) List(_ context.Context) ([]string, error) {
	env := os.Environ()
	names := make([]string, 0, len(env))
	for _, e := range env {
		idx := strings.IndexByte(e, '=')
		if idx > 0 {
			names = append(names, e[:idx])
		}
	}
	return names, nil
}

func (s *EnvSecretsStore) ExportForContainer(ctx context.Context, names []string, publicKey string) (*ContainerSecrets, error) {
	return exportForContainer(ctx, s, names, publicKey)
}

// ─── SopsSecretsStore ────────────────────────────────────────────────────────

// SopsSecretsStore reads from a SOPS-encrypted secrets file. The decrypted
// map is cached for the session; an fsnotify watcher invalidates the cache
// when the file changes.
type SopsSecretsStore struct {
	FilePath       string
	SopsConfigPath string

	cacheMu sync.RWMutex
	cache   map[string]string
	watcher *fsnotify.Watcher
}

// NewSopsSecretsStore creates a SopsSecretsStore with an fsnotify watcher.
func NewSopsSecretsStore(filePath, sopsConfigPath string) (*SopsSecretsStore, error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("fsnotify: %w", err)
	}
	if err := w.Add(filePath); err != nil {
		w.Close()
		return nil, fmt.Errorf("fsnotify watch %s: %w", filePath, err)
	}
	s := &SopsSecretsStore{
		FilePath:       filePath,
		SopsConfigPath: sopsConfigPath,
		watcher:        w,
	}
	go s.watchLoop()
	return s, nil
}

func (s *SopsSecretsStore) watchLoop() {
	for {
		select {
		case _, ok := <-s.watcher.Events:
			if !ok {
				return
			}
			s.Reload()
		case _, ok := <-s.watcher.Errors:
			if !ok {
				return
			}
		}
	}
}

// Reload clears the in-memory cache so the next Get re-decrypts from disk.
func (s *SopsSecretsStore) Reload() {
	s.cacheMu.Lock()
	s.cache = nil
	s.cacheMu.Unlock()
}

// Close stops the fsnotify watcher.
func (s *SopsSecretsStore) Close() error { return s.watcher.Close() }

func (s *SopsSecretsStore) load(ctx context.Context) (map[string]string, error) {
	s.cacheMu.RLock()
	if s.cache != nil {
		defer s.cacheMu.RUnlock()
		return s.cache, nil
	}
	s.cacheMu.RUnlock()

	args := []string{"--decrypt", "--output-type", "json"}
	if s.SopsConfigPath != "" {
		args = append(args, "--config", s.SopsConfigPath)
	}
	args = append(args, s.FilePath)
	out, err := exec.CommandContext(ctx, "sops", args...).Output()
	if err != nil {
		return nil, fmt.Errorf("%w: sops decrypt: %v", ErrSecretsStoreFailure, err)
	}
	var m map[string]string
	if err := json.Unmarshal(out, &m); err != nil {
		return nil, fmt.Errorf("%w: parse sops output: %v", ErrSecretsStoreFailure, err)
	}

	s.cacheMu.Lock()
	s.cache = m
	s.cacheMu.Unlock()
	return m, nil
}

func (s *SopsSecretsStore) Get(ctx context.Context, name string) (string, error) {
	m, err := s.load(ctx)
	if err != nil {
		return "", err
	}
	val, ok := m[name]
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrSecretNotFound, name)
	}
	return val, nil
}

func (s *SopsSecretsStore) List(ctx context.Context) ([]string, error) {
	m, err := s.load(ctx)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	return names, nil
}

func (s *SopsSecretsStore) ExportForContainer(ctx context.Context, names []string, publicKey string) (*ContainerSecrets, error) {
	return exportForContainer(ctx, s, names, publicKey)
}

// ─── EncryptedFileSecretsStore ───────────────────────────────────────────────

// EncryptedFileSecretsStore reads from a file encrypted by an EncryptionProvider.
type EncryptedFileSecretsStore struct {
	FilePath string
	Enc      EncryptionProvider
}

func (s *EncryptedFileSecretsStore) load(ctx context.Context) (map[string]string, error) {
	data, err := os.ReadFile(s.FilePath)
	if err != nil {
		return nil, fmt.Errorf("%w: read %s: %v", ErrSecretsStoreFailure, s.FilePath, err)
	}
	plaintext, err := s.Enc.Decrypt(ctx, data)
	if err != nil {
		return nil, fmt.Errorf("%w: decrypt: %v", ErrSecretsStoreFailure, err)
	}
	var m map[string]string
	if err := json.Unmarshal(plaintext, &m); err != nil {
		return nil, fmt.Errorf("%w: parse: %v", ErrSecretsStoreFailure, err)
	}
	return m, nil
}

func (s *EncryptedFileSecretsStore) Get(ctx context.Context, name string) (string, error) {
	m, err := s.load(ctx)
	if err != nil {
		return "", err
	}
	val, ok := m[name]
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrSecretNotFound, name)
	}
	return val, nil
}

func (s *EncryptedFileSecretsStore) List(ctx context.Context) ([]string, error) {
	m, err := s.load(ctx)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	return names, nil
}

func (s *EncryptedFileSecretsStore) ExportForContainer(ctx context.Context, names []string, publicKey string) (*ContainerSecrets, error) {
	return exportForContainer(ctx, s, names, publicKey)
}
