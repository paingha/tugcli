package daemon

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
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
