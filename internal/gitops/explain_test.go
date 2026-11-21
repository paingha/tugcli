package gitops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/paingha/tugcli/internal/ops"
)

func explain(t *testing.T, c *cluster, s *Snapshot, kind, ns, name string) *Explanation {
	t.Helper()
	x, err := Explain(context.Background(), c.c, s, find(t, s, kind, ns, name), ExplainOptions{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	return x
}

// findingLines writes each finding on a line, for comparing and for
// failure messages: "health error deployment/pricing: CreateContainerConfigError".
func findingLines(x *Explanation) []string {
	out := make([]string, len(x.Findings))
	for i, f := range x.Findings {
		out[i] = fmt.Sprintf("%s %s %s: %s", f.Stage, f.Severity, strings.Join(f.Objects, ","), f.Reason)
	}
	return out
}

func TestExplainSaysWhereTheFailureSits(t *testing.T) {
	c := shopCluster(t)
	s := load(t, c)
	for _, tt := range []struct {
		kind, ns, name string
		stage          Stage
		healthy        bool
		summary        string
		// findings are lines of findingLines, in order.
		findings []string
	}{
		{KindKustomization, "flux-system", "apps", StageDependency, false, "something it depends on is not ready", []string{
			"dependency error kustomization/apps,kustomization/infra-configs: DependencyNotReady",
			"reconcile info kustomization/apps: RevisionNotApplied",
		}},
		{KindKustomization, "flux-system", "tenants", StageDependency, false, "something it depends on is not ready", []string{
			"dependency error kustomization/tenants,kustomization/apps: DependencyNotReady",
			"dependency error kustomization/tenants: DependencyNotFound",
			"reconcile info kustomization/tenants: RevisionNotApplied",
		}},
		{KindKustomization, "flux-system", "infra-configs", StageAdmission, false, "the cluster's admission control rejects what it applies", []string{
			"admission error kustomization/infra-configs: ReconciliationFailed",
		}},
		{KindKustomization, "flux-system", "team", StageSource, false, "its source cannot be fetched", []string{
			"source error kustomization/team,gitrepository/team: GitOperationFailed",
		}},
		{KindKustomization, "flux-system", "storefront", StageHealth, false, "what it manages is not healthy", []string{
			"health error pod/pricing-7d9f-a: CreateContainerConfigError",
			"health error database/orders: BackupFailing",
			"health warning configmap/gone: Missing",
		}},
		{KindKustomization, "flux-system", "monitoring", StageReconcile, false, "its latest change is not applied yet", []string{
			"reconcile warning kustomization/monitoring: NotReconciled",
			"reconcile info kustomization/monitoring: RevisionNotApplied",
		}},
		{KindKustomization, "flux-system", "legacy", StageSuspended, false, "it is suspended", []string{
			"suspended warning kustomization/legacy: Suspended",
			"reconcile warning kustomization/legacy: RevisionNotApplied",
		}},
		{KindKustomization, "flux-system", "edge", StageNone, true, "ReconciliationSucceeded: Applied revision: main@sha1:1a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d7e8f9a0b", []string{
			"health info kustomization/edge: Remote",
		}},
		{KindKustomization, "flux-system", "flux-system", StageHealth, false, "what it manages is not healthy", []string{
			"health error gitrepository/team: GitOperationFailed",
			"health error kustomization/apps: DependencyNotReady",
			"health warning gateway/edge-gateway: UnknownKind",
		}},
		{KindKustomization, "flux-system", "infra-controllers", StageNone, true, "ReconciliationSucceeded: Applied revision: main@sha1:1a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d7e8f9a0b", []string{}},
		{KindHelmRelease, "shop", "podinfo", StageApply, false, "its Helm chart fails to install or upgrade", []string{
			"reconcile info helmrelease/podinfo: Stalled",
			"apply error helmrelease/podinfo: UpgradeFailed",
			"apply warning helmrelease/podinfo: RollbackSucceeded",
		}},
		{KindHelmRelease, "shop", "redis", StageSource, false, "its source cannot be fetched", []string{
			"source error helmrelease/redis,helmrepository/bitnami: IndexationFailed",
		}},
		{KindApplication, "argocd", "guestbook", StageHealth, false, "what it manages is not healthy", []string{
			"reconcile warning application/guestbook: OutOfSync",
			"health error pod/pricing-7d9f-a: CreateContainerConfigError",
		}},
		{KindApplication, "argocd", "payments", StageSource, false, "its source cannot be fetched", []string{
			"source error application/payments: ComparisonError",
		}},
		{KindApplication, "argocd", "ledger", StageAdmission, false, "the cluster's admission control rejects what it applies", []string{
			"admission error application/ledger: SyncFailed",
			"admission error statefulset/ledger: SyncFailed",
		}},
		{KindApplication, "argocd", "edge-shop", StageHealth, false, "what it manages is not healthy", []string{
			"reconcile warning application/edge-shop: NotReconciled",
			"health info application/edge-shop: Remote",
			"health error deployment/cart: Degraded",
		}},
		{KindApplication, "argocd", "platform", StageHealth, false, "what it manages is not healthy", []string{
			"health error application/guestbook: Degraded",
			"health error application/payments: ComparisonError",
		}},
	} {
		t.Run(tt.kind+"/"+tt.name, func(t *testing.T) {
			x := explain(t, c, s, tt.kind, tt.ns, tt.name)
			got := findingLines(x)
			if x.Stage != tt.stage || x.Healthy != tt.healthy || x.Summary != tt.summary {
				t.Errorf("Explain = stage %s, healthy %v, summary %q; want %s, %v, %q\n%s",
					x.Stage, x.Healthy, x.Summary, tt.stage, tt.healthy, tt.summary, strings.Join(got, "\n"))
			}
			if !slices.Equal(got, tt.findings) {
				t.Errorf("findings:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(tt.findings, "\n"))
			}
		})
	}
	if w := writes(c); len(w) > 0 {
		t.Errorf("Explain wrote to the cluster: %v", w)
	}
	if r := secretReads(c); len(r) > 0 {
		t.Errorf("Explain read Secrets through the dynamic client, data and all: %v", r)
	}
}

// finding returns the finding with a reason about an object.
func finding(t *testing.T, x *Explanation, object, reason string) Finding {
	t.Helper()
	for _, f := range x.Findings {
		if f.Reason == reason && slices.Contains(f.Objects, object) {
			return f
		}
	}
	t.Fatalf("no %s finding about %s in\n%s", reason, object, strings.Join(findingLines(x), "\n"))
	return Finding{}
}

func TestExplainEvidenceAndNextSteps(t *testing.T) {
	c := shopCluster(t)
	s := load(t, c)
	for _, tt := range []struct {
		kind, ns, name, object, reason string
		message, next                  string
	}{
		{KindKustomization, "flux-system", "tenants", "kustomization/apps", "DependencyNotReady",
			"depends on kustomization/apps, which is not ready: DependencyNotReady: dependency 'flux-system/infra-configs' is not ready; " +
				"the chain stops at kustomization/infra-configs: ReconciliationFailed: ClusterIssuer/letsencrypt dry-run failed (Forbidden): admission webhook",
			"see why: `tug gitops explain kustomization/infra-configs -n flux-system`"},
		{KindKustomization, "flux-system", "tenants", "kustomization/tenants", "DependencyNotFound",
			"depends on kustomization/crds in platform, which is not in the cluster", "check its spec.dependsOn"},
		{KindKustomization, "flux-system", "infra-configs", "kustomization/infra-configs", "ReconciliationFailed",
			`Ready is False: ClusterIssuer/letsencrypt dry-run failed (Forbidden): admission webhook "validate.kyverno.svc-fail" denied the request: ` +
				"resource ClusterIssuer//letsencrypt was blocked due to the following policies require-team-label: check-team: 'validation error: label team is required.'",
			"change the manifest so that the policy named above allows it"},
		{KindKustomization, "flux-system", "team", "gitrepository/team", "GitOperationFailed",
			"its source gitrepository/team cannot be fetched: building artifact: failed to checkout and determine revision: unable to clone 'https://github.com/example/team': authentication required",
			"see the source's conditions and events: `tug why gitrepositories.source.toolkit.fluxcd.io/team -n flux-system`"},
		{KindKustomization, "flux-system", "storefront", "configmap/gone", "Missing",
			"is not in the cluster, though kustomization/storefront manages it; Flux creates it again at its next reconcile, every 10m0s", ""},
		{KindKustomization, "flux-system", "flux-system", "gitrepository/team", "GitOperationFailed",
			"is not ready: building artifact: failed to checkout and determine revision",
			"see its conditions and events: `tug why gitrepositories.source.toolkit.fluxcd.io/team -n flux-system`"},
		{KindKustomization, "flux-system", "flux-system", "kustomization/apps", "DependencyNotReady",
			"is not ready: dependency 'flux-system/infra-configs' is not ready", "see why: `tug gitops explain kustomization/apps -n flux-system`"},
		{KindKustomization, "flux-system", "flux-system", "gateway/edge-gateway", "UnknownKind",
			"the cluster serves no Gateway in gateway.example.com/v1, so it cannot be there", "`tug get crds`"},
		{KindKustomization, "flux-system", "monitoring", "kustomization/monitoring", "NotReconciled",
			"it is at generation 3, but was last reconciled at 2", "`tug why deploy/kustomize-controller -n flux-system`"},
		{KindKustomization, "flux-system", "monitoring", "kustomization/monitoring", "RevisionNotApplied",
			"its source offers revision main@sha1:1a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d7e8f9a0b, but it last applied main@sha1:9f8e7d6c5b4a3f2e1d0c9b8a7f6e5d4c3b2a1f0e; Flux applies it at its next reconcile, every 30m0s", ""},
		{KindKustomization, "flux-system", "legacy", "kustomization/legacy", "Suspended",
			"is suspended, so Flux does not reconcile it", "`flux resume kustomization legacy -n flux-system`"},
		{KindHelmRelease, "shop", "podinfo", "helmrelease/podinfo", "UpgradeFailed",
			`Ready is False: Helm upgrade failed for release shop/podinfo with chart podinfo@6.7.1: cannot patch "podinfo" with kind Deployment`,
			"see the release's history: `helm history podinfo -n shop`"},
		{KindHelmRelease, "shop", "podinfo", "helmrelease/podinfo", "RollbackSucceeded",
			"its release failed, and helm-controller remediated it: Helm rollback to previous release shop/podinfo.v3 with chart podinfo@6.7.0 succeeded", ""},
		{KindHelmRelease, "shop", "redis", "helmrepository/bitnami", "IndexationFailed",
			"its source helmrepository/bitnami in flux-system cannot be fetched: failed to fetch Helm repository index",
			"`tug why helmrepositories.source.toolkit.fluxcd.io/bitnami -n flux-system`"},
		{KindApplication, "argocd", "guestbook", "application/guestbook", "OutOfSync",
			"2 of its objects differ from its source, though Argo CD synced revision 3f2c1a0b9d8e7f6a5b4c3d2e1f0a9b8c7d6e5f4a: they changed in the cluster since, and automated sync applies only new revisions while spec.syncPolicy.automated.selfHeal is off",
			"see what differs: `tug gitops drift application/guestbook -n argocd`"},
		{KindApplication, "argocd", "payments", "application/payments", "ComparisonError",
			"Argo CD cannot compare it with its source: Failed to load target state: failed to generate manifest for source 1 of 1: rpc error: code = Unknown desc = failed to list refs: authentication required",
			"`argocd repo list`"},
		{KindApplication, "argocd", "ledger", "statefulset/ledger", "SyncFailed",
			`admission webhook "validation.gatekeeper.sh" denied the request: [psp-privileged-container] Privileged container is not allowed`, ""},
		{KindApplication, "argocd", "ledger", "application/ledger", "SyncFailed",
			"its last sync, of revision 7e6d5c4b3a2f1e0d9c8b7a6f5e4d3c2b1a0f9e8d, failed: one or more objects failed to apply", "`argocd app get ledger --show-operation`"},
		{KindApplication, "argocd", "edge-shop", "deployment/cart", "Degraded",
			`Argo CD reports it degraded: Deployment "cart" exceeded its progress deadline`, ""},
		{KindApplication, "argocd", "edge-shop", "application/edge-shop", "NotReconciled",
			"Argo CD last compared it with its source 60m ago", "`tug why sts/argocd-application-controller -n argocd`"},
		{KindApplication, "argocd", "platform", "application/guestbook", "Degraded",
			"is not ready: ", "see why: `tug gitops explain application/guestbook -n argocd`"},
	} {
		t.Run(tt.name+"/"+tt.object+"/"+tt.reason, func(t *testing.T) {
			f := finding(t, explain(t, c, s, tt.kind, tt.ns, tt.name), tt.object, tt.reason)
			if !strings.Contains(f.Message, tt.message) {
				t.Errorf("message = %q, want it to contain %q", f.Message, tt.message)
			}
			if !strings.Contains(f.Next, tt.next) {
				t.Errorf("next = %q, want it to contain %q", f.Next, tt.next)
			}
		})
	}
}

func TestExplainChecksWhatItManages(t *testing.T) {
	c := shopCluster(t)
	s := load(t, c)
	x := explain(t, c, s, KindKustomization, "flux-system", "storefront")
	var got []string
	for _, m := range x.Managed {
		got = append(got, fmt.Sprintf("%s %s %s", m.Namespace, m, m.Health))
	}
	want := []string{
		"shop deployment/checkout ready",
		"shop deployment/pricing not-ready",
		"shop database/orders not-ready",
		"shop configmap/checkout-config none",
		"shop secret/checkout-tls none",
		"shop configmap/gone not-ready",
		" clusterrole/system:storefront-reader none",
	}
	if !slices.Equal(got, want) {
		t.Errorf("Managed =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// A HelmRelease without an inventory manages what helm-controller
	// labelled.
	x = explain(t, c, s, KindHelmRelease, "shop", "podinfo")
	if len(x.Managed) != 1 || x.Managed[0].String() != "deployment/podinfo" || x.Managed[0].Health != ops.HealthReady {
		t.Errorf("Managed of helmrelease/podinfo = %+v, want its ready deployment", x.Managed)
	}

	// Argo CD's health and sync of each, without its hooks.
	x = explain(t, c, s, KindApplication, "argocd", "guestbook")
	got = nil
	for _, m := range x.Managed {
		got = append(got, fmt.Sprintf("%s %s %s", m, m.Sync, m.Health))
	}
	want = []string{
		"deployment/pricing OutOfSync not-ready",
		"deployment/checkout Synced ready",
		"service/pricing Synced ready",
		"configmap/guestbook-ui OutOfSync none",
	}
	if !slices.Equal(got, want) {
		t.Errorf("Managed of application/guestbook =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestExplainRepeatsNoAnswerItCouldNotDecode(t *testing.T) {
	c := shopCluster(t)
	// A client that cannot decode an answer quotes it, which for a Secret
	// is its data.
	c.md.PrependReactor("get", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New(`Object 'Kind' is missing in '{"data":{"tls.key":"aHVudGVyMg=="}}'`)
	})
	c.md.PrependReactor("get", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("etcd is unavailable")
	})
	s := load(t, c)
	x := explain(t, c, s, KindKustomization, "flux-system", "storefront")
	b, err := json.Marshal(x)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "aHVudGVyMg") {
		t.Errorf("Explain repeated what a client could not decode, a Secret's data:\n%s", b)
	}
	got := map[string]string{}
	for _, m := range x.Managed {
		got[m.String()] = m.Status
	}
	if got["secret/checkout-tls"] != "could not be read" || got["configmap/checkout-config"] != "could not be read: etcd is unavailable" {
		t.Errorf("statuses = %q, want only what the API server said", got)
	}
}
