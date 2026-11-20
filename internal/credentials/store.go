// Package credentials keeps secrets in the OS keychain (or an encrypted file
// where there is none), caches credentials, and answers `tug auth token`.
package credentials

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/paingha/tugcli/internal/config"
)

// ErrNotFound is returned when a key has no value.
var ErrNotFound = errors.New("secret not found")

// Store is a small key/value store for secrets.
type Store interface {
	Get(key string) (string, error)
	Set(key, value string) error
	Delete(key string) error
	// Name describes the backend for doctor and error messages.
	Name() string
}

// Key helpers keep the naming in one place.

// CredentialKey holds the stored credential for a catalog cluster.
func CredentialKey(clusterID string) string { return "cred/" + clusterID }

// DelegateKey caches the output of a delegated exec plugin.
func DelegateKey(clusterID string) string { return "deleg/" + clusterID }

// SecretKey holds a provider secret declared by its manifest, per account.
func SecretKey(provider, account, name string) string {
	return "secret/" + provider + "/" + account + "/" + name
}

// Backend names.
const (
	BackendAuto     = "auto"
	BackendKeychain = "keychain"
	BackendFile     = "file"
)

// PassphraseEnv supplies the passphrase for the encrypted file backend.
const PassphraseEnv = "TUG_KEYRING_PASSPHRASE"

// Open returns the store for the configured backend.
func Open(backend string, paths config.Paths) (Store, error) {
	file := func() (Store, error) {
		pass := os.Getenv(PassphraseEnv)
		if pass == "" {
			return nil, fmt.Errorf("the encrypted file keyring needs %s to be set", PassphraseEnv)
		}
		return NewFileStore(filepath.Join(paths.Data, "secrets.enc"), pass), nil
	}
	switch strings.ToLower(backend) {
	case BackendKeychain:
		k := NewKeychainStore()
		if err := k.Probe(); err != nil {
			return nil, fmt.Errorf("OS keychain is not available: %w", err)
		}
		return k, nil
	case BackendFile:
		return file()
	case BackendAuto, "":
		k := NewKeychainStore()
		kerr := k.Probe()
		if kerr == nil {
			return k, nil
		}
		if os.Getenv(PassphraseEnv) != "" {
			return file()
		}
		return nil, fmt.Errorf("no keychain available; set %s to use an encrypted file instead: %w", PassphraseEnv, kerr)
	default:
		return nil, fmt.Errorf("unknown keyring backend %q (want auto, keychain or file)", backend)
	}
}

// MemoryStore is an in-memory Store for tests.
type MemoryStore struct {
	mu sync.Mutex
	m  map[string]string
}

var _ Store = (*MemoryStore)(nil)

// NewMemoryStore returns an empty in-memory store.
func NewMemoryStore() *MemoryStore { return &MemoryStore{m: map[string]string{}} }

// Get implements Store.
func (s *MemoryStore) Get(key string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[key]
	if !ok {
		return "", ErrNotFound
	}
	return v, nil
}

// Set implements Store.
func (s *MemoryStore) Set(key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[key] = value
	return nil
}

// Delete implements Store.
func (s *MemoryStore) Delete(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, key)
	return nil
}

// Name implements Store.
func (s *MemoryStore) Name() string { return "memory" }

// Keys returns the stored keys, sorted. For tests.
func (s *MemoryStore) Keys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.m))
	for k := range s.m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
