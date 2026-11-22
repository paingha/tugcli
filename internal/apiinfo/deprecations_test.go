package apiinfo

import (
	"context"
	"encoding/json"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clienttesting "k8s.io/client-go/testing"
)

// writtenMeta is an object last written by its managers at versions, as
// the metadata client lists it at apiVersion.
func writtenMeta(apiVersion, kind, name string, versions ...string) *metav1.PartialObjectMetadata {
	m := partialMeta(apiVersion, kind, "web", name)
	for _, v := range versions {
		m.ManagedFields = append(m.ManagedFields, metav1.ManagedFieldsEntry{Manager: "helm", Operation: metav1.ManagedFieldsOperationUpdate, APIVersion: v})
	}
	return m
}

func TestDeprecations(t *testing.T) {
	apis := discover(t, fakeCluster())
	applied := writtenMeta("autoscaling/v2", "HorizontalPodAutoscaler", "applied")
	applied.Annotations = map[string]string{lastApplied: `{"apiVersion":"autoscaling/v2beta2","kind":"HorizontalPodAutoscaler"}`}
	md := newMetadataClient(
		writtenMeta("autoscaling/v2", "HorizontalPodAutoscaler", "helm", "autoscaling/v2beta2", "autoscaling/v2"),
		applied,
		writtenMeta("autoscaling/v2", "HorizontalPodAutoscaler", "current", "autoscaling/v2"),
		writtenMeta("policy/v1", "PodDisruptionBudget", "old", "policy/v1beta1"),
		writtenMeta("example.com/v1", "Widget", "w1", "example.com/v1beta1"),
		writtenMeta("example.com/v1", "Widget", "w2", "example.com/v1"),
	)
	md.PrependReactor("list", "podsecuritypolicies", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "policy", Resource: "podsecuritypolicies"}, "", nil)
	})

	r, err := Deprecations(context.Background(), md, apis, []CRD{*parsedWidgets(t)}, Upstream())
	if err != nil {
		t.Fatal(err)
	}
	if r.Server.Minor != "1.24" || r.TableChecked != Upstream().Checked {
		t.Errorf("report server %+v, table %s", r.Server, r.TableChecked)
	}
	if len(r.Unlisted) != 1 || r.Unlisted[0] != "podsecuritypolicies.policy" {
		t.Errorf("Unlisted = %v, want the forbidden podsecuritypolicies.policy", r.Unlisted)
	}
	n := func(i int) *int { return &i }
	want := []Deprecation{
		{GroupVersion: "autoscaling/v2beta2", Kind: "HorizontalPodAutoscaler", Resource: "horizontalpodautoscalers.autoscaling", Served: true,
			RemovedIn: "1.26", Replacement: "autoscaling/v2", ReplacementSince: "1.23", Objects: n(2)},
		{GroupVersion: "example.com/v1beta1", Kind: "Widget", Resource: "widgets.example.com", CRD: "widgets.example.com", Served: true,
			Replacement: "example.com/v1", Deprecated: true, Warning: "example.com/v1beta1 Widget is deprecated; use example.com/v1", Stored: true, Objects: n(1)},
		{GroupVersion: "policy/v1beta1", Kind: "PodDisruptionBudget", Resource: "poddisruptionbudgets.policy", Served: true,
			RemovedIn: "1.25", Replacement: "policy/v1", ReplacementSince: "1.21", Objects: n(1)},
		{GroupVersion: "policy/v1beta1", Kind: "PodSecurityPolicy", Resource: "podsecuritypolicies.policy", Served: true,
			RemovedIn: "1.25", Note: Upstream().Removals[findRemoval(t, "policy/v1beta1", "PodSecurityPolicy")].Note},
	}
	if got, wantJSON := jsonOf(t, r.Items), jsonOf(t, want); got != wantJSON {
		t.Errorf("Deprecations items =\n%s\nwant\n%s", got, wantJSON)
	}
}

func findRemoval(t *testing.T, gv, kind string) int {
	t.Helper()
	for i, r := range Upstream().Removals {
		if r.GroupVersion == gv && len(r.Kinds) == 1 && r.Kinds[0] == kind {
			return i
		}
	}
	t.Fatalf("no removal of %s %s", gv, kind)
	return 0
}

func jsonOf(t *testing.T, v any) string {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A definition with every object migrated and nothing deprecated is not
// reported, and an unmigrated version that is no longer served is.
func TestCRDDeprecations(t *testing.T) {
	migrated := *parsedWidgets(t)
	migrated.StoredVersions = []string{"v1"}
	migrated.Versions = []CRDVersion{{Name: "v1", Served: true, Storage: true}, {Name: "v1beta1"}}
	if got := FindDeprecations(&APIs{}, []CRD{migrated}, Upstream()); len(got) != 0 {
		t.Errorf("FindDeprecations of a migrated definition = %+v, want none", got)
	}
	migrated.StoredVersions = []string{"v1beta1", "v1"}
	got := FindDeprecations(&APIs{}, []CRD{migrated}, Upstream())
	if len(got) != 1 || got[0].GroupVersion != "example.com/v1beta1" || got[0].Served || !got[0].Stored || got[0].Replacement != "example.com/v1" {
		t.Errorf("FindDeprecations of an unmigrated definition = %+v, want example.com/v1beta1 stored and not served", got)
	}
}

func TestWrittenAt(t *testing.T) {
	m := &metav1.ObjectMeta{
		ManagedFields: []metav1.ManagedFieldsEntry{{APIVersion: "apps/v1"}, {APIVersion: "apps/v1"}, {APIVersion: "extensions/v1beta1"}},
		Annotations:   map[string]string{lastApplied: "not json"},
	}
	if got := writtenAt(m); len(got) != 2 || got[0] != "apps/v1" || got[1] != "extensions/v1beta1" {
		t.Errorf("writtenAt = %v, want each version once", got)
	}
}
