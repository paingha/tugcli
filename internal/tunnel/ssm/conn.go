package ssm

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	awsv2 "github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/aws/smithy-go"
	"github.com/gorilla/websocket"
)

// clientVersion is the Session Manager plugin version tug reports. The agent
// chooses the mode of a LocalPortForwarding session from it: plugins from
// 1.1.70 on get TCP multiplexing over smux, and older ones get basic port
// forwarding, where the session's stream data is one TCP connection. tug
// opens a session per connection, so it reports 1.1.61.0, the last plugin
// release before multiplexing (1.2.7.0), to be given the basic mode.
const clientVersion = "1.1.61.0"

// terminateFlagAgentVersion is the last agent version that does not
// understand the TerminateSession flag; the plugin calls the
// TerminateSession API for those agents instead.
const terminateFlagAgentVersion = "2.3.722.0"

// Timing and sizes of the data channel. The protocol values are the
// Session Manager plugin's.
const (
	// chunkSize is the most data one input message carries. The plugin and
	// the agent read 1024 bytes at a time.
	chunkSize = 1024

	defaultRTT       = 100 * time.Millisecond
	defaultRTO       = 200 * time.Millisecond
	clockGranularity = 10 * time.Millisecond
	maxRTO           = time.Second
	resendInterval   = 100 * time.Millisecond
	// resendGiveUp is how long unacknowledged input is resent before the
	// channel is given up as dead.
	resendGiveUp = 5 * time.Minute
	pingInterval = 5 * time.Minute

	// maxInFlight is how many input messages may await acknowledgement
	// before Write waits.
	maxInFlight = 4096
	// maxHeld is how many output messages that arrived ahead of a missing
	// one are held; beyond that they are dropped unacknowledged, and the
	// agent resends them.
	maxHeld = 10000
	// readBufferLimit is how much output is buffered for Read before the
	// channel stops reading from the WebSocket.
	readBufferLimit = 4 << 20
	maxMessageSize  = 1 << 20
	outQueue        = 64

	handshakeTimeout = 30 * time.Second
	writeTimeout     = 30 * time.Second
	lingerTimeout    = 2 * time.Second
	flushTimeout     = time.Second
	terminateTimeout = 5 * time.Second

	// A dropped data channel is resumed with up to resumeRetries retries
	// after the first try, waiting resumeDelay (plus up to as much again
	// at random) and doubling up to maxResumeDelay, as the plugin's
	// RepeatableExponentialRetryer does.
	resumeRetries  = 5
	resumeDelay    = 100 * time.Millisecond
	maxResumeDelay = 5 * time.Second
)

// conn is a port session's data channel, presented as a net.Conn.
//
// Three goroutines run while it is open: the reader, which handles every
// message from Session Manager and resumes the session when the WebSocket
// drops; the writer, which owns writes to the WebSocket; and the
// maintainer, which resends unacknowledged input and pings. Close stops all
// three.
type conn struct {
	api       API
	sessionID string
	address   string
	log       *slog.Logger
	dialer    *websocket.Dialer
	signer    *streamSigner
	clientID  string
	// resumeDelay is the first wait before retrying to resume.
	resumeDelay time.Duration
	// kms and target set up KMS encryption when the agent asks for it.
	kms    KMSAPI
	target string

	// handshakes and challenges carry handshake messages from the reader to
	// open.
	handshakes chan message
	challenges chan message

	// The WebSocket is replaced when a dropped channel is resumed.
	wsMu      sync.Mutex
	ws        *websocket.Conn // nil while reconnecting
	wsChanged chan struct{}   // closed and replaced when ws changes
	wsDone    bool            // Close has taken the last WebSocket
	streamURL string

	// out queues binary messages for the writer.
	out chan []byte
	// writeMu makes each Write call atomic.
	writeMu sync.Mutex

	mu           sync.Mutex
	rbuf         bytes.Buffer
	rerr         error // returned by Read once rbuf is empty
	werr         error // returned by Write
	nextSeq      int64
	unacked      []*sentInput
	rtt, rttVar  time.Duration
	rto          time.Duration
	agentVersion string
	byService    bool           // Session Manager closed the channel, so the session is over
	enc          *sessionCipher // set when the session is KMS-encrypted

	// Owned by the reader goroutine.
	expected  int64
	held      map[int64]message
	handshook bool

	readable chan struct{} // notified when output arrives
	space    chan struct{} // notified when Read makes room in rbuf
	acked    chan struct{} // notified when an acknowledgement arrives
	ready    chan struct{} // closed when the handshake completes
	broken   chan struct{} // closed when the channel fails or is closed by Session Manager
	closed   chan struct{} // closed when Close starts
	stop     chan struct{} // closed to stop the writer and the maintainer

	closeOnce sync.Once
	reader    sync.WaitGroup
	workers   sync.WaitGroup

	readDeadline, writeDeadline deadline
}

var _ net.Conn = (*conn)(nil)

