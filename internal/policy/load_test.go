package policy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/paingha/tugcli/internal/config"
)

func fixture(t *testing.T, name string) ([]byte, string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return data, hex.EncodeToString(sum[:])
}

func names(packs []*Pack) []string {
	out := make([]string, len(packs))
	for i, p := range packs {
		out[i] = p.Name + " " + p.Version
	}
	return out
}

func TestLoadFiles(t *testing.T) {
	_, sum := fixture(t, "second.yaml")
	abs, err := filepath.Abs(filepath.Join("testdata", "second.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	l := &Loader{Dir: "testdata"}
	packs, err := l.Load(context.Background(), []config.PolicyPackSource{
		{Path: "acme.yaml"},                       // relative to the config directory
		{Path: abs, SHA256: strings.ToUpper(sum)}, // a pin is checked when given, in any case
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(names(packs), ", "), "baseline 1.0.0, acme 2.1.0, second 1"; got != want {
		t.Errorf("Load = %s, want %s", got, want)
	}
	if packs[1].Source != filepath.Join("testdata", "acme.yaml") || packs[2].Source != abs {
		t.Errorf("sources = %q, %q", packs[1].Source, packs[2].Source)
	}
}

func TestLoadRejects(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "baseline.yaml"), baselineYAML, 0o600); err != nil {
		t.Fatal(err)
	}
	acme, _ := fixture(t, "acme.yaml")
	if err := os.WriteFile(filepath.Join(dir, "acme.yaml"), acme, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "huge.yaml"), make([]byte, maxPackBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	zeros := strings.Repeat("0", 64)
	tests := []struct {
		name string
		srcs []config.PolicyPackSource
		want string
	}{
		{"wrong pin", []config.PolicyPackSource{{Path: "acme.yaml", SHA256: zeros}}, "not the " + zeros + " config.yaml pins"},
		{"malformed pin", []config.PolicyPackSource{{Path: "acme.yaml", SHA256: "abc"}}, `sha256 "abc" must be 64 hexadecimal digits`},
		{"path and url", []config.PolicyPackSource{{Path: "acme.yaml", URL: "https://example.com/a.yaml", SHA256: zeros}}, "a path or a url, not both"},
		{"neither", []config.PolicyPackSource{{SHA256: zeros}}, "give the pack's path, or its url and sha256"},
		{"url without a pin", []config.PolicyPackSource{{URL: "https://example.com/a.yaml"}}, "https://example.com/a.yaml needs a sha256"},
		{"plain http", []config.PolicyPackSource{{URL: "http://example.com/a.yaml", SHA256: zeros}}, "packs are downloaded over https only"},
		{"missing file", []config.PolicyPackSource{{Path: "gone.yaml"}}, "gone.yaml"},
		{"too large", []config.PolicyPackSource{{Path: "huge.yaml"}}, "larger than 1 MiB"},
		{"baseline's name", []config.PolicyPackSource{{Path: "baseline.yaml"}}, "is called baseline, as tug's own is; give it another name"},
		{"two of a name", []config.PolicyPackSource{{Path: "acme.yaml"}, {Path: "acme.yaml"}}, "policy.packs[1] in config.yaml: " + filepath.Join(dir, "acme.yaml") + " is called acme, as " + filepath.Join(dir, "acme.yaml") + " is"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := (&Loader{Dir: dir}).Load(context.Background(), tt.srcs)
			if err == nil || !strings.Contains(err.Error(), tt.want) || !strings.Contains(err.Error(), "in config.yaml: ") {
				t.Errorf("Load = %v, want an error with %q", err, tt.want)
			}
		})
	}
}

func TestLoadURL(t *testing.T) {
	acme, sum := fixture(t, "acme.yaml")
	var hits atomic.Int32
	serve := func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		switch r.URL.Path {
		case "/acme.yaml":
			_, _ = w.Write(acme)
		case "/changed.yaml":
			_, _ = w.Write(append(acme, "# changed\n"...))
		default:
			http.NotFound(w, r)
		}
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(serve))
	defer srv.Close()
	cache := filepath.Join(t.TempDir(), "policy")
	ctx := context.Background()
	load := func(l *Loader, url, pin string) ([]*Pack, error) {
		return l.Load(ctx, []config.PolicyPackSource{{URL: url, SHA256: pin}})
	}

	l := &Loader{Cache: cache, HTTP: srv.Client()}
	packs, err := load(l, srv.URL+"/acme.yaml", sum)
	if err != nil {
		t.Fatal(err)
	}
	if packs[1].Name != "acme" || packs[1].Source != srv.URL+"/acme.yaml" || hits.Load() != 1 {
		t.Fatalf("Load = %v from %s after %d requests", names(packs), packs[1].Source, hits.Load())
	}
	cached := filepath.Join(cache, sum+".yaml")
	if st, err := os.Stat(cached); err != nil {
		t.Fatalf("no cached copy: %v", err)
	} else if runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Errorf("cached copy has mode %v, want 0600", st.Mode().Perm())
	}

	// The pin names the content, so the cached copy serves from now on.
	srv.Close()
	if _, err := load(&Loader{Cache: cache}, srv.URL+"/acme.yaml", sum); err != nil {
		t.Fatalf("Load from the cache: %v", err)
	}

	srv = httptest.NewTLSServer(http.HandlerFunc(serve))
	defer srv.Close()
	l.HTTP = srv.Client()

	// A cached copy that was changed is downloaded again.
	if err := os.WriteFile(cached, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := hits.Load()
	if _, err := load(l, srv.URL+"/acme.yaml", sum); err != nil || hits.Load() != before+1 {
		t.Fatalf("Load over a changed cache = %v after %d requests, want a new download", err, hits.Load()-before)
	}
	if b, _ := os.ReadFile(cached); string(b) != string(acme) {
		t.Errorf("the cache was not repaired: %q", b)
	}

	// A pack that changed under its pin is refused. Without a cache, as
	// the cached copy would otherwise answer for the pin.
	l = &Loader{HTTP: srv.Client()}
	if _, err := load(l, srv.URL+"/changed.yaml", sum); err == nil || !strings.Contains(err.Error(), "/changed.yaml has sha256 ") {
		t.Errorf("Load of a changed pack = %v, want a sha256 mismatch", err)
	}
	if _, err := load(l, srv.URL+"/gone.yaml", sum); err == nil || !strings.Contains(err.Error(), "404 Not Found") {
		t.Errorf("Load of a missing pack = %v, want 404", err)
	}

	// Plain HTTP is for loopback test servers only.
	plain := httptest.NewServer(http.HandlerFunc(serve))
	defer plain.Close()
	if _, err := load(&Loader{}, plain.URL+"/acme.yaml", sum); err != nil {
		t.Errorf("Load over loopback HTTP: %v", err)
	}
}

func TestSelect(t *testing.T) {
	base, acme, second := &Pack{Name: "baseline"}, &Pack{Name: "acme"}, &Pack{Name: "second"}
	all := []*Pack{base, acme, second}
	tests := []struct {
		names, defaults []string
		want            string
		err             string
	}{
		{want: "baseline acme second"},
		{defaults: []string{"acme"}, want: "acme"},
		{names: []string{"second", "baseline", "second"}, defaults: []string{"acme"}, want: "second baseline"},
		{names: []string{"nope"}, err: `--pack names "nope", but no pack has that name; the packs are baseline, acme, second`},
		{defaults: []string{"old"}, err: `policy.default in config.yaml names "old"`},
	}
	for _, tt := range tests {
		got, err := Select(all, tt.names, tt.defaults)
		var gotNames []string
		for _, p := range got {
			gotNames = append(gotNames, p.Name)
		}
		if tt.err != "" {
			if err == nil || !strings.Contains(err.Error(), tt.err) {
				t.Errorf("Select(%v, %v) = %v, want an error with %q", tt.names, tt.defaults, err, tt.err)
			}
			continue
		}
		if err != nil || strings.Join(gotNames, " ") != tt.want {
			t.Errorf("Select(%v, %v) = %v, %v; want %s", tt.names, tt.defaults, gotNames, err, tt.want)
		}
	}
}

func TestCheckSource(t *testing.T) {
	sha := strings.Repeat("ab", 32)
	for _, tt := range []struct {
		src  config.PolicyPackSource
		want string
	}{
		{config.PolicyPackSource{Path: "packs/acme.yaml"}, ""},
		{config.PolicyPackSource{URL: "https://policy.example/p.yaml", SHA256: sha}, ""},
		{config.PolicyPackSource{URL: "http://127.0.0.1:8080/p.yaml", SHA256: sha}, ""},
		{config.PolicyPackSource{}, "give the pack's path, or its url and sha256"},
		{config.PolicyPackSource{Path: "a.yaml", URL: "https://x/a.yaml", SHA256: sha}, "give a path or a url, not both"},
		{config.PolicyPackSource{URL: "https://policy.example/p.yaml"}, "https://policy.example/p.yaml needs a sha256, which pins the pack so that a change to it is never run unseen"},
		{config.PolicyPackSource{URL: "http://policy.example/p.yaml", SHA256: sha}, "refusing http://policy.example/p.yaml: packs are downloaded over https only"},
		{config.PolicyPackSource{Path: "a.yaml", SHA256: "abc"}, `sha256 "abc" must be 64 hexadecimal digits`},
	} {
		err := CheckSource(tt.src)
		if (tt.want == "" && err != nil) || (tt.want != "" && (err == nil || err.Error() != tt.want)) {
			t.Errorf("CheckSource(%+v) = %v, want %q", tt.src, err, tt.want)
		}
	}
}
