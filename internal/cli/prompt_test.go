package cli

import "testing"

// TestPickWithoutTerminal checks the error a script sees when a name matches
// more than one cluster and there is no terminal to ask on.
func TestPickWithoutTerminal(t *testing.T) {
	isolate(t)
	srv, _ := fakeAPIServer(t)
	mustTug(t, "import", "kubeconfig", writeKubeconfig(t, srv))
	newSession(t)

	r := tug(t, "connect", "payments")
	want := "tug: Clusters: 2 matches (payments-prod, payments-staging); be more specific\n"
	if r.code == 0 || r.stderr != want {
		t.Fatalf("tug connect payments = exit %d, stderr %q, want a failure with %q", r.code, r.stderr, want)
	}
}
