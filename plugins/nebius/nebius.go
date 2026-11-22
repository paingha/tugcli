// Package nebius is tug's first-party provider plugin for Nebius AI Cloud
// Managed Service for Kubernetes (mk8s). It drives the nebius CLI rather
// than the API: clusters come from `nebius mk8s cluster list`, and the
// credential is a delegate that runs `nebius mk8s v1 cluster get-token`, as
// the kubeconfig written by `nebius mk8s cluster get-credentials` does. The
// CLI profile holds the user or service account credentials, so listing and
// kubectl use the same identity and the plugin declares no secrets.
package nebius

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/paingha/tugcli/pkg/provider"
)

// Name and version of the plugin.
const (
	Name    = "nebius"
	Version = "0.1.1"
)

// CLI is the name of the Nebius AI Cloud CLI executable.
const CLI = "nebius"

// Account settings.
const (
	// ConfigProject is the project (the mk8s parent ID) to list clusters
	// in. Without it, the CLI uses its profile's parent-id.
	ConfigProject = "project"
	// ConfigProfile is the CLI profile to use. Without it, the CLI uses
	// NEBIUS_PROFILE or the active profile.
	ConfigProfile = "profile"
)

// Exec settings that `nebius mk8s cluster get-credentials` writes into a
// kubeconfig, copied so that tug runs the token command the same way.
const (
	execAPIVersion      = "client.authentication.k8s.io/v1beta1"
	execInteractiveMode = "IfAvailable"
)

const installHint = "install the Nebius AI Cloud CLI (https://docs.nebius.com/cli/install), then set up a profile with `nebius profile create`"

// pageSize is how many items to ask for per CLI call, and parallel how many
// node group listings run at once.
const (
	pageSize = "100"
	parallel = 4
)

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
		Description:  "Nebius AI Cloud Managed Service for Kubernetes, through the nebius CLI",
		Capabilities: provider.CapList | provider.CapCredential,
	}
}

// CLI output, trimmed to the fields the plugin uses. The CLI prints the API's
// protobuf messages with their snake_case field names.
type (
	page[T any] struct {
		Items         []T    `json:"items"`
		NextPageToken string `json:"next_page_token"`
	}
	metadata struct {
		ID       string            `json:"id"`
		Name     string            `json:"name"`
		ParentID string            `json:"parent_id"`
		Labels   map[string]string `json:"labels"`
	}
	cluster struct {
		Metadata metadata `json:"metadata"`
		Spec     struct {
			ControlPlane struct {
				Version string `json:"version"`
			} `json:"control_plane"`
		} `json:"spec"`
		Status struct {
			State        string `json:"state"`
			ControlPlane struct {
				Version   string `json:"version"`
				Endpoints struct {
					PublicEndpoint  string `json:"public_endpoint"`
					PrivateEndpoint string `json:"private_endpoint"`
				} `json:"endpoints"`
				Auth struct {
					ClusterCACertificate string `json:"cluster_ca_certificate"`
				} `json:"auth"`
			} `json:"control_plane"`
		} `json:"status"`
	}
	nodeGroup struct {
		Metadata metadata `json:"metadata"`
		Spec     struct {
			Template struct {
				Resources struct {
					Platform string `json:"platform"`
				} `json:"resources"`
			} `json:"template"`
		} `json:"spec"`
	}
)

// List returns the project's mk8s clusters. Clusters without an endpoint yet
// (still being created) are skipped until the next sync.
func (p *Provider) List(ctx context.Context, req provider.ListRequest) ([]provider.Cluster, error) {
	acct := req.Account
	args := []string{"mk8s", "cluster", "list"}
	if project := acct.ConfigString(ConfigProject); project != "" {
		args = append(args, "--parent-id", project)
	}
	clusters, err := listAll[cluster](ctx, acct, args...)
	if err != nil {
		return nil, err
	}
	cred, err := credential(acct)
	if err != nil {
		return nil, err
	}
	ready := make([]cluster, 0, len(clusters))
	for _, cl := range clusters {
		ep := cl.Status.ControlPlane.Endpoints
		if ep.PublicEndpoint == "" && ep.PrivateEndpoint == "" {
			fmt.Fprintf(os.Stderr, "nebius: skipping %s: no API endpoint yet (state %s)\n", cl.Metadata.Name, cl.Status.State)
			continue
		}
		ready = append(ready, cl)
	}
	gpu, err := gpuClusters(ctx, acct, ready)
	if err != nil {
		return nil, err
	}
	out := make([]provider.Cluster, 0, len(ready))
	for i, cl := range ready {
		cp := cl.Status.ControlPlane
		pc := provider.Cluster{
			ID:      cl.Metadata.ID,
			Name:    cl.Metadata.Name,
			Version: kubernetesVersion(cp.Version, cl.Spec.ControlPlane.Version),
			Endpoints: provider.Endpoints{
				Public:  endpointURL(cp.Endpoints.PublicEndpoint),
				Private: endpointURL(cp.Endpoints.PrivateEndpoint),
			},
			Labels: map[string]string{},
		}
		if ca := cp.Auth.ClusterCACertificate; ca != "" {
			pc.CAData = []byte(ca)
		}
		for k, v := range cl.Metadata.Labels {
			pc.Labels[k] = v
		}
		if gpu[i] {
			pc.Labels["gpu"] = "true"
		}
		// The credential does not depend on the cluster. Returning it
		// inline saves a plugin call at the first kubectl command and lets
		// sync replace it when the profile setting changes.
		pc.Credential = &cred
		out = append(out, pc)
	}
	return out, nil
}

