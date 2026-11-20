// Package catalog is tug's registry of clusters. It merges what each source
// (a provider account) lists, keeps stable names, normalises labels and marks
// clusters that disappeared upstream as stale. It never holds secrets.
package catalog

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/paingha/tugcli/internal/config"
	"github.com/paingha/tugcli/pkg/provider"
)

// APIVersion of the catalog file.
const APIVersion = "tug.catalog/v1alpha1"

// FileName is the catalog file under the data directory.
const FileName = "catalog.yaml"

// Normalised label keys.
const (
	LabelProvider = "provider"
	LabelAccount  = "account"
	LabelRegion   = "region"
	LabelEnv      = "env"
	LabelGPU      = "gpu"
)

// ErrNotFound is returned when no cluster matches.
var ErrNotFound = errors.New("cluster not found")

// Catalog is the on-disk registry.
type Catalog struct {
	APIVersion string    `json:"apiVersion"`
	Sources    []Source  `json:"sources,omitempty"`
	Clusters   []Cluster `json:"clusters,omitempty"`
}

// Source is one provider account that tug imports clusters from.
type Source struct {
	Provider  string         `json:"provider"`
	Account   string         `json:"account"`
	Config    map[string]any `json:"config,omitempty"`
	LastSync  time.Time      `json:"lastSync,omitempty"`
	LastError string         `json:"lastError,omitempty"`
}

// Key identifies a source.
func (s Source) Key() string { return s.Provider + "/" + s.Account }

// ProviderAccount returns the source as the account sent to its provider.
func (s Source) ProviderAccount() provider.Account {
	return provider.Account{ID: s.Account, Config: s.Config}
}

// Cluster is one cluster tug can connect to.
type Cluster struct {
	// ID is "<provider>/<account>/<provider cluster id>" and never changes.
	ID string `json:"id"`
	// Name is unique in the catalog and is what users type.
	Name       string `json:"name"`
	Provider   string `json:"provider"`
	Account    string `json:"account"`
	ProviderID string `json:"providerID"`

	Region  string `json:"region,omitempty"`
	Version string `json:"version,omitempty"`

	Server                string `json:"server"`
	PrivateServer         string `json:"privateServer,omitempty"`
	CAData                []byte `json:"caData,omitempty"`
	TLSServerName         string `json:"tlsServerName,omitempty"`
	InsecureSkipTLSVerify bool   `json:"insecureSkipTLSVerify,omitempty"`
	ProxyURL              string `json:"proxyURL,omitempty"`
	DefaultNamespace      string `json:"defaultNamespace,omitempty"`
	// Tunnel is how tug reaches the API server when it is not directly
	// reachable, such as ssh://user@bastion or ssm://i-0123456789abcdef0.
	// The user sets it with `tug tunnel set`, and syncs keep it.
	Tunnel string `json:"tunnel,omitempty"`

	// Labels come from the provider and are normalised; UserLabels are set by
	// the user and win over them.
	Labels     map[string]string `json:"labels,omitempty"`
	UserLabels map[string]string `json:"userLabels,omitempty"`

	Stale     bool      `json:"stale,omitempty"`
	FirstSeen time.Time `json:"firstSeen"`
	LastSeen  time.Time `json:"lastSeen"`
}

// EffectiveLabels merges provider and user labels.
func (c *Cluster) EffectiveLabels() map[string]string {
	out := make(map[string]string, len(c.Labels)+len(c.UserLabels))
	for k, v := range c.Labels {
		out[k] = v
	}
	for k, v := range c.UserLabels {
		if v == "" {
			delete(out, k)
			continue
		}
		out[k] = v
	}
	return out
}

// Matches reports whether the cluster has every label in sel.
func (c *Cluster) Matches(sel map[string]string) bool {
	labels := c.EffectiveLabels()
	for k, v := range sel {
		if labels[k] != v {
			return false
		}
	}
	return true
}

// ClusterID builds a catalog ID.
func ClusterID(providerName, account, providerID string) string {
	return providerName + "/" + account + "/" + providerID
}

// Path returns the catalog path under a data directory.
func Path(dataDir string) string { return filepath.Join(dataDir, FileName) }

