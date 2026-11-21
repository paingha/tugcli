package omni

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"maps"
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
	"github.com/paingha/tugcli/plugins/internal/tugtest"
)

const (
	issuer   = "https://acme.omni.siderolabs.io/oidc"
	proxyURL = "https://acme.kubernetes.omni.siderolabs.io"
	endpoint = "https://acme.omni.siderolabs.io"
)

// omnictlScript stands in for omnictl. It answers the commands the plugin
// runs the way omnictl does: get -o json prints one indented JSON object per
// resource, kubeconfig writes the kubeconfig Omni's template makes to the
// path it is given, and errors come out as cobra prints them. It fails on
// anything else, so the tests also check the arguments. Where it answers,
// it uses only shell builtins and no here-documents, which macOS's sh
// writes to /tmp, so that it also works in tug's sandbox.
const omnictlScript = `#!/bin/sh
[ -n "$FAKE_OMNICTL_LOG" ] && echo "$*" >> "$FAKE_OMNICTL_LOG"
if [ "$BROWSER" != echo ]; then
	echo "Error: BROWSER is '$BROWSER', so omnictl would open a browser" >&2
	exit 1
fi
if [ -n "$OMNI_ENDPOINT" ] && [ "$OMNI_ENDPOINT" != https://acme.omni.siderolabs.io ]; then
	echo 'Error: rpc error: code = Unavailable desc = name resolver error: produced zero addresses' >&2
	exit 1
fi
if [ -n "$OMNI_SERVICE_ACCOUNT_KEY" ]; then
	if [ "$OMNI_SERVICE_ACCOUNT_KEY" != "$FAKE_OMNICTL_KEY" ] || [ -z "$OMNI_ENDPOINT" ]; then
		echo "Could not authenticate: rpc error: code = Unauthenticated desc = invalid signature" >&2
		exec sleep 30
	fi
fi
case "$FAKE_OMNICTL_STATE" in
signed-out)
	echo "Could not authenticate: failed to get signer: open /home/alice/.talos/keys/default-alice@example.com.pgp: no such file or directory" >&2
	echo "Please visit this page to authenticate: https://acme.omni.siderolabs.io/authenticate?flow=cli&public-key-id=0f3a" >&2
	exec sleep 30
	;;
unconfigured)
	echo 'Error: context "default" has not been configured, you will need to set it manually' >&2
	exit 1
	;;
esac
case "$1" in
get)
	if [ "$*" != "get clusters -o json" ] && [ "$*" != "get clusters -o json --context=staging" ]; then
		echo "Error: unexpected arguments: $*" >&2
		exit 1
	fi
	[ "$FAKE_OMNICTL_STATE" = empty ] && exit 0
	printf '%s\n' '
{
    "metadata": {
        "created": "2026-10-01T09:12:44Z",
        "finalizers": [
            "ClusterController",
            "ClusterStatusController"
        ],
        "id": "talos-prod",
        "labels": {
            "env": "prod",
            "Team": "payments",
            "omni.sidero.dev/cluster-template": "prod"
        },
        "namespace": "default",
        "owner": null,
        "phase": "running",
        "type": "Clusters.omni.sidero.dev",
        "updated": "2026-11-02T14:03:10Z",
        "version": 7
    },
    "spec": {
        "backupconfiguration": null,
        "features": {
            "diskencryption": true,
            "enableworkloadproxy": false,
            "useembeddeddiscoveryservice": true
        },
        "installimage": "",
        "kubernetesversion": "1.34.2",
        "talosversion": "1.11.5"
    }
}
{
    "metadata": {
        "created": "2026-11-18T08:00:00Z",
        "id": "edge-dev",
        "namespace": "default",
        "owner": null,
        "phase": "running",
        "type": "Clusters.omni.sidero.dev",
        "updated": "2026-11-18T08:00:00Z",
        "version": 2
    },
    "spec": {
        "backupconfiguration": null,
        "features": null,
        "installimage": "",
        "kubernetesversion": "1.33.6",
        "talosversion": "1.10.7"
    }
}
{
    "metadata": {
        "created": "2025-03-01T08:00:00Z",
        "id": "retired",
        "namespace": "default",
        "owner": null,
        "phase": "tearingDown",
        "type": "Clusters.omni.sidero.dev",
        "updated": "2026-11-20T08:00:00Z",
        "version": 31
    },
    "spec": {
        "kubernetesversion": "1.30.1",
        "talosversion": "1.7.4"
    }
}
'
	;;
kubeconfig)
	path=$2
	shift 2
	cluster= user= ttl= groups= service=
	for a in "$@"; do
		case "$a" in
		--cluster=*) cluster=${a#--cluster=} ;;
		--service-account) service=1 ;;
		--user=*) user=${a#--user=} ;;
		--ttl=*) ttl=${a#--ttl=} ;;
		--groups=*) groups=${a#--groups=} ;;
		--merge=false | --context=staging) ;;
		*)
			echo "Error: unknown flag: $a" >&2
			exit 1
			;;
		esac
	done
	case "$cluster" in
	talos-prod | edge-dev) ;;
	*)
		echo "Error: cluster not found: \"$cluster\"" >&2
		exit 1
		;;
	esac
	if [ -n "$service" ]; then
		if [ "$user" = intruder ]; then
			echo 'Error: rpc error: code = PermissionDenied desc = failed to validate: insufficient role: "Reader"' >&2
			exit 1
		fi
		printf '%s\n' "
apiVersion: v1
clusters:
- cluster:
    server: https://acme.kubernetes.omni.siderolabs.io
  name: acme-$cluster-$user
contexts:
- context:
    cluster: acme-$cluster-$user
    namespace: default
    user: acme-$cluster-$user
  name: acme-$cluster-$user
current-context: acme-$cluster-$user
kind: Config
preferences: {}
users:
- name: acme-$cluster-$user
  user:
    token: $FAKE_OMNICTL_TOKEN
" >"$path"
		exit 0
	fi
	printf '%s\n' "
apiVersion: v1
kind: Config
clusters:
  - cluster:
      server: https://acme.kubernetes.omni.siderolabs.io
    name: acme-$cluster
contexts:
  - context:
      cluster: acme-$cluster
      namespace: default
      user: acme-$cluster-alice@example.com
    name: acme-$cluster
current-context: acme-$cluster
users:
- name: acme-$cluster-alice@example.com
  user:
    exec:
      apiVersion: client.authentication.k8s.io/v1
      args:
        - oidc-login
        - get-token
        - --oidc-issuer-url=https://acme.omni.siderolabs.io/oidc
        - --oidc-client-id=native
        - --oidc-extra-scope=cluster:$cluster
      command: kubectl
      env: null
      interactiveMode: IfAvailable
      provideClusterInfo: false
" >"$path"
	;;
*)
	echo "Error: unknown command \"$1\" for \"omnictl\"" >&2
	exit 1
	;;
esac
`

