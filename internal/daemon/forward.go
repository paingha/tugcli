package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"regexp"
	"slices"
	"strconv"
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
	// Group, when set, is the set of forwards this one was started with,
	// such as a profile's, which StopGroup ends together.
	Group string `json:"group,omitempty"`
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
	// RetryAt is when a reconnecting forward next tries a pod.
	RetryAt time.Time `json:"retryAt,omitzero"`
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
	// ErrPortInUse is returned when a local port a forward asks for is
	// taken, by another forward or by another program.
	ErrPortInUse = errors.New("local port in use")
)

var validForwardName = regexp.MustCompile(`^[a-z0-9]([a-z0-9._-]{0,61}[a-z0-9])?$`)

// Backoff between attempts of a forward that keeps failing: tugd starts a
// forward that dropped again after ForwardMinBackoff, doubling the wait
// each time up to ForwardMaxBackoff.
const (
	ForwardMinBackoff = time.Second
	ForwardMaxBackoff = 30 * time.Second
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
	backoff := ForwardMinBackoff
	for {
		started := s.cfg.Now()
		up := func(pod string, bound []string) {
			s.mu.Lock()
			r.state.State, r.state.Pod, r.state.Bound, r.state.Since = ForwardUp, pod, bound, s.cfg.Now()
			r.state.RetryAt = time.Time{}
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
			backoff = ForwardMinBackoff
		}
		msg := "the port-forward ended"
		if err != nil {
			msg = err.Error()
		}
		s.mu.Lock()
		r.state.State, r.state.LastError, r.state.Pod = ForwardReconnecting, msg, ""
		r.state.RetryAt = s.cfg.Now().Add(backoff)
		r.state.Restarts++
		s.mu.Unlock()
		s.cfg.Log.Warn("port-forward ended; starting again", "forward", r.state.Name, "err", msg, "in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, ForwardMaxBackoff)
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

// GroupResult is what starting a group of forwards did.
type GroupResult struct {
	Group string `json:"group"`
	// Started are the forwards started now.
	Started []ForwardState `json:"started"`
	// Running were running already and are left as they are.
	Running []ForwardState `json:"running"`
	// Failed did not start.
	Failed []ForwardFailure `json:"failed"`
}

// ForwardFailure is a forward of a group that did not start.
type ForwardFailure struct {
	Name  string `json:"name"`
	Error string `json:"error"`
}

// StartGroup starts the forwards of a group, such as a profile's, that are
// not running yet; ones of the group that are running are left alone, so
// starting a group again is safe. It first checks every local port the new
// forwards ask for, against each other, against tugd's other forwards and
// against the rest of the machine, and starts none of them when a port is
// taken or a name belongs to another group. A forward whose first attempt
// fails, for example because its workload has no running pod, is reported
// in the result while the others keep running.
func (s *Server) StartGroup(ctx context.Context, group string, forwards []Forward) (GroupResult, error) {
	res := GroupResult{Group: group, Started: []ForwardState{}, Running: []ForwardState{}, Failed: []ForwardFailure{}}
	if !validForwardName.MatchString(group) {
		return res, fmt.Errorf("group name %q must be lower-case letters, digits, dots, dashes and underscores", group)
	}
	if s.cfg.Forward == nil {
		return res, errors.New("this tugd cannot run port-forwards")
	}
	for _, f := range forwards {
		if !validForwardName.MatchString(f.Name) {
			return res, fmt.Errorf("forward name %q must be lower-case letters, digits, dots, dashes and underscores", f.Name)
		}
	}

	var todo []Forward
	var conflicts []error
	claimed := map[uint16]string{} // local port to the forward that has it
	s.mu.Lock()
	for name, r := range s.forwards {
		for _, port := range localPortsOf(r.state) {
			claimed[port] = name
		}
	}
	for _, f := range forwards {
		f.Group = group
		r, ok := s.forwards[f.Name]
		switch {
		case !ok:
			todo = append(todo, f)
		case r.state.Group == group:
			res.Running = append(res.Running, r.state)
		default:
			conflicts = append(conflicts, fmt.Errorf("%w: %s, which is not part of %s; stop it with `tug pf stop %s`",
				ErrForwardExists, f.Name, group, f.Name))
		}
	}
	s.mu.Unlock()

	asked := map[uint16]string{}
	for _, f := range todo {
		for _, port := range plannedPorts(f.Ports) {
			holder, held := claimed[port]
			switch {
			case held:
				conflicts = append(conflicts, fmt.Errorf("%w: %s wants %d, where the forward %s listens; stop it with `tug pf stop %s`",
					ErrPortInUse, f.Name, port, holder, holder))
			case asked[port] != "":
				conflicts = append(conflicts, fmt.Errorf("%w: %s and %s both want %d", ErrPortInUse, asked[port], f.Name, port))
			default:
				if err := s.probe(f.Addresses, port); err != nil {
					conflicts = append(conflicts, fmt.Errorf("%w: %s wants %d, which another program on this machine has: %w",
						ErrPortInUse, f.Name, port, err))
				}
			}
			asked[port] = f.Name
		}
	}
	if len(conflicts) > 0 {
		return res, errors.Join(conflicts...)
	}

	type outcome struct {
		st  ForwardState
		err error
	}
	outcomes := make([]outcome, len(todo))
	var wg sync.WaitGroup
	for i, f := range todo {
		// Each waits for its own first attempt, which StartForward bounds.
		wg.Go(func() {
			st, err := s.StartForward(ctx, f)
			outcomes[i] = outcome{st, err}
		})
	}
	wg.Wait()
	for i, o := range outcomes {
		if o.err != nil {
			res.Failed = append(res.Failed, ForwardFailure{Name: todo[i].Name, Error: o.err.Error()})
			continue
		}
		res.Started = append(res.Started, o.st)
	}
	return res, nil
}

// StopGroup ends every forward of a group and returns their names.
func (s *Server) StopGroup(group string) []string {
	s.mu.Lock()
	var names []string
	for name, r := range s.forwards {
		if r.state.Group == group {
			names = append(names, name)
		}
	}
	s.mu.Unlock()
	slices.Sort(names)
	stopped := []string{}
	for _, name := range names {
		if s.StopForward(name) == nil {
			stopped = append(stopped, name)
		}
	}
	return stopped
}

// localPortsOf returns the local ports a forward holds: the ones bound, or
// before its first attempt is up, the ones it asked for.
func localPortsOf(st ForwardState) []uint16 {
	if st.Bound == nil {
		return plannedPorts(st.Ports)
	}
	return plannedPorts(st.Bound)
}

// plannedPorts returns the local ports that port specs ask for: 8080 and
// 8080:http ask for 8080. :80 asks for any free port and a bare port name
// listens on a number known only once the pod is found, so neither can be
// checked in advance.
func plannedPorts(specs []string) []uint16 {
	var out []uint16
	for _, spec := range specs {
		local, _, _ := strings.Cut(spec, ":")
		n, err := strconv.ParseUint(local, 10, 16)
		if err != nil || n == 0 {
			continue
		}
		out = append(out, uint16(n))
	}
	return out
}

// listenProbe checks that port is free on each address by listening on it
// for a moment. localhost is checked on 127.0.0.1, where clients look.
func listenProbe(addresses []string, port uint16) error {
	if len(addresses) == 0 {
		addresses = []string{"localhost"}
	}
	for _, addr := range addresses {
		if addr == "localhost" {
			addr = "127.0.0.1"
		}
		l, err := net.Listen("tcp", net.JoinHostPort(addr, strconv.Itoa(int(port))))
		if err != nil {
			return err
		}
		if err := l.Close(); err != nil {
			return err
		}
	}
	return nil
}
