//go:build linux && amd64

package sandbox

import (
	"slices"

	"golang.org/x/sys/unix"
)

// seccompTable returns the filter's table for amd64, which also refuses
// port I/O, uselib and the x32 ABI.
func seccompTable() (syscallTable, bool) {
	return syscallTable{
		arch:   unix.AUDIT_ARCH_X86_64,
		x32:    true,
		deny:   append(slices.Clone(deniedSyscalls), unix.SYS_IOPL, unix.SYS_IOPERM, unix.SYS_USELIB),
		clone:  unix.SYS_CLONE,
		clone3: unix.SYS_CLONE3,
		socket: unix.SYS_SOCKET,
	}, true
}
