//go:build unix

package provider

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// spinner uses processor time until it is stopped, as a plugin stuck in a
// loop would.
type spinner struct{ fake }

func (spinner) List(ctx context.Context, _ ListRequest) ([]Cluster, error) {
	fmt.Fprintln(os.Stderr, "spinning")
	for ctx.Err() == nil {
	}
	return nil, ctx.Err()
}

func TestMainStopsAtTheCPULimit(t *testing.T) {
	if os.Getenv("TUG_TEST_SPINNER") == "1" {
		os.Args = []string{"tug-provider-spinner", OpList}
		os.Exit(Main(spinner{}))
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestMainStopsAtTheCPULimit$")
	cmd.Env = append(os.Environ(), "TUG_TEST_SPINNER=1")
	cmd.Stdin = strings.NewReader(`{"kind":"ListRequest","account":{"id":"a"}}`)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(stderr)
	if !sc.Scan() || sc.Text() != "spinning" {
		_ = cmd.Process.Kill()
		t.Fatalf("the plugin did not start spinning: %q", sc.Text())
	}
	if err := cmd.Process.Signal(syscall.SIGXCPU); err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() {
		for sc.Scan() {
			if strings.Contains(sc.Text(), "CPU time limit") {
				break
			}
		}
		waited <- cmd.Wait()
	}()
	select {
	case err := <-waited:
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != ExitError {
			t.Fatalf("plugin ended with %v, want exit status %d", err, ExitError)
		}
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("the plugin kept running after SIGXCPU")
	}
}
