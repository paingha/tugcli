// Package management holds what the built-in providers that find clusters
// on a management cluster share: Cluster API, Argo CD and ClusterProfile
// read objects there instead of calling a cloud API. The management cluster
// is one already in tug's catalog, named by the account setting "cluster",
// so its own credential comes from the keychain like any other cluster's.
package management

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"slices"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"

	"github.com/paingha/tugcli/pkg/provider"
)

// Account settings shared by the management cluster providers.
const (
	// ConfigCluster names the management cluster, by catalog name or ID.
	ConfigCluster = "cluster"
	// ConfigNamespace limits a provider to one namespace of the management
	// cluster.
	ConfigNamespace = "namespace"
)

// ClusterConfig returns a client config for a cluster in tug's catalog,
// given its name or ID. tug builds it with kube.Factory; tests return a
// config for a fake API server.
type ClusterConfig func(ctx context.Context, cluster string) (*rest.Config, error)

// Cluster is a connection to one management cluster.
type Cluster struct {
	// Name is the catalog name or ID the account setting gave.
	Name string
	// Config is the client config, with API warnings silenced so that
	// deprecation notices do not reach tug's output.
	Config *rest.Config
}

// Connect returns a connection to the management cluster that the account's
// cluster setting names. providerName is used in hints.
func Connect(ctx context.Context, f ClusterConfig, providerName string, acct provider.Account) (*Cluster, error) {
	name := acct.ConfigString(ConfigCluster)
	if name == "" {
		return nil, provider.Errorf(provider.InvalidConfig, "%s account %q has no %s setting naming its management cluster", providerName, acct.ID, ConfigCluster).
			WithHint(fmt.Sprintf("tug import %s --account %s --set %s=<cluster in `tug ls`>", providerName, accountOrPlaceholder(acct.ID), ConfigCluster))
	}
	if f == nil {
		return nil, provider.Errorf(provider.Internal, "%s has no way to reach management clusters", providerName)
	}
	cfg, err := f(ctx, name)
	if err != nil {
		return nil, wrap(err, "management cluster "+name)
	}
	cfg = rest.CopyConfig(cfg)
	cfg.WarningHandler = rest.NoWarnings{}
	return &Cluster{Name: name, Config: cfg}, nil
}

func accountOrPlaceholder(id string) string {
	if id == "" {
		return "<account>"
	}
	return id
}

// ServedVersion returns the first of versions that the management cluster
// serves for an API group. It fails with NotFound when the group is not
// served at all, which usually means the setting names the wrong cluster.
func (c *Cluster) ServedVersion(ctx context.Context, group string, versions ...string) (string, error) {
	dc, err := discovery.NewDiscoveryClientForConfig(c.Config)
	if err != nil {
		return "", provider.Errorf(provider.InvalidConfig, "management cluster %s: %v", c.Name, err)
	}
	raw, err := dc.RESTClient().Get().AbsPath("/apis", group).DoRaw(ctx)
	if apierrors.IsNotFound(err) {
		return "", provider.Errorf(provider.NotFound, "%s does not serve the %s API", c.Name, group).
			WithHint(fmt.Sprintf("check that the account's %s setting names the cluster where %s is installed", ConfigCluster, group))
	}
	if err != nil {
		return "", c.Error(err, "discovering the "+group+" API")
	}
	var g metav1.APIGroup
	if err := json.Unmarshal(raw, &g); err != nil {
		return "", provider.Errorf(provider.Internal, "decoding the %s API group from %s: %v", group, c.Name, err)
	}
	for _, v := range versions {
		if slices.ContainsFunc(g.Versions, func(gv metav1.GroupVersionForDiscovery) bool { return gv.Version == v }) {
			return v, nil
		}
	}
	served := make([]string, len(g.Versions))
	for i, gv := range g.Versions {
		served[i] = gv.Version
	}
	return "", provider.Errorf(provider.Unimplemented, "%s serves %s versions %v; tug reads %v", c.Name, group, served, versions)
}

// Error converts an error from the management cluster's API into a provider
// error with the matching code. doing says what was in progress, for
// example "listing Cluster API clusters".
func (c *Cluster) Error(err error, doing string) error {
	if err == nil {
		return nil
	}
	return withHint(wrap(err, doing+" on "+c.Name), provider.Unauthenticated,
		fmt.Sprintf("the management cluster refused tug's credential; check it with `tug doctor %s`", c.Name))
}

func wrap(err error, doing string) error {
	var pe *provider.Error
	if errors.As(err, &pe) {
		e := *pe
		e.Message = doing + ": " + pe.Message
		return &e
	}
	code := provider.Internal
	retry := 0
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		code = provider.Unavailable
	case apierrors.IsUnauthorized(err):
		code = provider.Unauthenticated
	case apierrors.IsForbidden(err):
		code = provider.PermissionDenied
	case apierrors.IsNotFound(err):
		code = provider.NotFound
	case apierrors.IsTooManyRequests(err):
		code = provider.RateLimited
		if s, ok := apierrors.SuggestsClientDelay(err); ok {
			retry = s
		}
	case apierrors.IsServiceUnavailable(err), apierrors.IsServerTimeout(err), apierrors.IsTimeout(err),
		apierrors.IsInternalError(err), apierrors.IsUnexpectedServerError(err):
		code = provider.Unavailable
	case apierrors.IsBadRequest(err), apierrors.IsInvalid(err):
		code = provider.InvalidConfig
	default:
		// Anything that is not an API status never reached the API server:
		// a refused connection, a TLS or DNS failure.
		var status apierrors.APIStatus
		if !errors.As(err, &status) {
			code = provider.Unavailable
		}
	}
	e := provider.Errorf(code, "%s: %v", doing, err)
	e.RetryAfterSeconds = retry
	return e
}

// withHint adds a hint to a provider error of the given code that has none.
func withHint(err error, code provider.Code, hint string) error {
	var pe *provider.Error
	if errors.As(err, &pe) && pe.Code == code && pe.Hint == "" {
		return pe.WithHint(hint)
	}
	return err
}

// Decode converts an object from the dynamic client into a Go struct with
// JSON tags, the way the API server would decode it.
func Decode(u *unstructured.Unstructured, out any) error {
	b, err := json.Marshal(u.Object)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

// CertExpiry returns when the first certificate in PEM data expires, or the
// zero time when there is none.
func CertExpiry(certPEM []byte) time.Time {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return time.Time{}
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return time.Time{}
	}
	return cert.NotAfter
}

// Expiry returns when tug should read a credential from the management
// cluster again: after ttl, or when its certificate expires if that is
// sooner. Credentials there are rotated in place, so even long-lived ones
// are read again from time to time.
func Expiry(now time.Time, ttl time.Duration, certPEM []byte) time.Time {
	exp := now.Add(ttl)
	if na := CertExpiry(certPEM); !na.IsZero() && na.Before(exp) {
		exp = na
	}
	return exp
}
