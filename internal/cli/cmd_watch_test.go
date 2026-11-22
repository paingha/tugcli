package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"path"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/paingha/tugcli/internal/ops"
)

// watchAPIServer is fakeAPIServer that also serves watches. Each watch is
// held open until the client hangs up; once every resource in want is
// being watched, it first sends the events script holds for its resource.
type watchAPIServer struct {
	*httptest.Server
	mu     sync.Mutex
	open   map[string]int
	want   []string
	script map[string][]any
}

func newWatchAPIServer(t *testing.T) (*watchAPIServer, *atomic.Int32) {
	t.Helper()
	base, writes := fakeAPIServer(t)
	s := &watchAPIServer{open: map[string]int{}}
	s.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if q := r.URL.Query().Get("watch"); q != "true" && q != "1" {
			base.Config.Handler.ServeHTTP(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+secretToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		s.serveWatch(w, r)
	}))
	t.Cleanup(s.Close)
	return s, writes
}

func (s *watchAPIServer) serveWatch(w http.ResponseWriter, r *http.Request) {
	resource := path.Base(r.URL.Path)
	s.mu.Lock()
	s.open[resource]++
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.open[resource]--
		s.mu.Unlock()
	}()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	flush := w.(http.Flusher).Flush
	flush()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for !s.allOpen() {
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
		}
	}
	s.mu.Lock()
	events := s.script[resource]
	s.mu.Unlock()
	enc := json.NewEncoder(w)
	for _, ev := range events {
		if enc.Encode(ev) != nil {
			return
		}
		flush()
	}
	<-r.Context().Done()
}

func (s *watchAPIServer) allOpen() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !slices.ContainsFunc(s.want, func(r string) bool { return s.open[r] == 0 })
}

// play sets what the next run's watches send, once all of want are open,
// after the last run's watches have all closed.
func (s *watchAPIServer) play(t *testing.T, want []string, script map[string][]any) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.mu.Lock()
		idle := !slices.ContainsFunc(slices.Collect(maps.Values(s.open)), func(n int) bool { return n > 0 })
		if idle {
			s.want, s.script = want, script
		}
		s.mu.Unlock()
		if idle {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the last run's watches are still open")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func watchEvent(typ string, obj any) map[string]any {
	return map[string]any{"type": typ, "object": obj}
}

// linesWriter collects output, and cancels a context once it has n lines,
// as Ctrl-C would.
type linesWriter struct {
	mu     sync.Mutex
	b      bytes.Buffer
	n      int
	cancel context.CancelFunc
}

func (w *linesWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.b.Write(p)
	if bytes.Count(w.b.Bytes(), []byte("\n")) >= w.n {
		w.cancel()
	}
	return len(p), nil
}

func (w *linesWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

func TestWatchOfACustomResource(t *testing.T) {
	isolate(t)
	srv, writes := newWatchAPIServer(t)
	mustTug(t, "import", "kubeconfig", writeKubeconfig(t, srv.Server))
	newSession(t)
	mustTug(t, "connect", "payments-prod")
	everything := []string{"databases", "backups", "secrets"}
	ready := watchEvent("MODIFIED", shopDatabase("Database", "orders", "True", "Serving", "accepting connections").Object)

	srv.play(t, everything, map[string][]any{"databases": {ready}})
	out := mustTug(t, "watch", "db/orders", "-n", "shop", "--until", "ready")
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	wantLines := []string{
		`✗ database/orders  BackupFailing: the latest backup failed (owns 2, 1 not ready)`,
		`✗ backup/orders-nightly  UploadFailed: bucket "orders-backups" not found`,
		`✓ database/orders  Ready False → True (Serving: accepting connections)`,
	}
	stamp := regexp.MustCompile(`^\d\d:\d\d:\d\d `)
	if len(lines) != len(wantLines) {
		t.Fatalf("tug watch db/orders --until ready printed %d lines, want %d:\n%s", len(lines), len(wantLines), out)
	}
	for i, line := range lines {
		if !stamp.MatchString(line) || stamp.ReplaceAllString(line, "") != wantLines[i] {
			t.Errorf("tug watch line %d = %q, want a time and %q", i, line, wantLines[i])
		}
	}

	// As JSON, with events, until Ctrl-C.
	warning := &corev1.Event{
		TypeMeta:       metav1.TypeMeta{Kind: "Event", APIVersion: "v1"},
		ObjectMeta:     metav1.ObjectMeta{Name: "orders-nightly.upload", Namespace: "shop", UID: "event-upload"},
		InvolvedObject: corev1.ObjectReference{Kind: "Backup", Name: "orders-nightly", Namespace: "shop", UID: "backup-orders-nightly"},
		Type:           corev1.EventTypeWarning, Reason: "UploadFailed", Message: "bucket missing", Count: 2,
	}
	srv.play(t, append(everything, "events"), map[string][]any{"databases": {ready}, "events": {watchEvent("ADDED", warning)}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stdout := &linesWriter{n: 4, cancel: cancel}
	var stderr bytes.Buffer
	if code := Run(ctx, []string{"watch", "db/orders", "-n", "shop", "--events", "-o", "json"}, strings.NewReader(""), stdout, &stderr); code != 0 {
		t.Fatalf("tug watch -o json ended by Ctrl-C: exit %d, want 0\n%s%s", code, stdout, &stderr)
	}
	var got []string
	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		var ch ops.Change
		if err := json.Unmarshal([]byte(line), &ch); err != nil {
			t.Fatalf("tug watch -o json line %q: %v", line, err)
		}
		got = append(got, fmt.Sprintf("%s %s root=%v %s", ch.Type, ch.Object, ch.Root, ch.Health))
	}
	slices.Sort(got)
	want := []string{
		"condition database/orders root=true ready",
		"event backup/orders-nightly root=false not-ready",
		"state backup/orders-nightly root=false not-ready",
		"state database/orders root=true not-ready",
	}
	if !slices.Equal(got, want) {
		t.Errorf("tug watch -o json --events = %q, want %q", got, want)
	}

	srv.play(t, everything, nil)
	r := tug(t, "watch", "db/orders", "-n", "shop", "--until", "deleted", "--timeout", "300ms")
	if r.code != 1 || !strings.Contains(r.stderr, "tug: timed out after 300ms waiting for database/orders to be deleted") {
		t.Errorf("tug watch --until deleted --timeout 300ms = %d, stderr %q, want a time-out", r.code, r.stderr)
	}

	for _, tt := range []struct{ args, want string }{
		{"watch db/orders -n shop --timeout 1m", "--timeout needs --until"},
		{"watch db/orders -n shop -o yaml", "watch supports -o table or json"},
		{"watch db/orders -n shop --until gone", `--until wants ready or deleted, got "gone"`},
	} {
		if r := tug(t, strings.Fields(tt.args)...); r.code == 0 || !strings.Contains(r.stderr, tt.want) {
			t.Errorf("tug %s = %d, stderr %q, want %q", tt.args, r.code, r.stderr, tt.want)
		}
	}
	if writes.Load() != 0 {
		t.Fatalf("watch wrote to the cluster %d times", writes.Load())
	}
}
