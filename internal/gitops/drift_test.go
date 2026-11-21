package gitops

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
)

func TestDrift(t *testing.T) {
	c := shopCluster(t)
	s := load(t, c)
	for _, tt := range []struct {
		kind, ns, name string
		detection      string
		note           string
		drifted        []string
	}{
		{KindApplication, "argocd", "guestbook", DetectionArgoCD, "", []string{
			"shop deployment/pricing OutOfSync health=Degraded prune=false",
			"shop configmap/guestbook-ui OutOfSync health= prune=true",
		}},
		{KindApplication, "argocd", "platform", DetectionArgoCD, "", nil},
		{KindApplication, "argocd", "payments", DetectionArgoCD, "Argo CD has not compared it with its source", nil},
		{KindApplication, "argocd", "edge-shop", DetectionArgoCD, "as Argo CD found it in edge-eu", nil},
		{KindHelmRelease, "shop", "podinfo", DetectionHelm, "as helm-controller last reported it", []string{
			"shop deployment/podinfo changed (0 additions, 1 changes, 0 removals) health= prune=false",
			"shop service/podinfo removed health= prune=false",
		}},
		{KindHelmRelease, "shop", "redis", DetectionOff, "set spec.driftDetection.mode to warn", nil},
		{KindKustomization, "flux-system", "apps", DetectionCorrected,
			"undoing changes made in the cluster, and reports no drift between; to compare the cluster with a checkout of its source, run `flux diff kustomization apps -n flux-system --path ./apps/production`", nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, err := Drift(context.Background(), c.c, s, find(t, s, tt.kind, tt.ns, tt.name))
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, d := range r.Drifted {
				got = append(got, fmt.Sprintf("%s %s %s health=%s prune=%v", d.Namespace, d, d.Status, d.Health, d.RequiresPruning))
			}
			if r.Detection != tt.detection || !strings.Contains(r.Note, tt.note) || !slices.Equal(got, tt.drifted) {
				t.Errorf("Drift = %s, note %q,\n%s\nwant %s, a note with %q,\n%s",
					r.Detection, r.Note, strings.Join(got, "\n"), tt.detection, tt.note, strings.Join(tt.drifted, "\n"))
			}
			if r.Drifted == nil {
				t.Errorf("Drifted is nil, which JSON shows as null rather than []")
			}
		})
	}
	if w := writes(c); len(w) > 0 {
		t.Errorf("Drift wrote to the cluster: %v", w)
	}
}
