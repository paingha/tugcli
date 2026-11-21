package alias

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestExpand(t *testing.T) {
	aliases := map[string][]string{
		"rs":     {"restart"},
		"wide":   {"get", "-o", "wide"},
		"get":    {"get", "--show-labels"},
		"status": {"get", "pods"}, // shadows tug's own command: never expanded
		"empty":  {},
	}
	builtin := func(w string) bool { return w == "status" || w == "restart" }
	tests := []struct {
		args string
		want string // "" when nothing expands
	}{
		{"rs api", "restart api"},
		{"wide pods -n shop", "get -o wide pods -n shop"},
		{"--debug rs api", "--debug restart api"},
		{"-n shop wide pods", "-n shop get -o wide pods"},
		{"-n rs wide", "-n rs get -o wide"}, // rs is -n's value, not a word
		{"--namespace=rs rs api", "--namespace=rs restart api"},
		{"get pods", "get --show-labels pods"}, // expanded once, not again
		{"status", ""},
		{"restart api", ""},
		{"empty", ""},
		{"logs api", ""},
		{"api rs", ""}, // only the first word is an alias
		{"-- rs", ""},
		{"", ""},
	}
	for _, tt := range tests {
		e, ok := Expand(strings.Fields(tt.args), aliases, builtin)
		got := ""
		if ok {
			got = strings.Join(e.Args, " ")
		}
		if got != tt.want {
			t.Errorf("Expand(%q) = %q, want %q", tt.args, got, tt.want)
		}
	}
	e, _ := Expand([]string{"wide", "pods"}, aliases, builtin)
	if e.Name != "wide" || !slices.Equal(e.Words, []string{"get", "-o", "wide"}) {
		t.Errorf("Expand(wide pods) = %+v", e)
	}
}

func TestSplitAndJoin(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"get -o wide", []string{"get", "-o", "wide"}},
		{"  logs   api  -f ", []string{"logs", "api", "-f"}},
		{`get pods -l 'app in (api, web)'`, []string{"get", "pods", "-l", "app in (api, web)"}},
		{`exec api -- sh -c "echo \"hi\" \$HOME"`, []string{"exec", "api", "--", "sh", "-c", `echo "hi" $HOME`}},
		{`a\ b c`, []string{"a b", "c"}},
		{`''`, []string{""}},
		{"", nil},
	}
	for _, tt := range tests {
		got, err := Split(tt.in)
		if err != nil || !slices.Equal(got, tt.want) {
			t.Errorf("Split(%q) = %q, %v, want %q", tt.in, got, err, tt.want)
			continue
		}
		if tt.want == nil {
			continue
		}
		back, err := Split(Join(got))
		if err != nil || !slices.Equal(back, tt.want) {
			t.Errorf("Split(Join(%q)) = %q, %v", got, back, err)
		}
	}
	for _, bad := range []string{`get 'pods`, `get "pods`} {
		if _, err := Split(bad); err == nil {
			t.Errorf("Split(%q) succeeded, want an unterminated quote error", bad)
		}
	}
	if got := Join([]string{"get", "-l", "app=api", "it's"}); got != `get -l app=api 'it'\''s'` {
		t.Errorf("Join = %s", got)
	}
}

func TestStore(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	if all, err := s.Load(); err != nil || len(all) != 0 {
		t.Fatalf("Load() on a new store = %v, %v", all, err)
	}
	if err := s.Set("wide", []string{"get", "-o", "wide"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Set("rs", []string{"restart"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Set("rs", []string{"rollout", "restart"}); err != nil {
		t.Fatal(err)
	}
	all, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := Names(all); !slices.Equal(got, []string{"rs", "wide"}) {
		t.Errorf("Names = %v", got)
	}
	if !slices.Equal(all["rs"], []string{"rollout", "restart"}) {
		t.Errorf("rs = %v after replacing it", all["rs"])
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(filepath.Join(dir, "aliases.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("aliases.yaml has mode %v, want 0600", fi.Mode().Perm())
		}
	}

	removed, err := s.Remove("rs", "nope")
	if !errors.Is(err, ErrNotFound) || !slices.Equal(removed, []string{"rs"}) {
		t.Errorf("Remove(rs, nope) = %v, %v", removed, err)
	}
	if all, _ := s.Load(); len(all) != 1 {
		t.Errorf("after Remove: %v", all)
	}

	for _, bad := range []string{"", "-x", "two words", "a/b"} {
		if err := s.Set(bad, []string{"get"}); err == nil {
			t.Errorf("Set(%q) succeeded", bad)
		}
	}
	if err := s.Set("none", nil); err == nil {
		t.Error("Set with no expansion succeeded")
	}
}
