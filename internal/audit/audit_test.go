package audit

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/paingha/tugcli/internal/filelock"
)

var base = time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

func clusters(entries []Entry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Cluster
	}
	return out
}

func TestRecordAndFilter(t *testing.T) {
	ctx := context.Background()
	l := New(filepath.Join(t.TempDir(), "audit"))
	if got, err := l.Entries(ctx, Filter{}); err != nil || len(got) != 0 || got == nil {
		t.Fatalf("Entries before any write = %v, %v; want an empty list", got, err)
	}
	for i, e := range []Entry{
		{Cluster: "prod", ClusterID: "aws/1/prod", Source: SourceTug, Allowed: AllowedUnlocked, Method: "DELETE", Path: "/api/v1/namespaces/web/pods/api-0"},
		{Cluster: "staging", ClusterID: "aws/1/staging", Source: SourceKubectl, Allowed: AllowedReadWrite, Command: []string{"delete", "pod", "api-0"}},
		{Cluster: "prod", ClusterID: "aws/1/prod", Source: SourceProxy, Allowed: AllowedUnlocked, Method: "PATCH", Path: "/apis/apps/v1/namespaces/web/deployments/api", Client: "Helm/3.16.2"},
	} {
		e.Time = base.Add(time.Duration(i) * time.Hour)
		if err := l.Record(ctx, e); err != nil {
			t.Fatal(err)
		}
	}

	tests := []struct {
		name   string
		filter Filter
		want   []string
	}{
		{"all, newest first", Filter{}, []string{"prod", "staging", "prod"}},
		{"by name", Filter{Cluster: "prod"}, []string{"prod", "prod"}},
		{"by ID", Filter{Cluster: "aws/1/staging"}, []string{"staging"}},
		{"since", Filter{Since: base.Add(time.Hour)}, []string{"prod", "staging"}},
		{"nothing", Filter{Cluster: "dev"}, []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := l.Entries(ctx, tt.filter)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(clusters(got), tt.want) {
				t.Errorf("Entries(%+v) = %v, want %v", tt.filter, clusters(got), tt.want)
			}
		})
	}
	got, _ := l.Entries(ctx, Filter{})
	if got[0].Client != "Helm/3.16.2" || got[0].Method != "PATCH" || !got[0].Time.Equal(base.Add(2*time.Hour)) {
		t.Errorf("newest entry = %+v", got[0])
	}
}

func TestRecordTimesEntriesAndKeepsFilesPrivate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data", "audit")
	l := New(dir, WithNow(func() time.Time { return base }))
	if err := l.Record(context.Background(), Entry{Cluster: "prod"}); err != nil {
		t.Fatal(err)
	}
	got, err := l.Entries(context.Background(), Filter{})
	if err != nil || len(got) != 1 || !got[0].Time.Equal(base) {
		t.Fatalf("Entries = %+v, %v; want one entry at %s", got, err, base)
	}
	if runtime.GOOS == "windows" {
		return // no Unix permission bits
	}
	for path, want := range map[string]os.FileMode{dir: 0o700, filepath.Dir(dir): 0o700, l.Path(): 0o600} {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != want {
			t.Errorf("mode of %s = %#o, want %#o", path, got, want)
		}
	}
}

func TestRotation(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	// Each entry is about 100 bytes, so every file holds two.
	l := New(dir, WithMaxSize(250), WithKeep(2))
	for i := range 10 {
		if err := l.Record(ctx, Entry{Time: base.Add(time.Duration(i) * time.Minute), Cluster: fmt.Sprintf("c%d", i), Source: SourceTug, Allowed: AllowedReadWrite}); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []string{l.Path(), l.rotated(1), l.rotated(2)} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Size() > 250 {
			t.Errorf("%s has %d bytes, over the limit", p, fi.Size())
		}
	}
	if _, err := os.Stat(l.rotated(3)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a third rotated file was kept: %v", err)
	}
	got, err := l.Entries(ctx, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"c9", "c8", "c7", "c6", "c5", "c4"}; !slices.Equal(clusters(got), want) {
		t.Errorf("entries after rotation = %v, want %v", clusters(got), want)
	}
}

func TestConcurrentWritersKeepWholeLines(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	var wg sync.WaitGroup
	for w := range 8 {
		// A Log per writer, as separate processes would have.
		l := New(dir)
		wg.Go(func() {
			for i := range 25 {
				if err := l.Record(ctx, Entry{Cluster: fmt.Sprintf("w%d-%d", w, i), Command: []string{strings.Repeat("x", 500)}}); err != nil {
					t.Error(err)
				}
			}
		})
	}
	wg.Wait()
	got, err := New(dir).Entries(ctx, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 200 {
		t.Errorf("read back %d entries, want 200", len(got))
	}
}

func TestRecordGivesUpWhenCancelled(t *testing.T) {
	dir := t.TempDir()
	unlock, err := filelock.Lock(filepath.Join(dir, lockName))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := New(dir).Record(ctx, Entry{Cluster: "prod"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Record while another writer holds the lock = %v, want the context's error", err)
	}
}

func TestEntriesSkipsTornLines(t *testing.T) {
	dir := t.TempDir()
	l := New(dir)
	if err := l.Record(context.Background(), Entry{Time: base, Cluster: "prod"}); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(l.Path(), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprint(f, `{"time":"2026-11-20T13:00:00Z","clus`) // a crash mid-write
	f.Close()
	got, err := l.Entries(context.Background(), Filter{})
	if err != nil || !slices.Equal(clusters(got), []string{"prod"}) {
		t.Fatalf("Entries = %v, %v; want the whole entry only", clusters(got), err)
	}
}

func TestRedactCommand(t *testing.T) {
	tests := []struct{ args, want string }{
		{"delete pod api-0 -n web", "delete pod api-0 -n web"},
		{"-n web --context prod delete pod api-0 --grace-period=0 --force", "-n web --context prod delete pod api-0 --grace-period=0 --force"},
		{"create secret generic db --from-literal=password=hunter2 -n shop", "create secret generic db --from-literal=*** -n shop"},
		{"create secret docker-registry reg --docker-password hunter2", "create secret docker-registry reg --docker-password ***"},
		{"--token hunter2 delete pod x", "--token *** delete pod x"},
		{"set env deploy/api PASSWORD=hunter2 MODE=fast", "set env deploy/api PASSWORD=*** MODE=***"},
		{"patch deploy api -p {\"spec\":{\"x\":\"hunter2\"}}", "patch deploy api -p ***"},
		{"patch deploy api -p{\"x\":\"hunter2\"}", "patch deploy api -p***"},
		{"exec -it api-0 -- mysql -phunter2", "exec -it api-0 -- ***"},
		{"scale deploy/api --replicas 3 -nweb", "scale deploy/api --replicas 3 -nweb"},
		{"config set users.prod.token hunter2", "config set *** ***"},
		{"vault-login --role admin hunter2", "vault-login --role *** ***"},
		{"apply -f deploy.yaml -l app=web", "apply -f deploy.yaml -l app=web"},
	}
	for _, tt := range tests {
		t.Run(tt.args, func(t *testing.T) {
			got := strings.Join(RedactCommand(strings.Fields(tt.args)), " ")
			if got != tt.want {
				t.Errorf("RedactCommand(%q) = %q, want %q", tt.args, got, tt.want)
			}
			if strings.Contains(got, "hunter2") {
				t.Errorf("RedactCommand(%q) kept the secret: %q", tt.args, got)
			}
		})
	}
}
