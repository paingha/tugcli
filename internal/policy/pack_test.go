package policy

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func TestBaseline(t *testing.T) {
	p := Baseline()
	if p.Name != BaselineName || p.Version == "" || p.Source != "built in" || len(p.Checks) < 7 {
		t.Fatalf("Baseline() = %s %s from %s with %d checks", p.Name, p.Version, p.Source, len(p.Checks))
	}
	for _, c := range p.Checks {
		if c.Blocking() || c.Remediation == "" {
			t.Errorf("baseline check %s: blocking %v, remediation %q; want advisory with a remediation", c.ID, c.Blocking(), c.Remediation)
		}
	}
}

func TestParseRejects(t *testing.T) {
	check := func(rest string) string { return "{id: c, severity: warning, description: d, " + rest + "}" }
	tests := []struct {
		name   string
		pack   string // a whole pack; empty to use checks
		checks []string
		want   string
	}{
		{name: "wrong apiVersion", pack: "apiVersion: v1\nkind: PolicyPack\nname: x\nversion: 1\nchecks: [{id: c, severity: info, description: d, probes: [readiness]}]",
			want: "apiVersion must be tug.policy/v1alpha1"},
		{name: "wrong kind", pack: "apiVersion: tug.policy/v1alpha1\nkind: Policy\nname: x\nversion: 1\nchecks: [{id: c, severity: info, description: d, probes: [readiness]}]",
			want: "kind must be PolicyPack"},
		{name: "no version", pack: "apiVersion: tug.policy/v1alpha1\nkind: PolicyPack\nname: x\nchecks: [{id: c, severity: info, description: d, probes: [readiness]}]",
			want: `version "" must be set`},
		{name: "bad name", pack: "apiVersion: tug.policy/v1alpha1\nkind: PolicyPack\nname: Acme Packs\nversion: 1\nchecks: [{id: c, severity: info, description: d, probes: [readiness]}]",
			want: `name "Acme Packs" must be lower case`},
		{name: "no checks", pack: "apiVersion: tug.policy/v1alpha1\nkind: PolicyPack\nname: x\nversion: 1\nchecks: []", want: "at least one check"},
		{name: "unknown field", pack: "apiVersion: tug.policy/v1alpha1\nkind: PolicyPack\nname: x\nversion: 1\nscript: rm -rf /\nchecks: []",
			want: `unknown field "script"`},
		{name: "unknown check type", checks: []string{check("script: {run: x}")}, want: `unknown field "script"`},
		{name: "duplicate id", checks: []string{check("probes: [readiness]"), check("probes: [liveness]")}, want: `check "c" appears twice`},
		{name: "bad severity", checks: []string{"{id: c, severity: critical, description: d, probes: [readiness]}"}, want: `severity must be info, warning or error, not "critical"`},
		{name: "bad enforcement", checks: []string{check("enforcement: enforce, probes: [readiness]")}, want: `enforcement must be advisory or blocking, not "enforce"`},
		{name: "no description", checks: []string{"{id: c, severity: info, probes: [readiness]}"}, want: "description must say what the check wants"},
		{name: "bad id", checks: []string{"{id: Has_Caps, severity: info, description: d, probes: [readiness]}"}, want: `id "Has_Caps" must be lower case`},
		{name: "nothing to check", checks: []string{check("remediation: r")}, want: "say what to check with one of requiredLabels"},
		{name: "two things to check", checks: []string{check("probes: [readiness], requiredLabels: [team]")}, want: "has requiredLabels and probes; a check checks one thing"},
		{name: "bad probe", checks: []string{check("probes: [ready]")}, want: `probes lists "ready"`},
		{name: "bad label key", checks: []string{check("requiredLabels: [bad key]")}, want: `required label "bad key"`},
		{name: "bad match label key", checks: []string{check("probes: [readiness], match: {labels: {'-x': y}}")}, want: `match.labels key "-x"`},
		{name: "empty registries", checks: []string{check("imageRegistries: {allow: []}")}, want: "imageRegistries.allow must list at least one registry"},
		{name: "registry with scheme", checks: []string{check("imageRegistries: {allow: ['https://ghcr.io']}")}, want: "without a scheme"},
		{name: "empty resources", checks: []string{check("resources: {}")}, want: "resources must list requests, limits or both"},
		{name: "empty security context", checks: []string{check("securityContext: {}")}, want: "securityContext must set runAsNonRoot"},
		{name: "negative replicas", checks: []string{check("podDisruptionBudget: {minReplicas: -1}")}, want: "minReplicas cannot be negative"},
		{name: "empty storage classes", checks: []string{check("storageClasses: {}")}, want: "storageClasses must list the classes to allow"},
		{name: "conditions need kinds", checks: []string{check("conditions: [Ready]")}, want: "match.kinds must name the kinds to check"},
		{name: "unknown kind without group", checks: []string{check("conditions: [Ready], match: {kinds: [Certificate]}")},
			want: "kind Certificate is not built into Kubernetes; name its API group too, as Certificate.example.com"},
		{name: "malformed kind", checks: []string{check("conditions: [Ready], match: {kinds: [cert-manager.io/Certificate]}")}, want: "must be a kind such as Deployment"},
		{name: "kind the type cannot check", checks: []string{check("resources: {requests: [cpu]}, match: {kinds: [ConfigMap]}")},
			want: "cannot check kind ConfigMap; this type checks only Deployment, StatefulSet, DaemonSet, ReplicaSet, Job, CronJob and Pod"},
		{name: "budgets need replicas", checks: []string{check("podDisruptionBudget: {}, match: {kinds: [DaemonSet]}")},
			want: "cannot check kind DaemonSet; this type checks only Deployment, StatefulSet and ReplicaSet"},
		{name: "namespaces and exclusions", checks: []string{check("probes: [readiness], match: {namespaces: [a], excludeNamespaces: [b]}")},
			want: "match has namespaces and excludeNamespaces; use one"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var err error
			if tt.pack != "" {
				_, err = Parse([]byte(tt.pack), "test.yaml")
			} else {
				_, err = parseChecks(tt.checks...)
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) || !strings.HasPrefix(err.Error(), "test.yaml: ") {
				t.Errorf("Parse = %v, want an error from test.yaml with %q", err, tt.want)
			}
		})
	}
}

