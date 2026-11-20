package iap

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gorilla/websocket"

	"github.com/paingha/tugcli/pkg/provider"
)

// errLost means the relay needs bytes the tunnel no longer has, because it
// stopped acknowledging them for longer than maxUnconfirmed allows.
var errLost = errors.New("cannot resume the IAP session without bytes it has dropped")

// resume reconnects the session after its WebSocket dropped, as gcloud
// does: it opens the relay's reconnect endpoint with the session ID and the
// count of bytes received, which the relay resends after, and resends what
// the relay says it did not receive. It retries with back-off until the
// reconnect timeout, ctx ends, or an attempt fails in a way that will not
// pass.
func (c *conn) resume(ctx context.Context) (*websocket.Conn, error) {
	giveUp := time.Now().Add(c.cl.reconnectTimeout)
	wait := c.cl.reconnectWait
	for attempt := 1; ; attempt++ {
		ws, err := c.reopen(ctx)
		if err == nil {
			c.cl.logger.Debug("resumed the IAP tunnel", "target", c.t.String(), "attempts", attempt)
			return ws, nil
		}
		if ctx.Err() != nil {
			return nil, fmt.Errorf("resuming the IAP tunnel to %s: %w", c.t, ctx.Err())
		}
		if !retryable(err) {
			return nil, fmt.Errorf("resuming the IAP tunnel to %s: %w", c.t, err)
		}
		if time.Now().Add(wait).After(giveUp) {
			return nil, fmt.Errorf("resuming the IAP tunnel to %s: gave up after %d attempts: %w", c.t, attempt, err)
		}
		c.cl.logger.Debug("resuming the IAP tunnel again", "target", c.t.String(), "after", wait, "error", err)
		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return nil, fmt.Errorf("resuming the IAP tunnel to %s: %w", c.t, ctx.Err())
		}
		wait = min(wait*11/10, c.cl.maxReconnectWait)
	}
}

// reopen makes one attempt to resume the session.
func (c *conn) reopen(ctx context.Context) (*websocket.Conn, error) {
	c.mu.Lock()
	q := c.t.reconnectQuery(c.sid, c.received)
	c.mu.Unlock()
	ws, f, err := c.cl.open(ctx, c.cl.endpoint("reconnect", q), c.t, tagReconnectSuccessAck)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.resumeAt(f.count); err != nil {
		_ = ws.Close()
		return nil, err
	}
	c.local = ws.LocalAddr()
	return ws, nil
}

// resumeAt continues the stream from the n bytes the relay says it has
// received. The caller holds mu.
func (c *conn) resumeAt(n uint64) error {
	switch {
	case n < c.confirmed:
		return fmt.Errorf("%w: it resumed at %d bytes after acknowledging %d", errProtocol, n, c.confirmed)
	case n > c.wrote:
		return fmt.Errorf("%w: it resumed at %d bytes when %d were sent", errProtocol, n, c.wrote)
	case n < c.base:
		return fmt.Errorf("%w: the relay has %d bytes, and the tunnel kept only those from %d for resending", errLost, n, c.base)
	}
	c.confirmed = n
	c.trim(n)
	c.wrote = n
	// The reconnect URL told the relay how much had arrived.
	c.acked = c.received
	return nil
}

// retryable reports whether another attempt to resume may succeed after
// err: a new access token for close code 4004, or a network fault, or a
// relay that is unavailable for now. Refusals and protocol faults are
// final.
func retryable(err error) bool {
	var ce *closeError
	var pe *provider.Error
	switch {
	case errors.Is(err, errProtocol), errors.Is(err, errLost):
		return false
	case errors.As(err, &ce):
		return ce.code == closeReauthRequired
	case errors.As(err, &pe):
		return pe.Code == provider.Unavailable || pe.Code == provider.RateLimited
	default:
		return true
	}
}