// sentInput is an input message kept until the agent acknowledges it.
type sentInput struct {
	seq    int64
	raw    []byte
	first  time.Time
	last   time.Time
	resent bool
}

func newConn(api API, sessionID, address string, log *slog.Logger) *conn {
	return &conn{
		api:         api,
		sessionID:   sessionID,
		address:     address,
		log:         log,
		clientID:    newUUID().String(),
		resumeDelay: resumeDelay,
		wsChanged:   make(chan struct{}),
		handshakes:  make(chan message),
		challenges:  make(chan message),
		out:         make(chan []byte, outQueue),
		rtt:         defaultRTT,
		rto:         defaultRTO,
		held:        map[int64]message{},
		readable:    make(chan struct{}, 1),
		space:       make(chan struct{}, 1),
		acked:       make(chan struct{}, 1),
		ready:       make(chan struct{}),
		broken:      make(chan struct{}),
		closed:      make(chan struct{}),
		stop:        make(chan struct{}),
	}
}

// open connects the data channel and waits for the agent's handshake.
func (c *conn) open(ctx context.Context, dialer *websocket.Dialer, signer *streamSigner, streamURL, token string) error {
	c.dialer, c.signer = dialer, signer
	ws, err := c.dialSocket(ctx, streamURL, token)
	if err != nil {
		return err
	}
	if !c.installSocket(ws, streamURL) {
		return net.ErrClosed
	}
	c.reader.Add(1)
	go c.readLoop()
	c.workers.Add(2)
	go c.writeLoop()
	go c.maintain()

	timer := time.NewTimer(handshakeTimeout)
	defer timer.Stop()
	for {
		select {
		case m := <-c.handshakes:
			if err := c.answerHandshake(ctx, m); err != nil {
				return err
			}
		case m := <-c.challenges:
			if err := c.answerChallenge(m); err != nil {
				return err
			}
		case <-c.ready:
			c.log.Debug("SSM data channel open", "agent", c.agentVersionString(), "encrypted", c.encrypted())
			return nil
		case <-c.broken:
			select {
			case <-c.ready:
				// The session opened and ended at once; Read and Write
				// report how, after any output it carried.
				return nil
			default:
			}
			return c.writeErr()
		case <-ctx.Done():
			return fmt.Errorf("waiting for the agent's handshake: %w", ctx.Err())
		case <-timer.C:
			return fmt.Errorf("the agent did not complete the handshake within %s; check that the instance's SSM agent is up to date", handshakeTimeout)
		}
	}
}

// encrypted reports whether session data is encrypted with KMS.
func (c *conn) encrypted() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.enc != nil
}

// dialSocket opens a WebSocket to the stream URL and hands over the token.
func (c *conn) dialSocket(ctx context.Context, streamURL, token string) (*websocket.Conn, error) {
	u, err := url.Parse(streamURL)
	if err != nil {
		return nil, fmt.Errorf("reading the stream URL: %w", err)
	}
	if u.Scheme != "wss" {
		return nil, fmt.Errorf("the stream URL is %s://, not wss://", u.Scheme)
	}
	var header http.Header
	if c.signer != nil && !presigned(u) {
		if header, err = c.signer.header(ctx, streamURL, time.Now()); err != nil {
			return nil, fmt.Errorf("signing the data channel request: %w", err)
		}
	}
	ws, resp, err := c.dialer.DialContext(ctx, streamURL, header)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		if resp != nil {
			return nil, fmt.Errorf("opening the data channel on %s: %w (HTTP %d)", u.Host, err, resp.StatusCode)
		}
		return nil, fmt.Errorf("opening the data channel on %s: %w", u.Host, err)
	}
	ws.SetReadLimit(maxMessageSize)
	hello, err := json.Marshal(openDataChannel{
		MessageSchemaVersion: "1.0",
		RequestID:            newUUID().String(),
		TokenValue:           token,
		ClientID:             c.clientID,
		ClientVersion:        clientVersion,
	})
	if err != nil {
		_ = ws.Close()
		return nil, fmt.Errorf("encoding the open data channel request: %w", err)
	}
	// The socket is not installed yet, so nothing else writes to it.
	_ = ws.SetWriteDeadline(time.Now().Add(writeTimeout))
	if err := ws.WriteMessage(websocket.TextMessage, hello); err != nil {
		_ = ws.Close()
		return nil, fmt.Errorf("sending the session token: %w", err)
	}
	return ws, nil
}

// installSocket makes ws the channel's WebSocket. It refuses, closing ws,
// once Close has started.
func (c *conn) installSocket(ws *websocket.Conn, streamURL string) bool {
	c.wsMu.Lock()
	defer c.wsMu.Unlock()
	if c.wsDone {
		_ = ws.Close()
		return false
	}
	c.ws, c.streamURL = ws, streamURL
	c.socketChanged()
	return true
}

