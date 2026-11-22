package policy

import (
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/paingha/tugcli/internal/ops"
)

func TestCheckTypes(t *testing.T) {
	yes, no := true, false
	str := func(s string) *string { return &s }
	image := func(img string) templateOption {
		return withMain(func(c *corev1.Container) { c.Image = img })
	}
	withLabels := func(d runtime.Object, labels map[string]string) runtime.Object {
		d.(metav1.Object).SetLabels(labels)
		return d
	}
	withAnnotations := func(d runtime.Object, a map[string]string) runtime.Object {
		d.(metav1.Object).SetAnnotations(a)
		return d
	}
	inNamespace := func(d runtime.Object, ns string) runtime.Object {
		d.(metav1.Object).SetNamespace(ns)
		return d
	}
	sidecar := func(f func(*corev1.Container)) templateOption {
		return func(t *corev1.PodTemplateSpec) {
			c := goodContainer("sidecar")
			f(&c)
			t.Spec.Containers = append(t.Spec.Containers, c)
		}
	}
	initContainer := func(f func(*corev1.Container)) templateOption {
		return func(t *corev1.PodTemplateSpec) {
			c := goodContainer("init")
			f(&c)
			t.Spec.InitContainers = append(t.Spec.InitContainers, c)
		}
	}
	noSpread := func(t *corev1.PodTemplateSpec) { t.Spec.TopologySpreadConstraints = nil }
	claimTemplate := func(class string) *corev1.PersistentVolumeClaim {
		return &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "data"}, Spec: corev1.PersistentVolumeClaimSpec{StorageClassName: &class}}
	}
	ledger := statefulSet("ledger", 1)
	ledger.Spec.VolumeClaimTemplates = []corev1.PersistentVolumeClaim{*claimTemplate("fast-ssd")}

	tests := []struct {
		name           string
		check          string
		objects        []runtime.Object
		budgets        []policyv1.PodDisruptionBudget
		budgetsUnknown bool
		want           []string
	}{
		{
			name:  "required labels need a value",
			check: "{id: owner, severity: warning, description: d, requiredLabels: [team, cost-centre]}",
			objects: []runtime.Object{
				deployment("api", 1),
				withLabels(deployment("web", 1), nil),
				withLabels(deployment("blank", 1), map[string]string{"team": "", "cost-centre": "42"}),
				withLabels(deployment("full", 1), map[string]string{"team": "shop", "cost-centre": "42"}),
			},
			want: []string{
				"deploy/api |  | has no cost-centre label",
				"deploy/blank |  | has no team label",
				"deploy/web |  | has no team or cost-centre label",
			},
		},
		{
			name:  "required annotations, on cronjobs too by default",
			check: "{id: owner, severity: info, description: d, requiredAnnotations: [example.com/owner]}",
			objects: []runtime.Object{
				deployment("api", 1),
				withAnnotations(deployment("web", 1), map[string]string{"example.com/owner": "alice"}),
				cronJob("nightly"),
			},
			want: []string{"cronjob/nightly,deploy/api |  | has no example.com/owner annotation"},
		},
		{
			name:  "image registries, by registry or path, init containers included",
			check: "{id: registries, severity: error, description: d, imageRegistries: {allow: [registry.example.com, ghcr.io/acme]}}",
			objects: []runtime.Object{
				deployment("api", 1),
				deployment("web", 1, image("nginx:1.27")),
				deployment("tools", 1, image("ghcr.io/acmecorp/tool:1")),
				deployment("pinned", 1, image("ghcr.io/acme/tool@sha256:0123")),
				deployment("setup", 1, initContainer(func(c *corev1.Container) { c.Image = "busybox" })),
				pod("debug", nil, image("localhost:5000/debug:dev")),
				cronJob("nightly", image("docker.io/library/postgres:17")),
			},
			want: []string{
				"cronjob/nightly | main | runs docker.io/library/postgres:17, from docker.io, which is not one of the allowed registries: registry.example.com, ghcr.io/acme",
				"deploy/setup | init | runs busybox, from docker.io, which is not one of the allowed registries: registry.example.com, ghcr.io/acme",
				"deploy/tools | main | runs ghcr.io/acmecorp/tool:1, from ghcr.io, which is not one of the allowed registries: registry.example.com, ghcr.io/acme",
				"deploy/web | main | runs nginx:1.27, from docker.io, which is not one of the allowed registries: registry.example.com, ghcr.io/acme",
				"pod/debug | main | runs localhost:5000/debug:dev, from localhost:5000, which is not one of the allowed registries: registry.example.com, ghcr.io/acme",
			},
		},
		{
			name:  "resource requests and limits, where a limit stands for its request",
			check: "{id: resources, severity: warning, description: d, resources: {requests: [cpu, memory], limits: [memory]}}",
			objects: []runtime.Object{
				deployment("api", 1),
				deployment("limits-only", 1, withMain(func(c *corev1.Container) {
					c.Resources = corev1.ResourceRequirements{Limits: corev1.ResourceList{
						corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("1Gi"),
					}}
				})),
				deployment("bare", 1, withMain(func(c *corev1.Container) { c.Resources = corev1.ResourceRequirements{} })),
				deployment("sidecar", 1, sidecar(func(c *corev1.Container) { delete(c.Resources.Limits, corev1.ResourceMemory) })),
			},
			want: []string{
				"deploy/bare | main | sets no cpu or memory request and no memory limit",
				"deploy/sidecar | sidecar | sets no memory limit",
			},
		},
		{
			name:  "probes, on long-running workloads by default",
			check: "{id: probes, severity: warning, description: d, probes: [readiness, liveness]}",
			objects: []runtime.Object{
				deployment("api", 1),
				deployment("web", 1, withMain(func(c *corev1.Container) { c.ReadinessProbe, c.LivenessProbe = nil, nil })),
				statefulSet("db", 1, withMain(func(c *corev1.Container) { c.LivenessProbe = nil })),
				cronJob("nightly", withMain(func(c *corev1.Container) { c.ReadinessProbe, c.LivenessProbe = nil, nil })),
			},
			want: []string{
				"deploy/web | main | has no readiness or liveness probe",
				"sts/db | main | has no liveness probe",
			},
		},
		{
			name:  "PodDisruptionBudget coverage",
			check: "{id: budget, severity: warning, description: d, podDisruptionBudget: {minReplicas: 2}}",
			objects: []runtime.Object{
				deployment("api", 3),
				deployment("web", 3),
				deployment("solo", 1),
				statefulSet("db", 2),
			},
			budgets: []policyv1.PodDisruptionBudget{
				budget("api", "shop", &metav1.LabelSelector{MatchLabels: map[string]string{"app": "api"}}),
				budget("web", "elsewhere", &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}}),
				budget("nothing", "shop", nil),
				budget("db", "shop", &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
					{Key: "app", Operator: metav1.LabelSelectorOpIn, Values: []string{"db", "cache"}},
				}}),
			},
			want: []string{"deploy/web |  | no PodDisruptionBudget selects its pods, so a node drain can evict all 3 replicas at once"},
		},
		{
			name:    "an empty budget selector takes in every pod of its namespace",
			check:   "{id: budget, severity: warning, description: d, podDisruptionBudget: {}}",
			objects: []runtime.Object{deployment("api", 1), inNamespace(deployment("web", 1), "other")},
			budgets: []policyv1.PodDisruptionBudget{budget("all", "shop", &metav1.LabelSelector{})},
			want:    []string{"deploy/web |  | no PodDisruptionBudget selects its pods, so a node drain can evict its only replica"},
		},
		{
			name:           "budgets that could not be listed prove nothing",
			check:          "{id: budget, severity: warning, description: d, podDisruptionBudget: {}}",
			objects:        []runtime.Object{deployment("api", 3)},
			budgetsUnknown: true,
		},
		{
			name:  "runAsNonRoot from the container or else its pod",
			check: "{id: non-root, severity: warning, description: d, securityContext: {runAsNonRoot: true}}",
			objects: []runtime.Object{
				deployment("api", 1),
				deployment("pod-level", 1, withMain(func(c *corev1.Container) { c.SecurityContext.RunAsNonRoot = nil }), func(t *corev1.PodTemplateSpec) {
					t.Spec.SecurityContext = &corev1.PodSecurityContext{RunAsNonRoot: &yes}
				}),
				deployment("overridden", 1, withMain(func(c *corev1.Container) { c.SecurityContext.RunAsNonRoot = &no }), func(t *corev1.PodTemplateSpec) {
					t.Spec.SecurityContext = &corev1.PodSecurityContext{RunAsNonRoot: &yes}
				}),
				deployment("unset", 1, withMain(func(c *corev1.Container) { c.SecurityContext = nil })),
			},
			want: []string{
				"deploy/overridden | main | may run as root: it sets runAsNonRoot: false",
				"deploy/unset | main | may run as root: neither it nor its pod sets runAsNonRoot: true",
			},
		},
		{
			name:  "privileged and a read-only root filesystem, init containers included",
			check: "{id: hardened, severity: error, description: d, securityContext: {privileged: false, readOnlyRootFilesystem: true}}",
			objects: []runtime.Object{
				deployment("api", 1),
				deployment("agent", 1, withMain(func(c *corev1.Container) {
					c.SecurityContext.Privileged, c.SecurityContext.ReadOnlyRootFilesystem = &yes, nil
				})),
				deployment("setup", 1, initContainer(func(c *corev1.Container) { c.SecurityContext.Privileged = &yes })),
			},
			want: []string{
				"deploy/agent | main | can write to its root filesystem: readOnlyRootFilesystem is not true; runs privileged, with root's access to its node",
				"deploy/setup | init | runs privileged, with root's access to its node",
			},
		},
		{
			name:  "topology spread on named keys",
			check: "{id: spread, severity: info, description: d, topologySpread: {minReplicas: 2, topologyKeys: [topology.kubernetes.io/zone, kubernetes.io/hostname]}}",
			objects: []runtime.Object{
				deployment("api", 3),
				deployment("solo", 1, noSpread),
				statefulSet("db", 2, noSpread),
			},
			want: []string{
				"deploy/api |  | has no topology spread constraint on kubernetes.io/hostname",
				"sts/db |  | has no topology spread constraint on topology.kubernetes.io/zone or kubernetes.io/hostname",
			},
		},
		{
			name:    "topology spread on any key",
			check:   "{id: spread, severity: info, description: d, topologySpread: {}}",
			objects: []runtime.Object{deployment("api", 3), deployment("web", 3, noSpread), deployment("solo", 1, noSpread)},
			want: []string{
				"deploy/solo |  | has no topology spread constraint",
				"deploy/web |  | has no topology spread constraint, so its 3 replicas may all run on one node",
			},
		},
		{
			name:  "storage classes of claims and claim templates",
			check: "{id: storage, severity: warning, description: d, storageClasses: {allow: [standard, gp3]}, match: {kinds: [PersistentVolumeClaim, StatefulSet]}}",
			objects: []runtime.Object{
				claim("data-a", str("standard")),
				claim("data-b", str("fast-ssd")),
				claim("data-c", nil),
				ledger,
			},
			want: []string{
				`pvc/data-b |  | asks for storage class "fast-ssd", which is not allowed; allowed: standard, gp3`,
				"pvc/data-c |  | names no storage class, so it gets the cluster's default, if there is one; allowed: standard, gp3",
				`sts/ledger |  | its volume claim template data asks for storage class "fast-ssd", which is not allowed; allowed: standard, gp3`,
			},
		},
		{
			name:    "the default storage class, when allowed",
			check:   "{id: storage, severity: warning, description: d, storageClasses: {allow: [standard], allowDefault: true}}",
			objects: []runtime.Object{claim("data-a", nil), claim("data-b", str("fast-ssd")), ledger},
			want:    []string{`pvc/data-b |  | asks for storage class "fast-ssd", which is not allowed; allowed: standard`},
		},
		{
			name:  "custom resource conditions must be True",
			check: "{id: ready, severity: error, description: d, conditions: [Ready, Synced], match: {kinds: [Database.db.example.com]}}",
			objects: []runtime.Object{
				database("orders", "Ready=False BackupFailing: the latest backup failed"),
				database("inventory", "Ready=True Serving: accepting connections", "Synced=True Synced: up to date"),
				database("new"),
			},
			want: []string{
				"database/new |  | has no Ready condition",
				"database/new,database/orders |  | has no Synced condition",
				"database/orders |  | Ready is False (BackupFailing): the latest backup failed",
			},
		},
		{
			name:  "match on labels",
			check: "{id: owner, severity: info, description: d, requiredAnnotations: [owner], match: {labels: {team: shop}}}",
			objects: []runtime.Object{
				deployment("api", 1),
				withLabels(deployment("web", 1), map[string]string{"team": "payments"}),
			},
			want: []string{"deploy/api |  | has no owner annotation"},
		},
		{
			name:  "match on kinds",
			check: "{id: owner, severity: info, description: d, requiredAnnotations: [owner], match: {kinds: [statefulset]}}",
			objects: []runtime.Object{
				deployment("api", 1),
				statefulSet("db", 1),
			},
			want: []string{"sts/db |  | has no owner annotation"},
		},
		{
			name:  "match leaves out namespaces",
			check: "{id: owner, severity: info, description: d, requiredAnnotations: [owner], match: {excludeNamespaces: [kube-system]}}",
			objects: []runtime.Object{
				deployment("api", 1),
				inNamespace(deployment("coredns", 1), "kube-system"),
			},
			want: []string{"deploy/api |  | has no owner annotation"},
		},
		{
			name:  "match takes in only some namespaces",
			check: "{id: owner, severity: info, description: d, requiredAnnotations: [owner], match: {namespaces: [kube-system]}}",
			objects: []runtime.Object{
				deployment("api", 1),
				inNamespace(deployment("coredns", 1), "kube-system"),
			},
			want: []string{"deploy/coredns |  | has no owner annotation"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := packOf(t, tt.check)
			inv := &Inventory{Objects: toUnstructured(t, tt.objects...), PodDisruptionBudgets: tt.budgets, BudgetsUnknown: tt.budgetsUnknown}
			r := Evaluate([]*Pack{p}, inv)
			got := findings(r)
			slices.Sort(got)
			if !slices.Equal(got, tt.want) {
				t.Errorf("Evaluate(%s) =\n  %s\nwant\n  %s", tt.check, strings.Join(got, "\n  "), strings.Join(tt.want, "\n  "))
			}
			if !r.Passed {
				t.Errorf("Evaluate(%s).Passed = false, want true for an advisory check", tt.check)
			}
		})
	}
}

