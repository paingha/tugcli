package profile

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

const full = `
cluster: payments
labels: {region: eu}
namespace: shop
safety: read-only
proxy: true
aliases:
  api-logs: logs api -f
  psql: sh sts/postgres -- psql -c "select 1"
forwards:
  - name: db
    target: sts/postgres
    ports: [5432, "9000:http", ":80"]
  - name: api
    target: api
    namespace: web
    addresses: [127.0.0.1]
`

func TestParse(t *testing.T) {
	p, err := Parse("payments", []byte(full))
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "payments" || p.Namespace != "shop" || !p.Proxy || !p.ForceReadOnly() {
		t.Fatalf("Parse = %+v", p)
	}
	if got, want := p.Args(), []string{"payments", "region=eu"}; !slices.Equal(got, want) {
		t.Fatalf("Args() = %v, want %v", got, want)
	}
	if got, want := []string(p.Forwards[0].Ports), []string{"5432", "9000:http", ":80"}; !slices.Equal(got, want) {
		t.Fatalf("ports = %v, want %v", got, want)
	}
	if got := p.ForwardName(p.Forwards[1]); got != "payments.api" {
		t.Fatalf("ForwardName = %q, want payments.api", got)
	}
	words, err := p.Expand("psql", []string{"-x"})
	if want := []string{"sh", "sts/postgres", "--", "psql", "-c", "select 1", "-x"}; err != nil || !slices.Equal(words, want) {
		t.Fatalf("Expand(psql) = %q, %v; want %q", words, err, want)
	}
	if _, err := p.Expand("nope", nil); err == nil || !strings.Contains(err.Error(), "api-logs, psql") {
		t.Fatalf("Expand of an unknown alias: %v", err)
	}

	auto, err := Parse("dev", []byte("cluster: dev\n"))
	if err != nil || auto.Safety != SafetyAuto || auto.ForceReadOnly() {
		t.Fatalf("a profile without safety = %+v, %v; want auto", auto, err)
	}
	upper, err := Parse("dev", []byte("cluster: dev\nsafety: Read-Only\n"))
	if err != nil || !upper.ForceReadOnly() {
		t.Fatalf("safety: Read-Only = %+v, %v; want read-only", upper, err)
	}
}

// A profile may make a session stricter than its cluster's labels but
// never looser, and says so instead of quietly ignoring the setting.
func TestParseRefuses(t *testing.T) {
	tests := []struct {
		name, yaml, want string
	}{
		{"read-write", "cluster: prod\nsafety: read-write\n", "never read-write"},
		{"unknown safety", "cluster: prod\nsafety: maybe\n", "not auto or read-only"},
		{"a field that does not exist", "cluster: prod\nreadWrite: true\n", "unknown field"},
		{"no cluster", "namespace: x\n", "names no cluster"},
		{"bad alias name", "cluster: c\naliases: {Bad Name: status}\n", "alias \"Bad Name\""},
		{"empty alias", "cluster: c\naliases: {x: \"  \"}\n", "alias x is empty"},
		{"open quote", "cluster: c\naliases: {x: \"logs 'api\"}\n", "not closed"},
		{"alias loop", "cluster: c\naliases: {x: profile run x}\n", "runs another alias"},
		{"forward without target", "cluster: c\nforwards: [{name: db}]\n", "has no target"},
		{"two forwards with a name", "cluster: c\nforwards: [{name: db, target: a}, {name: db, target: b}]\n", "a second forward named db"},
		{"forward name with a dot", "cluster: c\nforwards: [{name: a.b, target: a}]\n", "name \"a.b\""},
		{"bad port", "cluster: c\nforwards: [{name: db, target: a, ports: [\"1:2:3\"]}]\n", "is not like"},
		{"port out of range", "cluster: c\nforwards: [{name: db, target: a, ports: [70000]}]\n", "must be a port number"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse("p", []byte(tt.yaml))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Parse = %v, want an error containing %q", err, tt.want)
			}
		})
	}
	_, err := Parse("p", []byte("cluster: prod\nsafety: read-write\n"))
	if !errors.Is(err, ErrReadWrite) {
		t.Fatalf("safety: read-write = %v, want ErrReadWrite", err)
	}
	if _, err := Parse("Has Space", []byte("cluster: c\n")); err == nil {
		t.Fatal("a profile name with a space was accepted")
	}
}