// dropSocket closes ws and, if it is the channel's WebSocket, leaves the
// channel without one until the session is resumed. Only the reader drops
// a socket: others close one that fails, so that the reader's next read
// fails and it resumes.
func (c *conn) dropSocket(ws *websocket.Conn) {
	c.wsMu.Lock()
	if c.ws == ws {
		c.ws = nil
		c.socketChanged()
	}
	c.wsMu.Unlock()
	_ = ws.Close()
}

// socketChanged wakes whoever waits for a different WebSocket. c.wsMu must
// be held.
func (c *conn) socketChanged() {
	close(c.wsChanged)
	c.wsChanged = make(chan struct{})
}

// takeSocket returns the WebSocket for Close to close, and keeps any later
// one from being installed.
func (c *conn) takeSocket() *websocket.Conn {
	c.wsMu.Lock()
	defer c.wsMu.Unlock()
	c.wsDone = true
	ws := c.ws
	c.ws = nil
	return ws
}

// socket returns the WebSocket, or nil while the channel reconnects.
func (c *conn) socket() *websocket.Conn {
	c.wsMu.Lock()
	defer c.wsMu.Unlock()
	return c.ws
}

// waitSocket returns the WebSocket, waiting while the channel reconnects
// and while it is still failed, the socket a write last failed on.
func (c *conn) waitSocket(failed *websocket.Conn) (*websocket.Conn, error) {
	for {
		c.wsMu.Lock()
		ws, changed := c.ws, c.wsChanged
		c.wsMu.Unlock()
		if ws != nil && ws != failed {
			return ws, nil
		}
		select {
		case <-changed:
		case <-c.stop:
			return nil, net.ErrClosed
		case <-c.broken:
			return nil, c.writeErr()
		}
	}
}

// readLoop handles messages from Session Manager until the WebSocket fails
// for good or the channel is closed.
func (c *conn) readLoop() {
	defer c.reader.Done()
	for {
		ws, err := c.waitSocket(nil)
		if err != nil {
			return
		}
		kind, data, err := ws.ReadMessage()
		if err != nil {
			if c.lost(ws, err) {
				continue
			}
			return
		}
		if kind != websocket.BinaryMessage && kind != websocket.TextMessage {
			continue
		}
		m, err := parseMessage(data)
		if err != nil {
			c.log.Debug("dropping data channel message", "error", err)
			continue
		}
		switch m.Type {
		case typeOutput:
			c.handleOutput(m)
		case typeAcknowledge:
			c.handleAck(m)
		case typeChannelClosed:
			c.handleChannelClosed(m)
			return
		case typeStartPublication, typePausePublication:
			// Session Manager pauses publication while the agent is away.
			// Like the plugin, tug keeps resending unacknowledged input
			// until the agent is back.
		default:
			c.log.Debug("ignoring data channel message", "type", m.Type)
		}
	}
}

// lost handles the WebSocket failing with err. Once the session is up it
// resumes the session and reports true; otherwise, or if resuming fails, it
// records why the channel stopped.
func (c *conn) lost(ws *websocket.Conn, err error) bool {
	select {
	case <-c.closed:
		c.fail(net.ErrClosed, net.ErrClosed)
		return false
	default:
	}
	if !c.resumable() {
		if websocket.IsCloseError(err, websocket.CloseNormalClosure) {
			c.fail(io.EOF, ErrSessionClosed)
			return false
		}
		err = fmt.Errorf("reading the SSM data channel: %w", err)
		c.fail(err, err)
		return false
	}
	if err := c.resume(ws, err); err != nil {
		c.fail(err, err)
		return false
	}
	return true
}

// resumable reports whether a dropped WebSocket should be resumed: the
// handshake is done and nothing else has ended the channel.
func (c *conn) resumable() bool {
	select {
	case <-c.ready:
	default:
		return false
	}
	select {
	case <-c.broken:
		return false
	default:
		return true
	}
}

// permanentError marks a failure to resume that retrying cannot fix.
type permanentError struct{ err error }

func (e *permanentError) Error() string { return e.err.Error() }
func (e *permanentError) Unwrap() error { return e.err }

// resume replaces a dropped WebSocket the way the plugin does: it asks
// ResumeSession for a new stream URL and token, reconnects, and resends the
// input the agent has not acknowledged; the agent resends its own. It
// retries with exponential backoff, but stops at once if Session Manager
// no longer has the session.
func (c *conn) resume(old *websocket.Conn, cause error) error {
	c.dropSocket(old)
	c.log.Info("the SSM data channel dropped; resuming the session", "error", cause)
	ctx, cancel := untilClosed(context.Background(), c.closed)
	defer cancel()
	delay := c.resumeDelay + rand.N(c.resumeDelay)
	var err error
	for attempt := 0; ; attempt++ {
		var ws *websocket.Conn
		if ws, err = c.reconnect(ctx); err == nil {
			if !c.installSocket(ws, c.currentStreamURL()) {
				return net.ErrClosed
			}
			c.log.Info("resumed the SSM session", "attempts", attempt+1, "resent", c.resendUnacked())
			return nil
		}
		var permanent *permanentError
		if errors.As(err, &permanent) || attempt == resumeRetries || ctx.Err() != nil {
			break
		}
		c.log.Debug("resuming the SSM session", "attempt", attempt+1, "error", err)
		select {
		case <-time.After(delay):
		case <-ctx.Done():
		}
		delay = min(delay*2, maxResumeDelay)
	}
	if ctx.Err() != nil {
		return net.ErrClosed
	}
	// The cause was logged when the channel dropped.
	return fmt.Errorf("resuming SSM session %s after the data channel dropped: %w", c.sessionID, err)
}

