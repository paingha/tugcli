package config

import "os"

// EnsurePrivateDir creates dir for this user only (0o700), or checks that an
// existing one is private with CheckPrivateDir. tug's runtime directory may
// be in a shared temp directory (/tmp/tug-<uid> when $XDG_RUNTIME_DIR is
// unset), where another user could create it first and then replace the
// session kubeconfigs, tugd's socket or its log file inside it.
func EnsurePrivateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return CheckPrivateDir(dir)
}
