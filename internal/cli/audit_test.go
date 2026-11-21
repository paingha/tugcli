package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/paingha/tugcli/internal/audit"
	"github.com/paingha/tugcli/internal/render"
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

// summary is an entry as "kind cluster detail", oldest first.
func summary(entries []audit.Entry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range slices.Backward(entries) {
		out = append(out, strings.TrimSpace(e.Kind+" "+e.Cluster+" "+auditDetail(e)))
	}
	return out
}

func TestAuditRecordsWrites(t *testing.T) {
	home := isolate(t)
	useFakeKubectl(t)
	srv, writes := fakeAPIServer(t)
	mustTug(t, "import", "kubeconfig", writeKubeconfig(t, srv))
	sessionID := newSession(t)
	if out := mustTug(t, "audit"); out != "Nothing recorded.\n" {
		t.Fatalf("audit before anything happened: %q", out)
	}

	// tug's own write, through the guard, on a read-write session.
	mustTug(t, "connect", "payments-staging")
	mustTug(t, "restart", "api")

	// On a read-only session, writes are refused, and recorded as refused;
	// once it is unlocked, they go through.
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
	// Reads are not recorded.
	mustTug(t, "get", "pods")
	mustTug(t, "logs", "api")
	if writes.Load() != 2 {
		t.Fatalf("the API server saw %d writes, want the 2 restarts", writes.Load())
	}

	entries := auditEntries(t, "--event", "write")
	want := []struct {
		source, cluster, allowed, intent, action, result string
	}{
		{audit.SourceKubectl, "payments-prod", audit.AllowedUnlocked, "kubectl create", "kubectl create secret generic db --from-literal=*** -n shop", "exit 0"},
		{audit.SourceTug, "payments-prod", audit.AllowedUnlocked, "tug restart", "PATCH deployments/api-server -n default (tug restart)", "200"},
		{audit.SourceTug, "payments-staging", audit.AllowedReadWrite, "tug restart", "PATCH deployments/api-server -n default (tug restart)", "200"},
	}
	if len(entries) != len(want) {
		t.Fatalf("audit has %d writes, want %d: %+v", len(entries), len(want), entries)
	}
	for i, w := range want {
		e := entries[i]
		if e.Kind != audit.KindWrite || e.Source != w.source || e.Cluster != w.cluster || e.Allowed != w.allowed ||
			e.Intent != w.intent || auditAction(e) != w.action || auditResult(render.Colors{}, e) != w.result ||
			e.Session != sessionID || e.ClusterID == "" || e.ID == "" || e.Time.IsZero() {
			t.Errorf("write %d = %+v (%s, %s), want %+v in session %s", i, e, auditAction(e), auditResult(render.Colors{}, e), w, sessionID)
		}
	}
	if entries[1].Path != "/apis/apps/v1/namespaces/default/deployments/api-server" || entries[1].Method != "PATCH" ||
		entries[1].Namespace != "default" || entries[0].Namespace != "shop" {
		t.Errorf("writes = %+v", entries[:2])
	}

	// Everything else that happened, in order.
	all := summary(auditEntries(t))
	wantAll := []string{
		"session-start payments-staging namespace default, read-write",
		"write payments-staging PATCH deployments/api-server -n default (tug restart)",
		"session-end payments-staging namespace default",
		"session-start payments-prod namespace default, read-only",
		"refused payments-prod tug restart",
		"refused payments-prod kubectl delete pod api-0",
		"unlock payments-prod until ",
		"write payments-prod PATCH deployments/api-server -n default (tug restart)",
		"write payments-prod kubectl create secret generic db --from-literal=*** -n shop",
	}
	if len(all) != len(wantAll) {
		t.Fatalf("audit = %q, want %q", all, wantAll)
	}
	for i := range wantAll {
		if !strings.HasPrefix(all[i], wantAll[i]) {
			t.Errorf("entry %d = %q, want %q", i, all[i], wantAll[i])
		}
	}
	refused := auditEntries(t, "--event", "refused")
	if len(refused) != 2 || refused[0].Source != audit.SourceKubectl || refused[0].Reason != audit.ReasonReadOnly ||
		refused[1].Source != audit.SourceTug || refused[1].Intent != "tug restart" || refused[1].Reason != audit.ReasonReadOnly {
		t.Errorf("refused = %+v", refused)
	}
	unlock := auditEntries(t, "--event", "unlock")
	if len(unlock) != 1 || unlock[0].Until.Sub(unlock[0].Time) < 4*time.Minute || unlock[0].Intent != "tug unlock" || !unlock[0].ReadOnly {
		t.Errorf("unlock = %+v, want one for 5m", unlock)
	}

	// Filters, by name or ID, session, namespace and kind.
	if got := auditEntries(t, "--cluster", "payments-staging"); len(got) != 3 || got[0].Cluster != "payments-staging" {
		t.Errorf("--cluster payments-staging: %+v", got)
	}
	if got := auditEntries(t, "--cluster", entries[0].ClusterID, "--event", "write"); len(got) != 2 {
		t.Errorf("--cluster %s: %+v", entries[0].ClusterID, got)
	}
	if got := auditEntries(t, "--session", sessionID); len(got) != len(wantAll) {
		t.Errorf("--session %s kept %d entries, want %d", sessionID, len(got), len(wantAll))
	}
	if got := auditEntries(t, "--session", "nosuchsession"); len(got) != 0 {
		t.Errorf("--session nosuchsession: %+v", got)
	}
	if got := auditEntries(t, "-n", "shop"); len(got) != 1 || got[0].Source != audit.SourceKubectl {
		t.Errorf("-n shop: %+v", got)
	}
	if r := tug(t, "audit", "--event", "writes"); r.code == 0 || !strings.Contains(r.stderr, "session-start") {
		t.Errorf("audit --event writes: %+v", r)
	}
	old := audit.Entry{Time: time.Now().Add(-48 * time.Hour), Source: audit.SourceTug, Cluster: "payments-staging",
		Allowed: audit.AllowedReadWrite, Method: "DELETE", Path: "/api/v1/namespaces/scratch"}
	if err := audit.New(audit.Dir(filepath.Join(home, "data"))).Record(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	if got := auditEntries(t, "--event", "write"); len(got) != 4 || got[3].Path != old.Path || got[3].Kind != audit.KindWrite {
		t.Errorf("audit after an old entry: %+v", got)
	}
	if got := auditEntries(t, "--since", "24h"); len(got) != len(wantAll) {
		t.Errorf("--since 24h kept %d entries, want %d", len(got), len(wantAll))
	}

	table := mustTug(t, "audit", "--cluster", "payments-prod")
	for _, want := range []string{"TIME", "SESSION", "SOURCE", "EVENT", "DETAIL", "RESULT", sessionID,
		"write (unlocked)", "refused", "read-only", "session-start", "unlock", "exit 0", "200",
		"kubectl create secret generic db --from-literal=*** -n shop", "PATCH deployments/api-server -n default (tug restart)"} {
		if !strings.Contains(table, want) {
			t.Errorf("audit table lacks %q:\n%s", want, table)
		}
	}
	if out := mustTug(t, "audit", "-o", "yaml"); !strings.Contains(out, "source: kubectl") || !strings.Contains(out, "kind: session-start") {
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

// Unlocks that are locked early or run out, and sessions that disconnect,
// close or are swept, are all recorded.
func TestAuditRecordsUnlocksAndSessionEnds(t *testing.T) {
	isolate(t)
	srv, _ := fakeAPIServer(t)
	mustTug(t, "import", "kubeconfig", writeKubeconfig(t, srv))
	first := newSession(t)
	mustTug(t, "connect", "payments-prod", "-n", "shop")
	mustTug(t, "unlock", "5m")
	mustTug(t, "lock")
	mustTug(t, "lock") // nothing to lock: not recorded
	mustTug(t, "unlock", "1ms")
	time.Sleep(20 * time.Millisecond)
	mustTug(t, "status") // the next command notices it ran out
	mustTug(t, "status")
	if out := mustTug(t, "status", "-o", "json"); strings.Contains(out, "unlockedUntil") {
		t.Errorf("status after the unlock ran out: %s", out)
	}
	mustTug(t, "disconnect")
	mustTug(t, "disconnect") // not connected: not recorded

	// A terminal that closes, and one whose shell died, which the next new
	// session sweeps away.
	mustTug(t, "connect", "payments-staging")
	mustTug(t, "session", "end")
	dead := newSessionWithPID(t, 999999) // assumed unused
	mustTug(t, "connect", "payments-staging")
	newSession(t)

	got := summary(auditEntries(t))
	want := []string{
		"session-start payments-prod namespace shop, read-only",
		"unlock payments-prod until ",
		"lock payments-prod was until ",
		"unlock payments-prod until ",
		"unlock-expired payments-prod",
		"session-end payments-prod namespace shop",
		"session-start payments-staging namespace default, read-write",
		"session-end payments-staging namespace default",
		"session-start payments-staging namespace default, read-write",
		"session-end payments-staging namespace default",
	}
	if len(got) != len(want) {
		t.Fatalf("audit = %q, want %q", got, want)
	}
	for i := range want {
		if !strings.HasPrefix(got[i], want[i]) {
			t.Errorf("entry %d = %q, want %q", i, got[i], want[i])
		}
	}
	entries := auditEntries(t)
	expired := auditEntries(t, "--event", "unlock-expired")
	if len(expired) != 1 || expired[0].Session != first || !expired[0].Time.Equal(expired[0].Until) {
		t.Errorf("unlock-expired = %+v", expired)
	}
	if e := entries[0]; e.Session != dead || e.Intent != "tug session new" {
		t.Errorf("the swept session's end = %+v, want it from session new", e)
	}
	if e := entries[2]; e.Session != first || e.Intent != "tug session end" {
		t.Errorf("the closed session's end = %+v", e)
	}
}

// newSessionWithPID is newSession for a shell with another process ID.
func newSessionWithPID(t *testing.T, pid int) string {
	t.Helper()
	out := mustTug(t, "session", "new", "--shell", "bash", "--pid", fmt.Sprint(pid))
	for _, line := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(line, "export TUG_SESSION="); ok {
			id := strings.Trim(v, "'")
			t.Setenv("TUG_SESSION", id)
			return id
		}
	}
	t.Fatalf("no TUG_SESSION in %q", out)
	return ""
}

// An unlock that cannot be recorded is taken back.
func TestUnlockThatCannotBeRecordedIsTakenBack(t *testing.T) {
	home := isolate(t)
	srv, _ := fakeAPIServer(t)
	mustTug(t, "import", "kubeconfig", writeKubeconfig(t, srv))
	newSession(t)
	mustTug(t, "connect", "payments-prod")
	// A file where the log's directory goes.
	dir := audit.Dir(filepath.Join(home, "data"))
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	r := tug(t, "unlock", "5m")
	if r.code == 0 || !strings.Contains(r.stderr, "not unlocked") {
		t.Fatalf("unlock with a broken audit log: %+v", r)
	}
	var st statusView
	if err := json.Unmarshal([]byte(mustTug(t, "status", "-o", "json")), &st); err != nil {
		t.Fatal(err)
	}
	if st.Writable {
		t.Fatalf("status after a failed unlock = %+v, want read-only", st)
	}
}
