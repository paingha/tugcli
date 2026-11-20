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
	"strings"
	"sync"
	"time"

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

// Runner runs one plugin executable per operation.
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

	srvMu sync.Mutex
	srv   *server
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
// response bytes. The process gets a scrubbed environment, a timeout, and is
// sent SIGTERM then SIGKILL when the context ends.
func (r *Runner) Call(ctx context.Context, op string, request []byte) ([]byte, error) {
	if r.Serve {
		return r.callServe(ctx, op, request)
	}
	ctx, cancel := context.WithTimeout(ctx, r.timeout(op))
	defer cancel()

	cmd := exec.CommandContext(ctx, r.Bin, op)
	cmd.Env = r.Environ()
	cmd.Stdin = bytes.NewReader(request)
	cmd.Cancel = func() error { return terminate(cmd.Process) }
	cmd.WaitDelay = killGrace

	limit := r.MaxBytes
	if limit <= 0 {
		limit = provider.MaxMessageBytes
	}
	stdout := &limitedBuffer{limit: limit}
	cmd.Stdout = stdout
	stderr := r.stderrLogger(op)
	cmd.Stderr = stderr

	start := time.Now()
	err := cmd.Run()
	stderr.flush()
	r.logger().Debug("plugin call", "plugin", r.Name, "op", op, "duration", time.Since(start), "err", err)

	if stdout.exceeded {
		return nil, provider.Errorf(provider.Internal, "plugin %s: response exceeds %d bytes", r.Name, limit)
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		if errors.Is(ctxErr, context.DeadlineExceeded) {
			return nil, provider.Errorf(provider.Unavailable, "plugin %s %s timed out after %s", r.Name, op, r.timeout(op))
		}
		return nil, ctxErr
	}
	// A non-zero exit with a JSON error on stdout is a normal provider error;
	// the caller decodes it.
	if err != nil && stdout.Len() == 0 {
		return nil, provider.Errorf(provider.Internal, "plugin %s %s: %v", r.Name, op, err)
	}
	return stdout.Bytes(), nil
}

func (r *Runner) logger() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.New(slog.DiscardHandler)
}

// Environ returns the scrubbed environment the plugin runs with: a few basics
// such as PATH and HOME, plus the variables its manifest declares.
func (r *Runner) Environ() []string {
	names := append([]string{}, baseEnv...)
	if runtime.GOOS == "windows" {
		names = append(names, windowsEnv...)
	}
	names = append(names, r.Env...)
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
	return append(env, "TUG_PLUGIN=1")
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
