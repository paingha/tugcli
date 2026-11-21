// Package importer imports and syncs clusters from provider accounts into the
// catalog, and fetches credentials for catalog clusters from their provider.
package importer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/paingha/tugcli/internal/catalog"
	"github.com/paingha/tugcli/internal/credentials"
	"github.com/paingha/tugcli/internal/registry"
	"github.com/paingha/tugcli/internal/tunnel"
	"github.com/paingha/tugcli/pkg/provider"
)

// SecretEnvPrefix lets CI supply provider secrets without a prompt:
// TUG_SECRET_API_TOKEN supplies the secret named api_token.
const SecretEnvPrefix = "TUG_SECRET_"

// Service ties the registry, catalog and credential store together.
type Service struct {
	Registry *registry.Registry
	Store    credentials.Store
	Broker   *credentials.Broker
	Now      func() time.Time
}

// Result is the outcome of importing one source.
type Result struct {
	Source      catalog.Source
	Merge       catalog.MergeResult
	Clusters    int
	Credentials int
	Err         error
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Import lists one source and merges it into cat. The source is added to the
// catalog. Credentials returned inline are moved to the store.
func (s *Service) Import(ctx context.Context, cat *catalog.Catalog, src catalog.Source) Result {
	cat.AddSource(src)
	res := Result{Source: src}
	p, err := s.Registry.Get(src.Provider)
	if err != nil {
		res.Err = err
		s.recordError(cat, src, err)
		return res
	}
	secrets, err := s.Secrets(p.Info(), src.Account)
	if err != nil {
		res.Err = err
		s.recordError(cat, src, err)
		return res
	}
	listed, err := p.List(ctx, provider.ListRequest{Account: src.ProviderAccount(), Secrets: secrets})
	if err != nil {
		res.Err = err
		s.recordError(cat, src, err)
		return res
	}
	tunnelErr := dropUnusableTunnels(src.Provider, listed)
	now := s.now()
	res.Merge = cat.Merge(src, listed, now)
	res.Clusters = len(listed)
	var credErrs []error
	for _, pc := range listed {
		if pc.Credential == nil {
			continue
		}
		id := catalog.ClusterID(src.Provider, src.Account, pc.ID)
		if err := s.Broker.Put(id, *pc.Credential); err != nil {
			credErrs = append(credErrs, fmt.Errorf("%s: %w", pc.Name, err))
			continue
		}
		res.Credentials++
	}
	if stored, ok := cat.Source(src.Provider, src.Account); ok {
		stored.LastSync = now
		stored.LastError = ""
		res.Source = *stored
	}
	res.Err = tunnelErr
	if len(credErrs) > 0 {
		res.Err = errors.Join(res.Err, fmt.Errorf("storing credentials in %s: %w", s.Store.Name(), errors.Join(credErrs...)))
	}
	return res
}

// dropUnusableTunnels removes the tunnels in a listing that tug cannot open
// or keep in its catalog, such as one with a password, and says which.
func dropUnusableTunnels(providerName string, listed []provider.Cluster) error {
	var errs []error
	for i := range listed {
		spec := listed[i].Tunnel
		if spec == "" {
			continue
		}
		if _, err := tunnel.Check(spec); err != nil {
			listed[i].Tunnel = ""
			errs = append(errs, fmt.Errorf("%s: ignoring the tunnel %s lists for it: %w", listed[i].Name, providerName, err))
		}
	}
	return errors.Join(errs...)
}

func (s *Service) recordError(cat *catalog.Catalog, src catalog.Source, err error) {
	if stored, ok := cat.Source(src.Provider, src.Account); ok {
		stored.LastError = err.Error()
	}
}

// Sync re-imports every source in the catalog.
func (s *Service) Sync(ctx context.Context, cat *catalog.Catalog) []Result {
	sources := append([]catalog.Source(nil), cat.Sources...)
	results := make([]Result, 0, len(sources))
	for _, src := range sources {
		results = append(results, s.Import(ctx, cat, src))
	}
	return results
}

// Forget removes a source, its clusters, their credentials and its secrets.
func (s *Service) Forget(cat *catalog.Catalog, providerName, account string) ([]catalog.Cluster, error) {
	removed := cat.RemoveSource(providerName, account)
	var errs []error
	for _, cl := range removed {
		errs = append(errs, s.Broker.Forget(cl.ID))
	}
	if p, err := s.Registry.Get(providerName); err == nil {
		for _, spec := range p.Info().Secrets {
			errs = append(errs, s.Store.Delete(credentials.SecretKey(providerName, account, spec.Name)))
		}
	}
	return removed, errors.Join(errs...)
}

// MissingSecrets returns the declared, required secrets that are neither
// stored nor supplied through the environment.
func (s *Service) MissingSecrets(info provider.Info, account string) []provider.SecretSpec {
	var missing []provider.SecretSpec
	for _, spec := range info.Secrets {
		if _, ok := s.secret(info.Name, account, spec.Name); !ok && !spec.Optional {
			missing = append(missing, spec)
		}
	}
	return missing
}

// SetSecret stores a provider secret for an account.
func (s *Service) SetSecret(providerName, account, name, value string) error {
	return s.Store.Set(credentials.SecretKey(providerName, account, name), value)
}

// Secrets loads the provider's declared secrets for an account. Only declared
// secrets are ever loaded, so a provider cannot read another's.
func (s *Service) Secrets(info provider.Info, account string) (map[string]string, error) {
	if len(info.Secrets) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(info.Secrets))
	for _, spec := range info.Secrets {
		v, ok := s.secret(info.Name, account, spec.Name)
		if !ok {
			if spec.Optional {
				continue
			}
			return nil, provider.Errorf(provider.Unauthenticated, "no %s stored for %s account %q", spec.Name, info.Name, account).
				WithHint(fmt.Sprintf("run `tug import %s --account %s` to enter it", info.Name, account))
		}
		out[spec.Name] = v
	}
	return out, nil
}

