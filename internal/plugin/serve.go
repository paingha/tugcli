package plugin

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"github.com/paingha/tugcli/pkg/provider"
)

// server is a plugin process in serve mode: one process answers many calls,
// matched to their requests by ID, so tugd and commands that make several
// calls skip a process start each time. It starts on the first call and
// starts again after the process dies.
type server struct {
	r *Runner

	mu      sync.Mutex
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	writeMu sync.Mutex
	pending map[string]chan []byte
	next    uint64
	done    chan struct{} // closed when the process has exited
	exitErr error
	closed  bool
}

func (r *Runner) callServe(ctx context.Context, op string, request []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout(op))
	defer cancel()
	r.srvMu.Lock()
	if r.srv == nil {
		r.srv = &server{r: r}
	}
	s := r.srv
	r.srvMu.Unlock()

	id, ch, done, err := s.register(op)
	if err != nil {
		return nil, err
	}
	defer s.unregister(id)
	frame := provider.ServeRequest{ID: id, Op: op}
	if len(request) > 0 {
		frame.Request = request
	}
	if err := s.send(frame); err != nil {
		return nil, provider.Errorf(provider.Internal, "plugin %s: sending %s: %v", r.Name, op, err)
	}
	select {
	case resp := <-ch:
		return resp, nil
	case <-done:
		return nil, provider.Errorf(provider.Internal, "plugin %s stopped while answering %s: %v", r.Name, op, s.exitError())
	case <-ctx.Done():
		_ = s.send(provider.ServeRequest{ID: id, Cancel: true}) // best effort
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, provider.Errorf(provider.Unavailable, "plugin %s %s timed out after %s", r.Name, op, r.timeout(op))
		}
		return nil, ctx.Err()
	}
}

// register starts the process if it is not running and reserves an ID.
func (s *server) register(op string) (string, chan []byte, chan struct{}, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return "", nil, nil, provider.Errorf(provider.Unavailable, "plugin %s has been stopped", s.r.Name)
	}
	if s.cmd == nil {
		if err := s.start(); err != nil {
			return "", nil, nil, provider.Errorf(provider.Internal, "starting plugin %s in serve mode: %v", s.r.Name, err)
		}
	}
	s.next++
	id := strconv.FormatUint(s.next, 10)
	ch := make(chan []byte, 1)
	s.pending[id] = ch
	s.r.logger().Debug("plugin call", "plugin", s.r.Name, "op", op, "id", id, "mode", "serve")
	return id, ch, s.done, nil
}

func (s *server) unregister(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.pending, id)
}

// start runs the process; the caller holds mu.
func (s *server) start() error {
	cmd := exec.Command(s.r.Bin, provider.OpServe) //nolint:gosec // the plugin's own checked binary
	cmd.Env = s.r.Environ()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr := s.r.stderrLogger(provider.OpServe)
	cmd.Stderr = stderr
	cmd.WaitDelay = killGrace
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan struct{})
	s.cmd, s.stdin, s.pending, s.done = cmd, stdin, map[string]chan []byte{}, done
	go s.read(cmd, stdout, stderr, done)
	return nil
}

// read delivers responses until the process's stdout ends, then reaps the
// process and lets the next call start a new one.
func (s *server) read(cmd *exec.Cmd, stdout io.Reader, stderr *lineLogger, done chan struct{}) {
	limit := s.r.MaxBytes
	if limit <= 0 {
		limit = provider.MaxMessageBytes
	}
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64<<10), int(limit)+64<<10)
	for sc.Scan() {
		var resp provider.ServeResponse
		if err := json.Unmarshal(sc.Bytes(), &resp); err != nil {
			s.r.logger().Warn("plugin wrote a line that is not a serve response", "plugin", s.r.Name, "err", err)
			continue
		}
		s.mu.Lock()
		ch, ok := s.pending[resp.ID]
		s.mu.Unlock()
		if ok {
			ch <- resp.Response // buffered; each ID gets one answer
		} else if resp.ID == "" {
			s.r.logger().Warn("plugin refused a serve request", "plugin", s.r.Name, "response", string(resp.Response))
		}
	}
	scanErr := sc.Err()
	waitErr := cmd.Wait()
	stderr.flush()
	s.mu.Lock()
	s.exitErr = errors.Join(scanErr, waitErr)
	if s.exitErr == nil {
		s.exitErr = errors.New("it exited")
	}
	if s.cmd == cmd {
		s.cmd, s.stdin = nil, nil
	}
	s.mu.Unlock()
	close(done)
}

func (s *server) send(frame provider.ServeRequest) error {
	b, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	s.mu.Lock()
	w := s.stdin
	s.mu.Unlock()
	if w == nil {
		return errors.New("the plugin is not running")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err = w.Write(append(b, '\n'))
	return err
}

// process returns the running process and the channel closed when it
// exits, or nil when none is running.
func (s *server) process() (*exec.Cmd, chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cmd, s.done
}

func (s *server) exitError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exitErr
}

// close ends the process: stdin closes so it can finish in-flight work,
// then SIGTERM after the grace period, then SIGKILL.
func (s *server) close() error {
	s.mu.Lock()
	s.closed = true
	cmd, stdin, done := s.cmd, s.stdin, s.done
	s.mu.Unlock()
	if cmd == nil {
		return nil
	}
	_ = stdin.Close()
	select {
	case <-done:
		return nil
	case <-time.After(killGrace):
	}
	_ = terminate(cmd.Process)
	select {
	case <-done:
		return nil
	case <-time.After(killGrace):
		_ = cmd.Process.Kill()
		<-done
		return fmt.Errorf("plugin %s did not stop and was killed", s.r.Name)
	}
}

// Close stops the runner's serve-mode process, if one is running.
func (r *Runner) Close() error {
	r.srvMu.Lock()
	s := r.srv
	r.srvMu.Unlock()
	if s == nil {
		return nil
	}
	return s.close()
}
