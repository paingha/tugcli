package ssm

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	awsv2 "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/gorilla/websocket"
)

const (
	testTarget    = "i-0123456789abcdef0"
	testSessionID = "tug-test-0123456789"
	testToken     = "token-for-test"
)

// fakeAgent plays Session Manager and the SSM agent at the far end of a
// data channel. It serves the WebSocket, runs the Port session handshake and
// echoes input back as output, acknowledging, holding back and reordering
// messages the way the agent does.
type fakeAgent struct {
	agentVersion string
	// actions are the handshake's requested actions; nil asks for a Port
	// session.
	actions []requestedAction
	// greeting is output sent right after the handshake, deliberately out
	// of order and with a duplicate.
	greeting []string
	// dropFirst ignores the first data message, neither processing nor
	// acknowledging it, so the client must resend it.
	dropFirst bool
	// silent never starts the handshake.
	silent bool
	// closeWith, when set, closes the channel after the handshake with
	// this output.
	closeWith *string

	done chan struct{} // closed when the session ends

	mu       sync.Mutex
	problems []string
	header   http.Header
	hello    openDataChannel
	inputs   []message     // every input message, resends included
	acks     map[int64]int // output sequence numbers the client acknowledged
	sent     []int64       // output sequence numbers sent, duplicates included
	response *handshakeResponse
	flags    []flag
}

// agentSession is the state of one data channel on the agent's side.
type agentSession struct {
	a        *fakeAgent
	ws       *websocket.Conn
	expected int64
	held     map[int64]message
	nextSeq  int64
	dropped  bool
}

func (a *fakeAgent) problem(format string, args ...any) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.problems = append(a.problems, fmt.Sprintf(format, args...))
}

// check fails the test with whatever went wrong on the agent's side.
func (a *fakeAgent) check(t *testing.T) {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, p := range a.problems {
		t.Errorf("fake agent: %s", p)
	}
}

func (a *fakeAgent) serve(w http.ResponseWriter, r *http.Request) {
	defer close(a.done)
	ws, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
	if err != nil {
		a.problem("upgrading: %v", err)
		return
	}
	defer ws.Close()
	a.mu.Lock()
	a.header = r.Header.Clone()
	a.mu.Unlock()

	kind, data, err := ws.ReadMessage()
	if err != nil {
		a.problem("reading the open data channel request: %v", err)
		return
	}
	var hello openDataChannel
	if kind != websocket.TextMessage || json.Unmarshal(data, &hello) != nil {
		a.problem("first message is not an open data channel request: %q", data)
		return
	}
	a.mu.Lock()
	a.hello = hello
	a.mu.Unlock()
	if hello.TokenValue != testToken {
		a.problem("token = %q, want %q", hello.TokenValue, testToken)
		return
	}

	s := &agentSession{a: a, ws: ws, held: map[int64]message{}}
	if !a.silent {
		actions := a.actions
		if actions == nil {
			actions = []requestedAction{{
				ActionType:       actionSessionType,
				ActionParameters: json.RawMessage(`{"SessionType":"Port","Properties":{"portNumber":"443","type":"LocalPortForwarding","host":"10.0.0.1","localPortNumber":"0"}}`),
			}}
		}
		s.sendJSON(payloadHandshakeRequest, handshakeRequest{AgentVersion: a.agentVersion, RequestedClientActions: actions})
	}
	for {
		_, data, err := ws.ReadMessage()
		if err != nil {
			return
		}
		m, err := parseMessage(data)
		if err != nil {
			a.problem("client sent a bad message: %v", err)
			continue
		}
		switch m.Type {
		case typeAcknowledge:
			s.acknowledged(m)
		case typeInput:
			s.input(m)
		default:
			a.problem("client sent a %s message", m.Type)
		}
	}
}

func (s *agentSession) acknowledged(m message) {
	var a ackContent
	if err := json.Unmarshal(m.Payload, &a); err != nil {
		s.a.problem("bad acknowledgement %q: %v", m.Payload, err)
		return
	}
	if a.MessageType != typeOutput || !a.Sequential || m.Flags != flagsAck {
		s.a.problem("acknowledgement %+v with flags %d", a, m.Flags)
	}
	s.a.mu.Lock()
	defer s.a.mu.Unlock()
	if s.a.acks == nil {
		s.a.acks = map[int64]int{}
	}
	s.a.acks[a.Sequence]++
}

// input handles an input message the way the agent's data channel does:
// in order, holding back and acknowledging messages that arrive early.
func (s *agentSession) input(m message) {
	s.a.mu.Lock()
	s.a.inputs = append(s.a.inputs, m)
	s.a.mu.Unlock()
	if s.a.dropFirst && m.PayloadType == payloadOutput && !s.dropped {
		s.dropped = true
		return
	}
	switch {
	case m.Sequence < s.expected:
		s.ack(m)
	case m.Sequence > s.expected:
		s.held[m.Sequence] = m
		s.ack(m)
	default:
		s.ack(m)
		s.process(m)
		s.expected++
		for {
			next, ok := s.held[s.expected]
			if !ok {
				break
			}
			delete(s.held, s.expected)
			s.process(next)
			s.expected++
		}
	}
}

