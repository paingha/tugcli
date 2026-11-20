// Package registry holds every cluster provider tug knows, built in or loaded
// as a plugin, behind one interface.
package registry

import (
	"context"
	"fmt"
	"slices"
	"sort"

	"github.com/paingha/tugcli/pkg/provider"
)

// Provider is the interface every cluster source implements. Built-in
// providers implement it directly; the plugin host implements it by running a
// plugin process.
type Provider interface {
	// Info returns the name, version, capabilities and declared secrets.
	Info() provider.Info
	// Accounts returns accounts already set up on this machine.
	Accounts(ctx context.Context) ([]provider.Account, error)
	// List returns the clusters in one account.
	List(ctx context.Context, req provider.ListRequest) ([]provider.Cluster, error)
	// Credential returns a credential for one cluster.
	Credential(ctx context.Context, req provider.CredentialRequest) (provider.Credential, error)
}

// Kind says where a provider comes from.
type Kind int

const (
	// KindBuiltin is compiled into tug.
	KindBuiltin Kind = iota + 1
	// KindPlugin is an installed plugin.
	KindPlugin
	// KindDevPlugin is a linked or $TUG_PLUGIN_PATH plugin without checksums.
	KindDevPlugin
)

func (k Kind) String() string {
	switch k {
	case KindBuiltin:
		return "built-in"
	case KindPlugin:
		return "plugin"
	case KindDevPlugin:
		return "dev plugin"
	default:
		return "unknown"
	}
}

// Entry is one registered provider.
type Entry struct {
	Provider Provider
	Kind     Kind
	// Location is the plugin directory, or "" for built-ins.
	Location string
}

// Registry maps provider names to providers.
type Registry struct {
	entries map[string]Entry
	// allowOverride lists built-in names that plugins may replace.
	allowOverride []string
}

// New returns an empty registry. Plugins may replace the built-ins named in
// allowOverride; any other name clash is refused.
func New(allowOverride []string) *Registry {
	return &Registry{entries: map[string]Entry{}, allowOverride: allowOverride}
}

// Add registers a provider.
func (r *Registry) Add(p Provider, kind Kind, location string) error {
	name := p.Info().Name
	if name == "" {
		return fmt.Errorf("provider at %q has no name", location)
	}
	if existing, ok := r.entries[name]; ok {
		if existing.Kind == KindBuiltin && kind != KindBuiltin && !slices.Contains(r.allowOverride, name) {
			return fmt.Errorf("plugin %q at %s would replace the built-in provider; allow it with plugins.allowOverride in config.yaml", name, location)
		}
		if existing.Kind != KindBuiltin && kind != KindBuiltin {
			// First plugin found wins; discovery adds them in precedence order.
			return nil
		}
	}
	r.entries[name] = Entry{Provider: p, Kind: kind, Location: location}
	return nil
}

// Get returns the provider with the given name.
func (r *Registry) Get(name string) (Provider, error) {
	e, ok := r.entries[name]
	if !ok {
		return nil, fmt.Errorf("unknown provider %q (known: %v)", name, r.Names())
	}
	return e.Provider, nil
}

// Entry returns the registration for a provider.
func (r *Registry) Entry(name string) (Entry, bool) {
	e, ok := r.entries[name]
	return e, ok
}

// Names returns the provider names, sorted.
func (r *Registry) Names() []string {
	names := make([]string, 0, len(r.entries))
	for n := range r.entries {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// FromSDK adapts a provider written against the public SDK to the registry
// interface. Accounts reports Unimplemented when p does not list accounts.
func FromSDK(p provider.Provider) Provider { return sdkAdapter{p: p} }

type sdkAdapter struct{ p provider.Provider }

var _ Provider = sdkAdapter{}

func (a sdkAdapter) Info() provider.Info {
	info := a.p.Info()
	info.Capabilities = provider.Capabilities(a.p)
	return info
}

func (a sdkAdapter) Accounts(ctx context.Context) ([]provider.Account, error) {
	al, ok := a.p.(provider.AccountLister)
	if !ok {
		return nil, provider.Errorf(provider.Unimplemented, "%s does not list accounts", a.p.Info().Name)
	}
	return al.Accounts(ctx)
}

func (a sdkAdapter) List(ctx context.Context, req provider.ListRequest) ([]provider.Cluster, error) {
	return a.p.List(ctx, req)
}

func (a sdkAdapter) Credential(ctx context.Context, req provider.CredentialRequest) (provider.Credential, error) {
	return a.p.Credential(ctx, req)
}
