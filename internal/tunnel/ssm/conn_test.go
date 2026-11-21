package ssm

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	awsv2 "github.com/aws/aws-sdk-go-v2/aws"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

func dialFake(t *testing.T, a *fakeAgent) (net.Conn, *fakeAPI) {
	t.Helper()
	api, opts := newFake(t, a)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := Dial(ctx, Config{Target: testTarget}, "10.0.0.1:443", opts...)
	if err != nil {
		a.check(t)
		t.Fatalf("Dial() error = %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, api
}

func TestDialEchoesThroughTheDataChannel(t *testing.T) {
	agent := &fakeAgent{greeting: []string{"hello, ", "out of ", "order"}, dropFirst: true}
	c, api := dialFake(t, agent)

	if len(api.starts) != 1 {
		t.Fatalf("StartSession calls = %d, want 1", len(api.starts))
	}
	in := api.starts[0]
	if *in.Target != testTarget || *in.DocumentName != DocumentName {
		t.Errorf("StartSession(Target %q, DocumentName %q), want %q, %q", *in.Target, *in.DocumentName, testTarget, DocumentName)
	}
	wantParams := map[string][]string{"host": {"10.0.0.1"}, "portNumber": {"443"}}
	if len(in.Parameters) != len(wantParams) || !slices.Equal(in.Parameters["host"], wantParams["host"]) ||
		!slices.Equal(in.Parameters["portNumber"], wantParams["portNumber"]) {
		t.Errorf("StartSession Parameters = %v, want %v", in.Parameters, wantParams)
	}

	// The greeting arrives last part first, with a duplicate.
	greeting := make([]byte, len("hello, out of order"))
	if _, err := io.ReadFull(c, greeting); err != nil {
		t.Fatalf("reading the greeting: %v", err)
	}
	if string(greeting) != "hello, out of order" {
		t.Errorf("greeting = %q, want %q", greeting, "hello, out of order")
	}

	data := make([]byte, 64*1024+100)
	_, _ = rand.Read(data)
	if n, err := c.Write(data); err != nil || n != len(data) {
		t.Fatalf("Write() = %d, %v, want %d, nil", n, err, len(data))
	}
	echo := make([]byte, len(data))
	if _, err := io.ReadFull(c, echo); err != nil {
		t.Fatalf("reading the echo: %v", err)
	}
	if !bytes.Equal(echo, data) {
		t.Fatal("echo differs from the data written")
	}

	s := agent.session(t, 0)
	hello, resp := s.hello(), s.handshakeResponse()
	s.mu.Lock()
	var seqs []int64
	var sizes []int
	for _, m := range s.inputs {
		if m.PayloadType == payloadOutput {
			seqs = append(seqs, m.Sequence)
			sizes = append(sizes, len(m.Payload))
		}
	}
	s.mu.Unlock()

	if hello.ClientVersion != clientVersion || hello.MessageSchemaVersion != "1.0" || hello.ClientID == "" || hello.RequestID == "" {
		t.Errorf("open data channel request = %+v", hello)
	}
	if resp == nil {
		t.Fatal("the agent got no handshake response")
	}
	if resp.ClientVersion != clientVersion || len(resp.ProcessedClientActions) != 1 ||
		resp.ProcessedClientActions[0].ActionType != actionSessionType || resp.ProcessedClientActions[0].ActionStatus != actionSuccess {
		t.Errorf("handshake response = %+v", resp)
	}
	if sizes[0] != chunkSize || slices.Max(sizes) != chunkSize {
		t.Errorf("input payload sizes = %v, want frames of %d bytes", sizes, chunkSize)
	}
	// The agent dropped the first data message, so it came again.
	if len(seqs) < 2 || !slices.Contains(seqs[1:], seqs[0]) {
		t.Errorf("data sequence numbers %v do not resend %d", seqs, seqs[0])
	}

	// Every output message is acknowledged, the duplicate twice.
	eventually(t, "acknowledgements", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		want := map[int64]int{}
		for _, seq := range s.sent {
			want[seq]++
		}
		for seq, n := range want {
			if s.acks[seq] < n {
				return false
			}
		}
		return true
	})
	// And the agent's acknowledgements clear every input message.
	eventually(t, "the agent's acknowledgements", func() bool {
		cc := c.(*conn)
		cc.mu.Lock()
		defer cc.mu.Unlock()
		return len(cc.unacked) == 0
	})

	if err := c.Close(); err != nil {
		t.Errorf("Close() error = %v", err)
	}
	if err := c.Close(); err != nil {
		t.Errorf("second Close() error = %v", err)
	}
	s.waitDone(t)
	if flags := s.flagsSent(); !slices.Equal(flags, []flag{flagTerminateSession}) {
		t.Errorf("flags sent = %v, want [%d]", flags, flagTerminateSession)
	}
	if got := api.terminations(); !slices.Equal(got, []string{s.id}) {
		t.Errorf("TerminateSession calls = %v, want [%s]", got, s.id)
	}
	if _, err := c.Read(make([]byte, 1)); !errors.Is(err, net.ErrClosed) {
		t.Errorf("Read() after Close error = %v, want net.ErrClosed", err)
	}
	if _, err := c.Write([]byte("x")); !errors.Is(err, net.ErrClosed) {
		t.Errorf("Write() after Close error = %v, want net.ErrClosed", err)
	}
	if got := c.LocalAddr().String(); got != s.id {
		t.Errorf("LocalAddr() = %q, want %q", got, s.id)
	}
	if got := c.RemoteAddr(); got.Network() != "ssm" || got.String() != "10.0.0.1:443" {
		t.Errorf("RemoteAddr() = %s %s, want ssm 10.0.0.1:443", got.Network(), got)
	}
	agent.check(t)
}

func TestDialRefusesWhatItCannotDo(t *testing.T) {
	tests := []struct {
		name    string
		actions []requestedAction
		wantErr error
		wantMsg string
	}{
		{
			name: "KMS encryption",
			actions: []requestedAction{
				{ActionType: actionSessionType, ActionParameters: json.RawMessage(`{"SessionType":"Port","Properties":{}}`)},
				{ActionType: actionKMSEncryption, ActionParameters: json.RawMessage(`{"KMSKeyId":"arn:aws:kms:eu-west-1:111122223333:key/x"}`)},
			},
			wantErr: ErrKMSEncryption,
		},
		{
			name:    "shell session",
			actions: []requestedAction{{ActionType: actionSessionType, ActionParameters: json.RawMessage(`{"SessionType":"Standard_Stream"}`)}},
			wantMsg: `"Standard_Stream" session`,
		},
		{
			name:    "unknown action",
			actions: []requestedAction{{ActionType: "Teleport", ActionParameters: json.RawMessage(`{}`)}},
			wantMsg: "Teleport",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agent := &fakeAgent{actions: tt.actions}
			api, opts := newFake(t, agent)
			_, err := Dial(context.Background(), Config{Target: testTarget}, "10.0.0.1:443", opts...)
			if err == nil {
				t.Fatal("Dial() error = nil")
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Errorf("Dial() error = %v, want %v", err, tt.wantErr)
			}
			if tt.wantMsg != "" && !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("Dial() error = %v, want it to mention %s", err, tt.wantMsg)
			}
			s := agent.session(t, 0)
			s.waitDone(t)
			resp := s.handshakeResponse()
			if resp == nil || len(resp.ProcessedClientActions) != len(tt.actions) || len(resp.Errors) == 0 {
				t.Fatalf("handshake response = %+v, want one result per action and an error", resp)
			}
			last := resp.ProcessedClientActions[len(resp.ProcessedClientActions)-1]
			if last.ActionStatus == actionSuccess {
				t.Errorf("handshake response %+v accepts %s", last, last.ActionType)
			}
			if got := api.terminations(); !slices.Equal(got, []string{s.id}) {
				t.Errorf("TerminateSession calls = %v, want [%s]", got, s.id)
			}
			agent.check(t)
		})
	}
}

