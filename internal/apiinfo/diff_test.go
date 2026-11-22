package apiinfo

import (
	"slices"
	"testing"
)

func TestCompare(t *testing.T) {
	res := func(group, name, kind string, versions ...string) Resource {
		return Resource{Group: group, Name: name, Kind: kind, Version: versions[0], Versions: versions}
	}
	crd := func(name, storage string, versions ...string) CRD {
		c := CRD{Name: name}
		for _, v := range versions {
			c.Versions = append(c.Versions, CRDVersion{Name: v, Served: true, Storage: v == storage})
		}
		return c
	}
	a := Snapshot{
		Cluster: "eu",
		APIs: &APIs{Server: Server{GitVersion: "v1.33.2", Minor: "1.33"}, Resources: []Resource{
			res("", "pods", "Pod", "v1"),
			res("apps", "deployments", "Deployment", "v1"),
			res("autoscaling", "horizontalpodautoscalers", "HorizontalPodAutoscaler", "v2", "v1", "v2beta2"),
			res("example.com", "gadgets", "Gadget", "v1"),
			res("example.com", "widgets", "Widget", "v1", "v1beta1"),
		}},
		CRDs: []CRD{crd("gadgets.example.com", "v1", "v1"), crd("widgets.example.com", "v1", "v1", "v1beta1")},
	}
	b := Snapshot{
		Cluster: "us",
		APIs: &APIs{Server: Server{GitVersion: "v1.35.1", Minor: "1.35"}, Resources: []Resource{
			res("", "pods", "Pod", "v1"),
			res("apps", "deployments", "Deployment", "v1"),
			res("autoscaling", "horizontalpodautoscalers", "HorizontalPodAutoscaler", "v1", "v2"),
			res("cert-manager.io", "certificates", "Certificate", "v1"),
			res("example.com", "widgets", "Widget", "v1beta1", "v1"),
		}},
		CRDs: []CRD{crd("certificates.cert-manager.io", "v1", "v1"), crd("widgets.example.com", "v1beta1", "v1", "v1beta1")},
	}

	d := Compare(a, b)
	if d.A.Cluster != "eu" || d.B.Server.Minor != "1.35" {
		t.Errorf("Compare sides = %+v, %+v", d.A, d.B)
	}
	var got []string
	for _, r := range d.Resources {
		got = append(got, r.Resource)
	}
	want := []string{"horizontalpodautoscalers.autoscaling", "certificates.cert-manager.io", "gadgets.example.com", "widgets.example.com"}
	if !slices.Equal(got, want) {
		t.Fatalf("Compare resources = %v, want %v", got, want)
	}
	hpa, certs, gadgets, widgets := d.Resources[0], d.Resources[1], d.Resources[2], d.Resources[3]
	if !slices.Equal(hpa.A.Versions, []string{"v2", "v1", "v2beta2"}) || !slices.Equal(hpa.B.Versions, []string{"v2", "v1"}) || hpa.A.CRD {
		t.Errorf("hpa = %+v, want versions in Kubernetes order and no CRD", hpa)
	}
	if len(certs.A.Versions) != 0 || !certs.B.CRD || certs.B.Storage != "v1" || certs.Kind != "Certificate" {
		t.Errorf("certificates = %+v, want only on us", certs)
	}
	if len(gadgets.B.Versions) != 0 || !gadgets.A.CRD {
		t.Errorf("gadgets = %+v, want only on eu", gadgets)
	}
	if widgets.A.Storage != "v1" || widgets.B.Storage != "v1beta1" || !slices.Equal(widgets.A.Versions, widgets.B.Versions) {
		t.Errorf("widgets = %+v, want the same versions stored differently", widgets)
	}

	// Without one side's definitions, storage versions are not compared.
	b.CRDs = nil
	for _, r := range Compare(a, b).Resources {
		if r.Resource == "widgets.example.com" {
			t.Errorf("Compare without definitions reported %+v", r)
		}
	}
	if same := Compare(a, a); len(same.Resources) != 0 {
		t.Errorf("Compare(a, a) = %+v, want no differences", same.Resources)
	}
}
