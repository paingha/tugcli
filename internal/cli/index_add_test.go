package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/paingha/tugcli/internal/sigstore"
)

const releaseWorkflow = "https://github.com/tests/tug-provider-idx/.github/workflows/release.yml@refs/tags/v1.0.0"

// packageIdxPlugin packages a plugin named idx, version 1.0.0, for this
// platform and returns the archive's path.
func packageIdxPlugin(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	manifest := "apiVersion: tug.plugin/v1alpha1\nkind: ProviderPlugin\nmetadata: {name: idx, version: 1.0.0, publisher: tests}\n" +
		"spec:\n  protocols: [provider/v1alpha1]\n  capabilities: [list, credential]\n  platforms:\n" +
		"  - {os: " + runtime.GOOS + ", arch: " + runtime.GOARCH + ", bin: tug-provider-idx}\n"
	if err := os.WriteFile(filepath.Join(dir, "plugin.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tug-provider-idx"), []byte(rotatePluginScript), 0o755); err != nil {
		t.Fatal(err)
	}
	return strings.Fields(mustTug(t, "plugin", "package", dir, "-o", t.TempDir()))[0]
}

func TestPluginIndexAddSigstore(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script plugin")
	}
	isolate(t)
	archive := packageIdxPlugin(t)
	index := filepath.Join(t.TempDir(), "index.yaml")
	url := "https://example.com/" + filepath.Base(archive)
	bundle := url + ".sigstore.json"

	tests := []struct {
		name  string
		flags []string
		want  string
	}{
		{"no bundle", []string{"--identity", releaseWorkflow, "--issuer", sigstore.GitHubIssuer}, "--bundle"},
		{"no identity", []string{"--bundle", bundle, "--issuer", sigstore.GitHubIssuer}, "--identity"},
		{"no issuer", []string{"--bundle", bundle, "--identity", releaseWorkflow}, "--issuer"},
		{"both identities", []string{"--bundle", bundle, "--identity", releaseWorkflow, "--identity-regexp", ".*", "--issuer", sigstore.GitHubIssuer}, "identity-regexp"},
		{"plain http bundle", []string{"--bundle", "http://example.com/x.sigstore.json", "--identity", releaseWorkflow, "--issuer", sigstore.GitHubIssuer}, "https url"},
		{"bad regexp", []string{"--bundle", bundle, "--identity-regexp", "(", "--issuer", sigstore.GitHubIssuer}, "identity regexp"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := append([]string{"plugin", "index", "add", index, archive, "--url", url}, tt.flags...)
			if r := tug(t, args...); r.code == 0 || !strings.Contains(r.stderr, tt.want) {
				t.Errorf("index add %s: %+v, want an error mentioning %q", strings.Join(tt.flags, " "), r, tt.want)
			}
		})
	}
	if _, err := os.Stat(index); err == nil {
		t.Fatal("a refused index add wrote the index")
	}

	out := mustTug(t, "plugin", "index", "add", index, archive, "--url", url,
		"--bundle", bundle, "--identity", releaseWorkflow, "--issuer", sigstore.GitHubIssuer)
	if !strings.Contains(out, "Added idx 1.0.0 to "+index+", signed by "+releaseWorkflow) {
		t.Errorf("index add: %s", out)
	}
	b, err := os.ReadFile(index)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"bundle: " + bundle, "identity: " + releaseWorkflow, "issuer: " + sigstore.GitHubIssuer} {
		if !strings.Contains(string(b), want) {
			t.Errorf("the index lacks %q:\n%s", want, b)
		}
	}

	// Adding the version again without a signature replaces the entry.
	mustTug(t, "plugin", "index", "add", index, archive, "--url", url)
	if b, _ := os.ReadFile(index); strings.Contains(string(b), "sigstore") {
		t.Errorf("the index still has the signature:\n%s", b)
	}
}
