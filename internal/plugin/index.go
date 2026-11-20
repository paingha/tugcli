package plugin

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/paingha/tugcli/internal/config"
)

// Index API version and kind.
const (
	IndexAPIVersion = "tug.plugin.index/v1alpha1"
	IndexKind       = "PluginIndex"
	// SignatureSuffix names an index's detached signature: index.yaml.sig.
	SignatureSuffix = ".sig"
	// indexTTL is how long a downloaded index is reused.
	indexTTL = time.Hour
	// maxIndexBytes bounds a downloaded index or signature.
	maxIndexBytes = 8 << 20
)

// Index is a list of reviewed plugins and where to get them. It is signed
// with ed25519 as a whole, and each version pins its archive's sha256, so a
// plugin installed from it is checked from the index key down to every
// binary.
type Index struct {
	APIVersion string        `json:"apiVersion"`
	Kind       string        `json:"kind"`
	Plugins    []IndexPlugin `json:"plugins"`
}

// IndexPlugin is one plugin in an index.
type IndexPlugin struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Publisher   string         `json:"publisher,omitempty"`
	Homepage    string         `json:"homepage,omitempty"`
	Versions    []IndexVersion `json:"versions"`
}

// IndexVersion is one released version of a plugin.
type IndexVersion struct {
	Version       string `json:"version"`
	URL           string `json:"url"`
	SHA256        string `json:"sha256"`
	MinTugVersion string `json:"minTugVersion,omitempty"`
}

// Latest returns the highest version, or false when there is none.
func (p IndexPlugin) Latest() (IndexVersion, bool) {
	if len(p.Versions) == 0 {
		return IndexVersion{}, false
	}
	vs := slices.Clone(p.Versions)
	slices.SortFunc(vs, func(a, b IndexVersion) int { return compareVersions(b.Version, a.Version) })
	return vs[0], true
}

// Version returns the named version.
func (p IndexPlugin) Version(v string) (IndexVersion, bool) {
	i := slices.IndexFunc(p.Versions, func(x IndexVersion) bool { return strings.TrimPrefix(x.Version, "v") == strings.TrimPrefix(v, "v") })
	if i < 0 {
		return IndexVersion{}, false
	}
	return p.Versions[i], true
}

// Validate checks the index's shape.
func (ix *Index) Validate() error {
	var errs []error
	if ix.APIVersion != IndexAPIVersion || ix.Kind != IndexKind {
		errs = append(errs, fmt.Errorf("apiVersion and kind must be %s and %s", IndexAPIVersion, IndexKind))
	}
	seen := map[string]bool{}
	for _, p := range ix.Plugins {
		if !validName.MatchString(p.Name) {
			errs = append(errs, fmt.Errorf("plugin name %q is not valid", p.Name))
		}
		if seen[p.Name] {
			errs = append(errs, fmt.Errorf("plugin %s is listed twice", p.Name))
		}
		seen[p.Name] = true
		for _, v := range p.Versions {
			u, err := url.Parse(v.URL)
			secure := err == nil && (u.Scheme == "https" || u.Scheme == "http" && isLoopback(u.Hostname()))
			if v.Version == "" || !secure || len(v.SHA256) != 64 {
				errs = append(errs, fmt.Errorf("plugin %s version %q needs a version, an https url and a sha256", p.Name, v.Version))
			}
		}
	}
	return errors.Join(errs...)
}

// ParseIndex reads an index after checking its signature against key.
func ParseIndex(data, sig []byte, key ed25519.PublicKey) (*Index, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sig)))
	if err != nil || len(raw) != ed25519.SignatureSize {
		return nil, errors.New("the index signature is not a base64 ed25519 signature")
	}
	if !ed25519.Verify(key, data, raw) {
		return nil, errors.New("the index signature does not match its key; it may have been changed")
	}
	ix := &Index{}
	if err := yaml.UnmarshalStrict(data, ix); err != nil {
		return nil, fmt.Errorf("reading the index: %w", err)
	}
	if err := ix.Validate(); err != nil {
		return nil, err
	}
	return ix, nil
}

// ParsePublicKey reads a base64 ed25519 public key.
func ParsePublicKey(s string) (ed25519.PublicKey, error) {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, errors.New("the index public key must be a base64 ed25519 key, as tug plugin index keygen prints")
	}
	return ed25519.PublicKey(b), nil
}

// NewIndexKey returns a new signing key and its public half, both base64.
func NewIndexKey() (private, public string, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	return base64.StdEncoding.EncodeToString(priv), base64.StdEncoding.EncodeToString(pub), nil
}

// SignIndex signs data with a base64 private key from NewIndexKey, after
// checking that it is a valid index.
func SignIndex(data []byte, privateKey string) ([]byte, error) {
	ix := &Index{}
	if err := yaml.UnmarshalStrict(data, ix); err != nil {
		return nil, fmt.Errorf("reading the index: %w", err)
	}
	if err := ix.Validate(); err != nil {
		return nil, err
	}
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(privateKey))
	if err != nil || len(b) != ed25519.PrivateKeySize {
		return nil, errors.New("the signing key is not a base64 ed25519 private key")
	}
	sig := ed25519.Sign(ed25519.PrivateKey(b), data)
	return []byte(base64.StdEncoding.EncodeToString(sig) + "\n"), nil
}

