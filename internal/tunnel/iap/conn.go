package iap

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Buffer limits and timings.
const (
	// ackWindow is how many received bytes may go unacknowledged before
	// the tunnel sends an ACK: twice the largest DATA frame, as in gcloud.
	ackWindow = 2 * maxDataFrame
	// maxReadBuffer is how many received bytes wait for Read before the
	// tunnel stops reading from the relay, which then stops sending.
	maxReadBuffer = 1 << 20
	// maxUnwritten is how many bytes Write takes ahead of the WebSocket
	// before it blocks.
	maxUnwritten = 256 << 10
	// maxUnconfirmed is how many written bytes the tunnel keeps for
	// resending until the relay acknowledges them. Past it the oldest go,
	// and the session can no longer resume from before them.
	maxUnconfirmed = 4 << 20
	// flushTimeout is how long Close waits for buffered bytes to reach the
	// relay, as gcloud waits for all data to be sent.
	flushTimeout = 10 * time.Second
	// closeFrameTimeout bounds sending the close frame, and then waiting
	// for the relay's.
	closeFrameTimeout = time.Second
)

// errRelayClosed is what Write returns once the relay has ended the tunnel
// normally, which Read reports as io.EOF.
var errRelayClosed = errors.New("the IAP relay closed the tunnel")

// conn is an IAP relay session as a net.Conn. One goroutine, run, owns the
// session: for each WebSocket it starts a reader and a writer, and when the
// WebSocket drops it resumes the session on a new one. Close stops it.
//
// The bytes written are a stream. out holds the part from offset base that
// the relay has not acknowledged; wrote is the offset the writer has
// reached on the current WebSocket, and goes back to what the relay says it
// received when a session resumes.
type conn struct {
	cl     *client
	t      target
	cancel context.CancelFunc // ends a resume in progress

	closing   chan struct{} // closed by Close
	closeOnce sync.Once
	done      chan struct{} // closed when run has returned
	failed    chan struct{} // closed when err is set

	// Wake-ups, each with room for one signal and one waiter.
	work     chan struct{} // the writer: bytes or an ACK to send
	space    chan struct{} // Write: the writer took bytes
	progress chan struct{} // Close: the writer finished a frame
	readable chan struct{} // Read: bytes arrived
	drained  chan struct{} // the reader: Read made room

	wcall sync.Mutex // keeps each Write's bytes together

	rdl, wdl *deadline

	mu    sync.Mutex
	sid   string
	local net.Addr
	err   error // why the tunnel ended; Read returns it once rbuf is empty

	rbuf     bytes.Buffer
	received uint64 // stream bytes received from the relay
	acked    uint64 // the received count the relay was last told

	out       []byte
	base      uint64 // stream offset of out[0]
	wrote     uint64 // stream offset written to the current WebSocket
	writing   bool   // the writer is sending a DATA frame
	confirmed uint64 // stream bytes the relay has acknowledged
}

var _ net.Conn = (*conn)(nil)

// newConn starts the session that ws opened. ctx gives the session's
// context its values but not its cancellation, which is Close's.
func newConn(ctx context.Context, cl *client, t target, ws *websocket.Conn, sid string) *conn {
	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	c := &conn{
		cl:       cl,
		t:        t,
		cancel:   cancel,
		closing:  make(chan struct{}),
		done:     make(chan struct{}),
		failed:   make(chan struct{}),
		work:     make(chan struct{}, 1),
		space:    make(chan struct{}, 1),
		progress: make(chan struct{}, 1),
		readable: make(chan struct{}, 1),
		drained:  make(chan struct{}, 1),
		rdl:      newDeadline(),
		wdl:      newDeadline(),
		sid:      sid,
		local:    ws.LocalAddr(),
	}
	go c.run(ctx, ws)
	return c
}

// run serves the session until it ends or Close is called, resuming it
// each time its WebSocket drops.
func (c *conn) run(ctx context.Context, ws *websocket.Conn) {
	defer close(c.done)
	defer c.cancel()
	for {
		err := c.serve(ws)
		if isDone(c.closing) {
			return
		}
		if c.cl.reconnectTimeout <= 0 || !resumable(err) {
			c.fail(c.streamError(err))
			return
		}
		c.cl.logger.Debug("resuming the IAP tunnel", "target", c.t.String(), "error", err)
		ws, err = c.resume(ctx)
		if err != nil {
			if !isDone(c.closing) {
				c.fail(err)
			}
			return
		}
	}
}

// loopEnd is how a reader or writer goroutine ended.
type loopEnd struct {
	reader bool
	err    error
}

// serve runs a reader and a writer on ws until one of them stops, or until
// Close, when it first lets the writer flush. It returns why it stopped.
func (c *conn) serve(ws *websocket.Conn) error {
	stop := make(chan struct{})
	ends := make(chan loopEnd, 2)
	go func() { ends <- loopEnd{reader: true, err: c.readLoop(ws, stop)} }()
	go func() { ends <- loopEnd{err: c.writeLoop(ws, stop)} }()
	running := 2
	var err error
	select {
	case e := <-ends:
		running--
		err = e.err
	case <-c.closing:
		err = net.ErrClosed
		running -= c.shutdown(ws, ends)
	}
	close(stop)
	_ = ws.Close()
	for ; running > 0; running-- {
		<-ends
	}
	return err
}

