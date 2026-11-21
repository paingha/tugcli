package resolve

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func testMapper() meta.RESTMapper {
	m := meta.NewDefaultRESTMapper(nil)
	m.Add(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}, meta.RESTScopeNamespace)
	m.Add(schema.GroupVersionKind{Group: "cert-manager.io", Version: "v1", Kind: "Certificate"}, meta.RESTScopeNamespace)
	m.Add(schema.GroupVersionKind{Group: "cert-manager.io", Version: "v1", Kind: "ClusterIssuer"}, meta.RESTScopeRoot)
	m.Add(schema.GroupVersionKind{Group: "kustomize.toolkit.fluxcd.io", Version: "v1", Kind: "Kustomization"}, meta.RESTScopeNamespace)
	return m
}

func TestFindResource(t *testing.T) {
	tests := []struct {
		resource string
		want     string // group/version, resource and kind
	}{
		{"certificate", "cert-manager.io/v1, Resource=certificates Certificate"},
		{"Certificates", "cert-manager.io/v1, Resource=certificates Certificate"},
		{"certificates.cert-manager.io", "cert-manager.io/v1, Resource=certificates Certificate"},
		{"certificates.v1.cert-manager.io", "cert-manager.io/v1, Resource=certificates Certificate"},
		{"kustomization.kustomize.toolkit.fluxcd.io", "kustomize.toolkit.fluxcd.io/v1, Resource=kustomizations Kustomization"},
		{"deployments.apps", "apps/v1, Resource=deployments Deployment"},
		{"clusterissuer", "cert-manager.io/v1, Resource=clusterissuers ClusterIssuer"},
	}
	for _, tt := range tests {
		t.Run(tt.resource, func(t *testing.T) {
			m, err := FindResource(testMapper(), tt.resource)
			if err != nil {
				t.Fatalf("FindResource(%q) = %v", tt.resource, err)
			}
			if got := m.Resource.String() + " " + m.GroupVersionKind.Kind; got != tt.want {
				t.Errorf("FindResource(%q) = %s, want %s", tt.resource, got, tt.want)
			}
		})
	}

	_, err := FindResource(testMapper(), "widgets")
	if err == nil || !strings.Contains(err.Error(), `no resource type "widgets"`) || !strings.Contains(err.Error(), "tug api-resources") {
		t.Errorf("FindResource(widgets) = %v, want no resource type and a hint", err)
	}
}

func TestObjectNames(t *testing.T) {
	cert := Object{
		Resource: schema.GroupVersionResource{Group: "cert-manager.io", Version: "v1", Resource: "certificates"},
		Kind:     "Certificate", Name: "web-tls", Namespace: "shop",
	}
	if got := cert.String(); got != "certificate/web-tls" {
		t.Errorf("String() = %q, want certificate/web-tls", got)
	}
	if got := cert.Ref(); got != "certificates.cert-manager.io/web-tls" {
		t.Errorf("Ref() = %q, want certificates.cert-manager.io/web-tls", got)
	}
	if _, ok := cert.Workload(); ok {
		t.Error("a Certificate is not a workload")
	}

	pod := ObjectOf(Target{Kind: KindPod, Name: "api-0", Namespace: "shop"})
	if got := pod.Ref(); got != "pods/api-0" {
		t.Errorf("Ref() of a pod = %q, want pods/api-0", got)
	}
	for _, k := range []Kind{KindDeployment, KindStatefulSet, KindDaemonSet, KindJob, KindPod} {
		in := Target{Kind: k, Name: "x", Namespace: "shop"}
		out, ok := ObjectOf(in).Workload()
		if !ok || out.Kind != k || out.Name != "x" || out.Namespace != "shop" {
			t.Errorf("ObjectOf(%v).Workload() = %+v, %v, want the same target", in, out, ok)
		}
	}
}
