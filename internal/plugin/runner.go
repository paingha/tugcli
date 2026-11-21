package plugin

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/paingha/tugcli/internal/sandbox"
	"github.com/paingha/tugcli/pkg/provider"
)

// Default limits from the protocol.
const (
	DefaultListTimeout       = 30 * time.Second
	DefaultCredentialTimeout = 10 * time.Second
	// DefaultRotateTimeout is longer because a provider may have to wait for
	// a regenerated credential after revoking the old one.
	DefaultRotateTimeout = 60 * time.Second
	DefaultTimeout       = 30 * time.Second
	// killGrace is how long a plugin has to exit after SIGTERM.
	killGrace = 2 * time.Second
)

// baseEnv is passed to every plugin; everything else must be declared.
var baseEnv = []string{"PATH", "HOME", "LANG", "LC_ALL", "LC_CTYPE", "TZ", "TMPDIR", "USER", "SSL_CERT_FILE", "SSL_CERT_DIR", "HTTPS_PROXY", "HTTP_PROXY", "NO_PROXY", "https_proxy", "http_proxy", "no_proxy"}

var windowsEnv = []string{"SYSTEMROOT", "SYSTEMDRIVE", "WINDIR", "COMSPEC", "PATHEXT", "APPDATA", "LOCALAPPDATA", "USERPROFILE", "TEMP", "TMP", "PROGRAMDATA"}

// Runner runs a plugin: a process, or a WebAssembly module instance, for each
// operation, or one for every call in serve mode.
type Runner struct {
	Bin  string
	Name string
	// Env lists extra host variables the manifest declares.
	Env      []string
	Timeouts map[string]time.Duration
	Log      *slog.Logger
	// MaxBytes caps the response; defaults to provider.MaxMessageBytes.
	MaxBytes int64
	// Serve keeps one plugin process for every call, when the manifest
	// declares the serve capability. Close stops it.
	Serve bool
	// WASM, when set, runs Bin as a WebAssembly module inside tug rather
	// than as a process. The module is compiled on the first call.
	WASM *WASMOptions
	// Sandbox, when set, confines every plugin process the runner starts. A
	// WebAssembly plugin has no process; its runtime confines it instead.
	Sandbox *sandbox.Sandbox

	srvMu sync.Mutex
	srv   *server

	wasmMu sync.Mutex
	wasm   *wasmHost
}

func (r *Runner) timeout(op string) time.Duration {
	if d, ok := r.Timeouts[op]; ok && d > 0 {
		return d
	}
	switch op {
	case provider.OpList:
		return DefaultListTimeout
	case provider.OpCredential:
		return DefaultCredentialTimeout
	case provider.OpRotate:
		return DefaultRotateTimeout
	default:
		return DefaultTimeout
	}
}

// Call runs the plugin for op with the request bytes and returns the
// response bytes. The process gets a scrubbed environment, a timeout, its
// sandbox when the runner has one, and is sent SIGTERM then SIGKILL when the
// context ends. A WebAssembly plugin gets the environment and the timeout,
// and is stopped where it is when the context ends.
func (r *Runner) Call(ctx context.Context, op string, request []byte) ([]byte, error) {
	if r.Serve {
		return r.callServe(ctx, op, request)
	}
	if r.WASM != nil {
		return r.callWASM(ctx, op, request)
	}
	ctx, cancel := context.WithTimeout(ctx, r.timeout(op))
	defer cancel()

	c, err := r.Command(op)
	if err != nil {
		return nil, err
	}
	defer c.Cleanup()
	cmd := exec.CommandContext(ctx, c.Path, c.Args...)
	cmd.Env = c.Env
	cmd.Stdin = bytes.NewReader(request)
	cmd.Cancel = func() error { return terminate(cmd.Process) }
	cmd.WaitDelay = killGrace

	stdout := &limitedBuffer{limit: r.maxBytes()}
	cmd.Stdout = stdout
	stderr := r.stderrLogger(op)
	cmd.Stderr = stderr

	start := time.Now()
	err = cmd.Run()
	stderr.flush()
	r.logger().Debug("plugin call", "plugin", r.Name, "op", op, "duration", time.Since(start), "err", err)
	return r.response(ctx, op, stdout, err)
}

