package sandbox

import (
	"encoding/binary"
	"testing"

	"golang.org/x/net/bpf"
)

// seccompData builds the struct seccomp_data the filter reads. The kernel
// loads its words in the machine's byte order; x/net's BPF machine loads
// them big-endian, so each word is written big-endian at the offset the
// kernel would have it, which gives the machine the same values.
func seccompData(arch, nr, arg0 uint32) []byte {
	b := make([]byte, 64)
	binary.BigEndian.PutUint32(b[offsetNR:], nr)
	binary.BigEndian.PutUint32(b[offsetArch:], arch)
	binary.BigEndian.PutUint32(b[offsetArg0:], arg0)
	return b
}

func TestSeccompFilter(t *testing.T) {
	const (
		arch    = 0xc000003e
		ptrace  = 101
		mount   = 165
		clone   = 56
		clone3  = 435
		socket  = 41
		getpid  = 39
		afInet  = 2
		threads = 0x003d0f00 // what glibc passes to clone for a new thread
		vfork   = 0x00004111 // what Go's os/exec passes to clone
	)
	table := syscallTable{arch: arch, x32: true, deny: []uint32{ptrace, mount}, clone: clone, clone3: clone3, socket: socket}
	tests := []struct {
		name      string
		allowUnix bool
		data      []byte
		want      uint32
	}{
		{"an allowed call", false, seccompData(arch, getpid, 0), seccompRetAllow},
		{"a denied call", false, seccompData(arch, ptrace, 0), seccompRetErrno | errnoEPERM},
		{"the last denied call", false, seccompData(arch, mount, 0), seccompRetErrno | errnoEPERM},
		{"another architecture", false, seccompData(0x40000003, getpid, 0), seccompRetKillProcess},
		{"the x32 ABI", false, seccompData(arch, x32SyscallBit|getpid, 0), seccompRetKillProcess},
		{"clone for a thread", false, seccompData(arch, clone, threads), seccompRetAllow},
		{"clone for a child process", false, seccompData(arch, clone, vfork), seccompRetAllow},
		{"clone into a user namespace", false, seccompData(arch, clone, 0x10000000|0x11), seccompRetErrno | errnoEPERM},
		{"clone into a network namespace", false, seccompData(arch, clone, 0x40000000), seccompRetErrno | errnoEPERM},
		{"clone3", false, seccompData(arch, clone3, 0), seccompRetErrno | errnoENOSYS},
		{"an inet socket", false, seccompData(arch, socket, afInet), seccompRetAllow},
		{"a unix socket", false, seccompData(arch, socket, afUnix), seccompRetErrno | errnoEPERM},
		{"a unix socket the policy allows", true, seccompData(arch, socket, afUnix), seccompRetAllow},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw, err := filter(table, tt.allowUnix)
			if err != nil {
				t.Fatal(err)
			}
			insts, ok := bpf.Disassemble(raw)
			if !ok {
				t.Fatalf("filter assembled instructions that do not disassemble: %v", insts)
			}
			vm, err := bpf.NewVM(insts)
			if err != nil {
				t.Fatal(err)
			}
			got, err := vm.Run(tt.data)
			if err != nil {
				t.Fatal(err)
			}
			if uint32(got) != tt.want {
				t.Errorf("filter returns %#x, want %#x", uint32(got), tt.want)
			}
		})
	}
}

func TestSeccompFilterWithoutX32(t *testing.T) {
	const arch = 0xc00000b7
	raw, err := filter(syscallTable{arch: arch, deny: []uint32{117}, clone: 220, clone3: 435, socket: 198}, false)
	if err != nil {
		t.Fatal(err)
	}
	insts, _ := bpf.Disassemble(raw)
	vm, err := bpf.NewVM(insts)
	if err != nil {
		t.Fatal(err)
	}
	// Without the x32 check, a large number is just an unknown call.
	if got, _ := vm.Run(seccompData(arch, x32SyscallBit|1, 0)); uint32(got) != seccompRetAllow {
		t.Errorf("filter returns %#x for a large call number on arm64, want allow", uint32(got))
	}
}

func TestLinkRefusesLongJumps(t *testing.T) {
	prog := []insn{{ins: bpf.JumpIf{Cond: bpf.JumpEqual, Val: 1}, jt: "end"}}
	for range 300 {
		prog = append(prog, insn{ins: bpf.LoadAbsolute{Off: 0, Size: 4}})
	}
	prog = append(prog, insn{label: "end", ins: bpf.RetConstant{Val: 0}})
	if _, err := link(prog); err == nil {
		t.Fatal("link accepted a jump past 255 instructions")
	}
	if _, err := link([]insn{{ins: bpf.JumpIf{Cond: bpf.JumpEqual}, jt: "nowhere"}}); err == nil {
		t.Fatal("link accepted a jump to a missing label")
	}
}