// gpuClusters reports, for each cluster, whether it has a node group on a
// GPU platform. It lists a few clusters' node groups at a time and stops at
// the first error, since a listing must be complete or fail.
func gpuClusters(ctx context.Context, acct provider.Account, clusters []cluster) ([]bool, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	gpu := make([]bool, len(clusters))
	errs := make([]error, len(clusters))
	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	for i, cl := range clusters {
		wg.Go(func() {
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				errs[i] = ctx.Err()
				return
			}
			defer func() { <-sem }()
			groups, err := listAll[nodeGroup](ctx, acct, "mk8s", "node-group", "list", "--parent-id", cl.Metadata.ID)
			if err != nil {
				errs[i] = err
				cancel()
				return
			}
			for _, ng := range groups {
				if strings.HasPrefix(ng.Spec.Template.Resources.Platform, "gpu-") {
					gpu[i] = true
				}
			}
		})
	}
	wg.Wait()
	// Report the error that stopped the others, not the cancellations it
	// caused.
	for _, err := range errs {
		if err != nil && !errors.Is(err, context.Canceled) {
			return nil, err
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return gpu, nil
}

// kubernetesVersion returns the Kubernetes part of a control plane version
// such as 1.30.0-nebius-cp.3, or the desired version when the actual one is
// not known yet.
func kubernetesVersion(actual, desired string) string {
	if actual == "" {
		return desired
	}
	v, _, _ := strings.Cut(actual, "-nebius")
	return v
}

// endpointURL turns a control plane endpoint, which is a DNS name or IP
// address, into the URL the Kubernetes API is served at.
func endpointURL(s string) string {
	if s == "" {
		return ""
	}
	if u, err := url.Parse(s); err == nil && u.Scheme != "" && u.Host != "" {
		return s
	}
	return "https://" + strings.TrimPrefix(s, "//")
}

// Credential returns a delegate that runs the CLI's token command. It does
// not call the CLI itself: when the profile needs to sign in again, kubectl
// runs the delegate with the terminal, where the CLI can do so.
func (p *Provider) Credential(_ context.Context, req provider.CredentialRequest) (provider.Credential, error) {
	return credential(req.Account)
}

// credential builds the exec entry that get-credentials writes: the CLI's
// path, `mk8s v1 cluster get-token`, the profile when one is set, and JSON
// output.
func credential(acct provider.Account) (provider.Credential, error) {
	bin, err := findCLI()
	if err != nil {
		return provider.Credential{}, err
	}
	args := []string{"mk8s", "v1", "cluster", "get-token"}
	if profile := acct.ConfigString(ConfigProfile); profile != "" {
		args = append(args, "--profile", profile)
	}
	args = append(args, "--format", "json")
	c := provider.Delegate(bin, args...)
	c.Exec.APIVersion = execAPIVersion
	c.Exec.InteractiveMode = execInteractiveMode
	c.Exec.InstallHint = installHint
	return c, nil
}

// findCLI returns the path of the nebius executable: on PATH, or where its
// installer puts it, for a tug started without the shell's PATH.
func findCLI() (string, error) {
	if path, err := exec.LookPath(CLI); err == nil {
		return path, nil
	}
	if home, err := os.UserHomeDir(); err == nil {
		path := filepath.Join(home, ".nebius", "bin", CLI)
		if fi, err := os.Stat(path); err == nil && !fi.IsDir() {
			return path, nil
		}
	}
	return "", provider.Errorf(provider.Unavailable, "the nebius CLI is not on PATH or in ~/.nebius/bin").WithHint(installHint)
}

// listAll runs a CLI list command page by page and returns every item.
func listAll[T any](ctx context.Context, acct provider.Account, args ...string) ([]T, error) {
	var out []T
	token := ""
	for {
		pageArgs := append(append([]string{}, args...), "--page-size", pageSize)
		if token != "" {
			pageArgs = append(pageArgs, "--page-token", token)
		}
		data, err := run(ctx, acct, pageArgs...)
		if err != nil {
			return nil, err
		}
		var pg page[T]
		if err := json.Unmarshal(data, &pg); err != nil {
			return nil, provider.Errorf(provider.Internal, "%s printed something that is not JSON: %v", command(args), err).
				WithHint("update the CLI with `nebius update`")
		}
		out = append(out, pg.Items...)
		if pg.NextPageToken == "" {
			return out, nil
		}
		if pg.NextPageToken == token {
			return nil, provider.Errorf(provider.Internal, "%s returned the same page token twice", command(args))
		}
		token = pg.NextPageToken
	}
}

// run runs the CLI with JSON output, the account's profile, and without
// opening a browser: a sync in the background must not start a sign-in. It
// returns stdout, or an error classified by the CLI's exit code.
func run(ctx context.Context, acct provider.Account, args ...string) ([]byte, error) {
	bin, err := findCLI()
	if err != nil {
		return nil, err
	}
	profile := acct.ConfigString(ConfigProfile)
	full := append(append([]string{}, args...), "--format", "json", "--no-browser")
	if profile != "" {
		full = append(full, "--profile", profile)
	}
	cmd := exec.CommandContext(ctx, bin, full...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	// A child the CLI starts may keep the pipes open after it is killed.
	cmd.WaitDelay = time.Second
	err = cmd.Run()
	if err == nil {
		return stdout.Bytes(), nil
	}
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return nil, provider.Errorf(provider.Unavailable, "%s did not finish in time", command(args)).
			WithHint(signInHint(profile) + "; the CLI cannot ask for a sign-in while tug runs it")
	case ctx.Err() != nil:
		return nil, ctx.Err()
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return nil, exitError(exitErr.ExitCode(), command(args), firstLine(stderr.String()), profile)
	}
	return nil, provider.Errorf(provider.Unavailable, "running %s: %v", command(args), err)
}

// command names a CLI command in messages, without its flags.
func command(args []string) string {
	words := []string{CLI}
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			break
		}
		words = append(words, a)
	}
	return strings.Join(words, " ")
}

