package plugin

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/paingha/tugcli/pkg/provider"
)

// fuzzInstance is a serve-mode plugin that has already exited.
type fuzzInstance struct{}

func (fuzzInstance) wait() error      { return nil }
func (fuzzInstance) terminate() error { return nil }
func (fuzzInstance) kill() error      { return nil }

// FuzzServeResponses feeds what a plugin in serve mode might write to the
// host's reader. Whatever the plugin writes, the reader must deliver at
// most one answer to each waiting call and return once stdout ends; calls
// that gave up waiting, as they do when they time out, must not hold it up.
func FuzzServeResponses(f *testing.F) {
	ok := func(id, kind string) string {
		resp, _ := json.Marshal(provider.ServeResponse{ID: id, Response: json.RawMessage(`{"apiVersion":"provider/v1alpha1","kind":"` + kind + `"}`)})
		return string(resp) + "\n"
	}
	f.Add(ok("1", "ListResponse") + ok("2", "CredentialResponse"))
	f.Add(ok("", "Error") + "not json\n" + ok("3", "Error"))
	f.Add(`{"id":"1","response":null}` + "\n" + `{"id":1}` + "\n" + `{"response":{}}`)
	f.Add(strings.Repeat(" ", 100) + "\n\n" + ok("9", "ListResponse"))
	f.Fuzz(func(t *testing.T, stdout string) {
		r := &Runner{Name: "fuzz"}
		s := &server{r: r}
		inst := fuzzInstance{}
		done := make(chan struct{})
		// Calls 1 and 2 have given up and read nothing; call 3 still waits.
		s.inst, s.pending, s.done = inst, map[string]chan []byte{}, done
		for _, id := range []string{"1", "2", "3"} {
			s.pending[id] = make(chan []byte, 1)
		}
		go s.read(inst, io.NopCloser(strings.NewReader(stdout)), r.stderrLogger(provider.OpServe), done)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("the serve reader did not return after the plugin's stdout ended:\n%q", stdout)
		}
		if s.exitError() == nil {
			t.Fatal("the reader left no exit error for waiting calls")
		}
	})
}

// FuzzPluginResponses decodes what a plugin might answer through the host's
// own decoding, and checks that text the plugin controls and tug prints,
// such as an error's message and hint, cannot carry terminal control
// sequences.
func FuzzPluginResponses(f *testing.F) {
	for _, s := range []string{
		`{"apiVersion":"provider/v1alpha1","kind":"Error","code":"Unauthenticated","message":"bad token","hint":"run tug import"}`,
		`{"apiVersion":"provider/v1alpha1","kind":"Error","code":"Internal","message":"line one\nline two\ttabbed"}`,
		`{"apiVersion":"provider/v1alpha1","kind":"CheckAccessResponse","findings":[{"severity":"error","message":"blocked","fix":"allow 192.0.2.1/32"}]}`,
		`{"apiVersion":"provider/v1alpha1","kind":"DescribeResponse","name":"x","description":"Acme clusters","protocols":["provider/v1alpha1"],"capabilities":["list"]}`,
		`{"apiVersion":"provider/v1alpha1","kind":"AccountsResponse","accounts":[{"id":"a","description":"from ~/.acme"}]}`,
	} {
		for op := range len(fuzzOps) {
			f.Add(byte(op), []byte(s))
		}
	}
	f.Fuzz(func(t *testing.T, pick byte, out []byte) {
		p := &Provider{plugin: &Plugin{Manifest: &Manifest{Metadata: Metadata{Name: "fuzz"}}}, runner: &Runner{Name: "fuzz"}}
		var texts []string
		var err error
		switch op := fuzzOps[int(pick)%len(fuzzOps)]; op {
		case provider.OpDescribe:
			var d provider.Description
			d, err = decode(p, out, provider.DecodeDescribe)
			texts = append(texts, d.Description)
		case provider.OpAccounts:
			var accts []provider.Account
			accts, err = decode(p, out, provider.DecodeAccounts)
			for _, a := range accts {
				texts = append(texts, a.Description)
			}
		case provider.OpCheckAccess:
			var fs []provider.Finding
			fs, err = decode(p, out, provider.DecodeCheckAccess)
			for _, f := range fs {
				texts = append(texts, f.Message, f.Fix)
			}
		default:
			_, err = decode(p, out, provider.DecodeList)
		}
		var pe *provider.Error
		if errors.As(err, &pe) {
			texts = append(texts, pe.Message, pe.Hint, string(pe.Code))
		}
		for _, s := range texts {
			if i := strings.IndexFunc(s, unprintable); i >= 0 {
				t.Fatalf("plugin text %q keeps the control character %U", s, []rune(s[i:])[0])
			}
		}
	})
}

// fuzzOps are the operations whose responses FuzzPluginResponses decodes.
var fuzzOps = []string{provider.OpDescribe, provider.OpAccounts, provider.OpList, provider.OpCheckAccess}

