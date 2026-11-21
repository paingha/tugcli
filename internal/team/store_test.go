package team

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/paingha/tugcli/internal/config"
)

func configTeam(name, url, publicKey string) config.TeamCatalog {
	return config.TeamCatalog{Name: name, URL: url, PublicKey: publicKey}
}

func gitConfig(repo, path, ref string) *config.TeamGit {
	return &config.TeamGit{Repo: repo, Path: path, Ref: ref}
}

// catalogServer serves a team catalog and its signature, which a test can
// change between requests.
type catalogServer struct {
	*httptest.Server
	mu        sync.Mutex
	data, sig []byte
}

func newCatalogServer(t *testing.T) *catalogServer {
	t.Helper()
	s := &catalogServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		switch r.URL.Path {
		case "/team.yaml":
			_, _ = w.Write(s.data)
		case "/team.yaml.sig":
			_, _ = w.Write(s.sig)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *catalogServer) publish(data string, sig []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data, s.sig = []byte(data), sig
}

func withVersion(doc string, v string) string {
	return strings.Replace(doc, "version: 1\n", "version: "+v+"\n", 1)
}

func TestCheckAndAccept(t *testing.T) {
	priv, pub := newKey(t)
	srv := newCatalogServer(t)
	src := Source{Name: "platform", URL: srv.URL + "/team.yaml", PublicKey: pub}
	now := time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)
	st := NewStore(t.TempDir(), func() time.Time { return now })
	f := &Fetcher{}
	ctx := context.Background()

	srv.publish(testCatalog, sign(t, testCatalog, priv))
	u, err := st.Check(ctx, f, src)
	if err != nil {
		t.Fatalf("Check() = %v", err)
	}
	if !u.New || u.Previous != nil || u.Document.Version != 1 {
		t.Fatalf("first Check() = %+v, want a new version 1", u)
	}
	if err := st.Accept(src.Name, u.Data, u.Sig, u.Document); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(filepath.Join(st.Dir, "platform", catalogFile))
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("saved catalog mode = %v, %v; want 0600", fi.Mode(), err)
		}
	}

	// The same catalog again is nothing new.
	if u, err := st.Check(ctx, f, src); err != nil || u.New {
		t.Fatalf("Check(unchanged) = %+v, %v; want nothing new", u, err)
	}

	tests := []struct {
		name    string
		data    string
		sig     []byte
		wantErr string
	}{
		{"tampered", strings.Replace(testCatalog, "maxUnlock: 30m", "maxUnlock: 8h", 1), sign(t, testCatalog, priv), "signature does not match"},
		{"other key", withVersion(testCatalog, "2"), sign(t, withVersion(testCatalog, "2"), mustKey(t)), "signature does not match"},
		{"same version, other contents", testCatalog + "# changed\n", sign(t, testCatalog+"# changed\n", priv), "without raising its version"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv.publish(tt.data, tt.sig)
			if _, err := st.Check(ctx, f, src); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Check() = %v, want an error containing %q", err, tt.wantErr)
			}
			// The last good version stays.
			acc, err := st.Accepted(src)
			if err != nil || acc == nil || acc.Document.Version != 1 {
				t.Fatalf("Accepted() = %+v, %v; want version 1", acc, err)
			}
		})
	}

	// A newer version is new, and an older one is then refused.
	v3 := withVersion(testCatalog, "3")
	srv.publish(v3, sign(t, v3, priv))
	u, err = st.Check(ctx, f, src)
	if err != nil || !u.New || u.Previous == nil || u.Previous.Document.Version != 1 {
		t.Fatalf("Check(v3) = %+v, %v", u, err)
	}
	if err := st.Accept(src.Name, u.Data, u.Sig, u.Document); err != nil {
		t.Fatal(err)
	}
	v2 := withVersion(testCatalog, "2")
	srv.publish(v2, sign(t, v2, priv))
	if _, err := st.Check(ctx, f, src); !errors.Is(err, ErrRollback) {
		t.Fatalf("Check(v2 after v3) = %v, want ErrRollback", err)
	}
	// Even when the saved catalog is lost, its version is remembered.
	if err := os.Remove(filepath.Join(st.Dir, "platform", catalogFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Check(ctx, f, src); !errors.Is(err, ErrRollback) {
		t.Fatalf("Check(v2 without the saved catalog) = %v, want ErrRollback", err)
	}
}

func mustKey(t *testing.T) string {
	t.Helper()
	priv, _ := newKey(t)
	return priv
}

func TestOfflineKeepsTheLastGoodVersion(t *testing.T) {
	priv, pub := newKey(t)
	srv := newCatalogServer(t)
	src := Source{Name: "platform", URL: srv.URL + "/team.yaml", PublicKey: pub}
	st := NewStore(t.TempDir(), nil)
	ctx := context.Background()
	srv.publish(testCatalog, sign(t, testCatalog, priv))
	u, err := st.Check(ctx, &Fetcher{}, src)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Accept(src.Name, u.Data, u.Sig, u.Document); err != nil {
		t.Fatal(err)
	}

	srv.Close()
	_, err = st.Check(ctx, &Fetcher{HTTP: &http.Client{Timeout: time.Second}}, src)
	if err == nil {
		t.Fatal("Check() with the server gone = nil, want an error")
	}
	if err := st.NoteCheck(src.Name, err); err != nil {
		t.Fatal(err)
	}
	acc, err := st.Accepted(src)
	if err != nil || acc == nil || len(acc.Document.Policies) != 2 {
		t.Fatalf("Accepted() offline = %+v, %v; want the cached version", acc, err)
	}
	if acc.State.LastError == "" {
		t.Error("the failed check was not recorded")
	}

	// A cached catalog changed on disk is not trusted.
	path := filepath.Join(st.Dir, "platform", catalogFile)
	if err := os.WriteFile(path, []byte(strings.Replace(testCatalog, "readOnly: true", "readOnly: false", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Accepted(src); !errors.Is(err, ErrSignature) {
		t.Fatalf("Accepted(tampered cache) = %v, want ErrSignature", err)
	}
}

func TestAddedTeams(t *testing.T) {
	st := NewStore(t.TempDir(), nil)
	if got, err := st.Added(); err != nil || len(got) != 0 {
		t.Fatalf("Added() = %v, %v; want none", got, err)
	}
	a := configTeam("platform", "https://a.example.com/team.yaml", "k1")
	b := config.TeamCatalog{Name: "infra", Git: gitConfig("git@github.com:acme/t.git", "team.yaml", ""), PublicKey: "k2"}
	for _, tc := range []config.TeamCatalog{a, b} {
		if err := st.Add(tc); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Add(a); err == nil {
		t.Error("Add() of a team already added = nil, want an error")
	}
	got, err := st.Added()
	if err != nil || len(got) != 2 || got[0].Name != "platform" || got[1].Git == nil {
		t.Fatalf("Added() = %+v, %v", got, err)
	}
	if ok, err := st.RemoveAdded("platform"); err != nil || !ok {
		t.Fatalf("RemoveAdded(platform) = %v, %v", ok, err)
	}
	if ok, _ := st.RemoveAdded("platform"); ok {
		t.Error("RemoveAdded() twice reported a removal")
	}
}

func TestFetchFromGit(t *testing.T) {
	if testing.Short() {
		t.Skip("runs git")
	}
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	priv, pub := newKey(t)
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "catalogs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "catalogs", "platform.yaml"), []byte(testCatalog), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "catalogs", "platform.yaml.sig"), sign(t, testCatalog, priv), 0o600); err != nil {
		t.Fatal(err)
	}
	// Git writes these files with CRLF when it checks them out, on every
	// platform, as it does everywhere on Windows with core.autocrlf. The
	// signature covers the bytes as committed, so tug must read those.
	if err := os.WriteFile(filepath.Join(repo, ".gitattributes"), []byte("* text eol=crlf\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command(gitBin, append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+filepath.Join(repo, ".no-config"), "GIT_CONFIG_NOSYSTEM=1",
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "--quiet", "--initial-branch=main")
	git("add", ".")
	git("commit", "--quiet", "-m", "catalog")

	work := t.TempDir()
	src, err := NewSource(config.TeamCatalog{Name: "platform", Git: gitConfig(repo, "catalogs/platform.yaml", "main"), PublicKey: b64(pub)}, true)
	if err != nil {
		t.Fatal(err)
	}
	u, err := NewStore(t.TempDir(), nil).Check(context.Background(), &Fetcher{WorkDir: work}, src)
	if err != nil {
		t.Fatalf("Check(git) = %v", err)
	}
	if u.Document.Version != 1 || !u.New {
		t.Fatalf("Check(git) = %+v", u)
	}
	if entries, _ := os.ReadDir(work); len(entries) != 0 {
		t.Errorf("the clone was left behind: %v", entries)
	}

	src.Git.Path = "catalogs/missing.yaml"
	if _, _, err := (&Fetcher{WorkDir: work}).Fetch(context.Background(), src); err == nil {
		t.Error("Fetch() of a missing path = nil, want an error")
	}
}
