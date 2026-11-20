package iap

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/oauth2"
)

// testToken is the access token the fake relay accepts.
const testToken = "test-access-token"

// fakeRelay is an IAP relay on this machine. It checks each request as the
// real relay would, speaks the frame protocol, and echoes every session's
// stream back to it. It decodes frames itself rather than with the code
// under test.
type fakeRelay struct {
	t   *testing.T
	srv *httptest.Server
	wg  sync.WaitGroup // handlers, which outlive the server's own tracking once hijacked

	mu       sync.Mutex
	requests []*http.Request
	sessions map[string]*fakeSession
	nextSID  int
	socks    []*websocket.Conn

	// Behaviour, set before dialling.
	refuse     int      // answer the upgrade with this HTTP status
	closeFirst int      // close new sessions with this code instead of opening them
	firstFrame []byte   // send this instead of CONNECT_SUCCESS_SID
	wrongProto bool     // agree to no subprotocol
	afterOpen  [][]byte // frames to send after CONNECT_SUCCESS_SID
	paused     chan struct{}
}

// fakeSession is one relay session: the stream from the client, which is
// also the stream back to it.
type fakeSession struct {
	sid      string
	stream   []byte   // every byte received, in order
	acks     []uint64 // ACK counts the client sent
	maxFrame int      // the largest DATA payload the client sent
	dropAt   int      // drop the WebSocket once the stream reaches this length
	dropCode int      // close code for the drop; 0 cuts the connection
	closed   []int    // close codes the client sent
}

// newFakeRelay starts a relay at a ws:// URL.
func newFakeRelay(t *testing.T) *fakeRelay {
	t.Helper()
	return startFakeRelay(t, httptest.NewServer)
}

// newFakeTLSRelay starts a relay at a wss:// URL, with a certificate that
// options trusts.
func newFakeTLSRelay(t *testing.T) *fakeRelay {
	t.Helper()
	return startFakeRelay(t, httptest.NewTLSServer)
}

func startFakeRelay(t *testing.T, start func(http.Handler) *httptest.Server) *fakeRelay {
	t.Helper()
	r := &fakeRelay{t: t, sessions: map[string]*fakeSession{}}
	r.srv = start(r)
	t.Cleanup(func() {
		r.srv.Close()
		r.mu.Lock()
		if r.paused != nil && !isDone(r.paused) {
			close(r.paused)
		}
		for _, ws := range r.socks {
			_ = ws.Close()
		}
		r.mu.Unlock()
		r.wg.Wait()
	})
	return r
}

// url is the relay URL to give WithRelayURL.
func (r *fakeRelay) url() string { return "ws" + strings.TrimPrefix(r.srv.URL, "http") + "/v4" }

// options are the Dial options that point at the relay with the test token.
func (r *fakeRelay) options(extra ...Option) []Option {
	opts := []Option{
		WithRelayURL(r.url()),
		WithTokenSource(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: testToken})),
	}
	if r.srv.TLS != nil {
		opts = append(opts, WithTLSConfig(r.srv.Client().Transport.(*http.Transport).TLSClientConfig.Clone()))
	}
	return append(opts, extra...)
}

// pause stops sessions reading from the client until resume.
func (r *fakeRelay) pause() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.paused = make(chan struct{})
}

func (r *fakeRelay) resume() {
	r.mu.Lock()
	defer r.mu.Unlock()
	close(r.paused)
}

func (r *fakeRelay) waitIfPaused() {
	r.mu.Lock()
	p := r.paused
	r.mu.Unlock()
	if p != nil {
		<-p
	}
}

// session returns the only session.
func (r *fakeRelay) session() *fakeSession {
	r.t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.sessions) != 1 {
		r.t.Fatalf("the relay has %d sessions, want 1", len(r.sessions))
	}
	for _, s := range r.sessions {
		return s
	}
	return nil
}

// set changes a session under the relay's lock.
func (r *fakeRelay) set(f func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f()
}

// get reads under the relay's lock.
func get[T any](r *fakeRelay, f func() T) T {
	r.mu.Lock()
	defer r.mu.Unlock()
	return f()
}

func (r *fakeRelay) request(i int) *http.Request {
	r.t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if i >= len(r.requests) {
		r.t.Fatalf("the relay had %d requests, want more than %d", len(r.requests), i)
	}
	return r.requests[i]
}

