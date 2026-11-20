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

// serve answers requests from stdin, one JSON line each and concurrently,
// until stdin closes or ctx ends. Responses may come out of order; each
// carries its request's ID.
func serve(ctx context.Context, p Provider, stdin io.Reader, stdout, stderr io.Writer) int {
	var (
		outMu   sync.Mutex
		wg      sync.WaitGroup
		cMu     sync.Mutex
		cancels = map[string]context.CancelFunc{}
	)
	write := func(id string, msg any) {
		b, err := json.Marshal(msg)
		if err != nil {
			b, _ = json.Marshal(errorResponseMsg{typeMeta: meta(kindError), Error: Error{Code: Internal, Message: err.Error()}})
		}
		line, _ := json.Marshal(ServeResponse{ID: id, Response: b})
		outMu.Lock()
		defer outMu.Unlock()
		_, _ = stdout.Write(append(line, '\n'))
	}
	writeErr := func(id, op string, e *Error) {
		if e.Code == "" {
			e.Code = Internal
		}
		write(id, errorResponseMsg{typeMeta: meta(kindError), Error: *e})
		fmt.Fprintf(stderr, "%s %s: %v\n", progName(), op, e)
	}

	lines := make(chan []byte)
	readErr := make(chan error, 1)
	go func() {
		sc := bufio.NewScanner(stdin)
		sc.Buffer(make([]byte, 64<<10), maxServeLine)
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
			cMu.Lock()
			for _, cancel := range cancels {
				cancel()
			}
			cMu.Unlock()
			return ExitOK
		case err := <-readErr:
			if err != nil && !errors.Is(err, io.EOF) {
				writeErr("", OpServe, Errorf(InvalidConfig, "reading requests: %v", err))
				return ExitError
			}
			return ExitOK // stdin closed: finish what is in flight, then exit
		case line = <-lines:
		}
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var req ServeRequest
		if err := json.Unmarshal(line, &req); err != nil || req.ID == "" {
			writeErr("", OpServe, Errorf(InvalidConfig, "each line must be a JSON object with an id"))
			continue
		}
		if req.Cancel {
			cMu.Lock()
			if cancel, ok := cancels[req.ID]; ok {
				cancel()
			}
			cMu.Unlock()
			continue
		}
		if req.Op == OpServe || !IsKnownOp(req.Op) {
			writeErr(req.ID, req.Op, Errorf(InvalidConfig, "unknown operation %q", req.Op))
			continue
		}
		rctx, cancel := context.WithCancel(ctx)
		cMu.Lock()
		cancels[req.ID] = cancel
		cMu.Unlock()
		body := []byte(req.Request)
		if bytes.Equal(bytes.TrimSpace(body), []byte("null")) {
			body = nil
		}
		wg.Go(func() {
			defer func() {
				cMu.Lock()
				delete(cancels, req.ID)
				cMu.Unlock()
				cancel()
			}()
			resp, err := dispatch(rctx, p, req.Op, bytes.NewReader(body))
			if err != nil {
				writeErr(req.ID, req.Op, AsError(err))
				return
			}
			write(req.ID, resp)
		})
	}
}
