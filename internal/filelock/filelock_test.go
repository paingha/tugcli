package filelock

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestTryLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	unlock, err := TryLock(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := TryLock(path); !errors.Is(err, ErrLocked) {
		t.Fatalf("TryLock while held = %v, want ErrLocked", err)
	}
	unlock()
	again, err := TryLock(path)
	if err != nil {
		t.Fatalf("TryLock after unlock = %v", err)
	}
	again()
}

func TestLockWaits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	unlock, err := Lock(path)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan error, 1)
	go func() {
		u, err := Lock(path)
		if err == nil {
			u()
		}
		got <- err
	}()
	select {
	case err := <-got:
		t.Fatalf("Lock returned while the lock was held: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	unlock()
	select {
	case err := <-got:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Lock did not return after the holder unlocked")
	}
}
