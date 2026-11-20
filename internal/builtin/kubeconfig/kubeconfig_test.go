package kubeconfig

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/paingha/tugcli/pkg/provider"
)

const sample = `apiVersion: v1
kind: Config
clusters:
- name: lke
  cluster:
    server: https://lke.example:443
    certificate-authority-data: Q0EtREFUQQ==
- name: eks
  cluster:
    server: https://eks.example
- name: session
  cluster:
    server: https://session.example
users:
- name: lke-admin
  user:
    token: secret-token
- name: eks-user
  user:
    exec:
      apiVersion: client.authentication.k8s.io/v1beta1
      command: aws
      args: [eks, get-token, --cluster-name, prod]
      env:
      - name: AWS_PROFILE
        value: work
- name: tug-user
  user:
    exec:
      apiVersion: client.authentication.k8s.io/v1
      command: /usr/local/bin/tug
      args: [auth, token, --cluster, x]
contexts:
- name: lke-ctx
  context: {cluster: lke, user: lke-admin, namespace: payments}
- name: eks-ctx
  context: {cluster: eks, user: eks-user}
- name: tug-session
  context: {cluster: session, user: tug-user}
current-context: lke-ctx
`

func writeSample(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "lke-kubeconfig.yaml")
	if err := os.WriteFile(path, []byte(sample), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestList(t *testing.T) {
	path := writeSample(t)
	p := New(WithHome(t.TempDir()))
	acct := provider.Account{ID: "lke", Config: map[string]any{ConfigPath: path}}

	clusters, err := p.List(context.Background(), provider.ListRequest{Account: acct})
	if err != nil {
		t.Fatal(err)
	}
	if len(clusters) != 2 {
		t.Fatalf("want 2 clusters (tug's own session skipped), got %d: %+v", len(clusters), clusters)
	}
	byID := map[string]provider.Cluster{}
	for _, c := range clusters {
		byID[c.ID] = c
	}

	lke := byID["lke-ctx"]
	if lke.Endpoints.Public != "https://lke.example:443" || string(lke.CAData) != "CA-DATA" || lke.DefaultNamespace != "payments" {
		t.Fatalf("lke cluster: %+v", lke)
	}
	if lke.Credential == nil || lke.Credential.Type != provider.CredentialToken || lke.Credential.Token != "secret-token" {
		t.Fatalf("embedded token not returned inline: %+v", lke.Credential)
	}

	eks := byID["eks-ctx"]
	if eks.Credential == nil || eks.Credential.Type != provider.CredentialDelegate {
		t.Fatalf("exec user not delegated: %+v", eks.Credential)
	}
	if e := eks.Credential.Exec; e.Command != "aws" || e.Env["AWS_PROFILE"] != "work" || e.APIVersion != "client.authentication.k8s.io/v1beta1" {
		t.Fatalf("delegate exec: %+v", e)
	}
}

func TestCredentialRereadsFile(t *testing.T) {
	path := writeSample(t)
	p := New(WithHome(t.TempDir()))
	acct := provider.Account{ID: "lke", Config: map[string]any{ConfigPath: path}}

	c, err := p.Credential(context.Background(), provider.CredentialRequest{Account: acct, ClusterID: "lke-ctx"})
	if err != nil || c.Token != "secret-token" {
		t.Fatalf("credential = %+v, %v", c, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	_, err = p.Credential(context.Background(), provider.CredentialRequest{Account: acct, ClusterID: "lke-ctx"})
	if provider.CodeOf(err) != provider.NotFound {
		t.Fatalf("missing file: want NotFound, got %v", err)
	}
}

func TestAccounts(t *testing.T) {
	home := t.TempDir()
	kube := filepath.Join(home, ".kube")
	if err := os.MkdirAll(kube, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"config", "work.yaml"} {
		if err := os.WriteFile(filepath.Join(kube, f), []byte(sample), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	extra := writeSample(t)
	skipped := filepath.Join(t.TempDir(), "sessions")
	if err := os.MkdirAll(skipped, 0o700); err != nil {
		t.Fatal(err)
	}
	sessionFile := filepath.Join(skipped, "kubeconfig")
	if err := os.WriteFile(sessionFile, []byte(sample), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"KUBECONFIG": extra + string(os.PathListSeparator) + sessionFile}
	p := New(WithHome(home), WithGetenv(func(k string) string { return env[k] }), WithSkipDirs(skipped))

	accts, err := p.Accounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, a := range accts {
		ids = append(ids, a.ID)
	}
	want := []string{"default", "lke-kubeconfig", "work"}
	if len(ids) != len(want) {
		t.Fatalf("accounts = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("accounts = %v, want %v", ids, want)
		}
	}
}
