// Package team reads team catalogs: signed documents in which a team
// publishes names, aliases and labels for its clusters, provider account
// settings, approved plugin indexes and safety policies. A team catalog
// never carries credentials, and following one grants no Kubernetes
// permission: each person still lists and reaches clusters as themselves,
// through their own provider and OIDC identities.
//
// The package fetches catalogs over https or from a git repository, checks
// their ed25519 signatures and versions, keeps the last good version of
// each for offline use, merges them into the local catalog with provenance
// and turns their policies into guard limits.
package team

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/paingha/tugcli/internal/catalog"
	"github.com/paingha/tugcli/internal/config"
	"github.com/paingha/tugcli/internal/guard"
)

// Catalog API version and kind.
const (
	APIVersion = "tug.team/v1alpha1"
	Kind       = "TeamCatalog"
	// SignatureSuffix names a catalog's detached signature: team.yaml.sig.
	SignatureSuffix = ".sig"
	// DefaultGitPath is the catalog's path in a git repository when the
	// configuration names none.
	DefaultGitPath = "team.yaml"
	// maxCatalogBytes bounds a downloaded catalog or signature.
	maxCatalogBytes = 4 << 20
)

// ErrSignature is returned for a catalog whose signature does not match the
// team's key.
var ErrSignature = errors.New("the team catalog's signature does not match the team's key; it may have been changed")

// Document is a team catalog. It is signed with ed25519 as a whole.
type Document struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	// Version goes up with every change the team publishes. tug refuses a
	// version lower than the one it has accepted, so an old catalog cannot
	// be served again in place of a newer one.
	Version     int64  `json:"version"`
	Description string `json:"description,omitempty"`
	// Accounts are provider accounts to import clusters from. Each person
	// lists them with their own credentials.
	Accounts []Account `json:"accounts,omitempty"`
	// Clusters give names, aliases and labels to clusters people import.
	Clusters []Cluster `json:"clusters,omitempty"`
	// PluginIndexes are signed plugin indexes the team approves.
	PluginIndexes []config.PluginIndex `json:"pluginIndexes,omitempty"`
	// Policies set read-only defaults and limits by label.
	Policies []Policy `json:"policies,omitempty"`
}

// Account is a provider account and its settings, as tug import takes
// them with --account and --set.
type Account struct {
	Provider string         `json:"provider"`
	Account  string         `json:"account"`
	Config   map[string]any `json:"config,omitempty"`
}

// Key identifies the account as catalog sources do: provider/account.
func (a Account) Key() string { return a.Provider + "/" + a.Account }

// Cluster is what the team says about one cluster.
type Cluster struct {
	// Match picks the cluster out of each person's catalog.
	Match Match `json:"match"`
	// Name is the name everyone uses for the cluster.
	Name    string   `json:"name,omitempty"`
	Aliases []string `json:"aliases,omitempty"`
	// Env sets the env label, which read-only selectors and policies
	// usually match on.
	Env    string            `json:"env,omitempty"`
	Labels map[string]string `json:"labels,omitempty"`
}

// Match picks clusters by what every person's catalog has in common:
// what the provider calls the cluster and where its API server is. Every
// field that is set must match, and providerID or server must be set.
type Match struct {
	Provider string `json:"provider,omitempty"`
	// Account is the local account name, which can differ from person
	// to person; leave it out unless it is the same for everyone.
	Account    string `json:"account,omitempty"`
	ProviderID string `json:"providerID,omitempty"`
	Server     string `json:"server,omitempty"`
}

// Matches reports whether cl is the cluster m describes.
func (m Match) Matches(cl *catalog.Cluster) bool {
	switch {
	case m.Provider != "" && cl.Provider != m.Provider,
		m.Account != "" && cl.Account != m.Account,
		m.ProviderID != "" && cl.ProviderID != m.ProviderID:
		return false
	case m.Server != "":
		want := normaliseServer(m.Server)
		return normaliseServer(cl.Server) == want || cl.PrivateServer != "" && normaliseServer(cl.PrivateServer) == want
	}
	return true
}

func (m Match) String() string {
	var parts []string
	for _, kv := range [][2]string{{"provider", m.Provider}, {"account", m.Account}, {"providerID", m.ProviderID}, {"server", m.Server}} {
		if kv[1] != "" {
			parts = append(parts, kv[0]+"="+kv[1])
		}
	}
	return strings.Join(parts, " ")
}

func normaliseServer(s string) string {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil || u.Host == "" {
		return strings.TrimSuffix(strings.ToLower(s), "/")
	}
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host) + strings.TrimSuffix(u.Path, "/")
}

// labels returns the labels the entry sets, with Env as the env label.
func (c Cluster) labels() map[string]string {
	out := make(map[string]string, len(c.Labels)+1)
	for k, v := range c.Labels {
		k = strings.ToLower(k)
		if k == catalog.LabelEnv {
			v = catalog.NormaliseEnv(v)
		}
		out[k] = v
	}
	if c.Env != "" {
		out[catalog.LabelEnv] = catalog.NormaliseEnv(c.Env)
	}
	return out
}

