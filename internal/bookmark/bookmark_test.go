package bookmark

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func api() Bookmark {
	return Bookmark{
		Name: "api", Cluster: "payments-prod", Namespace: "shop",
		Target:  &Target{Resource: "deployments.apps", Kind: "Deployment", Name: "api-server"},
		Created: time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC),
	}
}

func TestStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	if got, err := s.List(); err != nil || len(got) != 0 {
		t.Fatalf("List() on a new store = %v, %v; want nothing", got, err)
	}
	place := Bookmark{Name: "edge", Cluster: "edge-eu", Namespace: "ingress"}
	for _, b := range []Bookmark{api(), place} {
		if err := s.Add(b, false); err != nil {
			t.Fatalf("Add(%s) = %v", b.Name, err)
		}
	}

	got, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "api" || got[1].Name != "edge" {
		t.Fatalf("List() = %+v, want api then edge", got)
	}
	b, err := s.Get("api")
	if err != nil {
		t.Fatal(err)
	}
	if b.Target == nil || b.Target.Ref() != "deployments.apps/api-server" || b.Target.String() != "deployment/api-server" {
		t.Fatalf("Get(api).Target = %+v", b.Target)
	}
	if w := b.Where(); w != "deployment/api-server in shop on payments-prod" {
		t.Errorf("Where() = %q", w)
	}
	if w := place.Where(); w != "in ingress on edge-eu" {
		t.Errorf("Where() of a place = %q", w)
	}

	if runtime.GOOS != "windows" {
		fi, err := os.Stat(filepath.Join(dir, "bookmarks.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("bookmarks.yaml has mode %v, want 0600", fi.Mode().Perm())
		}
	}
}

func TestStoreAddRefusesTakenNameUnlessReplacing(t *testing.T) {
	s := New(t.TempDir())
	if err := s.Add(api(), false); err != nil {
		t.Fatal(err)
	}
	other := api()
	other.Namespace = "payments"
	if err := s.Add(other, false); !errors.Is(err, ErrExists) {
		t.Fatalf("Add(taken name) = %v, want ErrExists", err)
	}
	if err := s.Add(other, true); err != nil {
		t.Fatalf("Add(taken name, replace) = %v", err)
	}
	if b, _ := s.Get("api"); b.Namespace != "payments" {
		t.Errorf("after replacing, namespace = %q, want payments", b.Namespace)
	}
}

func TestStoreRemove(t *testing.T) {
	s := New(t.TempDir())
	if err := s.Add(api(), false); err != nil {
		t.Fatal(err)
	}
	removed, err := s.Remove("api", "nope")
	if !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), `"nope"`) {
		t.Errorf("Remove(api, nope) error = %v, want ErrNotFound naming nope", err)
	}
	if len(removed) != 1 || removed[0].Name != "api" {
		t.Errorf("Remove(api, nope) removed %+v, want api", removed)
	}
	if _, err := s.Get("api"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get(api) after Remove = %v, want ErrNotFound", err)
	}
}

func TestValidName(t *testing.T) {
	tests := []struct {
		name string
		ok   bool
	}{
		{"api", true},
		{"api-server.prod_2", true},
		{"9lives", true},
		{"", false},
		{"-api", false},
		{".hidden", false},
		{"a/b", false},
		{"two words", false},
	}
	for _, tt := range tests {
		if err := ValidName(tt.name); (err == nil) != tt.ok {
			t.Errorf("ValidName(%q) = %v, want ok=%v", tt.name, err, tt.ok)
		}
	}
	s := New(t.TempDir())
	if err := s.Add(Bookmark{Name: "api"}, false); err == nil {
		t.Error("Add without a cluster succeeded")
	}
}

func TestStoreRejectsUnknownFields(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "bookmarks.yaml"), []byte("bookmark: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(dir).List(); err == nil || !strings.Contains(err.Error(), "bookmarks.yaml") {
		t.Fatalf("List() of a misspelt file = %v, want an error naming the file", err)
	}
}
