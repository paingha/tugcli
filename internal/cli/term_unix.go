//go:build !windows

package cli

import (
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/term"
	"k8s.io/client-go/tools/remotecommand"
)

// watchTerminalSize sends the terminal's size now and after every SIGWINCH,
// until stop is called.
func watchTerminalSize(f *os.File) (remotecommand.TerminalSizeQueue, func()) {
	q := &sizeQueue{ch: make(chan remotecommand.TerminalSize, 1)}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGWINCH)
	done := make(chan struct{})
	send := func() {
		if w, h, err := term.GetSize(int(f.Fd())); err == nil {
			select {
			case q.ch <- remotecommand.TerminalSize{Width: uint16(w), Height: uint16(h)}:
			default: // a resize is already pending; the next one wins
			}
		}
	}
	send()
	go func() {
		defer close(q.ch)
		for {
			select {
			case <-sig:
				send()
			case <-done:
				return
			}
		}
	}()
	return q, func() {
		signal.Stop(sig)
		close(done)
	}
}
