// Package filelock takes exclusive advisory locks on files, so tug processes
// that change the same state take turns. The operating system releases a lock
// when the process holding it exits, so a crash never leaves one behind.
package filelock

import (
	"errors"
	"os"
)

// ErrLocked is returned by TryLock when another holder has the lock.
var ErrLocked = errors.New("locked by another process")

// Lock waits for an exclusive lock on path, creating the file if needed, and
// returns the function that releases it. Locks on the same file conflict
// within one process too, so goroutines can use them as well.
func Lock(path string) (unlock func(), err error) { return lock(path, true) }

// TryLock is Lock without waiting: it returns ErrLocked when the lock is
// held.
func TryLock(path string) (unlock func(), err error) { return lock(path, false) }

func lock(path string, wait bool) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := lockFile(f, wait); err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		unlockFile(f)
		f.Close()
	}, nil
}
