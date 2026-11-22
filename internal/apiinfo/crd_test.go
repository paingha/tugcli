package apiinfo

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	metadatafake "k8s.io/client-go/metadata/fake"
	"sigs.k8s.io/yaml"
)

// widgetsCRD reads the fixture definition as the dynamic client returns it.
func widgetsCRD(t *testing.T) *unstructured.Unstructured {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "widgets-crd.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	u := &unstructured.Unstructured{}
	if err := yaml.Unmarshal(b, &u.Object); err != nil {
		t.Fatal(err)
	}
	return u
}

func parsedWidgets(t *testing.T) *CRD {
	t.Helper()
	c, err := ParseCRD(widgetsCRD(t))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestParseCRD(t *testing.T) {
	c := parsedWidgets(t)
	if c.Name != "widgets.example.com" || c.Group != "example.com" || c.Kind != "Widget" || c.Plural != "widgets" ||
		c.Scope != "Namespaced" || !slices.Equal(c.ShortNames, []string{"wd"}) || !slices.Equal(c.Categories, []string{"all", "toys"}) {
		t.Errorf("ParseCRD names = %+v", c)
	}
	if got := c.Served(); !slices.Equal(got, []string{"v1", "v1beta1"}) {
		t.Errorf("Served() = %v, want v1 first", got)
	}
	if c.Preferred() != "v1" || c.Storage() != "v1" || !slices.Equal(c.Unmigrated(), []string{"v1beta1"}) {
		t.Errorf("Preferred() = %q, Storage() = %q, Unmigrated() = %v; want v1, v1, [v1beta1]", c.Preferred(), c.Storage(), c.Unmigrated())
	}
	want := Conversion{Strategy: "Webhook", Webhook: "widgets/widget-webhook:9443/convert", ReviewVersions: []string{"v1"}}
	if !slices.Equal(c.Conversion.ReviewVersions, want.ReviewVersions) || c.Conversion.Strategy != want.Strategy || c.Conversion.Webhook != want.Webhook {
		t.Errorf("Conversion = %+v, want %+v", c.Conversion, want)
	}
	beta, _ := c.Version("v1beta1")
	if !beta.Deprecated || !strings.Contains(beta.DeprecationWarning, "use example.com/v1") || beta.Schema == nil {
		t.Errorf("Version(v1beta1) = %+v, want deprecated with a warning", beta)
	}
	v1, _ := c.Version("v1")
	if !slices.Equal(v1.Subresources, []string{"status", "scale"}) || len(v1.PrinterColumns) != 3 || v1.PrinterColumns[2].Priority != 1 {
		t.Errorf("Version(v1) = %+v, want status and scale and three columns", v1)
	}
	if len(c.Conditions) != 2 || c.Conditions[1].Type != "Established" || c.Conditions[1].Status != "True" {
		t.Errorf("Conditions = %+v", c.Conditions)
	}
	// The JSON form leaves out the schemas, which `crd schema` shows.
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "openAPIV3Schema") || strings.Contains(string(b), "Parts the widget") {
		t.Errorf("CRD JSON carries the schema: %s", b)
	}
}

func TestConversionDefaults(t *testing.T) {
	tests := []struct{ conversion, want string }{
		{``, "None"},
		{`,"conversion":{"strategy":"None"}`, "None"},
		{`,"conversion":{"strategy":"Webhook","webhook":{"clientConfig":{"url":"https://convert.example.com/x"}}}`, "https://convert.example.com/x"},
		{`,"conversion":{"strategy":"Webhook","webhook":{"clientConfig":{"service":{"namespace":"n","name":"s"}}}}`, "n/s:443"},
	}
	for _, tt := range tests {
		c, err := parseCRDJSON([]byte(`{"metadata":{"name":"a.b"},"spec":{"group":"b"` + tt.conversion + `}}`))
		if err != nil {
			t.Fatal(err)
		}
		if got := c.Conversion.Webhook; got != tt.want && c.Conversion.Strategy != tt.want {
			t.Errorf("conversion %s = %+v, want %s", tt.conversion, c.Conversion, tt.want)
		}
	}
}

func crdClients(t *testing.T) (*dynamicfake.FakeDynamicClient, *metadatafake.FakeMetadataClient) {
	t.Helper()
	other := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition",
		"metadata": map[string]any{"name": "gadgets.example.com"},
		"spec":     map[string]any{"group": "example.com", "names": map[string]any{"kind": "Gadget", "plural": "gadgets"}, "scope": "Cluster"},
	}}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{CRDResource: "CustomResourceDefinitionList"}, widgetsCRD(t), other)
	md := newMetadataClient(
		partialMeta("apiextensions.k8s.io/v1", "CustomResourceDefinition", "", "widgets.example.com"),
		partialMeta("apiextensions.k8s.io/v1", "CustomResourceDefinition", "", "gadgets.example.com"))
	return dyn, md
}

// newMetadataClient is a fake metadata client holding objects.
func newMetadataClient(objs ...runtime.Object) *metadatafake.FakeMetadataClient {
	scheme := metadatafake.NewTestScheme()
	metav1.AddMetaToScheme(scheme)
	return metadatafake.NewSimpleMetadataClient(scheme, objs...)
}

// partialMeta is an object as the metadata client lists it.
func partialMeta(apiVersion, kind, ns, name string) *metav1.PartialObjectMetadata {
	return &metav1.PartialObjectMetadata{
		TypeMeta:   metav1.TypeMeta{APIVersion: apiVersion, Kind: kind},
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
	}
}

