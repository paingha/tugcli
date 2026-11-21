package gitops

import (
	"cmp"
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
)

// How a tool reports drift for an object.
const (
	// DetectionArgoCD is Argo CD's comparison of each object an
	// Application manages with its source.
	DetectionArgoCD = "argocd"
	// DetectionHelm is helm-controller's drift detection, on for a
	// HelmRelease whose spec.driftDetection.mode is warn or enabled.
	DetectionHelm = "helm"
	// DetectionOff is a HelmRelease without drift detection.
	DetectionOff = "off"
	// DetectionCorrected is a Kustomization: kustomize-controller undoes
	// drift each time it reconciles, and reports none between.
	DetectionCorrected = "corrected"
)

// DriftReport is what Drift found: the objects that Flux or Argo CD itself
// reports out of sync with their source.
type DriftReport struct {
	Object *Object `json:"object"`
	// Detection says how the tool reports drift for the object: argocd,
	// helm, off or corrected.
	Detection string `json:"detection"`
	// Note says what the report covers, and what it cannot.
	Note    string    `json:"note,omitempty"`
	Drifted []Drifted `json:"drifted"`
}

// Drifted is an object the tool reports out of sync with its source.
type Drifted struct {
	Kind      string `json:"kind"`
	Group     string `json:"group,omitempty"`
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
	// Status is what the tool says of it: Argo CD's OutOfSync, or
	// helm-controller's account of the change.
	Status string `json:"status"`
	// Health is Argo CD's health of it; Missing means it is not in the
	// cluster.
	Health string `json:"health,omitempty"`
	// RequiresPruning is true for an object that is no longer in the
	// source, which Argo CD deletes at a sync with pruning.
	RequiresPruning bool `json:"requiresPruning,omitempty"`
	// Seen is when helm-controller reported it.
	Seen time.Time `json:"seen,omitzero"`
}

// String returns the kind in lower case and the name: "deployment/api".
func (d Drifted) String() string { return strings.ToLower(d.Kind) + "/" + d.Name }

// Drift returns the objects that Flux or Argo CD reports out of sync for a
// Kustomization, HelmRelease or Application: what Argo CD found when it last
// compared an Application with its source, or what helm-controller's drift
// detection last reported for a HelmRelease. tug computes no diff of its
// own, and only reads.
func Drift(ctx context.Context, c Clients, s *Snapshot, o *Object) (*DriftReport, error) {
	r := &DriftReport{Object: o, Drifted: []Drifted{}}
	switch o.Kind {
	case KindApplication:
		r.Detection = DetectionArgoCD
		for _, res := range argoResources(o.u) {
			if res.Status != "OutOfSync" || res.Hook {
				continue
			}
			r.Drifted = append(r.Drifted, Drifted{
				Kind: res.Kind, Group: res.Group, Name: res.Name, Namespace: res.Namespace,
				Status: res.Status, Health: res.Health, RequiresPruning: res.RequiresPruning,
			})
		}
		switch {
		case o.Sync == "Unknown" || o.Sync == "":
			r.Note = "Argo CD has not compared it with its source, so it reports no drift" + prefixed(": ", o.Message)
		case !o.inCluster():
			r.Note = fmt.Sprintf("as Argo CD found it in %s", cmp.Or(o.Destination.Name, o.Destination.Server))
		}
	case KindHelmRelease:
		mode := text(o.u.Object, "spec", "driftDetection", "mode")
		if mode != "warn" && mode != "enabled" {
			r.Detection = DetectionOff
			r.Note = "helm-controller does not check it for drift; set spec.driftDetection.mode to warn, or to enabled to also correct it"
			return r, nil
		}
		r.Detection = DetectionHelm
		r.Note = "as helm-controller last reported it; it reports drift each time it reconciles, every " + cmp.Or(o.Interval, "interval")
		if mode == "enabled" {
			r.Note += ", and corrects it"
		}
		drifted, err := helmDrift(ctx, c, o)
		if err != nil {
			return nil, err
		}
		r.Drifted = drifted
	default:
		r.Detection = DetectionCorrected
		r.Note = fmt.Sprintf("kustomize-controller applies its source each time it reconciles, every %s, undoing changes made in the cluster, and reports no drift between; "+
			"to compare the cluster with a checkout of its source, run `flux diff kustomization %s -n %s --path %s`",
			cmp.Or(o.Interval, "interval"), o.Name, o.Namespace, cmp.Or(text(o.u.Object, "spec", "path"), "."))
	}
	return r, nil
}

// driftLine is one object in helm-controller's drift event, such as
// "Deployment/shop/podinfo changed (0 additions, 1 changes, 0 removals)".
var driftLine = regexp.MustCompile(`^\s*([A-Za-z][A-Za-z0-9]*)/(?:([a-z0-9][a-z0-9.-]*)/)?([^\s/]+)\s+(.+)$`)

// helmDrift reads the objects in helm-controller's latest drift event about
// a HelmRelease.
func helmDrift(ctx context.Context, c Clients, o *Object) ([]Drifted, error) {
	sel := fields.OneTermEqualSelector("involvedObject.uid", string(o.u.GetUID())).String()
	l, err := c.Typed.CoreV1().Events(o.Namespace).List(ctx, metav1.ListOptions{FieldSelector: sel})
	if err != nil {
		return nil, fmt.Errorf("listing the events of %s: %w", o, err)
	}
	var latest *corev1.Event
	for i, e := range l.Items {
		if e.InvolvedObject.UID != o.u.GetUID() || !strings.Contains(e.Reason, "Drift") {
			continue
		}
		if latest == nil || eventTime(&e).After(eventTime(latest)) {
			latest = &l.Items[i]
		}
	}
	out := []Drifted{}
	if latest == nil {
		return out, nil
	}
	for _, line := range strings.Split(latest.Message, "\n") {
		m := driftLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		out = append(out, Drifted{Kind: m[1], Namespace: m[2], Name: m[3], Status: strings.TrimSpace(m[4]), Seen: eventTime(latest)})
	}
	slices.SortStableFunc(out, func(a, b Drifted) int {
		return cmp.Or(strings.Compare(a.Kind, b.Kind), strings.Compare(a.Namespace, b.Namespace), strings.Compare(a.Name, b.Name))
	})
	return out, nil
}

// eventTime is when an event was last seen.
func eventTime(e *corev1.Event) time.Time {
	switch {
	case e.Series != nil && !e.Series.LastObservedTime.IsZero():
		return e.Series.LastObservedTime.Time
	case !e.LastTimestamp.IsZero():
		return e.LastTimestamp.Time
	case !e.EventTime.IsZero():
		return e.EventTime.Time
	default:
		return e.CreationTimestamp.Time
	}
}
