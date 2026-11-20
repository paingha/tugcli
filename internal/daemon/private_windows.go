//go:build windows

package daemon

import (
	"fmt"
	"os"
)

// checkPrivateDir makes sure dir is a real directory, not a link. On Windows
// tug's runtime directory and the temp directory are in the user's own
// profile, which other users cannot write to.
func checkPrivateDir(dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("refusing to use %s for tugd: it is not a directory", dir)
	}
	return nil
}