// reconnect asks ResumeSession for a new token and opens a new WebSocket.
func (c *conn) reconnect(ctx context.Context) (*websocket.Conn, error) {
	out, err := c.api.ResumeSession(ctx, &ssm.ResumeSessionInput{SessionId: awsv2.String(c.sessionID)})
	if err != nil {
		var apiErr smithy.APIError
		if errors.As(err, &apiErr) {
			return nil, &permanentError{err}
		}
		return nil, err
	}
	token := awsv2.ToString(out.TokenValue)
	if token == "" {
		// The plugin takes a resume without a token to mean the session
		// timed out.
		return nil, &permanentError{fmt.Errorf("%w: it timed out", ErrSessionClosed)}
	}
	streamURL := awsv2.ToString(out.StreamUrl)
	if streamURL == "" {
		streamURL = c.currentStreamURL()
	}
	ws, err := c.dialSocket(ctx, streamURL, token)
	if err != nil {
		return nil, err
	}
	c.wsMu.Lock()
	c.streamURL = streamURL
	c.wsMu.Unlock()
	return ws, nil
}

func (c *conn) currentStreamURL() string {
	c.wsMu.Lock()
	defer c.wsMu.Unlock()
	return c.streamURL
}

// resendUnacked sends again every input message the agent has not
// acknowledged, which may have been lost with the old WebSocket, and
// returns how many there were.
func (c *conn) resendUnacked() int {
	now := time.Now()
	c.mu.Lock()
	raws := make([][]byte, len(c.unacked))
	for i, s := range c.unacked {
		s.last = now
		s.resent = true
		raws[i] = s.raw
	}
	c.mu.Unlock()
	for _, raw := range raws {
		if c.enqueue(raw) != nil {
			break
		}
	}
	return len(raws)
}

