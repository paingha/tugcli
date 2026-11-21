package cli

import (
	"strings"
	"testing"
)

func TestPickWithoutATerminal(t *testing.T) {
	isolate(t)
	srv, writes := fakeAPIServer(t)
	mustTug(t, "import", "kubeconfig", writeKubeconfig(t, srv))
	newSession(t)
	mustTug(t, "connect", "payments-prod")

	r := tug(t, "pick", "-n", "shop")
	if r.code == 0 || !strings.Contains(r.stderr, "be more specific") || !strings.Contains(r.stderr, "deploy/billing") {
		t.Errorf("pick without a terminal or a query: %+v", r)
	}
	r = tug(t, "pick", "-n", "shop", "--query", "billing")
	if r.code == 0 || !strings.Contains(r.stderr, `matching "billing": 2 matches (deploy/billing, pod/billing-5d4f-x2x4z)`) {
		t.Errorf("pick with an ambiguous query: %+v", r)
	}

	// One match runs why on it, and says what it ran.
	r = tug(t, "pick", "-n", "shop", "--query", "billing crash")
	if r.code != 1 || !strings.Contains(r.stdout, "pod/billing-5d4f-x2x4z in shop is not ready") ||
		!strings.Contains(r.stderr, "tug why pods/billing-5d4f-x2x4z -n shop") {
		t.Errorf("pick --query 'billing crash': %+v", r)
	}
	if out := mustTug(t, "pick", "deploy", "-n", "shop", "--query", "checkout", "--action", "logs"); !strings.Contains(out, "log of checkout-") {
		t.Errorf("pick deploy --action logs:\n%s", out)
	}
	if out := mustTug(t, "pick", "db", "-n", "shop", "--query", "orders", "--action", "tree"); !strings.Contains(out, "backup/orders-nightly") {
		t.Errorf("pick db --action tree:\n%s", out)
	}

	for _, tt := range []struct {
		args []string
		want string
	}{
		{[]string{"pick", "-n", "shop", "--action", "restart"}, "unknown action"},
		{[]string{"pick", "gizmos"}, "no resource type"},
		{[]string{"pick", "-n", "shop", "--query", "zzzz"}, `nothing in namespace shop matches "zzzz"`},
		{[]string{"pick", "db", "-n", "shop", "--query", "inventory", "--action", "pf"}, "not on database/inventory"},
	} {
		if r := tug(t, tt.args...); r.code == 0 || !strings.Contains(r.stderr, tt.want) {
			t.Errorf("tug %s: %+v, want an error with %q", strings.Join(tt.args, " "), r, tt.want)
		}
	}
	if writes.Load() != 0 {
		t.Fatalf("pick wrote %d times to the API server", writes.Load())
	}
}
