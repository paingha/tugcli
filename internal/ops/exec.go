package ops

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/streaming/pkg/httpstream"
)

// shellScript starts bash when the image has it and sh otherwise.
const shellScript = "if command -v bash >/dev/null 2>&1; then exec bash; else exec sh; fi"

// ShellCommand is the command `tug sh` runs when none is given.
func ShellCommand() []string { return []string{"/bin/sh", "-c", shellScript} }

// ExecOptions describe a command to run in a container.
type ExecOptions struct {
	Namespace string
	Pod       string
	Container string
	// Command defaults to ShellCommand.
	Command []string

	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	TTY    bool
	// Sizes reports terminal resizes when TTY is set.
	Sizes remotecommand.TerminalSizeQueue
}

// Exec runs a command in a container, over WebSockets with a SPDY fallback
// for older API servers. The session guard sees it as a write.
func Exec(ctx context.Context, cfg *rest.Config, cs kubernetes.Interface, o ExecOptions) error {
	cmd := o.Command
	if len(cmd) == 0 {
		cmd = ShellCommand()
	}
	req := cs.CoreV1().RESTClient().Post().
		Resource("pods").Namespace(o.Namespace).Name(o.Pod).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: o.Container,
			Command:   cmd,
			Stdin:     o.Stdin != nil,
			Stdout:    o.Stdout != nil,
			Stderr:    o.Stderr != nil && !o.TTY,
			TTY:       o.TTY,
		}, scheme.ParameterCodec)

	exec, err := newExecutor(cfg, req.URL())
	if err != nil {
		return err
	}
	stderr := o.Stderr
	if o.TTY {
		stderr = nil // a TTY merges stderr into stdout
	}
	return exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin:             o.Stdin,
		Stdout:            o.Stdout,
		Stderr:            stderr,
		Tty:               o.TTY,
		TerminalSizeQueue: o.Sizes,
	})
}

func newExecutor(cfg *rest.Config, u *url.URL) (remotecommand.Executor, error) {
	spdy, err := remotecommand.NewSPDYExecutor(cfg, http.MethodPost, u)
	if err != nil {
		return nil, err
	}
	ws, err := remotecommand.NewWebSocketExecutor(cfg, http.MethodGet, u.String())
	if err != nil {
		return nil, err
	}
	return remotecommand.NewFallbackExecutor(ws, spdy, shouldFallback)
}

// shouldFallback reports whether a WebSocket failure means the server or a
// proxy only speaks SPDY.
func shouldFallback(err error) bool {
	return httpstream.IsUpgradeFailure(err) || httpstream.IsHTTPSProxyError(err)
}

// ExitCode extracts a remote command's exit code from Exec's error.
func ExitCode(err error) (int, bool) {
	var ee interface{ ExitStatus() int }
	if errors.As(err, &ee) {
		return ee.ExitStatus(), true
	}
	return 0, false
}