// untilClosed returns a context that is also cancelled when done is
// closed. Its goroutine ends when the context does.
func untilClosed(parent context.Context, done <-chan struct{}) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	go func() {
		select {
		case <-done:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}

// handleOutput acknowledges an output message and processes output in
// sequence order, holding back messages that arrive early.
func (c *conn) handleOutput(m message) {
	switch {
	case m.Sequence < c.expected:
		// A resend whose acknowledgement was lost: acknowledge it again.
		c.ack(m)
	case m.Sequence > c.expected:
		if _, ok := c.held[m.Sequence]; !ok && len(c.held) >= maxHeld {
			return
		}
		c.held[m.Sequence] = m
		c.ack(m)
	default:
		c.ack(m)
		c.process(m)
		c.expected++
		for {
			next, ok := c.held[c.expected]
			if !ok {
				break
			}
			delete(c.held, c.expected)
			c.process(next)
			c.expected++
		}
	}
}

// process acts on one output message, in sequence order.
func (c *conn) process(m message) {
	switch m.PayloadType {
	case payloadOutput:
		c.mu.Lock()
		enc := c.enc
		c.mu.Unlock()
		payload := m.Payload
		if enc != nil {
			var err error
			if payload, err = enc.decrypt(payload); err != nil {
				err = fmt.Errorf("reading output %d: %w", m.Sequence, err)
				c.fail(err, err)
				return
			}
		}
		c.deliver(payload)
	case payloadHandshakeRequest:
		c.toHandshake(c.handshakes, m)
	case payloadEncChallengeRequest:
		c.toHandshake(c.challenges, m)
	case payloadHandshakeComplete:
		c.handleHandshakeComplete(m)
	case payloadFlag:
		if len(m.Payload) == 4 && flag(binary.BigEndian.Uint32(m.Payload)) == flagConnectToPortError {
			err := fmt.Errorf("the instance could not connect to %s", c.address)
			c.fail(err, err)
		}
	default:
		c.log.Debug("ignoring output", "payloadType", m.PayloadType, "sequence", m.Sequence)
	}
}

// toHandshake hands a handshake message to open, which answers it with the
// context of Dial: answering may call KMS. A message that arrives once the
// handshake is over is dropped.
func (c *conn) toHandshake(ch chan message, m message) {
	select {
	case <-c.ready:
		c.log.Debug("ignoring a handshake message after the handshake", "payloadType", m.PayloadType)
	case ch <- m:
	case <-c.closed:
	}
}

// deliver hands output to Read, waiting while the read buffer is full.
func (c *conn) deliver(p []byte) {
	if len(p) == 0 {
		return
	}
	for {
		c.mu.Lock()
		if c.rbuf.Len() < readBufferLimit {
			c.rbuf.Write(p)
			c.mu.Unlock()
			notify(c.readable)
			return
		}
		c.mu.Unlock()
		select {
		case <-c.space:
		case <-c.closed:
			return
		}
	}
}

// answerHandshake answers the agent's handshake request. tug accepts a Port
// session, with KMS encryption if the agent asks for it, and refuses any
// action it does not know. The error says why the session cannot go on.
func (c *conn) answerHandshake(ctx context.Context, m message) error {
	var req handshakeRequest
	if err := json.Unmarshal(m.Payload, &req); err != nil {
		return fmt.Errorf("reading the agent's handshake request: %w", err)
	}
	c.mu.Lock()
	c.agentVersion = req.AgentVersion
	c.mu.Unlock()

	resp := handshakeResponse{ClientVersion: clientVersion, ProcessedClientActions: []processedAction{}}
	var refused error
	for _, action := range req.RequestedClientActions {
		done := processedAction{ActionType: action.ActionType, ActionStatus: actionSuccess}
		switch action.ActionType {
		case actionSessionType:
			var st sessionTypeRequest
			if err := json.Unmarshal(action.ActionParameters, &st); err != nil || st.SessionType != sessionTypePort {
				done.ActionStatus = actionFailed
				done.Error = fmt.Sprintf("session type %q is not %s", st.SessionType, sessionTypePort)
				refused = fmt.Errorf("the agent started a %q session, not a %s session", st.SessionType, sessionTypePort)
				break
			}
			var props struct {
				Type string `json:"type"`
			}
			_ = json.Unmarshal(st.Properties, &props)
			c.log.Debug("port session", "agent", req.AgentVersion, "type", props.Type)
		case actionKMSEncryption:
			var kr kmsEncryptionRequest
			_ = json.Unmarshal(action.ActionParameters, &kr)
			key, err := c.setUpEncryption(ctx, kr.KMSKeyID)
			if err != nil {
				done.ActionStatus = actionFailed
				done.Error = "setting up KMS encryption: " + err.Error()
				refused = fmt.Errorf("%w: %w", ErrKMSEncryption, err)
				break
			}
			done.ActionResult = kmsEncryptionResponse{KMSCipherTextKey: key}
			c.log.Debug("KMS encryption", "key", kr.KMSKeyID)
		default:
			done.ActionStatus = actionUnsupported
			done.Error = "unsupported action " + action.ActionType
			refused = fmt.Errorf("the agent asked for %s, which tug does not support", action.ActionType)
		}
		if done.Error != "" {
			resp.Errors = append(resp.Errors, done.Error)
		}
		resp.ProcessedClientActions = append(resp.ProcessedClientActions, done)
	}
	if err := c.queueJSON(payloadHandshakeResponse, resp); err != nil {
		return fmt.Errorf("answering the agent's handshake: %w", err)
	}
	return refused
}

func (c *conn) handleHandshakeComplete(m message) {
	var hc handshakeComplete
	if err := json.Unmarshal(m.Payload, &hc); err == nil && hc.CustomerMessage != "" {
		c.log.Info("message from Session Manager", "message", hc.CustomerMessage)
	}
	if !c.handshook {
		c.handshook = true
		close(c.ready)
	}
}

// handleAck forgets an acknowledged input message and updates the
// retransmission timeout from its round trip.
func (c *conn) handleAck(m message) {
	var a ackContent
	if err := json.Unmarshal(m.Payload, &a); err != nil {
		c.log.Debug("dropping acknowledgement", "error", err)
		return
	}
	now := time.Now()
	c.mu.Lock()
	for i, s := range c.unacked {
		if s.seq != a.Sequence {
			continue
		}
		// Karn's rule: the round trip of a resent message is ambiguous.
		if !s.resent {
			c.observeRTT(now.Sub(s.last))
		}
		c.unacked = slices.Delete(c.unacked, i, i+1)
		break
	}
	c.mu.Unlock()
	notify(c.acked)
}

// observeRTT folds a round trip into the retransmission timeout the way the
// plugin and the agent do (Jacobson and Karels). c.mu must be held.
func (c *conn) observeRTT(sample time.Duration) {
	diff := c.rtt - sample
	if diff < 0 {
		diff = -diff
	}
	c.rttVar = c.rttVar*3/4 + diff/4
	c.rtt = c.rtt*7/8 + sample/8
	c.rto = min(c.rtt+max(clockGranularity, 4*c.rttVar), maxRTO)
}

func (c *conn) handleChannelClosed(m message) {
	var cc channelClosed
	// A closed channel is closed whatever else its payload says.
	_ = json.Unmarshal(m.Payload, &cc)
	c.mu.Lock()
	c.byService = true
	c.mu.Unlock()
	c.log.Debug("Session Manager closed the channel", "output", cc.Output)
	if cc.Output == "" {
		c.fail(io.EOF, ErrSessionClosed)
		return
	}
	err := fmt.Errorf("%w: %s", ErrSessionClosed, strings.TrimSpace(cc.Output))
	c.fail(err, err)
}

// ack acknowledges an output message.
func (c *conn) ack(m message) {
	payload, err := json.Marshal(ackContent{MessageType: m.Type, MessageID: m.ID.String(), Sequence: m.Sequence, Sequential: true})
	if err != nil {
		c.log.Debug("encoding acknowledgement", "error", err)
		return
	}
	am := newMessage(typeAcknowledge, 0, flagsAck, 0, payload, time.Now())
	// This fails only when the channel is going away.
	_ = c.enqueue(am.marshal())
}

// queueJSON sends v, encoded as JSON, as the next input message.
func (c *conn) queueJSON(pt payloadType, v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encoding %T: %w", v, err)
	}
	return c.queueInput(pt, payload)
}

