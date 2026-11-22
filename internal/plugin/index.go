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
	"github.com/paingha/tugcli/internal/sigstore"
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
	// maxBundleBytes bounds a downloaded Sigstore bundle, which is a few
	// KiB.
	maxBundleBytes = 1 << 20
)

// Index is a list of reviewed plugins and where to get them. It is signed
// with ed25519 as a whole, and each version pins its archive's sha256, so a
// plugin installed from it is checked from the index key down to every
// binary. A version may also name a Sigstore signature over its archive and
// who must have made it, which ties the archive to the publisher's own
// release workflow as well as to the index.
//
// An index can also withdraw archives it listed, in Revoked, and name the
// keys it is no longer signed with, in SupersededKeys. A tug older than
// these fields refuses an index that has them, rather than installing a
// revoked plugin.
type Index struct {
	APIVersion string        `json:"apiVersion"`
	Kind       string        `json:"kind"`
	Plugins    []IndexPlugin `json:"plugins"`
	// Revoked lists archives that must not be installed.
	Revoked []Revocation `json:"revoked,omitempty"`
	// SupersededKeys lists keys the index was signed with before, each
	// with the key that took its place, if one did.
	SupersededKeys []SupersededKey `json:"supersededKeys,omitempty"`

	// signer is the key the signature was checked with.
	signer ed25519.PublicKey
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
	// Sigstore, when set, is the archive's Sigstore signature, which
	// tug plugin install checks before the sha256.
	Sigstore *IndexSigstore `json:"sigstore,omitempty"`
}

// IndexSigstore says where a version's Sigstore bundle is and who must have
// signed the archive.
type IndexSigstore struct {
	// Bundle is the https URL of the Sigstore bundle, as cosign sign-blob
	// --bundle writes it.
	Bundle string `json:"bundle"`
	// Identity is the signing certificate's identity. For a GitHub Actions
	// workflow it is the workflow's URL and ref:
	// https://github.com/acme/tug-provider-acme/.github/workflows/release.yml@refs/tags/v0.1.0.
	Identity string `json:"identity,omitempty"`
	// IdentityRegexp matches the whole identity instead of Identity.
	IdentityRegexp string `json:"identityRegexp,omitempty"`
	// Issuer is the OIDC issuer that vouched for the identity, such as
	// https://token.actions.githubusercontent.com for GitHub Actions.
	Issuer string `json:"issuer"`
}

// Signer is the identity the bundle's certificate must name.
func (s *IndexSigstore) Signer() sigstore.Identity {
	return sigstore.Identity{Subject: s.Identity, SubjectRegexp: s.IdentityRegexp, Issuer: s.Issuer}
}

