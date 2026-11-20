// Package doctor diagnoses why tug, or kubectl through tug, cannot reach a
// cluster: tug's own setup (shell hook, credential store, kubectl and its
// credential plugin policy, the catalog) and, for one cluster, DNS, TCP, TLS,
// credentials, the API server, clock and version skew, permissions and the
// provider's access list. It returns results and never prints.
package doctor

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"slices"
	"time"

	"k8s.io/client-go/rest"

	"github.com/paingha/tugcli/internal/catalog"
	"github.com/paingha/tugcli/internal/credentials"
	"github.com/paingha/tugcli/pkg/provider"
)

// Status of one check.
type Status string

// Check outcomes, from best to worst.
const (
	OK   Status = "ok"
	Skip Status = "skip"
	Warn Status = "warn"
	Fail Status = "fail"
)

// Groups that results belong to.
const (
	GroupSetup   = "setup"
	GroupCluster = "cluster"
)

// Result is the outcome of one check.
type Result struct {
	Group  string `json:"group"`
	Name   string `json:"name"`
	Status Status `json:"status"`
	Detail string `json:"detail,omitempty"`
	// Fix says what to do when the check did not pass.
	Fix string `json:"fix,omitempty"`
}

// Report collects every result.
type Report struct {
	Cluster string   `json:"cluster,omitempty"`
	Results []Result `json:"results"`
}

// Failed reports whether any check failed.
func (r Report) Failed() bool {
	return slices.ContainsFunc(r.Results, func(x Result) bool { return x.Status == Fail })
}

// Count returns how many results have the status.
func (r Report) Count(s Status) int {
	n := 0
	for _, x := range r.Results {
		if x.Status == s {
			n++
		}
	}
	return n
}

// Resolver looks up host names; *net.Resolver satisfies it.
type Resolver interface {
	LookupHost(ctx context.Context, host string) ([]string, error)
}

// DialFunc opens a network connection; (*net.Dialer).DialContext satisfies it.
type DialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// Config is everything Run checks. Fields left nil are skipped.
type Config struct {
	// Now defaults to time.Now.
	Now func() time.Time

	Session SessionInfo
	Store   StoreInfo
	Kubectl KubectlInfo
	Kuberc  KubercInfo
	Catalog *catalog.Catalog

	// Cluster is the cluster to check; nil checks tug's setup only.
	Cluster   *catalog.Cluster
	Namespace string
	// Credential resolves the cluster's credential.
	Credential func(ctx context.Context) (*credentials.Resolved, error)
	// RESTConfig builds a client config for the cluster, guarded read-only.
	RESTConfig func(ctx context.Context) (*rest.Config, error)
	// Access runs the provider's access check, when it has one.
	Access func(ctx context.Context) ([]provider.Finding, error)

	Resolver Resolver
	Dial     DialFunc
	// Tunnel, when set, is how Dial reaches the cluster; names are then
	// resolved at the far end, so the DNS check is left to it.
	Tunnel string
	// Proxy picks a proxy for a request; defaults to the environment's.
	Proxy func(*http.Request) (*url.URL, error)
	// Timeout bounds each network check; defaults to 5 seconds.
	Timeout time.Duration
}

func (c *Config) defaults() {
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Resolver == nil {
		c.Resolver = net.DefaultResolver
	}
	if c.Dial == nil {
		c.Dial = (&net.Dialer{}).DialContext
	}
	if c.Proxy == nil {
		c.Proxy = http.ProxyFromEnvironment
	}
	if c.Timeout <= 0 {
		c.Timeout = 5 * time.Second
	}
}

// Run runs every check that cfg has inputs for.
func Run(ctx context.Context, cfg Config) Report {
	cfg.defaults()
	var r Report
	r.Results = append(r.Results, checkSession(cfg.Session))
	r.Results = append(r.Results, checkStore(cfg.Store))
	kubectl, clientVersion := checkKubectl(ctx, cfg.Kubectl)
	r.Results = append(r.Results, kubectl)
	r.Results = append(r.Results, checkKuberc(cfg.Kuberc))
	if cfg.Catalog != nil {
		r.Results = append(r.Results, checkCatalog(cfg.Catalog)...)
	}
	if cfg.Cluster != nil {
		r.Cluster = cfg.Cluster.Name
		r.Results = append(r.Results, checkCluster(ctx, cfg, clientVersion)...)
	}
	return r
}

func setup(name string, s Status, detail, fix string) Result {
	return Result{Group: GroupSetup, Name: name, Status: s, Detail: detail, Fix: fix}
}

func cluster(name string, s Status, detail, fix string) Result {
	return Result{Group: GroupCluster, Name: name, Status: s, Detail: detail, Fix: fix}
}
