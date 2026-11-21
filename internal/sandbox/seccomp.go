package sandbox

import (
	"fmt"

	"golang.org/x/net/bpf"
)

// Values from Linux's seccomp.h, sched.h and errno-base.h, which are the
// same on every architecture tug supports. They are spelled out here, not
// taken from x/sys/unix, so that the filter's tests run on every platform.
const (
	seccompRetKillProcess = 0x80000000
	seccompRetErrno       = 0x00050000
	seccompRetAllow       = 0x7fff0000

	errnoEPERM  = 1
	errnoENOSYS = 38

	afUnix = 1

	// x32SyscallBit marks the system calls of amd64's x32 ABI.
	x32SyscallBit = 0x40000000

	// cloneNamespaces are clone's flags that create namespaces: NEWNS,
	// NEWCGROUP, NEWUTS, NEWIPC, NEWUSER, NEWPID and NEWNET.
	cloneNamespaces = 0x00020000 | 0x02000000 | 0x04000000 | 0x08000000 | 0x10000000 | 0x20000000 | 0x40000000
)

// Offsets in struct seccomp_data. The first argument's low 32 bits come
// first on the little-endian machines tug supports.
const (
	offsetNR   = 0
	offsetArch = 4
	offsetArg0 = 16
)

// syscallTable is what the filter needs to know about one architecture.
type syscallTable struct {
	// arch is the AUDIT_ARCH_ value the kernel reports for it.
	arch uint32
	// x32 refuses the x32 ABI's system calls, on amd64.
	x32 bool
	// deny are system calls that fail with EPERM.
	deny []uint32
	// clone fails with EPERM when it is asked for new namespaces.
	clone uint32
	// clone3 fails with ENOSYS, because its flags are behind a pointer the
	// filter cannot read; libc and Go then fall back to clone.
	clone3 uint32
	// socket fails with EPERM for Unix sockets unless the policy allows
	// them.
	socket uint32
}

// insn is an instruction of the filter with jumps to labels instead of
// offsets.
type insn struct {
	label  string
	ins    bpf.Instruction
	jt, jf string // jump targets of a JumpIf; empty means the next one
}

// filter assembles the seccomp program for t: a system call from another
// architecture or ABI kills the process, the calls in t.deny fail with
// EPERM, clone with namespace flags and (unless allowUnix) socket(AF_UNIX)
// fail with EPERM, clone3 fails with ENOSYS, and everything else is allowed.
func filter(t syscallTable, allowUnix bool) ([]bpf.RawInstruction, error) {
	prog := []insn{
		{ins: bpf.LoadAbsolute{Off: offsetArch, Size: 4}},
		{ins: bpf.JumpIf{Cond: bpf.JumpEqual, Val: t.arch}, jf: "kill"},
		{ins: bpf.LoadAbsolute{Off: offsetNR, Size: 4}},
	}
	if t.x32 {
		prog = append(prog, insn{ins: bpf.JumpIf{Cond: bpf.JumpGreaterOrEqual, Val: x32SyscallBit}, jt: "kill"})
	}
	for _, nr := range t.deny {
		prog = append(prog, insn{ins: bpf.JumpIf{Cond: bpf.JumpEqual, Val: nr}, jt: "eperm"})
	}
	prog = append(prog,
		insn{ins: bpf.JumpIf{Cond: bpf.JumpEqual, Val: t.clone3}, jt: "enosys"},
		insn{ins: bpf.JumpIf{Cond: bpf.JumpEqual, Val: t.clone}, jt: "clone"},
	)
	if !allowUnix {
		prog = append(prog, insn{ins: bpf.JumpIf{Cond: bpf.JumpEqual, Val: t.socket}, jt: "socket"})
	}
	prog = append(prog,
		insn{ins: bpf.RetConstant{Val: seccompRetAllow}},
		insn{label: "clone", ins: bpf.LoadAbsolute{Off: offsetArg0, Size: 4}},
		insn{ins: bpf.JumpIf{Cond: bpf.JumpBitsSet, Val: cloneNamespaces}, jt: "eperm"},
		insn{ins: bpf.RetConstant{Val: seccompRetAllow}},
		insn{label: "socket", ins: bpf.LoadAbsolute{Off: offsetArg0, Size: 4}},
		insn{ins: bpf.JumpIf{Cond: bpf.JumpEqual, Val: afUnix}, jt: "eperm"},
		insn{ins: bpf.RetConstant{Val: seccompRetAllow}},
		insn{label: "eperm", ins: bpf.RetConstant{Val: seccompRetErrno | errnoEPERM}},
		insn{label: "enosys", ins: bpf.RetConstant{Val: seccompRetErrno | errnoENOSYS}},
		insn{label: "kill", ins: bpf.RetConstant{Val: seccompRetKillProcess}},
	)
	insts, err := link(prog)
	if err != nil {
		return nil, err
	}
	return bpf.Assemble(insts)
}

// link turns label jumps into the relative skips classic BPF uses.
func link(prog []insn) ([]bpf.Instruction, error) {
	at := map[string]int{}
	for i, in := range prog {
		if in.label != "" {
			at[in.label] = i
		}
	}
	skip := func(i int, label string) (uint8, error) {
		if label == "" {
			return 0, nil
		}
		to, ok := at[label]
		if !ok {
			return 0, fmt.Errorf("seccomp filter: no instruction labelled %s", label)
		}
		n := to - i - 1
		if n < 0 || n > 255 {
			return 0, fmt.Errorf("seccomp filter: jump from %d to %s is out of range", i, label)
		}
		return uint8(n), nil
	}
	out := make([]bpf.Instruction, len(prog))
	for i, in := range prog {
		j, ok := in.ins.(bpf.JumpIf)
		if !ok {
			out[i] = in.ins
			continue
		}
		var err error
		if j.SkipTrue, err = skip(i, in.jt); err != nil {
			return nil, err
		}
		if j.SkipFalse, err = skip(i, in.jf); err != nil {
			return nil, err
		}
		out[i] = j
	}
	return out, nil
}
