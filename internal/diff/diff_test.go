package diff

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/yaml"
)

// fixture reads an object from testdata.
func fixture(t *testing.T, name string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	if err := yaml.Unmarshal(b, &obj); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return obj
}

// paths writes each change as its op and path, for comparing in tests.
func paths(changes []Change) []string {
	out := make([]string, len(changes))
	for i, c := range changes {
		out[i] = string(c.Op) + " " + c.Path.String()
	}
	return out
}

// web is the path of the storefront's container.
var web = Path{Field("spec"), Field("template"), Field("spec"), Field("containers"), Named("web")}

func at(p Path, steps ...Elem) Path {
	for _, s := range steps {
		p = p.child(s)
	}
	return p
}

func TestNormalise(t *testing.T) {
	live := fixture(t, "storefront-prod.yaml")
	got := Normalise(live, NormaliseOptions{})
	md := got["metadata"].(map[string]any)
	for _, f := range []string{"uid", "resourceVersion", "generation", "creationTimestamp", "managedFields"} {
		if _, ok := md[f]; ok {
			t.Errorf("Normalise kept metadata.%s", f)
		}
	}
	if _, ok := got["status"]; ok {
		t.Error("Normalise kept status without NormaliseOptions.Status")
	}
	ann := md["annotations"].(map[string]any)
	if _, ok := ann[lastApplied]; ok {
		t.Error("Normalise kept the last-applied annotation")
	}
	if ann["deployment.kubernetes.io/revision"] != "7" || md["name"] != "storefront" {
		t.Errorf("Normalise dropped what it should keep: %v", md)
	}
	// The original is untouched.
	if _, ok := live["metadata"].(map[string]any)["managedFields"]; !ok {
		t.Error("Normalise changed its input")
	}
	if _, ok := Normalise(live, NormaliseOptions{Status: true})["status"]; !ok {
		t.Error("Normalise dropped status with NormaliseOptions.Status")
	}
	if Normalise(nil, NormaliseOptions{}) != nil {
		t.Error("Normalise(nil) is not nil")
	}

	// Across clusters, owner UIDs go; an annotation map left empty goes.
	owned := map[string]any{"metadata": map[string]any{
		"name":            "x",
		"annotations":     map[string]any{lastApplied: "{}"},
		"ownerReferences": []any{map[string]any{"kind": "Database", "name": "orders", "uid": "1234"}},
	}}
	got = Normalise(owned, NormaliseOptions{AcrossClusters: true})
	md = got["metadata"].(map[string]any)
	if _, ok := md["annotations"]; ok {
		t.Errorf("Normalise kept an empty annotations map: %v", md)
	}
	if ref := md["ownerReferences"].([]any)[0].(map[string]any); ref["uid"] != nil || ref["name"] != "orders" {
		t.Errorf("Normalise across clusters: ownerReference %v, want no uid", ref)
	}
}

func TestCompare(t *testing.T) {
	obj := func(spec string) map[string]any {
		var m map[string]any
		if err := yaml.Unmarshal([]byte(spec), &m); err != nil {
			t.Fatal(err)
		}
		return Normalise(m, NormaliseOptions{})
	}
	for _, tt := range []struct {
		name     string
		from, to string
		want     []string
	}{
		{"same", "a: 1\nb: [x]", "b: [x]\na: 1.0", nil},
		{"scalar", "a: 1", "a: 2", []string{"change a"}},
		{"map keys", "m: {x: 1, y: 2}", "m: {y: 2, z: 3}", []string{"remove m.x", "add m.z"}},
		{"type change", "a: {x: 1}", "a: [1]", []string{"change a"}},
		{
			"named items match by name whatever their order",
			"c: [{name: a, image: one}, {name: b, image: two}]",
			"c: [{name: b, image: three}, {name: a, image: one}, {name: d}]",
			[]string{`change c[name="b"].image`, `add c[name="d"]`},
		},
		{"positional lists", "args: [a, b, c]", "args: [a, x]", []string{"change args[1]", "remove args[2]"}},
		{"duplicate names go by position", "c: [{name: a}, {name: a}]", "c: [{name: a}, {name: b}]", []string{"change c[1].name"}},
		{"first container", "c: []", "c: [{name: a}]", []string{`add c[name="a"]`}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := paths(Compare(obj(tt.from), obj(tt.to))); !slices.Equal(got, tt.want) {
				t.Errorf("Compare(%q, %q) = %q, want %q", tt.from, tt.to, got, tt.want)
			}
		})
	}
	if got := Compare(nil, obj("a: 1")); len(got) != 1 || got[0].Op != OpAdd || len(got[0].Path) != 0 {
		t.Errorf("Compare(nil, obj) = %+v, want the whole object added", got)
	}
	if got := Compare(obj("a: 1"), nil); len(got) != 1 || got[0].Op != OpRemove {
		t.Errorf("Compare(obj, nil) = %+v, want the whole object removed", got)
	}
}