func TestSessionManagerClosesTheChannel(t *testing.T) {
	tests := []struct {
		name      string
		output    string
		wantRead  error
		wantWrite error
	}{
		{name: "no reason", output: "", wantRead: io.EOF, wantWrite: ErrSessionClosed},
		{name: "with a reason", output: "Unable to connect to specified port", wantRead: ErrSessionClosed, wantWrite: ErrSessionClosed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agent := &fakeAgent{greeting: []string{"bye"}, closeWith: &tt.output}
			c, api := dialFake(t, agent)
			buf := make([]byte, 3)
			if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "bye" {
				t.Fatalf("reading before the close = %q, %v, want %q", buf, err, "bye")
			}
			_, err := c.Read(buf)
			if !errors.Is(err, tt.wantRead) {
				t.Errorf("Read() error = %v, want %v", err, tt.wantRead)
			}
			if tt.output != "" && !strings.Contains(err.Error(), tt.output) {
				t.Errorf("Read() error = %v, want it to say %q", err, tt.output)
			}
			if _, err := c.Write([]byte("x")); !errors.Is(err, tt.wantWrite) {
				t.Errorf("Write() error = %v, want %v", err, tt.wantWrite)
			}
			if err := c.Close(); err != nil {
				t.Errorf("Close() error = %v", err)
			}
			if got := api.terminations(); len(got) != 0 {
				t.Errorf("TerminateSession calls = %v for a session Session Manager ended", got)
			}
			agent.check(t)
		})
	}
}

