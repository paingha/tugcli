# The argocd suite: tug gitops status, explain and dependencies on what Argo
# CD delivers. Sourced by scripts/e2e-kind.sh, connected to $cluster.

echo "--- tug gitops finds no Argo CD before it is installed"
# This also fills tug's discovery cache, which must not hide Argo CD once it
# is installed.
expect_exit 0 tug gitops status -A -o json
lacks "$out" '"argocd"'

echo "--- install Argo CD $ARGOCD_VERSION's core, and deliver the guestbook through an app of apps"
kubectl create namespace argocd
# Redis is pulled from Docker Hub, as every other image of these suites is,
# rather than from public.ecr.aws, which limits anonymous pulls: on CI, its
# redis once did not start in five minutes while the images from quay.io
# took seconds.
curl -fsSL "https://raw.githubusercontent.com/argoproj/argo-cd/$ARGOCD_VERSION/manifests/core-install.yaml" |
  sed 's#public\.ecr\.aws/docker/library/redis:#redis:#' > "$work/argocd-core.yaml"
grep -q 'image: redis:' "$work/argocd-core.yaml" || fail "Argo CD's core install no longer names redis as expected" "$work/argocd-core.yaml"
kubectl apply -n argocd --server-side --force-conflicts -f "$work/argocd-core.yaml"
rolled_out argocd deployment statefulset
kubectl apply -f "$manifests/argocd.yaml"
eventually 60 "the app of apps to create example.guestbook" kubectl -n argocd get application example.guestbook
kubectl -n argocd wait --for=jsonpath='{.status.sync.status}'=Synced application/example.guestbook --timeout=300s
kubectl -n argocd wait --for=jsonpath='{.status.health.status}'=Healthy application/example.guestbook --timeout=300s
kubectl -n argocd wait --for=jsonpath='{.status.sync.status}'=Synced application/apps --timeout=300s
eventually 60 "broken to fail on its path" matches 'ComparisonError' \
  kubectl -n argocd get application broken -o jsonpath='{.status.conditions[*].type}'

echo "--- tug gitops status"
tug ns e2e-argo
expect_exit 0 tug gitops status
has "$out" '^application/example\.guestbook +argocd +True +Synced, Healthy ' '^application/broken +argocd +False '
expect_exit 0 tug gitops status -A -o name
has "$out" '^applications\.argoproj\.io/apps$' '^applications\.argoproj\.io/example\.guestbook$' \
  '^applications\.argoproj\.io/broken$'

echo "--- tug gitops explain and dependencies"
expect_exit 0 tug gitops explain app/example.guestbook
has "$out" '^application/example\.guestbook in argocd is healthy: Synced, Healthy' \
  '^Manages [0-9]+ objects, none of them unhealthy'
expect_exit 1 tug gitops explain broken
has "$out" '^application/broken in argocd is not ready' 'ComparisonError' 'app path does not exist'
expect_exit 0 tug gitops dependencies app/apps
has "$out" '^Manages:' 'application/example\.guestbook ' 'Argo CD Applications have no dependsOn'
expect_exit 0 tug gitops deps app/example.guestbook -o json
has "$out" '"name": "apps"'