// Load reads the catalog. A missing file is an empty catalog.
func Load(path string) (*Catalog, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &Catalog{APIVersion: APIVersion}, nil
	}
	if err != nil {
		return nil, err
	}
	c := &Catalog{}
	if err := yaml.Unmarshal(b, c); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if c.APIVersion == "" {
		c.APIVersion = APIVersion
	}
	if c.APIVersion != APIVersion {
		return nil, fmt.Errorf("%s: unsupported apiVersion %q", path, c.APIVersion)
	}
	return c, nil
}

// Save writes the catalog atomically.
func (c *Catalog) Save(path string) error {
	c.APIVersion = APIVersion
	b, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	return config.WriteFileAtomic(path, b, 0o600)
}

// Source returns the source with the given provider and account.
func (c *Catalog) Source(providerName, account string) (*Source, bool) {
	for i := range c.Sources {
		if c.Sources[i].Provider == providerName && c.Sources[i].Account == account {
			return &c.Sources[i], true
		}
	}
	return nil, false
}

// AddSource adds or updates a source.
func (c *Catalog) AddSource(s Source) *Source {
	if existing, ok := c.Source(s.Provider, s.Account); ok {
		existing.Config = s.Config
		return existing
	}
	c.Sources = append(c.Sources, s)
	return &c.Sources[len(c.Sources)-1]
}

// RemoveSource removes a source and its clusters, returning the removed
// clusters so that the caller can forget their credentials.
func (c *Catalog) RemoveSource(providerName, account string) []Cluster {
	c.Sources = slices.DeleteFunc(c.Sources, func(s Source) bool {
		return s.Provider == providerName && s.Account == account
	})
	var removed []Cluster
	c.Clusters = slices.DeleteFunc(c.Clusters, func(cl Cluster) bool {
		if cl.Provider == providerName && cl.Account == account {
			removed = append(removed, cl)
			return true
		}
		return false
	})
	return removed
}

// Get returns the cluster with the given ID or name.
func (c *Catalog) Get(idOrName string) (*Cluster, error) {
	if cl := c.byID(idOrName); cl != nil {
		return cl, nil
	}
	for i := range c.Clusters {
		if c.Clusters[i].Name == idOrName {
			return &c.Clusters[i], nil
		}
	}
	return nil, fmt.Errorf("%w: %s", ErrNotFound, idOrName)
}

func (c *Catalog) byID(id string) *Cluster {
	for i := range c.Clusters {
		if c.Clusters[i].ID == id {
			return &c.Clusters[i]
		}
	}
	return nil
}

// Select returns the clusters matching every label in sel.
func (c *Catalog) Select(sel map[string]string) []*Cluster {
	var out []*Cluster
	for i := range c.Clusters {
		if c.Clusters[i].Matches(sel) {
			out = append(out, &c.Clusters[i])
		}
	}
	return out
}

// MergeResult summarises a merge.
type MergeResult struct {
	Added, Updated, Stale []string // cluster names
}

// Merge records what a source listed at time now. Clusters from this source
// that were not listed are marked stale rather than removed.
func (c *Catalog) Merge(src Source, listed []provider.Cluster, now time.Time) MergeResult {
	var res MergeResult
	seen := make(map[string]bool, len(listed))
	for _, pc := range listed {
		id := ClusterID(src.Provider, src.Account, pc.ID)
		seen[id] = true
		next := fromProvider(src, pc, now)
		if existing := c.byID(id); existing != nil {
			next.Name = existing.Name
			next.UserLabels = existing.UserLabels
			next.Tunnel = existing.Tunnel
			next.FirstSeen = existing.FirstSeen
			// A listing without a CA for an unchanged endpoint means the
			// provider could not fetch it this time, for example a kubeconfig
			// that was briefly unavailable. Keep the one already trusted
			// rather than breaking TLS verification.
			if len(next.CAData) == 0 && next.Server == existing.Server {
				next.CAData = existing.CAData
			}
			*existing = next
			res.Updated = append(res.Updated, next.Name)
			continue
		}
		next.Name = c.uniqueName(pc.Name, src)
		c.Clusters = append(c.Clusters, next)
		res.Added = append(res.Added, next.Name)
	}
	for i := range c.Clusters {
		cl := &c.Clusters[i]
		if cl.Provider == src.Provider && cl.Account == src.Account && !seen[cl.ID] && !cl.Stale {
			cl.Stale = true
			res.Stale = append(res.Stale, cl.Name)
		}
	}
	slices.SortFunc(c.Clusters, func(a, b Cluster) int { return strings.Compare(a.Name, b.Name) })
	return res
}

