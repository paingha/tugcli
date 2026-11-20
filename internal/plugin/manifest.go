// Package plugin is the plugin host: it finds provider plugins, checks their
// manifests and checksums, and runs them as processes speaking JSON on stdio.
package plugin

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"

	"sigs.k8s.io/yaml"

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

var validName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
var validEnv = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

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
	return m, nil
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
	if !slices.ContainsFunc(m.Spec.Protocols, func(p string) bool {
		return slices.Contains(provider.SupportedAPIVersions(), p)
	}) {
		errs = append(errs, fmt.Errorf("spec.protocols %v has no version this tug speaks (%v)", m.Spec.Protocols, provider.SupportedAPIVersions()))
	}
	if len(m.Spec.Platforms) == 0 {
		errs = append(errs, errors.New("spec.platforms is empty"))
	}
	for i, p := range m.Spec.Platforms {
		if p.OS == "" || p.Arch == "" || p.Bin == "" {
			errs = append(errs, fmt.Errorf("spec.platforms[%d] needs os, arch and bin", i))
		}
		if filepath.IsAbs(p.Bin) || strings.HasPrefix(filepath.Clean(p.Bin), "..") {
			errs = append(errs, fmt.Errorf("spec.platforms[%d].bin must be a path inside the plugin directory", i))
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

// Platform returns the entry for this OS and architecture.
func (m *Manifest) Platform() (Platform, error) {
	for _, p := range m.Spec.Platforms {
		if p.OS == runtime.GOOS && p.Arch == runtime.GOARCH {
			return p, nil
		}
	}
	return Platform{}, fmt.Errorf("plugin %s has no build for %s/%s", m.Metadata.Name, runtime.GOOS, runtime.GOARCH)
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
