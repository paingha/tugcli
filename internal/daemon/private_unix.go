//go:build !windows

package daemon

import (
	"fmt"
	"os"
	"syscall"
)

// checkPrivateDir makes sure only this user can change what is in dir: a
// real directory, not a link, owned by this user and writable by no one else.
// In a shared temp directory another user could otherwise create it first,
// or replace tugd's socket with their own. The files in it carry their own
// 0o600 modes, so others reading the directory is harmless.
func checkPrivateDir(dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("refusing to use %s for tugd: it is not a directory", dir)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return fmt.Errorf("refusing to use %s for tugd: it belongs to another user; set XDG_RUNTIME_DIR or TUG_HOME to a directory of your own", dir)
	}
	if perm := fi.Mode().Perm(); perm&0o022 != 0 {
		return fmt.Errorf("refusing to use %s for tugd: other users can write to it (mode %#o); run `chmod 700 %s`", dir, perm, dir)
	}
	return nil
}
