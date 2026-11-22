package diff

import (
	"context"
	"slices"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/version"
	discoveryfake "k8s.io/client-go/discovery/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	metadatafake "k8s.io/client-go/metadata/fake"
	clienttesting "k8s.io/client-go/testing"
)

var (
	deployments = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	configmaps  = schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
	secrets     = schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
)

func mapping(r schema.GroupVersionResource, kind string) *meta.RESTMapping {
	return &meta.RESTMapping{Resource: r, GroupVersionKind: r.GroupVersion().WithKind(kind), Scope: meta.RESTScopeNamespace}
}

func crd(name string, versions ...string) *unstructured.Unstructured {
	vs := make([]any, len(versions))
	for i, v := range versions {
		vs[i] = map[string]any{"name": v, "served": true}
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition",
		"metadata": map[string]any{"name": name},
		"spec":     map[string]any{"versions": vs},
	}}
}

func configMap(name string, data map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": name, "namespace": "shop", "uid": name + "-uid"},
		"data":     data,
	}}
}

func secretMeta(name string, labels map[string]string) *metav1.PartialObjectMetadata {
	return &metav1.PartialObjectMetadata{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "shop", Labels: labels},
	}
}

// fakeCluster is a cluster of fake clients. Its dynamic client refuses to
// serve Secrets, so a test fails if their data is ever asked for.
func fakeCluster(t *testing.T, name, gitVersion string, groups []string, objs []runtime.Object, secretObjs ...runtime.Object) Cluster {
	t.Helper()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		crdResource: "CustomResourceDefinitionList",
		deployments: "DeploymentList",
		configmaps:  "ConfigMapList",
		secrets:     "SecretList",
	}, objs...)
	dyn.PrependReactor("*", "secrets", func(clienttesting.Action) (bool, runtime.Object, error) {
		t.Errorf("%s: the dynamic client was asked for Secrets, which carry their data", name)
		return true, nil, nil
	})
	scheme := metadatafake.NewTestScheme()
	if err := metav1.AddMetaToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	md := metadatafake.NewSimpleMetadataClient(scheme, secretObjs...)
	var resources []*metav1.APIResourceList
	for _, gv := range groups {
		resources = append(resources, &metav1.APIResourceList{GroupVersion: gv})
	}
	disc := &discoveryfake.FakeDiscovery{Fake: &clienttesting.Fake{Resources: resources}, FakedServerVersion: &version.Info{GitVersion: gitVersion}}
	return Cluster{Name: name, Clients: Clients{Dynamic: dyn, Metadata: md}, Discovery: disc}
}

func TestCompareClusters(t *testing.T) {
	prod := fakeCluster(t, "prod", "v1.35.1", []string{"v1", "apps/v1", "db.example.com/v1", "db.example.com/v2"},
		[]runtime.Object{
			crd("databases.db.example.com", "v1", "v2"),
			crd("backups.db.example.com", "v1"),
			configMap("flags", map[string]any{"checkout": "on"}),
			configMap("same", map[string]any{"a": "b"}),
			configMap("prod-only", nil),
		},
		secretMeta("api-key", map[string]string{"rotated": "2026-11"}),
		secretMeta("tls", nil))
	staging := fakeCluster(t, "staging", "v1.34.2", []string{"v1", "apps/v1", "db.example.com/v1", "batch/v1"},
		[]runtime.Object{
			crd("databases.db.example.com", "v1"),
			crd("backups.db.example.com", "v1"),
			configMap("flags", map[string]any{"checkout": "off"}),
			configMap("same", map[string]any{"a": "b"}),
			configMap("staging-only", nil),
		},
		secretMeta("api-key", map[string]string{"rotated": "2026-10"}),
		secretMeta("tls", nil))

	d, err := CompareClusters(context.Background(), prod, staging, DriftOptions{
		Resources: []*meta.RESTMapping{mapping(configmaps, "ConfigMap"), mapping(secrets, "Secret"), mapping(deployments, "Deployment")},
		Namespace: "shop",
	})
	if err != nil {
		t.Fatal(err)
	}
	if d.Same || d.Here.Version != "v1.35.1" || d.There.Version != "v1.34.2" {
		t.Errorf("Same = %v, versions %s and %s", d.Same, d.Here.Version, d.There.Version)
	}
	if !slices.Equal(d.APIVersions.OnlyHere, []string{"db.example.com/v2"}) || !slices.Equal(d.APIVersions.OnlyThere, []string{"batch/v1"}) {
		t.Errorf("APIVersions = %+v", d.APIVersions)
	}
	if len(d.CRDs) != 1 || d.CRDs[0].Name != "databases.db.example.com" ||
		!slices.Equal(d.CRDs[0].HereVersions, []string{"v1", "v2"}) || !slices.Equal(d.CRDs[0].ThereVersions, []string{"v1"}) {
		t.Errorf("CRDs = %+v, want databases with v1, v2 here and v1 there", d.CRDs)
	}
	var got []string
	for _, o := range d.Objects {
		s := o.Kind + "/" + o.Name + " " + o.State
		for _, c := range o.Changes {
			s += " " + c.Path.String()
		}
		got = append(got, s)
	}
	want := []string{
		"configmap/flags differs data.checkout",
		"configmap/prod-only only-here",
		"configmap/staging-only only-there",
		"secret/api-key differs metadata.labels.rotated",
	}
	if !slices.Equal(got, want) {
		t.Errorf("Objects =\n%q\nwant\n%q", got, want)
	}
	if d.Compared != 6 || !slices.Equal(d.Resources, []string{"configmaps", "secrets", "deployments.apps"}) {
		t.Errorf("Compared = %d, Resources = %q", d.Compared, d.Resources)
	}

	same, err := CompareClusters(context.Background(), prod, prod, DriftOptions{Resources: []*meta.RESTMapping{mapping(configmaps, "ConfigMap")}, Namespace: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	if !same.Same || len(same.Objects) != 0 || same.Compared != 3 {
		t.Errorf("a cluster against itself: %+v", same)
	}
}