func (s *agentSession) process(m message) {
	switch m.PayloadType {
	case payloadHandshakeResponse:
		var resp handshakeResponse
		if err := json.Unmarshal(m.Payload, &resp); err != nil {
			s.a.problem("bad handshake response %q: %v", m.Payload, err)
			return
		}
		s.a.mu.Lock()
		s.a.response = &resp
		s.a.mu.Unlock()
		for _, action := range resp.ProcessedClientActions {
			if action.ActionStatus != actionSuccess {
				return // the agent cancels the session
			}
		}
		s.sendJSON(payloadHandshakeComplete, handshakeComplete{HandshakeTimeToComplete: time.Second})
		s.greet()
		if s.a.closeWith != nil {
			s.write(newMessage(typeChannelClosed, 0, flagsAck, 0, mustJSON(channelClosed{
				MessageType: typeChannelClosed, SessionID: testSessionID, SchemaVersion: 1, Output: *s.a.closeWith,
			}), time.Now()))
		}
	case payloadOutput:
		s.send(payloadOutput, m.Payload)
	case payloadFlag:
		s.a.mu.Lock()
		s.a.flags = append(s.a.flags, flag(binary.BigEndian.Uint32(m.Payload)))
		s.a.mu.Unlock()
	default:
		s.a.problem("unexpected input payload type %d", m.PayloadType)
	}
}

// greet sends the greeting last part first, then the second part again.
func (s *agentSession) greet() {
	var parts []message
	for _, p := range s.a.greeting {
		parts = append(parts, s.next(payloadOutput, []byte(p)))
	}
	slices.Reverse(parts)
	for _, m := range parts {
		s.write(m)
	}
	if len(parts) > 1 {
		s.write(parts[len(parts)-2])
	}
}

func (s *agentSession) ack(m message) {
	s.write(newMessage(typeAcknowledge, 0, flagsAck, 0, mustJSON(ackContent{
		MessageType: m.Type, MessageID: m.ID.String(), Sequence: m.Sequence, Sequential: true,
	}), time.Now()))
}

// next numbers an output message.
func (s *agentSession) next(pt payloadType, payload []byte) message {
	var f uint64
	if s.nextSeq == 0 {
		f = 1 // SYN, as the agent sets on its first message
	}
	m := newMessage(typeOutput, s.nextSeq, f, pt, payload, time.Now())
	s.nextSeq++
	return m
}

func (s *agentSession) send(pt payloadType, payload []byte) { s.write(s.next(pt, payload)) }

func (s *agentSession) sendJSON(pt payloadType, v any) { s.send(pt, mustJSON(v)) }

func (s *agentSession) write(m message) {
	if m.Type == typeOutput {
		s.a.mu.Lock()
		s.a.sent = append(s.a.sent, m.Sequence)
		s.a.mu.Unlock()
	}
	// A write can fail once the client has closed the WebSocket, as when
	// acknowledging its last message; the client side is checked instead.
	_ = s.ws.WriteMessage(websocket.BinaryMessage, m.marshal())
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// fakeAPI is the SSM API: StartSession points at a fake agent.
type fakeAPI struct {
	streamURL string
	startErr  error

	mu         sync.Mutex
	starts     []*ssm.StartSessionInput
	terminated []string
}

func (f *fakeAPI) StartSession(_ context.Context, in *ssm.StartSessionInput, _ ...func(*ssm.Options)) (*ssm.StartSessionOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts = append(f.starts, in)
	if f.startErr != nil {
		return nil, f.startErr
	}
	return &ssm.StartSessionOutput{
		SessionId:  awsv2.String(testSessionID),
		StreamUrl:  awsv2.String(f.streamURL),
		TokenValue: awsv2.String(testToken),
	}, nil
}

func (f *fakeAPI) TerminateSession(_ context.Context, in *ssm.TerminateSessionInput, _ ...func(*ssm.Options)) (*ssm.TerminateSessionOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.terminated = append(f.terminated, awsv2.ToString(in.SessionId))
	return &ssm.TerminateSessionOutput{SessionId: in.SessionId}, nil
}

func (f *fakeAPI) terminations() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.terminated)
}

// startAgent serves a over TLS and returns the stream URL and a dialer that
// trusts the server.
func startAgent(t *testing.T, a *fakeAgent) (string, *websocket.Dialer) {
	t.Helper()
	if a.agentVersion == "" {
		a.agentVersion = "3.3.1142.0"
	}
	a.done = make(chan struct{})
	srv := httptest.NewTLSServer(http.HandlerFunc(a.serve))
	t.Cleanup(srv.Close)
	tlsConfig := srv.Client().Transport.(*http.Transport).TLSClientConfig
	streamURL := "wss://" + srv.Listener.Addr().String() + "/v1/data-channel/" + testSessionID + "?role=publish_subscribe"
	return streamURL, &websocket.Dialer{TLSClientConfig: tlsConfig, HandshakeTimeout: 5 * time.Second}
}

// newFake starts a fake agent and returns the options that make Dial use
// it through a fake SSM API.
func newFake(t *testing.T, a *fakeAgent) (*fakeAPI, []Option) {
	t.Helper()
	streamURL, dialer := startAgent(t, a)
	api := &fakeAPI{streamURL: streamURL}
	return api, []Option{WithAPI(api), WithDialer(dialer)}
}

// eventually waits for cond, failing the test after five seconds.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitDone waits for the fake agent's session to end.
func (a *fakeAgent) waitDone(t *testing.T) {
	t.Helper()
	select {
	case <-a.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the fake agent's session did not end")
	}
}
