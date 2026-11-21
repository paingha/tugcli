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

// server is a plugin in serve mode: one process, or one module instance,
// answers many calls, matched to their requests by ID, so tugd and commands
// that make several calls skip a start each time. It starts on the first
// call and starts again after the plugin dies.
type server struct {
	r *Runner

	mu      sync.Mutex
	inst    instance
	stdin   io.WriteCloser
	writeMu sync.Mutex
	pending map[string]chan []byte
	next    uint64
	done    chan struct{} // closed when the plugin has exited
	exitErr error
	closed  bool
}

// instance is a running serve-mode plugin: a process, or a WebAssembly
// module instance (*wasmInstance).
type instance interface {
	// wait blocks until the plugin has exited.
	wait() error
	// terminate asks it to stop at once.
	terminate() error
	// kill stops it when terminate did not.
	kill() error
}

// procInstance is a plugin process.
type procInstance struct{ cmd *exec.Cmd }

func (p *procInstance) wait() error      { return p.cmd.Wait() }
func (p *procInstance) terminate() error { return terminate(p.cmd.Process) }
func (p *procInstance) kill() error      { return p.cmd.Process.Kill() }

func (r *Runner) callServe(ctx context.Context, op string, request []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout(op))
	defer cancel()
	r.srvMu.Lock()
	if r.srv == nil {
		r.srv = &server{r: r}
	}
	s := r.srv
	r.srvMu.Unlock()

	id, ch, inst, done, err := s.register(op)
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
		err := s.exitError()
		e := provider.Errorf(provider.Internal, "plugin %s stopped while answering %s: %v", r.Name, op, err)
		var mle *memoryLimitError
		if errors.As(err, &mle) {
			e = e.WithHint(memoryHint)
		}
		return nil, e
	case <-ctx.Done():
		_ = s.send(provider.ServeRequest{ID: id, Cancel: true}) // best effort
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			if r.WASM != nil {
				// A module answers one request at a time, so one that ran
				// out of time holds up every request behind it. The next
				// call starts a fresh instance.
				s.stop(inst)
			}
			return nil, provider.Errorf(provider.Unavailable, "plugin %s %s timed out after %s", r.Name, op, r.timeout(op))
		}
		return nil, ctx.Err()
	}
}

// register starts the plugin if it is not running and reserves an ID.
func (s *server) register(op string) (string, chan []byte, instance, chan struct{}, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return "", nil, nil, nil, provider.Errorf(provider.Unavailable, "plugin %s has been stopped", s.r.Name)
	}
	if s.inst == nil {
		if err := s.start(); err != nil {
			return "", nil, nil, nil, provider.Errorf(provider.Internal, "starting plugin %s in serve mode: %v", s.r.Name, err)
		}
	}
	s.next++
	id := strconv.FormatUint(s.next, 10)
	ch := make(chan []byte, 1)
	s.pending[id] = ch
	s.r.logger().Debug("plugin call", "plugin", s.r.Name, "op", op, "id", id, "mode", "serve")
	return id, ch, s.inst, s.done, nil
}

func (s *server) unregister(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.pending, id)
}

// start runs the plugin; the caller holds mu.
func (s *server) start() error {
	stderr := s.r.stderrLogger(provider.OpServe)
	start := s.r.startProcess
	if s.r.WASM != nil {
		start = s.r.startWASM
	}
	inst, stdin, stdout, err := start(stderr)
	if err != nil {
		return err
	}
	done := make(chan struct{})
	s.inst, s.stdin, s.pending, s.done = inst, stdin, map[string]chan []byte{}, done
	go s.read(inst, stdout, stderr, done)
	return nil
}

// startProcess starts the plugin's executable in serve mode.
func (r *Runner) startProcess(stderr io.Writer) (instance, io.WriteCloser, io.ReadCloser, error) {
	cmd := exec.Command(r.Bin, provider.OpServe) //nolint:gosec // the plugin's own checked binary
	cmd.Env = r.Environ()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, nil, err
	}
	cmd.Stderr = stderr
	cmd.WaitDelay = killGrace
	if err := cmd.Start(); err != nil {
		return nil, nil, nil, err
	}
	return &procInstance{cmd: cmd}, stdin, stdout, nil
}

// read delivers responses until the plugin's stdout ends, then reaps it and
// lets the next call start a new one.
func (s *server) read(inst instance, stdout io.ReadCloser, stderr *lineLogger, done chan struct{}) {
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64<<10), int(s.r.maxBytes())+64<<10)
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
	if scanErr != nil {
		// A line too long to read: nothing after it can be matched.
		_ = inst.terminate()
	}
	_ = stdout.Close() // so a plugin still writing fails rather than blocks
	waitErr := inst.wait()
	stderr.flush()
	s.mu.Lock()
	s.exitErr = errors.Join(scanErr, waitErr)
	if s.exitErr == nil {
		s.exitErr = errors.New("it exited")
	}
	if s.inst == inst {
		s.inst, s.stdin = nil, nil
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

// running returns the running plugin and the channel closed when it exits,
// or nil when none is running.
func (s *server) running() (instance, chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inst, s.done
}

// stop terminates inst if it is still the one running. It is forgotten at
// once, so the next call starts another without waiting for it to end.
func (s *server) stop(inst instance) {
	s.mu.Lock()
	current := s.inst == inst
	if current {
		s.inst, s.stdin = nil, nil
	}
	s.mu.Unlock()
	if current {
		s.r.logger().Debug("stopping a plugin that ran out of time", "plugin", s.r.Name)
		_ = inst.terminate()
	}
}

func (s *server) exitError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exitErr
}

// close ends the plugin: stdin closes so it can finish in-flight work, then
// it is terminated after the grace period, then killed.
func (s *server) close() error {
	s.mu.Lock()
	s.closed = true
	inst, stdin, done := s.inst, s.stdin, s.done
	s.mu.Unlock()
	if inst == nil {
		return nil
	}
	_ = stdin.Close()
	select {
	case <-done:
		return nil
	case <-time.After(killGrace):
	}
	_ = inst.terminate()
	select {
	case <-done:
		return nil
	case <-time.After(killGrace):
		_ = inst.kill()
		<-done
		return fmt.Errorf("plugin %s did not stop and was killed", s.r.Name)
	}
}

// Close stops the runner's serve-mode plugin, if one is running, and frees a
// WebAssembly plugin's compiled module.
func (r *Runner) Close() error {
	r.srvMu.Lock()
	s := r.srv
	r.srvMu.Unlock()
	var err error
	if s != nil {
		err = s.close()
	}
	r.wasmMu.Lock()
	h := r.wasm
	r.wasm = nil
	r.wasmMu.Unlock()
	if h != nil {
		err = errors.Join(err, h.close())
	}
	return err
}
