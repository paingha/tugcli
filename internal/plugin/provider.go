package plugin

import (
	"context"
	"io"
	"slices"

	"github.com/paingha/tugcli/internal/registry"
	"github.com/paingha/tugcli/pkg/provider"
)

// Plugin is a discovered plugin.
type Plugin struct {
	Manifest *Manifest
	// Dir is the plugin directory; Bin is the executable for this platform.
	Dir string
	Bin string
	// Dev is true for linked and $TUG_PLUGIN_PATH plugins, which are not
	// checksummed.
	Dev bool
	// WASM is true when Bin is the plugin's WebAssembly build, which tug
	// runs itself rather than as a process.
	WASM bool
}

// Name is the provider name.
func (p *Plugin) Name() string { return p.Manifest.Metadata.Name }

// Provider runs a plugin for each operation. It implements registry.Provider.
type Provider struct {
	plugin *Plugin
	runner *Runner
}

var _ registry.Provider = (*Provider)(nil)

// NewProvider returns a provider backed by p's executable.
func NewProvider(p *Plugin, r *Runner) *Provider {
	return &Provider{plugin: p, runner: r}
}

// Close stops the plugin's serve-mode process, if it has one.
func (p *Provider) Close() error { return p.runner.Close() }

// Plugin returns the plugin behind the provider.
func (p *Provider) Plugin() *Plugin { return p.plugin }

// Exec runs the plugin with these arguments and streams, as its own process
// would; see Runner.Exec.
func (p *Provider) Exec(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	return p.runner.Exec(ctx, args, stdin, stdout, stderr)
}

// Info comes from the manifest, so listing providers starts no processes.
func (p *Provider) Info() provider.Info {
	m := p.plugin.Manifest
	return provider.Info{
		Name:         m.Metadata.Name,
		Version:      m.Metadata.Version,
		Capabilities: m.Spec.Capabilities | provider.CapList | provider.CapCredential,
		Secrets:      m.Spec.Secrets,
	}
}

// Describe runs the plugin's describe operation.
func (p *Provider) Describe(ctx context.Context) (provider.Description, error) {
	out, err := p.call(ctx, provider.OpDescribe, nil)
	if err != nil {
		return provider.Description{}, err
	}
	return decode(p, out, provider.DecodeDescribe)
}

// Accounts implements registry.Provider.
func (p *Provider) Accounts(ctx context.Context) ([]provider.Account, error) {
	if !p.plugin.Manifest.Spec.Capabilities.Has(provider.CapAccounts) {
		return nil, provider.Errorf(provider.Unimplemented, "%s does not list accounts", p.plugin.Name())
	}
	out, err := p.call(ctx, provider.OpAccounts, nil)
	if err != nil {
		return nil, err
	}
	return decode(p, out, provider.DecodeAccounts)
}

// List implements registry.Provider.
func (p *Provider) List(ctx context.Context, req provider.ListRequest) ([]provider.Cluster, error) {
	req.Secrets = p.declared(req.Secrets)
	out, err := p.call(ctx, provider.OpList, req)
	if err != nil {
		return nil, err
	}
	return decode(p, out, provider.DecodeList)
}

// CheckAccess runs the plugin's checkAccess operation, which explains whether
// the caller can reach the cluster's API server.
func (p *Provider) CheckAccess(ctx context.Context, req provider.AccessRequest) ([]provider.Finding, error) {
	if !p.plugin.Manifest.Spec.Capabilities.Has(provider.CapCheckAccess) {
		return nil, provider.Errorf(provider.Unimplemented, "%s does not check access", p.plugin.Name())
	}
	req.Secrets = p.declared(req.Secrets)
	out, err := p.call(ctx, provider.OpCheckAccess, req)
	if err != nil {
		return nil, err
	}
	return decode(p, out, provider.DecodeCheckAccess)
}

// Credential implements registry.Provider.
func (p *Provider) Credential(ctx context.Context, req provider.CredentialRequest) (provider.Credential, error) {
	req.Secrets = p.declared(req.Secrets)
	out, err := p.call(ctx, provider.OpCredential, req)
	if err != nil {
		return provider.Credential{}, err
	}
	return decode(p, out, provider.DecodeCredential)
}

// Rotate runs the plugin's rotate operation, which replaces a cluster's
// credential and revokes the old one.
func (p *Provider) Rotate(ctx context.Context, req provider.CredentialRequest) (provider.Credential, error) {
	if !p.plugin.Manifest.Spec.Capabilities.Has(provider.CapRotate) {
		return provider.Credential{}, provider.Errorf(provider.Unimplemented, "%s cannot rotate credentials", p.plugin.Name())
	}
	req.Secrets = p.declared(req.Secrets)
	out, err := p.call(ctx, provider.OpRotate, req)
	if err != nil {
		return provider.Credential{}, err
	}
	return decode(p, out, provider.DecodeCredential)
}

func (p *Provider) call(ctx context.Context, op string, payload any) ([]byte, error) {
	req, err := provider.EncodeRequest(op, payload)
	if err != nil {
		return nil, err
	}
	return p.runner.Call(ctx, op, req)
}

// decode reads a plugin's response with dec. An error the plugin answered
// with gets the sandbox hint when it reads like the sandbox refused
// something.
func decode[T any](p *Provider, out []byte, dec func([]byte) (T, error)) (T, error) {
	v, err := dec(out)
	return v, p.runner.explain(err)
}

// declared drops any secret the manifest does not declare, so a plugin only
// ever sees its own secrets.
func (p *Provider) declared(secrets map[string]string) map[string]string {
	if len(secrets) == 0 {
		return nil
	}
	out := make(map[string]string, len(secrets))
	for k, v := range secrets {
		if slices.ContainsFunc(p.plugin.Manifest.Spec.Secrets, func(s provider.SecretSpec) bool { return s.Name == k }) {
			out[k] = v
		}
	}
	return out
}
