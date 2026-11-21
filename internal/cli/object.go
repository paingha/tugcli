package cli

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/restmapper"

	"github.com/paingha/tugcli/internal/ops"
	"github.com/paingha/tugcli/internal/resolve"
)

// objectClients returns the clients that why and tree need for objects other
// than workloads, with a RESTMapper that knows kubectl's short names.
func (a *app) objectClients(env *opsEnv) (ops.Clients, meta.RESTMapper, error) {
	dc, err := env.factory.DiscoveryFor(env.cfg)
	if err != nil {
		return ops.Clients{}, nil, err
	}
	dyn, err := dynamic.NewForConfig(env.cfg)
	if err != nil {
		return ops.Clients{}, nil, err
	}
	md, err := metadata.NewForConfig(env.cfg)
	if err != nil {
		return ops.Clients{}, nil, err
	}
	warn := func(msg string) { fmt.Fprintf(a.stderr, "%s %s\n", a.colors().Yellow("warning:"), msg) }
	mapper := restmapper.NewShortcutExpander(restmapper.NewDeferredDiscoveryRESTMapper(dc), dc, warn)
	return ops.Clients{Typed: env.cs, Dynamic: dyn, Metadata: md, Discovery: dc}, mapper, nil
}

// resolveObject turns a target argument into any object, for the commands
// that work on more than workloads. Without a kind, or with a workload's,
// it resolves as resolveTarget does. Any other kind is looked up in the
// cluster's API discovery as kubectl does, and its objects are matched by
// name, asking when several match equally well.
func (a *app) resolveObject(ctx context.Context, env *opsEnv, c ops.Clients, mapper meta.RESTMapper, arg string) (resolve.Object, error) {
	q, err := resolve.ParseQuery(arg)
	if err != nil {
		return resolve.Object{}, err
	}
	if q.Resource == "" {
		t, err := a.resolveTarget(ctx, env, arg)
		return resolve.ObjectOf(t), err
	}
	m, err := resolve.FindResource(mapper, q.Resource)
	if err != nil {
		return resolve.Object{}, err
	}
	if k, ok := resolve.WorkloadKind(m.Resource.GroupResource()); ok {
		t, err := a.resolveTarget(ctx, env, k.Short()+"/"+q.Name)
		return resolve.ObjectOf(t), err
	}
	objects, err := resolve.Objects(ctx, c.Metadata, m, env.namespace)
	if err != nil {
		return resolve.Object{}, fmt.Errorf("listing %s: %w", m.Resource.GroupResource(), err)
	}
	names := make([]string, len(objects))
	for i, o := range objects {
		names[i] = o.Name
	}
	matches := resolve.Best(q.Name, names)
	where := " in namespace " + env.namespace
	if m.Scope.Name() == meta.RESTScopeNameRoot {
		where = ""
	}
	i := 0
	switch len(matches) {
	case 0:
		hint := ""
		if where != "" {
			hint = "; use -n for another namespace"
		}
		return resolve.Object{}, fmt.Errorf("no %s%s matches %q%s", m.Resource.GroupResource(), where, q.Name, hint)
	case 1:
	default:
		labels := make([]string, len(matches))
		for j, name := range matches {
			labels[j] = q.Resource + "/" + name
		}
		if i, err = a.pick("Matches for "+arg+":", labels); err != nil {
			return resolve.Object{}, err
		}
	}
	for _, o := range objects {
		if o.Name == matches[i] {
			return o, nil
		}
	}
	return resolve.Object{}, fmt.Errorf("no %s%s matches %q", m.Resource.GroupResource(), where, q.Name)
}
