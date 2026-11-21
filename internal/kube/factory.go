// Package kube builds Kubernetes clients for the session's cluster. Every
// client goes through the guard, and credentials come from the broker in
// process rather than by running `tug auth token`.
package kube

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"sync/atomic"
	"time"

	"golang.org/x/oauth2"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/disk"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/client-go/transport"

	"github.com/paingha/tugcli/internal/catalog"
	"github.com/paingha/tugcli/internal/credentials"
	"github.com/paingha/tugcli/internal/guard"
	"github.com/paingha/tugcli/internal/tunnel"
)

// discoveryTTL is how long cached API discovery stays fresh.
const discoveryTTL = 6 * time.Hour

// Factory builds clients for one cluster.
type Factory struct {
	Cluster   *catalog.Cluster
	Namespace string
	Broker    *credentials.Broker
	Guard     *guard.Policy
	// CacheDir is tug's cache directory; discovery is cached per cluster
	// beneath it.
	CacheDir  string
	UserAgent string
	// Tunnels opens the cluster's tunnel, when it has one.
	Tunnels *tunnel.Pool
}

// RESTConfig returns a client config for the cluster.
func (f *Factory) RESTConfig(ctx context.Context) (*rest.Config, error) {
	cl := f.Cluster
	cfg := &rest.Config{
		Host:      cl.Server,
		UserAgent: f.UserAgent,
		TLSClientConfig: rest.TLSClientConfig{
			CAData:     cl.CAData,
			ServerName: cl.TLSServerName,
			Insecure:   cl.InsecureSkipTLSVerify,
		},
		QPS:   50,
		Burst: 100,
	}
	switch {
	case cl.Tunnel != "":
		// The tunnel is a proxy to client-go, so exec, attach and
		// port-forward upgrades go through it too.
		if f.Tunnels == nil {
			return nil, fmt.Errorf("cluster %s is reached through a tunnel, which this command cannot open", cl.Name)
		}
		u, err := f.Tunnels.ProxyURL(ctx, cl.Tunnel)
		if err != nil {
			return nil, fmt.Errorf("opening the tunnel to %s: %w", cl.Name, err)
		}
		cfg.Proxy = http.ProxyURL(u)
	case cl.ProxyURL != "":
		u, err := url.Parse(cl.ProxyURL)
		if err != nil {
			return nil, fmt.Errorf("cluster %s: invalid proxy URL: %w", cl.Name, err)
		}
		cfg.Proxy = http.ProxyURL(u)
	}

	r, err := f.Broker.Resolve(ctx, cl.ID, false)
	if err != nil {
		return nil, fmt.Errorf("getting credentials for %s: %w", cl.Name, err)
	}
	switch {
	case r.Delegate != nil:
		cfg.ExecProvider = execProvider(r)
	case len(r.CertPEM) > 0:
		cfg.CertData, cfg.KeyData = r.CertPEM, r.KeyPEM
	default:
		cfg.Wrap(transport.TokenSourceWrapTransport(newBrokerTokenSource(f.Broker, cl.ID, r)))
	}
	if f.Guard != nil {
		cfg.Wrap(func(rt http.RoundTripper) http.RoundTripper { return guard.Wrap(rt, f.Guard) })
	}
	return cfg, nil
}

// Clientset returns a typed client.
func (f *Factory) Clientset(ctx context.Context) (kubernetes.Interface, error) {
	cfg, err := f.RESTConfig(ctx)
	if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfig(cfg)
}

// Discovery returns a discovery client cached on disk per cluster.
func (f *Factory) Discovery(ctx context.Context) (discovery.CachedDiscoveryInterface, error) {
	cfg, err := f.RESTConfig(ctx)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(f.CacheDir, "discovery", safeName(f.Cluster.ID))
	return disk.NewCachedDiscoveryClientForConfig(cfg, dir, filepath.Join(f.CacheDir, "http"), discoveryTTL)
}

var unsafeChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func safeName(s string) string { return unsafeChars.ReplaceAllString(s, "_") }

func execProvider(r *credentials.Resolved) *clientcmdapi.ExecConfig {
	d := r.Delegate
	apiVersion := d.APIVersion
	if apiVersion == "" {
		apiVersion = "client.authentication.k8s.io/v1"
	}
	mode := clientcmdapi.ExecInteractiveMode(d.InteractiveMode)
	if mode == "" {
		mode = clientcmdapi.IfAvailableExecInteractiveMode
	}
	ec := &clientcmdapi.ExecConfig{
		Command:         d.Command,
		Args:            d.Args,
		APIVersion:      apiVersion,
		InteractiveMode: mode,
		InstallHint:     d.InstallHint,
	}
	for k, v := range d.Env {
		ec.Env = append(ec.Env, clientcmdapi.ExecEnvVar{Name: k, Value: v})
	}
	return ec
}

// brokerTokenSource hands client-go tokens from the broker, which refreshes
// them when they expire. client-go calls Token for every request and does
// not serialise the calls, so concurrent requests call it at once.
type brokerTokenSource struct {
	broker    *credentials.Broker
	clusterID string
	// first is the credential RESTConfig already resolved, handed to
	// exactly one caller.
	first atomic.Pointer[credentials.Resolved]
}

var _ oauth2.TokenSource = (*brokerTokenSource)(nil)

func newBrokerTokenSource(b *credentials.Broker, clusterID string, first *credentials.Resolved) *brokerTokenSource {
	s := &brokerTokenSource{broker: b, clusterID: clusterID}
	s.first.Store(first)
	return s
}

func (s *brokerTokenSource) Token() (*oauth2.Token, error) {
	r := s.first.Swap(nil)
	if r == nil {
		var err error
		r, err = s.broker.Resolve(context.Background(), s.clusterID, false)
		if err != nil {
			return nil, err
		}
	}
	if r.Token == "" {
		return nil, fmt.Errorf("credential for %s is not a token", s.clusterID)
	}
	t := &oauth2.Token{AccessToken: r.Token, TokenType: "Bearer"}
	if r.ExpiresAt != nil {
		t.Expiry = *r.ExpiresAt
	}
	return t, nil
}
