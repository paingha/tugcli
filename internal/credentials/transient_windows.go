//go:build windows

package credentials

import (
	"errors"
	"syscall"

	"golang.org/x/sys/windows"
)

// transient reports whether a file operation failed only because another
// holder of the file was busy with it. Windows refuses to create a file that
// another holder is still deleting, and to replace or delete one that
// another holder has open, with these errors; they clear once the other
// holder is done.
func transient(err error) bool {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	switch errno {
	case windows.ERROR_ACCESS_DENIED, windows.ERROR_SHARING_VIOLATION, windows.ERROR_LOCK_VIOLATION:
		return true
	}
	return false
}