func TestStore(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if ps, err := s.List(); err != nil || len(ps) != 0 {
		t.Fatalf("List before any profile = %v, %v", ps, err)
	}
	if _, err := s.Load("payments"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Load of a missing profile = %v, want ErrNotFound", err)
	}
	if _, err := s.Save("payments", []byte("cluster: prod\nsafety: read-write\n")); err == nil {
		t.Fatal("Save wrote a profile that does not parse")
	}
	if _, err := os.Stat(filepath.Join(s.Dir(), "payments.yaml")); !os.IsNotExist(err) {
		t.Fatalf("a refused profile was written: %v", err)
	}

	body := "# reviewed by the team\n" + full
	if _, err := s.Save("payments", []byte(body)); err != nil {
		t.Fatal(err)
	}
	path, _ := s.Path("payments")
	if b, _ := os.ReadFile(path); string(b) != body {
		t.Fatalf("Save changed the text:\n%s", b)
	}
	if runtime.GOOS != "windows" {
		for p, want := range map[string]os.FileMode{path: 0o600, s.Dir(): 0o700} {
			if st, err := os.Stat(p); err != nil || st.Mode().Perm() != want {
				t.Fatalf("mode of %s = %v, %v; want %v", p, st.Mode().Perm(), err, want)
			}
		}
	}
	if _, err := s.Save("dev", []byte("cluster: dev\n")); err != nil {
		t.Fatal(err)
	}
	// A broken file is reported but does not hide the others.
	if err := os.WriteFile(filepath.Join(s.Dir(), "broken.yaml"), []byte("cluster: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ps, err := s.List()
	if err == nil || !strings.Contains(err.Error(), "broken.yaml") {
		t.Fatalf("List with a broken file: %v", err)
	}
	var names []string
	for _, p := range ps {
		names = append(names, p.Name)
	}
	if want := []string{"dev", "payments"}; !slices.Equal(names, want) {
		t.Fatalf("List = %v, want %v", names, want)
	}
}

func TestTemplateParses(t *testing.T) {
	for _, tt := range []struct{ cluster, ns string }{{"", ""}, {"payments: prod", "it's"}} {
		b := Template("payments", tt.cluster, tt.ns)
		p, err := Parse("payments", b)
		if err != nil {
			t.Fatalf("the template for %q does not parse: %v\n%s", tt.cluster, err, b)
		}
		if tt.cluster != "" && (p.Cluster != tt.cluster || p.Namespace != tt.ns) {
			t.Fatalf("template cluster %q namespace %q, want %q %q", p.Cluster, p.Namespace, tt.cluster, tt.ns)
		}
	}
}

func TestWords(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"logs api -f", []string{"logs", "api", "-f"}},
		{"  a\t b  ", []string{"a", "b"}},
		{`sh db -- psql -c 'select * from t'`, []string{"sh", "db", "--", "psql", "-c", "select * from t"}},
		{`echo "a \"b\" \\ c"`, []string{"echo", `a "b" \ c`}},
		{`a\ b ''`, []string{"a b", ""}},
		{`x"y"'z'`, []string{"xyz"}},
		{"", nil},
	}
	for _, tt := range tests {
		got, err := Words(tt.in)
		if err != nil || !slices.Equal(got, tt.want) {
			t.Errorf("Words(%q) = %q, %v; want %q", tt.in, got, err, tt.want)
		}
	}
	for _, bad := range []string{`'open`, `"open`, `trailing\`} {
		if _, err := Words(bad); err == nil {
			t.Errorf("Words(%q) succeeded", bad)
		}
	}
}
