package ops

import (
	"context"
	"errors"
	"fmt"
	"slices"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/discovery"
)

// PreferredResources returns the resources the cluster serves, each at its
// group's preferred version, and the group versions whose discovery failed,
// whose resources are missing. A failed group is not an error.
//
// It is for callers that go through every kind the cluster serves, which
// would silently miss the objects of a kind whose CRD was installed after
// tug's discovery cache was filled: the cache stays fresh for hours. So
// when d answered from what an earlier command cached, PreferredResources
// reads discovery again from the cluster. That is two requests on clusters
// with aggregated discovery (on by default since Kubernetes 1.27), answered
// 304 Not Modified through the HTTP cache when nothing has changed, and one
// per group version on older clusters. Later calls with the same d, such as
// watch's searches for new kinds, use what the first one read. Mapping a
// name to its kind needs no such care: the RESTMapper reads discovery again
// itself when the kind is not in the cache.
func PreferredResources(ctx context.Context, d discovery.DiscoveryInterface) ([]*metav1.APIResourceList, []string, error) {
	read := func() ([]*metav1.APIResourceList, error) {
		return discovery.ServerPreferredResourcesWithContext(ctx, discovery.ToDiscoveryInterfaceWithContext(d))
	}
	lists, err := read()
	if cached, ok := d.(discovery.CachedDiscoveryInterface); ok && !cached.Fresh() {
		cached.Invalidate()
		lists, err = read()
	}
	var failed []string
	if err != nil {
		var partial *discovery.ErrGroupDiscoveryFailed
		if !errors.As(err, &partial) {
			return nil, nil, fmt.Errorf("discovering the cluster's resources: %w", err)
		}
		for gv := range partial.Groups {
			failed = append(failed, gv.String())
		}
		slices.Sort(failed)
	}
	return lists, failed, nil
}