func TestPathString(t *testing.T) {
	p := at(web, Field("env"), Named(`LOG"LEVEL`), Field("value"))
	if got, want := p.String(), `spec.template.spec.containers[name="web"].env[name="LOG\"LEVEL"].value`; got != want {
		t.Errorf("String() = %s, want %s", got, want)
	}
	if got := at(nil, Field("args"), Index(2)).String(); got != "args[2]" {
		t.Errorf("String() = %s, want args[2]", got)
	}

	// JSON output reads back to a path that writes the same.
	for _, s := range []string{
		p.String(),
		`spec.ports[0].port`,
		`metadata.annotations.deployment.kubernetes.io/revision`,
		`c[name="a]b"].x`,
		``,
	} {
		var back Path
		if err := back.UnmarshalText([]byte(s)); err != nil {
			t.Errorf("UnmarshalText(%s): %v", s, err)
			continue
		}
		if back.String() != s {
			t.Errorf("UnmarshalText(%s) writes %s", s, back)
		}
	}
	steps := Path{Field("a"), Named("b"), Index(1)}
	var back Path
	if err := back.UnmarshalText([]byte(steps.String())); err != nil || !slices.Equal(back, steps) {
		t.Errorf("UnmarshalText(%s) = %#v, %v; want the same steps", steps, back, err)
	}
	for _, bad := range []string{"a[", "a[x]", `a[name="b"`, `a[name=b]`} {
		var back Path
		if err := back.UnmarshalText([]byte(bad)); err == nil {
			t.Errorf("UnmarshalText(%s) = %s, want an error", bad, back)
		}
	}
}

