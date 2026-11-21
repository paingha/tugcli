package ssm

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"sync"
	"testing"
	"time"
)

// echo writes data to c and checks that it comes back.
func echo(c net.Conn, data []byte) error {
	if err := c.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return err
	}
	if _, err := c.Write(data); err != nil {
		return fmt.Errorf("writing: %w", err)
	}
	got := make([]byte, len(data))
	if _, err := io.ReadFull(c, got); err != nil {
		return fmt.Errorf("reading the echo: %w", err)
	}
	if !bytes.Equal(got, data) {
		return errors.New("the echo differs from the data written")
	}
	return nil
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

func newMuxDialer(t *testing.T, agent *fakeAgent) (*Dialer, *fakeAPI) {
	t.Helper()
	api, opts := newFake(t, agent)
	d, err := NewDialer(Config{Target: testTarget, Mux: true}, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d, api
}

func TestMuxSharesOneSession(t *testing.T) {
	agent := &fakeAgent{}
	d, api := newMuxDialer(t, agent)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Connections dialled at once share one session.
	const n = 6
	conns := make([]net.Conn, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			c, err := d.Dial(ctx, "10.0.0.1:443")
			if err != nil {
				errs[i] = err
				return
			}
			conns[i] = c
			errs[i] = echo(c, randomBytes(3*chunkSize+i))
		})
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("connection %d: %v", i, err)
		}
	}
	if got := api.startCount(); got != 1 {
		t.Errorf("StartSession calls = %d, want 1 for %d connections", got, n)
	}
	s := agent.session(t, 0)
	if v := s.hello().ClientVersion; v != muxClientVersion {
		t.Errorf("open data channel ClientVersion = %s, want %s", v, muxClientVersion)
	}
	if v := s.handshakeResponse().ClientVersion; v != muxClientVersion {
		t.Errorf("handshake ClientVersion = %s, want %s", v, muxClientVersion)
	}
	if got := s.streamCount(); got != n {
		t.Errorf("streams = %d, want %d", got, n)
	}
	if got := conns[0].LocalAddr().String(); got != s.id {
		t.Errorf("LocalAddr() = %s, want the session %s", got, s.id)
	}
	if got := conns[0].RemoteAddr().String(); got != "10.0.0.1:443" {
		t.Errorf("RemoteAddr() = %s, want 10.0.0.1:443", got)
	}

	// Closing one connection leaves the others working.
	if err := conns[0].Close(); err != nil {
		t.Errorf("Close() error = %v", err)
	}
	if err := echo(conns[1], []byte("still here")); err != nil {
		t.Errorf("after closing another connection: %v", err)
	}
	// The far end hanging up reaches the reader, which basic mode cannot
	// report.
	if _, err := conns[2].Write([]byte("bye")); err != nil {
		t.Fatal(err)
	}
	if _, err := conns[2].Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Errorf("Read() after the far end closed error = %v, want io.EOF", err)
	}
	if len(api.terminations()) != 0 {
		t.Errorf("TerminateSession calls = %v while connections are open", api.terminations())
	}

	// Closing the Dialer ends the session and its connections.
	if err := d.Close(); err != nil {
		t.Errorf("Dialer.Close() error = %v", err)
	}
	s.waitDone(t)
	if got := api.terminations(); !slices.Equal(got, []string{s.id}) {
		t.Errorf("TerminateSession calls = %v, want [%s]", got, s.id)
	}
	if _, err := conns[3].Read(make([]byte, 1)); err == nil {
		t.Error("Read() after Dialer.Close error = nil")
	}
	if _, err := d.Dial(ctx, "10.0.0.1:443"); !errors.Is(err, net.ErrClosed) {
		t.Errorf("Dial() after Close error = %v, want net.ErrClosed", err)
	}
	agent.check(t)
}

func TestMuxFallsBackForOldAgents(t *testing.T) {
	agent := &fakeAgent{agentVersion: "3.0.100.0"}
	d, api := newMuxDialer(t, agent)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for i := range 2 {
		c, err := d.Dial(ctx, "10.0.0.1:443")
		if err != nil {
			t.Fatalf("Dial() %d error = %v", i, err)
		}
		defer c.Close()
		if err := echo(c, randomBytes(chunkSize+i)); err != nil {
			t.Fatalf("connection %d: %v", i, err)
		}
		s := agent.session(t, i)
		if v := s.handshakeResponse().ClientVersion; v != basicClientVersion {
			t.Errorf("session %d handshake ClientVersion = %s, want %s", i, v, basicClientVersion)
		}
	}
	if got := api.startCount(); got != 2 {
		t.Errorf("StartSession calls = %d, want a session per connection", got)
	}
	agent.check(t)
}

func TestMuxStartsAgainWhenTheSessionEnds(t *testing.T) {
	agent := &fakeAgent{}
	d, api := newMuxDialer(t, agent)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	first, err := d.Dial(ctx, "10.0.0.1:443")
	if err != nil {
		t.Fatal(err)
	}
	if err := echo(first, []byte("one")); err != nil {
		t.Fatal(err)
	}

	agent.session(t, 0).closeChannel()
	if err := first.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var timeout net.Error
	if _, err := first.Read(make([]byte, 1)); err == nil || errors.As(err, &timeout) && timeout.Timeout() {
		t.Fatalf("Read() after Session Manager closed the session error = %v, want the end of the stream", err)
	}

	second, err := d.Dial(ctx, "10.0.0.1:443")
	if err != nil {
		t.Fatalf("Dial() after the session ended error = %v", err)
	}
	if err := echo(second, []byte("two")); err != nil {
		t.Fatal(err)
	}
	if got := api.startCount(); got != 2 {
		t.Errorf("StartSession calls = %d, want a new session after the first ended", got)
	}
	agent.check(t)
}

func TestDialWithMuxOwnsItsSession(t *testing.T) {
	agent := &fakeAgent{}
	api, opts := newFake(t, agent)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := Dial(ctx, Config{Target: testTarget, Mux: true}, "10.0.0.1:443", opts...)
	if err != nil {
		t.Fatal(err)
	}
	if err := echo(c, []byte("alone")); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Errorf("Close() error = %v", err)
	}
	s := agent.session(t, 0)
	s.waitDone(t)
	if got := api.terminations(); !slices.Equal(got, []string{s.id}) {
		t.Errorf("TerminateSession calls = %v, want [%s]", got, s.id)
	}
	agent.check(t)
}
