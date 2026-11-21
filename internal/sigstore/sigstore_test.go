package sigstore

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sigstore/sigstore-go/pkg/root"

	"github.com/paingha/tugcli/internal/sigstore/sigstoretest"
)

const workflow = "https://github.com/acme/tug-provider-acme/.github/workflows/release.yml@refs/tags/v0.1.0"

func writeArchive(t *testing.T, data string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tug-provider-acme_0.1.0.tar.gz")
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestVerify(t *testing.T) {
	ss := sigstoretest.New(t)
	v := New(ss.TrustedRoot(t))
	archive := writeArchive(t, "the archive")
	bundle := ss.Sign(t, workflow, GitHubIssuer, []byte("the archive"))

	tests := []struct {
		name string
		id   Identity
	}{
		{"exact identity", Identity{Subject: workflow, Issuer: GitHubIssuer}},
		{"identity regexp", Identity{SubjectRegexp: `https://github\.com/acme/tug-provider-acme/\.github/workflows/release\.yml@refs/tags/v.*`, Issuer: GitHubIssuer}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, err := v.Verify(archive, bundle, tt.id)
			if err != nil {
				t.Fatalf("Verify = %v, want a signer", err)
			}
			if s.Subject != workflow || s.Issuer != GitHubIssuer || s.Time.IsZero() {
				t.Errorf("Verify = %+v, want %s from %s with a time", s, workflow, GitHubIssuer)
			}
		})
	}
}

func TestVerifyRefuses(t *testing.T) {
	ss := sigstoretest.New(t)
	v := New(ss.TrustedRoot(t))
	archive := writeArchive(t, "the archive")
	bundle := ss.Sign(t, workflow, GitHubIssuer, []byte("the archive"))
	good := Identity{Subject: workflow, Issuer: GitHubIssuer}

	other := sigstoretest.New(t)
	foreign := other.Sign(t, workflow, GitHubIssuer, []byte("the archive"))

	tests := []struct {
		name     string
		archive  string
		bundle   []byte
		id       Identity
		mismatch bool // the error is ErrIdentityMismatch
		want     string
	}{
		{"another workflow", archive, bundle, Identity{Subject: strings.Replace(workflow, "acme/", "mallory/", 1), Issuer: GitHubIssuer}, true, "expected https://github.com/mallory/"},
		{"another issuer", archive, bundle, Identity{Subject: workflow, Issuer: "https://accounts.google.com"}, true, "issuer https://accounts.google.com"},
		{"regexp matching only part", archive, bundle, Identity{SubjectRegexp: `https://github\.com/acme/`, Issuer: GitHubIssuer}, true, "matching"},
		{"tampered archive", writeArchive(t, "the archive, changed"), bundle, good, false, "signature"},
		{"another Sigstore", archive, foreign, good, false, "log"},
		{"not a bundle", archive, []byte(`{"mediaType": "nonsense"}`), good, false, "bundle"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, err := v.Verify(tt.archive, tt.bundle, tt.id)
			if err == nil {
				t.Fatalf("Verify = %+v, want an error", s)
			}
			if got := errors.Is(err, ErrIdentityMismatch); got != tt.mismatch {
				t.Errorf("errors.Is(%v, ErrIdentityMismatch) = %v, want %v", err, got, tt.mismatch)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Verify = %v, want it to mention %q", err, tt.want)
			}
		})
	}
}

// TestVerifyStale checks that a failure with the built-in trust root says
// it was used, since an old root is a likely cause.
func TestVerifyStale(t *testing.T) {
	ss := sigstoretest.New(t)
	v := &Verifier{trusted: ss.TrustedRoot(t), stale: errors.New("no route to host")}
	_, err := v.Verify(writeArchive(t, "changed"), ss.Sign(t, workflow, GitHubIssuer, []byte("the archive")), Identity{Subject: workflow, Issuer: GitHubIssuer})
	if err == nil || !strings.Contains(err.Error(), "built into tug") || !strings.Contains(err.Error(), "no route to host") {
		t.Errorf("Verify = %v, want it to say the built-in trust root was used and why", err)
	}
}

func TestIdentityValidate(t *testing.T) {
	tests := []struct {
		name string
		id   Identity
		want string // empty when valid
	}{
		{"subject", Identity{Subject: workflow, Issuer: GitHubIssuer}, ""},
		{"regexp", Identity{SubjectRegexp: "https://github.com/acme/.*", Issuer: GitHubIssuer}, ""},
		{"neither", Identity{Issuer: GitHubIssuer}, "identity or identity regexp is needed"},
		{"both", Identity{Subject: workflow, SubjectRegexp: ".*", Issuer: GitHubIssuer}, "not both"},
		{"bad regexp", Identity{SubjectRegexp: "(", Issuer: GitHubIssuer}, "identity regexp"},
		{"no issuer", Identity{Subject: workflow}, "issuer"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.id.Validate()
			switch {
			case tt.want == "" && err != nil:
				t.Errorf("Validate() = %v, want nil", err)
			case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
				t.Errorf("Validate() = %v, want an error mentioning %q", err, tt.want)
			}
		})
	}
}

// TestBuiltinRoot checks that the trust root built into tug is the
// public-good one, with the Fulcio, Rekor and CT logs it needs.
func TestBuiltinRoot(t *testing.T) {
	tr, err := root.NewTrustedRootFromJSON(builtinRoot)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(tr.FulcioCertificateAuthorities()); n == 0 {
		t.Error("the built-in root has no Fulcio certificate authority")
	}
	if n := len(tr.CTLogs()); n == 0 {
		t.Error("the built-in root has no CT log")
	}
	var rekor bool
	for _, l := range tr.RekorLogs() {
		rekor = rekor || l.BaseURL == "https://rekor.sigstore.dev"
	}
	if !rekor {
		t.Error("the built-in root does not list https://rekor.sigstore.dev")
	}
}

// TestPublicGoodOffline checks that a first run that cannot reach the TUF
// repository falls back to the trust root built into tug.
func TestPublicGoodOffline(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	cache := filepath.Join(t.TempDir(), "sigstore")
	v, err := publicGood(context.Background(), cache, srv.URL)
	if err != nil {
		t.Fatalf("publicGood = %v, want the built-in root", err)
	}
	if v.Stale() == nil {
		t.Error("Stale() = nil, want why TUF was not used")
	}
	if len(v.trusted.FulcioCertificateAuthorities()) == 0 {
		t.Error("the fallback verifier trusts no Fulcio certificate authority")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := publicGood(ctx, cache, srv.URL); !errors.Is(err, context.Canceled) {
		t.Errorf("publicGood(cancelled) = %v, want context.Canceled", err)
	}
}

func TestFromFile(t *testing.T) {
	ss := sigstoretest.New(t)
	path := filepath.Join(t.TempDir(), "trusted_root.json")
	if err := os.WriteFile(path, ss.TrustedRootJSON(t), 0o600); err != nil {
		t.Fatal(err)
	}
	v, err := FromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	archive := writeArchive(t, "the archive")
	if _, err := v.Verify(archive, ss.Sign(t, workflow, GitHubIssuer, []byte("the archive")), Identity{Subject: workflow, Issuer: GitHubIssuer}); err != nil {
		t.Errorf("Verify with a trust root from a file = %v, want nil", err)
	}
	if _, err := FromFile(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Error("FromFile(missing) = nil error, want one")
	}
}
