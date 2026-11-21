// Command tug-provider-probe is a test plugin for tug's WebAssembly sandbox.
// Built with GOOS=wasip1 GOARCH=wasm, its list operation does what the
// account's settings ask, from inside the sandbox, and reports the outcome
// in the labels of a single cluster:
//
//   - url: GET it with provider.HTTPClient; post: POST this body instead
//   - spin: loop for ever
//   - sleep: sleep for this long, such as 1h
//   - allocMiB: allocate and touch this many MiB
//   - files: write a file to $TMPDIR, list it, "/" and its parent, and try
//     to read this path
//   - env: report the environment
package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/paingha/tugcli/pkg/provider"
)

type probe struct{}

func (probe) Info() provider.Info {
	return provider.Info{Name: "probe", Version: "0.1.0", Description: "tug's WebAssembly sandbox test"}
}

var sink []byte

func (probe) List(ctx context.Context, req provider.ListRequest) ([]provider.Cluster, error) {
	labels := map[string]string{}
	cfg := req.Account
	switch {
	case cfg.ConfigString("url") != "":
		status, body, err := fetch(ctx, cfg.ConfigString("url"), cfg.ConfigString("post"))
		if err != nil {
			return nil, provider.Errorf(provider.Unavailable, "%v", err)
		}
		labels["status"], labels["body"] = strconv.Itoa(status), body
	case cfg.ConfigString("spin") != "":
		for {
		}
	case cfg.ConfigString("sleep") != "":
		d, err := time.ParseDuration(cfg.ConfigString("sleep"))
		if err != nil {
			return nil, provider.Errorf(provider.InvalidConfig, "%v", err)
		}
		time.Sleep(d)
	case cfg.ConfigString("allocMiB") != "":
		n, _ := strconv.Atoi(cfg.ConfigString("allocMiB"))
		sink = make([]byte, n<<20)
		for i := 0; i < len(sink); i += 4096 {
			sink[i] = 1
		}
		labels["allocated"] = strconv.Itoa(n)
	case cfg.ConfigString("files") != "":
		path := os.TempDir() + "/probe.txt"
		if err := os.WriteFile(path, []byte("ok"), 0o600); err != nil {
			return nil, provider.Errorf(provider.Internal, "writing %s: %v", path, err)
		}
		labels["tmp"] = path
		labels["listTmp"] = list(os.TempDir())
		labels["root"] = list("/")
		labels["parent"] = list(os.TempDir() + "/..")
		if b, err := os.ReadFile(cfg.ConfigString("files")); err != nil {
			labels["read"] = "error: " + err.Error()
		} else {
			labels["read"] = string(b)
		}
	case cfg.ConfigString("env") != "":
		labels["env"] = strings.Join(os.Environ(), " ")
		labels["args"] = strings.Join(os.Args, " ")
	}
	return []provider.Cluster{{ID: "probe", Name: "probe", Endpoints: provider.Endpoints{Public: "https://probe.example:6443"}, Labels: labels}}, nil
}

func fetch(ctx context.Context, url, post string) (int, string, error) {
	method, body := http.MethodGet, io.Reader(nil)
	if post != "" {
		method, body = http.MethodPost, strings.NewReader(post)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("X-Probe", "1")
	resp, err := provider.HTTPClient().Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, "", fmt.Errorf("reading the body: %w", err)
	}
	return resp.StatusCode, string(b), nil
}

func list(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "error: " + err.Error()
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	return strings.Join(names, ",")
}

func (probe) Credential(context.Context, provider.CredentialRequest) (provider.Credential, error) {
	return provider.Token("probe", provider.NoExpiry), nil
}

func main() { os.Exit(provider.Main(probe{})) }
