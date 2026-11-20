package argocd

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/rest"

	"github.com/paingha/tugcli/pkg/provider"
)

const mgmtToken = "mgmt-token"

var now = time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

// fakeSecret is an Argo CD Secret: its labels and its string data.
type fakeSecret struct {
	name   string
	labels map[string]string
	data   map[string]string
}

// fakeAPI serves Secrets in one namespace to callers presenting mgmtToken.
type fakeAPI struct {
	namespace string
	secrets   []fakeSecret
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Header.Get("Authorization") != "Bearer "+mgmtToken {
		status(w, http.StatusUnauthorized, "Unauthorized")
		return
	}
	// /api/v1/namespaces/<namespace>/secrets[/<name>]
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	isSecrets := len(parts) >= 5 && parts[0] == "api" && parts[2] == "namespaces" && parts[4] == "secrets"
	switch {
	case r.Method != http.MethodGet:
		status(w, http.StatusMethodNotAllowed, "MethodNotAllowed")
	case isSecrets && len(parts) == 5:
		// A real API server lists any namespace, existing or not.
		sel, err := labels.Parse(r.URL.Query().Get("labelSelector"))
		if err != nil {
			status(w, http.StatusBadRequest, "BadRequest")
			return
		}
		items := []any{}
		for _, s := range f.secrets {
			if parts[3] == f.namespace && sel.Matches(labels.Set(s.labels)) {
				items = append(items, f.object(s))
			}
		}
		writeJSON(w, map[string]any{"kind": "SecretList", "apiVersion": "v1", "metadata": map[string]any{}, "items": items})
	case isSecrets && len(parts) == 6:
		i := slices.IndexFunc(f.secrets, func(s fakeSecret) bool { return s.name == parts[5] })
		if i < 0 || parts[3] != f.namespace {
			status(w, http.StatusNotFound, "NotFound")
			return
		}
		writeJSON(w, f.object(f.secrets[i]))
	default:
		status(w, http.StatusNotFound, "NotFound")
	}
}

func (f *fakeAPI) object(s fakeSecret) map[string]any {
	data := map[string][]byte{}
	for k, v := range s.data {
		data[k] = []byte(v)
	}
	return map[string]any{
		"kind": "Secret", "apiVersion": "v1", "type": "Opaque",
		"metadata": map[string]any{"name": s.name, "namespace": f.namespace, "labels": s.labels},
		"data":     data,
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	_ = json.NewEncoder(w).Encode(v)
}

func status(w http.ResponseWriter, code int, reason string) {
	w.WriteHeader(code)
	writeJSON(w, map[string]any{"kind": "Status", "apiVersion": "v1", "status": "Failure", "reason": reason, "code": code, "message": strings.ToLower(reason)})
}

// clusterSecret returns an Argo CD cluster Secret with the given config.
func clusterSecret(name, server string, config map[string]any, extra map[string]string) fakeSecret {
	b, _ := json.Marshal(config)
	s := fakeSecret{
		name:   name,
		labels: map[string]string{secretTypeLabel: secretTypeValue},
		data:   map[string]string{"server": server, "config": string(b)},
	}
	for k, v := range extra {
		if after, ok := strings.CutPrefix(k, "label:"); ok {
			s.labels[after] = v
			continue
		}
		s.data[k] = v
	}
	return s
}

func testCert(t *testing.T, notAfter time.Time) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "argocd"}, NotBefore: notAfter.Add(-24 * time.Hour), NotAfter: notAfter}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

