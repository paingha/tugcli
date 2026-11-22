package credentials

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRetry(t *testing.T) {
	busy := errors.New("busy")
	isBusy := func(err error) bool { return errors.Is(err, busy) }
	tests := []struct {
		name  string
		fails []error // what each call returns before it succeeds
		wait  time.Duration
		want  error
		calls int
	}{
		{name: "succeeds at once", calls: 1},
		{name: "a busy file is tried again", fails: []error{busy, busy}, wait: time.Second, calls: 3},
		{name: "another error is returned at once", fails: []error{os.ErrPermission}, wait: time.Second, want: os.ErrPermission, calls: 1},
		{name: "until the wait is over", fails: []error{busy, busy, busy, busy, busy}, wait: 0, want: busy, calls: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			err := retry(func() error {
				calls++
				if calls <= len(tt.fails) {
					return tt.fails[calls-1]
				}
				return nil
			}, isBusy, tt.wait)
			if !errors.Is(err, tt.want) || (tt.want == nil && err != nil) || calls != tt.calls {
				t.Errorf("retry = %v after %d calls, want %v after %d", err, calls, tt.want, tt.calls)
			}
		})
	}
	if transient(nil) || transient(os.ErrNotExist) || transient(os.ErrExist) {
		t.Error("transient accepts an error that is not a busy file")
	}
}

func TestLockFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "secrets.enc.lock")
	unlock, err := lockFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// A second holder waits until the first lets go.
	got := make(chan error, 1)
	go func() {
		unlock2, err := lockFile(path)
		if err == nil {
			unlock2()
		}
		got <- err
	}()
	select {
	case err := <-got:
		t.Fatalf("lockFile returned %v while the lock was held", err)
	case <-time.After(100 * time.Millisecond):
	}
	unlock()
	if err := <-got; err != nil {
		t.Fatalf("lockFile after unlock: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("lock file left after unlock: %v", err)
	}

	// A lock left behind by a holder that died is taken over.
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Minute)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	unlock, err = lockFile(path)
	if err != nil {
		t.Fatalf("lockFile over an abandoned lock: %v", err)
	}
	unlock()

	// An error that is not about the lock is returned at once.
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := lockFile(filepath.Join(file, "x.lock")); err == nil || time.Since(start) > time.Second {
		t.Errorf("lockFile under a file = %v after %v, want an error at once", err, time.Since(start))
	}
}

// TestLockWaitsWhileItIsReleased makes creating the lock fail as Windows
// does while the previous holder is still removing it, and checks that the
// lock is taken once that passes rather than failing.
func TestLockWaitsWhileItIsReleased(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.enc.lock")
	busy := errors.New("access is denied")
	calls := 0
	create := func(p string) error {
		calls++
		if calls <= 2 {
			return &os.PathError{Op: "open", Path: p, Err: busy}
		}
		return createExclusive(p)
	}
	unlock, err := takeLock(path, create, func(err error) bool { return errors.Is(err, busy) })
	if err != nil {
		t.Fatalf("takeLock while the lock was being released: %v", err)
	}
	unlock()
	if calls != 3 {
		t.Errorf("takeLock tried %d times, want 3", calls)
	}

	// Without such an error, it fails at once, as before.
	calls = 0
	if _, err := takeLock(path, create, func(error) bool { return false }); err == nil || calls != 1 {
		t.Errorf("takeLock with a refusal that is not transient = %v after %d tries, want an error after 1", err, calls)
	}
}

// TestFileStoreConcurrentWriters has two stores on one file, as two tug
// processes or tugd and a command would, write and read it at once. On
// Windows this is where a holder releasing the lock, or replacing the file,
// used to make another fail with "Access is denied".
func TestFileStoreConcurrentWriters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.enc")
	stores := []*FileStore{NewFileStore(path, "correct horse"), NewFileStore(path, "correct horse")}
	if err := stores[0].Set("seed", "x"); err != nil {
		t.Fatal(err)
	}
	const writers, writes = 8, 10
	var wg sync.WaitGroup
	for w := range writers {
		s := stores[w%len(stores)]
		wg.Go(func() {
			for i := range writes {
				key := fmt.Sprintf("w%d/%d", w, i)
				if err := s.Set(key, key); err != nil {
					t.Errorf("Set(%s): %v", key, err)
					return
				}
				if v, err := s.Get(key); err != nil || v != key {
					t.Errorf("Get(%s) = %q, %v", key, v, err)
					return
				}
			}
		})
	}
	wg.Wait()
	m, _, err := NewFileStore(path, "correct horse").load()
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != writers*writes+1 {
		var missing []string
		for w := range writers {
			for i := range writes {
				if k := fmt.Sprintf("w%d/%d", w, i); m[k] != k {
					missing = append(missing, k)
				}
			}
		}
		t.Errorf("the store has %d keys, want %d; missing %s", len(m), writers*writes+1, strings.Join(missing, ", "))
	}
	if _, err := os.Stat(path + ".lock"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("lock file left behind: %v", err)
	}
}
