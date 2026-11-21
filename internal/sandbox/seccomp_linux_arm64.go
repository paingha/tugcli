//go:build linux && arm64

package sandbox

import "golang.org/x/sys/unix"

// seccompTable returns the filter's table for arm64.
func seccompTable() (syscallTable, bool) {
	return syscallTable{
		arch:   unix.AUDIT_ARCH_AARCH64,
		deny:   deniedSyscalls,
		clone:  unix.SYS_CLONE,
		clone3: unix.SYS_CLONE3,
		socket: unix.SYS_SOCKET,
	}, true
}
