package policy

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
)

// rule is what one type of check checks.
type rule interface {
	// validate checks the rule's settings.
	validate() error
	// scope is which kinds the rule can check.
	scope() kindScope
	// defaultKinds are the kinds it checks when the check names none; nil
	// when it must name them.
	defaultKinds() []kind
	// check returns what is wrong with o, or nothing.
	check(o *object, e *env) []problem
}

// env is what rules may look at besides the object.
type env struct {
	budgets []policyv1.PodDisruptionBudget
	// budgetsUnknown is set when the budgets could not be listed, so that
	// a missing one proves nothing.
	budgetsUnknown bool
}

// problem is one thing wrong with an object.
type problem struct {
	// container names the container it is about, if it is about one.
	container string
	message   string
}

// templateProblem is what a rule that reads pod templates reports for an
// object whose template it cannot read.
func templateProblem(o *object) []problem {
	return []problem{{message: "cannot be checked: " + o.templateErr.Error()}}
}

// keysRule wants label or annotation keys to have values.
type keysRule struct {
	what string // label or annotation
	keys []string
}

func (r keysRule) validate() error      { return validateKeys("required "+r.what, r.keys) }
func (r keysRule) scope() kindScope     { return anyKind }
func (r keysRule) defaultKinds() []kind { return workloadKinds }

