package ssm

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/smithy-go"
)

// dialResumable dials a fake agent with a short resume backoff.
func dialResumable(t *testing.T, agent *fakeAgent, api *fakeAPI, opts []Option) net.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := Dial(ctx, Config{Target: testTarget}, "10.0.0.1:443", append(opts, withResumeDelay(time.Millisecond))...)
	if err != nil {
		agent.check(t)
		t.Fatalf("Dial() error = %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestResumeAfterTheWebSocketDrops(t *testing.T) {
	agent := &fakeAgent{dropOnData: 3}
	api, opts := newFake(t, agent)
	// The first try to resume fails as a network error would; the second
	// works.
	api.resumeErrs = []error{errors.New("dial tcp: connection refused")}
	c := dialResumable(t, agent, api, opts)

	data := make([]byte, 10*chunkSize+7)
	_, _ = rand.Read(data)
	if _, err := c.Write(data); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	echo := make([]byte, len(data))
	if err := c.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(c, echo); err != nil {
		t.Fatalf("reading the echo across the drop: %v", err)
	}
	if !bytes.Equal(echo, data) {
		t.Fatal("the echo across the drop differs from the data written")
	}

	if n := api.resumeCount(); n != 2 {
		t.Errorf("ResumeSession calls = %d, want 2", n)
	}
	s := agent.session(t, 0)
	s.mu.Lock()
	hellos, urls := slices.Clone(s.hellos), slices.Clone(s.urls)
	var dataSeqs []int64
	for _, m := range s.inputs {
		if m.PayloadType == payloadOutput {
			dataSeqs = append(dataSeqs, m.Sequence)
		}
	}
	s.mu.Unlock()
	if len(hellos) != 2 {
		t.Fatalf("open data channel requests = %d, want 2", len(hellos))
	}
	if hellos[1].TokenValue == hellos[0].TokenValue || hellos[1].ClientID != hellos[0].ClientID {
		t.Errorf("resumed open data channel request = %+v, want a new token and the same client ID as %+v", hellos[1], hellos[0])
	}
	if !strings.Contains(urls[1], "resumed=1") {
		t.Errorf("resumed connection went to %q, not the stream URL ResumeSession returned", urls[1])
	}
	// The third data message was processed but its acknowledgement lost, so
	// the client sent it again after resuming.
	lost := dataSeqs[2]
	if n := countOf(dataSeqs, lost); n < 2 {
		t.Errorf("data message %d was sent %d times, want it resent after the resume (sequence numbers %v)", lost, n, dataSeqs)
	}

	if err := c.Close(); err != nil {
		t.Errorf("Close() error = %v", err)
	}
	if got := api.terminations(); !slices.Equal(got, []string{s.id}) {
		t.Errorf("TerminateSession calls = %v, want [%s]", got, s.id)
	}
	agent.check(t)
}

func countOf(seqs []int64, seq int64) int {
	n := 0
	for _, s := range seqs {
		if s == seq {
			n++
		}
	}
	return n
}

func TestResumeGivesUp(t *testing.T) {
	refused := errors.New("dial tcp: connection refused")
	tests := []struct {
		name        string
		errs        []error
		wantResumes int
		wantErr     error
		wantMsg     string
	}{
		{
			name:        "the session is gone",
			errs:        []error{&smithy.GenericAPIError{Code: "DoesNotExistException", Message: "no such session"}},
			wantResumes: 1,
			wantMsg:     "DoesNotExistException",
		},
		{
			name:        "the network stays down",
			errs:        slices.Repeat([]error{refused}, resumeRetries+5),
			wantResumes: resumeRetries + 1,
			wantErr:     refused,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agent := &fakeAgent{dropOnData: 1}
			api, opts := newFake(t, agent)
			api.resumeErrs = tt.errs
			c := dialResumable(t, agent, api, opts)
			if _, err := c.Write([]byte("x")); err != nil {
				t.Fatalf("Write() error = %v", err)
			}
			if err := c.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
				t.Fatal(err)
			}
			_, err := c.Read(make([]byte, 1))
			if err == nil || !strings.Contains(err.Error(), "resuming SSM session") {
				t.Fatalf("Read() error = %v, want a failure to resume", err)
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Errorf("Read() error = %v, want %v", err, tt.wantErr)
			}
			if tt.wantMsg != "" && !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("Read() error = %v, want it to mention %s", err, tt.wantMsg)
			}
			if n := api.resumeCount(); n != tt.wantResumes {
				t.Errorf("ResumeSession calls = %d, want %d", n, tt.wantResumes)
			}
			agent.check(t)
		})
	}
}

func TestResumeWithoutAToken(t *testing.T) {
	agent := &fakeAgent{dropOnData: 1}
	api, opts := newFake(t, agent)
	api.noResumeToken = true
	c := dialResumable(t, agent, api, opts)
	if _, err := c.Write([]byte("x")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if err := c.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Read(make([]byte, 1)); !errors.Is(err, ErrSessionClosed) {
		t.Errorf("Read() error = %v, want ErrSessionClosed", err)
	}
	if n := api.resumeCount(); n != 1 {
		t.Errorf("ResumeSession calls = %d, want 1", n)
	}
	agent.check(t)
}
