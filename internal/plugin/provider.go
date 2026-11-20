package plugin

import (
	"context"
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

// Plugin returns the plugin behind the provider.
func (p *Provider) Plugin() *Plugin { return p.plugin }

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
	return provider.DecodeDescribe(out)
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
	return provider.DecodeAccounts(out)
}

// List implements registry.Provider.
func (p *Provider) List(ctx context.Context, req provider.ListRequest) ([]provider.Cluster, error) {
	req.Secrets = p.declared(req.Secrets)
	out, err := p.call(ctx, provider.OpList, req)
	if err != nil {
		return nil, err
	}
	return provider.DecodeList(out)
}

// Credential implements registry.Provider.
func (p *Provider) Credential(ctx context.Context, req provider.CredentialRequest) (provider.Credential, error) {
	req.Secrets = p.declared(req.Secrets)
	out, err := p.call(ctx, provider.OpCredential, req)
	if err != nil {
		return provider.Credential{}, err
	}
	return provider.DecodeCredential(out)
}

func (p *Provider) call(ctx context.Context, op string, payload any) ([]byte, error) {
	req, err := provider.EncodeRequest(op, payload)
	if err != nil {
		return nil, err
	}
	return p.runner.Call(ctx, op, req)
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
