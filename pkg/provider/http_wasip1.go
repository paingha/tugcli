//go:build wasip1

package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"time"
	"unsafe"
)

// The host functions tug provides in HostModule. Pointers are offsets in the
// module's memory and every value is an i32; docs/plugins.md has the ABI.

//go:wasmimport tug_http request
func hostRequest(head unsafe.Pointer, headLen uint32, body unsafe.Pointer, bodyLen uint32, handle unsafe.Pointer) uint32

//go:wasmimport tug_http response_head
func hostResponseHead(handle uint32, buf unsafe.Pointer, bufLen uint32, n unsafe.Pointer) uint32

//go:wasmimport tug_http read_body
func hostReadBody(handle uint32, buf unsafe.Pointer, bufLen uint32, n unsafe.Pointer) uint32

//go:wasmimport tug_http close
func hostClose(handle uint32) uint32

//go:wasmimport tug_http last_error
func hostLastError(buf unsafe.Pointer, bufLen uint32, n unsafe.Pointer) uint32

// A module compiled for wasip1 has no sockets, so Go's own transport cannot
// dial. Installing the host transport as the default lets a plugin use
// http.DefaultClient, or a client of its own, unchanged.
func init() { http.DefaultTransport = hostTransport{} }

func platformTransport() http.RoundTripper { return hostTransport{} }

// hostTransport sends each request through tug, which allows only https to
// the hosts in the manifest's spec.network. tug follows no redirects; the
// http.Client does, sending each hop through here, so each hop is checked.
//
// A host call blocks every goroutine in the module until it returns, so the
// request's deadline goes to tug, which enforces it.
type hostTransport struct{}

// RoundTrip implements http.RoundTripper.
func (hostTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	body, err := requestBody(req)
	if err != nil {
		return nil, err
	}
	ctx := req.Context()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	head := HostRequest{Method: req.Method, URL: req.URL.String(), Header: req.Header}
	if head.Method == "" {
		head.Method = http.MethodGet
	}
	if deadline, ok := ctx.Deadline(); ok {
		left := time.Until(deadline)
		if left <= 0 {
			return nil, context.DeadlineExceeded
		}
		head.TimeoutMillis = max(left.Milliseconds(), 1)
	}
	hb, err := json.Marshal(head)
	if err != nil {
		return nil, err
	}
	var handle uint32
	if e := HostErrno(hostRequest(bytesPtr(hb), uint32(len(hb)), bytesPtr(body), uint32(len(body)), unsafe.Pointer(&handle))); e != HostOK {
		return nil, lastHostError(e)
	}
	resp, err := hostResponse(handle, req)
	if err != nil {
		hostClose(handle)
		return nil, err
	}
	return resp, nil
}

// requestBody reads and closes the request body, which tug needs whole.
func requestBody(req *http.Request) ([]byte, error) {
	if req.Body == nil || req.Body == http.NoBody {
		return nil, nil
	}
	defer req.Body.Close()
	b, err := io.ReadAll(io.LimitReader(req.Body, MaxHostRequestBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > MaxHostRequestBodyBytes {
		return nil, &HostError{Errno: HostTooLarge, Message: fmt.Sprintf("the request body is larger than %d bytes", MaxHostRequestBodyBytes)}
	}
	return b, nil
}

func hostResponse(handle uint32, req *http.Request) (*http.Response, error) {
	buf := make([]byte, 4<<10)
	var n uint32
	e := HostErrno(hostResponseHead(handle, bytesPtr(buf), uint32(len(buf)), unsafe.Pointer(&n)))
	if e == HostShortBuffer {
		buf = make([]byte, n)
		e = HostErrno(hostResponseHead(handle, bytesPtr(buf), uint32(len(buf)), unsafe.Pointer(&n)))
	}
	if e != HostOK {
		return nil, lastHostError(e)
	}
	var head HostResponse
	if err := json.Unmarshal(buf[:n], &head); err != nil {
		return nil, fmt.Errorf("reading the response head from %s: %w", HostModule, err)
	}
	header := http.Header(head.Header)
	if header == nil {
		header = http.Header{}
	}
	b := &hostBody{handle: handle}
	// A body the plugin never closes still frees tug's side once it is
	// garbage. tug never reuses a handle, so a late cleanup closes nothing
	// else.
	b.cleanup = runtime.AddCleanup(b, func(h uint32) { hostClose(h) }, handle)
	return &http.Response{
		Status:        fmt.Sprintf("%d %s", head.Status, http.StatusText(head.Status)),
		StatusCode:    head.Status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        header,
		Body:          b,
		ContentLength: head.ContentLength,
		Request:       req,
	}, nil
}

// hostBody reads a response body from tug in pieces.
type hostBody struct {
	handle  uint32
	closed  bool
	cleanup runtime.Cleanup
}

var errBodyClosed = errors.New("http: read on closed response body")

func (b *hostBody) Read(p []byte) (int, error) {
	if b.closed {
		return 0, errBodyClosed
	}
	if len(p) == 0 {
		return 0, nil
	}
	var n uint32
	if e := HostErrno(hostReadBody(b.handle, bytesPtr(p), uint32(len(p)), unsafe.Pointer(&n))); e != HostOK {
		return 0, lastHostError(e)
	}
	if n == 0 {
		return 0, io.EOF
	}
	return int(n), nil
}

func (b *hostBody) Close() error {
	if b.closed {
		return nil
	}
	b.closed = true
	b.cleanup.Stop()
	if e := HostErrno(hostClose(b.handle)); e != HostOK {
		return lastHostError(e)
	}
	return nil
}

// lastHostError returns e with the message tug kept for it.
func lastHostError(e HostErrno) error {
	buf := make([]byte, 512)
	var n uint32
	r := HostErrno(hostLastError(bytesPtr(buf), uint32(len(buf)), unsafe.Pointer(&n)))
	if r == HostShortBuffer {
		buf = make([]byte, n)
		r = HostErrno(hostLastError(bytesPtr(buf), uint32(len(buf)), unsafe.Pointer(&n)))
	}
	he := &HostError{Errno: e}
	if r == HostOK {
		he.Message = string(buf[:n])
	}
	return he
}

// bytesPtr is the address of b's first byte, or 0 for an empty slice.
func bytesPtr(b []byte) unsafe.Pointer {
	if len(b) == 0 {
		return nil
	}
	return unsafe.Pointer(unsafe.SliceData(b))
}
