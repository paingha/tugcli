package cli

import "k8s.io/client-go/tools/remotecommand"

// sizeQueue hands terminal sizes to the exec stream.
type sizeQueue struct {
	ch chan remotecommand.TerminalSize
}

var _ remotecommand.TerminalSizeQueue = (*sizeQueue)(nil)

// Next blocks until the terminal is resized; nil means stop.
func (q *sizeQueue) Next() *remotecommand.TerminalSize {
	size, ok := <-q.ch
	if !ok {
		return nil
	}
	return &size
}
