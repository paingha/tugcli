package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPins(t *testing.T) {
	isolate(t)
	srv, _ := fakeAPIServer(t)
	mustTug(t, "import", "kubeconfig", writeKubeconfig(t, srv))
	newSession(t)
	mustTug(t, "connect", "payments-staging")
	status := func() statusView {
		t.Helper()
		var st statusView
		if err := json.Unmarshal([]byte(mustTug(t, "status", "-o", "json")), &st); err != nil {
			t.Fatal(err)
		}
		return st
	}

	// tug pin set writes, allows and applies the pin.
	repo := t.TempDir()
	r := tug(t, "pin", "set", "payments-prod", "-n", "payments", "--dir", repo)
	if r.code != 0 || !strings.Contains(r.stdout, "Wrote "+filepath.Join(repo, ".tug.yaml")) || !strings.Contains(r.stderr, "pinned by") {
		t.Fatalf("pin set: %+v", r)
	}
	if st := status(); st.Cluster != "payments-prod" || st.Namespace != "payments" || !st.ReadOnly {
		t.Fatalf("after pin set: %+v; want payments-prod/payments, read-only by its labels", st)
	}

	// Moving around inside the tree changes nothing; leaving it goes back.
	sub := filepath.Join(repo, "cmd", "api")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	if r := tug(t, "session", "pin", "--dir", sub); r.code != 0 || r.stderr != "" {
		t.Fatalf("cd within the pinned tree: %+v", r)
	}
	r = tug(t, "session", "pin", "--dir", t.TempDir())
	if r.code != 0 || !strings.Contains(r.stderr, "back to payments-staging") {
		t.Fatalf("leaving the pinned tree: %+v", r)
	}
	if st := status(); st.Cluster != "payments-staging" {
		t.Fatalf("after leaving: %+v", st)
	}

	// A pin file nobody allowed is reported once and not used.
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, ".tug.yaml"), []byte("cluster: payments-prod\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r = tug(t, "session", "pin", "--dir", other)
	if r.code != 0 || !strings.Contains(r.stderr, "run `tug pin allow`") {
		t.Fatalf("an unknown pin file: %+v", r)
	}
	if r := tug(t, "session", "pin", "--dir", other); r.stderr != "" {
		t.Fatalf("the notice was repeated: %q", r.stderr)
	}
	if st := status(); st.Cluster != "payments-staging" {
		t.Fatalf("an unknown pin file switched the cluster: %+v", st)
	}
	mustTug(t, "pin", "allow", other)
	if st := status(); st.Cluster != "payments-prod" {
		t.Fatalf("after pin allow: %+v", st)
	}
	// A changed file needs allowing again.
	if err := os.WriteFile(filepath.Join(other, ".tug.yaml"), []byte("cluster: payments-staging\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := tug(t, "session", "pin", "--dir", other); !strings.Contains(r.stderr, "run `tug pin allow`") {
		t.Fatalf("a changed pin file: %+v", r)
	}
	if out := mustTug(t, "pin", "show", other); !strings.Contains(out, "not allowed") {
		t.Fatalf("pin show: %s", out)
	}

	// Connecting by hand ends the pin, so leaving does not undo it.
	mustTug(t, "pin", "allow", other)
	mustTug(t, "connect", "payments-staging", "-n", "kube-system")
	if r := tug(t, "session", "pin", "--dir", t.TempDir()); r.stderr != "" {
		t.Fatalf("leaving after a manual connect: %q", r.stderr)
	}
	if st := status(); st.Cluster != "payments-staging" || st.Namespace != "kube-system" {
		t.Fatalf("a manual connect was undone: %+v", st)
	}

	if out := mustTug(t, "pin", "revoke", other); !strings.Contains(out, "Revoked") {
		t.Fatalf("pin revoke: %s", out)
	}
}
