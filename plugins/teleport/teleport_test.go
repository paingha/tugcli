package teleport

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"maps"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/paingha/tugcli/pkg/conformance"
	"github.com/paingha/tugcli/pkg/provider"
)

const proxyHost = "teleport.example.com"

// tlsRoutingProfile is a tsh profile for a proxy with TLS routing, as tsh
// login writes it.
const tlsRoutingProfile = `web_proxy_addr: teleport.example.com:443
ssh_proxy_addr: teleport.example.com:443
kube_proxy_addr: teleport.example.com:443
user: alice
cluster: root
tls_routing_enabled: true
private_key_policy: none
piv_slot: ""
piv_pin_cache_ttl: 0s
`

// tshScript stands in for tsh. It prints recorded output for the commands
// the plugin runs, and fails on any others, so the tests also check the
// arguments.
const tshScript = `#!/bin/sh
case "$*" in
"status --format=json" | "status --format=json --proxy=teleport.example.com:443")
	cat <<'EOF'
{
  "active": {
    "profile_url": "https://teleport.example.com:443",
    "username": "alice",
    "cluster": "root",
    "roles": ["access"],
    "logins": ["alice"],
    "kubernetes_enabled": true,
    "kubernetes_users": ["alice"],
    "valid_until": "2026-11-21T08:00:00Z"
  },
  "profiles": []
}
EOF
	;;
"kube ls --format=json --proxy=teleport.example.com:443 --cluster=root")
	cat <<'EOF'
[
  {
    "kube_cluster_name": "eks-prod",
    "labels": {"env": "prod", "region": "eu-west-1", "teleport.dev/origin": "cloud"},
    "selected": false,
    "scope": ""
  },
  {
    "kube_cluster_name": "minikube",
    "labels": null,
    "selected": true,
    "scope": ""
  },
  {
    "kube_cluster_name": "minikube",
    "labels": null,
    "selected": false,
    "scope": "/staging"
  }
]
EOF
	;;
"kube ls --format=json --proxy=teleport.example.com:443 --cluster=leaf")
	cat <<'EOF'
[{"kube_cluster_name": "edge", "labels": {"env": "staging"}, "selected": false, "scope": ""}]
EOF
	;;
*)
	echo "ERROR: unexpected arguments: $*" >&2
	exit 1
	;;
esac
`

// authority is a Teleport certificate authority for one cluster.
type authority struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

