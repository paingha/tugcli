package plugin

import (
	"strings"
	"testing"
)

const indexYAML = `apiVersion: tug.plugin.index/v1alpha1
kind: PluginIndex
plugins:
- name: acme
  versions:
  - {version: 0.9.0, url: "https://example.com/a-0.9.0.tar.gz", sha256: "%s"}
  - {version: 0.10.0, url: "https://example.com/a-0.10.0.tar.gz", sha256: "%s"}
`

func TestIndexSignatures(t *testing.T) {
	sum := strings.Repeat("a", 64)
	data := []byte(strings.NewReplacer("%s", sum).Replace(indexYAML))
	priv, pub, err := NewIndexKey()
	if err != nil {
		t.Fatal(err)
	}
	sig, err := SignIndex(data, priv)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ParsePublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	ix, err := ParseIndex(data, sig, key)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := ix.Plugins[0].Latest(); v.Version != "0.10.0" {
		t.Fatalf("Latest = %s, want 0.10.0 (versions compare as numbers)", v.Version)
	}
	if _, ok := ix.Plugins[0].Version("v0.9.0"); !ok {
		t.Fatal("Version(v0.9.0) not found")
	}

	_, otherPub, _ := NewIndexKey()
	other, _ := ParsePublicKey(otherPub)
	if _, err := ParseIndex(data, sig, other); err == nil {
		t.Fatal("an index signed with another key was accepted")
	}
	if _, err := ParseIndex(append(data, '#'), sig, key); err == nil {
		t.Fatal("a changed index was accepted")
	}
}

func TestIndexValidate(t *testing.T) {
	sum := strings.Repeat("a", 64)
	tests := []struct{ name, body, want string }{
		{"plain http", strings.Replace(indexYAML, "https://example.com/a-0.9.0", "http://example.com/a-0.9.0", 1), "https url"},
		{"short sha", strings.Replace(indexYAML, `sha256: "%s"}`+"\n  - ", `sha256: "abc"}`+"\n  - ", 1), "sha256"},
		{"unknown field", strings.Replace(indexYAML, "- name: acme", "- name: acme\n  rating: 5", 1), "rating"},
	}
	priv, _, _ := NewIndexKey()
	for _, tt := range tests {
		data := []byte(strings.NewReplacer("%s", sum).Replace(tt.body))
		if _, err := SignIndex(data, priv); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: SignIndex = %v, want an error mentioning %q", tt.name, err, tt.want)
		}
	}
}
