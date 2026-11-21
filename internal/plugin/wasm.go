package plugin

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/experimental"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"github.com/tetratelabs/wazero/sys"

	"github.com/paingha/tugcli/pkg/provider"
)

// DefaultWASMMemoryLimit caps a WebAssembly plugin's memory when
// WASMOptions sets no limit.
const DefaultWASMMemoryLimit = 256 << 20

const (
	// wasmPageBytes is the size of a WebAssembly memory page.
	wasmPageBytes = 64 << 10
	// wasmTempDir is where a module sees its private temporary directory.
	wasmTempDir = "/tmp"
	// maxPendingRequestBytes caps the requests waiting for a serve-mode
	// module to read them.
	maxPendingRequestBytes = 64 << 20
)

// WASMOptions configure the sandbox a WebAssembly plugin runs in. The module
// gets what a process would: the operation as its argument, the request on
// stdin, stdout for its response and stderr for its logs, and the
// environment its manifest allows. It sees no files but a private temporary
// directory at /tmp, and no network but https to the hosts in Network,
// through the tug_http host functions.
type WASMOptions struct {
	// Network is the manifest's spec.network.
	Network []string
	// MemoryLimit caps the module's memory in bytes; zero means
	// DefaultWASMMemoryLimit.
	MemoryLimit uint64
	// CacheDir keeps compiled modules between runs, so that only the first
	// run of a plugin pays for compiling it. Empty compiles in memory.
	CacheDir string
	// TempDir holds each instance's private temporary directory; empty
	// means the system's.
	TempDir string
	// Transport sends the module's requests. Nil uses a transport with the
	// system's certificate roots and the proxy from the environment. tug
	// adds no cookies or credentials of its own either way.
	Transport http.RoundTripper
}

// wasmHost is a WebAssembly plugin compiled and ready to run: one wazero
// runtime for the plugin, with WASI and tug_http, shared by its calls.
type wasmHost struct {
	name    string
	rt      wazero.Runtime
	cache   wazero.CompilationCache
	module  wazero.CompiledModule
	allow   allowList
	client  *http.Client
	limit   uint64
	tempDir string
	log     *slog.Logger
}

// newWASMHost compiles the module at bin and checks that it is a WASI
// command that imports nothing tug does not provide.
func newWASMHost(ctx context.Context, name, bin string, o WASMOptions, log *slog.Logger) (*wasmHost, error) {
	allow, err := newAllowList(o.Network)
	if err != nil {
		return nil, err
	}
	code, err := os.ReadFile(bin)
	if err != nil {
		return nil, err
	}
	h := &wasmHost{
		name:    name,
		allow:   allow,
		client:  newHostClient(o.Transport),
		limit:   o.MemoryLimit,
		tempDir: o.TempDir,
		log:     log,
	}
	if h.limit == 0 {
		h.limit = DefaultWASMMemoryLimit
	}
	// Closing on context done makes wazero check for cancellation in loops
	// and calls, so a timeout stops even a module that never yields.
	cfg := wazero.NewRuntimeConfig().WithCloseOnContextDone(true)
	if o.CacheDir != "" {
		if err := os.MkdirAll(o.CacheDir, 0o700); err != nil {
			return nil, err
		}
		if h.cache, err = wazero.NewCompilationCacheWithDir(o.CacheDir); err != nil {
			return nil, fmt.Errorf("opening the WebAssembly cache: %w", err)
		}
		cfg = cfg.WithCompilationCache(h.cache)
	}
	h.rt = wazero.NewRuntimeWithConfig(ctx, cfg)
	if err := h.setUp(ctx, code); err != nil {
		_ = h.close() // the error from setting up is the one to report
		return nil, err
	}
	return h, nil
}

func (h *wasmHost) setUp(ctx context.Context, code []byte) error {
	if _, err := wasi_snapshot_preview1.Instantiate(ctx, h.rt); err != nil {
		return err
	}
	if err := h.instantiateHTTP(ctx); err != nil {
		return err
	}
	m, err := h.rt.CompileModule(ctx, code)
	if err != nil {
		return fmt.Errorf("compiling the WebAssembly module: %w", err)
	}
	if err := checkModule(m, h.limit); err != nil {
		return err
	}
	h.module = m
	return nil
}

