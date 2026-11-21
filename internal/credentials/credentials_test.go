package credentials

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/zalando/go-keyring"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	clientauthv1 "k8s.io/client-go/pkg/apis/clientauthentication/v1"
	authexec "k8s.io/client-go/tools/auth/exec"

	"github.com/paingha/tugcli/internal/config"
	"github.com/paingha/tugcli/pkg/provider"
)

func TestFileStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.enc")
	s := NewFileStore(path, "correct horse")

	if _, err := s.Get("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(missing) = %v, want ErrNotFound", err)
	}
	if err := s.Set("a", "1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Set("b", "2"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("a"); err != nil {
		t.Fatal(err)
	}

	reopened := NewFileStore(path, "correct horse")
	if v, err := reopened.Get("b"); err != nil || v != "2" {
		t.Fatalf("Get(b) = %q, %v", v, err)
	}
	if _, err := reopened.Get("a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted key still present: %v", err)
	}

	wrong := NewFileStore(path, "wrong")
	if _, err := wrong.Get("b"); err == nil || !strings.Contains(err.Error(), "cannot decrypt") {
		t.Fatalf("wrong passphrase: %v", err)
	}
}

// fakeKeyring enforces a per-item size limit like the real backends.
type fakeKeyring struct {
	items map[string]string
	limit int
}

func (f *fakeKeyring) Get(_, user string) (string, error) {
	v, ok := f.items[user]
	if !ok {
		return "", keyring.ErrNotFound
	}
	return v, nil
}

func (f *fakeKeyring) Set(_, user, password string) error {
	if len(password) > f.limit {
		return keyring.ErrSetDataTooBig
	}
	f.items[user] = password
	return nil
}

func (f *fakeKeyring) Delete(_, user string) error {
	if _, ok := f.items[user]; !ok {
		return keyring.ErrNotFound
	}
	delete(f.items, user)
	return nil
}

func TestKeychainChunking(t *testing.T) {
	fk := &fakeKeyring{items: map[string]string{}, limit: 2000}
	k := &KeychainStore{kr: fk}

	big := strings.Repeat("x", 5000)
	if err := k.Set("cred/big", big); err != nil {
		t.Fatal(err)
	}
	if got, err := k.Get("cred/big"); err != nil || got != big {
		t.Fatalf("chunked round trip failed: len %d, %v", len(got), err)
	}
	if len(fk.items) != 4 { // header + 3 chunks
		t.Fatalf("want 4 items, got %d", len(fk.items))
	}

	// Replacing with a small value removes the old chunks.
	if err := k.Set("cred/big", "small"); err != nil {
		t.Fatal(err)
	}
	if len(fk.items) != 1 {
		t.Fatalf("old chunks left behind: %v", keys(fk.items))
	}
	if err := k.Delete("cred/big"); err != nil {
		t.Fatal(err)
	}
	if _, err := k.Get("cred/big"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestBrokerCachesUntilExpiry(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	calls := 0
	b := &Broker{
		Store: NewMemoryStore(),
		Now:   func() time.Time { return now },
		Fetch: func(context.Context, string) (provider.Credential, error) {
			calls++
			return provider.Token("tok", now.Add(15*time.Minute)), nil
		},
	}
	ctx := context.Background()
	for range 3 {
		ec, err := b.ExecCredential(ctx, "c1", false)
		if err != nil {
			t.Fatal(err)
		}
		if ec.Status.Token != "tok" || ec.Status.ExpirationTimestamp == nil {
			t.Fatalf("status: %+v", ec.Status)
		}
	}
	if calls != 1 {
		t.Fatalf("provider called %d times, want 1", calls)
	}

	now = now.Add(14*time.Minute + 30*time.Second) // within the skew of expiry
	if _, err := b.ExecCredential(ctx, "c1", false); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("expired credential not refreshed: %d calls", calls)
	}

	if _, err := b.ExecCredential(ctx, "c1", true); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("--refresh did not ask the provider: %d calls", calls)
	}
}

