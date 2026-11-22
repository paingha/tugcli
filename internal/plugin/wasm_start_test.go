package plugin

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/paingha/tugcli/pkg/provider"
)

// TestWASMHostStopsOnSIGTERMDuringStartUp checks what the conformance suite
// checks, "SIGTERM stops the plugin promptly", while the host is still
// compiling the module: start-up is held back for longer than the suite
// waits, as a loaded machine under the race detector, or a cold cache,
// holds it back.
func TestWASMHostStopsOnSIGTERMDuringStartUp(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no SIGTERM")
	}
	p, err := Load(exampleWASMDir(t), true)
	if err != nil {
		t.Fatal(err)
	}
	target := wasmTarget(t, p)
	cmd := exec.Command(target.Command, provider.OpList)
	cmd.Env = append(slices.Clone(target.Env), wasmSlowStartEnv+"=30s")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	time.Sleep(200 * time.Millisecond) // as the suite does
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the WebAssembly host was still running 2s after SIGTERM, waiting for its module to compile")
	}
}

// TestWASMCallsStopWaitingForACompile checks that calls stop waiting for a
// slow compile when their context ends, that the compile then serves the
// next call, and that closing the runner during one is safe.
func TestWASMCallsStopWaitingForACompile(t *testing.T) {
	p, err := Load(exampleWASMDir(t), true)
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	r := &Runner{Bin: p.Bin, Name: p.Name(), WASM: &WASMOptions{CacheDir: wasmCache(), beforeCompile: func() { <-release }}}
	t.Cleanup(func() { _ = r.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	code, err := r.Exec(ctx, []string{provider.OpDescribe}, strings.NewReader(""), io.Discard, io.Discard)
	if err != nil || code != provider.ExitError {
		t.Fatalf("Exec cancelled during the compile = %d, %v; want exit status %d", code, err, provider.ExitError)
	}
	if _, err := r.Call(ctx, provider.OpDescribe, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Call cancelled during the compile = %v, want the context's error", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("cancelled calls waited %s for the compile", elapsed)
	}

	close(release)
	out, err := r.Call(context.Background(), provider.OpDescribe, nil)
	if err != nil {
		t.Fatalf("Call after the compile finished: %v", err)
	}
	if d, err := provider.DecodeDescribe(out); err != nil || d.Name != "example" {
		t.Fatalf("describe = %+v, %v", d, err)
	}

	// Closing the runner mid-compile frees the module once it is done, and
	// the runner compiles again when it is next used.
	held := make(chan struct{})
	r2 := &Runner{Bin: p.Bin, Name: p.Name(), Serve: true, WASM: &WASMOptions{CacheDir: wasmCache(), beforeCompile: func() { <-held }}}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel2()
	if _, err := r2.Call(ctx2, provider.OpDescribe, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("serve-mode Call cancelled during the compile = %v, want the context's error", err)
	}
	r2.wasmMu.Lock()
	job := r2.compiling
	r2.wasmMu.Unlock()
	if err := r2.Close(); err != nil {
		t.Fatal(err)
	}
	close(held)
	<-job.done
	r2.wasmMu.Lock()
	kept := r2.wasm
	r2.wasmMu.Unlock()
	if job.h != nil || kept != nil {
		t.Fatal("a compile abandoned by Close kept its module")
	}
	out, err = r2.Call(context.Background(), provider.OpDescribe, nil)
	if err != nil {
		t.Fatalf("Call after Close compiled again: %v", err)
	}
	_ = r2.Close()
	if _, err := provider.DecodeDescribe(out); err != nil {
		t.Fatal(err)
	}
}
