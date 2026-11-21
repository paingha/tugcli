// Package plugin is the plugin host: it finds provider plugins, checks their
// manifests and checksums, and runs them speaking JSON on stdio, either as
// processes or, for WebAssembly builds, as modules inside tug's sandbox.
package plugin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/paingha/tugcli/internal/version"
	"github.com/paingha/tugcli/pkg/provider"
)

// Manifest API version and kind.
const (
	ManifestAPIVersion = "tug.plugin/v1alpha1"
	ManifestKind       = "ProviderPlugin"
	ManifestFile       = "plugin.yaml"
	// BinaryPrefix is the executable name prefix for provider plugins.
	BinaryPrefix = "tug-provider-"
)

// Manifest is plugin.yaml.
type Manifest struct {
	APIVersion string   `json:"apiVersion"`
	Kind       string   `json:"kind"`
	Metadata   Metadata `json:"metadata"`
	Spec       Spec     `json:"spec"`

	// Unknown lists the fields of plugin.yaml that this tug does not know,
	// as paths such as "spec.platfroms". They are ignored rather than
	// refused, so a manifest written for a newer tug still loads; a typo is
	// the other usual cause, so commands that show a manifest warn about
	// them.
	Unknown []string `json:"-"`
}

// Metadata names a plugin.
type Metadata struct {
	Name      string `json:"name"`
	Version   string `json:"version"`
	Publisher string `json:"publisher,omitempty"`
	Homepage  string `json:"homepage,omitempty"`
}

// Spec says what a plugin needs before it ever runs.
type Spec struct {
	Protocols     []string              `json:"protocols"`
	MinTugVersion string                `json:"minTugVersion,omitempty"`
	Capabilities  provider.Capability   `json:"capabilities"`
	Platforms     []Platform            `json:"platforms"`
	Secrets       []provider.SecretSpec `json:"secrets,omitempty"`
	// Env lists host environment variables passed through, e.g. AWS_PROFILE.
	Env []string `json:"env,omitempty"`
	// Network lists the hosts the plugin talks to; shown at install time.
	Network []string `json:"network,omitempty"`
	// AccountConfig is a JSON Schema for per-account settings.
	AccountConfig map[string]any `json:"accountConfig,omitempty"`
}

// Platform is one executable for an OS and architecture.
type Platform struct {
	OS     string `json:"os"`
	Arch   string `json:"arch"`
	Bin    string `json:"bin"`
	SHA256 string `json:"sha256,omitempty"`
}

// The platform of a WebAssembly build, as GOOS and GOARCH name it. tug runs
// such a build itself, on every OS and architecture.
const (
	WASMOS   = "wasip1"
	WASMArch = "wasm"
)

// IsWASM reports whether p is a WebAssembly build.
func (p Platform) IsWASM() bool { return p.OS == WASMOS && p.Arch == WASMArch }

var (
	validName    = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	validEnv     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	validVersion = regexp.MustCompile(`^v?\d+(\.\d+){0,2}$`)
)

// LoadManifest reads plugin.yaml from dir.
func LoadManifest(dir string) (*Manifest, error) {
	b, err := os.ReadFile(filepath.Join(dir, ManifestFile))
	if err != nil {
		return nil, err
	}
	m := &Manifest{}
	if err := yaml.Unmarshal(b, m); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Join(dir, ManifestFile), err)
	}
	if err := m.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Join(dir, ManifestFile), err)
	}
	var raw any
	if err := yaml.Unmarshal(b, &raw); err == nil {
		m.Unknown = unknownFields(raw, reflect.TypeFor[Manifest](), "")
	}
	return m, nil
}

var jsonUnmarshaler = reflect.TypeFor[json.Unmarshaler]()

