package policy

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/paingha/tugcli/internal/ops"
)

// Report is what evaluating packs found.
type Report struct {
	// Target is the one object checked, when the caller named one.
	Target string `json:"target,omitempty"`
	// Namespace is the namespace checked; empty for every namespace.
	Namespace string    `json:"namespace,omitempty"`
	Packs     []PackRef `json:"packs"`
	// Objects is how many objects at least one check applied to.
	Objects int `json:"objects"`
	// Passed is false when a blocking check failed. Advisory checks never
	// fail.
	Passed bool `json:"passed"`
	// Findings come in namespace order, blocking first, then by severity
	// and in the order of the packs and their checks.
	Findings []Result `json:"findings"`
	// Unlisted names the resources that could not be listed, so whose
	// findings may be missing.
	Unlisted []string `json:"unlisted,omitempty"`
}

// PackRef names one pack that was evaluated.
type PackRef struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Result is one failure of a check: a finding as tug why reports it, with
// the check it came from. Objects with the same failure share a result.
type Result struct {
	ops.Finding
	// Namespace is where the objects are; empty for cluster-scoped ones.
	Namespace string `json:"namespace,omitempty"`
	Pack      string `json:"pack"`
	Check     string `json:"check"`
	// Blocking is true when the check is blocking, so the result fails tug
	// check.
	Blocking bool `json:"blocking"`
}

// Blocking counts the results of blocking checks.
func (r *Report) Blocking() int {
	n := 0
	for _, f := range r.Findings {
		if f.Blocking {
			n++
		}
	}
	return n
}

// Evaluate runs the checks of packs on every object inv holds.
func Evaluate(packs []*Pack, inv *Inventory) *Report {
	r := &Report{Passed: true, Findings: []Result{}, Unlisted: inv.Unlisted}
	e := &env{budgets: inv.PodDisruptionBudgets, budgetsUnknown: inv.BudgetsUnknown}
	type key struct{ namespace, pack, check, container, message string }
	index := map[key]int{}
	// order is where each check comes in the packs, for sorting.
	order := map[[2]string]int{}
	for _, p := range packs {
		r.Packs = append(r.Packs, PackRef{Name: p.Name, Version: p.Version})
		for _, c := range p.Checks {
			order[[2]string{p.Name, c.ID}] = len(order)
		}
	}
	for _, u := range inv.Objects {
		o := newObject(u)
		applied := false
		for _, p := range packs {
			for i := range p.Checks {
				c := &p.Checks[i]
				if !c.applies(o) {
					continue
				}
				applied = true
				for _, pr := range c.rule.check(o, e) {
					k := key{u.GetNamespace(), p.Name, c.ID, pr.container, pr.message}
					if j, ok := index[k]; ok {
						r.Findings[j].Objects = append(r.Findings[j].Objects, o.ref())
						continue
					}
					index[k] = len(r.Findings)
					r.Findings = append(r.Findings, Result{
						Finding: ops.Finding{
							Severity: c.Severity, Objects: []string{o.ref()}, Container: pr.container,
							Reason: c.ID, Message: pr.message, Next: c.Remediation,
						},
						Namespace: u.GetNamespace(), Pack: p.Name, Check: c.ID, Blocking: c.Blocking(),
					})
					if c.Blocking() {
						r.Passed = false
					}
				}
			}
		}
		if applied {
			r.Objects++
		}
	}
	for i := range r.Findings {
		slices.Sort(r.Findings[i].Objects)
	}
	slices.SortStableFunc(r.Findings, func(a, b Result) int {
		return cmp.Or(
			cmp.Compare(a.Namespace, b.Namespace),
			compareBool(b.Blocking, a.Blocking),
			cmp.Compare(rank(a.Severity), rank(b.Severity)),
			cmp.Compare(order[[2]string{a.Pack, a.Check}], order[[2]string{b.Pack, b.Check}]),
			cmp.Compare(a.Objects[0], b.Objects[0]),
			cmp.Compare(a.Container, b.Container),
		)
	})
	return r
}

func compareBool(a, b bool) int {
	switch {
	case a == b:
		return 0
	case a:
		return 1
	default:
		return -1
	}
}

func rank(s ops.Severity) int {
	switch s {
	case ops.SeverityError:
		return 0
	case ops.SeverityWarning:
		return 1
	default:
		return 2
	}
}

// applies reports whether the check's match takes in o.
func (c *Check) applies(o *object) bool {
	if !slices.ContainsFunc(c.kinds, func(k kind) bool { return k.of(o.gk) }) {
		return false
	}
	ns := o.u.GetNamespace()
	if len(c.Match.Namespaces) > 0 && !slices.Contains(c.Match.Namespaces, ns) {
		return false
	}
	if ns != "" && slices.Contains(c.Match.ExcludeNamespaces, ns) {
		return false
	}
	have := o.u.GetLabels()
	for k, v := range c.Match.Labels {
		if got, ok := have[k]; !ok || got != v {
			return false
		}
	}
	return true
}

// Select returns the packs that names name, in that order. Without names,
// it returns those that defaults names, from config.yaml, or else every
// pack.
func Select(packs []*Pack, names, defaults []string) ([]*Pack, error) {
	from := "--pack"
	if len(names) == 0 {
		names, from = defaults, "policy.default in config.yaml"
	}
	if len(names) == 0 {
		return packs, nil
	}
	var out []*Pack
	for _, name := range names {
		i := slices.IndexFunc(packs, func(p *Pack) bool { return p.Name == name })
		if i < 0 {
			have := make([]string, len(packs))
			for j, p := range packs {
				have[j] = p.Name
			}
			return nil, fmt.Errorf("%s names %q, but no pack has that name; the packs are %s", from, name, strings.Join(have, ", "))
		}
		if !slices.Contains(out, packs[i]) {
			out = append(out, packs[i])
		}
	}
	return out, nil
}
