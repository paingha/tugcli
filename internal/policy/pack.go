// Package policy evaluates diagnostic policy packs: versioned YAML files of
// checks that say what workloads and other objects should look like, such as
// resource requests, probes, a non-root user, an allowed image registry or a
// condition that must be True. Packs are data only: there is no scripting and
// no plugin code in them. tug check gathers the objects a pack's checks look
// at, evaluates them and reports each failure as an ops.Finding, in the
// format of tug why.
package policy

import (
	_ "embed" // the baseline pack
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/api/validate/content"
	"sigs.k8s.io/yaml"

	"github.com/paingha/tugcli/internal/ops"
)

// APIVersion is the version of the pack format this tug reads.
const APIVersion = "tug.policy/v1alpha1"

// PackKind is the kind every pack file declares.
const PackKind = "PolicyPack"

// BaselineName is the name of the pack built into tug.
const BaselineName = "baseline"

//go:embed packs/baseline.yaml
var baselineYAML []byte

// Baseline returns the pack built into tug: advisory checks that most
// workloads should pass.
func Baseline() *Pack {
	p, err := Parse(baselineYAML, "built in")
	if err != nil {
		panic(fmt.Sprintf("the built-in baseline pack is invalid: %v", err)) // a programmer error, caught by the tests
	}
	return p
}

// Pack is a policy pack: a named, versioned set of checks.
type Pack struct {
	APIVersion  string  `json:"apiVersion"`
	Kind        string  `json:"kind"`
	Name        string  `json:"name"`
	Version     string  `json:"version"`
	Description string  `json:"description,omitempty"`
	Checks      []Check `json:"checks"`
	// Source is where the pack was loaded from: "built in", a file or a
	// URL. It is not part of the file.
	Source string `json:"-"`
}

// Enforcement says whether a failed check fails tug check.
type Enforcement string

// Enforcements.
const (
	// Advisory checks report what they find and never fail the command.
	// Checks are advisory unless they say otherwise.
	Advisory Enforcement = "advisory"
	// Blocking checks make tug check exit 1 when they fail.
	Blocking Enforcement = "blocking"
)

// Check is one check of a pack. Exactly one of the fields after Match says
// what it checks.
type Check struct {
	// ID names the check within its pack, in lower case with dashes.
	ID          string       `json:"id"`
	Severity    ops.Severity `json:"severity"`
	Enforcement Enforcement  `json:"enforcement,omitempty"`
	// Description says what the check wants, as a sentence.
	Description string `json:"description"`
	// Remediation says how to fix an object that fails, and becomes the
	// finding's next step.
	Remediation string `json:"remediation,omitempty"`
	Match       Match  `json:"match,omitzero"`

	// RequiredLabels are label keys the object must have, with a value.
	RequiredLabels []string `json:"requiredLabels,omitempty"`
	// RequiredAnnotations are annotation keys the object must have, with a
	// value.
	RequiredAnnotations []string `json:"requiredAnnotations,omitempty"`
	// ImageRegistries limits where containers' images come from.
	ImageRegistries *RegistryRule `json:"imageRegistries,omitempty"`
	// Resources are the requests and limits every container must set.
	Resources *ResourcesRule `json:"resources,omitempty"`
	// Probes are the probes every container must have: readiness, liveness
	// or startup.
	Probes []string `json:"probes,omitempty"`
	// PodDisruptionBudget wants a budget to cover the workload's pods.
	PodDisruptionBudget *BudgetRule `json:"podDisruptionBudget,omitempty"`
	// SecurityContext is how every container must run.
	SecurityContext *SecurityContextRule `json:"securityContext,omitempty"`
	// TopologySpread wants the workload's pods spread across nodes or
	// zones.
	TopologySpread *TopologySpreadRule `json:"topologySpread,omitempty"`
	// StorageClasses limits the storage classes volume claims ask for.
	StorageClasses *StorageClassRule `json:"storageClasses,omitempty"`
	// Conditions are status condition types that must be True, as on a
	// custom resource.
	Conditions []string `json:"conditions,omitempty"`

	rule  rule
	kinds []kind
}

// Blocking reports whether a failure of the check fails tug check.
func (c *Check) Blocking() bool { return c.Enforcement == Blocking }

// Kinds returns the kinds the check applies to: those its match names, or
// the defaults for its type.
func (c *Check) Kinds() []string {
	out := make([]string, len(c.kinds))
	for i, k := range c.kinds {
		out[i] = k.String()
	}
	return out
}

