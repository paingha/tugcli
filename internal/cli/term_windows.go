//go:build windows

package cli

import (
	"os"
	"time"

	"golang.org/x/term"
	"k8s.io/client-go/tools/remotecommand"
)

// watchTerminalSize polls the console size, since Windows has no SIGWINCH.
func watchTerminalSize(f *os.File) (remotecommand.TerminalSizeQueue, func()) {
	q := &sizeQueue{ch: make(chan remotecommand.TerminalSize, 1)}
	done := make(chan struct{})
	go func() {
		defer close(q.ch)
		var last remotecommand.TerminalSize
		tick := time.NewTicker(250 * time.Millisecond)
		defer tick.Stop()
		for {
			if w, h, err := term.GetSize(int(f.Fd())); err == nil {
				size := remotecommand.TerminalSize{Width: uint16(w), Height: uint16(h)}
				if size != last {
					last = size
					select {
					case q.ch <- size:
					default:
					}
				}
			}
			select {
			case <-tick.C:
			case <-done:
				return
			}
		}
	}()
	return q, func() { close(done) }
}
