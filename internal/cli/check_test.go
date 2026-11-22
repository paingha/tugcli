package cli

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paingha/tugcli/internal/ops"
	"github.com/paingha/tugcli/internal/policy"
)

// The fake API server's shop namespace has no PodDisruptionBudgets, which
// check lists for its podDisruptionBudget checks.
func init() {
	fakeResources["poddisruptionbudgets"] = fakeResource{"policy/v1", "PodDisruptionBudgetList"}
}

// shopPack has a check of each kind of object in the shop namespace.
const shopPack = `apiVersion: tug.policy/v1alpha1
kind: PolicyPack
name: shop
version: 2.1.0
description: What the shop's platform team asks for.
checks:
  - id: team-label
    severity: error
    enforcement: blocking
    description: Deployments say which team owns them.
    remediation: add a team label to the deployment
    match: {kinds: [Deployment]}
    requiredLabels: [team]
  - id: registries
    severity: warning
    description: Pods run images from the shop's registry.
    remediation: "push the image to registry.example.com, then redeploy: ` + "`tug restart`" + `"
    match: {kinds: [Pod]}
    imageRegistries: {allow: [registry.example.com]}
  - id: storage
    severity: warning
    description: Claims use the standard class.
    storageClasses: {allow: [standard]}
  - id: databases-ready
    severity: error
    description: Databases are ready.
    match: {kinds: [Database.db.example.com]}
    conditions: [Ready]
  - id: budget
    severity: warning
    description: Scaled deployments have a budget.
    podDisruptionBudget: {minReplicas: 2}
  - id: secret-owner
    severity: info
    description: Secrets say who owns them.
    match: {kinds: [Secret]}
    requiredAnnotations: [owner]
`