// exitError classifies a CLI failure by its documented exit code
// (https://docs.nebius.com/cli/exit-codes). Codes above 50 are the same
// errors from the CLI's follow-up calls.
func exitError(code int, cmd, stderr, profile string) error {
	msg := cmd + " failed"
	if stderr != "" {
		msg = cmd + ": " + stderr
	}
	msg = fmt.Sprintf("%s (exit %d)", msg, code)
	grpc := code
	if code > 50 && code <= 90 {
		grpc = code - 40
	}
	switch {
	case code == 7:
		return provider.Errorf(provider.Unauthenticated, "%s", msg).WithHint(signInHint(profile))
	case code == 2 || code == 3:
		return provider.Errorf(provider.InvalidConfig, "%s", msg).
			WithHint("check the account settings; without project, the CLI profile needs a parent-id (`nebius profile update --parent-id <project ID>`)")
	case code == 4:
		return provider.Errorf(provider.InvalidConfig, "%s", msg).
			WithHint("check the profile with `nebius profile list`, or create one with `nebius profile create`")
	case grpc == 13:
		return provider.Errorf(provider.NotFound, "%s", msg).
			WithHint("check the project setting; the list of projects in the web console (https://console.nebius.com) shows their IDs")
	case grpc == 15:
		return provider.Errorf(provider.PermissionDenied, "%s", msg).
			WithHint("the CLI profile's account needs a role that can view Managed Kubernetes clusters in the project, such as viewer")
	case grpc == 16 || grpc == 23:
		return provider.Errorf(provider.RateLimited, "%s", msg).WithHint("wait a minute, then run tug sync")
	case grpc == 12 || grpc == 20:
		return provider.Errorf(provider.Unavailable, "%s", msg)
	default:
		return provider.Errorf(provider.Internal, "%s", msg)
	}
}

// signInHint says how to sign the CLI profile in again.
func signInHint(profile string) string {
	cmd := "nebius iam whoami"
	if profile != "" {
		cmd += " --profile " + profile
	}
	return "run `" + cmd + "` in a terminal to sign the CLI in, then try again"
}

// firstLine returns the first non-empty line of s, shortened for a message.
func firstLine(s string) string {
	for line := range strings.Lines(s) {
		if line = strings.TrimSpace(line); line != "" {
			if len(line) > 300 {
				line = line[:300] + "..."
			}
			return line
		}
	}
	return ""
}
