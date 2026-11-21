// Package provider is the public Go SDK for writing tug provider plugins.
//
// A provider is anything that can list Kubernetes clusters and hand out
// credentials for them. A plugin is an executable named tug-provider-<name>.
// For each operation tug starts it with the operation as its only argument,
// writes one JSON request to stdin and reads one JSON response from stdout.
// Anything written to stderr goes to tug's log.
//
// Most plugins only need to implement [Provider] and call [Main]:
//
//	func main() { os.Exit(provider.Main(myProvider{})) }
//
// This package depends on the standard library only, and tug itself uses the
// same types, so the wire format is defined in exactly one place.
package provider

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// APIVersion is the protocol version this SDK speaks.
const APIVersion = "provider/v1alpha1"

// Capability is a bit set of the optional operations a provider implements.
// It is encoded in JSON as a list of names, for example ["list","credential"].
type Capability uint32

// Capabilities, one bit per optional operation.
const (
	CapAccounts Capability = 1 << iota
	CapList
	CapCredential
	CapRotate
	CapCheckAccess
	CapServe
)

var capabilityNames = []struct {
	c    Capability
	name string
}{
	{CapAccounts, "accounts"},
	{CapList, "list"},
	{CapCredential, "credential"},
	{CapRotate, "rotate"},
	{CapCheckAccess, "checkAccess"},
	{CapServe, "serve"},
}

// Has reports whether every capability in o is set in c.
func (c Capability) Has(o Capability) bool { return c&o == o }

// Names returns the capability names in a stable order.
func (c Capability) Names() []string {
	var out []string
	for _, n := range capabilityNames {
		if c.Has(n.c) {
			out = append(out, n.name)
		}
	}
	return out
}

func (c Capability) String() string { return strings.Join(c.Names(), ",") }

// ParseCapability converts a capability name to its bit.
func ParseCapability(name string) (Capability, error) {
	for _, n := range capabilityNames {
		if n.name == name {
			return n.c, nil
		}
	}
	return 0, fmt.Errorf("unknown capability %q", name)
}

// MarshalJSON encodes the set as a list of names.
func (c Capability) MarshalJSON() ([]byte, error) {
	names := c.Names()
	if names == nil {
		names = []string{}
	}
	return json.Marshal(names)
}

// UnmarshalJSON decodes a list of names. Unknown names are ignored so that
// newer plugins keep working with older hosts.
func (c *Capability) UnmarshalJSON(b []byte) error {
	var names []string
	if err := json.Unmarshal(b, &names); err != nil {
		return err
	}
	*c = 0
	for _, n := range names {
		if v, err := ParseCapability(n); err == nil {
			*c |= v
		}
	}
	return nil
}

// SecretSpec declares a secret the provider needs. tug prompts for it once per
// account and keeps it in the OS keychain.
type SecretSpec struct {
	Name     string `json:"name"`
	Prompt   string `json:"prompt,omitempty"`
	Optional bool   `json:"optional,omitempty"`
}

// Info describes a provider.
type Info struct {
	Name         string       `json:"name"`
	Version      string       `json:"version,omitempty"`
	Description  string       `json:"description,omitempty"`
	Capabilities Capability   `json:"capabilities"`
	Secrets      []SecretSpec `json:"secrets,omitempty"`
}

// Account is one set of provider settings, for example an AWS profile or a
// CoreWeave organisation. Config holds provider-specific settings that the
// manifest's accountConfig schema describes.
type Account struct {
	ID          string         `json:"id"`
	Description string         `json:"description,omitempty"`
	Config      map[string]any `json:"config,omitempty"`
}

// ConfigString returns a string setting from the account config, or "".
func (a Account) ConfigString(key string) string {
	if a.Config == nil {
		return ""
	}
	if s, ok := a.Config[key].(string); ok {
		return s
	}
	return ""
}

// Endpoints are the API server URLs for a cluster.
type Endpoints struct {
	Public  string `json:"public,omitempty"`
	Private string `json:"private,omitempty"`
}

// Cluster is one Kubernetes cluster a provider can reach.
type Cluster struct {
	// ID is stable for the life of the cluster within its account.
	ID      string `json:"id"`
	Name    string `json:"name"`
	Region  string `json:"region,omitempty"`
	Version string `json:"version,omitempty"`

	Endpoints             Endpoints `json:"endpoints"`
	CAData                []byte    `json:"caData,omitempty"`
	TLSServerName         string    `json:"tlsServerName,omitempty"`
	InsecureSkipTLSVerify bool      `json:"insecureSkipTLSVerify,omitempty"`
	ProxyURL              string    `json:"proxyURL,omitempty"`
	DefaultNamespace      string    `json:"defaultNamespace,omitempty"`

	Labels map[string]string `json:"labels,omitempty"`

	// Credential may be set when the provider already holds the credential at
	// list time, for example a token embedded in a downloaded kubeconfig. tug
	// moves it to the keychain and never writes it to the catalog.
	Credential *Credential `json:"credential,omitempty"`
}

// CredentialType names one of the four credential shapes.
type CredentialType string

// Credential shapes.
const (
	CredentialToken      CredentialType = "token"
	CredentialClientCert CredentialType = "clientCert"
	CredentialOIDC       CredentialType = "oidc"
	CredentialDelegate   CredentialType = "delegate"
)

