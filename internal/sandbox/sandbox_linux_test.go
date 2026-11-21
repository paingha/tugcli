//go:build linux

package sandbox

import (
	"slices"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestHandledFS(t *testing.T) {
	tests := []struct {
		abi       int
		has, lack uint64
	}{
		{1, unix.LANDLOCK_ACCESS_FS_EXECUTE | unix.LANDLOCK_ACCESS_FS_MAKE_SYM, unix.LANDLOCK_ACCESS_FS_REFER | unix.LANDLOCK_ACCESS_FS_TRUNCATE},
		{2, unix.LANDLOCK_ACCESS_FS_REFER, unix.LANDLOCK_ACCESS_FS_TRUNCATE},
		{3, unix.LANDLOCK_ACCESS_FS_TRUNCATE, unix.LANDLOCK_ACCESS_FS_IOCTL_DEV},
		{5, unix.LANDLOCK_ACCESS_FS_IOCTL_DEV, unix.LANDLOCK_ACCESS_FS_RESOLVE_UNIX},
		{9, unix.LANDLOCK_ACCESS_FS_IOCTL_DEV, unix.LANDLOCK_ACCESS_FS_RESOLVE_UNIX},
	}
	for _, tt := range tests {
		h := handledFS(tt.abi)
		if h&tt.has != tt.has || h&tt.lack != 0 {
			t.Errorf("handledFS(%d) = %#x, want %#x set and %#x clear", tt.abi, h, tt.has, tt.lack)
		}
	}
}

func TestSeccompTableAssembles(t *testing.T) {
	table, ok := seccompTable()
	if !ok {
		t.Skip("no seccomp table for this architecture")
	}
	sorted := slices.Sorted(slices.Values(table.deny))
	if len(slices.Compact(sorted)) != len(table.deny) {
		t.Errorf("deny list has repeats: %v", table.deny)
	}
	for _, allowUnix := range []bool{false, true} {
		if _, err := filter(table, allowUnix); err != nil {
			t.Errorf("filter(allowUnix=%v): %v", allowUnix, err)
		}
	}
}

func TestExecNeedsThePolicy(t *testing.T) {
	t.Setenv(policyEnv, "")
	if err := Exec(nil); err == nil {
		t.Error("Exec with nothing to run succeeded")
	}
	if err := Exec([]string{"/bin/true"}); err == nil || !strings.Contains(err.Error(), "reading the sandbox policy") {
		t.Errorf("Exec with an empty policy = %v, want it refused", err)
	}
}

func TestCheckDescribesLinux(t *testing.T) {
	st := Check()
	if st.Files && !strings.Contains(st.Mechanism, "Landlock ABI") {
		t.Errorf("Check() = %+v, want the Landlock ABI named", st)
	}
	if !st.Network && st.Missing == "" {
		t.Errorf("Check() = %+v, want the missing network rules explained", st)
	}
}