func TestBrokerStoredCredentialWithoutExpiry(t *testing.T) {
	b := &Broker{Store: NewMemoryStore()}
	if err := b.Put("c1", provider.ClientCert([]byte("CERT"), []byte("KEY"), provider.NoExpiry)); err != nil {
		t.Fatal(err)
	}
	ec, err := b.ExecCredential(context.Background(), "c1", false)
	if err != nil {
		t.Fatal(err)
	}
	if ec.Status.ClientCertificateData != "CERT" || ec.Status.ClientKeyData != "KEY" || ec.Status.ExpirationTimestamp != nil {
		t.Fatalf("status: %+v", ec.Status)
	}
	if ec.APIVersion != "client.authentication.k8s.io/v1" || ec.Kind != "ExecCredential" {
		t.Fatalf("type meta: %s %s", ec.APIVersion, ec.Kind)
	}
}

func TestBrokerDelegate(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	runs := 0
	b := &Broker{
		Store: NewMemoryStore(),
		Now:   func() time.Time { return now },
		RunExec: func(_ context.Context, cfg provider.ExecConfig, cluster *ExecCluster) ([]byte, error) {
			runs++
			if cluster != nil {
				t.Errorf("cluster = %+v for a plugin that does not ask for it", cluster)
			}
			if cfg.Command != "aws" {
				t.Errorf("command = %q", cfg.Command)
			}
			return json.Marshal(map[string]any{
				"apiVersion": "client.authentication.k8s.io/v1beta1",
				"kind":       "ExecCredential",
				"status":     map[string]any{"token": "k8s-aws-v1.x", "expirationTimestamp": now.Add(10 * time.Minute)},
			})
		},
	}
	if err := b.Put("eks", provider.Delegate("aws", "eks", "get-token")); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		ec, err := b.ExecCredential(context.Background(), "eks", false)
		if err != nil {
			t.Fatal(err)
		}
		if ec.Status.Token != "k8s-aws-v1.x" {
			t.Fatalf("token = %q", ec.Status.Token)
		}
	}
	if runs != 1 {
		t.Fatalf("delegate ran %d times, want 1 (cached by expiry)", runs)
	}
}

// member is the cluster that the cluster info tests authenticate to.
var member = ExecCluster{
	Server:                "https://member-1.example.com:6443",
	CAData:                []byte("-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"),
	TLSServerName:         "member-1.internal",
	InsecureSkipTLSVerify: true,
	ProxyURL:              "http://proxy.example:3128",
}

// secretReader is a delegate credential that, like the cluster inventory's
// secretreader plugin, reads which cluster it is for from its cluster
// config.
func secretReader() provider.Credential {
	c := provider.Delegate("secretreader-plugin")
	c.Exec.ProvideClusterInfo = true
	c.Exec.ClusterConfig = json.RawMessage(`{"clusterName":"member-1"}`)
	return c
}

func TestBrokerDelegateClusterInfo(t *testing.T) {
	const id = "clusterprofile/inv/fleet/member-1"
	errGone := errors.New("cluster not found")
	tests := []struct {
		name     string
		clusters ClusterLookup
		wantErr  error  // matched with errors.Is
		wantMsg  string // "" means success
	}{
		{name: "looked up", clusters: func(clusterID string) (ExecCluster, error) {
			if clusterID != id {
				return ExecCluster{}, fmt.Errorf("lookup of %q, want %q", clusterID, id)
			}
			return member, nil
		}},
		{name: "not in the catalog", clusters: func(string) (ExecCluster, error) { return ExecCluster{}, errGone },
			wantErr: errGone, wantMsg: "looking up the cluster for secretreader-plugin"},
		{name: "no lookup", wantMsg: "asks for cluster information"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got *ExecCluster
			runs := 0
			b := &Broker{
				Store:    NewMemoryStore(),
				Clusters: tt.clusters,
				RunExec: func(_ context.Context, cfg provider.ExecConfig, cluster *ExecCluster) ([]byte, error) {
					runs++
					got = cluster
					if string(cfg.ClusterConfig) != `{"clusterName":"member-1"}` {
						t.Errorf("cluster config = %s", cfg.ClusterConfig)
					}
					return []byte(`{"apiVersion":"client.authentication.k8s.io/v1","kind":"ExecCredential","status":{"token":"from-a-secret"}}`), nil
				},
			}
			if err := b.Put(id, secretReader()); err != nil {
				t.Fatal(err)
			}
			ec, err := b.ExecCredential(context.Background(), id, false)
			if tt.wantMsg != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantMsg) || (tt.wantErr != nil && !errors.Is(err, tt.wantErr)) {
					t.Fatalf("ExecCredential() = %v, want an error containing %q", err, tt.wantMsg)
				}
				if runs != 0 {
					t.Fatalf("the plugin ran %d times without its cluster", runs)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if ec.Status.Token != "from-a-secret" {
				t.Fatalf("token = %q, want from-a-secret", ec.Status.Token)
			}
			if got == nil || !reflect.DeepEqual(*got, member) {
				t.Fatalf("RunExec cluster = %+v, want %+v", got, member)
			}
		})
	}
}