// Validate checks that the bundle URL is https and the signer is complete.
func (s *IndexSigstore) Validate() error {
	var errs []error
	if !secureURL(s.Bundle) {
		errs = append(errs, errors.New("an https url for the Sigstore bundle is needed"))
	}
	if err := s.Signer().Validate(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// Latest returns the highest version, or false when there is none.
func (p IndexPlugin) Latest() (IndexVersion, bool) {
	return p.LatestFunc(func(IndexVersion) bool { return true })
}

// LatestFunc returns the highest version for which ok is true, or false
// when there is none.
func (p IndexPlugin) LatestFunc(ok func(IndexVersion) bool) (IndexVersion, bool) {
	vs := slices.DeleteFunc(slices.Clone(p.Versions), func(v IndexVersion) bool { return !ok(v) })
	if len(vs) == 0 {
		return IndexVersion{}, false
	}
	slices.SortFunc(vs, func(a, b IndexVersion) int { return compareVersions(b.Version, a.Version) })
	return vs[0], true
}

// Version returns the named version.
func (p IndexPlugin) Version(v string) (IndexVersion, bool) {
	i := slices.IndexFunc(p.Versions, func(x IndexVersion) bool { return sameVersion(x.Version, v) })
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
			if v.Version == "" || !secureURL(v.URL) || len(v.SHA256) != 64 {
				errs = append(errs, fmt.Errorf("plugin %s version %q needs a version, an https url and a sha256", p.Name, v.Version))
			} else if !validPluginVersion.MatchString(v.Version) {
				errs = append(errs, fmt.Errorf("plugin %s version %q is not a version such as 0.1.0", p.Name, v.Version))
			}
			if v.Sigstore != nil {
				if err := v.Sigstore.Validate(); err != nil {
					errs = append(errs, fmt.Errorf("plugin %s version %q sigstore: %w", p.Name, v.Version, err))
				}
			}
		}
	}
	for _, r := range ix.Revoked {
		if err := r.validate(); err != nil {
			errs = append(errs, err)
		}
	}
	for _, k := range ix.SupersededKeys {
		if err := k.validate(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// secureURL reports whether rawURL is https, or plain http to a loopback
// address for local testing.
func secureURL(rawURL string) bool {
	u, err := url.Parse(rawURL)
	return err == nil && u.Host != "" && (u.Scheme == "https" || u.Scheme == "http" && isLoopback(u.Hostname()))
}

// ParseIndex reads an index after checking its signature against key. When
// key has been superseded, the signature may instead be by a successor that
// key handed over to, through the index's supersededKeys; SignedBy then
// returns that successor, which the caller should trust from then on. A
// key the index lists as superseded never signs it.
func ParseIndex(data, sig []byte, key ed25519.PublicKey) (*Index, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sig)))
	if err != nil || len(raw) != ed25519.SignatureSize {
		return nil, errors.New("the index signature is not a base64 ed25519 signature")
	}
	// The handovers come from the index itself, so it is read before its
	// signature is checked; nothing in it is used until one has been.
	ix := &Index{}
	if err := yaml.UnmarshalStrict(data, ix); err != nil {
		if ed25519.Verify(key, data, raw) {
			return nil, fmt.Errorf("reading the index: %w", err)
		}
		return nil, errors.New("the index signature does not match its key; it may have been changed")
	}
	chain := ix.successors(key)
	i := slices.IndexFunc(chain, func(k ed25519.PublicKey) bool { return ed25519.Verify(k, data, raw) })
	if i < 0 {
		return nil, errors.New("the index signature does not match its key; it may have been changed, or the publisher may have changed keys without handing over to the new one")
	}
	ix.signer = chain[i]
	if err := ix.Validate(); err != nil {
		return nil, err
	}
	if ix.superseded(ix.signer) {
		return nil, errors.New("the index is signed with a key it lists as superseded")
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
// checking that it is a valid index that the key has not been superseded
// in.
func SignIndex(data []byte, privateKey string) ([]byte, error) {
	ix := &Index{}
	if err := yaml.UnmarshalStrict(data, ix); err != nil {
		return nil, fmt.Errorf("reading the index: %w", err)
	}
	if err := ix.Validate(); err != nil {
		return nil, err
	}
	key, err := parsePrivateKey(privateKey)
	if err != nil {
		return nil, err
	}
	if ix.superseded(key.Public().(ed25519.PublicKey)) {
		return nil, errors.New("the index lists this key as superseded; sign it with the key that replaced it")
	}
	sig := ed25519.Sign(key, data)
	return []byte(base64.StdEncoding.EncodeToString(sig) + "\n"), nil
}

// IndexAddition is what AddToIndex records about an archive besides what it
// reads from the archive itself.
type IndexAddition struct {
	// URL is where the archive is published.
	URL string
	// Description, when set, replaces the plugin's description.
	Description string
	// Sigstore, when set, names the archive's Sigstore bundle and signer.
	Sigstore *IndexSigstore
}

// AddToIndex records a packaged archive in the index file at path, creating
// the file when it is missing, and returns the plugin and the version it
// added. It reads the archive's manifest and hashes it, so the entry
// matches what install will check.
func AddToIndex(path, archive string, add IndexAddition) (*IndexPlugin, IndexVersion, error) {
	ix, err := readIndexFile(path)
	if err != nil {
		return nil, IndexVersion{}, err
	}
	sum, err := fileSHA256(archive)
	if err != nil {
		return nil, IndexVersion{}, err
	}
	tmp, err := os.MkdirTemp("", "tug-index-")
	if err != nil {
		return nil, IndexVersion{}, err
	}
	defer os.RemoveAll(tmp)
	if err := extract(archive, tmp); err != nil {
		return nil, IndexVersion{}, err
	}
	root, err := pluginRoot(tmp)
	if err != nil {
		return nil, IndexVersion{}, err
	}
	m, err := LoadManifest(root)
	if err != nil {
		return nil, IndexVersion{}, err
	}
	v := IndexVersion{Version: m.Metadata.Version, URL: add.URL, SHA256: sum, MinTugVersion: m.Spec.MinTugVersion, Sigstore: add.Sigstore}
	i := slices.IndexFunc(ix.Plugins, func(p IndexPlugin) bool { return p.Name == m.Metadata.Name })
	if i < 0 {
		ix.Plugins = append(ix.Plugins, IndexPlugin{Name: m.Metadata.Name})
		i = len(ix.Plugins) - 1
	}
	p := &ix.Plugins[i]
	p.Publisher, p.Homepage = m.Metadata.Publisher, m.Metadata.Homepage
	if add.Description != "" {
		p.Description = add.Description
	}
	if j := slices.IndexFunc(p.Versions, func(x IndexVersion) bool { return x.Version == v.Version }); j >= 0 {
		p.Versions[j] = v
	} else {
		p.Versions = append(p.Versions, v)
	}
	slices.SortFunc(ix.Plugins, func(a, b IndexPlugin) int { return strings.Compare(a.Name, b.Name) })
	if err := writeIndexFile(path, ix); err != nil {
		return nil, IndexVersion{}, err
	}
	added := ix.Plugins[slices.IndexFunc(ix.Plugins, func(p IndexPlugin) bool { return p.Name == m.Metadata.Name })]
	return &added, v, nil
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
	// KeysFile, when set, records each index's newest key once the index
	// has handed over to it from the key in config.yaml. Later loads start
	// from the recorded key, so an index signed with the old one, which
	// may have leaked, is refused from then on.
	KeysFile string
}

// Load returns the index from src, from the cache when it is fresh. The
// signature is checked on every load, cached or not.
func (c *IndexCache) Load(ctx context.Context, src IndexSource) (*Index, error) {
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	key := c.trustedKey(src)
	dataPath := filepath.Join(c.Dir, src.Name+".yaml")
	sigPath := dataPath + SignatureSuffix
	if st, err := os.Stat(dataPath); err == nil && now().Sub(st.ModTime()) < indexTTL {
		data, err1 := os.ReadFile(dataPath)
		sig, err2 := os.ReadFile(sigPath)
		if err1 == nil && err2 == nil {
			if ix, err := ParseIndex(data, sig, key); err == nil {
				return ix, c.recordKey(src, key, ix)
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
	ix, err := ParseIndex(data, sig, key)
	if err != nil {
		return nil, fmt.Errorf("index %s: %w", src.Name, err)
	}
	if err := os.MkdirAll(c.Dir, 0o700); err == nil {
		_ = config.WriteFileAtomic(dataPath, data, 0o600) // a cache; a failed write only costs a download
		_ = config.WriteFileAtomic(sigPath, sig, 0o600)
	}
	return ix, c.recordKey(src, key, ix)
}

// keyRecord is what KeysFile holds for one index: the key config.yaml
// names, and the key tug has moved to from it.
type keyRecord struct {
	From string `json:"from"`
	To   string `json:"to"`
}

func (c *IndexCache) keyRecords() map[string]keyRecord {
	records := map[string]keyRecord{}
	if c.KeysFile == "" {
		return records
	}
	if b, err := os.ReadFile(c.KeysFile); err == nil {
		_ = yaml.Unmarshal(b, &records) // an unreadable file holds no records
	}
	return records
}

// trustedKey returns the key to check src's index with: the one tug moved
// to from src's key, or src's key.
func (c *IndexCache) trustedKey(src IndexSource) ed25519.PublicKey {
	rec, ok := c.keyRecords()[src.Name]
	if !ok || rec.From != base64.StdEncoding.EncodeToString(src.PublicKey) {
		return src.PublicKey
	}
	if key, err := ParsePublicKey(rec.To); err == nil {
		return key
	}
	return src.PublicKey
}

// recordKey records the key ix was signed with, when it is not the key the
// load started from.
func (c *IndexCache) recordKey(src IndexSource, started ed25519.PublicKey, ix *Index) error {
	if c.KeysFile == "" || ix.signer.Equal(started) {
		return nil
	}
	records := c.keyRecords()
	records[src.Name] = keyRecord{From: base64.StdEncoding.EncodeToString(src.PublicKey), To: base64.StdEncoding.EncodeToString(ix.signer)}
	b, err := yaml.Marshal(records)
	if err != nil {
		return err
	}
	if err := config.WriteFileAtomic(c.KeysFile, b, 0o600); err != nil {
		return fmt.Errorf("index %s: recording its new key: %w", src.Name, err)
	}
	return nil
}

// Loaded is an index that loaded, and where it came from.
type Loaded struct {
	Source IndexSource
	Index  *Index
}

// KeyChanged reports whether the index is now trusted under a key other
// than the one config.yaml names, and returns that key, base64.
func (l Loaded) KeyChanged() (string, bool) {
	if l.Index.signer.Equal(l.Source.PublicKey) {
		return "", false
	}
	return base64.StdEncoding.EncodeToString(l.Index.signer), true
}

// LoadAll loads every index. An index that cannot be loaded is reported in
// errs; the others still load.
func (c *IndexCache) LoadAll(ctx context.Context, srcs []IndexSource) (loaded []Loaded, errs []error) {
	for _, src := range srcs {
		ix, err := c.Load(ctx, src)
		if err != nil {
			errs = append(errs, err)
		}
		if ix != nil {
			loaded = append(loaded, Loaded{Source: src, Index: ix})
		}
	}
	return loaded, errs
}

// FindRevoked returns the revocation of the archive with this sha256 in any
// of the indexes, and the index that revoked it.
func FindRevoked(loaded []Loaded, sha256 string) (Revocation, string, bool) {
	for _, l := range loaded {
		if r, ok := l.Index.RevocationFor(sha256); ok {
			return r, l.Source.Name, true
		}
	}
	return Revocation{}, "", false
}

// FindRevokedVersion returns a revocation that names this plugin and
// version in any of the indexes, and the index that revoked it.
func FindRevokedVersion(loaded []Loaded, name, version string) (Revocation, string, bool) {
	for _, l := range loaded {
		if r, ok := l.Index.RevocationForVersion(name, version); ok {
			return r, l.Source.Name, true
		}
	}
	return Revocation{}, "", false
}

func (c *IndexCache) get(ctx context.Context, rawURL string) ([]byte, error) {
	return fetchSmall(ctx, c.HTTP, rawURL, "indexes", maxIndexBytes)
}

// FetchBundle downloads a Sigstore bundle over HTTPS. Plain HTTP is accepted
// only for loopback hosts, for local testing.
func FetchBundle(ctx context.Context, rawURL string) ([]byte, error) {
	return fetchSmall(ctx, nil, rawURL, "Sigstore bundles", maxBundleBytes)
}

// fetchSmall downloads a file of at most limit bytes into memory. what names
// such files in the error for a URL that is not https.
func fetchSmall(ctx context.Context, client *http.Client, rawURL, what string, limit int) ([]byte, error) {
	if !secureURL(rawURL) {
		return nil, fmt.Errorf("refusing %s: %s are downloaded over https only", rawURL, what)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
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
	b, err := io.ReadAll(io.LimitReader(resp.Body, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if len(b) > limit {
		return nil, fmt.Errorf("%s is larger than %d MiB", rawURL, limit>>20)
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
	loaded, errs := c.LoadAll(ctx, srcs)
	return Search(loaded, query), errs
}

// Search finds plugins whose name, description or publisher contains query
// (all of them for an empty query) in the loaded indexes.
func Search(loaded []Loaded, query string) []Match {
	q := strings.ToLower(query)
	var matches []Match
	for _, l := range loaded {
		for _, p := range l.Index.Plugins {
			hay := strings.ToLower(p.Name + " " + p.Description + " " + p.Publisher)
			if strings.Contains(hay, q) {
				matches = append(matches, Match{Index: l.Source.Name, Plugin: p})
			}
		}
	}
	return matches
}