func TestGetAndListCRDs(t *testing.T) {
	ctx := context.Background()
	dyn, md := crdClients(t)
	c, err := ReadCRD(ctx, dyn, "widgets.example.com")
	if err != nil || c.Kind != "Widget" {
		t.Fatalf("ReadCRD(widgets.example.com) = %+v, %v", c, err)
	}
	if _, err := ReadCRD(ctx, dyn, "nothing.example.com"); !errors.Is(err, ErrNoCRD) {
		t.Errorf("ReadCRD(nothing.example.com) = %v, want ErrNoCRD", err)
	}
	all, err := ListCRDs(ctx, dyn)
	if err != nil || len(all) != 2 || all[0].Name != "gadgets.example.com" || all[1].Name != "widgets.example.com" {
		t.Errorf("ListCRDs = %+v, %v; want gadgets then widgets", all, err)
	}
	got, err := CRDNames(ctx, md)
	if err != nil || !slices.Equal(got, []string{"gadgets.example.com", "widgets.example.com"}) {
		t.Errorf("CRDNames = %v, %v", got, err)
	}
}

func TestTypeName(t *testing.T) {
	tests := []struct{ schema, want string }{
		{`{"type":"string"}`, "string"},
		{`{"type":"string","format":"date-time"}`, "string (date-time)"},
		{`{"type":"array","items":{"type":"string"}}`, "[]string"},
		{`{"type":"array","items":[{"type":"integer"}]}`, "[]integer"},
		{`{"type":"array"}`, "[]any"},
		{`{"type":"object","additionalProperties":{"type":"string"}}`, "map[string]string"},
		{`{"type":"object","additionalProperties":true}`, "map[string]any"},
		{`{"type":"object","additionalProperties":false,"properties":{"a":{}}}`, "object"},
		{`{"x-kubernetes-int-or-string":true,"anyOf":[{"type":"integer"},{"type":"string"}]}`, "int-or-string"},
		{`{"type":"object","x-kubernetes-embedded-resource":true}`, "object (a Kubernetes object)"},
		{`{"x-kubernetes-preserve-unknown-fields":true}`, "any"},
	}
	for _, tt := range tests {
		var s Schema
		if err := json.Unmarshal([]byte(tt.schema), &s); err != nil {
			t.Fatalf("%s: %v", tt.schema, err)
		}
		if got := s.TypeName(); got != tt.want {
			t.Errorf("TypeName(%s) = %q, want %q", tt.schema, got, tt.want)
		}
	}
}

// fieldLines writes fields as indented "path type required" lines.
func fieldLines(fs []Field) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		req := ""
		if f.Required {
			req = " required"
		}
		out[i] = strings.Repeat("  ", f.Depth) + f.Path + " " + f.Type + req
	}
	return out
}

func TestFields(t *testing.T) {
	v1, _ := parsedWidgets(t).Version("v1")
	tests := []struct {
		path  string
		depth int
		want  []string
	}{
		{"", 1, []string{"apiVersion string", "kind string", "metadata object", "spec object", "status object"}},
		{"spec.parts", 0, []string{
			"spec.parts []object",
			"  spec.parts.count integer",
			"  spec.parts.name string required",
		}},
		{"SPEC.Size", 0, []string{"spec.size string required"}},
		{"spec", 1, []string{
			"spec object",
			"  spec.labels map[string]string",
			"  spec.parts []object",
			"  spec.port int-or-string",
			"  spec.replicas integer (int32)",
			"  spec.size string required",
			"  spec.template object (a Kubernetes object)",
		}},
		{"status", 0, []string{
			"status object",
			"  status.conditions []object",
			"    status.conditions.status string",
			"    status.conditions.type string",
		}},
	}
	for _, tt := range tests {
		got, err := Fields(v1.Schema, tt.path, tt.depth)
		if err != nil {
			t.Fatalf("Fields(%q, %d): %v", tt.path, tt.depth, err)
		}
		if lines := fieldLines(got); !slices.Equal(lines, tt.want) {
			t.Errorf("Fields(%q, %d) =\n%s\nwant\n%s", tt.path, tt.depth, strings.Join(lines, "\n"), strings.Join(tt.want, "\n"))
		}
	}

	size, err := Fields(v1.Schema, "spec.size", 0)
	if err != nil {
		t.Fatal(err)
	}
	if f := size[0]; f.Description != "Size of the widget." || !slices.Equal(f.Enum, []string{"small", "large"}) || f.Default != "small" {
		t.Errorf("Fields(spec.size) = %+v, want its description, enum and default", f)
	}
	all, err := Fields(v1.Schema, "", 0)
	if err != nil || len(all) != 16 {
		t.Errorf("Fields of the whole schema = %d fields, %v; want 16", len(all), err)
	}
}

func TestFieldsErrors(t *testing.T) {
	c := parsedWidgets(t)
	v1, _ := c.Version("v1")
	beta, _ := c.Version("v1beta1")
	tests := []struct {
		schema *Schema
		path   string
		want   string
	}{
		{v1.Schema, "spec.sise", `no field "sise" under spec; it has labels, parts, port, replicas, size, template`},
		{v1.Schema, "specs", `no field "specs" at the top level; it has apiVersion, kind, metadata, spec, status`},
		{v1.Schema, "spec.size.unit", `no field "unit" under spec.size, which has no fields of its own`},
		{beta.Schema, "spec", `no field "spec" at the top level, which has no fields of its own`},
		{nil, "", "no schema"},
	}
	for _, tt := range tests {
		if _, err := Fields(tt.schema, tt.path, 0); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("Fields(%q) = %v, want %q", tt.path, err, tt.want)
		}
	}
}
