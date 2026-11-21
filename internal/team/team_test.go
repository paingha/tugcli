package team

import (
	"crypto/ed25519"
	"errors"
	"strings"
	"testing"
)

// testCatalog is a valid team catalog at version 1.
const testCatalog = `apiVersion: tug.team/v1alpha1
kind: TeamCatalog
version: 1
description: Platform team clusters
accounts:
- provider: aws
  account: platform
  config:
    regions: us-east-1,eu-west-1
clusters:
- match: {provider: kubeconfig, providerID: payments-prod}
  name: pay-prod
  aliases: [pay]
  env: production
  labels: {team: payments}
pluginIndexes:
- name: acme
  url: https://plugins.example.com/index.yaml
  publicKey: ` + testIndexKey + `
policies:
- name: production-read-only
  selector: {env: prod}
  readOnly: true
  maxUnlock: 30m
- name: staging-write
  selector: {env: staging}
`

// testIndexKey is a valid ed25519 public key for plugin index entries.
const testIndexKey = "Lm2ugxoFcHsw/4y9Wbtx4phb9kJXFGT8ZwhVwOD0Ugk="

// newKey returns a signing key in the form tug team keygen prints, and
// its public half.
func newKey(t *testing.T) (string, ed25519.PublicKey) {
	t.Helper()
	priv, pub, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	key, err := ParsePublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return priv, key
}

func sign(t *testing.T, data, priv string) []byte {
	t.Helper()
	sig, err := Sign([]byte(data), priv)
	if err != nil {
		t.Fatalf("Sign() = %v", err)
	}
	return sig
}

func TestSignAndParse(t *testing.T) {
	priv, pub := newKey(t)
	sig := sign(t, testCatalog, priv)
	doc, err := Parse([]byte(testCatalog), sig, pub)
	if err != nil {
		t.Fatalf("Parse() = %v", err)
	}
	if doc.Version != 1 || len(doc.Clusters) != 1 || doc.Clusters[0].labels()["env"] != "prod" {
		t.Fatalf("Parse() = %+v", doc)
	}
	if got := doc.Policies[0].Summary(); got != "env=prod: read-only, unlocks up to 30m" {
		t.Errorf("Summary() = %q", got)
	}

	tampered := strings.Replace(testCatalog, "maxUnlock: 30m", "maxUnlock: 24h", 1)
	if _, err := Parse([]byte(tampered), sig, pub); !errors.Is(err, ErrSignature) {
		t.Errorf("Parse(tampered) = %v, want ErrSignature", err)
	}
	_, other := newKey(t)
	if _, err := Parse([]byte(testCatalog), sig, other); !errors.Is(err, ErrSignature) {
		t.Errorf("Parse(other key) = %v, want ErrSignature", err)
	}
	if _, err := Parse([]byte(testCatalog), []byte("not a signature"), pub); err == nil {
		t.Error("Parse(garbage signature) = nil, want an error")
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		edit    func(string) string
		wantErr string
	}{
		{"valid", func(s string) string { return s }, ""},
		{"no version", func(s string) string { return strings.Replace(s, "version: 1\n", "", 1) }, "version must be"},
		{"wrong kind", func(s string) string { return strings.Replace(s, "kind: TeamCatalog", "kind: PluginIndex", 1) }, "apiVersion and kind"},
		{"token in settings", func(s string) string {
			return strings.Replace(s, "    regions: us-east-1,eu-west-1", "    regions: us-east-1\n    apiToken: abc", 1)
		}, "apiToken looks like a credential"},
		{"nested secret", func(s string) string {
			return strings.Replace(s, "    regions: us-east-1,eu-west-1", "    auth:\n      client_secret: abc", 1)
		}, "auth.client_secret looks like a credential"},
		{"match without an identity", func(s string) string {
			return strings.Replace(s, "{provider: kubeconfig, providerID: payments-prod}", "{provider: kubeconfig}", 1)
		}, "match needs a providerID or a server"},
		{"alias equal to a name", func(s string) string { return strings.Replace(s, "aliases: [pay]", "aliases: [pay-prod]", 1) }, "used twice"},
		{"slash in a name", func(s string) string { return strings.Replace(s, "name: pay-prod", "name: pay/prod", 1) }, "not valid"},
		{"provider label", func(s string) string {
			return strings.Replace(s, "labels: {team: payments}", "labels: {provider: gcp}", 1)
		}, "the provider label"},
		{"env disagrees", func(s string) string {
			return strings.Replace(s, "labels: {team: payments}", "labels: {env: dev}", 1)
		}, "disagree"},
		{"index url with a password", func(s string) string {
			return strings.Replace(s, "https://plugins.example.com", "https://ci:hunter2@plugins.example.com", 1)
		}, "carries a password"},
		{"index over http", func(s string) string {
			return strings.Replace(s, "https://plugins.example.com", "http://plugins.example.com", 1)
		}, "needs an https url"},
		{"maxUnlock without readOnly", func(s string) string { return strings.Replace(s, "  readOnly: true\n", "", 1) }, "needs readOnly: true"},
		{"policy without a selector", func(s string) string { return strings.Replace(s, "  selector: {env: staging}\n", "", 1) }, "needs a selector"},
		{"unknown field", func(s string) string { return s + "credentials: {}\n" }, "unknown field"},
	}
	priv, _ := newKey(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Sign([]byte(tt.edit(testCatalog)), priv)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("Sign() = %v, want nil", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Fatalf("Sign() = %v, want an error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestNewSource(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		repo    string
		path    string
		ref     string
		wantErr string
	}{
		{name: "https", url: "https://teams.example.com/team.yaml"},
		{name: "loopback http", url: "http://127.0.0.1:8080/team.yaml"},
		{name: "plain http", url: "http://teams.example.com/team.yaml", wantErr: "must be https"},
		{name: "password in the url", url: "https://me:hunter2@teams.example.com/team.yaml", wantErr: "carries a password"},
		{name: "password in the git url", repo: "https://me:hunter2@github.com/acme/t.git", wantErr: "carries a password"},
		{name: "git over https with a user", repo: "https://me@github.com/acme/t.git"},
		{name: "git over ssh", repo: "git@github.com:acme/tug-team.git", ref: "main"},
		{name: "git path outside", repo: "https://github.com/acme/t.git", path: "../x.yaml", wantErr: "inside the repository"},
		{name: "git option", repo: "--upload-pack=evil", wantErr: "cannot start with a dash"},
		{name: "git ext transport", repo: "ext::sh -c evil", wantErr: "not allowed"},
		{name: "neither", wantErr: "needs the catalog's url"},
	}
	_, pub, _ := NewKey()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tc := configTeam("platform", tt.url, pub)
			if tt.repo != "" {
				tc.Git = gitConfig(tt.repo, tt.path, tt.ref)
			}
			src, err := NewSource(tc, false)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("NewSource() = %v, want nil", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Fatalf("NewSource() = %v, want an error containing %q", err, tt.wantErr)
			case tt.wantErr == "" && tt.repo != "" && src.Git.Path == "":
				t.Fatal("NewSource() left the git path empty")
			}
		})
	}
	if _, err := NewSource(configTeam("Platform Team", "https://x.example.com/t.yaml", pub), false); err == nil {
		t.Error("NewSource() accepted an invalid team name")
	}
	if _, err := NewSource(configTeam("platform", "https://x.example.com/t.yaml", "short"), false); err == nil {
		t.Error("NewSource() accepted an invalid key")
	}
}
