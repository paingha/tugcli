package credentials

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientauthv1 "k8s.io/client-go/pkg/apis/clientauthentication/v1"

	sdk "github.com/paingha/tugcli/pkg/provider"
)

// Skew is how long before expiry a cached credential is treated as expired.
const Skew = 60 * time.Second

// Fetcher asks a cluster's provider for a fresh credential.
type Fetcher func(ctx context.Context, clusterID string) (sdk.Credential, error)

// ErrLoginRequired is wrapped by OIDCTokens when nobody is signed in to the
// issuer and it may not start a sign-in itself.
var ErrLoginRequired = errors.New("sign-in required")

// OIDCTokens turns a credential of type oidc into an ID token, signing in or
// refreshing as needed. The oidc package implements it.
type OIDCTokens interface {
	// IDToken returns a valid ID token and when it expires. With refresh
	// set, it does not answer from its cache.
	IDToken(ctx context.Context, c sdk.Credential, refresh bool) (token string, expiresAt time.Time, err error)
}

// Broker answers credential requests from the cache and falls back to the
// provider. It is the core of `tug auth token`.
type Broker struct {
	Store Store
	Fetch Fetcher
	Now   func() time.Time
	// RunExec runs a delegated exec plugin and returns its stdout. Tests
	// replace it.
	RunExec func(ctx context.Context, cfg sdk.ExecConfig) ([]byte, error)
	// OIDC answers credentials of type oidc. Without it they are refused.
	OIDC OIDCTokens
}

// Resolved is a credential ready to use: either a token, a client certificate,
// or a delegated exec config that the caller runs itself.
type Resolved struct {
	Token     string
	CertPEM   []byte
	KeyPEM    []byte
	ExpiresAt *time.Time
	Delegate  *sdk.ExecConfig
}

func (b *Broker) now() time.Time {
	if b.Now != nil {
		return b.Now()
	}
	return time.Now()
}

// Put stores a credential for a cluster, replacing any earlier one. Import and
// sync use it for credentials that providers return inline.
func (b *Broker) Put(clusterID string, c sdk.Credential) error {
	if err := c.Validate(); err != nil {
		return err
	}
	data, err := json.Marshal(c)
	if err != nil {
		return err
	}
	if err := b.Store.Set(CredentialKey(clusterID), string(data)); err != nil {
		return err
	}
	// A new stored credential makes any delegated output stale.
	_ = b.Store.Delete(DelegateKey(clusterID))
	return nil
}

// Forget removes everything stored for a cluster.
func (b *Broker) Forget(clusterID string) error {
	err1 := b.Store.Delete(CredentialKey(clusterID))
	err2 := b.Store.Delete(DelegateKey(clusterID))
	return errors.Join(err1, err2)
}

// Credential returns the stored credential for a cluster, fetching a new one
// from the provider when there is none or it has expired.
func (b *Broker) Credential(ctx context.Context, clusterID string, refresh bool) (sdk.Credential, error) {
	if !refresh {
		if c, ok := b.cached(clusterID); ok {
			return c, nil
		}
	}
	if b.Fetch == nil {
		return sdk.Credential{}, fmt.Errorf("no stored credential for %s", clusterID)
	}
	c, err := b.Fetch(ctx, clusterID)
	if err != nil {
		return sdk.Credential{}, err
	}
	if err := c.Validate(); err != nil {
		return sdk.Credential{}, fmt.Errorf("provider returned an invalid credential: %w", err)
	}
	if err := b.Put(clusterID, c); err != nil {
		return sdk.Credential{}, fmt.Errorf("storing credential in %s: %w", b.Store.Name(), err)
	}
	return c, nil
}

func (b *Broker) cached(clusterID string) (sdk.Credential, bool) {
	v, err := b.Store.Get(CredentialKey(clusterID))
	if err != nil {
		return sdk.Credential{}, false
	}
	var c sdk.Credential
	if json.Unmarshal([]byte(v), &c) != nil || c.Validate() != nil {
		return sdk.Credential{}, false
	}
	if c.Expired(b.now(), Skew) {
		return sdk.Credential{}, false
	}
	return c, true
}

// Resolve returns a credential in usable form. Delegated credentials are
// returned as an exec config; call ExecCredential to run them.
func (b *Broker) Resolve(ctx context.Context, clusterID string, refresh bool) (*Resolved, error) {
	c, err := b.Credential(ctx, clusterID, refresh)
	if err != nil {
		return nil, err
	}
	switch c.Type {
	case sdk.CredentialToken:
		return &Resolved{Token: c.Token, ExpiresAt: c.ExpiresAt}, nil
	case sdk.CredentialClientCert:
		return &Resolved{CertPEM: c.CertData, KeyPEM: c.KeyData, ExpiresAt: c.ExpiresAt}, nil
	case sdk.CredentialDelegate:
		return &Resolved{Delegate: c.Exec}, nil
	case sdk.CredentialOIDC:
		return b.resolveOIDC(ctx, clusterID, c, refresh)
	default:
		return nil, fmt.Errorf("unknown credential type %q", c.Type)
	}
}

