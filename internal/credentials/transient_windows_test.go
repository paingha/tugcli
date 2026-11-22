//go:build windows

package credentials

import (
	"fmt"
	"os"
	"testing"

	"golang.org/x/sys/windows"
)

func TestTransientOnWindows(t *testing.T) {
	for err, want := range map[error]bool{
		&os.PathError{Op: "open", Path: "secrets.enc.lock", Err: windows.ERROR_ACCESS_DENIED}:   true,
		&os.LinkError{Op: "rename", Old: "a", New: "b", Err: windows.ERROR_SHARING_VIOLATION}:   true,
		fmt.Errorf("reading: %w", &os.PathError{Op: "read", Err: windows.ERROR_LOCK_VIOLATION}): true,
		&os.PathError{Op: "open", Path: "secrets.enc", Err: windows.ERROR_FILE_NOT_FOUND}:       false,
		&os.PathError{Op: "open", Path: "secrets.enc.lock", Err: windows.ERROR_FILE_EXISTS}:     false,
	} {
		if got := transient(err); got != want {
			t.Errorf("transient(%v) = %v, want %v", err, got, want)
		}
	}
}
