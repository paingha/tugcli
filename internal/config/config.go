// Package config holds tug's directories and its user-edited settings.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"time"

	"sigs.k8s.io/yaml"
)

// Paths are the directories tug reads and writes.
type Paths struct {
	Config  string // settings the user edits: config.yaml
	Data    string // state tug owns: catalog, plugins, encrypted secrets
	Cache   string // safe to delete: discovery cache
	Runtime string // per-terminal sessions; private to the user
}

// DefaultPaths resolves the directories from the environment.
//
// TUG_HOME, when set, roots all four under one directory, which keeps tests and
// CI isolated. Otherwise the XDG base directory variables are honoured on every
// platform, falling back to ~/.config, ~/.local/share and ~/.cache (and
// %AppData% / %LocalAppData% on Windows). Sessions live under
// $XDG_RUNTIME_DIR/tug, or a private directory in the system temp dir.
func DefaultPaths() (Paths, error) {
	if home := os.Getenv("TUG_HOME"); home != "" {
		return Paths{
			Config:  filepath.Join(home, "config"),
			Data:    filepath.Join(home, "data"),
			Cache:   filepath.Join(home, "cache"),
			Runtime: filepath.Join(home, "run"),
		}, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return Paths{}, fmt.Errorf("finding home directory: %w", err)
	}
	p := Paths{
		Config: xdg("XDG_CONFIG_HOME", filepath.Join(home, ".config")),
		Data:   xdg("XDG_DATA_HOME", filepath.Join(home, ".local", "share")),
		Cache:  xdg("XDG_CACHE_HOME", filepath.Join(home, ".cache")),
	}
	if runtime.GOOS == "windows" {
		if os.Getenv("XDG_CONFIG_HOME") == "" {
			if d, err := os.UserConfigDir(); err == nil {
				p.Config = d
			}
		}
		if local := os.Getenv("LOCALAPPDATA"); local != "" {
			if os.Getenv("XDG_DATA_HOME") == "" {
				p.Data = local
			}
			if os.Getenv("XDG_CACHE_HOME") == "" {
				p.Cache = filepath.Join(local, "cache")
			}
		}
	}
	p.Config = filepath.Join(p.Config, "tug")
	p.Data = filepath.Join(p.Data, "tug")
	p.Cache = filepath.Join(p.Cache, "tug")
	if rt := os.Getenv("XDG_RUNTIME_DIR"); rt != "" {
		p.Runtime = filepath.Join(rt, "tug")
	} else {
		p.Runtime = filepath.Join(os.TempDir(), "tug-"+userTag())
	}
	return p, nil
}

func xdg(env, def string) string {
	if v := os.Getenv(env); v != "" && filepath.IsAbs(v) {
		return v
	}
	return def
}

func userTag() string {
	if uid := os.Getuid(); uid >= 0 {
		return strconv.Itoa(uid)
	}
	if u := os.Getenv("USERNAME"); u != "" {
		return u
	}
	return "user"
}

// Config is config.yaml.
type Config struct {
	Keyring  KeyringConfig  `json:"keyring,omitempty"`
	Kubectl  string         `json:"kubectl,omitempty"`
	Guard    GuardConfig    `json:"guard,omitempty"`
	Plugins  PluginsConfig  `json:"plugins,omitempty"`
	Sessions SessionsConfig `json:"sessions,omitempty"`
	OIDC     OIDCConfig     `json:"oidc,omitempty"`
	// Teams are the team catalogs tug follows, besides those added with
	// tug team add.
	Teams []TeamCatalog `json:"teams,omitempty"`

	Intelligence IntelligenceConfig `json:"intelligence,omitzero"`
}

// TeamCatalog is where a team publishes its signed catalog: names and
// labels for its clusters, provider account settings, plugin indexes and
// safety policies. A team catalog never carries credentials.
type TeamCatalog struct {
	Name string `json:"name"`
	// URL is the https URL of the catalog; its signature is at URL + ".sig".
	URL string `json:"url,omitempty"`
	// Git is a file in a git repository, instead of URL.
	Git *TeamGit `json:"git,omitempty"`
	// PublicKey is the base64 ed25519 key the catalog must be signed with.
	PublicKey string `json:"publicKey"`
}

// TeamGit is a team catalog kept in a git repository.
type TeamGit struct {
	// Repo is what git clone takes: an https or ssh URL, or a path.
	Repo string `json:"repo"`
	// Path is the catalog's path in the repository, team.yaml by default;
	// its signature is next to it, with ".sig" added.
	Path string `json:"path,omitempty"`
	// Ref is the branch or tag to read, the repository's default branch
	// when empty.
	Ref string `json:"ref,omitempty"`
}

