package plugin

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"time"

	"github.com/tetratelabs/wazero/api"

	"github.com/paingha/tugcli/pkg/provider"
)

// hostFunction is one function of tug_http. Every parameter and the result
// are i32; the result is a provider.HostErrno.
type hostFunction struct {
	name   string
	params []string
	call   func(s *httpSession, ctx context.Context, mem api.Memory, args []uint64) provider.HostErrno
}

// hostFunctions is the tug_http ABI; docs/plugins.md describes it, and
// pkg/conformance checks that modules import it as written there.
var hostFunctions = []hostFunction{
	{"request", []string{"head_ptr", "head_len", "body_ptr", "body_len", "handle_ptr"}, (*httpSession).request},
	{"response_head", []string{"handle", "buf_ptr", "buf_len", "len_ptr"}, (*httpSession).responseHead},
	{"read_body", []string{"handle", "buf_ptr", "buf_len", "n_ptr"}, (*httpSession).readBody},
	{"close", []string{"handle"}, (*httpSession).close},
	{"last_error", []string{"buf_ptr", "buf_len", "len_ptr"}, (*httpSession).lastError},
}

func isHostFunction(name string) bool {
	return slices.ContainsFunc(hostFunctions, func(f hostFunction) bool { return f.name == name })
}

// instantiateHTTP adds tug_http to the runtime. Each call finds its
// instance's httpSession in its context.
func (h *wasmHost) instantiateHTTP(ctx context.Context) error {
	b := h.rt.NewHostModuleBuilder(provider.HostModule)
	for _, f := range hostFunctions {
		params := make([]api.ValueType, len(f.params))
		for i := range params {
			params[i] = api.ValueTypeI32
		}
		call := f.call
		b.NewFunctionBuilder().
			WithGoModuleFunction(api.GoModuleFunc(func(ctx context.Context, m api.Module, stack []uint64) {
				errno := provider.HostInvalid
				if s, ok := ctx.Value(httpSessionKey{}).(*httpSession); ok {
					errno = call(s, ctx, m.Memory(), stack)
				}
				stack[0] = api.EncodeU32(uint32(errno))
			}), params, []api.ValueType{api.ValueTypeI32}).
			WithParameterNames(f.params...).
			WithResultNames("errno").
			Export(f.name)
	}
	_, err := b.Instantiate(ctx)
	return err
}