func newFake(t *testing.T, namespace string) (*fakeAPI, *Provider) {
	t.Helper()
	certPEM, keyPEM := testCert(t, now.Add(48*time.Hour))
	f := &fakeAPI{namespace: namespace, secrets: []fakeSecret{
		clusterSecret("cluster-prod", "https://prod.example.com/", map[string]any{
			"bearerToken":     "argocd-sa-token",
			"tlsClientConfig": map[string]any{"insecure": false, "caData": []byte("CA-PEM"), "serverName": "prod.internal"},
		}, map[string]string{"name": "prod-eu", "project": "payments", "namespaces": "payments", "label:env": "prod"}),
		clusterSecret("cluster-eks", "https://0123ABCD.gr7.eu-west-1.eks.amazonaws.com", map[string]any{
			"awsAuthConfig":   map[string]any{"clusterName": "prod-eks", "roleARN": "arn:aws:iam::111122223333:role/argocd", "profile": "/mnt/aws/profile"},
			"tlsClientConfig": map[string]any{"caData": []byte("EKS-CA")},
		}, nil),
		clusterSecret("cluster-gke", "https://34.1.2.3", map[string]any{
			"execProviderConfig": map[string]any{"command": "argocd-k8s-auth", "args": []string{"gcp"}, "env": map[string]string{"GOOGLE_PROJECT": "acme"},
				"apiVersion": "client.authentication.k8s.io/v1beta1", "installHint": "install argocd-k8s-auth"},
		}, map[string]string{"namespaces": "a,b"}),
		clusterSecret("cluster-cert", "https://cert.example.com:6443", map[string]any{
			"tlsClientConfig": map[string]any{"insecure": true, "certData": certPEM, "keyData": keyPEM},
			"proxyUrl":        "http://proxy.example.com:3128",
		}, nil),
		clusterSecret("cluster-basic", "https://basic.example.com", map[string]any{"username": "admin", "password": "hunter2"}, nil),
		clusterSecret("cluster-info", "https://info.example.com", map[string]any{
			"execProviderConfig": map[string]any{"command": "plugin", "provideClusterInfo": true},
		}, nil),
		clusterSecret("in-cluster", "https://kubernetes.default.svc", map[string]any{}, map[string]string{"name": "in-cluster"}),
		clusterSecret("no-server", "", map[string]any{}, nil),
		{name: "repo-private", labels: map[string]string{secretTypeLabel: "repository"}, data: map[string]string{"url": "https://git.example.com/x", "password": "repo-pass"}},
	}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	p := New(WithNow(func() time.Time { return now }), WithClusterConfig(func(_ context.Context, name string) (*rest.Config, error) {
		if name != "mgmt" {
			return nil, provider.Errorf(provider.NotFound, "%s is not in the catalog", name)
		}
		return &rest.Config{Host: srv.URL, BearerToken: mgmtToken}, nil
	}))
	return f, p
}

func account(namespace string) provider.Account {
	cfg := map[string]any{ConfigCluster: "mgmt"}
	if namespace != "" {
		cfg[ConfigNamespace] = namespace
	}
	return provider.Account{ID: "argocd", Config: cfg}
}

func TestList(t *testing.T) {
	_, p := newFake(t, DefaultNamespace)
	clusters, err := p.List(context.Background(), provider.ListRequest{Account: account("")})
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]provider.Cluster{}
	var ids []string
	for _, c := range clusters {
		byID[c.ID] = c
		ids = append(ids, c.ID)
	}
	if want := []string{"cluster-prod", "cluster-eks", "cluster-gke", "cluster-cert", "cluster-basic", "cluster-info"}; !slices.Equal(ids, want) {
		t.Fatalf("List IDs = %v, want %v", ids, want)
	}
	prod := byID["cluster-prod"]
	if prod.Name != "prod-eu" || prod.Endpoints.Public != "https://prod.example.com" || string(prod.CAData) != "CA-PEM" ||
		prod.TLSServerName != "prod.internal" || prod.DefaultNamespace != "payments" ||
		prod.Labels["env"] != "prod" || prod.Labels[LabelProject] != "payments" || prod.Labels[secretTypeLabel] != "" {
		t.Fatalf("cluster-prod = %+v", prod)
	}
	if prod.Credential == nil || prod.Credential.Token != "argocd-sa-token" || !prod.Credential.ExpiresAt.Equal(now.Add(credentialTTL)) {
		t.Fatalf("cluster-prod credential = %+v", prod.Credential)
	}
	if gke := byID["cluster-gke"]; gke.Name != "cluster-gke" || gke.DefaultNamespace != "" {
		t.Fatalf("cluster-gke = %+v, want the Secret's name and no default namespace", gke)
	}
	if cert := byID["cluster-cert"]; !cert.InsecureSkipTLSVerify || cert.ProxyURL != "http://proxy.example.com:3128" {
		t.Fatalf("cluster-cert = %+v", cert)
	}
	if byID["cluster-basic"].Credential != nil || byID["cluster-info"].Credential != nil {
		t.Fatal("clusters with unsupported credentials carry one inline")
	}
}

