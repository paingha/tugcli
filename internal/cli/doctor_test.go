package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paingha/tugcli/internal/doctor"
)

func decodeReport(t *testing.T, out string) doctor.Report {
	t.Helper()
	var r doctor.Report
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("doctor -o json: %v\n%s", err, out)
	}
	return r
}

func results(r doctor.Report) map[string]doctor.Result {
	m := map[string]doctor.Result{}
	for _, x := range r.Results {
		m[x.Name] = x
	}
	return m
}

func TestDoctorWithoutACluster(t *testing.T) {
	isolate(t)
	r := tug(t, "doctor", "-o", "json")
	if r.code != 0 {
		t.Fatalf("doctor with warnings only should exit 0: %+v", r)
	}
	got := results(decodeReport(t, r.stdout))
	if got["shell session"].Status != doctor.Warn || !strings.Contains(got["shell session"].Fix, "tug init") {
		t.Fatalf("missing hook: %+v", got["shell session"])
	}
	if got["catalog"].Status != doctor.Warn || got["credential store"].Status != doctor.OK {
		t.Fatalf("setup checks: %+v", got)
	}
	if _, ok := got["TLS"]; ok {
		t.Fatal("cluster checks ran without a cluster")
	}
}

func TestDoctorChecksTheSessionCluster(t *testing.T) {
	home := isolate(t)
	srv, writes := fakeAPIServer(t)
	mustTug(t, "import", "kubeconfig", writeKubeconfig(t, srv))
	newSession(t)
	mustTug(t, "connect", "payments-prod")

	out := mustTug(t, "doctor", "-o", "json")
	report := decodeReport(t, out)
	if report.Cluster != "payments-prod" {
		t.Fatalf("checked %q, want this terminal's cluster", report.Cluster)
	}
	got := results(report)
	for _, name := range []string{"TCP", "TLS", "credentials", "API server", "identity", "permissions"} {
		if got[name].Status != doctor.OK {
			t.Errorf("%s = %+v, want ok", name, got[name])
		}
	}
	if !strings.Contains(got["identity"].Detail, "alice") || !strings.Contains(got["API server"].Detail, "v1.35.1") {
		t.Fatalf("details: %+v", got)
	}
	if writes.Load() != 0 {
		t.Fatalf("doctor wrote to the cluster %d times", writes.Load())
	}

	// A named cluster is checked instead, and the table names it.
	if out := mustTug(t, "doctor", "staging"); !strings.Contains(out, "cluster payments-staging") || !strings.Contains(out, "ok") {
		t.Fatalf("doctor staging:\n%s", out)
	}

	// kubectl's credential plugin policy can block tug: doctor fails and
	// says how to allow it.
	kuberc := filepath.Join(home, "kuberc")
	if err := os.WriteFile(kuberc, []byte("apiVersion: kubectl.config.k8s.io/v1beta1\nkind: Preference\ncredentialPluginPolicy: Allowlist\ncredentialPluginAllowlist:\n- command: aws\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBERC", kuberc)
	res := tug(t, "doctor")
	if res.code != 1 || !strings.Contains(res.stdout, "FAIL") || !strings.Contains(res.stdout, "fix: add `- command: ") {
		t.Fatalf("doctor with a blocking kuberc: %+v", res)
	}
}

func TestDoctorUnknownCluster(t *testing.T) {
	isolate(t)
	srv, _ := fakeAPIServer(t)
	mustTug(t, "import", "kubeconfig", writeKubeconfig(t, srv))
	if r := tug(t, "doctor", "nothing-like-this"); r.code == 0 || !strings.Contains(r.stderr, "no cluster matches") {
		t.Fatalf("got %+v", r)
	}
}
