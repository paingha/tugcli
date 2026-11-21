package cli

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paingha/tugcli/internal/bookmark"
)

func TestBookmarks(t *testing.T) {
	home := isolate(t)
	srv, writes := fakeAPIServer(t)
	mustTug(t, "import", "kubeconfig", writeKubeconfig(t, srv))
	newSession(t)

	if r := tug(t, "bookmark", "add", "bill", "deploy/billing", "-n", "shop"); r.code == 0 || !strings.Contains(r.stderr, "--cluster") {
		t.Errorf("bookmark add before connecting: %+v", r)
	}
	mustTug(t, "connect", "payments-prod")

	// Targets resolve now, fuzzily, and are saved exactly.
	if out := mustTug(t, "bookmark", "add", "bill", "billing", "-n", "shop"); out != "Bookmarked bill: deployment/billing in shop on payments-prod\n" {
		t.Errorf("bookmark add bill: %q", out)
	}
	if out := mustTug(t, "bookmark", "add", "orders", "db/ord", "-n", "shop"); out != "Bookmarked orders: database/orders in shop on payments-prod\n" {
		t.Errorf("bookmark add orders: %q", out)
	}
	if out := mustTug(t, "bookmark", "add", "pay", "-n", "payments"); out != "Bookmarked pay: in payments on payments-prod\n" {
		t.Errorf("bookmark add of a place: %q", out)
	}
	if r := tug(t, "bookmark", "add", "bill", "checkout", "-n", "shop"); r.code == 0 || !strings.Contains(r.stderr, "--replace") {
		t.Errorf("bookmark add of a taken name: %+v", r)
	}
	if r := tug(t, "bookmark", "add", "nothing", "nothing-like-this", "-n", "shop"); r.code == 0 || !strings.Contains(r.stderr, "matches") {
		t.Errorf("bookmark add of a missing object: %+v", r)
	}
	// On another cluster, the target resolves there without moving this
	// terminal.
	r := tug(t, "bookmark", "add", "stg", "deploy/checkout", "-n", "shop", "--cluster", "staging")
	if r.code != 0 || r.stdout != "Bookmarked stg: deployment/checkout in shop on payments-staging\n" {
		t.Fatalf("bookmark add --cluster staging: %+v", r)
	}

	var bs []bookmark.Bookmark
	if err := json.Unmarshal([]byte(mustTug(t, "bookmark", "ls", "-o", "json")), &bs); err != nil {
		t.Fatal(err)
	}
	if len(bs) != 4 || bs[0].Name != "bill" || bs[0].Target == nil || bs[0].Target.Ref() != "deployments.apps/billing" ||
		bs[1].Name != "orders" || bs[1].Target.Ref() != "databases.db.example.com/orders" || bs[2].Target != nil {
		t.Fatalf("bookmark ls -o json: %+v", bs)
	}
	if out := mustTug(t, "bookmark", "ls"); !strings.Contains(out, "orders   database/orders       shop        payments-prod") {
		t.Errorf("bookmark ls:\n%s", out)
	}

	// open runs why by default, and exits as why does.
	r = tug(t, "bookmark", "open", "orders")
	if r.code != 1 || !strings.Contains(r.stdout, "database/orders in shop is not ready") {
		t.Errorf("bookmark open orders: %+v", r)
	}
	if out := mustTug(t, "bookmark", "open", "orders", "tree"); !strings.Contains(out, "backup/orders-nightly") {
		t.Errorf("bookmark open orders tree:\n%s", out)
	}
	if out := mustTug(t, "bookmark", "open", "bill", "logs", "--", "--tail", "5"); !strings.Contains(out, "log of billing-5d4f-x2x4z/main") {
		t.Errorf("bookmark open bill logs:\n%s", out)
	}
	if r := tug(t, "bookmark", "open", "orders", "logs"); r.code == 0 || !strings.Contains(r.stderr, "not on database/orders") {
		t.Errorf("logs of a database: %+v", r)
	}
	if r := tug(t, "bookmark", "open", "pay", "why"); r.code == 0 || !strings.Contains(r.stderr, "no object to why") {
		t.Errorf("why on a place: %+v", r)
	}
	if r := tug(t, "bookmark", "open", "orders", "restart"); r.code == 0 || !strings.Contains(r.stderr, "unknown action") {
		t.Errorf("an action that is not offered: %+v", r)
	}
	if r := tug(t, "bookmark", "open", "nope"); r.code == 0 || !strings.Contains(r.stderr, "tug bookmark ls") {
		t.Errorf("bookmark open of a missing bookmark: %+v", r)
	}

	r = tug(t, "bookmark", "open", "stg")
	if !strings.Contains(r.stdout, "deploy/checkout in shop is healthy") || !strings.Contains(r.stderr, "temporary read-only session") {
		t.Errorf("bookmark open on another cluster: %+v", r)
	}
	if got := strings.TrimSpace(mustTug(t, "status", "-o", "name")); got != "payments-prod" {
		t.Errorf("opening a bookmark on another cluster moved the terminal to %q", got)
	}

	// A place connects this terminal there.
	if out := mustTug(t, "bookmark", "open", "pay"); !strings.Contains(out, "Connected to payments-prod, namespace payments") {
		t.Errorf("bookmark open pay: %q", out)
	}

	r = tug(t, "bookmark", "rm", "bill", "nope")
	if r.stdout != "Removed bookmark bill (deployment/billing in shop on payments-prod)\n" || r.code == 0 || !strings.Contains(r.stderr, `"nope"`) {
		t.Errorf("bookmark rm bill nope: %+v", r)
	}
	if got := mustTug(t, "bookmark", "ls", "-o", "name"); got != "orders\npay\nstg\n" {
		t.Errorf("bookmark ls -o name after rm = %q", got)
	}
	assertMode600(t, filepath.Join(home, "config", "bookmarks.yaml"))
	if writes.Load() != 0 {
		t.Fatalf("bookmarks wrote %d times to the API server", writes.Load())
	}
}