// checkModule refuses a module that imports what tug does not provide, is
// not a WASI command, or needs more memory than its limit from the start.
func checkModule(m wazero.CompiledModule, limit uint64) error {
	for _, f := range m.ImportedFunctions() {
		module, name, _ := f.Import()
		switch {
		case module == wasi_snapshot_preview1.ModuleName:
		case module == provider.HostModule && isHostFunction(name):
		default:
			return fmt.Errorf("the module imports %s.%s, which tug does not provide; a WebAssembly plugin may import only %s and %s",
				module, name, wasi_snapshot_preview1.ModuleName, provider.HostModule)
		}
	}
	if len(m.ImportedMemories()) > 0 {
		return errors.New("the module imports its memory; it must define and export it, as a WASI command does")
	}
	if _, ok := m.ExportedFunctions()["_start"]; !ok {
		return fmt.Errorf("the module has no _start function; build it as a WASI command (GOOS=%s GOARCH=%s go build)", WASMOS, WASMArch)
	}
	mem, ok := m.ExportedMemories()["memory"]
	if !ok {
		return errors.New(`the module does not export its memory as "memory", which WASI needs`)
	}
	if need := uint64(mem.Min()) * wasmPageBytes; need > limit {
		return fmt.Errorf("the module starts with %d MiB of memory, more than its %d MiB limit", need>>20, limit>>20)
	}
	return nil
}

func (h *wasmHost) close() error {
	ctx := context.Background()
	var errs []error
	if h.rt != nil {
		errs = append(errs, h.rt.Close(ctx))
	}
	if h.cache != nil {
		errs = append(errs, h.cache.Close(ctx))
	}
	return errors.Join(errs...)
}

// run starts an instance of the module and waits for it to end: its _start
// function returns, it calls proc_exit, or ctx ends. args follow the program
// name, as a process's would. It returns nil when the module exits 0, a
// *wasmExitError for another exit status, a *memoryLimitError when the module
// failed after tug refused it memory, ctx's error when ctx ended, and any
// other error for a trap.
func (h *wasmHost) run(ctx context.Context, args, env []string, stdin io.Reader, stdout, stderr io.Writer) error {
	tmp, err := os.MkdirTemp(h.tempDir, "tug-wasm-")
	if err != nil {
		return fmt.Errorf("making the module's temporary directory: %w", err)
	}
	defer os.RemoveAll(tmp)

	mem := &memoryLimit{limit: h.limit}
	sess := &httpSession{host: h, open: map[uint32]*openResponse{}}
	defer sess.closeAll()
	ctx = experimental.WithMemoryAllocator(ctx, mem)
	ctx = context.WithValue(ctx, httpSessionKey{}, sess)

	cfg := wazero.NewModuleConfig().
		WithName(""). // anonymous, so instances can run side by side
		WithArgs(append([]string{BinaryPrefix + h.name}, args...)...).
		WithStdin(stdin).
		WithStdout(stdout).
		WithStderr(stderr).
		WithFSConfig(wazero.NewFSConfig().WithDirMount(tmp, wasmTempDir)).
		WithSysWalltime().
		WithSysNanotime().
		WithNanosleep(sleepUntilDone(ctx)).
		WithRandSource(rand.Reader) // wazero's default is deterministic
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		cfg = cfg.WithEnv(k, v)
	}
	m, err := h.rt.InstantiateModule(ctx, h.module, cfg)
	if m != nil {
		_ = m.Close(context.Background()) // already closed unless _start returned
	}
	return exitError(ctx, err, mem)
}