// fakeOmnictl puts the fake omnictl first on PATH and returns the file it
// logs its arguments to, one call per line.
func fakeOmnictl(t *testing.T, state string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake omnictl is a shell script")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "omnictl"), []byte(omnictlScript), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	log := filepath.Join(t.TempDir(), "omnictl.log")
	t.Setenv("FAKE_OMNICTL_LOG", log)
	t.Setenv("FAKE_OMNICTL_STATE", state)
	return log
}

// calls returns the omnictl calls logged, with temporary paths replaced by
// <path>.
func calls(t *testing.T, log string) []string {
	t.Helper()
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for line := range strings.Lines(string(data)) {
		fields := strings.Fields(line)
		if len(fields) > 1 && fields[0] == "kubeconfig" {
			fields[1] = "<path>"
		}
		out = append(out, strings.Join(fields, " "))
	}
	return out
}

// serviceAccountKey returns a key in the form omnictl serviceaccount create
// prints: base64 of the account's name and armoured PGP private key.
func serviceAccountKey(t *testing.T, name string) string {
	t.Helper()
	data, err := json.Marshal(serviceAccount{
		Name:   name,
		PGPKey: "-----BEGIN PGP PRIVATE KEY BLOCK-----\n\nxVgEZnRk0RYJKwYBBAHaRw8BAQdA\n-----END PGP PRIVATE KEY BLOCK-----\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(data)
}

// serviceAccountJWT returns a token shaped like the ones Omni signs for
// service account kubeconfigs.
func serviceAccountJWT(t *testing.T, exp time.Time) string {
	t.Helper()
	claims, err := json.Marshal(map[string]any{
		"iat": exp.Add(-time.Hour).Unix(), "exp": exp.Unix(), "iss": "omni-acme-service-account-issuer",
		"sub": "ci-bot", "groups": []string{"system:masters"}, "cluster": "talos-prod",
	})
	if err != nil {
		t.Fatal(err)
	}
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"ES256","kid":"0f3a","typ":"JWT"}`)) + "." + enc(claims) + "." + enc([]byte("signature"))
}

func wantOIDC(t *testing.T, got *provider.Credential, cluster string) {
	t.Helper()
	if got == nil || got.Type != provider.CredentialOIDC || got.Issuer != issuer || got.ClientID != "native" ||
		!slices.Equal(got.Scopes, []string{"cluster:" + cluster}) || got.ClientSecret != "" || got.ExpiresAt != nil {
		t.Errorf("credential = %+v, want OIDC for %s with client native and scope cluster:%s", got, issuer, cluster)
	}
}

func TestList(t *testing.T) {
	log := fakeOmnictl(t, "")
	// Neither may reach omnictl: the user's own key would change who signs
	// in, and a browser must not open from a sync.
	t.Setenv("OMNI_SERVICE_ACCOUNT_KEY", "exported-in-the-shell")
	t.Setenv("BROWSER", "firefox")

	clusters, err := New().List(context.Background(), provider.ListRequest{Account: provider.Account{ID: "acme"}})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, c := range clusters {
		ids = append(ids, c.ID)
	}
	if want := []string{"talos-prod", "edge-dev"}; !slices.Equal(ids, want) {
		t.Fatalf("List IDs = %v, want %v without the cluster being torn down", ids, want)
	}
	prod := clusters[0]
	if prod.Name != "talos-prod" || prod.Version != "1.34.2" || prod.Endpoints.Public != proxyURL || prod.Endpoints.Private != "" || prod.CAData != nil {
		t.Errorf("talos-prod = %+v, want version 1.34.2 behind %s", prod, proxyURL)
	}
	if want := map[string]string{"env": "prod", "team": "payments", "talos-version": "1.11.5"}; !maps.Equal(prod.Labels, want) {
		t.Errorf("talos-prod labels = %v, want %v", prod.Labels, want)
	}
	wantOIDC(t, prod.Credential, "talos-prod")
	dev := clusters[1]
	if dev.Endpoints.Public != proxyURL || !maps.Equal(dev.Labels, map[string]string{"talos-version": "1.10.7"}) {
		t.Errorf("edge-dev = %+v", dev)
	}
	wantOIDC(t, dev.Credential, "edge-dev")

	// One kubeconfig serves every cluster.
	if got, want := calls(t, log), []string{"get clusters -o json", "kubeconfig <path> --cluster=talos-prod --merge=false"}; !slices.Equal(got, want) {
		t.Errorf("omnictl calls = %q, want %q", got, want)
	}
}

func TestListContext(t *testing.T) {
	log := fakeOmnictl(t, "")
	acct := provider.Account{ID: "staging", Config: map[string]any{ConfigContext: "staging"}}
	clusters, err := New().List(context.Background(), provider.ListRequest{Account: acct})
	if err != nil || len(clusters) != 2 {
		t.Fatalf("List = %+v, %v", clusters, err)
	}
	want := []string{"get clusters -o json --context=staging", "kubeconfig <path> --cluster=talos-prod --merge=false --context=staging"}
	if got := calls(t, log); !slices.Equal(got, want) {
		t.Errorf("omnictl calls = %q, want %q", got, want)
	}
}

func TestListEndpoint(t *testing.T) {
	fakeOmnictl(t, "")
	tests := []struct {
		endpoint string
		want     provider.Code
	}{
		{endpoint, ""},
		{"https://elsewhere.example.com", provider.Unavailable},
	}
	for _, tt := range tests {
		t.Run(tt.endpoint, func(t *testing.T) {
			acct := provider.Account{ID: "acme", Config: map[string]any{ConfigEndpoint: tt.endpoint}}
			clusters, err := New().List(context.Background(), provider.ListRequest{Account: acct})
			if tt.want == "" && (err != nil || len(clusters) != 2) {
				t.Errorf("List = %+v, %v, want both clusters", clusters, err)
			}
			if tt.want != "" && provider.CodeOf(err) != tt.want {
				t.Errorf("List = %v, want %s from omnictl reaching %s", err, tt.want, tt.endpoint)
			}
		})
	}
}

func TestListEmpty(t *testing.T) {
	log := fakeOmnictl(t, "empty")
	clusters, err := New().List(context.Background(), provider.ListRequest{Account: provider.Account{ID: "acme"}})
	if err != nil || clusters == nil || len(clusters) != 0 {
		t.Fatalf("List = %#v, %v, want an empty listing", clusters, err)
	}
	if got := calls(t, log); len(got) != 1 {
		t.Errorf("omnictl calls = %q, want only get clusters", got)
	}
}

func TestSignedOut(t *testing.T) {
	fakeOmnictl(t, "signed-out")
	start := time.Now()
	acct := provider.Account{ID: "staging", Config: map[string]any{ConfigContext: "staging"}}
	_, err := New().List(context.Background(), provider.ListRequest{Account: acct})
	pe := provider.AsError(err)
	if pe.Code != provider.Unauthenticated || pe.Hint != "run `omnictl get clusters --context=staging` in a terminal to sign in to Omni" {
		t.Errorf("List = %+v, want Unauthenticated with a hint to sign in", pe)
	}
	// omnictl would wait for the browser; the plugin stops it instead.
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("List took %s, want omnictl stopped as soon as it starts to sign in", d)
	}
}

func TestListFailures(t *testing.T) {
	t.Run("no omniconfig", func(t *testing.T) {
		fakeOmnictl(t, "unconfigured")
		_, err := New().List(context.Background(), provider.ListRequest{Account: provider.Account{ID: "acme"}})
		pe := provider.AsError(err)
		if want := `omnictl get clusters: context "default" has not been configured, you will need to set it manually`; pe.Code != provider.InvalidConfig || pe.Message != want {
			t.Errorf("List = %+v, want InvalidConfig %q", pe, want)
		}
		if !strings.Contains(pe.Hint, "omnictl config merge") {
			t.Errorf("hint = %q, want how to add the omniconfig", pe.Hint)
		}
	})
	t.Run("no omnictl", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		_, err := New().List(context.Background(), provider.ListRequest{Account: provider.Account{ID: "acme"}})
		if pe := provider.AsError(err); pe.Code != provider.Unavailable || pe.Hint != InstallHint {
			t.Errorf("List = %+v, want Unavailable with the install hint", pe)
		}
	})
}

func TestCredential(t *testing.T) {
	log := fakeOmnictl(t, "")
	c, err := New().Credential(context.Background(), provider.CredentialRequest{Account: provider.Account{ID: "acme"}, ClusterID: "edge-dev"})
	if err != nil {
		t.Fatal(err)
	}
	wantOIDC(t, &c, "edge-dev")
	if got, want := calls(t, log), []string{"kubeconfig <path> --cluster=edge-dev --merge=false"}; !slices.Equal(got, want) {
		t.Errorf("omnictl calls = %q, want %q", got, want)
	}

	_, err = New().Credential(context.Background(), provider.CredentialRequest{Account: provider.Account{ID: "acme"}, ClusterID: "gone"})
	if pe := provider.AsError(err); pe.Code != provider.NotFound || pe.Message != `omnictl kubeconfig: cluster not found: "gone"` || !strings.Contains(pe.Hint, "tug sync") {
		t.Errorf("Credential for a deleted cluster = %+v, want NotFound with a hint", pe)
	}
}

func TestServiceAccount(t *testing.T) {
	log := fakeOmnictl(t, "")
	key := serviceAccountKey(t, "ci-bot")
	exp := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	t.Setenv("FAKE_OMNICTL_KEY", key)
	t.Setenv("FAKE_OMNICTL_TOKEN", serviceAccountJWT(t, exp))
	ctx := context.Background()
	secrets := map[string]string{SecretServiceAccountKey: key + "\n"}
	acct := func(extra map[string]any) provider.Account {
		cfg := map[string]any{ConfigEndpoint: endpoint}
		maps.Copy(cfg, extra)
		return provider.Account{ID: "ci", Config: cfg}
	}

	clusters, err := New().List(ctx, provider.ListRequest{Account: acct(nil), Secrets: secrets})
	if err != nil || len(clusters) != 2 {
		t.Fatalf("List = %+v, %v", clusters, err)
	}
	for _, c := range clusters {
		if c.Credential != nil || c.Endpoints.Public != proxyURL {
			t.Errorf("%s = %+v, want the proxy and no token made while listing", c.ID, c)
		}
	}

	tests := []struct {
		name      string
		config    map[string]any
		wantFlags string
	}{
		{"defaults", nil, "--service-account --user=ci-bot --ttl=1h0m0s"},
		{"settings", map[string]any{ConfigServiceAccountUser: "deployer", ConfigServiceAccountGroups: " dev, ops ,", ConfigTokenTTL: "8h"},
			"--service-account --user=deployer --ttl=8h0m0s --groups=dev,ops"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := os.Remove(log); err != nil {
				t.Fatal(err)
			}
			c, err := New().Credential(ctx, provider.CredentialRequest{Account: acct(tt.config), ClusterID: "talos-prod", Secrets: secrets})
			if err != nil {
				t.Fatal(err)
			}
			if c.Type != provider.CredentialToken || c.Token != os.Getenv("FAKE_OMNICTL_TOKEN") || c.ExpiresAt == nil || !c.ExpiresAt.Equal(exp) {
				t.Errorf("Credential = %+v, want the service account token expiring %s", c, exp)
			}
			if got, want := calls(t, log), []string{"kubeconfig <path> --cluster=talos-prod --merge=false " + tt.wantFlags}; !slices.Equal(got, want) {
				t.Errorf("omnictl calls = %q, want %q", got, want)
			}
		})
	}

	t.Run("token without an expiry", func(t *testing.T) {
		t.Setenv("FAKE_OMNICTL_TOKEN", "opaque-token")
		now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
		p := New()
		p.now = func() time.Time { return now }
		c, err := p.Credential(ctx, provider.CredentialRequest{Account: acct(map[string]any{ConfigTokenTTL: "30m"}), ClusterID: "talos-prod", Secrets: secrets})
		if err != nil || c.ExpiresAt == nil || !c.ExpiresAt.Equal(now.Add(30*time.Minute)) {
			t.Errorf("Credential = %+v, %v, want the token until the TTL runs out", c, err)
		}
	})
	t.Run("role too weak", func(t *testing.T) {
		_, err := New().Credential(ctx, provider.CredentialRequest{Account: acct(map[string]any{ConfigServiceAccountUser: "intruder"}), ClusterID: "talos-prod", Secrets: secrets})
		if pe := provider.AsError(err); pe.Code != provider.PermissionDenied || !strings.Contains(pe.Hint, "Operator") {
			t.Errorf("Credential = %+v, want PermissionDenied with a hint about the role", pe)
		}
	})
	t.Run("revoked key", func(t *testing.T) {
		other := serviceAccountKey(t, "revoked")
		_, err := New().List(ctx, provider.ListRequest{Account: acct(nil), Secrets: map[string]string{SecretServiceAccountKey: other}})
		if pe := provider.AsError(err); pe.Code != provider.Unauthenticated || !strings.Contains(pe.Hint, "omnictl serviceaccount create") {
			t.Errorf("List with a revoked key = %+v, want Unauthenticated with a hint", pe)
		}
	})
}

func TestSettingErrors(t *testing.T) {
	key := serviceAccountKey(t, "ci-bot")
	tests := []struct {
		name    string
		config  map[string]any
		secrets map[string]string
		want    provider.Code
	}{
		{"not a key", map[string]any{ConfigEndpoint: endpoint}, map[string]string{SecretServiceAccountKey: "tug-conformance-invalid"}, provider.Unauthenticated},
		{"not a key and no endpoint", nil, map[string]string{SecretServiceAccountKey: base64.StdEncoding.EncodeToString([]byte(`{"name":"x"}`))}, provider.Unauthenticated},
		{"key without an endpoint", nil, map[string]string{SecretServiceAccountKey: key}, provider.InvalidConfig},
		{"plain http endpoint", map[string]any{ConfigEndpoint: "http://acme.omni.siderolabs.io"}, map[string]string{SecretServiceAccountKey: key}, provider.InvalidConfig},
		{"plain http endpoint without a key", map[string]any{ConfigEndpoint: "http://acme.omni.siderolabs.io"}, nil, provider.InvalidConfig},
		{"bad tokenTTL", map[string]any{ConfigEndpoint: endpoint, ConfigTokenTTL: "a day"}, map[string]string{SecretServiceAccountKey: key}, provider.InvalidConfig},
		{"tokenTTL too long", map[string]any{ConfigEndpoint: endpoint, ConfigTokenTTL: "100000h"}, map[string]string{SecretServiceAccountKey: key}, provider.InvalidConfig},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New().Credential(context.Background(), provider.CredentialRequest{Account: provider.Account{ID: "ci", Config: tt.config}, ClusterID: "talos-prod", Secrets: tt.secrets})
			if provider.CodeOf(err) != tt.want {
				t.Errorf("Credential = %v, want %s", err, tt.want)
			}
		})
	}
}

func TestParseKubeconfig(t *testing.T) {
	data := []byte(`apiVersion: v1
clusters:
- name: self-hosted-prod
  cluster:
    server: https://omni.internal.example.com:8095/
    certificate-authority-data: ` + base64.StdEncoding.EncodeToString([]byte("CA")) + `
users:
- name: self-hosted-prod-alice
  user:
    exec:
      command: kubectl
      args: [oidc-login, get-token, --oidc-issuer-url, "https://omni.internal.example.com/oidc", --oidc-client-id=native, "--oidc-extra-scope=cluster:prod,groups", --grant-type=authcode-keyboard]
`)
	acc, err := parseKubeconfig(data)
	if err != nil {
		t.Fatal(err)
	}
	if acc.server != "https://omni.internal.example.com:8095" || string(acc.ca) != "CA" || acc.issuer != "https://omni.internal.example.com/oidc" ||
		acc.clientID != "native" || !slices.Equal(acc.scopes, []string{"cluster:prod", "groups"}) {
		t.Errorf("parseKubeconfig = %+v", acc)
	}
	c, err := acc.oidc("dev")
	if err != nil || !slices.Equal(c.Scopes, []string{"groups", "cluster:dev"}) {
		t.Errorf("oidc(dev) = %+v, %v, want the cluster scope replaced", c, err)
	}

	if _, err := parseKubeconfig([]byte("apiVersion: v1\nclusters: []\n")); err == nil {
		t.Error("parseKubeconfig without a server = nil error")
	}
	acc, err = parseKubeconfig([]byte("clusters:\n- cluster: {server: https://x.example.com}\nusers:\n- user: {token: abc}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acc.oidc("x"); provider.CodeOf(err) != provider.Internal {
		t.Errorf("oidc for a token kubeconfig = %v, want Internal", err)
	}
}

func TestJWTExpiry(t *testing.T) {
	exp := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	enc := base64.RawURLEncoding.EncodeToString
	tests := []struct {
		name   string
		token  string
		want   time.Time
		wantOK bool
	}{
		{"service account token", serviceAccountJWT(t, exp), exp, true},
		{"no exp claim", enc([]byte(`{}`)) + "." + enc([]byte(`{"sub":"x"}`)) + ".sig", time.Time{}, false},
		{"not a JWT", "opaque", time.Time{}, false},
		{"payload not base64", "a.!!!.c", time.Time{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := jwtExpiry(tt.token)
			if ok != tt.wantOK || !got.Equal(tt.want) {
				t.Errorf("jwtExpiry = %v, %v, want %v, %v", got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestErrorCode(t *testing.T) {
	tests := []struct {
		msg  string
		want provider.Code
	}{
		{"rpc error: code = Unauthenticated desc = signature expired", provider.Unauthenticated},
		{"rpc error: code = PermissionDenied desc = insufficient role", provider.PermissionDenied},
		{`cluster not found: "x"`, provider.NotFound},
		{"rpc error: code = ResourceExhausted desc = rate limit exceeded", provider.RateLimited},
		{"context not found: work", provider.InvalidConfig},
		{"rpc error: code = Unavailable desc = connection refused", provider.Unavailable},
	}
	for _, tt := range tests {
		if got := errorCode(tt.msg, provider.Unavailable); got != tt.want {
			t.Errorf("errorCode(%q) = %s, want %s", tt.msg, got, tt.want)
		}
	}
}

func TestConformance(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the plugin")
	}
	bin := filepath.Join(t.TempDir(), "tug-provider-omni")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	if out, err := exec.Command("go", "build", "-o", bin, "./cmd/tug-provider-omni").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	fakeOmnictl(t, "")
	r := conformance.Run(context.Background(), conformance.Target{Command: bin}, conformance.Options{
		Name:            Name,
		Account:         provider.Account{ID: "acme"},
		DeclaredSecrets: New().Info().Secrets,
	})
	for _, x := range r.Results {
		t.Logf("%-4s %-40s %s", x.Status, x.Name, x.Detail)
	}
	if r.Failed() {
		t.Fatal("the omni plugin does not conform")
	}
	tugtest.ConformInSandbox(t, tugtest.Plugin{Name: Name, Manifest: "plugin.yaml", Bin: bin})
}
