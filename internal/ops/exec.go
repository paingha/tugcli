package ops

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"

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

	return stream(ctx, cfg, req.URL(), remotecommand.StreamOptions{
		Stdin:             o.Stdin,
		Stdout:            o.Stdout,
		Stderr:            o.Stderr,
		Tty:               o.TTY,
		TerminalSizeQueue: o.Sizes,
	})
}

// AttachOptions describe a connection to a running container's console.
type AttachOptions struct {
	Namespace string
	Pod       string
	Container string

	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	TTY    bool
	// Sizes reports terminal resizes when TTY is set.
	Sizes remotecommand.TerminalSizeQueue
}

// Attach connects to the console of a running container, as kubectl attach
// does, over WebSockets with a SPDY fallback. The session guard sees it as a
// write.
func Attach(ctx context.Context, cfg *rest.Config, cs kubernetes.Interface, o AttachOptions) error {
	req := cs.CoreV1().RESTClient().Post().
		Resource("pods").Namespace(o.Namespace).Name(o.Pod).SubResource("attach").
		VersionedParams(&corev1.PodAttachOptions{
			Container: o.Container,
			Stdin:     o.Stdin != nil,
			Stdout:    o.Stdout != nil,
			Stderr:    o.Stderr != nil && !o.TTY,
			TTY:       o.TTY,
		}, scheme.ParameterCodec)
	return stream(ctx, cfg, req.URL(), remotecommand.StreamOptions{
		Stdin:             o.Stdin,
		Stdout:            o.Stdout,
		Stderr:            o.Stderr,
		Tty:               o.TTY,
		TerminalSizeQueue: o.Sizes,
	})
}

// stream connects local streams to an exec or attach URL.
func stream(ctx context.Context, cfg *rest.Config, u *url.URL, o remotecommand.StreamOptions) error {
	exec, err := newExecutor(cfg, u)
	if err != nil {
		return err
	}
	if o.Tty {
		o.Stderr = nil // a TTY merges stderr into stdout
	}
	return exec.StreamWithContext(ctx, o)
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

// NoShell reports whether Exec failed because the container has no /bin/sh
// to run, as in distroless images. Container runtimes say so only in the
// error's text ("stat /bin/sh: no such file or directory" from runc,
// "executable file `/bin/sh` not found" from crun), so this is the one
// place tug reads an error message.
func NoShell(err error) bool {
	if err == nil {
		return false
	}
	if _, ok := ExitCode(err); ok {
		return false // the shell ran and exited
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "/bin/sh") &&
		(strings.Contains(msg, "no such file or directory") || strings.Contains(msg, "not found"))
}