func exitError(ctx context.Context, err error, mem *memoryLimit) error {
	if err == nil {
		return nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if mem.refused.Load() {
		return &memoryLimitError{limit: mem.limit}
	}
	var ee *sys.ExitError
	if errors.As(err, &ee) {
		switch ee.ExitCode() {
		case sys.ExitCodeContextCanceled:
			return context.Canceled
		case sys.ExitCodeDeadlineExceeded:
			return context.DeadlineExceeded
		}
		return &wasmExitError{code: ee.ExitCode()}
	}
	return err
}

// sleepUntilDone sleeps as WASI's poll_oneoff asks, but no longer than ctx
// lasts, so a module asleep in a long timer still stops on time.
func sleepUntilDone(ctx context.Context) sys.Nanosleep {
	return func(ns int64) {
		t := time.NewTimer(time.Duration(ns))
		defer t.Stop()
		select {
		case <-t.C:
		case <-ctx.Done():
		}
	}
}

// wasmExitError is a module's non-zero exit status, as *exec.ExitError is a
// process's.
type wasmExitError struct{ code uint32 }

func (e *wasmExitError) Error() string { return fmt.Sprintf("exit status %d", e.code) }

// memoryLimitError says a module failed after tug refused to grow its
// memory past the limit.
type memoryLimitError struct{ limit uint64 }

// memoryHint is the hint for a memoryLimitError.
const memoryHint = "if the plugin needs more, raise spec.limits.memoryMiB in its plugin.yaml, within plugins.wasm.memoryLimitMiB and plugins.limits.memoryMiB in tug's config.yaml"

func (e *memoryLimitError) Error() string {
	return fmt.Sprintf("ran out of memory: WebAssembly plugins may use %d MiB", e.limit>>20)
}

// memoryLimit backs a module's memory and refuses to grow it past limit. It
// enforces the limit itself, rather than leaving it to wazero, so that it
// knows when a refusal is what made a module fail, whatever language the
// module was written in.
type memoryLimit struct {
	limit   uint64
	refused atomic.Bool
}

// Allocate implements experimental.MemoryAllocator.
func (l *memoryLimit) Allocate(capacity, _ uint64) experimental.LinearMemory {
	return &linearMemory{limit: l, buf: make([]byte, 0, min(capacity, l.limit))}
}

type linearMemory struct {
	limit *memoryLimit
	buf   []byte
}

// Reallocate implements experimental.LinearMemory. wazero only grows a
// memory, and bytes past the old length are zero, as make leaves them.
func (m *linearMemory) Reallocate(size uint64) []byte {
	if size > m.limit.limit {
		m.limit.refused.Store(true)
		return nil
	}
	if size <= uint64(cap(m.buf)) {
		m.buf = m.buf[:size]
		return m.buf
	}
	buf := make([]byte, size, min(max(size, 2*uint64(cap(m.buf))), m.limit.limit))
	copy(buf, m.buf)
	m.buf = buf
	return buf
}

// Free implements experimental.LinearMemory.
func (m *linearMemory) Free() { m.buf = nil }

// wasmInstance is a WebAssembly plugin in serve mode: one module instance,
// running in a goroutine of its own and reading requests from a pipe.
type wasmInstance struct {
	cancel  context.CancelFunc
	stdin   *requestPipe
	done    chan struct{}
	err     error // how the instance ended; set before done closes
	stopped atomic.Bool
}

var errStopped = errors.New("tug stopped it")

func (w *wasmInstance) wait() error {
	<-w.done
	if w.stopped.Load() && errors.Is(w.err, context.Canceled) {
		return errStopped
	}
	return w.err
}

func (w *wasmInstance) terminate() error {
	w.stopped.Store(true)
	w.cancel()
	return w.stdin.Close() // wakes a module waiting for its next request
}

func (w *wasmInstance) kill() error { return w.terminate() }

// startWASM starts a module instance in serve mode.
func (r *Runner) startWASM(stderr io.Writer) (instance, io.WriteCloser, io.ReadCloser, error) {
	h, err := r.wasmHost()
	if err != nil {
		return nil, nil, nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	in := newRequestPipe(maxPendingRequestBytes)
	outR, outW := io.Pipe()
	w := &wasmInstance{cancel: cancel, stdin: in, done: make(chan struct{})}
	go func() {
		defer close(w.done)
		defer cancel()
		w.err = h.run(ctx, []string{provider.OpServe}, r.wasmEnviron(), in, outW, stderr)
		_ = outW.Close() // ends the server's read loop
	}()
	return w, in, outR, nil
}

// requestPipe is stdin for a module in serve mode. A write never waits for
// the module, which reads its next request only after answering the last,
// so a caller queued behind a slow request can still give up on time.
type requestPipe struct {
	mu     sync.Mutex
	ready  *sync.Cond
	buf    bytes.Buffer
	limit  int
	closed bool
}

func newRequestPipe(limit int) *requestPipe {
	p := &requestPipe{limit: limit}
	p.ready = sync.NewCond(&p.mu)
	return p
}

func (p *requestPipe) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return 0, io.ErrClosedPipe
	}
	if p.buf.Len()+len(b) > p.limit {
		return 0, fmt.Errorf("the plugin has not yet read %d bytes of earlier requests", p.buf.Len())
	}
	p.buf.Write(b)
	p.ready.Signal()
	return len(b), nil
}

func (p *requestPipe) Read(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for p.buf.Len() == 0 && !p.closed {
		p.ready.Wait()
	}
	if p.buf.Len() == 0 {
		return 0, io.EOF
	}
	return p.buf.Read(b)
}

// Close ends the stream once the module has read what is buffered.
func (p *requestPipe) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	p.ready.Broadcast()
	return nil
}

// readUntilDone passes r through a pipe that closes when ctx ends or stop is
// called, so a module blocked reading its stdin wakes up. The goroutine
// copying r may stay blocked in r.Read after that, until r has data or
// closes; Exec reads the stdin of a process that is about to exit.
func readUntilDone(ctx context.Context, r io.Reader) (io.Reader, func()) {
	pr, pw := io.Pipe()
	go func() {
		_, err := io.Copy(pw, r)
		pw.CloseWithError(err)
	}()
	stopAfter := context.AfterFunc(ctx, func() { _ = pr.Close() })
	return pr, func() {
		stopAfter()
		_ = pr.Close()
	}
}
