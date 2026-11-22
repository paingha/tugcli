# The flux suite: tug gitops status, explain and dependencies on what Flux
# delivers, and why and reconcile on Flux's objects and controllers. Sourced
# by scripts/e2e-kind.sh, connected to $cluster.

echo "--- tug gitops finds no Flux before it is installed"
# This also fills tug's discovery cache, which must not hide Flux once it is
# installed.
expect_exit 0 tug gitops status -A -o json
lacks "$out" '"flux"'

echo "--- install Flux $FLUX_VERSION, and deliver podinfo with it"
kubectl apply -f "https://github.com/fluxcd/flux2/releases/download/$FLUX_VERSION/install.yaml"
rolled_out flux-system deployment
kubectl create namespace e2e-flux
kubectl apply -f "$manifests/flux.yaml"
kubectl -n flux-system wait --for=condition=Ready kustomization/podinfo --timeout=300s
kubectl -n flux-system wait --for=condition=Ready helmrelease/podinfo-helm --timeout=300s
# podinfo-broken waits for podinfo, then fails on its path.
eventually 60 "podinfo-broken to fail on its path" matches 'does-not-exist' \
  kubectl -n flux-system get kustomization podinfo-broken -o jsonpath='{.status.conditions[?(@.type=="Ready")].message}'

echo "--- tug gitops status"
tug ns e2e-flux
expect_exit 0 tug gitops status
has "$out" '^gitrepository/podinfo +flux-system +True +Succeeded +False +6\.17\.0@sha1:[0-9a-f]+ ' \
  '^kustomization/podinfo +flux-system +True +ReconciliationSucceeded +False ' \
  '^kustomization/podinfo-broken +flux-system +False '
lacks "$out" '^helm'
expect_exit 0 tug gitops status -n e2e-helm
has "$out" '^helmrepository/podinfo +flux-system +True ' '^helmrelease/podinfo-helm +flux-system +True '
lacks "$out" '^kustomization/'
expect_exit 0 tug gitops status -A -o name
has "$out" '^gitrepositories\.source\.toolkit\.fluxcd\.io/podinfo$' \
  '^kustomizations\.kustomize\.toolkit\.fluxcd\.io/podinfo-broken$' \
  '^helmreleases\.helm\.toolkit\.fluxcd\.io/podinfo-helm$'

echo "--- tug gitops explain and dependencies"
expect_exit 0 tug gitops explain podinfo
has "$out" '^kustomization/podinfo in flux-system is healthy' '^source gitrepository/podinfo at 6\.17\.0@sha1:' \
  '^Manages [0-9]+ objects, none of them unhealthy'
expect_exit 1 tug gitops explain ks/podinfo-broken
has "$out" '^kustomization/podinfo-broken in flux-system is not ready' 'kustomization path not found: .*does-not-exist'
expect_exit 0 tug gitops explain hr/podinfo-helm
has "$out" '^helmrelease/podinfo-helm in flux-system is healthy' '^Manages [0-9]+ objects, none of them unhealthy'
expect_exit 0 tug gitops dependencies podinfo-broken
has "$out" '^Depends on:' '^kustomization/podinfo-broken ' 'kustomization/podinfo .*ReconciliationSucceeded'
expect_exit 0 tug gitops deps ks/podinfo -n flux-system -o json
has "$out" '"requiredBy"' '"name": "podinfo-broken"'

echo "--- tug why and reconcile on Flux's objects and controllers"
expect_exit 0 tug why kustomization.kustomize.toolkit.fluxcd.io/podinfo -n flux-system
has "$out" '^kustomization/podinfo in flux-system is healthy'
expect_exit 1 tug why ks/podinfo-broken -n flux-system
has "$out" 'does-not-exist'
expect_exit 0 tug reconcile status kustomize-controller
has "$out" '^deploy/kustomize-controller in flux-system is healthy: .*leader kustomize-controller-' \
  '^lease/kustomize-controller-leader-election '
expect_exit 0 tug reconcile explain ks/podinfo -n flux-system
has "$out" 'deploy/kustomize-controller in flux-system is healthy' 'kustomize-controller reported events about it'
expect_exit 0 tug reconcile trace ks/podinfo -n flux-system --logs
has "$out" 'managedFields +kustomization/podinfo ' 'event +kustomization/podinfo '
expect_exit 0 tug reconcile events ks/podinfo -n flux-system
has "$out" 'kustomization/podinfo +ReconciliationSucceeded +kustomize-controller'