func TestExecInfo(t *testing.T) {
	v1Cluster := clientauthv1.Cluster{
		Server:                   member.Server,
		TLSServerName:            member.TLSServerName,
		InsecureSkipTLSVerify:    true,
		CertificateAuthorityData: member.CAData,
		ProxyURL:                 member.ProxyURL,
	}
	withConfig := v1Cluster
	withConfig.Config = k8sruntime.RawExtension{Raw: []byte(`{"clusterName":"member-1"}`)}
	tests := []struct {
		name        string
		cfg         provider.ExecConfig
		interactive bool
		wantVersion string
		// wantType is the type client-go decodes it to, as a plugin built
		// with client-go would.
		wantType    string
		wantCluster *clientauthv1.Cluster
	}{
		{
			name:        "without cluster info",
			cfg:         provider.ExecConfig{Command: "aws"},
			interactive: true,
			wantVersion: "client.authentication.k8s.io/v1",
		},
		{
			name:        "v1 with cluster info",
			cfg:         *secretReader().Exec,
			wantVersion: "client.authentication.k8s.io/v1",
			wantType:    "*v1.ExecCredential",
			wantCluster: &withConfig,
		},
		{
			name:        "v1beta1 with cluster info and no config",
			cfg:         provider.ExecConfig{Command: "aws", APIVersion: "client.authentication.k8s.io/v1beta1", ProvideClusterInfo: true},
			interactive: true,
			wantVersion: "client.authentication.k8s.io/v1beta1",
			wantType:    "*v1beta1.ExecCredential",
			wantCluster: &v1Cluster,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := execInfo(tt.cfg, &member, tt.interactive)
			if err != nil {
				t.Fatal(err)
			}
			var got clientauthv1.ExecCredential
			if err := json.Unmarshal(b, &got); err != nil {
				t.Fatalf("execInfo() = %s, not an ExecCredential: %v", b, err)
			}
			if got.APIVersion != tt.wantVersion || got.Kind != "ExecCredential" || got.Spec.Interactive != tt.interactive {
				t.Fatalf("execInfo() = %s, want %s with interactive %t", b, tt.wantVersion, tt.interactive)
			}
			if !reflect.DeepEqual(got.Spec.Cluster, tt.wantCluster) {
				t.Fatalf("execInfo() spec.cluster = %+v, want %+v", got.Spec.Cluster, tt.wantCluster)
			}
			if tt.wantCluster == nil {
				return
			}
			obj, cfg, err := authexec.LoadExecCredential(b)
			if err != nil || fmt.Sprintf("%T", obj) != tt.wantType {
				t.Fatalf("LoadExecCredential(%s) = %T, %v; want %s", b, obj, err, tt.wantType)
			}
			if cfg.Host != member.Server || cfg.ServerName != member.TLSServerName || !cfg.Insecure || string(cfg.CAData) != string(member.CAData) {
				t.Fatalf("LoadExecCredential(%s) config = %+v, want the member cluster", b, cfg)
			}
		})
	}

	if _, err := execInfo(*secretReader().Exec, nil, false); err == nil || !strings.Contains(err.Error(), "none was given") {
		t.Fatalf("execInfo() without a cluster = %v, want an error", err)
	}
}

