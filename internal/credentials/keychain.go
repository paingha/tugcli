package credentials

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/zalando/go-keyring"
)

// Service is the keychain service name for every tug item.
const Service = "tug"

// chunkSize keeps each item under the smallest platform limit: Windows
// Credential Manager takes 2560 bytes, and macOS `security -i` takes a 4096
// byte command line after base64 encoding.
const chunkSize = 1800

const chunkHeader = "tug-chunks:"

// rawKeyring is the subset of go-keyring that KeychainStore uses, so tests can
// substitute a fake.
type rawKeyring interface {
	Get(service, user string) (string, error)
	Set(service, user, password string) error
	Delete(service, user string) error
}

type osKeyring struct{}

func (osKeyring) Get(s, u string) (string, error) { return keyring.Get(s, u) }
func (osKeyring) Set(s, u, p string) error        { return keyring.Set(s, u, p) }
func (osKeyring) Delete(s, u string) error        { return keyring.Delete(s, u) }

// KeychainStore keeps secrets in the macOS Keychain, Windows Credential Manager
// or the Linux Secret Service. Values larger than one item allows are split
// into numbered chunks.
type KeychainStore struct {
	kr rawKeyring
}

// NewKeychainStore returns a store backed by the OS keychain.
func NewKeychainStore() *KeychainStore { return &KeychainStore{kr: osKeyring{}} }

var _ Store = (*KeychainStore)(nil)

// Name implements Store.
func (k *KeychainStore) Name() string { return "keychain" }

// Probe checks that the keychain answers.
func (k *KeychainStore) Probe() error {
	_, err := k.kr.Get(Service, "__probe__")
	if err == nil || errors.Is(err, keyring.ErrNotFound) {
		return nil
	}
	return err
}

func (k *KeychainStore) get(key string) (string, error) {
	v, err := k.kr.Get(Service, key)
	if errors.Is(err, keyring.ErrNotFound) {
		return "", ErrNotFound
	}
	return v, err
}

// Get implements Store, joining chunked values.
func (k *KeychainStore) Get(key string) (string, error) {
	v, err := k.get(key)
	if err != nil {
		return "", err
	}
	n, ok := chunkCount(v)
	if !ok {
		return v, nil
	}
	var b strings.Builder
	for i := 0; i < n; i++ {
		part, err := k.get(chunkKey(key, i))
		if err != nil {
			return "", fmt.Errorf("reading chunk %d of %s: %w", i, key, err)
		}
		b.WriteString(part)
	}
	return b.String(), nil
}

// Set implements Store, chunking values too large for one item.
func (k *KeychainStore) Set(key, value string) error {
	if err := k.deleteChunks(key); err != nil {
		return err
	}
	if len(value) <= chunkSize {
		return k.kr.Set(Service, key, value)
	}
	n := 0
	for start := 0; start < len(value); start += chunkSize {
		end := min(start+chunkSize, len(value))
		if err := k.kr.Set(Service, chunkKey(key, n), value[start:end]); err != nil {
			return err
		}
		n++
	}
	return k.kr.Set(Service, key, chunkHeader+strconv.Itoa(n))
}

// Delete implements Store, removing any chunks.
func (k *KeychainStore) Delete(key string) error {
	if err := k.deleteChunks(key); err != nil {
		return err
	}
	if err := k.kr.Delete(Service, key); err != nil && !errors.Is(err, keyring.ErrNotFound) {
		return err
	}
	return nil
}

func (k *KeychainStore) deleteChunks(key string) error {
	v, err := k.get(key)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	n, ok := chunkCount(v)
	if !ok {
		return nil
	}
	for i := 0; i < n; i++ {
		if err := k.kr.Delete(Service, chunkKey(key, i)); err != nil && !errors.Is(err, keyring.ErrNotFound) {
			return err
		}
	}
	return nil
}

func chunkKey(key string, i int) string { return key + "#" + strconv.Itoa(i) }

func chunkCount(v string) (int, bool) {
	if !strings.HasPrefix(v, chunkHeader) {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimPrefix(v, chunkHeader))
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}