// unknownFields walks a decoded YAML document alongside the Go type it was
// decoded into and returns the paths of keys that type has no field for.
// Keys match field names without regard to case, as encoding/json does.
// Types that decode themselves, and maps, accept any keys.
func unknownFields(raw any, t reflect.Type, path string) []string {
	if reflect.PointerTo(t).Implements(jsonUnmarshaler) {
		return nil
	}
	switch t.Kind() {
	case reflect.Struct:
		obj, ok := raw.(map[string]any)
		if !ok {
			return nil
		}
		fields := map[string]reflect.Type{}
		for f := range t.Fields() {
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if name != "" && name != "-" {
				fields[strings.ToLower(name)] = f.Type
			}
		}
		var out []string
		for _, k := range slices.Sorted(maps.Keys(obj)) {
			p := k
			if path != "" {
				p = path + "." + k
			}
			ft, ok := fields[strings.ToLower(k)]
			if !ok {
				out = append(out, p)
				continue
			}
			out = append(out, unknownFields(obj[k], ft, p)...)
		}
		return out
	case reflect.Slice:
		items, ok := raw.([]any)
		if !ok {
			return nil
		}
		var out []string
		for i, item := range items {
			out = append(out, unknownFields(item, t.Elem(), fmt.Sprintf("%s[%d]", path, i))...)
		}
		return out
	default:
		return nil
	}
}

// Validate checks the manifest.
func (m *Manifest) Validate() error {
	var errs []error
	if m.APIVersion != ManifestAPIVersion {
		errs = append(errs, fmt.Errorf("apiVersion must be %s", ManifestAPIVersion))
	}
	if m.Kind != ManifestKind {
		errs = append(errs, fmt.Errorf("kind must be %s", ManifestKind))
	}
	if !validName.MatchString(m.Metadata.Name) {
		errs = append(errs, fmt.Errorf("metadata.name %q must be lower-case letters, digits and dashes", m.Metadata.Name))
	}
	if m.Metadata.Version == "" {
		errs = append(errs, errors.New("metadata.version is required"))
	}
	if v := m.Spec.MinTugVersion; v != "" && !validVersion.MatchString(v) {
		errs = append(errs, fmt.Errorf("spec.minTugVersion %q must be a version such as 0.4.0", v))
	}
	if !slices.ContainsFunc(m.Spec.Protocols, func(p string) bool {
		return slices.Contains(provider.SupportedAPIVersions(), p)
	}) {
		errs = append(errs, fmt.Errorf("spec.protocols %v has no version this tug speaks (%v)", m.Spec.Protocols, provider.SupportedAPIVersions()))
	}
	if len(m.Spec.Platforms) == 0 {
		errs = append(errs, errors.New("spec.platforms is empty"))
	}
	wasm := 0
	for i, p := range m.Spec.Platforms {
		if p.OS == "" || p.Arch == "" || p.Bin == "" {
			errs = append(errs, fmt.Errorf("spec.platforms[%d] needs os, arch and bin", i))
		}
		if filepath.IsAbs(p.Bin) || strings.HasPrefix(filepath.Clean(p.Bin), "..") {
			errs = append(errs, fmt.Errorf("spec.platforms[%d].bin must be a path inside the plugin directory", i))
		}
		if (p.OS == WASMOS) != (p.Arch == WASMArch) {
			errs = append(errs, fmt.Errorf("spec.platforms[%d]: a WebAssembly build is {os: %s, arch: %s}", i, WASMOS, WASMArch))
		}
		if p.IsWASM() {
			wasm++
		}
	}
	if wasm > 1 {
		errs = append(errs, errors.New("spec.platforms lists more than one WebAssembly build"))
	}
	if wasm > 0 {
		// The WebAssembly build is held to spec.network, so every entry
		// must be a host rather than a description.
		if _, err := newAllowList(m.Spec.Network); err != nil {
			errs = append(errs, fmt.Errorf("%w; the WebAssembly build may reach only the hosts in spec.network, written as api.example.com, *.example.com or api.example.com:8443", err))
		}
	}
	for _, s := range m.Spec.Secrets {
		if s.Name == "" {
			errs = append(errs, errors.New("spec.secrets entries need a name"))
		}
	}
	for _, e := range m.Spec.Env {
		if !validEnv.MatchString(e) {
			errs = append(errs, fmt.Errorf("spec.env %q is not a variable name", e))
		}
	}
	return errors.Join(errs...)
}

