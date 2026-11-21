//go:build linux

package sandbox

import (
	"errors"
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

// deniedSyscalls are the system calls a plugin has no use for and that
// would let it reach past its sandbox or the kernel: debugging and reading
// other processes, mounts and namespaces, kernel modules and kexec, BPF,
// perf, io_uring and userfaultfd (large kernel attack surfaces), the kernel
// keyrings, file handles, and setting the clock. The architecture files add
// their own.
var deniedSyscalls = []uint32{
	unix.SYS_PTRACE, unix.SYS_PROCESS_VM_READV, unix.SYS_PROCESS_VM_WRITEV, unix.SYS_PIDFD_GETFD, unix.SYS_PROCESS_MADVISE,
	unix.SYS_MOUNT, unix.SYS_UMOUNT2, unix.SYS_PIVOT_ROOT, unix.SYS_CHROOT, unix.SYS_MOUNT_SETATTR, unix.SYS_MOVE_MOUNT,
	unix.SYS_OPEN_TREE, unix.SYS_FSOPEN, unix.SYS_FSCONFIG, unix.SYS_FSMOUNT, unix.SYS_FSPICK,
	unix.SYS_UNSHARE, unix.SYS_SETNS,
	unix.SYS_INIT_MODULE, unix.SYS_FINIT_MODULE, unix.SYS_DELETE_MODULE, unix.SYS_KEXEC_LOAD, unix.SYS_KEXEC_FILE_LOAD,
	unix.SYS_BPF, unix.SYS_PERF_EVENT_OPEN, unix.SYS_IO_URING_SETUP, unix.SYS_IO_URING_ENTER, unix.SYS_IO_URING_REGISTER,
	unix.SYS_USERFAULTFD,
	unix.SYS_KEYCTL, unix.SYS_ADD_KEY, unix.SYS_REQUEST_KEY,
	unix.SYS_OPEN_BY_HANDLE_AT, unix.SYS_LOOKUP_DCOOKIE, unix.SYS_QUOTACTL,
	unix.SYS_REBOOT, unix.SYS_SWAPON, unix.SYS_SWAPOFF, unix.SYS_ACCT, unix.SYS_SYSLOG, unix.SYS_VHANGUP,
	unix.SYS_SETTIMEOFDAY, unix.SYS_CLOCK_SETTIME, unix.SYS_CLOCK_ADJTIME, unix.SYS_ADJTIMEX,
}

// applySeccomp installs the filter on this thread. allowUnix lets the
// plugin create Unix sockets, which its policy names; Landlock cannot tell
// one socket from another, so otherwise it may create none, and cannot
// reach the session bus or another program's socket. A kernel without
// seccomp filters, or an architecture without a table, gets none; tug
// logged that before it started the plugin.
func applySeccomp(allowUnix bool) error {
	t, ok := seccompTable()
	if !ok {
		return nil
	}
	prog, err := filter(t, allowUnix)
	if err != nil {
		return err
	}
	insts := make([]unix.SockFilter, len(prog))
	for i, in := range prog {
		insts[i] = unix.SockFilter{Code: in.Op, Jt: in.Jt, Jf: in.Jf, K: in.K}
	}
	fprog := unix.SockFprog{Len: uint16(len(insts)), Filter: &insts[0]}
	_, _, errno := unix.Syscall(unix.SYS_SECCOMP, unix.SECCOMP_SET_MODE_FILTER, 0, uintptr(unsafe.Pointer(&fprog)))
	runtime.KeepAlive(insts)
	switch {
	case errno == 0:
		return nil
	case errors.Is(errno, unix.EINVAL) || errors.Is(errno, unix.ENOSYS):
		return nil // no seccomp filters in this kernel
	default:
		return fmt.Errorf("loading the filter: %w", errno)
	}
}