// resolveOIDC signs in to the credential's issuer, or reuses the sign-in,
// and returns its ID token.
func (b *Broker) resolveOIDC(ctx context.Context, clusterID string, c sdk.Credential, refresh bool) (*Resolved, error) {
	if b.OIDC == nil {
		return nil, sdk.Errorf(sdk.Unimplemented, "this command cannot sign in to %s", c.Issuer)
	}
	token, exp, err := b.OIDC.IDToken(ctx, c, refresh)
	if errors.Is(err, ErrLoginRequired) {
		return nil, sdk.Errorf(sdk.Unauthenticated, "not signed in to %s", c.Issuer).
			WithHint(fmt.Sprintf("run `tug auth login %s` to sign in", clusterID))
	}
	if err != nil {
		return nil, err
	}
	return &Resolved{Token: token, ExpiresAt: &exp}, nil
}

// ExecCredential returns the client.authentication.k8s.io/v1 ExecCredential
// that kubectl expects from `tug auth token`.
func (b *Broker) ExecCredential(ctx context.Context, clusterID string, refresh bool) (*clientauthv1.ExecCredential, error) {
	r, err := b.Resolve(ctx, clusterID, refresh)
	if err != nil {
		return nil, err
	}
	out := &clientauthv1.ExecCredential{
		TypeMeta: metav1.TypeMeta{APIVersion: clientauthv1.SchemeGroupVersion.String(), Kind: "ExecCredential"},
	}
	if r.Delegate != nil {
		st, err := b.delegated(ctx, clusterID, *r.Delegate, refresh)
		if err != nil {
			return nil, err
		}
		out.Status = st
		return out, nil
	}
	out.Status = &clientauthv1.ExecCredentialStatus{
		Token:                 r.Token,
		ClientCertificateData: string(r.CertPEM),
		ClientKeyData:         string(r.KeyPEM),
	}
	if r.ExpiresAt != nil {
		t := metav1.NewTime(*r.ExpiresAt)
		out.Status.ExpirationTimestamp = &t
	}
	return out, nil
}

// delegated runs a delegated exec plugin, caching its output until expiry.
func (b *Broker) delegated(ctx context.Context, clusterID string, cfg sdk.ExecConfig, refresh bool) (*clientauthv1.ExecCredentialStatus, error) {
	if !refresh {
		if v, err := b.Store.Get(DelegateKey(clusterID)); err == nil {
			var st clientauthv1.ExecCredentialStatus
			if json.Unmarshal([]byte(v), &st) == nil && st.ExpirationTimestamp != nil &&
				b.now().Add(Skew).Before(st.ExpirationTimestamp.Time) {
				return &st, nil
			}
		}
	}
	run := b.RunExec
	if run == nil {
		run = RunExec
	}
	raw, err := run(ctx, cfg)
	if err != nil {
		return nil, err
	}
	var ec clientauthv1.ExecCredential
	if err := json.Unmarshal(raw, &ec); err != nil {
		return nil, fmt.Errorf("%s printed something that is not an ExecCredential: %w", cfg.Command, err)
	}
	if ec.Status == nil || (ec.Status.Token == "" && ec.Status.ClientCertificateData == "") {
		return nil, fmt.Errorf("%s returned an ExecCredential without a token or certificate", cfg.Command)
	}
	// Only cache outputs that say when they expire; otherwise the delegate
	// keeps control of its own caching.
	if ec.Status.ExpirationTimestamp != nil {
		if data, err := json.Marshal(ec.Status); err == nil {
			_ = b.Store.Set(DelegateKey(clusterID), string(data))
		}
	}
	return ec.Status, nil
}

// RunExec runs a client-go exec credential plugin and returns its stdout. The
// plugin inherits stdin and stderr so that it can prompt the user.
func RunExec(ctx context.Context, cfg sdk.ExecConfig) ([]byte, error) {
	cmd := exec.CommandContext(ctx, cfg.Command, cfg.Args...)
	cmd.Env = os.Environ()
	for k, v := range cfg.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	apiVersion := cfg.APIVersion
	if apiVersion == "" {
		apiVersion = clientauthv1.SchemeGroupVersion.String()
	}
	interactive := cfg.InteractiveMode != "Never" && execInfoInteractive()
	info, _ := json.Marshal(map[string]any{
		"apiVersion": apiVersion,
		"kind":       "ExecCredential",
		"spec":       map[string]any{"interactive": interactive},
	})
	cmd.Env = append(cmd.Env, "KUBERNETES_EXEC_INFO="+string(info))
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = os.Stderr
	if interactive {
		cmd.Stdin = os.Stdin
	}
	if err := cmd.Run(); err != nil {
		if errors.Is(err, exec.ErrNotFound) && cfg.InstallHint != "" {
			return nil, fmt.Errorf("%s not found: %s", cfg.Command, cfg.InstallHint)
		}
		return nil, fmt.Errorf("running %s: %w", cfg.Command, err)
	}
	return io.ReadAll(&stdout)
}

// execInfoInteractive reports whether kubectl told us the session is
// interactive through KUBERNETES_EXEC_INFO.
func execInfoInteractive() bool {
	v := os.Getenv("KUBERNETES_EXEC_INFO")
	if v == "" {
		return false
	}
	var info struct {
		Spec struct {
			Interactive bool `json:"interactive"`
		} `json:"spec"`
	}
	return json.Unmarshal([]byte(v), &info) == nil && info.Spec.Interactive
}