func fromProvider(src Source, pc provider.Cluster, now time.Time) Cluster {
	server := pc.Endpoints.Public
	if server == "" {
		server = pc.Endpoints.Private
	}
	cl := Cluster{
		ID:                    ClusterID(src.Provider, src.Account, pc.ID),
		Name:                  pc.Name,
		Provider:              src.Provider,
		Account:               src.Account,
		ProviderID:            pc.ID,
		Region:                pc.Region,
		Version:               pc.Version,
		Server:                server,
		CAData:                pc.CAData,
		TLSServerName:         pc.TLSServerName,
		InsecureSkipTLSVerify: pc.InsecureSkipTLSVerify,
		ProxyURL:              pc.ProxyURL,
		DefaultNamespace:      pc.DefaultNamespace,
		FirstSeen:             now,
		LastSeen:              now,
	}
	if pc.Endpoints.Private != "" && pc.Endpoints.Private != server {
		cl.PrivateServer = pc.Endpoints.Private
	}
	cl.Labels = NormaliseLabels(src.Provider, src.Account, pc)
	return cl
}

// uniqueName returns name, or name@account (then name@provider/account) when
// another cluster already uses it.
func (c *Catalog) uniqueName(name string, src Source) string {
	if name == "" {
		name = "cluster"
	}
	candidates := []string{name, name + "@" + src.Account, name + "@" + src.Provider + "-" + src.Account}
	for _, n := range candidates {
		if !c.nameTaken(n) {
			return n
		}
	}
	for i := 2; ; i++ {
		n := fmt.Sprintf("%s-%d", name, i)
		if !c.nameTaken(n) {
			return n
		}
	}
}

func (c *Catalog) nameTaken(name string) bool {
	return slices.ContainsFunc(c.Clusters, func(cl Cluster) bool { return cl.Name == name })
}

// NormaliseLabels returns the provider's labels plus provider, account, region
// and an env guessed from the cluster name when the provider does not set one.
func NormaliseLabels(providerName, account string, pc provider.Cluster) map[string]string {
	out := make(map[string]string, len(pc.Labels)+4)
	for k, v := range pc.Labels {
		out[strings.ToLower(k)] = v
	}
	out[LabelProvider] = providerName
	out[LabelAccount] = account
	if pc.Region != "" {
		out[LabelRegion] = strings.ToLower(pc.Region)
	}
	if env, ok := out[LabelEnv]; ok {
		out[LabelEnv] = normaliseEnv(env)
	} else if env := GuessEnv(pc.Name); env != "" {
		out[LabelEnv] = env
	}
	if v, ok := out[LabelGPU]; ok {
		out[LabelGPU] = strings.ToLower(v)
	}
	return out
}

var envAliases = map[string]string{
	"prod": "prod", "production": "prod", "prd": "prod", "live": "prod",
	"staging": "staging", "stage": "staging", "stg": "staging",
	"dev": "dev", "development": "dev",
	"test": "test", "qa": "test",
}

func normaliseEnv(v string) string {
	if n, ok := envAliases[strings.ToLower(v)]; ok {
		return n
	}
	return strings.ToLower(v)
}

// GuessEnv infers an environment from words in a cluster name, for example
// "payments-prod-eu" gives "prod". It returns "" when no word matches.
func GuessEnv(name string) string {
	words := strings.FieldsFunc(strings.ToLower(name), func(r rune) bool {
		return r == '-' || r == '_' || r == '.' || r == '/' || r == '@' || r == ':'
	})
	for _, w := range words {
		if env, ok := envAliases[w]; ok {
			return env
		}
	}
	return ""
}
