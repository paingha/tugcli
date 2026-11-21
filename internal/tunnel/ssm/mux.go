package ssm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/xtaci/smux"
)

// Multiplexed port forwarding, as the session-manager-plugin does it from
// 1.2.7.0. The agent multiplexes a LocalPortForwarding session with smux
// (protocol version 1) when the client reports version 1.1.70 or later,
// and the plugin does so when the agent is later than 3.0.196.0, so tug
// reports the multiplexing version only to such agents. Each smux stream is
// one TCP connection from the instance; the agent dials the destination
// when a stream opens and closes the stream when the destination closes.
//
// smux keepalives count as session activity, which defeats Session
// Manager's idle timeout, so the plugin turns them off with agents later
// than 3.1.1511.0, and those agents turn theirs off for clients later than
// 1.2.331.0. tug reports 1.2.339.0, the plugin release that made that
// change, and turns its keepalives off with the same agents.
const (
	basicClientVersion    = "1.1.61.0"
	muxClientVersion      = "1.2.339.0"
	muxAgentVersion       = "3.0.196.0"
	keepAliveAgentVersion = "3.1.1511.0"

	// muxStartTimeout bounds starting a shared session, which no single
	// caller's context does.
	muxStartTimeout = time.Minute
)

// localPortForwarding is the port session type the agent multiplexes.
const localPortForwarding = "LocalPortForwarding"

// muxSession is a multiplexed session shared by the Dialer's connections
// to one address.
type muxSession struct {
	ready chan struct{} // closed once the session is up or has failed

	// Set before ready is closed.
	ch   *conn
	sess *smux.Session
	err  error
	// basic is the session the agent gave instead when it was too old to
	// multiplex, for the first caller to take.
	basic *conn
}

// alive reports whether a started session can open more streams.
func (m *muxSession) alive() bool {
	select {
	case <-m.ready:
	default:
		return true // still starting
	}
	return m.err == nil && m.sess != nil && !m.sess.IsClosed() && !m.ch.isBroken()
}

func (m *muxSession) close() error {
	switch {
	case m.sess != nil:
		return m.sess.Close()
	case m.basic != nil:
		return m.basic.Close()
	}
	return nil
}

// muxConn is one connection over a multiplexed session.
type muxConn struct {
	*smux.Stream
	sessionID, address string
}

// LocalAddr returns the shared session's ID.
func (c *muxConn) LocalAddr() net.Addr { return addr(c.sessionID) }

// RemoteAddr returns the address the instance forwards to.
func (c *muxConn) RemoteAddr() net.Addr { return addr(c.address) }

// dialMux opens a stream on the shared session to address, starting the
// session if there is none, and falls back to a session of its own when
// the agent is too old to multiplex.
func (d *Dialer) dialMux(ctx context.Context, cl *clients, address, host, port string) (net.Conn, error) {
	for attempt := 0; ; attempt++ {
		m, basic, err := d.muxFor(ctx, cl, address, host, port)
		if err != nil {
			return nil, err
		}
		if basic != nil {
			return basic, nil
		}
		if m == nil {
			c, err := d.startSession(ctx, cl, address, host, port, false)
			if err != nil {
				return nil, err
			}
			return c, nil
		}
		stream, err := m.sess.OpenStream()
		if err == nil {
			return &muxConn{Stream: stream, sessionID: m.ch.sessionID, address: address}, nil
		}
		// The session died between the check and the open; start one
		// more before giving up.
		d.forget(address, m)
		if attempt > 0 {
			return nil, fmt.Errorf("opening a stream on SSM session %s: %w", m.ch.sessionID, err)
		}
	}
}

// muxFor returns the shared session for address, waiting for it to start.
// It returns a plain session instead when the agent turned out too old to
// multiplex, or neither when one has been seen to be.
func (d *Dialer) muxFor(ctx context.Context, cl *clients, address, host, port string) (*muxSession, net.Conn, error) {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil, nil, net.ErrClosed
	}
	if d.noMux[address] {
		d.mu.Unlock()
		return nil, nil, nil
	}
	m := d.muxes[address]
	if m != nil && !m.alive() {
		delete(d.muxes, address)
		dead := m
		// Ending a dead session calls TerminateSession; do it in the
		// background rather than delay this Dial.
		d.watchers.Go(func() { _ = dead.close() })
		m = nil
	}
	if m == nil {
		m = &muxSession{ready: make(chan struct{})}
		d.muxes[address] = m
		// The session outlives the caller that starts it, so it starts
		// under the Dialer, not under ctx.
		d.starts.Go(func() { d.startMux(context.WithoutCancel(ctx), cl, address, host, port, m) })
	}
	d.mu.Unlock()

	select {
	case <-m.ready:
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
	if m.err != nil {
		d.forget(address, m)
		return nil, nil, m.err
	}
	if m.sess != nil {
		return m, nil, nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.noMux[address] = true
	if d.muxes[address] == m {
		delete(d.muxes, address)
	}
	basic := m.basic
	m.basic = nil
	if basic == nil {
		return nil, nil, nil
	}
	return nil, basic, nil
}

// startMux starts the shared session for address.
func (d *Dialer) startMux(ctx context.Context, cl *clients, address, host, port string, m *muxSession) {
	defer close(m.ready)
	ctx, cancel := untilClosed(ctx, d.done)
	defer cancel()
	ctx, cancelTimeout := context.WithTimeout(ctx, muxStartTimeout)
	defer cancelTimeout()
	c, err := d.startSession(ctx, cl, address, host, port, true)
	if err != nil {
		m.err = err
		return
	}
	if !c.multiplexed() {
		c.log.Info("the instance's SSM agent is too old to multiplex; using a session per connection", "agent", c.agentVersionString())
		m.basic = c
		return
	}
	config := smux.DefaultConfig()
	config.KeepAliveDisabled = versionAfter(c.agentVersionString(), keepAliveAgentVersion)
	sess, err := smux.Client(c, config)
	if err != nil {
		_ = c.Close()
		m.err = fmt.Errorf("starting smux on SSM session %s: %w", c.sessionID, err)
		return
	}
	m.ch, m.sess = c, sess
	c.log.Debug("multiplexing connections over the SSM session", "keepalive", !config.KeepAliveDisabled)
	// When the channel ends, end smux too, which stops its goroutines and
	// fails its streams; closing smux closes the channel.
	d.watchers.Go(func() {
		select {
		case <-c.broken:
			_ = sess.Close()
		case <-sess.CloseChan():
		}
	})
}

// forget drops a shared session that failed, so the next Dial starts
// another.
func (d *Dialer) forget(address string, m *muxSession) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.muxes[address] == m {
		delete(d.muxes, address)
	}
}

// closeMuxes ends every shared session once their starts have finished,
// and waits for the watchers.
func (d *Dialer) closeMuxes(ms []*muxSession) error {
	d.starts.Wait()
	var errs []error
	for _, m := range ms {
		if err := m.close(); err != nil && !errors.Is(err, io.ErrClosedPipe) {
			errs = append(errs, err)
		}
	}
	d.watchers.Wait()
	return errors.Join(errs...)
}