func TestListNamespaceAndErrors(t *testing.T) {
	_, p := newFake(t, "gitops")
	clusters, err := p.List(context.Background(), provider.ListRequest{Account: account("gitops")})
	if err != nil || len(clusters) != 6 {
		t.Fatalf("List(namespace=gitops) = %d clusters, %v; want 6", len(clusters), err)
	}
	// The default namespace has no Secrets here.
	if clusters, err := p.List(context.Background(), provider.ListRequest{Account: account("")}); err != nil || len(clusters) != 0 {
		t.Fatalf("List(namespace=argocd) = %d clusters, %v; want none", len(clusters), err)
	}

	f, p := newFake(t, DefaultNamespace)
	f.secrets = append(f.secrets, fakeSecret{name: "cluster-bad", labels: map[string]string{secretTypeLabel: secretTypeValue},
		data: map[string]string{"server": "https://bad.example.com", "config": "{bearerToken"}})
	_, err = p.List(context.Background(), provider.ListRequest{Account: account("")})
	if pe := provider.AsError(err); pe.Code != provider.InvalidConfig || !strings.Contains(pe.Message, "argocd/cluster-bad") {
		t.Fatalf("List with a malformed config = %v, want InvalidConfig naming the Secret", err)
	}

	_, err = p.List(context.Background(), provider.ListRequest{Account: provider.Account{ID: "argocd"}})
	if pe := provider.AsError(err); pe.Code != provider.InvalidConfig || !strings.Contains(pe.Hint, "--set cluster=") {
		t.Fatalf("List without a cluster setting = %+v, want InvalidConfig with a hint", pe)
	}
}

func TestCredential(t *testing.T) {
	_, p := newFake(t, DefaultNamespace)
	get := func(id string) (provider.Credential, error) {
		return p.Credential(context.Background(), provider.CredentialRequest{Account: account(""), ClusterID: id})
	}

	c, err := get("cluster-eks")
	wantArgs := []string{"eks", "get-token", "--cluster-name", "prod-eks", "--role-arn", "arn:aws:iam::111122223333:role/argocd", "--region", "eu-west-1"}
	if err != nil || c.Type != provider.CredentialDelegate || c.Exec.Command != "aws" || !slices.Equal(c.Exec.Args, wantArgs) ||
		c.Exec.APIVersion != "client.authentication.k8s.io/v1beta1" || c.ExpiresAt != nil {
		t.Fatalf("Credential(cluster-eks) = %+v %+v, %v; want aws %v", c, c.Exec, err, wantArgs)
	}

	c, err = get("cluster-gke")
	if err != nil || c.Type != provider.CredentialDelegate || c.Exec.Command != "argocd-k8s-auth" || !slices.Equal(c.Exec.Args, []string{"gcp"}) ||
		c.Exec.Env["GOOGLE_PROJECT"] != "acme" || c.Exec.InstallHint != "install argocd-k8s-auth" {
		t.Fatalf("Credential(cluster-gke) = %+v, %v", c, err)
	}

	c, err = get("cluster-cert")
	if err != nil || c.Type != provider.CredentialClientCert || !c.ExpiresAt.Equal(now.Add(credentialTTL)) {
		t.Fatalf("Credential(cluster-cert) = %+v, %v; want a client certificate read again in %s", c, err, credentialTTL)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		id   string
		code provider.Code
		msg  string
	}{
		{"cluster-basic", provider.Unimplemented, "basic authentication"},
		{"cluster-info", provider.Unimplemented, "provideClusterInfo"},
		{"no-server", provider.NotFound, "no credentials"},
		{"repo-private", provider.NotFound, "not an Argo CD cluster Secret"},
		{"gone", provider.NotFound, "no cluster Secret argocd/gone"},
	}
	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			_, err := get(tt.id)
			if pe := provider.AsError(err); pe.Code != tt.code || !strings.Contains(pe.Message, tt.msg) {
				t.Fatalf("Credential(%s) error = %v, want %s containing %q", tt.id, err, tt.code, tt.msg)
			}
		})
	}
}

func TestEKSRegion(t *testing.T) {
	tests := []struct{ server, want string }{
		{"https://0123ABCD.gr7.eu-west-1.eks.amazonaws.com", "eu-west-1"},
		{"https://0123ABCD.yl4.cn-north-1.eks.amazonaws.com.cn", "cn-north-1"},
		{"https://eks.amazonaws.com", ""},
		{"https://prod.example.com", ""},
		{"::not a url", ""},
	}
	for _, tt := range tests {
		if got := eksRegion(tt.server); got != tt.want {
			t.Errorf("eksRegion(%q) = %q, want %q", tt.server, got, tt.want)
		}
	}
}
