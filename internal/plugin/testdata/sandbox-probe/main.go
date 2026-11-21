// Command probe is a provider plugin for tug's sandbox tests. Its list
// operation tries things the sandbox allows and things it refuses, and
// reports how each went as a label of the one cluster it lists: "ok",
// "denied", or "failed: " and the error. The account settings name what to
// try.
package main

import (
	"context"
	"errors"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/paingha/tugcli/pkg/provider"
)

type probe struct{}

func (probe) Info() provider.Info {
	return provider.Info{Name: "probe", Version: "0.1.0", Description: "Tries what tug's sandbox allows and refuses"}
}

func (probe) List(_ context.Context, req provider.ListRequest) ([]provider.Cluster, error) {
	cfg := req.Account.ConfigString
	// mustRead fails the call the way a real plugin would when the sandbox
	// keeps it from a file it needs.
	if path := cfg("mustRead"); path != "" {
		if _, err := os.ReadFile(path); err != nil {
			return nil, provider.Errorf(provider.Internal, "reading %v", err)
		}
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, provider.Errorf(provider.Internal, "finding the plugin: %v", err)
	}
	labels := map[string]string{
		"readSecret":     outcome(read(cfg("secret"))),
		"readDeclared":   outcome(read(cfg("declared"))),
		"readOwn":        outcome(read(filepath.Join(filepath.Dir(exe), "..", "plugin.yaml"))),
		"writeTemp":      outcome(os.WriteFile(filepath.Join(os.TempDir(), "probe"), []byte("x"), 0o600)),
		"writeOutside":   outcome(os.WriteFile(filepath.Join(cfg("outside"), "probe"), []byte("x"), 0o600)),
		"connectAllowed": outcome(dial(cfg("allowed"))),
		"connectDenied":  outcome(dial(cfg("denied"))),
		"exec":           outcome(exec.Command("/bin/sh", "-c", "exit 0").Run()),
	}
	return []provider.Cluster{{ID: "probe", Name: "probe", Endpoints: provider.Endpoints{Public: "https://probe.invalid"}, Labels: labels}}, nil
}

func (probe) Credential(context.Context, provider.CredentialRequest) (provider.Credential, error) {
	return provider.Token("probe", provider.NoExpiry), nil
}

func read(path string) error {
	_, err := os.ReadFile(path)
	return err
}

func dial(addr string) error {
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return err
	}
	return c.Close()
}

func outcome(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, fs.ErrPermission):
		return "denied"
	default:
		return "failed: " + err.Error()
	}
}

func main() { provider.Main(probe{}) }