// IntelligenceConfig turns on AI-assisted explanations, `tug why --deep`,
// which send redacted evidence about an object to a model provider. They are
// off unless Enabled is set; the intelligence package fills in the defaults
// and checks the rest. API keys never go here: they live in the credential
// store or TUG_SECRET_<PROVIDER>_API_KEY.
type IntelligenceConfig struct {
	Enabled bool `json:"enabled,omitempty"`
	// Provider is anthropic, openai, mistral or custom (any OpenAI-compatible
	// endpoint). Default anthropic.
	Provider string `json:"provider,omitempty"`
	// Model is any model id the provider serves. Each provider but custom
	// has a default.
	Model string `json:"model,omitempty"`
	// BaseURL is the provider's API address, for a gateway or a custom
	// endpoint. It must be https unless it is on this machine.
	BaseURL string `json:"baseURL,omitempty"`
	// Timeout bounds one request. Default 5m.
	Timeout Duration `json:"timeout,omitzero"`
	// MaxOutputTokens bounds the answer, thinking included. Default 16000.
	MaxOutputTokens int `json:"maxOutputTokens,omitempty"`
	// MonthlySpendCapUSD refuses a call that could take this calendar
	// month's (UTC) spend past it. Zero means no cap.
	MonthlySpendCapUSD float64 `json:"monthlySpendCapUSD,omitempty"`
	// Effort is how hard an Anthropic model works on its answer: low,
	// medium, high, xhigh or max. Default high.
	Effort string `json:"effort,omitempty"`
	// Prices add to or override the built-in price table, matched by the
	// longest model id prefix.
	Prices []ModelPrice `json:"prices,omitempty"`
}

// ModelPrice is a model's price in US dollars per million tokens.
type ModelPrice struct {
	// Model is a model id or the start of one: "gpt-6.2" covers
	// "gpt-6.2-astra" and "gpt-6.2-sol".
	Model     string  `json:"model"`
	Input     float64 `json:"input"`
	Output    float64 `json:"output"`
	CacheRead float64 `json:"cacheRead,omitempty"`
}

// OIDCConfig controls signing in to OpenID Connect issuers.
type OIDCConfig struct {
	// ListenAddresses are the loopback addresses tried, in order, for the
	// browser's redirect after sign-in, which comes back to
	// http://localhost:<port>. Default: 127.0.0.1:8000, then 127.0.0.1:18000.
	ListenAddresses []string `json:"listenAddresses,omitempty"`
}

// KeyringConfig selects where secrets live.
type KeyringConfig struct {
	// Backend is auto, keychain or file. auto uses the OS keychain when it
	// works and the encrypted file otherwise.
	Backend string `json:"backend,omitempty"`
}

// GuardConfig decides which clusters open read-only.
type GuardConfig struct {
	// ReadOnlyLabels: a cluster matching any of these label sets opens
	// read-only. Default: env=prod.
	ReadOnlyLabels []map[string]string `json:"readOnlyLabels,omitempty"`
}

// PluginsConfig controls the plugin host.
type PluginsConfig struct {
	// AllowOverride lists built-in provider names that a plugin may replace.
	AllowOverride []string `json:"allowOverride,omitempty"`
	// Timeouts per operation, for example {list: 60s}.
	Timeouts map[string]Duration `json:"timeouts,omitempty"`
	// Indexes are the signed plugin indexes that tug plugin search and
	// tug plugin install <name> use, in order.
	Indexes []PluginIndex `json:"indexes,omitempty"`
	// RequireSignatures refuses to install a plugin by name when its index
	// entry has no Sigstore signature. Archives installed from a file or
	// URL given on the command line are not affected; tug warns that they
	// are unsigned.
	RequireSignatures bool `json:"requireSignatures,omitempty"`
	// SigstoreTrustedRoot is a trusted_root.json to check plugin signatures
	// against instead of the public-good Sigstore deployment's, for a
	// private deployment or a machine that cannot reach Sigstore's TUF
	// repository. A relative path is taken from the config directory.
	SigstoreTrustedRoot string `json:"sigstoreTrustedRoot,omitempty"`
	// WASM controls plugins' WebAssembly builds.
	WASM WASMConfig `json:"wasm,omitempty"`
	// Sandbox confines plugins' native builds with the operating system's
	// sandbox.
	Sandbox SandboxConfig `json:"sandbox,omitzero"`
	// Limits caps the processor time, memory and time that plugins may
	// ask for in their manifests' spec.limits.
	Limits PluginLimits `json:"limits,omitzero"`
	// Pins holds a version for each plugin that must stay at it, such as
	// {acme: "0.3.1"}. tug plugin install refuses any other version of a
	// pinned plugin, and tug runs the pinned version even when a newer one
	// is installed. Linked plugins are not affected.
	Pins map[string]string `json:"pins,omitempty"`
}

// PluginPin returns the version the named plugin is pinned to, if any.
func (c PluginsConfig) PluginPin(name string) (string, bool) {
	v, ok := c.Pins[name]
	return v, ok && v != ""
}

