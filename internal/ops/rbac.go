package ops

import (
	"context"
	"slices"
	"strings"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
)

// serviceAccount names a service account.
type serviceAccount struct{ namespace, name string }

func (s serviceAccount) String() string { return s.namespace + "/" + s.name }

// statusGrant is a service account's permission to update or patch the
// status of a resource, with the role that grants it. Controllers write the
// status of what they reconcile, so the grant is evidence of what one
// reconciles, though not proof.
type statusGrant struct {
	account serviceAccount
	// group and resource are what the rule names; either may be "*".
	group, resource string
	// role is the role that grants it, such as "ClusterRole cert-manager".
	role string
	// namespace is where the grant applies: "" for the whole cluster.
	namespace string
}

// wildcard reports whether the grant covers every group or resource, as
// cluster-admin's does, which says nothing about what a controller
// reconciles.
func (g statusGrant) wildcard() bool { return g.group == "*" || g.resource == "*" }

// rbacIndex says which service accounts may write which resources' status.
type rbacIndex struct {
	grants []statusGrant
	// unreadable names the RBAC resources tug could not list, whose grants
	// are missing.
	unreadable []string
}

// loadRBAC reads every role and binding in the cluster and keeps the grants
// to write a status that service accounts hold. Roles and bindings that
// cannot be listed are noted rather than failing the whole.
func loadRBAC(ctx context.Context, cs kubernetes.Interface) (*rbacIndex, error) {
	x := &rbacIndex{}
	rbac := cs.RbacV1()
	all := metav1.ListOptions{}
	note := func(what string, err error) bool {
		if err == nil {
			return true
		}
		if ctx.Err() == nil {
			x.unreadable = append(x.unreadable, what)
		}
		return false
	}

	clusterRoles := map[string][]rbacv1.PolicyRule{}
	if list, err := rbac.ClusterRoles().List(ctx, all); note("clusterroles", err) {
		for _, r := range list.Items {
			clusterRoles[r.Name] = r.Rules
		}
	}
	roles := map[string][]rbacv1.PolicyRule{}
	if list, err := rbac.Roles(metav1.NamespaceAll).List(ctx, all); note("roles", err) {
		for _, r := range list.Items {
			roles[r.Namespace+"/"+r.Name] = r.Rules
		}
	}
	if list, err := rbac.ClusterRoleBindings().List(ctx, all); note("clusterrolebindings", err) {
		for _, b := range list.Items {
			if b.RoleRef.Kind != "ClusterRole" {
				continue
			}
			x.add(b.Subjects, "", clusterRoles[b.RoleRef.Name], "ClusterRole "+b.RoleRef.Name, "")
		}
	}
	if list, err := rbac.RoleBindings(metav1.NamespaceAll).List(ctx, all); note("rolebindings", err) {
		for _, b := range list.Items {
			switch b.RoleRef.Kind {
			case "ClusterRole":
				x.add(b.Subjects, b.Namespace, clusterRoles[b.RoleRef.Name], "ClusterRole "+b.RoleRef.Name, b.Namespace)
			case "Role":
				x.add(b.Subjects, b.Namespace, roles[b.Namespace+"/"+b.RoleRef.Name], "Role "+b.Namespace+"/"+b.RoleRef.Name, b.Namespace)
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return x, nil
}

// add records the status grants that rules give the service accounts among
// subjects. A RoleBinding's service account subjects default to its own
// namespace.
func (x *rbacIndex) add(subjects []rbacv1.Subject, bindingNamespace string, rules []rbacv1.PolicyRule, role, scope string) {
	for _, s := range subjects {
		if s.Kind != rbacv1.ServiceAccountKind {
			continue
		}
		sa := serviceAccount{namespace: s.Namespace, name: s.Name}
		if sa.namespace == "" {
			sa.namespace = bindingNamespace
		}
		for _, r := range rules {
			for _, gr := range statusResources(r) {
				x.grants = append(x.grants, statusGrant{account: sa, group: gr.Group, resource: gr.Resource, role: role, namespace: scope})
			}
		}
	}
}

// statusResources returns the resources whose status a rule lets its holder
// update or patch: "certificates/status" as certificates, "*/status" and "*"
// as "*".
func statusResources(r rbacv1.PolicyRule) []schema.GroupResource {
	writes := slices.ContainsFunc(r.Verbs, func(v string) bool {
		return v == rbacv1.VerbAll || v == "update" || v == "patch"
	})
	if !writes {
		return nil
	}
	var out []schema.GroupResource
	for _, res := range r.Resources {
		var name string
		switch {
		case res == rbacv1.ResourceAll || res == "*/status":
			name = "*"
		case strings.HasSuffix(res, "/status"):
			name = strings.TrimSuffix(res, "/status")
		default:
			continue
		}
		for _, g := range r.APIGroups {
			out = append(out, schema.GroupResource{Group: g, Resource: name})
		}
	}
	return out
}

// of returns the grants a service account holds.
func (x *rbacIndex) of(sa serviceAccount) []statusGrant {
	var out []statusGrant
	for _, g := range x.grants {
		if g.account == sa {
			out = append(out, g)
		}
	}
	return out
}

// writers returns the grants that name a resource's status outright, not
// through a wildcard, and apply in a namespace ("" for a cluster-scoped
// object).
func (x *rbacIndex) writers(gr schema.GroupResource, ns string) []statusGrant {
	var out []statusGrant
	for _, g := range x.grants {
		if g.wildcard() || g.group != gr.Group || g.resource != gr.Resource {
			continue
		}
		if g.namespace != "" && g.namespace != ns {
			continue
		}
		out = append(out, g)
	}
	return out
}