// queueInput sends payload as the next input message and keeps it for
// resending until the agent acknowledges it.
func (c *conn) queueInput(pt payloadType, payload []byte) error {
	now := time.Now()
	c.mu.Lock()
	if c.werr != nil {
		err := c.werr
		c.mu.Unlock()
		return err
	}
	if pt == payloadOutput && c.enc != nil {
		payload = c.enc.encrypt(payload)
	}
	m := newMessage(typeInput, c.nextSeq, flagsNone, pt, payload, now)
	raw := m.marshal()
	c.unacked = append(c.unacked, &sentInput{seq: m.Sequence, raw: raw, first: now, last: now})
	c.nextSeq++
	c.mu.Unlock()
	return c.enqueue(raw)
}

// enqueue hands a binary message to the writer.
func (c *conn) enqueue(raw []byte) error {
	select {
	case c.out <- raw:
		return nil
	case <-c.broken:
		return c.writeErr()
	case <-c.stop:
		return net.ErrClosed
	}
}

// writeLoop writes queued messages to the WebSocket, waiting while the
// channel reconnects. A message whose write fails is dropped: input is
// still unacknowledged and is resent once the session resumes, and the
// agent resends output whose acknowledgement was lost. When stopped it
// flushes what is queued, such as a TerminateSession flag, without waiting
// long for a dead network.
func (c *conn) writeLoop() {
	defer c.workers.Done()
	var failed *websocket.Conn
	for {
		select {
		case raw := <-c.out:
			ws, err := c.waitSocket(failed)
			if err != nil {
				return
			}
			if err := writeWS(ws, raw, time.Now().Add(writeTimeout)); err != nil {
				if !c.writeFailed(ws, err) {
					return
				}
				failed = ws
			}
		case <-c.stop:
			ws := c.socket()
			if ws == nil {
				return
			}
			deadline := time.Now().Add(flushTimeout)
			for {
				select {
				case raw := <-c.out:
					if writeWS(ws, raw, deadline) != nil {
						return
					}
				default:
					return
				}
			}
		}
	}
}

func writeWS(ws *websocket.Conn, raw []byte, deadline time.Time) error {
	_ = ws.SetWriteDeadline(deadline)
	return ws.WriteMessage(websocket.BinaryMessage, raw)
}

// writeFailed handles a failed write to ws. If the session can be resumed
// it closes ws, so that the reader's read fails too and it resumes, and
// reports true; otherwise it records the failure.
func (c *conn) writeFailed(ws *websocket.Conn, err error) bool {
	select {
	case <-c.closed:
		return false
	default:
	}
	if c.resumable() {
		c.log.Debug("writing to the SSM data channel", "error", err)
		_ = ws.Close()
		return true
	}
	err = fmt.Errorf("writing to the SSM data channel: %w", err)
	c.fail(err, err)
	return false
}

// maintain resends the oldest unacknowledged input once its retransmission
// timeout passes, and pings the WebSocket so idle sessions stay open.
func (c *conn) maintain() {
	defer c.workers.Done()
	resend := time.NewTicker(resendInterval)
	defer resend.Stop()
	ping := time.NewTicker(pingInterval)
	defer ping.Stop()
	for {
		select {
		case <-c.stop:
			return
		case <-c.broken:
			return
		case now := <-resend.C:
			c.resendOldest(now)
		case <-ping.C:
			ws := c.socket()
			if ws == nil {
				continue
			}
			if err := ws.WriteControl(websocket.PingMessage, []byte("keepalive"), time.Now().Add(writeTimeout)); err != nil && !c.writeFailed(ws, err) {
				return
			}
		}
	}
}

// resendOldest resends the oldest unacknowledged input message if its
// timeout has passed. The agent acknowledges messages that arrive early, so
// only the oldest can be missing. While the channel reconnects there is
// nowhere to send it; resuming resends everything.
func (c *conn) resendOldest(now time.Time) {
	if c.socket() == nil {
		return
	}
	c.mu.Lock()
	if len(c.unacked) == 0 || now.Sub(c.unacked[0].last) <= c.rto {
		c.mu.Unlock()
		return
	}
	s := c.unacked[0]
	if now.Sub(s.first) > resendGiveUp {
		c.mu.Unlock()
		err := fmt.Errorf("the agent has not acknowledged data for %s", resendGiveUp)
		c.fail(err, err)
		return
	}
	s.last = now
	s.resent = true
	seq, raw := s.seq, s.raw
	c.mu.Unlock()
	c.log.Debug("resending input", "sequence", seq)
	_ = c.enqueue(raw)
}