// CheckTugVersion returns an error when the manifest's minTugVersion is
// newer than tug, the running tug's version. Development builds are not
// releases and cannot be compared, so they are assumed to be new enough.
func (m *Manifest) CheckTugVersion(tug string) error {
	need := m.Spec.MinTugVersion
	if need == "" || !version.IsRelease(tug) {
		return nil
	}
	if compareVersions(tug, need) < 0 {
		return fmt.Errorf("plugin %s needs tug v%s or later and this is %s; upgrade tug to use it",
			m.Metadata.Name, strings.TrimPrefix(need, "v"), tug)
	}
	return nil
}

// Setting is one per-account setting from the manifest's accountConfig
// schema, passed to `tug import` as --set name=value.
type Setting struct {
	Name        string
	Type        string
	Description string
	Enum        []string
}

// Settings returns the properties of the accountConfig schema, sorted by
// name. It is empty when the manifest describes none.
func (m *Manifest) Settings() []Setting {
	props, _ := m.Spec.AccountConfig["properties"].(map[string]any)
	out := make([]Setting, 0, len(props))
	for _, name := range slices.Sorted(maps.Keys(props)) {
		s := Setting{Name: name}
		if prop, ok := props[name].(map[string]any); ok {
			s.Type, _ = prop["type"].(string)
			s.Description, _ = prop["description"].(string)
			if enum, ok := prop["enum"].([]any); ok {
				for _, e := range enum {
					s.Enum = append(s.Enum, fmt.Sprint(e))
				}
			}
		}
		out = append(out, s)
	}
	return out
}

// UnknownSettings returns the keys of config that the accountConfig schema
// does not describe, sorted. It returns nothing when the manifest has no
// schema or the schema allows other properties, since then any key may be
// meant.
func (m *Manifest) UnknownSettings(config map[string]any) []string {
	props, ok := m.Spec.AccountConfig["properties"].(map[string]any)
	if !ok {
		return nil
	}
	if extra, ok := m.Spec.AccountConfig["additionalProperties"]; ok && extra != false {
		return nil
	}
	var out []string
	for _, k := range slices.Sorted(maps.Keys(config)) {
		if _, ok := props[k]; !ok {
			out = append(out, k)
		}
	}
	return out
}

// Platform returns the build to run here: the entry for this OS and
// architecture, or else the WebAssembly build, which runs everywhere.
func (m *Manifest) Platform() (Platform, error) {
	builds := m.Runnable(false)
	if len(builds) == 0 {
		return Platform{}, m.noBuild()
	}
	return builds[0], nil
}

// Runnable returns the builds that can run here, best first: the entry for
// this OS and architecture, then the WebAssembly build. With preferWASM the
// WebAssembly build comes first.
func (m *Manifest) Runnable(preferWASM bool) []Platform {
	var native, wasm []Platform
	for _, p := range m.Spec.Platforms {
		switch {
		case p.IsWASM():
			wasm = append(wasm, p)
		case p.OS == runtime.GOOS && p.Arch == runtime.GOARCH:
			native = append(native, p)
		}
	}
	if preferWASM {
		return append(wasm, native...)
	}
	return append(native, wasm...)
}

func (m *Manifest) noBuild() error {
	return fmt.Errorf("plugin %s has no build for %s/%s and no WebAssembly build", m.Metadata.Name, runtime.GOOS, runtime.GOARCH)
}

// VerifyChecksum compares the binary at path with the manifest's sha256.
func VerifyChecksum(path, want string) error {
	if want == "" {
		return errors.New("manifest has no sha256 for this platform")
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, want) {
		return fmt.Errorf("checksum mismatch for %s: manifest says %s, file is %s", path, want, got)
	}
	return nil
}
