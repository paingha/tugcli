package iap

import (
	"sync"
	"time"
)

// deadline is a net.Conn read or write deadline: a time that blocked calls
// wait on through a channel that is closed once the time has passed.
type deadline struct {
	mu      sync.Mutex
	gen     uint64 // counts sets, so a timer that fires late does nothing
	timer   *time.Timer
	expired chan struct{}
}

func newDeadline() *deadline { return &deadline{expired: make(chan struct{})} }

// set moves the deadline to t. The zero time means no deadline.
func (d *deadline) set(t time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.gen++
	if d.timer != nil {
		d.timer.Stop()
		d.timer = nil
	}
	if isDone(d.expired) {
		d.expired = make(chan struct{})
	}
	if t.IsZero() {
		return
	}
	wait := time.Until(t)
	if wait <= 0 {
		close(d.expired)
		return
	}
	gen, ch := d.gen, d.expired
	d.timer = time.AfterFunc(wait, func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.gen == gen {
			close(ch)
		}
	})
}

// done returns a channel that is closed when the deadline passes.
func (d *deadline) done() <-chan struct{} {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.expired
}

// passed reports whether the deadline has passed.
func (d *deadline) passed() bool { return isDone(d.done()) }

// isDone reports whether ch is closed.
func isDone(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// notify wakes whoever waits on ch, a channel with room for one signal,
// without blocking. Signals that arrive while one is pending merge.
func notify(ch chan<- struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}