// Match says which objects a check applies to. An object must match every
// part that is set.
type Match struct {
	// Kinds are the kinds the check applies to, such as Deployment or, for
	// a kind tug does not know, Certificate.cert-manager.io with its
	// group. Each type of check has defaults.
	Kinds []string `json:"kinds,omitempty"`
	// Labels the object must have, with these values.
	Labels map[string]string `json:"labels,omitempty"`
	// Namespaces the object must be in. Cluster-scoped objects never
	// match a list of namespaces.
	Namespaces []string `json:"namespaces,omitempty"`
	// ExcludeNamespaces the object must not be in, such as kube-system.
	ExcludeNamespaces []string `json:"excludeNamespaces,omitempty"`
}

// RegistryRule limits where images come from.
type RegistryRule struct {
	// Allow lists registries, such as registry.example.com, or paths in a
	// registry, such as ghcr.io/acme. An image without a registry comes
	// from docker.io.
	Allow []string `json:"allow"`
}

// ResourcesRule lists the requests and limits every container must set.
type ResourcesRule struct {
	// Requests such as cpu and memory.
	Requests []string `json:"requests,omitempty"`
	// Limits such as memory.
	Limits []string `json:"limits,omitempty"`
}

// BudgetRule wants a PodDisruptionBudget to select a workload's pods.
type BudgetRule struct {
	// MinReplicas skips workloads with fewer replicas. Default 1: every
	// workload.
	MinReplicas int32 `json:"minReplicas,omitempty"`
}

// SecurityContextRule is how containers must run. Each field that is set
// is a value the container's effective setting must have.
type SecurityContextRule struct {
	// RunAsNonRoot, from the container's securityContext or else the
	// pod's.
	RunAsNonRoot *bool `json:"runAsNonRoot,omitempty"`
	// ReadOnlyRootFilesystem, from the container's securityContext.
	ReadOnlyRootFilesystem *bool `json:"readOnlyRootFilesystem,omitempty"`
	// Privileged, from the container's securityContext; false forbids
	// privileged containers.
	Privileged *bool `json:"privileged,omitempty"`
}

// TopologySpreadRule wants a workload's pods to have topology spread
// constraints.
type TopologySpreadRule struct {
	// MinReplicas skips workloads with fewer replicas. Default 1: every
	// workload.
	MinReplicas int32 `json:"minReplicas,omitempty"`
	// TopologyKeys each need a constraint, such as
	// topology.kubernetes.io/zone. Without them any constraint will do.
	TopologyKeys []string `json:"topologyKeys,omitempty"`
}

// StorageClassRule limits the storage classes volume claims ask for.
type StorageClassRule struct {
	// Allow lists the storage classes claims may name.
	Allow []string `json:"allow,omitempty"`
	// AllowDefault lets a claim name no class and get the cluster's
	// default.
	AllowDefault bool `json:"allowDefault,omitempty"`
}

// Parse reads a pack from YAML and checks it. origin names the file or URL
// in errors.
func Parse(data []byte, origin string) (*Pack, error) {
	var p Pack
	if err := yaml.UnmarshalStrict(data, &p); err != nil {
		return nil, fmt.Errorf("%s: %w", origin, err)
	}
	if err := p.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", origin, err)
	}
	p.Source = origin
	return &p, nil
}

// namePattern is what pack names and check IDs look like.
var namePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

