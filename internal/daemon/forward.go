package daemon

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

// Forward is a named port-forward that tugd keeps open, starting again on
// a new pod when the old one goes away.
type Forward struct {
	Name      string `json:"name"`
	ClusterID string `json:"clusterID"`
	// Cluster is the cluster's name, for display.
	Cluster   string `json:"cluster"`
	Namespace string `json:"namespace"`
	// Target is the resolved workload or pod, such as deploy/api-server.
	Target string `json:"target"`
	// Ports are as the user gave them: 8080, 9000:http, :80.
	Ports     []string `json:"ports,omitempty"`
	Addresses []string `json:"addresses,omitempty"`
}

// Forward states.
const (
	ForwardStarting     = "starting"
	ForwardUp           = "forwarding"
	ForwardReconnecting = "reconnecting"
)

// ForwardState is a forward and how it is doing.
type ForwardState struct {
	Forward
	State string `json:"state"`
	// Bound are the local:remote ports listening now. After the first
	// start the local ports stay the same across reconnects.
	Bound     []string  `json:"bound,omitempty"`
	Pod       string    `json:"pod,omitempty"`
	Since     time.Time `json:"since"`
	Restarts  int       `json:"restarts"`
	LastError string    `json:"lastError,omitempty"`
}

// ForwardFunc runs one attempt of a forward until it fails or ctx ends. It
// forwards to pinned when set (local:remote pairs from an earlier attempt)
// instead of f.Ports, and calls up once it is listening, with the pod and
// the ports bound.
type ForwardFunc func(ctx context.Context, f Forward, pinned []string, up func(pod string, bound []string)) error

// Errors from the forward API.
var (
	ErrForwardExists   = errors.New("a forward with that name is running")
	ErrForwardNotFound = errors.New("no forward with that name")
)

var validForwardName = regexp.MustCompile(`^[a-z0-9]([a-z0-9._-]{0,61}[a-z0-9])?$`)

// Backoff between attempts of a forward that keeps failing.
const (
	forwardMinBackoff = time.Second
	forwardMaxBackoff = 30 * time.Second
	// A forward that stayed up this long starts its backoff again.
	forwardStable = time.Minute
	// startWait bounds how long StartForward waits for the first attempt.
	startWait = 30 * time.Second
)

type forwardRunner struct {
	state  ForwardState
	cancel context.CancelFunc
	done   chan struct{}
	// first is closed when the first attempt is up or has failed.
	first     chan struct{}
	firstErr  error
	firstOnce sync.Once
}

// StartForward starts a named forward and waits for it to be listening. A
// first attempt that fails, for example because the workload has no running
// pod, removes the forward and returns the error.
func (s *Server) StartForward(ctx context.Context, f Forward) (ForwardState, error) {
	if !validForwardName.MatchString(f.Name) {
		return ForwardState{}, fmt.Errorf("forward name %q must be lower-case letters, digits, dots, dashes and underscores", f.Name)
	}
	if s.cfg.Forward == nil {
		return ForwardState{}, errors.New("this tugd cannot run port-forwards")
	}
	s.mu.Lock()
	if _, ok := s.forwards[f.Name]; ok {
		s.mu.Unlock()
		return ForwardState{}, fmt.Errorf("%w: %s; stop it first with `tug pf stop %s`", ErrForwardExists, f.Name, f.Name)
	}
	rctx, cancel := context.WithCancel(context.Background()) // outlives the request
	r := &forwardRunner{
		state:  ForwardState{Forward: f, State: ForwardStarting, Since: s.cfg.Now()},
		cancel: cancel,
		done:   make(chan struct{}),
		first:  make(chan struct{}),
	}
	s.forwards[f.Name] = r
	s.lastActive = s.cfg.Now()
	s.mu.Unlock()
	go s.supervise(rctx, r)

	select {
	case <-r.first:
		// firstErr is set before first closes.
		if r.firstErr != nil {
			_ = s.StopForward(f.Name)
			return ForwardState{}, r.firstErr
		}
	case <-ctx.Done():
	case <-time.After(startWait):
	}
	return s.forwardState(f.Name)
}

// supervise runs a forward's attempts until it is stopped.
func (s *Server) supervise(ctx context.Context, r *forwardRunner) {
	defer close(r.done)
	backoff := forwardMinBackoff
	for {
		started := s.cfg.Now()
		up := func(pod string, bound []string) {
			s.mu.Lock()
			r.state.State, r.state.Pod, r.state.Bound, r.state.Since = ForwardUp, pod, bound, s.cfg.Now()
			s.mu.Unlock()
			r.firstOnce.Do(func() { close(r.first) })
			s.cfg.Log.Info("forwarding", "forward", r.state.Name, "pod", pod, "ports", strings.Join(bound, ","))
		}
		s.mu.Lock()
		f, pinned := r.state.Forward, r.state.Bound
		s.mu.Unlock()
		err := s.cfg.Forward(ctx, f, pinned, up)
		if ctx.Err() != nil {
			return
		}
		s.mu.Lock()
		everUp := r.state.Bound != nil
		s.mu.Unlock()
		if !everUp {
			r.firstErr = err
			if r.firstErr == nil {
				r.firstErr = errors.New("the port-forward ended at once")
			}
			r.firstOnce.Do(func() { close(r.first) })
			return
		}
		if s.cfg.Now().Sub(started) > forwardStable {
			backoff = forwardMinBackoff
		}
		msg := "the port-forward ended"
		if err != nil {
			msg = err.Error()
		}
		s.mu.Lock()
		r.state.State, r.state.LastError, r.state.Pod = ForwardReconnecting, msg, ""
		r.state.Restarts++
		s.mu.Unlock()
		s.cfg.Log.Warn("port-forward ended; starting again", "forward", r.state.Name, "err", msg, "in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, forwardMaxBackoff)
	}
}

// StopForward ends a named forward.
func (s *Server) StopForward(name string) error {
	s.mu.Lock()
	r, ok := s.forwards[name]
	delete(s.forwards, name)
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("%w: %s", ErrForwardNotFound, name)
	}
	r.cancel()
	<-r.done
	return nil
}

// Forwards lists the named forwards by name.
func (s *Server) Forwards() []ForwardState {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ForwardState, 0, len(s.forwards))
	for _, r := range s.forwards {
		out = append(out, r.state)
	}
	slices.SortFunc(out, func(a, b ForwardState) int { return strings.Compare(a.Name, b.Name) })
	return out
}

func (s *Server) forwardState(name string) (ForwardState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.forwards[name]
	if !ok {
		return ForwardState{}, fmt.Errorf("%w: %s", ErrForwardNotFound, name)
	}
	return r.state, nil
}

// stopForwards ends every forward, as tugd stops.
func (s *Server) stopForwards() {
	s.mu.Lock()
	names := make([]string, 0, len(s.forwards))
	for n := range s.forwards {
		names = append(names, n)
	}
	s.mu.Unlock()
	for _, n := range names {
		_ = s.StopForward(n)
	}
}