// ExecConfig describes a command that prints a client.authentication.k8s.io
// ExecCredential, for example a vendor's own kubectl exec plugin.
type ExecConfig struct {
	Command         string            `json:"command"`
	Args            []string          `json:"args,omitempty"`
	Env             map[string]string `json:"env,omitempty"`
	APIVersion      string            `json:"apiVersion,omitempty"`
	InteractiveMode string            `json:"interactiveMode,omitempty"`
	InstallHint     string            `json:"installHint,omitempty"`

	// ProvideClusterInfo asks tug to tell the command which cluster it
	// authenticates to, as kubectl does for a kubeconfig exec plugin that
	// sets provideClusterInfo: the ExecCredential in KUBERNETES_EXEC_INFO
	// then has a spec.cluster with the cluster's server, CA data, TLS
	// server name, insecure flag and proxy URL from tug's catalog.
	ProvideClusterInfo bool `json:"provideClusterInfo,omitempty"`
	// ClusterConfig is passed to the command as spec.cluster.config, which
	// kubectl fills from the cluster's client.authentication.k8s.io/exec
	// kubeconfig extension: settings for this cluster only, such as the
	// audience to ask for. It is any JSON value, needs ProvideClusterInfo,
	// and like that extension is not meant for secrets.
	ClusterConfig json.RawMessage `json:"clusterConfig,omitempty"`
}

// Credential is what a provider returns for one cluster. Exactly one shape is
// filled, as named by Type.
type Credential struct {
	Type CredentialType `json:"type"`

	// token
	Token string `json:"token,omitempty"`

	// clientCert: PEM, base64-encoded on the wire like kubeconfig *-data fields.
	CertData []byte `json:"certData,omitempty"`
	KeyData  []byte `json:"keyData,omitempty"`

	// oidc
	Issuer       string   `json:"issuer,omitempty"`
	ClientID     string   `json:"clientID,omitempty"`
	ClientSecret string   `json:"clientSecret,omitempty"`
	Scopes       []string `json:"scopes,omitempty"`

	// delegate
	Exec *ExecConfig `json:"exec,omitempty"`

	// ExpiresAt is when tug must ask again. Nil means it does not expire.
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
}

// NoExpiry marks a credential that does not expire. It is the zero time.
var NoExpiry = time.Time{}

func expiry(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}

// Token returns a bearer token credential. Pass NoExpiry for long-lived tokens.
func Token(token string, expiresAt time.Time) Credential {
	return Credential{Type: CredentialToken, Token: token, ExpiresAt: expiry(expiresAt)}
}

// ClientCert returns a client certificate credential from PEM data.
func ClientCert(certPEM, keyPEM []byte, expiresAt time.Time) Credential {
	return Credential{Type: CredentialClientCert, CertData: certPEM, KeyData: keyPEM, ExpiresAt: expiry(expiresAt)}
}

// OIDC returns a credential that tells tug to sign in to an OpenID Connect
// issuer and use its ID token. tug always asks for the openid scope.
func OIDC(issuer, clientID string, scopes ...string) Credential {
	return Credential{Type: CredentialOIDC, Issuer: issuer, ClientID: clientID, Scopes: scopes}
}

// Delegate returns a credential that runs another exec plugin unchanged.
func Delegate(command string, args ...string) Credential {
	return Credential{Type: CredentialDelegate, Exec: &ExecConfig{Command: command, Args: args}}
}

// Validate checks that the credential has the fields its type needs.
func (c Credential) Validate() error {
	switch c.Type {
	case CredentialToken:
		if c.Token == "" {
			return fmt.Errorf("token credential has no token")
		}
	case CredentialClientCert:
		if len(c.CertData) == 0 || len(c.KeyData) == 0 {
			return fmt.Errorf("clientCert credential needs certData and keyData")
		}
	case CredentialOIDC:
		if c.Issuer == "" || c.ClientID == "" {
			return fmt.Errorf("oidc credential needs issuer and clientID")
		}
	case CredentialDelegate:
		if c.Exec == nil || c.Exec.Command == "" {
			return fmt.Errorf("delegate credential needs exec.command")
		}
		if cc := c.Exec.ClusterConfig; len(cc) > 0 {
			switch {
			case !json.Valid(cc):
				return fmt.Errorf("delegate credential's exec.clusterConfig is not JSON")
			case !c.Exec.ProvideClusterInfo && string(cc) != "null":
				// kubectl passes the extension only with provideClusterInfo,
				// so the command would never see it.
				return fmt.Errorf("delegate credential sets exec.clusterConfig without exec.provideClusterInfo")
			}
		}
	default:
		return fmt.Errorf("unknown credential type %q", c.Type)
	}
	return nil
}

// Expired reports whether the credential expires within skew of now.
func (c Credential) Expired(now time.Time, skew time.Duration) bool {
	return c.ExpiresAt != nil && !now.Add(skew).Before(*c.ExpiresAt)
}

// ListRequest asks for the clusters in one account.
type ListRequest struct {
	Account Account           `json:"account"`
	Secrets map[string]string `json:"secrets,omitempty"`
}

// CredentialRequest asks for a credential for one cluster.
type CredentialRequest struct {
	Account   Account           `json:"account"`
	ClusterID string            `json:"clusterID"`
	Secrets   map[string]string `json:"secrets,omitempty"`
}

// AccessRequest asks whether the caller can reach a cluster's API server.
type AccessRequest struct {
	Account  Account           `json:"account"`
	Cluster  Cluster           `json:"cluster"`
	EgressIP string            `json:"egressIP,omitempty"`
	Secrets  map[string]string `json:"secrets,omitempty"`
}

// Severity of an access finding.
type Severity string

// Severities, from least to most serious.
const (
	SeverityInfo    Severity = "info"
	SeverityWarning Severity = "warning"
	SeverityError   Severity = "error"
)

// Finding is one result of an access check.
type Finding struct {
	Severity Severity `json:"severity"`
	Message  string   `json:"message"`
	Fix      string   `json:"fix,omitempty"`
}