// shutdown ends the session for Close. It waits, up to flushTimeout, for
// the writer to send what Write took, sends a normal close frame, and waits
// for the relay's, so that closing the connection does not reset it before
// the relay has read everything. It returns how many of the reader and
// writer ended meanwhile, having taken their results from ends.
func (c *conn) shutdown(ws *websocket.Conn, ends <-chan loopEnd) int {
	flushed := time.NewTimer(flushTimeout)
	defer flushed.Stop()
flush:
	for {
		c.mu.Lock()
		done := c.wrote == c.end() && !c.writing
		c.mu.Unlock()
		if done {
			break
		}
		select {
		case <-c.progress:
		case <-ends:
			return 1 // the WebSocket broke, so nothing more can go
		case <-flushed.C:
			break flush
		}
	}
	msg := websocket.FormatCloseMessage(websocket.CloseNormalClosure, "")
	if err := ws.WriteControl(websocket.CloseMessage, msg, time.Now().Add(closeFrameTimeout)); err != nil {
		return 0
	}
	replied := time.NewTimer(closeFrameTimeout)
	defer replied.Stop()
	ended := 0
	for {
		select {
		case e := <-ends:
			ended++
			if e.reader {
				return ended
			}
		case <-replied.C:
			return ended
		}
	}
}

// end is the stream offset after the last byte Write took. The caller
// holds mu.
func (c *conn) end() uint64 { return c.base + uint64(len(c.out)) }

// trim drops out's bytes before stream offset n. The caller holds mu.
func (c *conn) trim(n uint64) {
	if n <= c.base {
		return
	}
	k := min(n-c.base, uint64(len(c.out)))
	c.out = c.out[k:]
	c.base += k
	if len(c.out) == 0 {
		c.out = nil
	}
}

// readLoop reads frames from ws until it fails or stop is closed.
func (c *conn) readLoop(ws *websocket.Conn, stop <-chan struct{}) error {
	for {
		mt, msg, err := ws.ReadMessage()
		if err != nil {
			return err
		}
		if mt != websocket.BinaryMessage {
			return fmt.Errorf("%w: a text message", errProtocol)
		}
		f, err := parseFrame(msg)
		if err != nil {
			return err
		}
		if f.extra > 0 {
			c.cl.logger.Debug("skipping bytes after an IAP relay frame", "frame", tagName(f.tag), "bytes", f.extra)
		}
		switch f.tag {
		case tagData:
			if err := c.deliver(f.data, stop); err != nil {
				return err
			}
		case tagAck:
			if err := c.confirm(f.count); err != nil {
				return err
			}
		case tagConnectSuccessSID, tagReconnectSuccessAck:
			return fmt.Errorf("%w: %s in an open session", errProtocol, tagName(f.tag))
		default:
			c.cl.logger.Debug("skipping an IAP relay frame with an unknown tag", "frame", tagName(f.tag))
		}
	}
}

// deliver queues received bytes for Read, waiting while the queue is full,
// and asks the writer for an ACK once enough are unacknowledged. After
// Close nobody reads, so the bytes go, and the reader can reach the relay's
// close frame.
func (c *conn) deliver(data []byte, stop <-chan struct{}) error {
	if isDone(c.closing) {
		return nil
	}
	for {
		c.mu.Lock()
		if c.rbuf.Len() < maxReadBuffer {
			c.rbuf.Write(data)
			c.received += uint64(len(data))
			ack := c.received-c.acked > ackWindow
			c.mu.Unlock()
			notify(c.readable)
			if ack {
				notify(c.work)
			}
			return nil
		}
		c.mu.Unlock()
		select {
		case <-c.drained:
		case <-c.closing:
			return nil
		case <-stop:
			return net.ErrClosed
		}
	}
}

// confirm records the relay's acknowledgement of n stream bytes.
func (c *conn) confirm(n uint64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case n < c.confirmed:
		return fmt.Errorf("%w: ACK of %d bytes after ACK of %d", errProtocol, n, c.confirmed)
	case n > c.wrote:
		return fmt.Errorf("%w: ACK of %d bytes when %d were sent", errProtocol, n, c.wrote)
	}
	c.confirmed = n
	c.trim(n)
	return nil
}

// writeLoop sends ACKs and the bytes Write took to ws until it fails or
// stop is closed. An ACK goes first when one is due.
func (c *conn) writeLoop(ws *websocket.Conn, stop <-chan struct{}) error {
	var msg []byte
	for {
		msg = msg[:0]
		c.mu.Lock()
		switch {
		case c.received-c.acked > ackWindow:
			msg = appendAck(msg, c.received)
			c.acked = c.received
		case c.wrote < c.end():
			i := c.wrote - c.base
			n := min(uint64(len(c.out))-i, maxDataFrame)
			msg = appendData(msg, c.out[i:i+n])
			// Count the bytes as written before they are, so an ACK
			// that overtakes this goroutine is not mistaken for one of
			// bytes never sent. If the write fails, resuming sets wrote
			// back to what the relay received.
			c.wrote += n
			c.writing = true
			if c.wrote-c.base > maxUnconfirmed {
				c.trim(c.wrote - maxUnconfirmed)
			}
		}
		c.mu.Unlock()
		if len(msg) == 0 {
			select {
			case <-c.work:
				continue
			case <-stop:
				return net.ErrClosed
			}
		}
		err := ws.WriteMessage(websocket.BinaryMessage, msg)
		c.mu.Lock()
		data := c.writing
		c.writing = false
		c.mu.Unlock()
		if data {
			notify(c.space)
			notify(c.progress)
		}
		if err != nil {
			return err
		}
	}
}

