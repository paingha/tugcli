// Package tailscale is tug's first-party provider plugin for Kubernetes
// clusters reached through the Tailscale Kubernetes operator's API server
// proxy. It finds the proxies among the peers that tailscale status --json
// shows, by the operator's tag, and connects to them as tailscale configure
// kubeconfig does: over https to the device's MagicDNS name, where the proxy,
// in its default auth mode, identifies the caller by tailnet identity.
package tailscale

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"

	"github.com/paingha/tugcli/pkg/provider"
)

// Name and version of the plugin.
const (
	Name    = "tailscale"
	Version = "0.1.0"
)

// ConfigTag is the account setting that names the tag marking the API server
// proxies.
const ConfigTag = "tag"

// DefaultTag is the tag the operator gives its own device, which runs the
// API server proxy: the default of the Helm chart's
// operatorConfig.defaultTags.
const DefaultTag = "tag:k8s-operator"

// InstallHint tells users where to get the tailscale command.
const InstallHint = "install Tailscale from https://tailscale.com/download and make sure the tailscale command is on PATH"

// unusedToken is the token tailscale configure kubeconfig gives the
// kubeconfig's user. The proxy ignores it and identifies the caller by
// tailnet identity; it is there so that kubectl does not ask for
// credentials.
const unusedToken = "unused"

// Provider implements the plugin.
type Provider struct{}

var _ provider.Provider = (*Provider)(nil)

// New returns the plugin.
func New() *Provider { return &Provider{} }

// Info implements provider.Provider.
func (p *Provider) Info() provider.Info {
	return provider.Info{
		Name:         Name,
		Version:      Version,
		Description:  "Kubernetes clusters behind the Tailscale operator's API server proxy",
		Capabilities: provider.CapList | provider.CapCredential,
	}
}

// tailscale status --json, trimmed to the fields the plugin uses. The CLI
// writes Go field names, without JSON tags.
type (
	status struct {
		BackendState string
		Peer         map[string]peer
	}
	peer struct {
		// ID is the device's stable node ID.
		ID string
		// DNSName is the device's MagicDNS name, ending in a dot.
		DNSName string
		Tags    []string
	}
)

// List returns one cluster for each peer that carries the account's tag.
func (p *Provider) List(ctx context.Context, req provider.ListRequest) ([]provider.Cluster, error) {
	st, err := tailscaleStatus(ctx)
	if err != nil {
		return nil, err
	}
	if err := running(st.BackendState); err != nil {
		return nil, err
	}
	tag := cmp.Or(req.Account.ConfigString(ConfigTag), DefaultTag)
	clusters := make([]provider.Cluster, 0, len(st.Peer))
	for _, ps := range st.Peer {
		if !slices.Contains(ps.Tags, tag) {
			continue
		}
		host := strings.TrimSuffix(ps.DNSName, ".")
		if ps.ID == "" || host == "" {
			fmt.Fprintf(os.Stderr, "tailscale: skipping a device tagged %s that has no node ID or MagicDNS name\n", tag)
			continue
		}
		// The first label is unique in the tailnet, and is what tailscale
		// configure kubeconfig accepts as a short name.
		name, _, _ := strings.Cut(host, ".")
		cred := provider.Token(unusedToken, provider.NoExpiry)
		clusters = append(clusters, provider.Cluster{
			ID:   ps.ID,
			Name: name,
			// The proxy is reachable only on the tailnet. Its certificate
			// comes from Let's Encrypt, so no CA is needed.
			Endpoints:  provider.Endpoints{Private: "https://" + host},
			Credential: &cred,
		})
	}
	slices.SortFunc(clusters, func(a, b provider.Cluster) int { return strings.Compare(a.Name, b.Name) })
	return clusters, nil
}

// Credential returns the token tailscale configure kubeconfig writes. It is
// the same for every cluster, so there is nothing to look up.
func (p *Provider) Credential(context.Context, provider.CredentialRequest) (provider.Credential, error) {
	return provider.Token(unusedToken, provider.NoExpiry), nil
}

// running checks that this device is connected to its tailnet, as tailscale
// configure kubeconfig does before it writes anything.
func running(state string) error {
	switch state {
	case "Running":
		return nil
	case "NeedsLogin":
		return provider.Errorf(provider.Unauthenticated, "Tailscale is not signed in").WithHint("run `tailscale login`")
	case "NeedsMachineAuth":
		return provider.Errorf(provider.Unauthenticated, "this device is waiting for approval by an admin of the tailnet").
			WithHint("ask an admin to approve the device on the Machines page of the Tailscale admin console")
	case "Stopped":
		return provider.Errorf(provider.Unavailable, "Tailscale is stopped").WithHint("run `tailscale up`")
	default:
		return provider.Errorf(provider.Unavailable, "Tailscale is not running (state %s)", cmp.Or(state, "unknown")).
			WithHint("run `tailscale up`")
	}
}

// tailscaleStatus runs tailscale status --json. The command prints JSON in
// every state, and fails only when it cannot reach tailscaled.
func tailscaleStatus(ctx context.Context) (status, error) {
	cmd := exec.CommandContext(ctx, "tailscale", "status", "--json")
	var stderr bytes.Buffer
	// The command's warnings go to the plugin's stderr too, which tug logs.
	cmd.Stderr = io.MultiWriter(&stderr, os.Stderr)
	out, err := cmd.Output()
	switch {
	case errors.Is(err, exec.ErrNotFound):
		return status{}, provider.Errorf(provider.Unavailable, "tailscale is not installed or not on PATH").WithHint(InstallHint)
	case err != nil && ctx.Err() != nil:
		return status{}, fmt.Errorf("running tailscale status: %w", ctx.Err())
	case err != nil:
		return status{}, provider.Errorf(provider.Unavailable, "tailscale status: %s", toolMessage(stderr.Bytes(), err)).
			WithHint("start Tailscale, then run `tailscale up`")
	}
	var st status
	if err := json.Unmarshal(out, &st); err != nil {
		return status{}, provider.Errorf(provider.Internal, "reading tailscale status: %v", err)
	}
	return st, nil
}

// toolMessage returns the last line a command printed on stderr, which is
// where tailscale says why it failed, or else how it exited.
func toolMessage(stderr []byte, err error) string {
	var last string
	for line := range strings.Lines(string(stderr)) {
		if line = strings.TrimSpace(line); line != "" {
			last = line
		}
	}
	return cmp.Or(last, err.Error())
}
