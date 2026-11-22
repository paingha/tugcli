package plugin

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/paingha/tugcli/internal/config"
	"github.com/paingha/tugcli/internal/sandbox"
	"github.com/paingha/tugcli/pkg/provider"
)

func TestManifestLimitsValidate(t *testing.T) {
	tests := []struct {
		name   string
		limits Limits
		want   string
	}{
		{"negative CPU", Limits{CPUSeconds: -1}, "spec.limits.cpuSeconds"},
		{"negative memory", Limits{MemoryMiB: -5}, "spec.limits.memoryMiB"},
		{"unknown operation", Limits{Timeouts: map[string]config.Duration{"lists": {Duration: time.Minute}}}, "spec.limits.timeouts.lists is not an operation"},
		{"serve has no timeout", Limits{Timeouts: map[string]config.Duration{"serve": {Duration: time.Minute}}}, "spec.limits.timeouts.serve"},
		{"zero timeout", Limits{Timeouts: map[string]config.Duration{"list": {}}}, "positive duration"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &Manifest{
				APIVersion: ManifestAPIVersion, Kind: ManifestKind,
				Metadata: Metadata{Name: "ok", Version: "1"},
				Spec: Spec{
					Protocols: []string{provider.APIVersion},
					Platforms: []Platform{{OS: "linux", Arch: "amd64", Bin: "bin/x"}},
					Limits:    tt.limits,
				},
			}
			if err := m.Validate(); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate = %v, want mention of %q", err, tt.want)
			}
		})
	}
}

func TestBudget(t *testing.T) {
	const mib = 1 << 20
	asks := Limits{CPUSeconds: 120, MemoryMiB: 512, Timeouts: map[string]config.Duration{
		"list": {Duration: 10 * time.Minute}, "credential": {Duration: 5 * time.Second},
	}}
	tests := []struct {
		name     string
		limits   Limits
		wasm     bool
		caps     Caps
		cpu      time.Duration
		memory   uint64
		wasmMem  uint64
		timeouts map[string]time.Duration
		capped   []string
	}{
		{
			name: "nothing asked, nothing capped",
			cpu:  0, memory: 0, wasmMem: DefaultWASMMemoryLimit,
			timeouts: map[string]time.Duration{"list": DefaultListTimeout, "credential": DefaultCredentialTimeout, "rotate": DefaultRotateTimeout, "describe": DefaultTimeout},
		},
		{
			name:   "asks, no caps",
			limits: asks,
			cpu:    2 * time.Minute, memory: 512 * mib, wasmMem: DefaultWASMMemoryLimit,
			timeouts: map[string]time.Duration{"list": DefaultMaxTimeout, "credential": 5 * time.Second},
			capped:   []string{"timeouts.list 10m0s, capped at 5m0s"},
		},
		{
			name:   "caps cut what is asked",
			limits: asks, wasm: true,
			caps: Caps{CPU: time.Minute, Memory: 256 * mib, Timeout: 20 * time.Minute, WASMMemory: 1024 * mib},
			cpu:  time.Minute, memory: 256 * mib, wasmMem: 256 * mib,
			timeouts: map[string]time.Duration{"list": 10 * time.Minute, "credential": 5 * time.Second},
			capped:   []string{"cpuSeconds 120, capped at 60", "memoryMiB 512, capped at 256"},
		},
		{
			name: "caps apply to plugins that ask for nothing",
			caps: Caps{CPU: time.Minute, Memory: 128 * mib, Timeout: 20 * time.Second},
			cpu:  time.Minute, memory: 128 * mib, wasmMem: 128 * mib,
			timeouts: map[string]time.Duration{"list": 20 * time.Second, "credential": DefaultCredentialTimeout, "rotate": 20 * time.Second},
		},
		{
			name:   "config timeouts win over the manifest, within the cap",
			limits: asks,
			caps:   Caps{Timeouts: map[string]time.Duration{"list": time.Hour, "credential": 30 * time.Second}, Timeout: 15 * time.Minute},
			cpu:    2 * time.Minute, memory: 512 * mib, wasmMem: DefaultWASMMemoryLimit,
			timeouts: map[string]time.Duration{"list": 15 * time.Minute, "credential": 30 * time.Second},
		},
		{
			name:   "a WebAssembly build asking for more than its cap",
			limits: Limits{MemoryMiB: 512}, wasm: true,
			memory: 512 * mib, wasmMem: DefaultWASMMemoryLimit,
			capped: []string{"memoryMiB 512 for WebAssembly, capped at 256"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &Manifest{Spec: Spec{Limits: tt.limits, Platforms: []Platform{{OS: "linux", Arch: "amd64", Bin: "x"}}}}
			if tt.wasm {
				m.Spec.Platforms = append(m.Spec.Platforms, Platform{OS: WASMOS, Arch: WASMArch, Bin: "x.wasm"})
			}
			b := m.Budget(tt.caps)
			if b.Process.CPU != tt.cpu || b.Process.Memory != tt.memory || b.WASMMemory != tt.wasmMem {
				t.Errorf("Budget = CPU %s, memory %d, WebAssembly %d; want %s, %d, %d", b.Process.CPU, b.Process.Memory, b.WASMMemory, tt.cpu, tt.memory, tt.wasmMem)
			}
			for op, want := range tt.timeouts {
				if got := b.Timeouts[op]; got != want {
					t.Errorf("Budget timeout for %s = %s, want %s", op, got, want)
				}
			}
			if fmt.Sprint(b.Capped) != fmt.Sprint(tt.capped) {
				t.Errorf("Budget capped %q, want %q", b.Capped, tt.capped)
			}
		})
	}
}

