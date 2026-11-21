package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// ServeRequest is one line a host writes to a plugin in serve mode. A
// request with Cancel set asks the plugin to stop working on the request
// with that ID.
type ServeRequest struct {
	ID      string          `json:"id"`
	Op      string          `json:"op,omitempty"`
	Request json.RawMessage `json:"request,omitempty"`
	Cancel  bool            `json:"cancel,omitempty"`
}

// ServeResponse is one line a plugin writes in serve mode: the response,
// or Error, it would have written for the same request in one-shot mode.
// A line the plugin could not read is answered with an empty ID.
type ServeResponse struct {
	ID       string          `json:"id"`
	Response json.RawMessage `json:"response"`
}

// maxServeLine bounds one line in serve mode: a message and its envelope.
const maxServeLine = MaxMessageBytes + 64<<10

// serve answers requests from stdin, one JSON line each, until stdin closes
// or ctx ends. Native plugins answer concurrently, so responses may come out
// of order; each carries its request's ID. WebAssembly plugins answer in
// order (see serveInOrder).
func serve(ctx context.Context, p Provider, stdin io.Reader, stdout, stderr io.Writer) int {
	s := &serveSession{p: p, stdout: stdout, stderr: stderr, cancels: map[string]context.CancelFunc{}}
	if serveInOrder {
		return s.inOrder(ctx, stdin)
	}
	return s.concurrent(ctx, stdin)
}

// serveSession is what one run of serve shares between its requests.
type serveSession struct {
	p              Provider
	stdout, stderr io.Writer

	outMu   sync.Mutex
	cMu     sync.Mutex
	cancels map[string]context.CancelFunc
}

// concurrent reads lines in a goroutine of its own and answers each request
// in another, so a cancel line can reach a request that is still running.
func (s *serveSession) concurrent(ctx context.Context, stdin io.Reader) int {
	var wg sync.WaitGroup
	lines := make(chan []byte)
	readErr := make(chan error, 1)
	go func() {
		sc := newServeScanner(stdin)
		for sc.Scan() {
			select {
			case lines <- bytes.Clone(sc.Bytes()):
			case <-ctx.Done():
				return
			}
		}
		readErr <- sc.Err()
	}()

	defer wg.Wait()
	for {
		var line []byte
		select {
		case <-ctx.Done():
			s.cMu.Lock()
			for _, cancel := range s.cancels {
				cancel()
			}
			s.cMu.Unlock()
			return ExitOK
		case err := <-readErr:
			return s.end(err) // stdin closed: finish what is in flight, then exit
		case line = <-lines:
		}
		req, body, ok := s.parse(line)
		if !ok {
			continue
		}
		rctx, cancel := context.WithCancel(ctx)
		s.cMu.Lock()
		s.cancels[req.ID] = cancel
		s.cMu.Unlock()
		wg.Go(func() {
			defer func() {
				s.cMu.Lock()
				delete(s.cancels, req.ID)
				s.cMu.Unlock()
				cancel()
			}()
			s.answer(rctx, req, body)
		})
	}
}

// inOrder answers each request before it reads the next line. Nothing runs
// while a line is read, so a cancel line has nothing to cancel.
func (s *serveSession) inOrder(ctx context.Context, stdin io.Reader) int {
	sc := newServeScanner(stdin)
	for sc.Scan() {
		if ctx.Err() != nil {
			return ExitOK
		}
		if req, body, ok := s.parse(sc.Bytes()); ok {
			s.answer(ctx, req, body)
		}
	}
	return s.end(sc.Err())
}

func newServeScanner(r io.Reader) *bufio.Scanner {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), maxServeLine)
	return sc
}

// parse reads one line. It answers lines that are not requests, and cancels
// the request a cancel line names, and then returns false.
func (s *serveSession) parse(line []byte) (ServeRequest, []byte, bool) {
	var req ServeRequest
	if len(bytes.TrimSpace(line)) == 0 {
		return req, nil, false
	}
	if err := json.Unmarshal(line, &req); err != nil || req.ID == "" {
		s.writeErr("", OpServe, Errorf(InvalidConfig, "each line must be a JSON object with an id"))
		return req, nil, false
	}
	if req.Cancel {
		s.cMu.Lock()
		if cancel, ok := s.cancels[req.ID]; ok {
			cancel()
		}
		s.cMu.Unlock()
		return req, nil, false
	}
	if req.Op == OpServe || !IsKnownOp(req.Op) {
		s.writeErr(req.ID, req.Op, Errorf(InvalidConfig, "unknown operation %q", req.Op))
		return req, nil, false
	}
	body := []byte(req.Request)
	if bytes.Equal(bytes.TrimSpace(body), []byte("null")) {
		body = nil
	}
	return req, body, true
}

// answer runs one request and writes its response.
func (s *serveSession) answer(ctx context.Context, req ServeRequest, body []byte) {
	resp, err := dispatch(ctx, s.p, req.Op, bytes.NewReader(body))
	if err != nil {
		s.writeErr(req.ID, req.Op, AsError(err))
		return
	}
	s.write(req.ID, resp)
}

// end returns the exit code once stdin has closed or failed.
func (s *serveSession) end(err error) int {
	if err != nil && !errors.Is(err, io.EOF) {
		s.writeErr("", OpServe, Errorf(InvalidConfig, "reading requests: %v", err))
		return ExitError
	}
	return ExitOK
}

func (s *serveSession) write(id string, msg any) {
	b, err := json.Marshal(msg)
	if err != nil {
		b, _ = json.Marshal(errorResponseMsg{typeMeta: meta(kindError), Error: Error{Code: Internal, Message: err.Error()}})
	}
	line, _ := json.Marshal(ServeResponse{ID: id, Response: b})
	s.outMu.Lock()
	defer s.outMu.Unlock()
	_, _ = s.stdout.Write(append(line, '\n'))
}

func (s *serveSession) writeErr(id, op string, e *Error) {
	if e.Code == "" {
		e.Code = Internal
	}
	s.write(id, errorResponseMsg{typeMeta: meta(kindError), Error: *e})
	fmt.Fprintf(s.stderr, "%s %s: %v\n", progName(), op, e)
}