func (s *Service) secret(providerName, account, name string) (string, bool) {
	if v := os.Getenv(SecretEnvPrefix + strings.ToUpper(name)); v != "" {
		return v, true
	}
	v, err := s.Store.Get(credentials.SecretKey(providerName, account, name))
	return v, err == nil && v != ""
}

// Fetcher returns a credentials.Fetcher that asks a catalog cluster's provider
// for a credential.
func (s *Service) Fetcher(cat *catalog.Catalog) credentials.Fetcher {
	return func(ctx context.Context, clusterID string) (provider.Credential, error) {
		p, req, err := s.credentialRequest(cat, clusterID)
		if err != nil {
			return provider.Credential{}, err
		}
		return p.Credential(ctx, req)
	}
}

// ClusterLookup returns a credentials.ClusterLookup that tells delegated
// exec plugins about a catalog cluster: what a session kubeconfig would
// tell kubectl about it.
func ClusterLookup(cat *catalog.Catalog) credentials.ClusterLookup {
	return func(clusterID string) (credentials.ExecCluster, error) {
		cl, err := cat.Get(clusterID)
		if err != nil {
			return credentials.ExecCluster{}, err
		}
		return credentials.ExecCluster{
			Server:                cl.Server,
			CAData:                cl.CAData,
			TLSServerName:         cl.TLSServerName,
			InsecureSkipTLSVerify: cl.InsecureSkipTLSVerify,
			ProxyURL:              cl.ProxyURL,
		}, nil
	}
}

// Rotator is a provider that can replace a cluster's credential, revoking
// the old one.
type Rotator interface {
	Rotate(ctx context.Context, req provider.CredentialRequest) (provider.Credential, error)
}

// CanRotate reports whether a cluster's provider can rotate its credential.
func (s *Service) CanRotate(cl *catalog.Cluster) bool {
	p, err := s.Registry.Get(cl.Provider)
	if err != nil {
		return false
	}
	_, ok := p.(Rotator)
	return ok && p.Info().Capabilities.Has(provider.CapRotate)
}

// Rotate asks a cluster's provider for a new credential, which revokes the
// old one, and stores it in place of the cached credential. When rotate
// fails, the cached credential is dropped anyway: the provider may already
// have revoked it, and the next request then asks the provider afresh.
func (s *Service) Rotate(ctx context.Context, cat *catalog.Catalog, clusterID string) error {
	p, req, err := s.credentialRequest(cat, clusterID)
	if err != nil {
		return err
	}
	r, ok := p.(Rotator)
	if !ok || !p.Info().Capabilities.Has(provider.CapRotate) {
		return provider.Errorf(provider.Unimplemented, "%s cannot rotate credentials", p.Info().Name)
	}
	c, err := r.Rotate(ctx, req)
	if err == nil {
		err = c.Validate()
		if err != nil {
			err = fmt.Errorf("provider returned an invalid credential: %w", err)
		}
	}
	if err != nil {
		_ = s.Broker.Forget(clusterID) // best effort; the error below matters more
		return err
	}
	if err := s.Broker.Put(clusterID, c); err != nil {
		_ = s.Broker.Forget(clusterID)
		return fmt.Errorf("storing the new credential in %s: %w; the next request asks the provider again", s.Store.Name(), err)
	}
	return nil
}

// credentialRequest finds a catalog cluster's provider and builds the
// request for its credential, with the account's settings and secrets.
func (s *Service) credentialRequest(cat *catalog.Catalog, clusterID string) (registry.Provider, provider.CredentialRequest, error) {
	cl, err := cat.Get(clusterID)
	if err != nil {
		return nil, provider.CredentialRequest{}, err
	}
	src, ok := cat.Source(cl.Provider, cl.Account)
	if !ok {
		return nil, provider.CredentialRequest{}, fmt.Errorf("cluster %s has no source in the catalog; run `tug import`", cl.Name)
	}
	p, err := s.Registry.Get(cl.Provider)
	if err != nil {
		return nil, provider.CredentialRequest{}, err
	}
	secrets, err := s.Secrets(p.Info(), cl.Account)
	if err != nil {
		return nil, provider.CredentialRequest{}, err
	}
	return p, provider.CredentialRequest{Account: src.ProviderAccount(), ClusterID: cl.ProviderID, Secrets: secrets}, nil
}
