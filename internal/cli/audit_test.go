package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/paingha/tugcli/internal/audit"
)

func auditEntries(t *testing.T, args ...string) []audit.Entry {
	t.Helper()
	var entries []audit.Entry
	out := mustTug(t, append([]string{"audit", "-o", "json"}, args...)...)
	if err := json.Unmarshal([]byte(out), &entries); err != nil {
		t.Fatalf("tug audit -o json: %v\n%s", err, out)
	}
	return entries
}

func TestAuditRecordsWrites(t *testing.T) {
	home := isolate(t)
	useFakeKubectl(t)
	srv, writes := fakeAPIServer(t)
	mustTug(t, "import", "kubeconfig", writeKubeconfig(t, srv))
	sessionID := newSession(t)
	if out := mustTug(t, "audit"); out != "No writes recorded.\n" {
		t.Fatalf("audit before any write: %q", out)
	}

	// tug's own write, through the guard, on a read-write session.
	mustTug(t, "connect", "payments-staging")
	mustTug(t, "restart", "api")

	// On a read-only session, refused writes are not recorded; once it is
	// unlocked, they go through and are.
	mustTug(t, "connect", "payments-prod")
	if r := tug(t, "restart", "api"); r.code == 0 {
		t.Fatal("restart allowed on a read-only session")
	}
	if r := tug(t, "delete", "pod", "api-0"); r.code == 0 {
		t.Fatal("kubectl delete allowed on a read-only session")
	}
	mustTug(t, "unlock", "5m")
	mustTug(t, "restart", "api")

	// kubectl on a direct session: tug sees only the command line, and keeps
	// it without the secret.
	out := mustTug(t, "create", "secret", "generic", "db", "--from-literal=password=hunter2", "-n", "shop")
	if !strings.Contains(out, "fake kubectl create secret generic db --from-literal=password=hunter2 -n shop") {
		t.Fatalf("kubectl did not get the command as given: %s", out)
	}
	// Reads are not writes.
	mustTug(t, "get", "pods")
	mustTug(t, "logs", "api")
	if writes.Load() != 2 {
		t.Fatalf("the API server saw %d writes, want the 2 restarts", writes.Load())
	}

	entries := auditEntries(t)
	want := []struct{ source, cluster, allowed, action string }{
		{audit.SourceKubectl, "payments-prod", audit.AllowedUnlocked, "kubectl create secret generic db --from-literal=*** -n shop"},
		{audit.SourceTug, "payments-prod", audit.AllowedUnlocked, "PATCH deployments/api-server -n default"},
		{audit.SourceTug, "payments-staging", audit.AllowedReadWrite, "PATCH deployments/api-server -n default"},
	}
	if len(entries) != len(want) {
		t.Fatalf("audit has %d entries, want %d: %+v", len(entries), len(want), entries)
	}
	for i, w := range want {
		e := entries[i]
		if e.Source != w.source || e.Cluster != w.cluster || e.Allowed != w.allowed || auditAction(e) != w.action ||
			e.Session != sessionID || e.ClusterID == "" || e.Time.IsZero() {
			t.Errorf("entry %d = %+v (%s), want %+v in session %s", i, e, auditAction(e), w, sessionID)
		}
	}
	if entries[1].Path != "/apis/apps/v1/namespaces/default/deployments/api-server" || entries[1].Method != "PATCH" {
		t.Errorf("guard entry = %+v", entries[1])
	}

	// Filters, by name or ID.
	if got := auditEntries(t, "--cluster", "payments-staging"); len(got) != 1 || got[0].Cluster != "payments-staging" {
		t.Errorf("--cluster payments-staging: %+v", got)
	}
	if got := auditEntries(t, "--cluster", entries[0].ClusterID); len(got) != 2 {
		t.Errorf("--cluster %s: %+v", entries[0].ClusterID, got)
	}
	old := audit.Entry{Time: time.Now().Add(-48 * time.Hour), Source: audit.SourceTug, Cluster: "payments-staging",
		Allowed: audit.AllowedReadWrite, Method: "DELETE", Path: "/api/v1/namespaces/scratch"}
	if err := audit.New(audit.Dir(filepath.Join(home, "data"))).Record(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	if got := auditEntries(t); len(got) != 4 || got[3].Path != old.Path {
		t.Errorf("audit after an old entry: %+v", got)
	}
	if got := auditEntries(t, "--since", "24h"); len(got) != 3 {
		t.Errorf("--since 24h kept %d entries, want 3", len(got))
	}

	table := mustTug(t, "audit", "--cluster", "payments-prod")
	for _, want := range []string{"TIME", "SESSION", "SOURCE", "ALLOWED", "WRITE", sessionID, "unlocked",
		"kubectl create secret generic db --from-literal=*** -n shop", "PATCH deployments/api-server -n default"} {
		if !strings.Contains(table, want) {
			t.Errorf("audit table lacks %q:\n%s", want, table)
		}
	}
	if out := mustTug(t, "audit", "-o", "yaml"); !strings.Contains(out, "source: kubectl") {
		t.Errorf("audit -o yaml:\n%s", out)
	}
	if r := tug(t, "audit", "-o", "name"); r.code == 0 || !strings.Contains(r.stderr, "table, json or yaml") {
		t.Errorf("audit -o name: %+v", r)
	}

	path := filepath.Join(home, "data", "audit", "audit.jsonl")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"hunter2", secretToken} {
		if strings.Contains(string(b), secret) {
			t.Errorf("the audit log holds %q:\n%s", secret, b)
		}
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != 0o600 {
			t.Errorf("audit log mode = %#o, want 0o600", got)
		}
	}
}

func TestNamespaceOf(t *testing.T) {
	tests := []struct{ path, want string }{
		{"/api/v1/namespaces/web/pods/api-0", "web"},
		{"/apis/apps/v1/namespaces/web/deployments/api/scale", "web"},
		{"/api/v1/namespaces/web", ""},
		{"/apis/rbac.authorization.k8s.io/v1/clusterroles/admin", ""},
		{"/api/v1/nodes/namespaces/x", ""},
	}
	for _, tt := range tests {
		if got := namespaceOf(tt.path); got != tt.want {
			t.Errorf("namespaceOf(%q) = %q, want %q", tt.path, got, tt.want)
		}
	}
}