// AddToIndex records a packaged archive, published at url, in the index
// file at path, creating the file when it is missing. It reads the
// archive's manifest and hashes it, so the entry matches what install
// will check.
func AddToIndex(path, archive, archiveURL, description string) (*IndexPlugin, error) {
	ix := &Index{APIVersion: IndexAPIVersion, Kind: IndexKind}
	if b, err := os.ReadFile(path); err == nil {
		if err := yaml.UnmarshalStrict(b, ix); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	sum, err := fileSHA256(archive)
	if err != nil {
		return nil, err
	}
	tmp, err := os.MkdirTemp("", "tug-index-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	if err := extract(archive, tmp); err != nil {
		return nil, err
	}
	root, err := pluginRoot(tmp)
	if err != nil {
		return nil, err
	}
	m, err := LoadManifest(root)
	if err != nil {
		return nil, err
	}
	v := IndexVersion{Version: m.Metadata.Version, URL: archiveURL, SHA256: sum, MinTugVersion: m.Spec.MinTugVersion}
	i := slices.IndexFunc(ix.Plugins, func(p IndexPlugin) bool { return p.Name == m.Metadata.Name })
	if i < 0 {
		ix.Plugins = append(ix.Plugins, IndexPlugin{Name: m.Metadata.Name})
		i = len(ix.Plugins) - 1
	}
	p := &ix.Plugins[i]
	p.Publisher, p.Homepage = m.Metadata.Publisher, m.Metadata.Homepage
	if description != "" {
		p.Description = description
	}
	if j := slices.IndexFunc(p.Versions, func(x IndexVersion) bool { return x.Version == v.Version }); j >= 0 {
		p.Versions[j] = v
	} else {
		p.Versions = append(p.Versions, v)
	}
	slices.SortFunc(ix.Plugins, func(a, b IndexPlugin) int { return strings.Compare(a.Name, b.Name) })
	if err := ix.Validate(); err != nil {
		return nil, err
	}
	b, err := yaml.Marshal(ix)
	if err != nil {
		return nil, err
	}
	// The index is published, so it is readable like other release files.
	if err := config.WriteFileAtomic(path, b, 0o644); err != nil {
		return nil, err
	}
	added := ix.Plugins[slices.IndexFunc(ix.Plugins, func(p IndexPlugin) bool { return p.Name == m.Metadata.Name })]
	return &added, nil
}

// IndexSource is one configured index: where it is and the key it must be
// signed with.
type IndexSource struct {
	Name      string
	URL       string
	PublicKey ed25519.PublicKey
}

// IndexCache downloads indexes and keeps verified copies for an hour.
type IndexCache struct {
	Dir  string
	HTTP *http.Client
	Now  func() time.Time
}

// Load returns the index from src, from the cache when it is fresh. The
// signature is checked on every load, cached or not.
func (c *IndexCache) Load(ctx context.Context, src IndexSource) (*Index, error) {
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	dataPath := filepath.Join(c.Dir, src.Name+".yaml")
	sigPath := dataPath + SignatureSuffix
	if st, err := os.Stat(dataPath); err == nil && now().Sub(st.ModTime()) < indexTTL {
		data, err1 := os.ReadFile(dataPath)
		sig, err2 := os.ReadFile(sigPath)
		if err1 == nil && err2 == nil {
			if ix, err := ParseIndex(data, sig, src.PublicKey); err == nil {
				return ix, nil
			}
		}
	}
	data, err := c.get(ctx, src.URL)
	if err != nil {
		return nil, fmt.Errorf("index %s: %w", src.Name, err)
	}
	sig, err := c.get(ctx, src.URL+SignatureSuffix)
	if err != nil {
		return nil, fmt.Errorf("index %s signature: %w", src.Name, err)
	}
	ix, err := ParseIndex(data, sig, src.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("index %s: %w", src.Name, err)
	}
	if err := os.MkdirAll(c.Dir, 0o700); err == nil {
		_ = config.WriteFileAtomic(dataPath, data, 0o600) // a cache; a failed write only costs a download
		_ = config.WriteFileAtomic(sigPath, sig, 0o600)
	}
	return ix, nil
}

func (c *IndexCache) get(ctx context.Context, rawURL string) ([]byte, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "https" && (u.Scheme != "http" || !isLoopback(u.Hostname())) {
		return nil, fmt.Errorf("refusing %s: indexes are downloaded over https only", rawURL)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("downloading %s: %s", rawURL, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxIndexBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxIndexBytes {
		return nil, fmt.Errorf("%s is larger than %d MiB", rawURL, maxIndexBytes>>20)
	}
	return b, nil
}

// Match is a plugin found in an index.
type Match struct {
	Index  string
	Plugin IndexPlugin
}

// Search finds plugins whose name, description or publisher contains query
// (all of them for an empty query) in every index. An index that cannot be
// loaded is reported in errs; the others still answer.
func (c *IndexCache) Search(ctx context.Context, srcs []IndexSource, query string) (matches []Match, errs []error) {
	q := strings.ToLower(query)
	for _, src := range srcs {
		ix, err := c.Load(ctx, src)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, p := range ix.Plugins {
			hay := strings.ToLower(p.Name + " " + p.Description + " " + p.Publisher)
			if strings.Contains(hay, q) {
				matches = append(matches, Match{Index: src.Name, Plugin: p})
			}
		}
	}
	return matches, errs
}
