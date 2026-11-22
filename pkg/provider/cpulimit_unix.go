//go:build unix

package provider

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

// stopOnCPULimit makes the plugin exit when the system says it has used up
// the processor time tug allowed it. The kernel says so with SIGXCPU, which
// Go programs otherwise ignore, and macOS never follows it with SIGKILL as
// Linux does. Call the returned function to stop watching.
func stopOnCPULimit() (stop func()) {
	ch := make(chan os.Signal, 1)
	done := make(chan struct{})
	signal.Notify(ch, syscall.SIGXCPU)
	go func() {
		select {
		case <-ch:
			fmt.Fprintf(os.Stderr, "%s: used up the CPU time limit tug set for it\n", progName())
			os.Exit(ExitError)
		case <-done:
		}
	}()
	return func() {
		signal.Stop(ch)
		close(done)
	}
}
