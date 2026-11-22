//go:build !windows

package credentials

// transient reports whether a file operation failed only because another
// holder of the file was busy with it. Unix lets a file be removed, renamed
// or replaced while others have it open, so no error is.
func transient(error) bool { return false }