// newHostClient returns the client that makes a module's requests. It
// follows no redirects: the module's own client does, sending each hop back
// through tug, where it is checked like any request.
func newHostClient(rt http.RoundTripper) *http.Client {
	if rt == nil {
		rt = &http.Transport{
			Proxy:                  http.ProxyFromEnvironment,
			DialContext:            (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			ForceAttemptHTTP2:      true,
			MaxIdleConns:           16,
			IdleConnTimeout:        90 * time.Second,
			TLSHandshakeTimeout:    10 * time.Second,
			ExpectContinueTimeout:  time.Second,
			MaxResponseHeaderBytes: provider.MaxHostResponseHeaderBytes,
		}
	}
	return &http.Client{
		Transport:     rt,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// httpSessionKey holds an instance's *httpSession in its context.
type httpSessionKey struct{}

// httpSession is one module instance's side of tug_http: its open responses
// and the message of its last failure. Only the instance's goroutine uses
// it, one host call at a time.
type httpSession struct {
	host    *wasmHost
	next    uint32 // handles are never reused
	open    map[uint32]*openResponse
	lastErr string
}

type openResponse struct {
	resp   *http.Response
	body   io.Reader
	read   int64
	err    error // the error that ended the body, returned once its bytes are read
	head   []byte
	cancel context.CancelFunc
}

func (s *httpSession) fail(e provider.HostErrno, format string, args ...any) provider.HostErrno {
	s.lastErr = fmt.Sprintf(format, args...)
	return e
}

// request: (head_ptr, head_len, body_ptr, body_len, handle_ptr) -> errno.
func (s *httpSession) request(ctx context.Context, mem api.Memory, args []uint64) provider.HostErrno {
	headPtr, headLen := api.DecodeU32(args[0]), api.DecodeU32(args[1])
	bodyPtr, bodyLen := api.DecodeU32(args[2]), api.DecodeU32(args[3])
	handlePtr := api.DecodeU32(args[4])
	if headLen > provider.MaxHostRequestHeadBytes {
		return s.fail(provider.HostTooLarge, "the request head is %d bytes; the limit is %d", headLen, provider.MaxHostRequestHeadBytes)
	}
	if bodyLen > provider.MaxHostRequestBodyBytes {
		return s.fail(provider.HostTooLarge, "the request body is %d bytes; the limit is %d", bodyLen, provider.MaxHostRequestBodyBytes)
	}
	head, ok := mem.Read(headPtr, headLen)
	if !ok {
		return s.fail(provider.HostInvalid, "the request head is outside the module's memory")
	}
	body, ok := mem.Read(bodyPtr, bodyLen)
	if !ok {
		return s.fail(provider.HostInvalid, "the request body is outside the module's memory")
	}
	var hr provider.HostRequest
	if err := json.Unmarshal(head, &hr); err != nil {
		return s.fail(provider.HostInvalid, "the request head is not valid JSON: %v", err)
	}
	if len(s.open) >= provider.MaxHostOpenResponses {
		return s.fail(provider.HostTooMany, "%d responses are open; close one first", len(s.open))
	}
	method := cmp.Or(hr.Method, http.MethodGet)
	u, err := url.Parse(hr.URL)
	if err != nil {
		return s.fail(provider.HostInvalid, "%s: %v", method, err)
	}
	where := logURL(u)
	if err := s.host.allow.check(u); err != nil {
		s.host.log.Warn("refused a plugin's request", "plugin", s.host.name, "method", method, "url", where, "reason", err)
		return s.fail(provider.HostDenied, "%s %s refused: %v", method, where, err)
	}

	reqCtx, cancel := context.WithCancel(ctx)
	if hr.TimeoutMillis > 0 {
		reqCtx, cancel = context.WithTimeout(ctx, time.Duration(hr.TimeoutMillis)*time.Millisecond)
	}
	// The body is copied: the transport may still be writing it after
	// this call returns and the module's memory changes.
	req, err := http.NewRequestWithContext(reqCtx, method, u.String(), bytes.NewReader(bytes.Clone(body)))
	if err != nil {
		cancel()
		return s.fail(provider.HostInvalid, "%s %s: %v", method, where, err)
	}
	for k, vs := range hr.Header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	start := time.Now()
	resp, err := s.host.client.Do(req) //nolint:bodyclose // the module closes it, or closeAll does when the instance ends
	if err != nil {
		cancel()
		s.host.log.Debug("plugin request failed", "plugin", s.host.name, "method", method, "url", where, "duration", time.Since(start), "err", err)
		if reqCtx.Err() != nil {
			return s.fail(provider.HostTimeout, "%s %s: %v", method, where, context.Cause(reqCtx))
		}
		return s.fail(provider.HostNetwork, "%s %s: %v", method, where, unwrapURLError(err))
	}
	s.host.log.Debug("plugin request", "plugin", s.host.name, "method", method, "url", where, "status", resp.StatusCode, "duration", time.Since(start))
	s.next++
	handle := s.next
	s.open[handle] = &openResponse{
		resp:   resp,
		body:   io.LimitReader(resp.Body, provider.MaxHostResponseBodyBytes+1),
		cancel: cancel,
	}
	if !mem.WriteUint32Le(handlePtr, handle) {
		s.closeHandle(handle)
		return s.fail(provider.HostInvalid, "the handle pointer is outside the module's memory")
	}
	return provider.HostOK
}

// responseHead: (handle, buf_ptr, buf_len, len_ptr) -> errno.
func (s *httpSession) responseHead(_ context.Context, mem api.Memory, args []uint64) provider.HostErrno {
	handle := api.DecodeU32(args[0])
	r, ok := s.open[handle]
	if !ok {
		return s.fail(provider.HostBadHandle, "no open response has handle %d", handle)
	}
	if r.head == nil {
		head, err := json.Marshal(provider.HostResponse{
			Status:        r.resp.StatusCode,
			Header:        r.resp.Header,
			ContentLength: r.resp.ContentLength,
			URL:           r.resp.Request.URL.String(),
		})
		if err != nil {
			return s.fail(provider.HostInvalid, "encoding the response head: %v", err)
		}
		r.head = head
	}
	return s.copyOut(mem, r.head, api.DecodeU32(args[1]), api.DecodeU32(args[2]), api.DecodeU32(args[3]))
}

// readBody: (handle, buf_ptr, buf_len, n_ptr) -> errno. n is 0 at the end
// of the body.
func (s *httpSession) readBody(_ context.Context, mem api.Memory, args []uint64) provider.HostErrno {
	handle, bufPtr, bufLen, nPtr := api.DecodeU32(args[0]), api.DecodeU32(args[1]), api.DecodeU32(args[2]), api.DecodeU32(args[3])
	r, ok := s.open[handle]
	if !ok {
		return s.fail(provider.HostBadHandle, "no open response has handle %d", handle)
	}
	if bufLen == 0 {
		return s.fail(provider.HostInvalid, "the buffer has no room")
	}
	dst, ok := mem.Read(bufPtr, bufLen)
	if !ok {
		return s.fail(provider.HostInvalid, "the buffer is outside the module's memory")
	}
	n, errno := s.fill(r, dst)
	if errno != provider.HostOK {
		return errno
	}
	if !mem.WriteUint32Le(nPtr, uint32(n)) {
		return s.fail(provider.HostInvalid, "the count pointer is outside the module's memory")
	}
	return provider.HostOK
}

// fill reads the next bytes of a body into dst, the module's own memory.
func (s *httpSession) fill(r *openResponse, dst []byte) (int, provider.HostErrno) {
	for {
		if r.err != nil {
			if errors.Is(r.err, io.EOF) {
				return 0, provider.HostOK
			}
			if errors.Is(r.err, context.DeadlineExceeded) || errors.Is(r.err, context.Canceled) {
				return 0, s.fail(provider.HostTimeout, "reading the response body: %v", r.err)
			}
			return 0, s.fail(provider.HostNetwork, "reading the response body: %v", r.err)
		}
		n, err := r.body.Read(dst)
		r.read += int64(n)
		if r.read > provider.MaxHostResponseBodyBytes {
			r.err = fmt.Errorf("the response body is larger than %d bytes", provider.MaxHostResponseBodyBytes)
			return 0, s.fail(provider.HostTooLarge, "%v", r.err)
		}
		if err != nil {
			r.err = err
		}
		if n > 0 {
			return n, provider.HostOK
		}
	}
}

// close: (handle) -> errno.
func (s *httpSession) close(_ context.Context, _ api.Memory, args []uint64) provider.HostErrno {
	handle := api.DecodeU32(args[0])
	if _, ok := s.open[handle]; !ok {
		return s.fail(provider.HostBadHandle, "no open response has handle %d", handle)
	}
	s.closeHandle(handle)
	return provider.HostOK
}

// lastError: (buf_ptr, buf_len, len_ptr) -> errno. The message is kept, so
// a module can call again with a bigger buffer.
func (s *httpSession) lastError(_ context.Context, mem api.Memory, args []uint64) provider.HostErrno {
	msg := s.lastErr
	errno := s.copyOut(mem, []byte(msg), api.DecodeU32(args[0]), api.DecodeU32(args[1]), api.DecodeU32(args[2]))
	s.lastErr = msg
	return errno
}

// copyOut writes len(data) at lenPtr, then data at buf when bufLen holds it.
func (s *httpSession) copyOut(mem api.Memory, data []byte, buf, bufLen, lenPtr uint32) provider.HostErrno {
	if !mem.WriteUint32Le(lenPtr, uint32(len(data))) {
		return s.fail(provider.HostInvalid, "the length pointer is outside the module's memory")
	}
	if uint32(len(data)) > bufLen {
		return s.fail(provider.HostShortBuffer, "the buffer holds %d bytes and %d are needed", bufLen, len(data))
	}
	if !mem.Write(buf, data) {
		return s.fail(provider.HostInvalid, "the buffer is outside the module's memory")
	}
	return provider.HostOK
}

func (s *httpSession) closeHandle(handle uint32) {
	r := s.open[handle]
	delete(s.open, handle)
	_ = r.resp.Body.Close() // the module is done with it
	r.cancel()
}

// closeAll frees what the module left open when its instance ends.
func (s *httpSession) closeAll() {
	for handle := range s.open {
		s.closeHandle(handle)
	}
}

// logURL is u without its query or credentials, which may hold secrets, for
// logs and messages.
func logURL(u *url.URL) string {
	c := *u
	c.User, c.RawQuery, c.ForceQuery, c.Fragment = nil, "", false, ""
	return c.String()
}

// unwrapURLError drops the method and URL that *url.Error repeats, since the
// message already has them without the query.
func unwrapURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}