// fail records why the channel stopped working; the first reason wins.
func (c *conn) fail(rerr, werr error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.werr != nil {
		return
	}
	c.rerr, c.werr = rerr, werr
	close(c.broken)
}

func (c *conn) writeErr() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.werr
}

func (c *conn) agentVersionString() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.agentVersion
}

// Read reads output from the remote address, in order.
func (c *conn) Read(b []byte) (int, error) {
	for {
		select {
		case <-c.closed:
			return 0, net.ErrClosed
		case <-c.readDeadline.wait():
			return 0, os.ErrDeadlineExceeded
		default:
		}
		if len(b) == 0 {
			return 0, nil
		}
		c.mu.Lock()
		if c.rbuf.Len() > 0 {
			n, _ := c.rbuf.Read(b)
			c.mu.Unlock()
			notify(c.space)
			return n, nil
		}
		err := c.rerr
		c.mu.Unlock()
		if err != nil {
			return 0, err
		}
		select {
		case <-c.readable:
		case <-c.broken:
		case <-c.closed:
			return 0, net.ErrClosed
		case <-c.readDeadline.wait():
			return 0, os.ErrDeadlineExceeded
		}
	}
}

// Write sends b to the remote address in messages of up to chunkSize bytes.
func (c *conn) Write(b []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	n := 0
	for n < len(b) {
		if err := c.waitWindow(); err != nil {
			return n, err
		}
		end := min(n+chunkSize, len(b))
		if err := c.queueInput(payloadOutput, b[n:end]); err != nil {
			return n, err
		}
		n = end
	}
	return n, nil
}

// waitWindow waits until fewer than maxInFlight input messages await
// acknowledgement.
func (c *conn) waitWindow() error {
	for {
		select {
		case <-c.closed:
			return net.ErrClosed
		case <-c.writeDeadline.wait():
			return os.ErrDeadlineExceeded
		default:
		}
		c.mu.Lock()
		err, full := c.werr, len(c.unacked) >= maxInFlight
		c.mu.Unlock()
		if err != nil {
			return err
		}
		if !full {
			return nil
		}
		select {
		case <-c.acked:
		case <-c.broken:
		case <-c.closed:
			return net.ErrClosed
		case <-c.writeDeadline.wait():
			return os.ErrDeadlineExceeded
		}
	}
}

// Close ends the session. It waits briefly for the agent to acknowledge
// data already written, tells the agent to stop, closes the WebSocket and
// calls TerminateSession. Calls after the first return nil.
func (c *conn) Close() error {
	var err error
	c.closeOnce.Do(func() { err = c.shutdown() })
	return err
}

func (c *conn) shutdown() error {
	close(c.closed)
	if c.socket() != nil {
		c.linger()
		if c.agentStopsOnFlag() {
			payload := binary.BigEndian.AppendUint32(nil, uint32(flagTerminateSession))
			c.mu.Lock()
			m := newMessage(typeInput, c.nextSeq, flagsNone, payloadFlag, payload, time.Now())
			c.nextSeq++
			c.mu.Unlock()
			select {
			case c.out <- m.marshal():
			case <-c.broken:
			case <-time.After(flushTimeout):
			}
		}
	}
	close(c.stop)
	c.workers.Wait()
	if ws := c.takeSocket(); ws != nil {
		_ = ws.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(flushTimeout))
		_ = ws.Close()
	}
	c.reader.Wait()
	c.readDeadline.stop()
	c.writeDeadline.stop()

	c.mu.Lock()
	over := c.byService
	c.mu.Unlock()
	if over {
		return nil
	}
	return c.terminate()
}

// linger waits a little for the agent to acknowledge input already written,
// as closing a TCP socket does not discard data already sent.
func (c *conn) linger() {
	timer := time.NewTimer(lingerTimeout)
	defer timer.Stop()
	poll := time.NewTicker(50 * time.Millisecond)
	defer poll.Stop()
	for {
		c.mu.Lock()
		n := len(c.unacked)
		c.mu.Unlock()
		if n == 0 || c.socket() == nil {
			return
		}
		select {
		case <-c.acked:
		case <-poll.C:
		case <-c.broken:
			return
		case <-timer.C:
			c.log.Debug("closing with unacknowledged input", "messages", n)
			return
		}
	}
}

// agentStopsOnFlag reports whether the channel is healthy and the agent
// understands the TerminateSession flag.
func (c *conn) agentStopsOnFlag() bool {
	select {
	case <-c.ready:
	default:
		return false
	}
	select {
	case <-c.broken:
		return false
	default:
	}
	return versionAfter(c.agentVersionString(), terminateFlagAgentVersion)
}