func writePolicyConfig(t *testing.T, home, config string) {
	t.Helper()
	dir := filepath.Join(home, "config")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "shop.yaml"), []byte(shopPack), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCheck(t *testing.T) {
	home := isolate(t)
	srv, writes := fakeAPIServer(t)
	mustTug(t, "import", "kubeconfig", writeKubeconfig(t, srv))
	newSession(t)
	mustTug(t, "connect", "payments-prod") // read-only, which check does not mind
	writePolicyConfig(t, home, "policy:\n  packs:\n    - path: shop.yaml\n")

	// A blocking check fails the command; findings are as why prints them.
	r := tug(t, "check", "-n", "shop", "--pack", "shop")
	if r.code != 1 {
		t.Fatalf("tug check -n shop: exit %d, want 1 for a blocking failure\n%s%s", r.code, r.stdout, r.stderr)
	}
	for _, want := range []string{
		"Checked 11 objects in shop against shop 2.1.0: 1 blocking failure and 5 advisory findings.\n",
		"\nerror    deploy/billing, deploy/checkout, deploy/ml-trainer and 1 more: shop/team-label [blocking]\n" +
			"         has no team label\n" +
			"         next: add a team label to the deployment\n",
		"\nerror    database/orders: shop/databases-ready\n" +
			"         Ready is False (BackupFailing): the latest backup failed\n",
		"\nwarning  pod/scratch, container app: shop/registries\n" +
			"         runs gcr.io/distroless/static-debian12, from gcr.io, which is not one of the allowed registries: registry.example.com\n" +
			"         next: push the image to registry.example.com, then redeploy:\n" +
			"               tug restart\n",
		"\nwarning  pvc/data-ledger-0: shop/storage\n" +
			"         asks for storage class \"fast-ssd\", which is not allowed; allowed: standard\n",
		"\nwarning  deploy/checkout: shop/budget\n" +
			"         no PodDisruptionBudget selects its pods, so a node drain can evict all 2 replicas at once\n",
		"\ninfo     secret/checkout-tls, secret/orders-credentials: shop/secret-owner\n" +
			"         has no owner annotation\n",
	} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("tug check -n shop lacks %q:\n%s", want, r.stdout)
		}
	}
	if strings.Contains(r.stdout, "pod/checkout-") || strings.Contains(r.stdout, "rs/") {
		t.Errorf("tug check reported pods or ReplicaSets that a deployment controls:\n%s", r.stdout)
	}
	if strings.Contains(r.stdout, "hunter2") || r.stderr != "" {
		t.Errorf("tug check -n shop: stderr %q, stdout:\n%s", r.stderr, r.stdout)
	}

	// JSON is the report, with each finding's pack and check.
	r = tug(t, "check", "-n", "shop", "--pack", "shop", "-o", "json")
	var rep policy.Report
	if err := json.Unmarshal([]byte(r.stdout), &rep); err != nil || r.code != 1 {
		t.Fatalf("tug check -o json: exit %d, %v\n%s", r.code, err, r.stdout)
	}
	f := rep.Findings[0]
	if rep.Passed || rep.Namespace != "shop" || rep.Objects != 11 || len(rep.Findings) != 6 ||
		f.Pack != "shop" || f.Check != "team-label" || f.Reason != "team-label" || !f.Blocking || f.Severity != ops.SeverityError || len(f.Objects) != 4 {
		t.Errorf("tug check -o json = passed %v in %q, %d objects, %d findings, first %+v", rep.Passed, rep.Namespace, rep.Objects, len(rep.Findings), f)
	}
	if !strings.Contains(r.stdout, `"pack": "shop"`) || !strings.Contains(r.stdout, `"severity": "error"`) {
		t.Errorf("tug check -o json does not put the finding's fields beside the check's:\n%s", r.stdout)
	}

	// Advisory checks never fail the command.
	out := mustTug(t, "check", "db/orders", "-n", "shop", "--pack", "shop")
	if !strings.HasPrefix(out, "Checked database/orders in shop against shop 2.1.0: 1 advisory finding.\n") {
		t.Errorf("tug check db/orders:\n%s", out)
	}
	out = mustTug(t, "check", "db/inventory", "-n", "shop", "--pack", "shop")
	if out != "Checked database/inventory in shop against shop 2.1.0: nothing wrong found.\n" {
		t.Errorf("tug check db/inventory = %q", out)
	}

	if out := mustTug(t, "check", "cm/checkout-config", "-n", "shop", "--pack", "baseline"); out != "No check of baseline 1.0.0 applies to cm/checkout-config in shop.\n" {
		t.Errorf("tug check of a kind no check names = %q", out)
	}

	// The baseline pack, on a workload.
	out = mustTug(t, "check", "checkout", "-n", "shop", "--pack", "baseline")
	for _, want := range []string{
		"Checked deploy/checkout in shop against baseline 1.0.0: 7 advisory findings.\n",
		"\nwarning  deploy/checkout, container main: baseline/resources\n         sets no cpu or memory request and no memory limit\n",
		"\nwarning  deploy/checkout: baseline/disruption-budget\n",
		"\ninfo     deploy/checkout: baseline/topology-spread\n" +
			"         has no topology spread constraint, so its 2 replicas may all run on one node\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("tug check checkout --pack baseline lacks %q:\n%s", want, out)
		}
	}

	// policy.default chooses the packs that run without --pack.
	writePolicyConfig(t, home, "policy:\n  packs:\n    - path: shop.yaml\n  default: [shop]\n")
	if out := mustTug(t, "check", "db/inventory", "-n", "shop"); !strings.Contains(out, " against shop 2.1.0: ") {
		t.Errorf("tug check with policy.default = %q", out)
	}
	out = mustTug(t, "check", "--list")
	if !strings.HasPrefix(out, "PACK         CHECK             SEVERITY   ENFORCEMENT   DESCRIPTION\n") ||
		!strings.Contains(out, "shop 2.1.0   team-label        error      blocking      Deployments say which team owns them.\n") ||
		strings.Contains(out, "baseline") {
		t.Errorf("tug check --list:\n%s", out)
	}
	var packs []struct {
		Name, Source string
		Checks       []struct {
			policy.Check
			Kinds []string
		}
	}
	if err := json.Unmarshal([]byte(mustTug(t, "check", "--list", "--pack", "baseline", "--pack", "shop", "-o", "json")), &packs); err != nil ||
		len(packs) != 2 || packs[0].Source != "built in" || packs[1].Checks[1].ImageRegistries == nil ||
		strings.Join(packs[1].Checks[2].Kinds, " ") != "PersistentVolumeClaim" {
		t.Errorf("tug check --list -o json = %+v, %v", packs, err)
	}

	for _, tt := range []struct {
		args []string
		want string
	}{
		{[]string{"check", "--pack", "nope"}, `--pack names "nope", but no pack has that name; the packs are baseline, shop`},
		{[]string{"check", "api", "-A"}, "name a target or use --all-namespaces, not both"},
		{[]string{"check", "-o", "name"}, "check supports -o table, json or yaml"},
	} {
		if r := tug(t, tt.args...); r.code != 1 || !strings.Contains(r.stderr, tt.want) {
			t.Errorf("tug %s = exit %d, stderr %q; want %q", strings.Join(tt.args, " "), r.code, r.stderr, tt.want)
		}
	}

	// A pack that changed under its pin is refused before anything runs.
	writePolicyConfig(t, home, "policy:\n  packs:\n    - path: shop.yaml\n      sha256: '"+strings.Repeat("0", 64)+"'\n")
	if r := tug(t, "check", "--list"); r.code != 1 || !strings.Contains(r.stderr, "if the pack changed on purpose, read it and update the pin") {
		t.Errorf("tug check with a wrong pin = exit %d, stderr %q", r.code, r.stderr)
	}
	if writes.Load() != 0 {
		t.Errorf("tug check made %d writes; it only reads", writes.Load())
	}
}

func TestCheckAcrossClusters(t *testing.T) {
	home := isolate(t)
	eu, euWrites := fakeAPIServer(t)
	us, usWrites := fakeAPIServer(t)
	mustTug(t, "import", "kubeconfig", writeFleetKubeconfig(t, map[string]*httptest.Server{"shop-staging-eu": eu, "shop-staging-us": us}))

	r := tug(t, "check", "checkout", "-n", "shop", "--pack", "baseline", "--cluster-selector", "env=staging")
	if r.code != 0 {
		t.Fatalf("exit %d, want 0 for advisory findings\nstdout: %s\nstderr: %s", r.code, r.stdout, r.stderr)
	}
	for _, name := range []string{"shop-staging-eu", "shop-staging-us"} {
		for _, want := range []string{
			name + " | Checked deploy/checkout in shop against baseline 1.0.0: 7 advisory findings.\n",
			name + " | warning  deploy/checkout, container main: baseline/resources\n",
		} {
			if !strings.Contains(r.stdout, want) {
				t.Errorf("stdout lacks %q:\n%s", want, r.stdout)
			}
		}
	}
	if euWrites.Load() != 0 || usWrites.Load() != 0 {
		t.Fatal("a check across clusters wrote")
	}
	assertNoSessions(t, home)
}