func TestImageRepository(t *testing.T) {
	tests := []struct{ image, want string }{
		{"nginx", "docker.io/library/nginx"},
		{"nginx:1.27", "docker.io/library/nginx"},
		{"acme/api:v2", "docker.io/acme/api"},
		{"index.docker.io/acme/api", "docker.io/acme/api"},
		{"ghcr.io/acme/api@sha256:0123", "ghcr.io/acme/api"},
		{"ghcr.io/acme/api:1.0@sha256:0123", "ghcr.io/acme/api"},
		{"localhost:5000/api:dev", "localhost:5000/api"},
		{"localhost/api", "localhost/api"},
		{"Registry.Example.com:443/Team/API:1", "registry.example.com:443/team/api"},
	}
	for _, tt := range tests {
		if got := imageRepository(tt.image); got != tt.want {
			t.Errorf("imageRepository(%q) = %q, want %q", tt.image, got, tt.want)
		}
	}
}

func TestEvaluateGroupsAndOrders(t *testing.T) {
	p := packOf(t,
		"{id: owner, severity: info, description: d, requiredAnnotations: [owner]}",
		"{id: probes, severity: warning, description: d, probes: [readiness]}",
		"{id: registries, severity: warning, enforcement: blocking, description: d, remediation: use the mirror, imageRegistries: {allow: [registry.example.com]}}",
	)
	noProbe := withMain(func(c *corev1.Container) { c.ReadinessProbe = nil })
	objs := []runtime.Object{
		deployment("web", 1, noProbe), deployment("api", 1, noProbe),
		deployment("tools", 1, withMain(func(c *corev1.Container) { c.Image = "busybox" })),
		claim("data", nil), // no check applies to it
	}
	r := Evaluate([]*Pack{p}, &Inventory{Objects: toUnstructured(t, objs...), Unlisted: []string{"cronjobs.batch"}})
	want := []string{
		"deploy/tools | main | runs busybox, from docker.io, which is not one of the allowed registries: registry.example.com [blocking]",
		"deploy/api,deploy/web | main | has no readiness probe",
		"deploy/api,deploy/tools,deploy/web |  | has no owner annotation",
	}
	if got := findings(r); !slices.Equal(got, want) {
		t.Errorf("Evaluate findings =\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
	if r.Passed || r.Blocking() != 1 || r.Objects != 3 {
		t.Errorf("Evaluate = passed %v, %d blocking, %d objects; want false, 1, 3", r.Passed, r.Blocking(), r.Objects)
	}
	f := r.Findings[0]
	if f.Pack != "test" || f.Check != "registries" || f.Reason != "registries" || f.Severity != ops.SeverityWarning || f.Next != "use the mirror" || f.Namespace != "shop" {
		t.Errorf("blocking finding = %+v", f)
	}
	if !slices.Equal(r.Unlisted, []string{"cronjobs.batch"}) || !slices.Equal(r.Packs, []PackRef{{Name: "test", Version: "1.0.0"}}) {
		t.Errorf("Evaluate = unlisted %v, packs %v", r.Unlisted, r.Packs)
	}
}

func TestBaselinePassesGoodWorkloads(t *testing.T) {
	good := []runtime.Object{deployment("api", 3), statefulSet("db", 2), cronJob("nightly"), pod("debug", nil)}
	budgets := []policyv1.PodDisruptionBudget{budget("all", "shop", &metav1.LabelSelector{})}
	r := Evaluate([]*Pack{Baseline()}, &Inventory{Objects: toUnstructured(t, good...), PodDisruptionBudgets: budgets})
	if len(r.Findings) != 0 || !r.Passed || r.Objects != 4 {
		t.Errorf("baseline on good workloads = %d objects, passed %v, findings:\n  %s", r.Objects, r.Passed, strings.Join(findings(r), "\n  "))
	}

	bare := deployment("bare", 2, func(t *corev1.PodTemplateSpec) {
		t.Spec = corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "nginx"}}}
	})
	r = Evaluate([]*Pack{Baseline()}, &Inventory{Objects: toUnstructured(t, bare)})
	var ids []string
	for _, f := range r.Findings {
		ids = append(ids, f.Check)
	}
	want := []string{"resources", "readiness-probe", "run-as-non-root", "disruption-budget", "liveness-probe", "read-only-root-filesystem", "topology-spread"}
	if !slices.Equal(ids, want) || !r.Passed {
		t.Errorf("baseline on a bare deployment = %v, passed %v; want %v, passed", ids, r.Passed, want)
	}
}