func TestDialStopsWithItsContext(t *testing.T) {
	agent := &fakeAgent{silent: true}
	api, opts := newFake(t, agent)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := Dial(ctx, Config{Target: testTarget}, "10.0.0.1:443", opts...)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Dial() error = %v, want context.DeadlineExceeded", err)
	}
	if took := time.Since(start); took > lingerTimeout {
		t.Errorf("Dial() took %s after its context ended", took)
	}
	s := agent.session(t, 0)
	s.waitDone(t)
	if got := api.terminations(); !slices.Equal(got, []string{s.id}) {
		t.Errorf("TerminateSession calls = %v, want [%s]", got, s.id)
	}
	agent.check(t)
}

func TestDeadlines(t *testing.T) {
	agent := &fakeAgent{}
	c, _ := dialFake(t, agent)

	if err := c.SetReadDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	_, err := c.Read(make([]byte, 1))
	var netErr net.Error
	if !errors.Is(err, os.ErrDeadlineExceeded) || !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Fatalf("Read() past the deadline error = %v, want a timeout", err)
	}
	if err := c.SetDeadline(time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write([]byte("x")); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("Write() past the deadline error = %v, want os.ErrDeadlineExceeded", err)
	}

	if err := c.SetDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write([]byte("ping")); err != nil {
		t.Fatalf("Write() after clearing the deadline: %v", err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "ping" {
		t.Errorf("Read() after clearing the deadline = %q, %v, want %q", buf, err, "ping")
	}

	// A blocked Read wakes when the deadline moves closer.
	if err := c.SetReadDeadline(time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, 1)
	go func() {
		_, err := c.Read(buf)
		errs <- err
	}()
	time.Sleep(50 * time.Millisecond)
	if err := c.SetReadDeadline(time.Now()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errs:
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Errorf("blocked Read() error = %v, want os.ErrDeadlineExceeded", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a blocked Read did not wake for its deadline")
	}

	// Close wakes a blocked Read too.
	if err := c.SetReadDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	go func() {
		_, err := c.Read(buf)
		errs <- err
	}()
	time.Sleep(50 * time.Millisecond)
	_ = c.Close()
	select {
	case err := <-errs:
		if !errors.Is(err, net.ErrClosed) {
			t.Errorf("Read() blocked across Close error = %v, want net.ErrClosed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not wake a blocked Read")
	}
	agent.check(t)
}

func TestStartSessionErrors(t *testing.T) {
	tests := []struct {
		name    string
		err     error
		wantMsg string
	}{
		{"not connected", &ssmtypes.TargetNotConnected{Message: awsv2.String("i-0123456789abcdef0 is not connected.")}, "not connected to Session Manager"},
		{"unknown instance", &ssmtypes.InvalidTarget{Message: awsv2.String("invalid target")}, "check the instance ID and the region"},
		{"other", errors.New("boom"), "boom"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := &fakeAPI{startErr: tt.err}
			_, err := Dial(context.Background(), Config{Target: testTarget}, "10.0.0.1:443", WithAPI(api))
			if !errors.Is(err, tt.err) || !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("Dial() error = %v, want %v saying %q", err, tt.err, tt.wantMsg)
			}
			if got := api.terminations(); len(got) != 0 {
				t.Errorf("TerminateSession calls = %v without a session", got)
			}
		})
	}
}

func TestDialChecksItsArguments(t *testing.T) {
	api := &fakeAPI{}
	tests := []struct {
		name    string
		cfg     Config
		address string
	}{
		{"bad target", Config{Target: "web-1"}, "10.0.0.1:443"},
		{"bad region", Config{Target: testTarget, Region: "us east"}, "10.0.0.1:443"},
		{"no port", Config{Target: testTarget}, "10.0.0.1"},
		{"port zero", Config{Target: testTarget}, "10.0.0.1:0"},
		{"named port", Config{Target: testTarget}, "10.0.0.1:https"},
		{"no host", Config{Target: testTarget}, ":443"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Dial(context.Background(), tt.cfg, tt.address, WithAPI(api)); err == nil {
				t.Errorf("Dial(%+v, %q) error = nil", tt.cfg, tt.address)
			}
		})
	}
	if len(api.starts) != 0 {
		t.Errorf("StartSession calls = %d for bad arguments", len(api.starts))
	}
}