func TestRunExecReplacesKubectlExecInfo(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script plugin")
	}
	dir := t.TempDir()
	plugin := filepath.Join(dir, "secretreader-plugin")
	script := "#!/bin/sh\nprintf '%s' \"$KUBERNETES_EXEC_INFO\" > \"$1\"\n" +
		`echo '{"apiVersion":"client.authentication.k8s.io/v1","kind":"ExecCredential","status":{"token":"from-a-secret"}}'` + "\n"
	if err := os.WriteFile(plugin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	// What kubectl tells tug when it runs `tug auth token`: interactive,
	// and about tug's own kubeconfig cluster.
	t.Setenv("KUBERNETES_EXEC_INFO", `{"apiVersion":"client.authentication.k8s.io/v1","kind":"ExecCredential","spec":{"interactive":true,"cluster":{"server":"https://kubectl.example"}}}`)

	info := filepath.Join(dir, "exec-info.json")
	cfg := *secretReader().Exec
	cfg.Command, cfg.Args, cfg.InteractiveMode = plugin, []string{info}, "Never"
	out, err := RunExec(context.Background(), cfg, &member)
	if err != nil || !strings.Contains(string(out), "from-a-secret") {
		t.Fatalf("RunExec() = %s, %v", out, err)
	}
	b, err := os.ReadFile(info)
	if err != nil {
		t.Fatal(err)
	}
	var got clientauthv1.ExecCredential
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("KUBERNETES_EXEC_INFO = %s: %v", b, err)
	}
	if got.Spec.Interactive || got.Spec.Cluster == nil || got.Spec.Cluster.Server != member.Server ||
		string(got.Spec.Cluster.Config.Raw) != `{"clusterName":"member-1"}` {
		t.Fatalf("KUBERNETES_EXEC_INFO = %s, want the member cluster, not interactive", b)
	}
}

// fakeOIDC answers oidc credentials with a fixed token, or an error.
type fakeOIDC struct {
	err       error
	calls     []provider.Credential
	refreshes int
}

func (f *fakeOIDC) IDToken(_ context.Context, c provider.Credential, refresh bool) (string, time.Time, error) {
	f.calls = append(f.calls, c)
	if refresh {
		f.refreshes++
	}
	if f.err != nil {
		return "", time.Time{}, f.err
	}
	return "id-token", time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC), nil
}

func TestBrokerOIDC(t *testing.T) {
	cred := provider.OIDC("https://issuer", "tug", "groups")
	tests := []struct {
		name     string
		oidc     *fakeOIDC
		wantCode provider.Code
		wantHint string
	}{
		{name: "signed in", oidc: &fakeOIDC{}},
		{name: "nobody signed in", oidc: &fakeOIDC{err: fmt.Errorf("no sign-in: %w", ErrLoginRequired)},
			wantCode: provider.Unauthenticated, wantHint: "tug auth login prov/acct/c"},
		{name: "issuer down", oidc: &fakeOIDC{err: provider.Errorf(provider.Unavailable, "down")}, wantCode: provider.Unavailable},
		{name: "no OIDC client", wantCode: provider.Unimplemented},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// --refresh asks the provider again too.
			fetch := func(context.Context, string) (provider.Credential, error) { return cred, nil }
			b := &Broker{Store: NewMemoryStore(), Fetch: fetch}
			if tt.oidc != nil {
				b.OIDC = tt.oidc
			}
			ec, err := b.ExecCredential(context.Background(), "prov/acct/c", true)
			if tt.wantCode != "" {
				pe := provider.AsError(err)
				if pe.Code != tt.wantCode || !strings.Contains(pe.Hint, tt.wantHint) {
					t.Fatalf("ExecCredential = %+v, want code %s and hint %q", pe, tt.wantCode, tt.wantHint)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if ec.Status.Token != "id-token" || ec.Status.ExpirationTimestamp == nil || ec.Status.ExpirationTimestamp.Year() != 2030 {
				t.Fatalf("status = %+v, want the ID token and its expiry", ec.Status)
			}
			if len(tt.oidc.calls) != 1 || tt.oidc.calls[0].Issuer != "https://issuer" || tt.oidc.refreshes != 1 {
				t.Fatalf("IDToken calls %+v, refreshes %d; want one refreshing call for the stored credential", tt.oidc.calls, tt.oidc.refreshes)
			}
		})
	}
}