func (r *fakeRelay) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.wg.Add(1)
	defer r.wg.Done()
	r.mu.Lock()
	r.requests = append(r.requests, req)
	refuse, closeFirst, firstFrame, wrongProto, afterOpen := r.refuse, r.closeFirst, r.firstFrame, r.wrongProto, r.afterOpen
	r.mu.Unlock()

	if got := req.Header.Get("Authorization"); got != "Bearer "+testToken {
		http.Error(w, "bad token", http.StatusUnauthorized)
		return
	}
	if got := req.Header.Get("Origin"); got != origin {
		r.t.Errorf("Origin = %q, want %q", got, origin)
	}
	if got := req.Header.Get("User-Agent"); !strings.HasPrefix(got, "tug/") {
		r.t.Errorf("User-Agent = %q, want tug/...", got)
	}
	if got := websocket.Subprotocols(req); len(got) != 1 || got[0] != subprotocol {
		r.t.Errorf("subprotocols = %q, want [%s]", got, subprotocol)
	}
	if refuse != 0 {
		http.Error(w, "refused by the test", refuse)
		return
	}
	if req.URL.Path != "/v4/connect" {
		http.NotFound(w, req)
		return
	}
	if err := checkConnectQuery(req.URL.Query()); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	up := websocket.Upgrader{
		Subprotocols: []string{subprotocol},
		CheckOrigin:  func(req *http.Request) bool { return req.Header.Get("Origin") == origin },
	}
	if wrongProto {
		up.Subprotocols = nil
	}
	ws, err := up.Upgrade(w, req, nil)
	if err != nil {
		return
	}
	r.mu.Lock()
	r.socks = append(r.socks, ws)
	r.mu.Unlock()
	defer ws.Close()

	r.waitIfPaused()
	switch {
	case closeFirst != 0:
		_ = ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(closeFirst, "closed by the test"), time.Now().Add(time.Second))
		return
	case firstFrame != nil:
		_ = ws.WriteMessage(websocket.BinaryMessage, firstFrame)
		_, _, _ = ws.ReadMessage()
		return
	}
	r.mu.Lock()
	r.nextSID++
	s := &fakeSession{sid: fmt.Sprintf("sid-%d", r.nextSID)}
	r.sessions[s.sid] = s
	r.mu.Unlock()
	if err := ws.WriteMessage(websocket.BinaryMessage, relayFrame(tagConnectSuccessSID, []byte(s.sid))); err != nil {
		return
	}
	for _, f := range afterOpen {
		if err := ws.WriteMessage(websocket.BinaryMessage, f); err != nil {
			return
		}
	}
	r.serve(s, ws)
}

// checkConnectQuery checks a connect URL's query for either mode.
func checkConnectQuery(q url.Values) error {
	want := []string{"project", "port", "newWebsocket", "zone", "instance", "interface"}
	if q.Has("group") {
		want = []string{"project", "port", "newWebsocket", "region", "network", "host", "group"}
	}
	for _, k := range want {
		if q.Get(k) == "" {
			return fmt.Errorf("connect query %s has no %s", q.Encode(), k)
		}
	}
	if len(q) != len(want) {
		return fmt.Errorf("connect query %s has settings beyond %v", q.Encode(), want)
	}
	if q.Get("newWebsocket") != "True" {
		return fmt.Errorf("connect query %s: newWebsocket is not True", q.Encode())
	}
	return nil
}

// serve echoes the session's stream on ws and acknowledges every DATA
// frame.
func (r *fakeRelay) serve(s *fakeSession, ws *websocket.Conn) {
	for {
		r.waitIfPaused()
		mt, msg, err := ws.ReadMessage()
		if err != nil {
			var ce *websocket.CloseError
			if errors.As(err, &ce) {
				r.set(func() { s.closed = append(s.closed, ce.Code) })
			}
			return
		}
		if mt != websocket.BinaryMessage || len(msg) < 2 {
			r.t.Errorf("the relay got a message of type %d and %d bytes", mt, len(msg))
			return
		}
		switch tag := binary.BigEndian.Uint16(msg); tag {
		case tagData:
			if len(msg) < 6 || int(binary.BigEndian.Uint32(msg[2:6])) != len(msg)-6 {
				r.t.Errorf("the relay got a malformed DATA frame of %d bytes", len(msg))
				return
			}
			data := msg[6:]
			n := len(data)
			r.mu.Lock()
			s.stream = append(s.stream, data...)
			s.maxFrame = max(s.maxFrame, n)
			received := uint64(len(s.stream))
			drop := s.dropAt > 0 && len(s.stream) >= s.dropAt
			code := s.dropCode
			if drop {
				s.dropAt = 0
			}
			r.mu.Unlock()
			if !writeStream(ws, data) {
				return
			}
			if err := ws.WriteMessage(websocket.BinaryMessage, relayCount(tagAck, received)); err != nil {
				return
			}
			if drop {
				if code != 0 {
					_ = ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, "drop"), time.Now().Add(time.Second))
				}
				return
			}
		case tagAck:
			if len(msg) != 10 {
				r.t.Errorf("ACK frame of %d bytes, want 10", len(msg))
			}
			r.set(func() { s.acks = append(s.acks, binary.BigEndian.Uint64(msg[2:])) })
		default:
			r.t.Errorf("the relay got a frame with tag %#04x", tag)
		}
	}
}

// writeStream sends p in DATA frames of at most 16 KiB.
func writeStream(ws *websocket.Conn, p []byte) bool {
	for len(p) > 0 {
		n := min(len(p), maxDataFrame)
		if err := ws.WriteMessage(websocket.BinaryMessage, relayFrame(tagData, p[:n])); err != nil {
			return false
		}
		p = p[n:]
	}
	return true
}

func relayFrame(tag uint16, p []byte) []byte {
	b := binary.BigEndian.AppendUint16(nil, tag)
	b = binary.BigEndian.AppendUint32(b, uint32(len(p)))
	return append(b, p...)
}

func relayCount(tag uint16, n uint64) []byte {
	b := binary.BigEndian.AppendUint16(nil, tag)
	return binary.BigEndian.AppendUint64(b, n)
}
