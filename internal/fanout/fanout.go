// Package fanout runs one command on many clusters at once, a bounded number
// at a time. It hands what each run writes to a sink a line at a time, named
// by cluster, and reports how each run ended. It never prints: the caller's
// sink decides how a line looks.
package fanout

import (
	"bytes"
	"context"
	"io"
	"sync"
)

// DefaultParallel is how many runs go at once unless the caller says.
const DefaultParallel = 8

// maxLine bounds how much of a line without a newline is held back; a longer
// one is passed on in pieces.
const maxLine = 64 << 10

// Stream says which output a line was written to.
type Stream int

// Streams a run writes to.
const (
	Stdout Stream = iota
	Stderr
)

// Sink receives what the runs write. Calls are made one at a time, so Line
// may write to a shared terminal without a lock of its own.
type Sink struct {
	Line func(cluster string, stream Stream, text string)
}

// Func runs the command on one cluster, writing to stdout and stderr. It must
// return soon after ctx is cancelled.
type Func func(ctx context.Context, cluster string, stdout, stderr io.Writer) error

// State is how a run ended.
type State int

// States of a run.
const (
	// Finished runs ended on their own, with or without an error.
	Finished State = iota
	// Interrupted runs ended after ctx was cancelled, so an error they
	// returned may be no more than the interruption.
	Interrupted
	// Skipped runs never started, because ctx was cancelled first.
	Skipped
)

// Result is how one cluster's run ended.
type Result struct {
	Cluster string
	State   State
	// Err is what the run returned; nil for a run that never started.
	Err error
}

// Failed reports whether the run ended on its own with an error.
func (r Result) Failed() bool { return r.State == Finished && r.Err != nil }

// Run runs fn for each cluster, at most parallel at once (DefaultParallel
// when parallel is below 1), and returns once every run has ended. Results
// are in the order of clusters. Cancelling ctx cancels the runs under way,
// and those not yet started are skipped.
func Run(ctx context.Context, clusters []string, parallel int, sink Sink, fn Func) []Result {
	if parallel < 1 {
		parallel = DefaultParallel
	}
	results := make([]Result, len(clusters))
	var out sync.Mutex // serialises calls to sink.Line
	emit := func(cluster string, stream Stream, text string) {
		out.Lock()
		defer out.Unlock()
		if sink.Line != nil {
			sink.Line(cluster, stream, text)
		}
	}

	slots := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	for i, name := range clusters {
		if !acquire(ctx, slots) {
			results[i] = Result{Cluster: name, State: Skipped}
			continue
		}
		wg.Go(func() {
			defer func() { <-slots }()
			stdout := &lineWriter{emit: func(s string) { emit(name, Stdout, s) }}
			stderr := &lineWriter{emit: func(s string) { emit(name, Stderr, s) }}
			err := fn(ctx, name, stdout, stderr)
			stdout.flush()
			stderr.flush()
			state := Finished
			if ctx.Err() != nil {
				state = Interrupted
			}
			results[i] = Result{Cluster: name, State: state, Err: err}
		})
	}
	wg.Wait()
	return results
}

// acquire takes a slot, or reports false once ctx is cancelled.
func acquire(ctx context.Context, slots chan struct{}) bool {
	if ctx.Err() != nil {
		return false
	}
	select {
	case slots <- struct{}{}:
		if ctx.Err() != nil {
			<-slots
			return false
		}
		return true
	case <-ctx.Done():
		return false
	}
}

// lineWriter passes on what is written to it a whole line at a time.
type lineWriter struct {
	emit func(string)

	mu  sync.Mutex
	buf []byte
}

var _ io.Writer = (*lineWriter)(nil)

func (w *lineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		w.emit(string(bytes.TrimSuffix(w.buf[:i], []byte("\r"))))
		w.buf = w.buf[i+1:]
	}
	if len(w.buf) >= maxLine {
		w.emit(string(w.buf))
		w.buf = nil
	}
	return len(p), nil
}

// flush passes on a last line that did not end with a newline.
func (w *lineWriter) flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.buf) > 0 {
		w.emit(string(w.buf))
		w.buf = nil
	}
}
