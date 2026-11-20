package pin

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestFindWalksUp(t *testing.T) {
	root := t.TempDir()
	deep := filepath.Join(root, "svc", "api", "cmd")
	if err := os.MkdirAll(deep, 0o700); err != nil {
		t.Fatal(err)
	}
	if f, err := Find(deep); err != nil || f != nil {
		t.Fatalf("Find with no pin file = %+v, %v", f, err)
	}
	if err := os.WriteFile(filepath.Join(root, "svc", FileName), []byte("cluster: staging\nnamespace: payments\nlabels: {region: eu}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := Find(deep)
	if err != nil {
		t.Fatal(err)
	}
	if f.Path != filepath.Join(root, "svc", FileName) || f.File.Namespace != "payments" || f.Hash == "" {
		t.Fatalf("Find = %+v", f)
	}
	if got := f.File.Args(); !slices.Equal(got, []string{"staging", "region=eu"}) {
		t.Fatalf("Args = %v", got)
	}
}

func TestFindRefusesBadFiles(t *testing.T) {
	tests := []struct{ body, want string }{
		{"clustr: staging\n", "clustr"},
		{"namespace: payments\n", "names no cluster"},
		{"cluster: [\n", FileName},
	}
	for _, tt := range tests {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, FileName), []byte(tt.body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Find(dir); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("Find(%q) = %v, want an error mentioning %q", tt.body, err, tt.want)
		}
	}
}

func TestTrustFollowsTheContent(t *testing.T) {
	dir := t.TempDir()
	trust := NewTrust(t.TempDir())
	f, err := Write(dir, File{Cluster: "staging", Namespace: "payments"})
	if err != nil {
		t.Fatal(err)
	}
	if trust.Allowed(f) {
		t.Fatal("a new pin file is trusted before it is allowed")
	}
	if err := trust.Allow(f); err != nil {
		t.Fatal(err)
	}
	if !trust.Allowed(f) {
		t.Fatal("an allowed pin file is not trusted")
	}
	// Someone changes the file: it needs allowing again.
	changed, err := Write(dir, File{Cluster: "prod", Namespace: "payments"})
	if err != nil {
		t.Fatal(err)
	}
	if trust.Allowed(changed) {
		t.Fatal("a changed pin file kept its trust")
	}
	if had, err := trust.Revoke(f.Path); err != nil || !had {
		t.Fatalf("Revoke = %v, %v", had, err)
	}
	if paths, _ := trust.Paths(); len(paths) != 0 {
		t.Fatalf("Paths after Revoke = %v", paths)
	}
}