func newAuthority(t *testing.T, cluster string) authority {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: cluster, Organization: []string{cluster}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return authority{cert: cert, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

// issue returns a user certificate signed by the authority, in PEM.
func (a authority) issue(t *testing.T, user string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: user},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, a.cert, &key.PublicKey, a.key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// tshHome is a tsh profile directory signed in to teleport.example.com,
// whose root cluster "root" trusts a leaf cluster "leaf".
type tshHome struct {
	dir          string
	root, leaf   authority
	userCertName string
}

// newHome writes a tsh profile directory with profileYAML and points
// TELEPORT_HOME at it.
func newHome(t *testing.T, profileYAML string) tshHome {
	t.Helper()
	h := tshHome{dir: t.TempDir(), root: newAuthority(t, "root"), leaf: newAuthority(t, "leaf"), userCertName: "alice.crt"}
	keys := filepath.Join(h.dir, "keys", proxyHost)
	writeFile(t, filepath.Join(h.dir, proxyHost+".yaml"), []byte(profileYAML))
	writeFile(t, filepath.Join(h.dir, "current-profile"), []byte(proxyHost+"\n"))
	writeFile(t, filepath.Join(keys, "cas", "root.pem"), h.root.pem)
	writeFile(t, filepath.Join(keys, "cas", "leaf.pem"), h.leaf.pem)
	writeFile(t, filepath.Join(keys, h.userCertName), h.root.issue(t, "alice"))
	t.Setenv(HomeEnv, h.dir)
	return h
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// fakeTool puts an executable shell script called name first on PATH.
func fakeTool(t *testing.T, name, script string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake " + name + " is a shell script")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func wantDelegate(t *testing.T, got *provider.Credential, home string, args ...string) {
	t.Helper()
	if got == nil || got.Type != provider.CredentialDelegate || got.Exec == nil {
		t.Fatalf("credential = %+v, want a delegate", got)
	}
	e := got.Exec
	if e.Command != "tsh" || !slices.Equal(e.Args, args) || e.APIVersion != execAPIVersion || e.InstallHint != InstallHint {
		t.Errorf("exec = %+v, want tsh %v with apiVersion %s and an install hint", e, args, execAPIVersion)
	}
	if want := map[string]string{HomeEnv: home}; !maps.Equal(e.Env, want) {
		t.Errorf("exec env = %v, want %v", e.Env, want)
	}
}

func TestList(t *testing.T) {
	h := newHome(t, tlsRoutingProfile)
	fakeTool(t, "tsh", tshScript)

	clusters, err := New().List(context.Background(), provider.ListRequest{Account: provider.Account{ID: "work"}})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, c := range clusters {
		ids = append(ids, c.ID)
	}
	// The second cluster named minikube cannot be told apart by tsh kube
	// credentials, so it is skipped.
	if want := []string{"root/eks-prod", "root/minikube"}; !slices.Equal(ids, want) {
		t.Fatalf("List IDs = %v, want %v", ids, want)
	}

	prod := clusters[0]
	if prod.Name != "eks-prod" || prod.Region != "eu-west-1" {
		t.Errorf("eks-prod name and region = %q, %q, want eks-prod, eu-west-1", prod.Name, prod.Region)
	}
	// What tsh kube login writes for a proxy with TLS routing.
	if prod.Endpoints.Public != "https://teleport.example.com:443" || prod.Endpoints.Private != "" {
		t.Errorf("endpoints = %+v, want public https://teleport.example.com:443", prod.Endpoints)
	}
	if want := "kube-teleport-proxy-alpn.teleport.example.com"; prod.TLSServerName != want {
		t.Errorf("TLSServerName = %q, want %q", prod.TLSServerName, want)
	}
	if !bytes.Equal(prod.CAData, h.root.pem) {
		t.Errorf("CAData = %q, want only the root cluster's CA %q", prod.CAData, h.root.pem)
	}
	wantLabels := map[string]string{"env": "prod", "teleport.dev/origin": "cloud", "teleport-cluster": "root"}
	if !maps.Equal(prod.Labels, wantLabels) {
		t.Errorf("labels = %v, want %v", prod.Labels, wantLabels)
	}
	wantDelegate(t, prod.Credential, h.dir,
		"kube", "credentials", "--kube-cluster=eks-prod", "--teleport-cluster=root", "--proxy=teleport.example.com:443")

	if mini := clusters[1]; len(mini.Labels) != 1 || mini.Region != "" {
		t.Errorf("minikube labels and region = %v, %q, want only teleport-cluster", mini.Labels, mini.Region)
	}
}

func TestListLeafCluster(t *testing.T) {
	h := newHome(t, tlsRoutingProfile)
	fakeTool(t, "tsh", tshScript)

	acct := provider.Account{ID: "edge", Config: map[string]any{ConfigProxy: "teleport.example.com:443", ConfigCluster: "leaf"}}
	clusters, err := New().List(context.Background(), provider.ListRequest{Account: acct})
	if err != nil {
		t.Fatal(err)
	}
	if len(clusters) != 1 || clusters[0].ID != "leaf/edge" {
		t.Fatalf("List = %+v, want leaf/edge", clusters)
	}
	edge := clusters[0]
	// kubectl still connects to the root cluster's proxy, so it trusts the
	// root cluster's CA.
	if edge.Endpoints.Public != "https://teleport.example.com:443" || !bytes.Equal(edge.CAData, h.root.pem) {
		t.Errorf("edge endpoint and CA = %q, %q, want the root proxy and its CA", edge.Endpoints.Public, edge.CAData)
	}
	if edge.Labels["teleport-cluster"] != "leaf" || edge.Labels["env"] != "staging" {
		t.Errorf("labels = %v, want teleport-cluster=leaf and env=staging", edge.Labels)
	}
	wantDelegate(t, edge.Credential, h.dir,
		"kube", "credentials", "--kube-cluster=edge", "--teleport-cluster=leaf", "--proxy=teleport.example.com:443")
}

func TestListFailures(t *testing.T) {
	tests := []struct {
		name    string
		profile string
		tsh     string
		code    provider.Code
		hint    string
	}{
		{
			name:    "signed out",
			profile: tlsRoutingProfile,
			tsh:     "#!/bin/sh\necho 'ERROR: Active profile expired.' >&2\nexit 1\n",
			code:    provider.Unauthenticated,
			hint:    "run `tsh login --proxy=<your Teleport proxy>`",
		},
		{
			name:    "load balancer needs a connection upgrade",
			profile: tlsRoutingProfile + "tls_routing_conn_upgrade_required: true\n",
			tsh:     tshScript,
			code:    provider.Unimplemented,
			hint:    "use `tsh proxy kube` or `tsh kubectl` for these clusters",
		},
		{
			name:    "hardware key",
			profile: strings.Replace(tlsRoutingProfile, "private_key_policy: none", "private_key_policy: hardware_key_touch", 1),
			tsh:     tshScript,
			code:    provider.Unimplemented,
			hint:    "use `tsh proxy kube` or `tsh kubectl` for these clusters",
		},
		{
			name:    "no Kubernetes proxy",
			profile: strings.Replace(tlsRoutingProfile, "kube_proxy_addr: teleport.example.com:443\n", "", 1),
			tsh:     tshScript,
			code:    provider.Unavailable,
			hint:    "if Kubernetes access has been enabled since you signed in, run `tsh login --proxy=teleport.example.com:443`",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			newHome(t, tt.profile)
			fakeTool(t, "tsh", tt.tsh)
			_, err := New().List(context.Background(), provider.ListRequest{Account: provider.Account{ID: "work"}})
			if pe := provider.AsError(err); pe.Code != tt.code || pe.Hint != tt.hint {
				t.Errorf("List = %+v, want %s with hint %q", pe, tt.code, tt.hint)
			}
		})
	}
}

func TestListShowsTshMessage(t *testing.T) {
	newHome(t, tlsRoutingProfile)
	fakeTool(t, "tsh", "#!/bin/sh\necho 'WARNING: an update is available' >&2\necho 'ERROR: Not logged in.' >&2\nexit 1\n")
	acct := provider.Account{ID: "work", Config: map[string]any{ConfigProxy: "teleport.example.com"}}
	_, err := New().List(context.Background(), provider.ListRequest{Account: acct})
	pe := provider.AsError(err)
	if want := "tsh status: Not logged in."; pe.Code != provider.Unauthenticated || pe.Message != want {
		t.Errorf("List = %+v, want Unauthenticated %q", pe, want)
	}
	if want := "run `tsh login --proxy=teleport.example.com`"; pe.Hint != want {
		t.Errorf("hint = %q, want %q", pe.Hint, want)
	}
}

func TestListWithoutTsh(t *testing.T) {
	newHome(t, tlsRoutingProfile)
	t.Setenv("PATH", t.TempDir())
	_, err := New().List(context.Background(), provider.ListRequest{Account: provider.Account{ID: "work"}})
	if pe := provider.AsError(err); pe.Code != provider.Unavailable || pe.Hint != InstallHint {
		t.Errorf("List = %+v, want Unavailable with the install hint", pe)
	}
}

func TestEndpoint(t *testing.T) {
	tests := []struct {
		name               string
		prof               profile
		server, serverName string
	}{
		{
			name:   "TLS routing",
			prof:   profile{WebProxyAddr: "teleport.example.com:443", KubeProxyAddr: "teleport.example.com:443", TLSRoutingEnabled: true},
			server: "https://teleport.example.com:443", serverName: "kube-teleport-proxy-alpn.teleport.example.com",
		},
		{
			name:   "TLS routing on an IP address",
			prof:   profile{WebProxyAddr: "192.0.2.10:443", KubeProxyAddr: "192.0.2.10:443", TLSRoutingEnabled: true},
			server: "https://192.0.2.10:443", serverName: "kube-teleport-proxy-alpn.teleport.cluster.local",
		},
		{
			name:   "separate Kubernetes listener",
			prof:   profile{WebProxyAddr: "teleport.example.com:3080", KubeProxyAddr: "kube.example.com:3026"},
			server: "https://kube.example.com:3026",
		},
		{
			name:   "Kubernetes listener without a port",
			prof:   profile{WebProxyAddr: "teleport.example.com:443", KubeProxyAddr: "kube.example.com"},
			server: "https://kube.example.com:3026",
		},
		{
			name:   "relay",
			prof:   profile{WebProxyAddr: "teleport.example.com:443", KubeProxyAddr: "teleport.example.com:443", TLSRoutingEnabled: true, RelayAddr: "relay.example.com:443"},
			server: "https://relay.example.com:443", serverName: "cluster-4ko8c6cp2ubvitaofo0e8fvkc584u2uu4ljn3huvoo2npkbpqtp0.kuberelay.teleport.cluster.local",
		},
		{
			name:   "the cluster's default relay",
			prof:   profile{KubeProxyAddr: "teleport.example.com:443", DefaultRelayAddr: "relay.example.com"},
			server: "https://relay.example.com", serverName: "cluster-4ko8c6cp2ubvitaofo0e8fvkc584u2uu4ljn3huvoo2npkbpqtp0.kuberelay.teleport.cluster.local",
		},
		{
			name:   "relay turned off",
			prof:   profile{KubeProxyAddr: "teleport.example.com:443", TLSRoutingEnabled: true, RelayAddr: "none", DefaultRelayAddr: "relay.example.com"},
			server: "https://teleport.example.com:443", serverName: "kube-teleport-proxy-alpn.teleport.example.com",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// The relay hash is Teleport's own test vector, for this pair of
			// names.
			server, serverName := tt.prof.endpoint("teleport.example.com", "prod-1")
			if server != tt.server || serverName != tt.serverName {
				t.Errorf("endpoint() = %q, %q, want %q, %q", server, serverName, tt.server, tt.serverName)
			}
		})
	}
}

func TestRelayServerName(t *testing.T) {
	// Test vectors from lib/kube/relay/hash_test.go in Teleport.
	tests := []struct{ cluster, kube, want string }{
		{"", "", "cluster-5mt5rf1jjpphdbl2d0vqv0ss3dthtohh7mrp4495h08ors36l8qg"},
		{"8701ae5eecf94a25adc7a90e3f373a25", "3929312bc0c74cd491a882f7abd7edbd", "cluster-1s9atu43ob9i0qhdt63ulnooefbgpdmfpn71f28pcc6q56eo87eg"},
	}
	for _, tt := range tests {
		if got := relayServerName(tt.cluster, tt.kube); got != tt.want+relaySNISuffix {
			t.Errorf("relayServerName(%q, %q) = %q, want %q", tt.cluster, tt.kube, got, tt.want+relaySNISuffix)
		}
	}
}

func TestRootCAs(t *testing.T) {
	tests := []struct {
		name     string
		certName string
		all      bool
		want     func(h tshHome) []byte
	}{
		{"root cluster only", "alice.crt", false, func(h tshHome) []byte { return h.root.pem }},
		{"older tsh certificate name", "alice-x509.pem", false, func(h tshHome) []byte { return h.root.pem }},
		{"every cluster with --load-all-cas", "alice.crt", true, func(h tshHome) []byte { return append(slices.Clone(h.leaf.pem), h.root.pem...) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHome(t, tlsRoutingProfile)
			keys := filepath.Join(h.dir, "keys", proxyHost)
			if tt.certName != h.userCertName {
				if err := os.Rename(filepath.Join(keys, h.userCertName), filepath.Join(keys, tt.certName)); err != nil {
					t.Fatal(err)
				}
			}
			got, err := rootCAs(keys, "alice", tt.all, "teleport.example.com:443")
			if err != nil {
				t.Fatal(err)
			}
			if want := tt.want(h); !bytes.Equal(got, want) {
				t.Errorf("rootCAs() = %q, want %q", got, want)
			}
		})
	}

	t.Run("signed out", func(t *testing.T) {
		h := newHome(t, tlsRoutingProfile)
		keys := filepath.Join(h.dir, "keys", proxyHost)
		if err := os.Remove(filepath.Join(keys, h.userCertName)); err != nil {
			t.Fatal(err)
		}
		_, err := rootCAs(keys, "alice", false, "teleport.example.com:443")
		if pe := provider.AsError(err); pe.Code != provider.Unauthenticated || pe.Hint == "" {
			t.Errorf("rootCAs() = %+v, want Unauthenticated with a hint", pe)
		}
	})
}

func TestCredential(t *testing.T) {
	h := newHome(t, tlsRoutingProfile)
	ctx := context.Background()

	t.Run("current profile", func(t *testing.T) {
		c, err := New().Credential(ctx, provider.CredentialRequest{Account: provider.Account{ID: "work"}, ClusterID: "root/eks-prod"})
		if err != nil {
			t.Fatal(err)
		}
		wantDelegate(t, &c, h.dir, "kube", "credentials", "--kube-cluster=eks-prod", "--teleport-cluster=root", "--proxy=teleport.example.com:443")
	})
	t.Run("account proxy", func(t *testing.T) {
		acct := provider.Account{ID: "edge", Config: map[string]any{ConfigProxy: "teleport.example.com", ConfigCluster: "leaf"}}
		c, err := New().Credential(ctx, provider.CredentialRequest{Account: acct, ClusterID: "leaf/edge"})
		if err != nil {
			t.Fatal(err)
		}
		wantDelegate(t, &c, h.dir, "kube", "credentials", "--kube-cluster=edge", "--teleport-cluster=leaf", "--proxy=teleport.example.com")
	})
	t.Run("no profile", func(t *testing.T) {
		// Without a profile to pin, tsh picks its current profile itself.
		p := &Provider{getenv: func(string) string { return "" }}
		t.Setenv("HOME", t.TempDir())
		t.Setenv("USERPROFILE", t.TempDir())
		c, err := p.Credential(ctx, provider.CredentialRequest{Account: provider.Account{ID: "work"}, ClusterID: "root/eks-prod"})
		if err != nil {
			t.Fatal(err)
		}
		if want := []string{"kube", "credentials", "--kube-cluster=eks-prod", "--teleport-cluster=root"}; !slices.Equal(c.Exec.Args, want) || c.Exec.Env != nil {
			t.Errorf("exec = %+v, want args %v and no env", c.Exec, want)
		}
	})
	for _, id := range []string{"eks-prod", "/eks-prod", "root/"} {
		t.Run("ID "+id, func(t *testing.T) {
			_, err := New().Credential(ctx, provider.CredentialRequest{Account: provider.Account{ID: "work"}, ClusterID: id})
			if provider.CodeOf(err) != provider.NotFound {
				t.Errorf("Credential(%q) = %v, want NotFound", id, err)
			}
		})
	}
}

func TestConformance(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the plugin")
	}
	bin := filepath.Join(t.TempDir(), "tug-provider-teleport")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	if out, err := exec.Command("go", "build", "-o", bin, "./cmd/tug-provider-teleport").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	newHome(t, tlsRoutingProfile)
	fakeTool(t, "tsh", tshScript)
	r := conformance.Run(context.Background(), conformance.Target{Command: bin}, conformance.Options{
		Name:    Name,
		Account: provider.Account{ID: "work"},
	})
	for _, x := range r.Results {
		t.Logf("%-4s %-40s %s", x.Status, x.Name, x.Detail)
	}
	if r.Failed() {
		t.Fatal("the teleport plugin does not conform")
	}
}