func TestStreamURLMustBeSecure(t *testing.T) {
	api := &fakeAPI{streamURL: "ws://ssmmessages.eu-west-1.amazonaws.com/v1/data-channel/x"}
	_, err := Dial(context.Background(), Config{Target: testTarget}, "10.0.0.1:443", WithAPI(api))
	if err == nil || !strings.Contains(err.Error(), "wss://") {
		t.Errorf("Dial() error = %v, want a refusal of ws://", err)
	}
	if got := api.terminations(); !slices.Equal(got, []string{testSessionID}) {
		t.Errorf("TerminateSession calls = %v, want [%s]", got, testSessionID)
	}
}

func TestObserveRTT(t *testing.T) {
	c := newConn(&fakeAPI{}, "", "", nil)
	c.observeRTT(100 * time.Millisecond)
	if c.rto != 110*time.Millisecond {
		t.Errorf("rto after a 100ms round trip = %s, want 110ms", c.rto)
	}
	for range 50 {
		c.observeRTT(5 * time.Second)
	}
	if c.rto != maxRTO {
		t.Errorf("rto after slow round trips = %s, want %s", c.rto, maxRTO)
	}
}

func TestVersionAfter(t *testing.T) {
	tests := []struct {
		v, than string
		want    bool
	}{
		{"3.3.1142.0", terminateFlagAgentVersion, true},
		{"2.3.722.0", terminateFlagAgentVersion, false},
		{"2.3.68.0", terminateFlagAgentVersion, false},
		{"2.3.722.1", terminateFlagAgentVersion, true},
		{"", terminateFlagAgentVersion, false},
	}
	for _, tt := range tests {
		if got := versionAfter(tt.v, tt.than); got != tt.want {
			t.Errorf("versionAfter(%q, %q) = %v, want %v", tt.v, tt.than, got, tt.want)
		}
	}
}