// TestOpenFileBackend covers the backends that never touch the OS keychain.
func TestOpenFileBackend(t *testing.T) {
	paths := config.Paths{Data: t.TempDir()}
	wantName := "file:" + filepath.Join(paths.Data, "secrets.enc")
	tests := []struct {
		name       string
		backend    string
		passphrase string
		wantErr    string // "" means the file store is expected
	}{
		{"file", "file", "pass", ""},
		{"file is case-insensitive", "FILE", "pass", ""},
		{"file without passphrase", "file", "", PassphraseEnv},
		{"unknown backend", "vault", "pass", `"vault"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(PassphraseEnv, tt.passphrase)
			s, err := Open(tt.backend, paths)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Open(%q) = %v, %v; want an error containing %q", tt.backend, s, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if s.Name() != wantName {
				t.Fatalf("Open(%q).Name() = %q, want %q", tt.backend, s.Name(), wantName)
			}
		})
	}
}

func TestBrokerForget(t *testing.T) {
	store := NewMemoryStore()
	b := &Broker{Store: store}
	if err := b.Put("c1", provider.Token("t1", provider.NoExpiry)); err != nil {
		t.Fatal(err)
	}
	if err := store.Set(DelegateKey("c1"), "{}"); err != nil {
		t.Fatal(err)
	}
	if err := b.Put("c2", provider.Token("t2", provider.NoExpiry)); err != nil {
		t.Fatal(err)
	}

	if err := b.Forget("c1"); err != nil {
		t.Fatal(err)
	}
	if got, want := store.Keys(), []string{CredentialKey("c2")}; len(got) != 1 || got[0] != want[0] {
		t.Fatalf("store keys after Forget(c1) = %v, want %v", got, want)
	}
	if err := b.Forget("never-stored"); err != nil {
		t.Fatalf("Forget of an unknown cluster = %v, want nil", err)
	}
}

func TestBrokerPutReplacesDelegateCache(t *testing.T) {
	store := NewMemoryStore()
	b := &Broker{Store: store}
	if err := store.Set(DelegateKey("c1"), `{"token":"old"}`); err != nil {
		t.Fatal(err)
	}
	if err := b.Put("c1", provider.Token("new", provider.NoExpiry)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(DelegateKey("c1")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cached delegate output survived Put: %v", err)
	}
	if err := b.Put("c1", provider.Credential{Type: provider.CredentialToken}); err == nil {
		t.Fatal("Put of an invalid credential succeeded")
	}
	if c, err := b.Credential(context.Background(), "c1", false); err != nil || c.Token != "new" {
		t.Fatalf("stored credential after a rejected Put = %+v, %v; want token new", c, err)
	}
}

func TestBrokerCredentialErrors(t *testing.T) {
	errDown := errors.New("provider is down")
	tests := []struct {
		name    string
		stored  string // raw value under CredentialKey, "" for none
		fetch   Fetcher
		wantErr string
		wantIs  error
	}{
		{name: "nothing stored and no fetcher", wantErr: "no stored credential for c1"},
		{
			name:   "fetch fails",
			fetch:  func(context.Context, string) (provider.Credential, error) { return provider.Credential{}, errDown },
			wantIs: errDown,
		},
		{
			name: "provider returns an invalid credential",
			fetch: func(context.Context, string) (provider.Credential, error) {
				return provider.Credential{Type: provider.CredentialToken}, nil
			},
			wantErr: "provider returned an invalid credential",
		},
		{name: "corrupt stored credential is not used", stored: "{not json", wantErr: "no stored credential for c1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := NewMemoryStore()
			if tt.stored != "" {
				if err := store.Set(CredentialKey("c1"), tt.stored); err != nil {
					t.Fatal(err)
				}
			}
			b := &Broker{Store: store, Fetch: tt.fetch}
			_, err := b.Credential(context.Background(), "c1", false)
			if tt.wantIs != nil && !errors.Is(err, tt.wantIs) {
				t.Fatalf("Credential() error = %v, want %v", err, tt.wantIs)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("Credential() error = %v, want one containing %q", err, tt.wantErr)
			}
		})
	}
}