// PluginLimits is plugins.limits: the most any plugin may use, whatever its
// manifest asks for. A zero field leaves that limit to the manifest.
type PluginLimits struct {
	// CPUSeconds caps the processor time of each plugin process, and
	// applies to plugins that ask for no limit as well.
	CPUSeconds int `json:"cpuSeconds,omitempty"`
	// MemoryMiB caps each plugin process's memory on Linux, and each
	// WebAssembly plugin's everywhere, and applies to plugins that ask for
	// no limit as well.
	MemoryMiB int `json:"memoryMiB,omitempty"`
	// Timeout caps how long any one call to a plugin may take, including
	// the timeouts set in plugins.timeouts. Without it, a manifest may ask
	// for at most five minutes.
	Timeout Duration `json:"timeout,omitzero"`
}

// WASMConfig controls the WebAssembly builds of plugins, which tug runs in
// its own sandbox.
type WASMConfig struct {
	// Prefer runs a plugin's WebAssembly build even when it also has a
	// build for this machine. Without it, the WebAssembly build is used
	// only when there is no such build.
	Prefer bool `json:"prefer,omitempty"`
	// MemoryLimitMiB caps each module's memory. Default 256.
	MemoryLimitMiB int `json:"memoryLimitMiB,omitempty"`
}

// MemoryLimit returns the configured memory limit in bytes, or zero for the
// plugin host's default.
func (c WASMConfig) MemoryLimit() uint64 {
	if c.MemoryLimitMiB <= 0 {
		return 0
	}
	return uint64(c.MemoryLimitMiB) << 20
}

// SandboxConfig is plugins.sandbox. The sandbox is on unless it says
// otherwise.
type SandboxConfig struct {
	// Enabled set to false runs every plugin without the sandbox.
	Enabled *bool `json:"enabled,omitempty"`
	// Exclude lists plugins that run without the sandbox, for ones that
	// need more than it allows.
	Exclude []string `json:"exclude,omitempty"`
}

// Confines reports whether the named plugin runs in the sandbox.
func (s SandboxConfig) Confines(plugin string) bool {
	return (s.Enabled == nil || *s.Enabled) && !slices.Contains(s.Exclude, plugin)
}

// PluginIndex is one signed plugin index.
type PluginIndex struct {
	Name string `json:"name"`
	// URL is the index file; its signature is at URL + ".sig".
	URL string `json:"url"`
	// PublicKey is the base64 ed25519 key the index must be signed with.
	PublicKey string `json:"publicKey"`
}

// SessionsConfig controls session cleanup.
type SessionsConfig struct {
	// MaxAge after which an abandoned session is removed. Default 7 days.
	MaxAge Duration `json:"maxAge,omitempty"`
}

// Duration is a time.Duration written as "30s" in YAML.
type Duration struct{ time.Duration }

// MarshalJSON writes the duration as a string such as "30s".
func (d Duration) MarshalJSON() ([]byte, error) { return []byte(strconv.Quote(d.String())), nil }

// UnmarshalJSON reads a duration string such as "30s".
func (d *Duration) UnmarshalJSON(b []byte) error {
	s, err := strconv.Unquote(string(b))
	if err != nil {
		return fmt.Errorf("duration must be a string like \"30s\": %s", b)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

// Defaults fills unset fields.
func (c *Config) Defaults() {
	if c.Keyring.Backend == "" {
		c.Keyring.Backend = "auto"
	}
	if c.Kubectl == "" {
		c.Kubectl = "kubectl"
	}
	if len(c.Guard.ReadOnlyLabels) == 0 {
		c.Guard.ReadOnlyLabels = []map[string]string{{"env": "prod"}}
	}
	if c.Sessions.MaxAge.Duration == 0 {
		c.Sessions.MaxAge.Duration = 7 * 24 * time.Hour
	}
}

// PluginTimeouts returns the configured timeout for each plugin operation
// that has one. Operations left out, or set to zero, keep the plugin host's
// defaults.
func (c *Config) PluginTimeouts() map[string]time.Duration {
	out := make(map[string]time.Duration, len(c.Plugins.Timeouts))
	for op, d := range c.Plugins.Timeouts {
		if d.Duration > 0 {
			out[op] = d.Duration
		}
	}
	return out
}

// Load reads config.yaml from dir. A missing file gives the defaults.
func Load(dir string) (*Config, error) {
	c := &Config{}
	b, err := os.ReadFile(filepath.Join(dir, "config.yaml"))
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if err := yaml.UnmarshalStrict(b, c); err != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Join(dir, "config.yaml"), err)
		}
	}
	if v := os.Getenv("TUG_KEYRING_BACKEND"); v != "" {
		c.Keyring.Backend = v
	}
	if v := os.Getenv("TUG_KUBECTL"); v != "" {
		c.Kubectl = v
	}
	c.Defaults()
	return c, nil
}

// WriteFileAtomic writes data to path through a temporary file and a rename, so
// readers never see a partial file.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(perm); err != nil && runtime.GOOS != "windows" {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
