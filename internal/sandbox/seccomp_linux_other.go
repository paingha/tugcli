//go:build linux && !amd64 && !arm64

package sandbox

// seccompTable reports that there is no filter for this architecture; tug
// releases are built for amd64 and arm64 only.
func seccompTable() (syscallTable, bool) { return syscallTable{}, false }