func (p *Pack) validate() error {
	var errs []error
	if p.APIVersion != APIVersion {
		errs = append(errs, fmt.Errorf("apiVersion must be %s, not %q", APIVersion, p.APIVersion))
	}
	if p.Kind != PackKind {
		errs = append(errs, fmt.Errorf("kind must be %s, not %q", PackKind, p.Kind))
	}
	if !namePattern.MatchString(p.Name) {
		errs = append(errs, fmt.Errorf("name %q must be lower case letters, digits and dashes", p.Name))
	}
	if p.Version == "" || strings.ContainsAny(p.Version, " \t\n") {
		errs = append(errs, fmt.Errorf("version %q must be set, without spaces, such as 1.2.0", p.Version))
	}
	if len(p.Checks) == 0 {
		errs = append(errs, errors.New("a pack needs at least one check"))
	}
	seen := map[string]bool{}
	for i := range p.Checks {
		c := &p.Checks[i]
		if seen[c.ID] {
			errs = append(errs, fmt.Errorf("check %q appears twice", c.ID))
		}
		seen[c.ID] = true
		if err := c.validate(); err != nil {
			name := c.ID
			if name == "" {
				name = fmt.Sprintf("#%d", i+1)
			}
			errs = append(errs, fmt.Errorf("check %s: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

func (c *Check) validate() error {
	var errs []error
	if !namePattern.MatchString(c.ID) {
		errs = append(errs, fmt.Errorf("id %q must be lower case letters, digits and dashes", c.ID))
	}
	switch c.Severity {
	case ops.SeverityInfo, ops.SeverityWarning, ops.SeverityError:
	default:
		errs = append(errs, fmt.Errorf("severity must be info, warning or error, not %q", c.Severity))
	}
	switch c.Enforcement {
	case "":
		c.Enforcement = Advisory
	case Advisory, Blocking:
	default:
		errs = append(errs, fmt.Errorf("enforcement must be advisory or blocking, not %q", c.Enforcement))
	}
	if strings.TrimSpace(c.Description) == "" {
		errs = append(errs, errors.New("description must say what the check wants"))
	}
	r, err := c.buildRule()
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	c.rule = r
	if err := r.validate(); err != nil {
		errs = append(errs, err)
	}
	if err := c.Match.validate(); err != nil {
		errs = append(errs, err)
	}
	kinds, err := c.matchKinds()
	if err != nil {
		errs = append(errs, err)
	}
	c.kinds = kinds
	return errors.Join(errs...)
}

// checkTypes are the fields that say what a check checks, as a pack
// writes them.
var checkTypes = []string{
	"requiredLabels", "requiredAnnotations", "imageRegistries", "resources", "probes",
	"podDisruptionBudget", "securityContext", "topologySpread", "storageClasses", "conditions",
}

// buildRule returns the rule of the one check type that is set.
func (c *Check) buildRule() (rule, error) {
	var set []string
	var r rule
	add := func(name string, present bool, mk func() rule) {
		if present {
			set = append(set, name)
			r = mk()
		}
	}
	add("requiredLabels", len(c.RequiredLabels) > 0, func() rule { return keysRule{what: "label", keys: c.RequiredLabels} })
	add("requiredAnnotations", len(c.RequiredAnnotations) > 0, func() rule { return keysRule{what: "annotation", keys: c.RequiredAnnotations} })
	add("imageRegistries", c.ImageRegistries != nil, func() rule { return registryRule{c.ImageRegistries} })
	add("resources", c.Resources != nil, func() rule { return resourcesRule{c.Resources} })
	add("probes", len(c.Probes) > 0, func() rule { return probesRule(c.Probes) })
	add("podDisruptionBudget", c.PodDisruptionBudget != nil, func() rule { return budgetRule{c.PodDisruptionBudget} })
	add("securityContext", c.SecurityContext != nil, func() rule { return securityRule{c.SecurityContext} })
	add("topologySpread", c.TopologySpread != nil, func() rule { return spreadRule{c.TopologySpread} })
	add("storageClasses", c.StorageClasses != nil, func() rule { return storageRule{c.StorageClasses} })
	add("conditions", len(c.Conditions) > 0, func() rule { return conditionsRule(c.Conditions) })
	switch len(set) {
	case 0:
		return nil, fmt.Errorf("say what to check with one of %s", strings.Join(checkTypes, ", "))
	case 1:
		return r, nil
	default:
		return nil, fmt.Errorf("has %s; a check checks one thing, so split it into several", strings.Join(set, " and "))
	}
}

// matchKinds parses the kinds the check names, or returns its type's
// defaults, and makes sure its type can check each.
func (c *Check) matchKinds() ([]kind, error) {
	if len(c.Match.Kinds) == 0 {
		if def := c.rule.defaultKinds(); len(def) > 0 {
			return def, nil
		}
		return nil, errors.New("match.kinds must name the kinds to check")
	}
	var errs []error
	var out []kind
	for _, s := range c.Match.Kinds {
		k, err := parseKind(s)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if !c.rule.scope().accepts(k) {
			errs = append(errs, fmt.Errorf("cannot check kind %s; %s", k, c.rule.scope().hint()))
			continue
		}
		if !slices.Contains(out, k) {
			out = append(out, k)
		}
	}
	return out, errors.Join(errs...)
}

func (m *Match) validate() error {
	var errs []error
	for k := range m.Labels {
		if msgs := content.IsLabelKey(k); len(msgs) > 0 {
			errs = append(errs, fmt.Errorf("match.labels key %q: %s", k, strings.Join(msgs, "; ")))
		}
	}
	if len(m.Namespaces) > 0 && len(m.ExcludeNamespaces) > 0 {
		errs = append(errs, errors.New("match has namespaces and excludeNamespaces; use one"))
	}
	return errors.Join(errs...)
}

// validateKeys checks label or annotation keys as Kubernetes would.
func validateKeys(field string, keys []string) error {
	var errs []error
	for _, k := range keys {
		if msgs := content.IsLabelKey(k); len(msgs) > 0 {
			errs = append(errs, fmt.Errorf("%s %q: %s", field, k, strings.Join(msgs, "; ")))
		}
	}
	return errors.Join(errs...)
}