// fail ends the tunnel with err, unless it has already ended.
func (c *conn) fail(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err == nil {
		c.err = err
		close(c.failed)
	}
}

// resumable reports whether the session may continue on a new WebSocket
// after serve stopped with err: the relay asked for new credentials, or
// the connection broke without a close frame. A message too large to read
// would only come again.
func resumable(err error) bool {
	var ce *websocket.CloseError
	switch {
	case errors.As(err, &ce):
		return ce.Code == closeReauthRequired || ce.Code == websocket.CloseAbnormalClosure
	case errors.Is(err, errProtocol), errors.Is(err, websocket.ErrReadLimit):
		return false
	default:
		return true
	}
}

// streamError is what Read and Write return after serve stopped with err
// and the session did not resume.
func (c *conn) streamError(err error) error {
	var ce *websocket.CloseError
	if errors.As(err, &ce) {
		switch ce.Code {
		case websocket.CloseNormalClosure:
			return io.EOF
		case websocket.CloseAbnormalClosure:
		default:
			return relayClosed(ce.Code, ce.Text, c.t)
		}
	}
	return fmt.Errorf("the IAP tunnel to %s broke: %w", c.t, err)
}

// Read reads bytes the relay sent, waiting for some if there are none.
func (c *conn) Read(p []byte) (int, error) {
	for {
		if isDone(c.closing) {
			return 0, net.ErrClosed
		}
		if c.rdl.passed() {
			return 0, os.ErrDeadlineExceeded
		}
		c.mu.Lock()
		if c.rbuf.Len() > 0 {
			n, _ := c.rbuf.Read(p)
			more := c.rbuf.Len() > 0
			c.mu.Unlock()
			notify(c.drained)
			if more {
				notify(c.readable)
			}
			return n, nil
		}
		err := c.err
		c.mu.Unlock()
		if err != nil {
			return 0, err
		}
		if len(p) == 0 {
			return 0, nil
		}
		select {
		case <-c.readable:
		case <-c.failed:
		case <-c.closing:
		case <-c.rdl.done():
		}
	}
}

// Write takes p for the relay, waiting while too much is waiting already.
// It returns once p is buffered, as a socket does; Close sends what is
// buffered before closing.
func (c *conn) Write(p []byte) (int, error) {
	c.wcall.Lock()
	defer c.wcall.Unlock()
	n := 0
	for {
		if err := c.writeError(); err != nil {
			return n, err
		}
		c.mu.Lock()
		room := maxUnwritten - int(c.end()-c.wrote)
		if room > 0 && n < len(p) {
			k := min(room, len(p)-n)
			c.out = append(c.out, p[n:n+k]...)
			n += k
		}
		c.mu.Unlock()
		if room > 0 {
			notify(c.work)
		}
		if n == len(p) {
			return n, nil
		}
		select {
		case <-c.space:
		case <-c.failed:
		case <-c.closing:
		case <-c.wdl.done():
		}
	}
}

// writeError is why Write cannot take bytes now, or nil.
func (c *conn) writeError() error {
	if isDone(c.closing) {
		return net.ErrClosed
	}
	if c.wdl.passed() {
		return os.ErrDeadlineExceeded
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if errors.Is(c.err, io.EOF) {
		return errRelayClosed
	}
	return c.err
}

// Close sends what Write has buffered, if the relay is connected and takes
// it within flushTimeout, then closes the session and stops its goroutines
// before it returns. Calling it again does nothing.
func (c *conn) Close() error {
	c.closeOnce.Do(func() {
		close(c.closing)
		c.cancel()
	})
	<-c.done
	return nil
}

// LocalAddr returns this end of the current WebSocket to the relay.
func (c *conn) LocalAddr() net.Addr {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.local
}

// RemoteAddr returns the tunnel's destination, with the network "iap".
func (c *conn) RemoteAddr() net.Addr { return c.t.addr() }

// SetDeadline implements net.Conn.
func (c *conn) SetDeadline(t time.Time) error {
	c.rdl.set(t)
	c.wdl.set(t)
	return nil
}

// SetReadDeadline implements net.Conn.
func (c *conn) SetReadDeadline(t time.Time) error {
	c.rdl.set(t)
	return nil
}

// SetWriteDeadline implements net.Conn. Write waits only for buffer space,
// so the deadline bounds that wait.
func (c *conn) SetWriteDeadline(t time.Time) error {
	c.wdl.set(t)
	return nil
}
