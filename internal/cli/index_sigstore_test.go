package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/paingha/tugcli/internal/sigstore"
	"github.com/paingha/tugcli/internal/sigstore/sigstoretest"
)

// signedIndex is a plugin archive signed by a virtual Sigstore, published
// with its bundle, and an index that lists it.
type signedIndex struct {
	home, site, index, archive, url string
	srv                             *httptest.Server
}

// newSignedIndex packages a plugin, signs it as releaseWorkflow and serves
// it with its bundle. config.yaml trusts the virtual Sigstore and the index,
// and sets requireSignatures as given.
func newSignedIndex(t *testing.T, requireSignatures bool) *signedIndex {
	t.Helper()
	s := &signedIndex{home: isolate(t), site: t.TempDir()}
	t.Setenv(IndexKeyEnv, "")
	ss := sigstoretest.New(t)

	s.archive = packageIdxPlugin(t)
	data, err := os.ReadFile(s.archive)
	if err != nil {
		t.Fatal(err)
	}
	name := filepath.Base(s.archive)
	s.write(t, name, data)
	s.write(t, name+".sigstore.json", ss.Sign(t, releaseWorkflow, sigstore.GitHubIssuer, data))
	s.srv = httptest.NewServer(http.FileServer(http.Dir(s.site)))
	t.Cleanup(s.srv.Close)
	s.url = s.srv.URL + "/" + name
	s.index = filepath.Join(s.site, "index.yaml")

	pub := strings.TrimSpace(mustTug(t, "plugin", "index", "keygen"))
	root := filepath.Join(s.home, "config", "sigstore-root.json")
	if err := os.MkdirAll(filepath.Dir(root), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(root, ss.TrustedRootJSON(t), 0o600); err != nil {
		t.Fatal(err)
	}
	conf := "plugins:\n  sigstoreTrustedRoot: sigstore-root.json\n"
	if requireSignatures {
		conf += "  requireSignatures: true\n"
	}
	conf += "  indexes:\n  - {name: test, url: " + s.srv.URL + "/index.yaml, publicKey: " + pub + "}\n"
	if err := os.WriteFile(filepath.Join(s.home, "config", "config.yaml"), []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	return s
}

func (s *signedIndex) write(t *testing.T, name string, data []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(s.site, name), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// add records the archive in the index with extra flags, signs the index
// and drops tug's cached copy of it, as a publisher's next release would.
func (s *signedIndex) add(t *testing.T, flags ...string) {
	t.Helper()
	mustTug(t, append([]string{"plugin", "index", "add", s.index, s.archive, "--url", s.url}, flags...)...)
	mustTug(t, "plugin", "index", "sign", s.index)
	if err := os.RemoveAll(filepath.Join(s.home, "cache", "plugin-index")); err != nil {
		t.Fatal(err)
	}
}

// signedBy gives index add the archive's bundle and a signer.
func (s *signedIndex) signedBy(identity string) []string {
	return []string{"--bundle", s.url + ".sigstore.json", "--identity", identity, "--issuer", sigstore.GitHubIssuer}
}

func TestPluginInstallSigned(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script plugin")
	}
	s := newSignedIndex(t, true)

	s.add(t, s.signedBy(releaseWorkflow)...)
	r := tug(t, "plugin", "install", "idx", "--yes")
	if r.code != 0 || !strings.Contains(r.stdout, "Installed idx 1.0.0") || !strings.Contains(r.stderr, "signed by:     "+releaseWorkflow) {
		t.Fatalf("install of a signed plugin: %+v", r)
	}

	// A signature by anyone else is refused, even though the index and the
	// archive's sha256 are good.
	mallory := strings.Replace(releaseWorkflow, "tests/", "mallory/", 1)
	s.add(t, s.signedBy(mallory)...)
	r = tug(t, "plugin", "install", "idx", "--yes", "--force")
	if r.code == 0 || !strings.Contains(r.stderr, "the signer is not the one expected") || !strings.Contains(r.stderr, "expected "+mallory) {
		t.Fatalf("install with the wrong identity: %+v", r)
	}

	// So is a regexp that matches only part of the identity; one that
	// matches all of it is accepted.
	s.add(t, "--bundle", s.url+".sigstore.json", "--identity-regexp", `https://github\.com/tests/`, "--issuer", sigstore.GitHubIssuer)
	if r := tug(t, "plugin", "install", "idx", "--yes", "--force"); r.code == 0 || !strings.Contains(r.stderr, "the signer is not the one expected") {
		t.Fatalf("install with a partial identity regexp: %+v", r)
	}
	s.add(t, "--bundle", s.url+".sigstore.json", "--identity-regexp", `https://github\.com/tests/tug-provider-idx/\.github/workflows/release\.yml@refs/tags/v.*`, "--issuer", sigstore.GitHubIssuer)
	if r := tug(t, "plugin", "install", "idx", "--yes", "--force"); r.code != 0 {
		t.Fatalf("install with an identity regexp: %+v", r)
	}

	// A bundle that cannot be downloaded is refused.
	s.add(t, "--bundle", s.url+".missing.json", "--identity", releaseWorkflow, "--issuer", sigstore.GitHubIssuer)
	if r := tug(t, "plugin", "install", "idx", "--yes", "--force"); r.code == 0 || !strings.Contains(r.stderr, "the Sigstore bundle for idx 1.0.0: downloading") {
		t.Fatalf("install with a missing bundle: %+v", r)
	}

	// An archive changed after signing is refused before its sha256 is
	// checked.
	s.add(t, s.signedBy(releaseWorkflow)...)
	data, err := os.ReadFile(s.archive)
	if err != nil {
		t.Fatal(err)
	}
	s.write(t, filepath.Base(s.archive), append(data, 0))
	if r := tug(t, "plugin", "install", "idx", "--yes", "--force"); r.code == 0 || !strings.Contains(r.stderr, "checking the Sigstore signature of idx 1.0.0") {
		t.Fatalf("install of a tampered archive: %+v", r)
	}
	s.write(t, filepath.Base(s.archive), data)

	// An index entry without a signature is refused under requireSignatures.
	s.add(t)
	if r := tug(t, "plugin", "install", "idx", "--yes", "--force"); r.code == 0 || !strings.Contains(r.stderr, "plugins.requireSignatures") {
		t.Fatalf("install of an unsigned entry with requireSignatures: %+v", r)
	}

	// An archive named on the command line still installs, with a warning.
	sum := sha256.Sum256(data)
	r = tug(t, "plugin", "install", s.url, "--sha256", hex.EncodeToString(sum[:]), "--yes", "--force")
	if r.code != 0 || !strings.Contains(r.stderr, "warning: "+s.url+" has no Sigstore signature") {
		t.Fatalf("direct install of an unsigned archive: %+v", r)
	}
}

func TestPluginInstallUnsigned(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script plugin")
	}
	s := newSignedIndex(t, false)
	s.add(t)
	r := tug(t, "plugin", "install", "idx", "--yes")
	if r.code != 0 || !strings.Contains(r.stderr, "warning: index test has no Sigstore signature for idx 1.0.0") {
		t.Fatalf("install of an unsigned entry without requireSignatures: %+v", r)
	}
}