// Policy is a safety policy: clusters whose labels match Selector open
// read-only when ReadOnly is set, and an unlock on them lasts at most
// MaxUnlock. Policies only make tug's guard stricter; they grant nothing.
type Policy struct {
	Name        string            `json:"name"`
	Description string            `json:"description,omitempty"`
	Selector    map[string]string `json:"selector"`
	ReadOnly    bool              `json:"readOnly,omitempty"`
	// MaxUnlock is the longest tug unlock allowed. It needs ReadOnly, and
	// it also refuses tug connect --read-write, an unlock without an end.
	MaxUnlock config.Duration `json:"maxUnlock,omitzero"`
}

// Summary describes the policy in a few words.
func (p Policy) Summary() string {
	mode := "read-write"
	if p.ReadOnly {
		mode = "read-only"
	}
	s := selectorString(p.Selector) + ": " + mode
	if p.MaxUnlock.Duration > 0 {
		s += ", unlocks up to " + guard.ShortDuration(p.MaxUnlock.Duration)
	}
	return s
}

var validName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// ValidName reports whether name can name a team, a policy or a plugin
// index in a team catalog: lowercase letters, digits and dashes.
func ValidName(name string) bool { return validName.MatchString(name) }

// validClusterName allows what cluster names usually hold, but not a slash,
// which catalog IDs use, or spaces.
var validClusterName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@:-]{0,252}$`)

// secretWords are parts of setting names that hold credentials, which a
// team catalog must not carry. Names are compared lowercase without
// dashes, underscores or dots.
var secretWords = []string{"token", "secret", "password", "passwd", "credential", "apikey", "accesskey", "privatekey"}

func secretLike(key string) bool {
	k := strings.NewReplacer("-", "", "_", "", ".", "").Replace(strings.ToLower(key))
	return slices.ContainsFunc(secretWords, func(w string) bool { return strings.Contains(k, w) })
}

// secretKeys returns the setting names in v, at any depth, that look like
// they hold credentials.
func secretKeys(prefix string, v any) []string {
	var out []string
	switch v := v.(type) {
	case map[string]any:
		for k, sub := range v {
			name := k
			if prefix != "" {
				name = prefix + "." + k
			}
			if secretLike(k) {
				out = append(out, name)
			}
			out = append(out, secretKeys(name, sub)...)
		}
	case []any:
		for _, sub := range v {
			out = append(out, secretKeys(prefix, sub)...)
		}
	}
	slices.Sort(out)
	return out
}

// Validate checks the document's shape, and that it carries no
// credentials.
func (d *Document) Validate() error {
	var errs []error
	if d.APIVersion != APIVersion || d.Kind != Kind {
		errs = append(errs, fmt.Errorf("apiVersion and kind must be %s and %s", APIVersion, Kind))
	}
	if d.Version < 1 {
		errs = append(errs, errors.New("version must be a whole number from 1 up, raised with every change"))
	}
	accounts := map[string]bool{}
	for _, a := range d.Accounts {
		if a.Provider == "" || a.Account == "" {
			errs = append(errs, errors.New("every account needs a provider and an account"))
			continue
		}
		if accounts[a.Key()] {
			errs = append(errs, fmt.Errorf("account %s is listed twice", a.Key()))
		}
		accounts[a.Key()] = true
		if keys := secretKeys("", a.Config); len(keys) > 0 {
			errs = append(errs, fmt.Errorf("account %s: %s looks like a credential, which a team catalog must not carry; each person signs in as themselves",
				a.Key(), strings.Join(keys, ", ")))
		}
	}
	names := map[string]bool{}
	claim := func(what, n string) {
		if !validClusterName.MatchString(n) {
			errs = append(errs, fmt.Errorf("cluster %s %q is not valid", what, n))
			return
		}
		if names[n] {
			errs = append(errs, fmt.Errorf("cluster name or alias %s is used twice", n))
		}
		names[n] = true
	}
	for i, c := range d.Clusters {
		if c.Match.ProviderID == "" && c.Match.Server == "" {
			errs = append(errs, fmt.Errorf("cluster %d (%s): match needs a providerID or a server", i+1, c.Name))
		}
		if hasPassword(c.Match.Server) {
			errs = append(errs, fmt.Errorf("cluster %d (%s): the server carries a password, which a team catalog must not", i+1, c.Name))
		}
		if c.Name != "" {
			claim("name", c.Name)
		}
		for _, a := range c.Aliases {
			claim("alias", a)
		}
		for k, v := range c.Labels {
			switch lk := strings.ToLower(k); {
			case lk == "" || v == "":
				errs = append(errs, fmt.Errorf("cluster %s: labels need a key and a value", c.Name))
			case lk == catalog.LabelProvider || lk == catalog.LabelAccount:
				errs = append(errs, fmt.Errorf("cluster %s: the %s label comes from where the cluster is imported and cannot be set", c.Name, lk))
			case lk == catalog.LabelEnv && c.Env != "" && catalog.NormaliseEnv(v) != catalog.NormaliseEnv(c.Env):
				errs = append(errs, fmt.Errorf("cluster %s: env %s and label env=%s disagree", c.Name, c.Env, v))
			}
		}
	}
	indexes := map[string]bool{}
	for _, ix := range d.PluginIndexes {
		if !ValidName(ix.Name) {
			errs = append(errs, fmt.Errorf("plugin index name %q is not valid", ix.Name))
		}
		if indexes[ix.Name] {
			errs = append(errs, fmt.Errorf("plugin index %s is listed twice", ix.Name))
		}
		indexes[ix.Name] = true
		switch {
		case !secureURL(ix.URL):
			errs = append(errs, fmt.Errorf("plugin index %s needs an https url", ix.Name))
		case hasPassword(ix.URL):
			errs = append(errs, fmt.Errorf("plugin index %s: its url carries a password, which a team catalog must not", ix.Name))
		}
		if _, err := ParsePublicKey(ix.PublicKey); err != nil {
			errs = append(errs, fmt.Errorf("plugin index %s: %w", ix.Name, err))
		}
	}
	policies := map[string]bool{}
	for _, p := range d.Policies {
		if !ValidName(p.Name) {
			errs = append(errs, fmt.Errorf("policy name %q is not valid", p.Name))
		}
		if policies[p.Name] {
			errs = append(errs, fmt.Errorf("policy %s is listed twice", p.Name))
		}
		policies[p.Name] = true
		if len(p.Selector) == 0 || slices.Contains(mapsKeys(p.Selector), "") {
			errs = append(errs, fmt.Errorf("policy %s needs a selector of label=value pairs", p.Name))
		}
		switch {
		case p.MaxUnlock.Duration < 0:
			errs = append(errs, fmt.Errorf("policy %s: maxUnlock cannot be negative", p.Name))
		case p.MaxUnlock.Duration > 0 && !p.ReadOnly:
			errs = append(errs, fmt.Errorf("policy %s: maxUnlock limits unlocks of read-only clusters, so it needs readOnly: true", p.Name))
		}
	}
	return errors.Join(errs...)
}

func mapsKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func selectorString(sel map[string]string) string {
	parts := make([]string, 0, len(sel))
	for _, k := range mapsKeys(sel) {
		parts = append(parts, k+"="+sel[k])
	}
	return strings.Join(parts, ",")
}

// Parse reads a catalog after checking its signature against key.
func Parse(data, sig []byte, key ed25519.PublicKey) (*Document, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sig)))
	if err != nil || len(raw) != ed25519.SignatureSize {
		return nil, errors.New("the team catalog's signature is not a base64 ed25519 signature")
	}
	if !ed25519.Verify(key, data, raw) {
		return nil, ErrSignature
	}
	return parseUnsigned(data)
}

func parseUnsigned(data []byte) (*Document, error) {
	d := &Document{}
	if err := yaml.UnmarshalStrict(data, d); err != nil {
		return nil, fmt.Errorf("reading the team catalog: %w", err)
	}
	if err := d.Validate(); err != nil {
		return nil, err
	}
	return d, nil
}

// ParsePublicKey reads a base64 ed25519 public key.
func ParsePublicKey(s string) (ed25519.PublicKey, error) {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, errors.New("the public key must be a base64 ed25519 key, as tug team keygen prints")
	}
	return ed25519.PublicKey(b), nil
}

// Fingerprint is a short form of a public key to show people:
// sha256: and the first 16 hex digits of the key's sha256.
func Fingerprint(key ed25519.PublicKey) string {
	sum := sha256.Sum256(key)
	return "sha256:" + hex.EncodeToString(sum[:8])
}

// NewKey returns a new signing key and its public half, both base64.
func NewKey() (private, public string, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	return base64.StdEncoding.EncodeToString(priv), base64.StdEncoding.EncodeToString(pub), nil
}

// Sign signs data with a base64 private key from NewKey, after checking
// that it is a valid team catalog.
func Sign(data []byte, privateKey string) ([]byte, error) {
	if _, err := parseUnsigned(data); err != nil {
		return nil, err
	}
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(privateKey))
	if err != nil || len(b) != ed25519.PrivateKeySize {
		return nil, errors.New("the signing key is not a base64 ed25519 private key")
	}
	sig := ed25519.Sign(ed25519.PrivateKey(b), data)
	return []byte(base64.StdEncoding.EncodeToString(sig) + "\n"), nil
}

// hasPassword reports whether rawURL carries a password, which tug never
// keeps in a team catalog or its settings.
func hasPassword(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil || u.User == nil {
		return false
	}
	_, ok := u.User.Password()
	return ok
}

// secureURL reports whether rawURL is https, or plain http to a loopback
// address for local testing.
func secureURL(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return false
	}
	return u.Scheme == "https" || u.Scheme == "http" && isLoopback(u.Hostname())
}

func isLoopback(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}