const fuzzManifest = `apiVersion: tug.plugin/v1alpha1
kind: ProviderPlugin
metadata: {name: acme, version: 0.1.0, publisher: Acme}
spec:
  protocols: [provider/v1alpha1]
  capabilities: [list, credential]
  platforms:
  - {os: linux, arch: amd64, bin: bin/tug-provider-acme}
  - {os: wasip1, arch: wasm, bin: bin/acme.wasm}
  secrets: [{name: token}]
  env: [ACME_PROFILE]
  network: [api.acme.example, "*.acme.example:8443"]
  sandbox: {tools: [acme], read: ["~/.acme"], ports: [8443]}
  limits: {cpuSeconds: 20, memoryMiB: 128, timeouts: {list: 2m}}
  accountConfig: {properties: {region: {type: string, enum: [eu, us]}}}
`

// FuzzManifest loads plugin.yaml files and checks what tug relies on once
// one is accepted: the builds and the install directory are inside the
// places tug puts them, and checking the manifest again agrees.
func FuzzManifest(f *testing.F) {
	f.Add([]byte(fuzzManifest))
	f.Add([]byte(strings.Replace(fuzzManifest, "version: 0.1.0", "version: v1.2.3-rc.1", 1)))
	f.Add([]byte(strings.Replace(fuzzManifest, "bin/acme.wasm", "../acme.wasm", 1)))
	f.Add([]byte(strings.Replace(fuzzManifest, "version: 0.1.0", "version: ../../../evil", 1)))
	f.Add([]byte("apiVersion: 1\nkind: [x]\nspec: {platforms: 3}\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, ManifestFile), data, 0o600); err != nil {
			t.Fatal(err)
		}
		m, err := LoadManifest(dir)
		if err != nil {
			return
		}
		if err := m.Validate(); err != nil {
			t.Fatalf("a loaded manifest does not validate again: %v", err)
		}
		for _, p := range m.Spec.Platforms {
			if !inside(dir, filepath.Join(dir, filepath.FromSlash(p.Bin))) {
				t.Fatalf("build %q is outside the plugin directory", p.Bin)
			}
		}
		plugins := filepath.Join(dir, "plugins")
		if dest := filepath.Join(plugins, m.Metadata.Name, m.Metadata.Version); !inside(filepath.Join(plugins, m.Metadata.Name), dest) || dest == filepath.Join(plugins, m.Metadata.Name) {
			t.Fatalf("version %q installs to %s, outside the plugin's own directory", m.Metadata.Version, dest)
		}
		for _, port := range m.SandboxPorts() {
			if port == 0 {
				t.Fatal("SandboxPorts returned port 0")
			}
		}
		_ = m.Settings()
		_ = m.UnknownSettings(map[string]any{"x": 1})
	})
}

// inside reports whether path is dir or below it.
func inside(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// fuzzIndexKey signs the indexes FuzzIndex reads; ed25519 keys are
// deterministic from their seed.
var fuzzIndexKey = ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, ed25519.SeedSize))

// FuzzIndex reads signed plugin indexes and checks that an index tug
// accepts names only plugins and versions it can install safely.
func FuzzIndex(f *testing.F) {
	sum := strings.Repeat("a", 64)
	f.Add([]byte(strings.NewReplacer("%s", sum).Replace(indexYAML)))
	f.Add([]byte(strings.NewReplacer("%s", sum).Replace(indexYAML) + `  - version: 1.0.0
    url: https://example.com/a-1.0.0.tar.gz
    sha256: ` + sum + `
    sigstore: {bundle: "https://example.com/b.json", identity: "https://github.com/a/b/.github/workflows/r.yml@refs/tags/v1", issuer: "https://token.actions.githubusercontent.com"}
`))
	f.Add([]byte("apiVersion: tug.plugin.index/v1alpha1\nkind: PluginIndex\nplugins: []\n"))
	f.Add([]byte("apiVersion: tug.plugin.index/v1alpha1\nkind: PluginIndex\nplugins: []\nrevoked:\n- {sha256: " + sum + ", plugin: acme, version: 0.1.0, reason: leaks}\n" +
		"supersededKeys:\n- {publicKey: " + base64.StdEncoding.EncodeToString(fuzzIndexKey.Public().(ed25519.PublicKey)) + "}\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		sig := []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(fuzzIndexKey, data)))
		ix, err := ParseIndex(data, sig, fuzzIndexKey.Public().(ed25519.PublicKey))
		if err != nil {
			return
		}
		for _, p := range ix.Plugins {
			if !validName.MatchString(p.Name) {
				t.Fatalf("index accepted the plugin name %q", p.Name)
			}
			for _, v := range p.Versions {
				if !validPluginVersion.MatchString(v.Version) {
					t.Fatalf("index accepted the version %q of %s", v.Version, p.Name)
				}
			}
			_, _ = p.Latest()
		}
		for _, r := range ix.Revoked {
			if len(r.SHA256) != 64 {
				t.Fatalf("index accepted the revocation %+v", r)
			}
		}
		if ix.superseded(ix.SignedBy()) {
			t.Fatal("index accepted a signature by a key it lists as superseded")
		}
	})
}

// FuzzSafePath checks that no archive member name extracts outside the
// plugin's directory.
func FuzzSafePath(f *testing.F) {
	for _, s := range []string{"plugin.yaml", "bin/x", "../x", "/etc/passwd", `..\x`, "a/../../b", "C:/x", "./a/./b/"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, name string) {
		dest := t.TempDir()
		target, err := safePath(dest, name)
		if err != nil {
			return
		}
		if !inside(dest, target) || target == dest {
			t.Fatalf("safePath(%q) = %s, outside %s", name, target, dest)
		}
	})
}
