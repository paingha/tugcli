package fanout

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// lines collects what a sink is given, as "cluster stream text".
type lines struct {
	mu  sync.Mutex
	got []string
}

func (l *lines) sink() Sink {
	return Sink{Line: func(cluster string, stream Stream, text string) {
		l.mu.Lock()
		defer l.mu.Unlock()
		name := "out"
		if stream == Stderr {
			name = "err"
		}
		l.got = append(l.got, cluster+" "+name+" "+text)
	}}
}

func (l *lines) sorted() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Sorted(slices.Values(l.got))
}

func TestRunSplitsLinesAndKeepsOrder(t *testing.T) {
	var l lines
	boom := errors.New("boom")
	results := Run(context.Background(), []string{"eu", "us", "ap"}, 2, l.sink(),
		func(_ context.Context, cluster string, stdout, stderr io.Writer) error {
			// Lines arrive in pieces, with CRLF, and the last has no newline.
			fmt.Fprint(stdout, "NAME\r\npod-")
			fmt.Fprint(stdout, cluster+"\nlast")
			if cluster == "ap" {
				fmt.Fprintln(stderr, "error: unreachable")
				return boom
			}
			return nil
		})

	want := []string{
		"ap err error: unreachable", "ap out NAME", "ap out last", "ap out pod-ap",
		"eu out NAME", "eu out last", "eu out pod-eu",
		"us out NAME", "us out last", "us out pod-us",
	}
	if got := l.sorted(); !slices.Equal(got, want) {
		t.Errorf("lines = %q, want %q", got, want)
	}
	if len(results) != 3 || results[0].Cluster != "eu" || results[1].Cluster != "us" || results[2].Cluster != "ap" {
		t.Fatalf("results = %+v, want eu, us, ap in order", results)
	}
	if results[0].Failed() || results[1].Failed() || !results[2].Failed() || !errors.Is(results[2].Err, boom) {
		t.Errorf("results = %+v, want only ap failed", results)
	}
}

func TestRunBoundsParallelism(t *testing.T) {
	tests := []struct {
		parallel, want int
	}{
		{1, 1},
		{3, 3},
		{0, DefaultParallel},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprint(tt.parallel), func(t *testing.T) {
			clusters := make([]string, 20)
			for i := range clusters {
				clusters[i] = fmt.Sprintf("c%d", i)
			}
			var running, peak atomic.Int32
			Run(context.Background(), clusters, tt.parallel, Sink{}, func(context.Context, string, io.Writer, io.Writer) error {
				n := running.Add(1)
				for {
					p := peak.Load()
					if n <= p || peak.CompareAndSwap(p, n) {
						break
					}
				}
				time.Sleep(5 * time.Millisecond)
				running.Add(-1)
				return nil
			})
			if got := int(peak.Load()); got != tt.want {
				t.Errorf("Run(parallel %d) ran %d at once, want %d", tt.parallel, got, tt.want)
			}
		})
	}
}

func TestRunStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan string, 4)
	done := make(chan []Result, 1)
	go func() {
		done <- Run(ctx, []string{"a", "b", "c", "d"}, 2, Sink{}, func(ctx context.Context, cluster string, _, _ io.Writer) error {
			started <- cluster
			<-ctx.Done() // a stream that runs until interrupted
			return ctx.Err()
		})
	}()
	<-started
	<-started
	cancel()
	var results []Result
	select {
	case results = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}
	var states []string
	for _, r := range results {
		switch r.State {
		case Interrupted:
			states = append(states, r.Cluster+" interrupted")
		case Skipped:
			states = append(states, r.Cluster+" skipped")
		default:
			states = append(states, r.Cluster+" finished")
		}
		if r.Failed() {
			t.Errorf("%s counted as failed: %+v", r.Cluster, r)
		}
	}
	if got, want := strings.Join(states, ", "), "a interrupted, b interrupted, c skipped, d skipped"; got != want {
		t.Errorf("states = %s, want %s", got, want)
	}
}

// A line without a newline is not held back for ever, as a progress bar's
// would be.
func TestLongLinesAreNotHeldBack(t *testing.T) {
	var got []int
	w := &lineWriter{emit: func(s string) { got = append(got, len(s)) }}
	if _, err := w.Write([]byte(strings.Repeat("x", maxLine+10))); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []int{maxLine + 10}) {
		t.Errorf("emitted lengths before flush = %v, want [%d]", got, maxLine+10)
	}
	w.flush()
	if len(got) != 1 {
		t.Errorf("flush emitted again: %v", got)
	}
}
