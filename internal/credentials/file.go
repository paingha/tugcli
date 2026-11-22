package credentials

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/crypto/scrypt"

	"github.com/paingha/tugcli/internal/config"
)

// FileStore keeps secrets in one file encrypted with AES-256-GCM under a key
// derived from a passphrase with scrypt. It is for CI and machines without a
// keychain.
type FileStore struct {
	path       string
	passphrase string

	// cached key for the salt it was derived from; keyMu guards both, since
	// tugd reads credentials from concurrent proxy requests
	keyMu sync.Mutex
	salt  []byte
	key   []byte
}

// NewFileStore returns a store backed by the encrypted file at path.
func NewFileStore(path, passphrase string) *FileStore {
	return &FileStore{path: path, passphrase: passphrase}
}

// Name implements Store.
func (f *FileStore) Name() string { return "file:" + f.path }

const (
	scryptN = 1 << 15
	scryptR = 8
	scryptP = 1
)

var _ Store = (*FileStore)(nil)

type fileEnvelope struct {
	Version int    `json:"version"`
	KDF     string `json:"kdf"`
	N       int    `json:"n"`
	R       int    `json:"r"`
	P       int    `json:"p"`
	Salt    []byte `json:"salt"`
	Nonce   []byte `json:"nonce"`
	Data    []byte `json:"data"`
}

func (f *FileStore) deriveKey(salt []byte, n, r, p int) ([]byte, error) {
	f.keyMu.Lock()
	defer f.keyMu.Unlock()
	if f.key != nil && string(f.salt) == string(salt) {
		return f.key, nil
	}
	key, err := scrypt.Key([]byte(f.passphrase), salt, n, r, p, 32)
	if err != nil {
		return nil, err
	}
	f.salt, f.key = append([]byte(nil), salt...), key
	return key, nil
}

func (f *FileStore) load() (map[string]string, []byte, error) {
	var b []byte
	err := retry(func() (err error) {
		b, err = os.ReadFile(f.path)
		return err
	}, transient, busyWait)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	var env fileEnvelope
	if err := json.Unmarshal(b, &env); err != nil {
		return nil, nil, fmt.Errorf("%s is corrupt: %w", f.path, err)
	}
	if env.Version != 1 || env.KDF != "scrypt" {
		return nil, nil, fmt.Errorf("%s: unsupported format version %d/%s", f.path, env.Version, env.KDF)
	}
	key, err := f.deriveKey(env.Salt, env.N, env.R, env.P)
	if err != nil {
		return nil, nil, err
	}
	gcm, err := newGCM(key)
	if err != nil {
		return nil, nil, err
	}
	plain, err := gcm.Open(nil, env.Nonce, env.Data, []byte(f.path))
	if err != nil {
		return nil, nil, fmt.Errorf("cannot decrypt %s: wrong %s?", f.path, PassphraseEnv)
	}
	m := map[string]string{}
	if err := json.Unmarshal(plain, &m); err != nil {
		return nil, nil, fmt.Errorf("%s: %w", f.path, err)
	}
	return m, env.Salt, nil
}

func (f *FileStore) save(m map[string]string, salt []byte) error {
	if salt == nil {
		salt = make([]byte, 16)
		if _, err := rand.Read(salt); err != nil {
			return err
		}
	}
	key, err := f.deriveKey(salt, scryptN, scryptR, scryptP)
	if err != nil {
		return err
	}
	gcm, err := newGCM(key)
	if err != nil {
		return err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	plain, err := json.Marshal(m)
	if err != nil {
		return err
	}
	env := fileEnvelope{
		Version: 1, KDF: "scrypt", N: scryptN, R: scryptR, P: scryptP,
		Salt: salt, Nonce: nonce, Data: gcm.Seal(nil, nonce, plain, []byte(f.path)),
	}
	b, err := json.Marshal(env)
	if err != nil {
		return err
	}
	return retry(func() error { return config.WriteFileAtomic(f.path, b, 0o600) }, transient, busyWait)
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Get implements Store.
func (f *FileStore) Get(key string) (string, error) {
	m, _, err := f.load()
	if err != nil {
		return "", err
	}
	v, ok := m[key]
	if !ok {
		return "", ErrNotFound
	}
	return v, nil
}

// Set implements Store.
func (f *FileStore) Set(key, value string) error {
	return f.update(func(m map[string]string) { m[key] = value })
}

// Delete implements Store.
func (f *FileStore) Delete(key string) error {
	return f.update(func(m map[string]string) { delete(m, key) })
}

func (f *FileStore) update(fn func(map[string]string)) error {
	unlock, err := lockFile(f.path + ".lock")
	if err != nil {
		return err
	}
	defer unlock()
	m, salt, err := f.load()
	if err != nil {
		return err
	}
	fn(m)
	return f.save(m, salt)
}

// How long the store waits: for another holder's lock, before a lock is
// taken to be abandoned, and for a file another holder is busy with.
const (
	lockWait  = 10 * time.Second
	lockStale = 30 * time.Second
	busyWait  = 2 * time.Second
)

// lockFile takes a simple exclusive lock by creating path. A lock older than
// 30 seconds is assumed abandoned. Every version of tug takes the lock this
// way, so a tugd still running from before an upgrade and a newer tug take
// turns.
func lockFile(path string) (func(), error) { return takeLock(path, createExclusive, transient) }

// createExclusive creates path, failing if it exists.
func createExclusive(path string) error {
	fh, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err == nil {
		fh.Close()
	}
	return err
}

// takeLock is lockFile with how the lock file is created and which errors
// mean another holder is busy with it passed in, so tests can make Windows'
// refusals happen anywhere.
func takeLock(path string, create func(string) error, isTransient func(error) bool) (func(), error) {
	deadline := time.Now().Add(lockWait)
	for {
		err := create(path)
		switch {
		case err == nil:
			return func() {
				// Best effort: a lock left behind is taken to be
				// abandoned after lockStale.
				_ = retry(func() error { return os.Remove(path) }, isTransient, busyWait)
			}, nil
		case errors.Is(err, os.ErrNotExist):
			if mkErr := os.MkdirAll(filepath.Dir(path), 0o700); mkErr != nil {
				return nil, mkErr
			}
			continue
		case errors.Is(err, os.ErrExist):
			if st, serr := os.Stat(path); serr == nil && time.Since(st.ModTime()) > lockStale {
				os.Remove(path)
				continue
			}
		case isTransient(err):
			// On Windows, the holder is still removing the lock: it is
			// being released, so wait as for a lock that is held.
		default:
			return nil, err
		}
		if time.Now().After(deadline) {
			if isTransient(err) {
				return nil, fmt.Errorf("taking the lock %s: %w", path, err)
			}
			return nil, fmt.Errorf("timed out waiting for lock %s", path)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// retry runs op until it succeeds, fails with an error isTransient does not
// accept, or wait has passed, and returns its last error.
func retry(op func() error, isTransient func(error) bool, wait time.Duration) error {
	return retryClock(op, isTransient, wait, time.Now)
}

// retryClock is retry with the clock passed in. It stops once the clock
// reaches the deadline, not only once it is past it: Windows' clock ticks
// coarsely enough that straight after a short wait it can still read the
// deadline exactly.
func retryClock(op func() error, isTransient func(error) bool, wait time.Duration, now func() time.Time) error {
	deadline := now().Add(wait)
	for {
		err := op()
		if err == nil || !isTransient(err) || !now().Before(deadline) {
			return err
		}
		time.Sleep(10 * time.Millisecond)
	}
}