// terminate calls TerminateSession, so the session does not wait out its
// idle timeout.
func (c *conn) terminate() error {
	if c.sessionID == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), terminateTimeout)
	defer cancel()
	if _, err := c.api.TerminateSession(ctx, &ssm.TerminateSessionInput{SessionId: awsv2.String(c.sessionID)}); err != nil {
		return fmt.Errorf("ending SSM session %s: %w", c.sessionID, err)
	}
	c.log.Debug("ended SSM session")
	return nil
}

// abort gives up on a channel that never opened and ends its session.
func (c *conn) abort(err error) {
	c.fail(err, err)
	_ = c.Close()
}

// LocalAddr returns the session ID.
func (c *conn) LocalAddr() net.Addr { return addr(c.sessionID) }

// RemoteAddr returns the address the instance forwards to.
func (c *conn) RemoteAddr() net.Addr { return addr(c.address) }

// SetDeadline sets the read and write deadlines.
func (c *conn) SetDeadline(t time.Time) error {
	c.readDeadline.set(t)
	c.writeDeadline.set(t)
	return nil
}

// SetReadDeadline sets the deadline for Read.
func (c *conn) SetReadDeadline(t time.Time) error {
	c.readDeadline.set(t)
	return nil
}

// SetWriteDeadline sets the deadline for Write.
func (c *conn) SetWriteDeadline(t time.Time) error {
	c.writeDeadline.set(t)
	return nil
}

// addr is an address on an SSM tunnel.
type addr string

func (a addr) Network() string { return Scheme }
func (a addr) String() string  { return string(a) }

// notify wakes a waiter on ch without blocking.
func notify(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// deadline is a resettable deadline that closes a channel when it passes, so
// blocked calls can wait on it. The zero value has no deadline.
type deadline struct {
	mu    sync.Mutex
	gen   uint64
	timer *time.Timer
	ch    chan struct{} // closed while the deadline has passed
}

// set moves the deadline to t; the zero time removes it.
func (d *deadline) set(t time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.gen++
	if d.timer != nil {
		d.timer.Stop()
		d.timer = nil
	}
	if d.ch == nil || isClosed(d.ch) {
		d.ch = make(chan struct{})
	}
	if t.IsZero() {
		return
	}
	wait := time.Until(t)
	if wait <= 0 {
		close(d.ch)
		return
	}
	gen, ch := d.gen, d.ch
	d.timer = time.AfterFunc(wait, func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		// A later set has replaced this deadline.
		if d.gen == gen {
			close(ch)
		}
	})
}

// wait returns a channel that is closed once the deadline has passed.
func (d *deadline) wait() <-chan struct{} {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ch == nil {
		d.ch = make(chan struct{})
	}
	return d.ch
}

// stop cancels a pending timer.
func (d *deadline) stop() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.gen++
	if d.timer != nil {
		d.timer.Stop()
		d.timer = nil
	}
}

func isClosed(ch chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// streamSigner signs the data channel's WebSocket request with SigV4 for the
// ssmmessages service, as current Session Manager plugins do. Older plugins
// relied on the session token alone, which Session Manager still accepts.
// It takes credentials from the shared cache each time, so a resumed or
// later session signs with fresh ones.
type streamSigner struct {
	creds  awsv2.CredentialsProvider
	region string
}

// emptyPayloadHash is the SHA-256 of an empty body.
const emptyPayloadHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

func (s *streamSigner) header(ctx context.Context, streamURL string, now time.Time) (http.Header, error) {
	creds, err := s.creds.Retrieve(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolving AWS credentials: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, streamURL, nil)
	if err != nil {
		return nil, err
	}
	if err := v4.NewSigner().SignHTTP(ctx, creds, req, emptyPayloadHash, "ssmmessages", s.region, now); err != nil {
		return nil, err
	}
	return req.Header, nil
}

// presigned reports whether the stream URL carries its own signature.
func presigned(u *url.URL) bool {
	q := u.Query()
	for _, key := range []string{"X-Amz-Algorithm", "X-Amz-Credential", "X-Amz-Date", "X-Amz-Expires",
		"X-Amz-SignedHeaders", "X-Amz-Signature", "X-Amz-Security-Token"} {
		if q.Has(key) {
			return true
		}
	}
	return false
}

// versionAfter reports whether the dotted version v is later than than.
// Parts that are not numbers count as zero.
func versionAfter(v, than string) bool {
	a, b := strings.Split(v, "."), strings.Split(than, ".")
	for i := range max(len(a), len(b)) {
		x, y := versionPart(a, i), versionPart(b, i)
		if x != y {
			return x > y
		}
	}
	return false
}

func versionPart(parts []string, i int) int {
	if i >= len(parts) {
		return 0
	}
	n, err := strconv.Atoi(parts[i])
	if err != nil {
		return 0
	}
	return n
}