func (r keysRule) check(o *object, _ *env) []problem {
	have := o.u.GetLabels()
	if r.what == "annotation" {
		have = o.u.GetAnnotations()
	}
	var missing []string
	for _, k := range r.keys {
		if have[k] == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return []problem{{message: fmt.Sprintf("has no %s %s", joinWords(missing, "or"), r.what)}}
}

// registryRule limits where containers' images come from.
type registryRule struct{ *RegistryRule }

func (r registryRule) validate() error {
	if len(r.Allow) == 0 {
		return errors.New("imageRegistries.allow must list at least one registry")
	}
	var errs []error
	for _, a := range r.Allow {
		if a == "" || strings.Contains(a, "://") || strings.ContainsAny(a, "@ ") {
			errs = append(errs, fmt.Errorf("imageRegistries.allow entry %q must be a registry, or a path in one, without a scheme, such as ghcr.io/acme", a))
		}
	}
	return errors.Join(errs...)
}

func (r registryRule) scope() kindScope     { return podKinds }
func (r registryRule) defaultKinds() []kind { return workloadKinds }

func (r registryRule) check(o *object, _ *env) []problem {
	if o.templateErr != nil {
		return templateProblem(o)
	}
	allowed := make([]string, len(r.Allow))
	for i, a := range r.Allow {
		allowed[i] = normaliseRegistry(a)
	}
	var out []problem
	spec := o.template.Spec
	for _, c := range slices.Concat(spec.InitContainers, spec.Containers) {
		repo := imageRepository(c.Image)
		if slices.ContainsFunc(allowed, func(a string) bool { return repo == a || strings.HasPrefix(repo, a+"/") }) {
			continue
		}
		out = append(out, problem{container: c.Name, message: fmt.Sprintf("runs %s, from %s, which is not one of the allowed registries: %s",
			c.Image, registryOf(repo), strings.Join(r.Allow, ", "))})
	}
	return out
}

// imageRepository returns where an image comes from, without its tag or
// digest, as the container runtime reads it: nginx:1.27 is
// docker.io/library/nginx, and acme/api is docker.io/acme/api.
func imageRepository(image string) string {
	image, _, _ = strings.Cut(strings.ToLower(image), "@")
	host, rest, ok := strings.Cut(image, "/")
	if !ok || (!strings.ContainsAny(host, ".:") && host != "localhost") {
		host, rest = "docker.io", image
		if !strings.Contains(rest, "/") {
			rest = "library/" + rest
		}
	}
	if i := strings.LastIndexByte(rest, ':'); i > strings.LastIndexByte(rest, '/') {
		rest = rest[:i]
	}
	return normaliseRegistry(host) + "/" + rest
}

// normaliseRegistry writes Docker Hub's other names as docker.io, in lower
// case, without a trailing slash.
func normaliseRegistry(s string) string {
	s = strings.TrimSuffix(strings.ToLower(s), "/")
	for _, alias := range []string{"index.docker.io", "registry-1.docker.io"} {
		if s == alias || strings.HasPrefix(s, alias+"/") {
			return "docker.io" + strings.TrimPrefix(s, alias)
		}
	}
	return s
}

func registryOf(repo string) string {
	host, _, _ := strings.Cut(repo, "/")
	return host
}

// resourcesRule wants containers to set requests and limits.
type resourcesRule struct{ *ResourcesRule }

func (r resourcesRule) validate() error {
	if len(r.Requests) == 0 && len(r.Limits) == 0 {
		return errors.New("resources must list requests, limits or both, such as requests: [cpu, memory]")
	}
	if slices.Contains(r.Requests, "") || slices.Contains(r.Limits, "") {
		return errors.New("resources lists an empty resource name")
	}
	return nil
}

func (r resourcesRule) scope() kindScope     { return podKinds }
func (r resourcesRule) defaultKinds() []kind { return workloadKinds }

func (r resourcesRule) check(o *object, _ *env) []problem {
	if o.templateErr != nil {
		return templateProblem(o)
	}
	var out []problem
	for _, c := range o.template.Spec.Containers {
		var requests, limits []string
		for _, name := range r.Requests {
			// Kubernetes takes a request from the limit when only that is set.
			_, req := c.Resources.Requests[corev1.ResourceName(name)]
			_, lim := c.Resources.Limits[corev1.ResourceName(name)]
			if !req && !lim {
				requests = append(requests, name)
			}
		}
		for _, name := range r.Limits {
			if _, ok := c.Resources.Limits[corev1.ResourceName(name)]; !ok {
				limits = append(limits, name)
			}
		}
		var missing []string
		if len(requests) > 0 {
			missing = append(missing, joinWords(requests, "or")+" request")
		}
		if len(limits) > 0 {
			missing = append(missing, joinWords(limits, "or")+" limit")
		}
		if len(missing) > 0 {
			out = append(out, problem{container: c.Name, message: "sets no " + strings.Join(missing, " and no ")})
		}
	}
	return out
}

// probesRule wants containers to have probes.
type probesRule []string

func (r probesRule) validate() error {
	for _, p := range r {
		switch p {
		case "readiness", "liveness", "startup":
		default:
			return fmt.Errorf("probes lists %q; the probes are readiness, liveness and startup", p)
		}
	}
	return nil
}

func (r probesRule) scope() kindScope     { return podKinds }
func (r probesRule) defaultKinds() []kind { return serviceKinds }

func (r probesRule) check(o *object, _ *env) []problem {
	if o.templateErr != nil {
		return templateProblem(o)
	}
	var out []problem
	for _, c := range o.template.Spec.Containers {
		var missing []string
		for _, p := range r {
			has := map[string]bool{
				"readiness": c.ReadinessProbe != nil,
				"liveness":  c.LivenessProbe != nil,
				"startup":   c.StartupProbe != nil,
			}[p]
			if !has {
				missing = append(missing, p)
			}
		}
		if len(missing) > 0 {
			out = append(out, problem{container: c.Name, message: "has no " + joinWords(missing, "or") + " probe"})
		}
	}
	return out
}

// budgetRule wants a PodDisruptionBudget to select a workload's pods.
type budgetRule struct{ *BudgetRule }

func (r budgetRule) validate() error {
	if r.MinReplicas < 0 {
		return errors.New("podDisruptionBudget.minReplicas cannot be negative")
	}
	return nil
}

func (r budgetRule) scope() kindScope     { return replicatedKinds }
func (r budgetRule) defaultKinds() []kind { return scaledKinds }

func (r budgetRule) check(o *object, e *env) []problem {
	if o.templateErr != nil {
		return templateProblem(o)
	}
	if o.replicas < max(r.MinReplicas, 1) || e.budgetsUnknown {
		return nil
	}
	pods := labels.Set(o.template.Labels)
	for _, b := range e.budgets {
		if b.Namespace != o.u.GetNamespace() {
			continue
		}
		// A budget without a selector selects nothing; an empty one, every
		// pod in its namespace.
		sel, err := metav1.LabelSelectorAsSelector(b.Spec.Selector)
		if err == nil && sel.Matches(pods) {
			return nil
		}
	}
	if o.replicas == 1 {
		return []problem{{message: "no PodDisruptionBudget selects its pods, so a node drain can evict its only replica"}}
	}
	return []problem{{message: fmt.Sprintf("no PodDisruptionBudget selects its pods, so a node drain can evict all %d replicas at once", o.replicas)}}
}

// securityRule wants containers to run with a securityContext.
type securityRule struct{ *SecurityContextRule }

func (r securityRule) validate() error {
	if r.RunAsNonRoot == nil && r.ReadOnlyRootFilesystem == nil && r.Privileged == nil {
		return errors.New("securityContext must set runAsNonRoot, readOnlyRootFilesystem or privileged")
	}
	return nil
}

func (r securityRule) scope() kindScope     { return podKinds }
func (r securityRule) defaultKinds() []kind { return workloadKinds }

func (r securityRule) check(o *object, _ *env) []problem {
	if o.templateErr != nil {
		return templateProblem(o)
	}
	spec := o.template.Spec
	var podNonRoot *bool
	if spec.SecurityContext != nil {
		podNonRoot = spec.SecurityContext.RunAsNonRoot
	}
	var out []problem
	for _, c := range slices.Concat(spec.InitContainers, spec.Containers) {
		sc := c.SecurityContext
		if sc == nil {
			sc = &corev1.SecurityContext{}
		}
		var wrong []string
		if want := r.RunAsNonRoot; want != nil {
			got, who := podNonRoot, "its pod sets"
			if sc.RunAsNonRoot != nil {
				got, who = sc.RunAsNonRoot, "it sets"
			}
			if value(got) != *want {
				why := "may run as root: neither it nor its pod sets runAsNonRoot: true"
				if got != nil {
					why = "may run as root: " + who + " runAsNonRoot: false"
				}
				wrong = append(wrong, securityMessage("runAsNonRoot", *want, why))
			}
		}
		if want := r.ReadOnlyRootFilesystem; want != nil && value(sc.ReadOnlyRootFilesystem) != *want {
			wrong = append(wrong, securityMessage("readOnlyRootFilesystem", *want,
				"can write to its root filesystem: readOnlyRootFilesystem is not true"))
		}
		if want := r.Privileged; want != nil && value(sc.Privileged) != *want {
			msg := "is not privileged"
			if !*want {
				msg = "runs privileged, with root's access to its node"
			}
			wrong = append(wrong, msg)
		}
		if len(wrong) > 0 {
			out = append(out, problem{container: c.Name, message: strings.Join(wrong, "; ")})
		}
	}
	return out
}

// securityMessage says what is wrong with a setting that should be true,
// or says it is set when it should not be.
func securityMessage(field string, want bool, ifWantTrue string) string {
	if want {
		return ifWantTrue
	}
	return "sets " + field + ": true"
}

func value(b *bool) bool { return b != nil && *b }

// spreadRule wants a workload's pods to have topology spread constraints.
type spreadRule struct{ *TopologySpreadRule }

func (r spreadRule) validate() error {
	if r.MinReplicas < 0 {
		return errors.New("topologySpread.minReplicas cannot be negative")
	}
	if slices.Contains(r.TopologyKeys, "") {
		return errors.New("topologySpread.topologyKeys lists an empty key")
	}
	return nil
}

func (r spreadRule) scope() kindScope     { return replicatedKinds }
func (r spreadRule) defaultKinds() []kind { return scaledKinds }

func (r spreadRule) check(o *object, _ *env) []problem {
	if o.templateErr != nil {
		return templateProblem(o)
	}
	if o.replicas < max(r.MinReplicas, 1) {
		return nil
	}
	constraints := o.template.Spec.TopologySpreadConstraints
	if len(r.TopologyKeys) == 0 {
		if len(constraints) > 0 {
			return nil
		}
		if o.replicas == 1 {
			return []problem{{message: "has no topology spread constraint"}}
		}
		return []problem{{message: fmt.Sprintf("has no topology spread constraint, so its %d replicas may all run on one node", o.replicas)}}
	}
	var missing []string
	for _, key := range r.TopologyKeys {
		if !slices.ContainsFunc(constraints, func(c corev1.TopologySpreadConstraint) bool { return c.TopologyKey == key }) {
			missing = append(missing, key)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return []problem{{message: fmt.Sprintf("has no topology spread constraint on %s", joinWords(missing, "or"))}}
}

// storageRule limits the storage classes claims ask for.
type storageRule struct{ *StorageClassRule }

func (r storageRule) validate() error {
	if len(r.Allow) == 0 && !r.AllowDefault {
		return errors.New("storageClasses must list the classes to allow, or set allowDefault")
	}
	return nil
}

func (r storageRule) scope() kindScope     { return storageKinds }
func (r storageRule) defaultKinds() []kind { return claimKinds }

// betaStorageClass is the annotation claims named their class with before
// spec.storageClassName.
const betaStorageClass = "volume.beta.kubernetes.io/storage-class"

func (r storageRule) check(o *object, _ *env) []problem {
	if o.gk.Kind != kindStatefulSet.Kind {
		if p, ok := r.claim(o.u.Object, o.u.GetAnnotations()); !ok {
			return []problem{{message: p}}
		}
		return nil
	}
	templates, _, err := unstructured.NestedSlice(o.u.Object, "spec", "volumeClaimTemplates")
	if err != nil {
		return []problem{{message: "cannot be checked: reading its volume claim templates: " + err.Error()}}
	}
	var out []problem
	for _, t := range templates {
		m, ok := t.(map[string]any)
		if !ok {
			continue
		}
		name, _, _ := unstructured.NestedString(m, "metadata", "name")
		annotations, _, _ := unstructured.NestedStringMap(m, "metadata", "annotations")
		if p, ok := r.claim(m, annotations); !ok {
			out = append(out, problem{message: fmt.Sprintf("its volume claim template %s %s", name, p)})
		}
	}
	return out
}

// claim checks one claim, or claim template, given as its fields.
func (r storageRule) claim(fields map[string]any, annotations map[string]string) (string, bool) {
	class, found, _ := unstructured.NestedString(fields, "spec", "storageClassName")
	if !found {
		class, found = annotations[betaStorageClass]
	}
	if !found {
		if r.AllowDefault {
			return "", true
		}
		return "names no storage class, so it gets the cluster's default, if there is one; allowed: " + strings.Join(r.Allow, ", "), false
	}
	if slices.Contains(r.Allow, class) {
		return "", true
	}
	return fmt.Sprintf("asks for storage class %q, which is not allowed; allowed: %s", class, strings.Join(r.Allow, ", ")), false
}

// conditionsRule wants status conditions to be True.
type conditionsRule []string

func (r conditionsRule) validate() error {
	if slices.Contains(r, "") {
		return errors.New("conditions lists an empty condition type")
	}
	return nil
}

func (r conditionsRule) scope() kindScope     { return anyKind }
func (r conditionsRule) defaultKinds() []kind { return nil }

func (r conditionsRule) check(o *object, _ *env) []problem {
	conditions, _, _ := unstructured.NestedSlice(o.u.Object, "status", "conditions")
	var out []problem
	for _, want := range r {
		var found map[string]any
		for _, c := range conditions {
			if m, ok := c.(map[string]any); ok && m["type"] == want {
				found = m
				break
			}
		}
		if found == nil {
			out = append(out, problem{message: fmt.Sprintf("has no %s condition", want)})
			continue
		}
		status, _ := found["status"].(string)
		if status == string(metav1.ConditionTrue) {
			continue
		}
		msg := fmt.Sprintf("%s is %s", want, status)
		if reason, _ := found["reason"].(string); reason != "" {
			msg += " (" + reason + ")"
		}
		if text, _ := found["message"].(string); text != "" {
			msg += ": " + text
		}
		out = append(out, problem{message: msg})
	}
	return out
}