func TestParseKind(t *testing.T) {
	tests := []struct {
		in   string
		want kind
	}{
		{"Deployment", kindDeployment},
		{"deployment", kindDeployment},
		{"Deployment.apps", kindDeployment},
		{"pod", kindPod},
		{"PersistentVolumeClaim", kindPersistentVolumeClaim},
		{"Certificate.cert-manager.io", kind{Group: "cert-manager.io", Kind: "Certificate"}},
		{"Database.DB.Example.com", kind{Group: "db.example.com", Kind: "Database"}},
		{"Deployment.example.com", kind{Group: "example.com", Kind: "Deployment"}},
	}
	for _, tt := range tests {
		got, err := parseKind(tt.in)
		if err != nil || got != tt.want {
			t.Errorf("parseKind(%q) = %v, %v; want %v", tt.in, got, err, tt.want)
		}
	}
	if s := (kind{Group: "cert-manager.io", Kind: "Certificate"}).String(); s != "Certificate.cert-manager.io" {
		t.Errorf("String() = %q, want Certificate.cert-manager.io", s)
	}
	if s := kindStatefulSet.String(); s != "StatefulSet" {
		t.Errorf("String() = %q, want StatefulSet", s)
	}
}

func TestFixturePack(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "acme.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	p, err := Parse(data, "acme.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Checks[4].Kinds(); !slices.Equal(got, []string{"Deployment"}) {
		t.Errorf("probes check kinds = %v", got)
	}
	if got := p.Checks[9].Kinds(); !slices.Equal(got, []string{"Database.db.example.com"}) {
		t.Errorf("databases-ready check kinds = %v", got)
	}

	web := deployment("web", 3, withMain(func(c *corev1.Container) {
		c.Image = "nginx:1.27"
		c.SecurityContext = nil
	}), func(t *corev1.PodTemplateSpec) { t.Spec.TopologySpreadConstraints = nil })
	web.Labels = map[string]string{"tier": "frontend"}
	coredns := deployment("coredns", 1, withMain(func(c *corev1.Container) { c.Image = "registry.acme.example/coredns:1.12" }))
	coredns.Namespace = "kube-system"
	class := "standard"
	objs := []runtime.Object{web, coredns, claim("data", &class), database("orders", "Ready=False Provisioning: waiting for a volume")}

	r := Evaluate([]*Pack{p}, &Inventory{Objects: toUnstructured(t, objs...)})
	got := map[string][]string{}
	for _, f := range r.Findings {
		for _, o := range f.Objects {
			got[f.Namespace+"/"+o] = append(got[f.Namespace+"/"+o], f.Check)
		}
	}
	want := map[string][]string{
		"shop/deploy/web":            {"non-root", "registries", "team-label", "budget", "probes", "on-call", "zones"},
		"kube-system/deploy/coredns": {"on-call"},
		"shop/pvc/data":              {"storage"},
		"shop/database/orders":       {"databases-ready"},
	}
	for obj, ids := range want {
		slices.Sort(ids)
		have := got[obj]
		slices.Sort(have)
		if !slices.Equal(have, ids) {
			t.Errorf("checks failed by %s = %v, want %v", obj, have, ids)
		}
	}
	if len(got) != len(want) {
		t.Errorf("objects with findings = %v, want those of %v", got, want)
	}
	if r.Passed || r.Blocking() != 3 {
		t.Errorf("Evaluate = passed %v with %d blocking findings, want failed with 3", r.Passed, r.Blocking())
	}
	if f := r.Findings[0]; f.Namespace != "kube-system" {
		t.Errorf("first finding is in %q; want findings in namespace order", f.Namespace)
	}
}
