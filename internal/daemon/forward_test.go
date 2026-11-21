package daemon

import (
	"context"
	"errors"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/paingha/tugcli/internal/session"
)

// fakeForwarder stands in for client-go's port-forward: each attempt binds
// the pinned ports, or a fresh local port, and runs until it is killed.
type fakeForwarder struct {
	mu       sync.Mutex
	attempts int
	pinned   [][]string
	kill     chan struct{}
	failWith error
}

func (f *fakeForwarder) forward(ctx context.Context, fw Forward, pinned []string, up func(string, []string)) error {
	f.mu.Lock()
	f.attempts++
	n := f.attempts
	f.pinned = append(f.pinned, pinned)
	fail := f.failWith
	f.mu.Unlock()
	if fail != nil {
		return fail
	}
	bound := pinned
	if bound == nil {
		bound = []string{"54321:80"}
	}
	up("web-"+string(rune('a'+n-1)), bound)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-f.kill:
		return errors.New("lost connection to pod")
	}
}

func TestNamedForwardReconnectsOnTheSamePorts(t *testing.T) {
	ff := &fakeForwarder{kill: make(chan struct{})}
	s := New(Config{RuntimeDir: t.TempDir(), Forward: ff.forward})
	ctx := context.Background()

	st, err := s.StartForward(ctx, Forward{Name: "web", Target: "deploy/web", Ports: []string{":80"}})
	if err != nil {
		t.Fatal(err)
	}
	if st.State != ForwardUp || st.Pod != "web-a" || !slices.Equal(st.Bound, []string{"54321:80"}) {
		t.Fatalf("StartForward = %+v", st)
	}
	if _, err := s.StartForward(ctx, Forward{Name: "web"}); !errors.Is(err, ErrForwardExists) {
		t.Fatalf("a second forward named web = %v, want ErrForwardExists", err)
	}
	if s.idle() {
		t.Fatal("tugd would stop while a forward runs")
	}

	// The pod goes away; the forward starts again on the same local port.
	ff.kill <- struct{}{}
	deadline := time.Now().Add(5 * time.Second)
	for {
		fs := s.Forwards()
		if len(fs) == 1 && fs[0].Restarts == 1 && fs[0].State == ForwardUp && fs[0].Pod == "web-b" {
			if fs[0].LastError != "lost connection to pod" {
				t.Fatalf("LastError = %q", fs[0].LastError)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no reconnect: %+v", fs)
		}
		time.Sleep(20 * time.Millisecond)
	}
	ff.mu.Lock()
	second := ff.pinned[1]
	ff.mu.Unlock()
	if !slices.Equal(second, []string{"54321:80"}) {
		t.Fatalf("second attempt pinned %v, want the first attempt's ports", second)
	}

	if err := s.StopForward("web"); err != nil {
		t.Fatal(err)
	}
	if len(s.Forwards()) != 0 {
		t.Fatal("the forward is still listed after stop")
	}
	if err := s.StopForward("web"); !errors.Is(err, ErrForwardNotFound) {
		t.Fatalf("second stop = %v, want ErrForwardNotFound", err)
	}
}

func TestNamedForwardFirstFailureIsReturned(t *testing.T) {
	ff := &fakeForwarder{failWith: errors.New("deploy/web has no running pod")}
	s := New(Config{RuntimeDir: t.TempDir(), Forward: ff.forward})
	_, err := s.StartForward(context.Background(), Forward{Name: "web"})
	if err == nil || !strings.Contains(err.Error(), "no running pod") {
		t.Fatalf("StartForward = %v, want the first attempt's error", err)
	}
	if len(s.Forwards()) != 0 {
		t.Fatal("a forward that never started is still listed")
	}
	if _, err := s.StartForward(context.Background(), Forward{Name: "Bad Name"}); err == nil {
		t.Fatal("a forward name with spaces was accepted")
	}
}

// groupForwarder binds each forward's ports as asked, 5432 as 5432:5432,
// and fails at once for a target called broken.
type groupForwarder struct {
	mu       sync.Mutex
	attempts map[string]int
}

func (g *groupForwarder) forward(ctx context.Context, fw Forward, _ []string, up func(string, []string)) error {
	g.mu.Lock()
	if g.attempts == nil {
		g.attempts = map[string]int{}
	}
	g.attempts[fw.Name]++
	g.mu.Unlock()
	if fw.Target == "broken" {
		return errors.New("deploy/broken has no running pod")
	}
	var bound []string
	for _, p := range fw.Ports {
		local, remote, ok := strings.Cut(p, ":")
		if !ok {
			remote = local
		}
		bound = append(bound, local+":"+remote)
	}
	up(fw.Name+"-pod", bound)
	<-ctx.Done()
	return ctx.Err()
}

func names(states []ForwardState) []string {
	out := []string{}
	for _, st := range states {
		out = append(out, st.Name)
	}
	return out
}

func TestStartGroup(t *testing.T) {
	gf := &groupForwarder{}
	s := New(Config{RuntimeDir: t.TempDir(), Forward: gf.forward})
	taken := map[uint16]bool{6000: true} // another program has 6000
	s.probe = func(_ []string, port uint16) error {
		if taken[port] {
			return errors.New("address already in use")
		}
		return nil
	}
	ctx := context.Background()
	payments := []Forward{
		{Name: "payments.db", Target: "sts/db", Ports: []string{"5432"}},
		{Name: "payments.api", Target: "api", Ports: []string{"8080:http", ":9090"}},
		{Name: "payments.broken", Target: "broken", Ports: []string{"7000"}},
	}
	res, err := s.StartGroup(ctx, "payments", payments)
	if err != nil {
		t.Fatal(err)
	}
	if got := names(res.Started); !slices.Equal(got, []string{"payments.db", "payments.api"}) {
		t.Fatalf("started %v", got)
	}
	if len(res.Failed) != 1 || res.Failed[0].Name != "payments.broken" || !strings.Contains(res.Failed[0].Error, "no running pod") {
		t.Fatalf("failed = %+v", res.Failed)
	}
	for _, st := range s.Forwards() {
		if st.Group != "payments" || st.State != ForwardUp {
			t.Fatalf("forward %+v, want group payments and forwarding", st)
		}
	}

	// Starting it again leaves the running ones alone and tries the rest.
	res, err = s.StartGroup(ctx, "payments", payments)
	if err != nil {
		t.Fatal(err)
	}
	if got := names(res.Running); !slices.Equal(got, []string{"payments.db", "payments.api"}) || len(res.Started) != 0 || len(res.Failed) != 1 {
		t.Fatalf("second start: %+v", res)
	}
	gf.mu.Lock()
	if gf.attempts["payments.db"] != 1 || gf.attempts["payments.broken"] != 2 {
		t.Fatalf("attempts = %v; want db once and broken twice", gf.attempts)
	}
	gf.mu.Unlock()

	// An ad hoc forward outside any group holds a port too.
	if _, err := s.StartForward(ctx, Forward{Name: "adhoc", Target: "web", Ports: []string{"3000"}}); err != nil {
		t.Fatal(err)
	}

	// Conflicts are found before anything starts, all of them at once.
	conflicts := []struct {
		name     string
		forwards []Forward
		want     []string
		is       error
	}{
		{"a port another forward has", []Forward{{Name: "other.db", Target: "db", Ports: []string{"5432:5432"}}}, []string{"5432", "payments.db"}, ErrPortInUse},
		{"a port an ad hoc forward has", []Forward{{Name: "other.web", Target: "web", Ports: []string{"3000"}}}, []string{"3000", "adhoc"}, ErrPortInUse},
		{"a port another program has", []Forward{{Name: "other.x", Target: "x", Ports: []string{"6000"}}}, []string{"6000", "another program"}, ErrPortInUse},
		{"two forwards of the group on one port", []Forward{
			{Name: "other.a", Target: "a", Ports: []string{"7100"}},
			{Name: "other.b", Target: "b", Ports: []string{"7100:80"}},
		}, []string{"other.a and other.b both want 7100"}, ErrPortInUse},
		{"a name another group has", []Forward{{Name: "payments.db", Target: "db"}}, []string{"payments.db", "not part of other"}, ErrForwardExists},
	}
	for _, tt := range conflicts {
		t.Run(tt.name, func(t *testing.T) {
			before := len(s.Forwards())
			_, err := s.StartGroup(ctx, "other", append([]Forward{{Name: "other.fine", Target: "fine", Ports: []string{"7200"}}}, tt.forwards...))
			if !errors.Is(err, tt.is) {
				t.Fatalf("StartGroup = %v, want %v", err, tt.is)
			}
			for _, w := range tt.want {
				if !strings.Contains(err.Error(), w) {
					t.Fatalf("error %q does not mention %q", err, w)
				}
			}
			if after := len(s.Forwards()); after != before {
				t.Fatalf("%d forwards after a refused start, want %d: nothing should start", after, before)
			}
		})
	}
	if _, err := s.StartGroup(ctx, "Bad Group", nil); err == nil {
		t.Fatal("a group name with a space was accepted")
	}

	// Stopping the group ends its forwards and nothing else.
	if got := s.StopGroup("payments"); !slices.Equal(got, []string{"payments.api", "payments.db"}) {
		t.Fatalf("StopGroup = %v", got)
	}
	if got := names(s.Forwards()); !slices.Equal(got, []string{"adhoc"}) {
		t.Fatalf("after StopGroup: %v, want only adhoc", got)
	}
	if got := s.StopGroup("payments"); len(got) != 0 {
		t.Fatalf("StopGroup again = %v, want nothing", got)
	}
	_ = s.StopForward("adhoc")
}

func TestListenProbe(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := uint16(l.Addr().(*net.TCPAddr).Port)
	if err := listenProbe(nil, port); err == nil {
		t.Fatalf("listenProbe on a port in use (%d) succeeded", port)
	}
	if err := listenProbe([]string{"127.0.0.1"}, port); err == nil {
		t.Fatal("listenProbe on 127.0.0.1 with the port in use succeeded")
	}
	l.Close()
	if err := listenProbe([]string{"localhost"}, port); err != nil {
		t.Fatalf("listenProbe on a free port: %v", err)
	}
}

func TestPlannedPorts(t *testing.T) {
	got := plannedPorts([]string{"8080", "9000:http", ":80", "http", "0:80", "70000"})
	if want := []uint16{8080, 9000}; !slices.Equal(got, want) {
		t.Fatalf("plannedPorts = %v, want %v", got, want)
	}
}

// A forward whose pod went away says when it tries again.
func TestReconnectingForwardSaysWhenItRetries(t *testing.T) {
	kill := make(chan struct{})
	var mu sync.Mutex
	attempts := 0
	forward := func(ctx context.Context, _ Forward, pinned []string, up func(string, []string)) error {
		mu.Lock()
		attempts++
		n := attempts
		mu.Unlock()
		if n > 1 {
			return errors.New("no running pod")
		}
		up("web-a", []string{"54321:80"})
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-kill:
			return errors.New("lost connection to pod")
		}
	}
	s := New(Config{RuntimeDir: t.TempDir(), Forward: forward})
	if _, err := s.StartForward(context.Background(), Forward{Name: "web"}); err != nil {
		t.Fatal(err)
	}
	kill <- struct{}{}
	deadline := time.Now().Add(5 * time.Second)
	for {
		fs := s.Forwards()
		if fs[0].State == ForwardReconnecting && !fs[0].RetryAt.IsZero() {
			if fs[0].RetryAt.Before(fs[0].Since) || fs[0].Restarts < 1 {
				t.Fatalf("reconnecting forward: %+v", fs[0])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no retry time: %+v", fs)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := s.StopForward("web"); err != nil {
		t.Fatal(err)
	}
}

func TestGroupsThroughTheControlSocket(t *testing.T) {
	runtime := t.TempDir()
	gf := &groupForwarder{}
	s := New(Config{RuntimeDir: runtime, Sessions: session.NewManager(runtime), Forward: gf.forward})
	s.probe = func([]string, uint16) error { return nil }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	c := NewClient(runtime)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := c.Status(ctx); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("tugd did not start")
		}
		time.Sleep(20 * time.Millisecond)
	}

	res, err := c.StartGroup(ctx, "dev", []Forward{{Name: "dev.db", Target: "db", Ports: []string{"5432"}}})
	if err != nil || len(res.Started) != 1 || res.Started[0].Group != "dev" || !slices.Equal(res.Started[0].Bound, []string{"5432:5432"}) {
		t.Fatalf("StartGroup = %+v, %v", res, err)
	}
	_, err = c.StartGroup(ctx, "other", []Forward{{Name: "other.db", Target: "db", Ports: []string{"5432"}}})
	if err == nil || !strings.Contains(err.Error(), "dev.db") {
		t.Fatalf("a conflicting group through the socket: %v", err)
	}
	stopped, err := c.StopGroup(ctx, "dev")
	if err != nil || !slices.Equal(stopped, []string{"dev.db"}) {
		t.Fatalf("StopGroup = %v, %v", stopped, err)
	}
	if stopped, err := c.StopGroup(ctx, "dev"); err != nil || stopped == nil || len(stopped) != 0 {
		t.Fatalf("StopGroup with nothing running = %#v, %v; want an empty list", stopped, err)
	}
	s.Shutdown()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