// selfExe returns this test binary, which stands in for tug.
func selfExe(t *testing.T) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return self
}

func TestCPULimitStopsPlugin(t *testing.T) {
	if !sandbox.SupportedLimits().CPU {
		t.Skip("no CPU limit here")
	}
	// The loop runs no other program: once bash has, it catches SIGXCPU,
	// which on macOS is all the kernel sends.
	bin := filepath.Join(t.TempDir(), "spin")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nwhile :; do :; done\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(t *testing.T, sb *sandbox.Sandbox) {
		r := &Runner{Bin: bin, Name: "spin", Self: selfExe(t), Limits: sandbox.Limits{CPU: time.Second}, Sandbox: sb,
			Timeouts: map[string]time.Duration{provider.OpList: 30 * time.Second}}
		start := time.Now()
		_, err := r.Call(context.Background(), provider.OpList, nil)
		pe := provider.AsError(err)
		if err == nil || !strings.Contains(pe.Hint, "CPU time 1s") {
			t.Fatalf("Call of a plugin that spins = %v (hint %q), want a failure with the CPU limit in its hint", err, pe.Hint)
		}
		if elapsed := time.Since(start); elapsed > 15*time.Second {
			t.Fatalf("the CPU limit took %s to stop the plugin", elapsed)
		}
	}
	t.Run("unsandboxed", func(t *testing.T) { run(t, nil) })
	t.Run("sandboxed", func(t *testing.T) {
		needSandbox(t)
		run(t, &sandbox.Sandbox{Policy: sandbox.Policy{Name: "spin", Dir: filepath.Dir(bin)}, Self: selfExe(t), TempRoot: t.TempDir()})
	})
}

// memoryHogEnv makes the test binary a plugin that tries to use 512 MiB.
const memoryHogEnv = "TUG_TEST_MEMORY_HOG"

func memoryHog() int {
	var keep [][]byte
	for range 512 {
		b := make([]byte, 1<<20)
		for i := range b {
			b[i] = 1
		}
		keep = append(keep, b)
	}
	fmt.Printf(`{"apiVersion":"provider/v1alpha1","kind":"ListResponse","clusters":[{"id":"%d","endpoints":{}}]}`+"\n", len(keep))
	return 0
}

func TestMemoryLimitStopsPlugin(t *testing.T) {
	if !sandbox.SupportedLimits().Memory {
		t.Skipf("no memory limit here: %s", sandbox.SupportedLimits().Missing)
	}
	if runtime.GOARCH == "386" || runtime.GOARCH == "arm" {
		t.Skip("a 32-bit address space limits the test itself")
	}
	t.Setenv(memoryHogEnv, "1")
	r := &Runner{Bin: selfExe(t), Name: "hog", Env: []string{memoryHogEnv}, Self: selfExe(t)}
	if _, err := r.Call(context.Background(), provider.OpList, nil); err != nil {
		t.Fatalf("the plugin without a limit: %v", err)
	}
	r.Limits = sandbox.Limits{Memory: 64 << 20}
	_, err := r.Call(context.Background(), provider.OpList, nil)
	if pe := provider.AsError(err); err == nil || !strings.Contains(pe.Hint, "memory 64 MiB") {
		t.Fatalf("Call of a plugin past its memory limit = %v, want a failure with the limit in its hint", err)
	}
}
