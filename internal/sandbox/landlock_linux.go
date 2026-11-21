//go:build linux

package sandbox

import (
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Landlock's rule types and its net rule, which x/sys/unix does not define.
const (
	landlockRulePathBeneath = 1
	landlockRuleNetPort     = 2
)

type landlockNetPortAttr struct {
	allowedAccess uint64
	port          uint64
}

// landlockRulesetAttr is struct landlock_ruleset_attr up to ABI 6. The
// kernel accepts a struct longer than its own as long as the fields it does
// not know are zero, which handledFS and scoped make sure of.
type landlockRulesetAttr struct {
	handledAccessFS  uint64
	handledAccessNet uint64
	scoped           uint64
}

// File system rights in groups. Rights a kernel's ABI does not have are
// masked out by handledFS.
const (
	fsRead  = unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_READ_DIR
	fsExec  = fsRead | unix.LANDLOCK_ACCESS_FS_EXECUTE
	fsWrite = fsRead | unix.LANDLOCK_ACCESS_FS_WRITE_FILE | unix.LANDLOCK_ACCESS_FS_REMOVE_DIR |
		unix.LANDLOCK_ACCESS_FS_REMOVE_FILE | unix.LANDLOCK_ACCESS_FS_MAKE_DIR | unix.LANDLOCK_ACCESS_FS_MAKE_REG |
		unix.LANDLOCK_ACCESS_FS_MAKE_SOCK | unix.LANDLOCK_ACCESS_FS_MAKE_FIFO | unix.LANDLOCK_ACCESS_FS_MAKE_SYM |
		unix.LANDLOCK_ACCESS_FS_REFER | unix.LANDLOCK_ACCESS_FS_TRUNCATE
	// fsFile are the rights a rule on a file, rather than a directory, may
	// grant.
	fsFile = unix.LANDLOCK_ACCESS_FS_EXECUTE | unix.LANDLOCK_ACCESS_FS_WRITE_FILE | unix.LANDLOCK_ACCESS_FS_READ_FILE |
		unix.LANDLOCK_ACCESS_FS_TRUNCATE | unix.LANDLOCK_ACCESS_FS_IOCTL_DEV
)

// landlockABI returns the Landlock ABI version the kernel offers, or 0 when
// it has none: too old, built without it, turned off at boot, or refused by
// a container's own seccomp filter.
func landlockABI() int {
	v, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, unix.LANDLOCK_CREATE_RULESET_VERSION)
	if errno != 0 {
		return 0
	}
	return int(v)
}

// handledFS returns the file system rights ABI abi can restrict, all of
// which the ruleset handles, so whatever a rule does not grant is refused.
func handledFS(abi int) uint64 {
	h := uint64(unix.LANDLOCK_ACCESS_FS_EXECUTE | unix.LANDLOCK_ACCESS_FS_WRITE_FILE | unix.LANDLOCK_ACCESS_FS_READ_FILE |
		unix.LANDLOCK_ACCESS_FS_READ_DIR | unix.LANDLOCK_ACCESS_FS_REMOVE_DIR | unix.LANDLOCK_ACCESS_FS_REMOVE_FILE |
		unix.LANDLOCK_ACCESS_FS_MAKE_CHAR | unix.LANDLOCK_ACCESS_FS_MAKE_DIR | unix.LANDLOCK_ACCESS_FS_MAKE_REG |
		unix.LANDLOCK_ACCESS_FS_MAKE_SOCK | unix.LANDLOCK_ACCESS_FS_MAKE_FIFO | unix.LANDLOCK_ACCESS_FS_MAKE_BLOCK |
		unix.LANDLOCK_ACCESS_FS_MAKE_SYM)
	if abi >= 2 {
		h |= unix.LANDLOCK_ACCESS_FS_REFER
	}
	if abi >= 3 {
		h |= unix.LANDLOCK_ACCESS_FS_TRUNCATE
	}
	if abi >= 5 {
		h |= unix.LANDLOCK_ACCESS_FS_IOCTL_DEV
	}
	return h
}

// applyLandlock restricts this thread to the files and TCP ports in r. On
// a kernel without Landlock it does nothing; tug logged that before it
// started the plugin.
func applyLandlock(r rules) error {
	abi := landlockABI()
	if abi < 1 {
		return nil
	}
	attr := landlockRulesetAttr{handledAccessFS: handledFS(abi)}
	if abi >= 4 {
		attr.handledAccessNet = unix.LANDLOCK_ACCESS_NET_BIND_TCP | unix.LANDLOCK_ACCESS_NET_CONNECT_TCP
	}
	if abi >= 6 {
		// Nor may the plugin signal processes outside its sandbox, or reach
		// abstract Unix sockets made outside it.
		attr.scoped = unix.LANDLOCK_SCOPE_SIGNAL | unix.LANDLOCK_SCOPE_ABSTRACT_UNIX_SOCKET
	}
	fd, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, uintptr(unsafe.Pointer(&attr)), unsafe.Sizeof(attr), 0)
	if errno != 0 {
		return fmt.Errorf("creating a ruleset: %w", errno)
	}
	ruleset := int(fd)
	defer unix.Close(ruleset)

	for _, set := range []struct {
		paths  []string
		access uint64
	}{
		{r.Programs, fsExec},
		{r.ProgramDirs, fsExec},
		{r.Libraries, fsExec},
		{r.Read, fsRead},
		{r.Write, fsWrite},
	} {
		for _, path := range set.paths {
			if err := allowPath(ruleset, path, set.access&attr.handledAccessFS); err != nil {
				return err
			}
		}
	}
	if attr.handledAccessNet != 0 {
		for _, port := range r.Ports {
			// No rule allows binding, so the plugin cannot listen on TCP.
			rule := landlockNetPortAttr{allowedAccess: unix.LANDLOCK_ACCESS_NET_CONNECT_TCP, port: uint64(port)}
			if _, _, errno := unix.Syscall6(unix.SYS_LANDLOCK_ADD_RULE, uintptr(ruleset), landlockRuleNetPort, uintptr(unsafe.Pointer(&rule)), 0, 0, 0); errno != 0 {
				return fmt.Errorf("allowing TCP port %d: %w", port, errno)
			}
		}
	}
	if _, _, errno := unix.Syscall(unix.SYS_LANDLOCK_RESTRICT_SELF, uintptr(ruleset), 0, 0); errno != 0 {
		return fmt.Errorf("restricting the process: %w", errno)
	}
	return nil
}

// allowPath grants access beneath path. A path that does not exist, or that
// this user cannot reach anyway, needs no rule.
func allowPath(ruleset int, path string, access uint64) error {
	fd, err := unix.Open(path, unix.O_PATH|unix.O_CLOEXEC, 0)
	switch {
	case errors.Is(err, unix.ENOENT), errors.Is(err, unix.ENOTDIR), errors.Is(err, unix.EACCES), errors.Is(err, unix.ELOOP):
		return nil
	case err != nil:
		return fmt.Errorf("opening %s: %w", path, err)
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return fmt.Errorf("checking %s: %w", path, err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		access &= fsFile
	}
	if access == 0 {
		return nil
	}
	rule := unix.LandlockPathBeneathAttr{Allowed_access: access, Parent_fd: int32(fd)}
	if _, _, errno := unix.Syscall6(unix.SYS_LANDLOCK_ADD_RULE, uintptr(ruleset), landlockRulePathBeneath, uintptr(unsafe.Pointer(&rule)), 0, 0, 0); errno != 0 {
		return fmt.Errorf("allowing %s: %w", path, errno)
	}
	return nil
}