// callWASM runs op in a new instance of the plugin's module.
func (r *Runner) callWASM(ctx context.Context, op string, request []byte) ([]byte, error) {
	// Compiling, on the first call, does not count against op's timeout.
	h, err := r.wasmHost()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, r.timeout(op))
	defer cancel()
	stdout := &limitedBuffer{limit: r.maxBytes()}
	stderr := r.stderrLogger(op)
	start := time.Now()
	err = h.run(ctx, []string{op}, r.wasmEnviron(), bytes.NewReader(request), stdout, stderr)
	stderr.flush()
	r.logger().Debug("plugin call", "plugin", r.Name, "op", op, "runtime", "wasm", "duration", time.Since(start), "err", err)
	return r.response(ctx, op, stdout, err)
}

// response turns what a call wrote to stdout, and how it ended, into the
// response or an error.
func (r *Runner) response(ctx context.Context, op string, stdout *limitedBuffer, err error) ([]byte, error) {
	if stdout.exceeded {
		return nil, provider.Errorf(provider.Internal, "plugin %s: response exceeds %d bytes", r.Name, stdout.limit)
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		if errors.Is(ctxErr, context.DeadlineExceeded) {
			return nil, provider.Errorf(provider.Unavailable, "plugin %s %s timed out after %s", r.Name, op, r.timeout(op))
		}
		return nil, ctxErr
	}
	// A non-zero exit with a JSON error on stdout is a normal provider error;
	// the caller decodes it. A plugin that dies without one may have been
	// stopped by its sandbox.
	if err != nil && stdout.Len() == 0 {
		var mle *memoryLimitError
		if errors.As(err, &mle) {
			return nil, provider.Errorf(provider.Internal, "plugin %s %s %v", r.Name, op, err).WithHint(memoryHint)
		}
		return nil, r.withSandboxHint(provider.Errorf(provider.Internal, "plugin %s %s: %v", r.Name, op, err))
	}
	return stdout.Bytes(), nil
}

func (r *Runner) maxBytes() int64 {
	if r.MaxBytes > 0 {
		return r.MaxBytes
	}
	return provider.MaxMessageBytes
}

// wasmHost returns the plugin's compiled module, compiling it on first use.
func (r *Runner) wasmHost() (*wasmHost, error) {
	r.wasmMu.Lock()
	defer r.wasmMu.Unlock()
	if r.wasm == nil {
		start := time.Now()
		h, err := newWASMHost(context.Background(), r.Name, r.Bin, *r.WASM, r.logger())
		if err != nil {
			return nil, provider.Errorf(provider.Internal, "plugin %s: %v", r.Name, err)
		}
		r.logger().Debug("compiled WebAssembly plugin", "plugin", r.Name, "duration", time.Since(start))
		r.wasm = h
	}
	return r.wasm, nil
}

// Prepare compiles a WebAssembly plugin, so that its first call need not,
// and reports a module that tug cannot run. With CacheDir set, the compiled
// module also serves later runs of tug. It does nothing for a native plugin.
func (r *Runner) Prepare() error {
	if r.WASM == nil {
		return nil
	}
	_, err := r.wasmHost()
	return err
}

// Exec runs the plugin with these arguments and streams, as its own process
// would, and returns its exit status; cancelling ctx stops it. For a
// WebAssembly plugin it is the process the conformance suite tests, through
// `tug provider-exec`. The error is for a plugin that could not start.
func (r *Runner) Exec(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	if r.WASM == nil {
		c, err := r.Command(args...)
		if err != nil {
			return 0, err
		}
		defer c.Cleanup()
		cmd := exec.CommandContext(ctx, c.Path, c.Args...)
		cmd.Env = c.Env
		cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
		cmd.Cancel = func() error { return terminate(cmd.Process) }
		cmd.WaitDelay = killGrace
		err = cmd.Run()
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return max(ee.ExitCode(), provider.ExitError), nil // -1 when a signal ended it
		}
		return 0, err
	}
	h, err := r.wasmHost()
	if err != nil {
		return 0, err
	}
	in, stop := readUntilDone(ctx, stdin)
	defer stop()
	err = h.run(ctx, args, r.wasmEnviron(), in, stdout, stderr)
	var ee *wasmExitError
	switch {
	case err == nil:
		return 0, nil
	case errors.As(err, &ee):
		return int(ee.code), nil
	case ctx.Err() != nil:
		return provider.ExitError, nil // as a process ended by a signal
	default:
		return provider.ExitError, fmt.Errorf("plugin %s %w", r.Name, err)
	}
}