func TestManagers(t *testing.T) {
	mgrs, err := Managers(fixture(t, "storefront-prod.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, m := range mgrs {
		names = append(names, m.Manager+"/"+m.Operation+"/"+m.Subresource)
	}
	want := []string{"kubectl/Apply/", "kube-controller-manager/Update/scale", "kube-controller-manager/Update/",
		"kube-controller-manager/Update/status", "kubectl-edit/Update/"}
	if !slices.Equal(names, want) {
		t.Fatalf("Managers = %q, want %q", names, want)
	}
	if got := mgrs[0].Time.Format("2006-01-02T15:04"); got != "2026-11-20T10:00" {
		t.Errorf("kubectl's time = %s", got)
	}
	wantFields := []string{
		"metadata.labels.app",
		"metadata.labels.team",
		"spec.selector",
		"spec.template.metadata.labels.app",
		`spec.template.spec.containers[name="web"].args`,
		`spec.template.spec.containers[name="web"].image`,
		`spec.template.spec.containers[name="web"].name`,
		`spec.template.spec.containers[name="web"].ports[containerPort=8080,protocol="TCP"].containerPort`,
		`spec.template.spec.containers[name="web"].ports[containerPort=8080,protocol="TCP"].protocol`,
	}
	if !slices.Equal(mgrs[0].Fields, wantFields) {
		t.Errorf("kubectl's fields =\n%s\nwant\n%s", strings.Join(mgrs[0].Fields, "\n"), strings.Join(wantFields, "\n"))
	}
	if got, want := mgrs[4].Fields, []string{
		`metadata.finalizers[="example.com/protect"]`,
		`spec.template.spec.containers[name="web"].env[name="LOG_LEVEL"].name`,
		`spec.template.spec.containers[name="web"].env[name="LOG_LEVEL"].value`,
	}; !slices.Equal(got, want) {
		t.Errorf("kubectl-edit's fields = %q, want %q", got, want)
	}

	if _, err := Managers(map[string]any{"metadata": map[string]any{"managedFields": []any{"nonsense"}}}); err == nil {
		t.Error("Managers accepted an entry that is not an object")
	}
}

func TestOwners(t *testing.T) {
	live := fixture(t, "storefront-prod.yaml")
	for _, tt := range []struct {
		path Path
		want []string
	}{
		{Path{Field("spec"), Field("replicas")}, []string{"kube-controller-manager/scale"}},
		{Path{Field("metadata"), Field("labels"), Field("app")}, []string{"kubectl"}},
		{Path{Field("metadata"), Field("annotations"), Field("deployment.kubernetes.io/revision")}, []string{"kube-controller-manager"}},
		// args is atomic: owning the list owns every item.
		{at(web, Field("args"), Index(1)), []string{"kubectl"}},
		// ports are keyed by containerPort and protocol.
		{at(web, Field("ports"), Index(0), Field("containerPort")), []string{"kubectl"}},
		{at(web, Field("env"), Named("LOG_LEVEL"), Field("value")), []string{"kubectl-edit"}},
		// A container is owned by everyone who owns a field of it.
		{web, []string{"kubectl", "kubectl-edit"}},
		// finalizers are a set, owned by value.
		{Path{Field("metadata"), Field("finalizers"), Index(0)}, []string{"kubectl-edit"}},
		// Defaulted by the API server: nobody owns them.
		{at(web, Field("imagePullPolicy")), nil},
		{Path{Field("spec"), Field("template"), Field("spec"), Field("restartPolicy")}, nil},
		{at(web, Field("env"), Named("NOT_THERE")), nil},
		{Path{Field("status"), Field("replicas")}, []string{"kube-controller-manager/status"}},
	} {
		var got []string
		for _, o := range Owners(live, tt.path) {
			s := o.Manager
			if o.Subresource != "" {
				s += "/" + o.Subresource
			}
			got = append(got, s)
		}
		if !slices.Equal(got, tt.want) {
			t.Errorf("Owners(%s) = %q, want %q", tt.path, got, tt.want)
		}
	}

	// Items of a list kept by position are owned through i: keys.
	byIndex := map[string]any{
		"metadata": map[string]any{"managedFields": []any{map[string]any{
			"manager": "ops", "operation": "Update", "fieldsType": "FieldsV1",
			"fieldsV1": map[string]any{"f:spec": map[string]any{"f:steps": map[string]any{"i:1": map[string]any{"f:run": map[string]any{}}}}},
		}}},
		"spec": map[string]any{"steps": []any{map[string]any{"run": "a"}, map[string]any{"run": "b"}}},
	}
	if got := Owners(byIndex, Path{Field("spec"), Field("steps"), Index(1), Field("run")}); len(got) != 1 || got[0].Manager != "ops" {
		t.Errorf("Owners(spec.steps[1].run) = %v, want ops", got)
	}
	if got := Owners(byIndex, Path{Field("spec"), Field("steps"), Index(0), Field("run")}); got != nil {
		t.Errorf("Owners(spec.steps[0].run) = %v, want none", got)
	}
}

func TestAgainstLeavesOutWhatClustersSet(t *testing.T) {
	prod, staging := fixture(t, "storefront-prod.yaml"), fixture(t, "storefront-staging.yaml")
	r, err := compareSides(sides{from: prod, to: staging, acrossClusters: true, text: true}, Options{FieldManager: DefaultFieldManager})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"remove metadata.finalizers",
		`remove spec.template.spec.containers[name="web"].env`,
		`change spec.template.spec.containers[name="web"].image`,
	}
	if got := paths(r.Changes); !slices.Equal(got, want) {
		t.Fatalf("changes = %q, want %q", got, want)
	}
	// replicas and the revision are the clusters' controllers'; the
	// terminationMessagePolicy is a default only prod's API server sets.
	if r.Omitted != 3 || r.Same {
		t.Errorf("Omitted = %d, Same = %v; want 3 and false", r.Omitted, r.Same)
	}
	image := r.Changes[2]
	if names := managerNames(image.FromOwners); !slices.Equal(names, []string{"kubectl"}) || managerNames(image.ToOwners)[0] != "kubectl" {
		t.Errorf("image owners = %v here, %v there; want kubectl on both", image.FromOwners, image.ToOwners)
	}
	if !slices.Equal(image.Conflicts, []string{"kubectl"}) {
		t.Errorf("image conflicts = %q, want kubectl, who owns it here", image.Conflicts)
	}
	if env := r.Changes[1]; managerNames(env.FromOwners)[0] != "kubectl-edit" || env.Conflicts != nil {
		t.Errorf("env: owners %v, conflicts %q; want kubectl-edit and none, since a field left out is not taken away", env.FromOwners, env.Conflicts)
	}
	text := FormatUnified("prod", "staging", r.Hunks)
	for _, gone := range []string{"replicas", "revision", "terminationMessagePolicy", "managedFields", "resourceVersion"} {
		if strings.Contains(text, gone) {
			t.Errorf("the diff shows %s, which was left out:\n%s", gone, text)
		}
	}
	for _, line := range []string{"-  finalizers:", "-        image: registry.example.com/storefront:1.4.2", "+        image: registry.example.com/storefront:1.5.0", "-        - name: LOG_LEVEL"} {
		if !strings.Contains(text, "\n"+line+"\n") {
			t.Errorf("the diff lacks %q:\n%s", line, text)
		}
	}

	// --all-fields keeps them.
	all, err := compareSides(sides{from: prod, to: staging, acrossClusters: true}, Options{AllFields: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(all.Changes) != 6 || all.Omitted != 0 {
		t.Errorf("with AllFields: %q, %d omitted; want 6 changes", paths(all.Changes), all.Omitted)
	}

	// Without managedFields, nothing is known, so nothing is left out.
	bare := Normalise(staging, NormaliseOptions{Status: true})
	r, err = compareSides(sides{from: prod, to: bare, acrossClusters: true}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if r.Omitted != 1 { // only the terminationMessagePolicy, which prod's side tells about
		t.Errorf("against an object without managedFields: %d omitted (%q kept), want 1", r.Omitted, paths(r.Changes))
	}
}

func TestFileOrigins(t *testing.T) {
	live, applied, manifest := fixture(t, "storefront-prod.yaml"), fixture(t, "storefront-applied.yaml"), fixture(t, "storefront-manifest.yaml")
	r, err := compareSides(sides{from: live, to: applied, manifest: manifest, text: true}, Options{FieldManager: "kubectl"})
	if err != nil {
		t.Fatal(err)
	}
	r.Conflicts = []Conflict{{Manager: "kube-controller-manager", Field: "spec.replicas"}}
	markConflicts(r)
	type change struct{ path, origin, conflicts string }
	var got []change
	for _, c := range r.Changes {
		got = append(got, change{c.Path.String(), c.Origin, strings.Join(c.Conflicts, ",")})
	}
	want := []change{
		{"metadata.labels.policy.example.com/scanned", OriginAdmission, ""},
		{"spec.replicas", OriginManifest, "kube-controller-manager"},
		{"spec.revisionHistoryLimit", OriginDefault, ""},
		{`spec.template.spec.containers[name="web"].image`, OriginManifest, ""},
	}
	if !slices.Equal(got, want) {
		t.Errorf("changes =\n%v\nwant\n%v", got, want)
	}
	if r.Same || len(r.Hunks) == 0 {
		t.Errorf("Same = %v with %d hunks, want a difference", r.Same, len(r.Hunks))
	}

	// A field the manifest no longer sets, and the applier owned, is taken
	// out by the apply.
	c := Change{Op: OpRemove, Path: Path{Field("spec"), Field("paused")}, FromOwners: []Owner{{Manager: "kubectl"}}}
	if got := origin(c, manifest, "kubectl"); got != OriginManifest {
		t.Errorf("origin of a field the manifest dropped = %q, want %q", got, OriginManifest)
	}

	// A new object is all the manifest's.
	r, err = compareSides(sides{to: applied, manifest: manifest, text: true}, Options{FieldManager: "kubectl"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Missing != "from" || len(r.Changes) != 1 || r.Changes[0].Origin != OriginManifest || r.Hunks[0].FromLine != 0 {
		t.Errorf("new object: missing %q, changes %+v, hunks %+v", r.Missing, paths(r.Changes), r.Hunks)
	}
}

func TestUnified(t *testing.T) {
	lines := func(s string) []string { return strings.Split(s, "\n") }
	for _, tt := range []struct {
		name, from, to, want string
	}{
		{"same", "a\nb", "a\nb", ""},
		{
			"one change in the middle",
			"1\n2\n3\n4\n5\n6\n7\n8\n9", "1\n2\n3\n4\nfive\n6\n7\n8\n9",
			"--- a\n+++ b\n@@ -2,7 +2,7 @@\n 2\n 3\n 4\n-5\n+five\n 6\n 7\n 8\n",
		},
		{
			"changes far apart make two hunks",
			"a\n1\n2\n3\n4\n5\n6\n7\n8\nb", "A\n1\n2\n3\n4\n5\n6\n7\n8\nB",
			"--- a\n+++ b\n@@ -1,4 +1,4 @@\n-a\n+A\n 1\n 2\n 3\n@@ -7,4 +7,4 @@\n 6\n 7\n 8\n-b\n+B\n",
		},
		{"new", "", "x\ny", "--- a\n+++ b\n@@ -0,0 +1,2 @@\n+x\n+y\n"},
		{"insert", "a\nc", "a\nb\nc", "--- a\n+++ b\n@@ -1,2 +1,3 @@\n a\n+b\n c\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			from := lines(tt.from)
			if tt.from == "" {
				from = nil
			}
			if got := FormatUnified("a", "b", Unified(from, lines(tt.to), 3)); got != tt.want {
				t.Errorf("Unified(%q, %q) =\n%s\nwant\n%s", tt.from, tt.to, got, tt.want)
			}
		})
	}
}

func TestConflictsOf(t *testing.T) {
	err := apierrors.NewApplyConflict([]metav1.StatusCause{
		{Type: metav1.CauseTypeFieldManagerConflict, Message: `conflict with "helm" using apps/v1`, Field: ".spec.replicas"},
		{Type: metav1.CauseTypeFieldManagerConflict, Message: `conflict with "kube-controller-manager" with subresource "scale" using apps/v1`, Field: `.spec.template.spec.containers[name="web"].image`},
		{Type: metav1.CauseTypeFieldValueInvalid, Message: "not a conflict"},
	}, "Apply failed with 2 conflicts")
	got := conflictsOf(err)
	want := []Conflict{
		{Manager: "helm", Field: "spec.replicas", Message: `conflict with "helm" using apps/v1`},
		{Manager: "kube-controller-manager", Field: `spec.template.spec.containers[name="web"].image`, Message: `conflict with "kube-controller-manager" with subresource "scale" using apps/v1`},
	}
	if !slices.Equal(got, want) {
		t.Errorf("conflictsOf = %+v, want %+v", got, want)
	}
	if got := conflictsOf(errors.New("plain")); got != nil {
		t.Errorf("conflictsOf(plain error) = %+v", got)
	}
}

func TestManifests(t *testing.T) {
	in := `---
apiVersion: v1
kind: ConfigMap
metadata: {name: one}
---
# only a comment
---
{"apiVersion": "v1", "kind": "List", "items": [
  {"apiVersion": "v1", "kind": "Service", "metadata": {"name": "two"}},
  {"apiVersion": "apps/v1", "kind": "Deployment", "metadata": {"name": "three", "namespace": "shop"}}]}
`
	objs, err := Manifests(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, o := range objs {
		got = append(got, o["kind"].(string)+"/"+o["metadata"].(map[string]any)["name"].(string))
	}
	if want := []string{"ConfigMap/one", "Service/two", "Deployment/three"}; !slices.Equal(got, want) {
		t.Errorf("Manifests = %q, want %q", got, want)
	}

	for _, tt := range []struct{ in, want string }{
		{"kind: ConfigMap\nmetadata: {name: x}", "no apiVersion"},
		{"apiVersion: v1\nmetadata: {name: x}", "no kind"},
		{"apiVersion: v1\nkind: Pod\nmetadata: {generateName: x-}", "only a generateName"},
		{"apiVersion: v1\nkind: Pod\nmetadata: {}", "a Pod has no name"},
		{"a: [", "document 1"},
	} {
		if _, err := Manifests(strings.NewReader(tt.in)); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("Manifests(%q) = %v, want an error with %q", tt.in, err, tt.want)
		}
	}
}

func TestReadFiles(t *testing.T) {
	dir := t.TempDir()
	write := func(name, s string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("b.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: b}\n")
	write("a.json", `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"a"}}`)
	write("notes.txt", "not a manifest")
	got, err := ReadFiles([]string{dir, "-"}, strings.NewReader("apiVersion: v1\nkind: Secret\nmetadata: {name: c}\n"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, m := range got {
		names = append(names, filepath.Base(m.Source)+":"+m.Object["metadata"].(map[string]any)["name"].(string))
	}
	if want := []string{"a.json:a", "b.yaml:b", "-:c"}; !slices.Equal(names, want) {
		t.Errorf("ReadFiles = %q, want %q", names, want)
	}
	if _, err := ReadFiles([]string{filepath.Join(dir, "missing.yaml")}, nil); err == nil {
		t.Error("ReadFiles of a missing file succeeded")
	}
}

func TestIsSecret(t *testing.T) {
	if !IsSecret(schema.GroupVersionResource{Version: "v1", Resource: "secrets"}) {
		t.Error("IsSecret(secrets) = false")
	}
	if IsSecret(schema.GroupVersionResource{Group: "example.com", Version: "v1", Resource: "secrets"}) {
		t.Error("IsSecret(secrets.example.com) = true")
	}
}
