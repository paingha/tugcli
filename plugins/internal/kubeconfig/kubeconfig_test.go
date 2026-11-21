package kubeconfig

import (
	"encoding/base64"
	"strings"
	"testing"
)

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func TestParse(t *testing.T) {
	yamlConfig := `apiVersion: v1
kind: Config
current-context: admin@prod
clusters:
- name: staging
  cluster:
    server: https://staging.example.com:6443
- name: prod
  cluster:
    server: https://prod.example.com:6443
    certificate-authority-data: ` + b64("prod CA") + `
    tls-server-name: kubernetes
users:
- name: viewer
  user:
    token: view-token
- name: admin
  user:
    client-certificate-data: ` + b64("cert") + `
    client-key-data: ` + b64("key") + `
contexts:
- name: viewer@staging
  context: {cluster: staging, user: viewer}
- name: admin@prod
  context: {cluster: prod, user: admin}
`
	jsonConfig := `{"clusters":[{"name":"c","cluster":{"server":"https://c.example.com","certificate-authority-data":"` + b64("c CA") + `"}}],
	  "users":[{"name":"u","user":{"token":"tok"}}]}`
	tests := []struct {
		name string
		in   string
		want Access
	}{
		{"current context", yamlConfig, Access{Server: "https://prod.example.com:6443", CAData: []byte("prod CA"), TLSServerName: "kubernetes", CertData: []byte("cert"), KeyData: []byte("key")}},
		{"first context", strings.Replace(yamlConfig, "current-context: admin@prod\n", "", 1), Access{Server: "https://staging.example.com:6443", Token: "view-token"}},
		{"JSON without contexts", jsonConfig, Access{Server: "https://c.example.com", CAData: []byte("c CA"), Token: "tok"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Parse([]byte(tt.in))
			if err != nil {
				t.Fatal(err)
			}
			if got.Server != tt.want.Server || string(got.CAData) != string(tt.want.CAData) || got.TLSServerName != tt.want.TLSServerName ||
				string(got.CertData) != string(tt.want.CertData) || string(got.KeyData) != string(tt.want.KeyData) || got.Token != tt.want.Token {
				t.Errorf("Parse = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestParseErrors(t *testing.T) {
	for name, in := range map[string]string{
		"not YAML":       "{",
		"no clusters":    "users: []\n",
		"missing":        "current-context: x\ncontexts: [{name: x, context: {cluster: gone}}]\nclusters: [{name: c, cluster: {server: https://c}}]\n",
		"CA not base64":  "clusters: [{name: c, cluster: {server: https://c, certificate-authority-data: '%%'}}]\n",
		"key not base64": "clusters: [{name: c, cluster: {server: https://c}}]\nusers: [{name: u, user: {client-key-data: '%%'}}]\n",
	} {
		if got, err := Parse([]byte(in)); err == nil {
			t.Errorf("Parse(%s) = %+v, want an error", name, got)
		}
	}
}