func (r *Runner) logger() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.New(slog.DiscardHandler)
}

// Environ returns the scrubbed environment the plugin runs with: a few basics
// such as PATH and HOME, plus the variables its manifest declares. A
// WebAssembly plugin gets less (see wasmEnviron).
func (r *Runner) Environ() []string {
	if r.WASM != nil {
		return r.wasmEnviron()
	}
	names := append([]string{}, baseEnv...)
	if runtime.GOOS == "windows" {
		names = append(names, windowsEnv...)
	}
	return append(lookupEnv(append(names, r.Env...)), "TUG_PLUGIN=1")
}

// wasmBaseEnv is what every WebAssembly plugin gets of tug's environment.
// A module has no programs to find, sees none of the user's files and opens
// no connections itself, so the paths and proxy settings a process gets
// would mean nothing to it.
var wasmBaseEnv = []string{"LANG", "LC_ALL", "LC_CTYPE", "TZ"}

// wasmEnviron is the environment of a WebAssembly plugin: the locale, the
// variables its manifest declares, HOME and TMPDIR set to its private
// temporary directory, and TUG_PLUGIN=1.
func (r *Runner) wasmEnviron() []string {
	fixed := []string{"HOME", "TMPDIR", "TUG_PLUGIN"}
	names := slices.DeleteFunc(append(slices.Clone(wasmBaseEnv), r.Env...), func(n string) bool {
		return slices.Contains(fixed, n)
	})
	return append(lookupEnv(names), "HOME="+wasmTempDir, "TMPDIR="+wasmTempDir, "TUG_PLUGIN=1")
}

// lookupEnv returns NAME=value for each of names that is set, once each.
func lookupEnv(names []string) []string {
	env := make([]string, 0, len(names)+1)
	seen := map[string]bool{}
	for _, n := range names {
		if seen[n] {
			continue
		}
		seen[n] = true
		if v, ok := os.LookupEnv(n); ok {
			env = append(env, n+"="+v)
		}
	}
	return env
}

// limitedBuffer stops accepting data past limit. The buffer is a named field,
// not embedded, so io.Copy cannot bypass Write through bytes.Buffer.ReadFrom.
type limitedBuffer struct {
	buf      bytes.Buffer
	limit    int64
	exceeded bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if int64(b.buf.Len()+len(p)) > b.limit {
		b.exceeded = true
		return 0, errors.New("response too large")
	}
	return b.buf.Write(p)
}

func (b *limitedBuffer) Len() int      { return b.buf.Len() }
func (b *limitedBuffer) Bytes() []byte { return b.buf.Bytes() }

// lineLogger sends each line a plugin writes to stderr to tug's log.
type lineLogger struct {
	log  *slog.Logger
	name string
	op   string
	buf  bytes.Buffer
}

func (r *Runner) stderrLogger(op string) *lineLogger {
	return &lineLogger{log: r.logger(), name: r.Name, op: op}
}

func (l *lineLogger) Write(p []byte) (int, error) {
	l.buf.Write(p)
	for {
		line, err := l.buf.ReadString('\n')
		if errors.Is(err, io.EOF) {
			l.buf.WriteString(line)
			return len(p), nil
		}
		l.emit(line)
	}
}

func (l *lineLogger) flush() {
	sc := bufio.NewScanner(&l.buf)
	for sc.Scan() {
		l.emit(sc.Text())
	}
}

func (l *lineLogger) emit(line string) {
	if line = strings.TrimRight(line, "\r\n"); line != "" {
		l.log.Info(line, "plugin", l.name, "op", l.op)
	}
}

// String renders the runner for debugging.
func (r *Runner) String() string { return fmt.Sprintf("%s (%s)", r.Name, r.Bin) }
