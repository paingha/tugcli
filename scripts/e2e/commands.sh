# The commands suite: why, tree, reconcile, watch, diff, drift, support
# bundle, check, api, crd, profiles and team catalogs. Sourced by
# scripts/e2e-kind.sh, connected to $cluster, with $cluster_b to compare it
# with.

# kb runs kubectl on the second cluster.
kb() { kubectl --kubeconfig "$work/kind-b.yaml" "$@"; }

# widget_ready sets the Ready condition of widget/gear, as a controller would.
widget_ready() {
  local now
  now="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  kubectl patch widget gear --subresource=status --type=merge -p "{\"status\":{\"observedGeneration\":1,\"conditions\":[
    {\"type\":\"Ready\",\"status\":\"$1\",\"reason\":\"$2\",\"message\":\"$3\",\"lastTransitionTime\":\"$now\",\"observedGeneration\":1}]}}"
}

echo "--- a custom resource with no controller, owning a ConfigMap and a deployment that cannot pull its image"
# Both clusters serve the CRD, so that tug drift finds the same APIs on each.
kubectl apply -f "$manifests/widget-crd.yaml"
kb apply -f "$manifests/widget-crd.yaml"
kubectl wait --for=condition=Established crd/widgets.e2e.tug.dev --timeout=60s
kb wait --for=condition=Established crd/widgets.e2e.tug.dev --timeout=60s
kubectl create namespace e2e-why
tug ns e2e-why
kubectl apply -f - <<'EOF'
apiVersion: e2e.tug.dev/v1
kind: Widget
metadata:
  name: gear
spec:
  size: 3
EOF
uid="$(kubectl get widget gear -o jsonpath='{.metadata.uid}')"
kubectl apply -f - <<EOF
apiVersion: v1
kind: ConfigMap
metadata:
  name: gear-parts
  ownerReferences:
    - {apiVersion: e2e.tug.dev/v1, kind: Widget, name: gear, uid: $uid, controller: true}
data:
  parts: "3"
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: gear-motor
  ownerReferences:
    - {apiVersion: e2e.tug.dev/v1, kind: Widget, name: gear, uid: $uid}
spec:
  replicas: 1
  selector:
    matchLabels: {app: gear-motor}
  template:
    metadata:
      labels: {app: gear-motor}
    spec:
      containers:
        - name: motor
          image: registry.invalid/e2e/motor:1
EOF
widget_ready False PartsMissing "2 of 3 parts are missing"
eventually 60 "gear-motor's image pull to fail" matches 'ErrImagePull|ImagePullBackOff' \
  kubectl get pods -l app=gear-motor -o jsonpath='{.items[*].status.containerStatuses[*].state.waiting.reason}'

echo "--- tug why: the custom resource's condition, and the diagnosis of what it owns"
expect_exit 1 tug why widget/gear
has "$out" '^widget/gear in e2e-why is not ready: PartsMissing: 2 of 3 parts are missing' \
  '^Ready +False +PartsMissing' \
  'pod/gear-motor-[a-z0-9-]+, container motor: (ErrImagePull|ImagePullBackOff)' \
  'cannot pull image registry\.invalid/e2e/motor:1'
expect_exit 1 tug why widget/gear -o json
has "$out" '"healthy": false' '"reason": "PartsMissing"'

echo "--- tug tree: everything the custom resource owns"
expect_exit 0 tug tree widget/gear
has "$out" '^widget/gear .*PartsMissing: 2 of 3 parts are missing' 'cm/gear-parts' 'deploy/gear-motor .*0/1 ready' \
  'rs/gear-motor-[a-z0-9]+ ' 'pod/gear-motor-[a-z0-9-]+ .*(ErrImagePull|ImagePullBackOff)' 'container motor '
expect_exit 0 tug tree widget/gear -o json
has "$out" '"kind": "ConfigMap"' '"kind": "Deployment"' '"kind": "Container"'

echo "--- tug reconcile status: a static pod that holds its lease, and a deployment found in its own namespace"
kcm="$(kubectl -n kube-system get pods -l component=kube-controller-manager -o name)"
expect_exit 0 tug reconcile status "$kcm" -n kube-system
has "$out" "^${kcm} in kube-system is healthy: .*leader" '^lease/kube-controller-manager '
expect_exit 0 tug reconcile status local-path-provisioner
has "$out" '^deploy/local-path-provisioner in local-path-storage is healthy'
expect_exit 0 tug reconcile status "$kcm" -n kube-system -o json
has "$out" '"healthy": true' '"leader": "kube-controller-manager-'

echo "--- tug reconcile explain: who reconciles the deployment, and that nothing reconciles the widget"
expect_exit 1 tug reconcile explain deploy/gear-motor
has "$out" '^  kube-controller-manager \(observed\)' 'deployment-controller reported events about it' \
  'container motor: (ErrImagePull|ImagePullBackOff)'
expect_exit 1 tug reconcile explain widget/gear
has "$out" 'ControllerUnknown' '^kubectl-patch +Update +status'

echo "--- tug reconcile trace and events"
expect_exit 0 tug reconcile trace deploy/gear-motor
has "$out" 'replicaset/gear-motor-[a-z0-9]+ +created, owned by deployment/gear-motor' 'ScalingReplicaSet'
expect_exit 0 tug reconcile trace deploy/gear-motor -o json
has "$out" '"source": "owned"' '"basis": "observed"'
expect_exit 0 tug reconcile events widget/gear
has "$out" 'deploy/gear-motor +ScalingReplicaSet +deployment-controller' 'Warning +pod/gear-motor-[a-z0-9-]+ +Failed +kubelet'

echo "--- why exits 0 once the widget and what it owns are healthy"
kubectl set image deploy/gear-motor motor=nginx:1.27-alpine
kubectl rollout status deploy/gear-motor --timeout=180s
widget_ready True Assembled "every part is in place"
expect_exit 0 tug why widget/gear
has "$out" '^widget/gear in e2e-why is healthy'

echo "--- tug watch --until ready follows a rollout to its end"
kubectl create deployment web --image=nginx:1.27-alpine --port=80
kubectl rollout status deploy/web --timeout=180s
# The new pods take a few seconds to get ready, so that the rollout is
# still going when watch starts.
kubectl patch deploy/web --type=json -p '[
  {"op": "replace", "path": "/spec/template/spec/containers/0/image", "value": "nginx:1.28-alpine"},
  {"op": "add", "path": "/spec/template/spec/containers/0/readinessProbe",
   "value": {"httpGet": {"path": "/", "port": 80}, "initialDelaySeconds": 5, "periodSeconds": 1}}]'
expect_exit 0 tug watch deploy/web --until ready --timeout 3m
has "$out" 'rs/web-[a-z0-9]+ +became ready: 1/1 ready, revision 2' 'deploy/web +became ready'

echo "--- watch gives up on a job that has failed, and on what is not ready in time"
kubectl apply -f "$manifests/failing-job.yaml"
expect_exit 1 tug watch job/fails --until ready --timeout 2m -o json
has "$out" '"object":"job/fails"'
has "$err" 'job/fails has failed'
kubectl create deployment stuck --image=registry.invalid/e2e/stuck:1
expect_exit 1 tug watch deploy/stuck --until ready --timeout 5s
has "$err" 'timed out after 5s'

echo "--- tug reconcile events -f follows new events, and ends cleanly on Ctrl-C"
# command runs the binary rather than the shell hook's tug function, so that
# $! is tug's process and the interrupt reaches it.
command tug reconcile events deploy/web -f > "$work/events.log" 2>&1 &
events=$!
sleep 2
kubectl scale deploy/web --replicas=2
eventually 15 "events -f to show the scale-up" grep -q 'Scaled up replica set web-[a-z0-9]* .*to 2' "$work/events.log"
kill -INT "$events"
wait "$events" || fail "tug reconcile events -f exited $? on Ctrl-C, want 0" "$work/events.log"

echo "--- tug diff -f: a server-side dry run, and kubectl diff's exit codes"
kubectl create namespace e2e-diff
kb create namespace e2e-diff
kubectl apply --server-side --field-manager=e2e -f "$manifests/shop.yaml"
kb apply --server-side --field-manager=e2e -f "$manifests/shop.yaml"
sed 's/greeting: hello/greeting: goodbye/; s/nginx:1.27-alpine/nginx:1.28-alpine/' "$manifests/shop.yaml" > "$work/shop-changed.yaml"
tug ns e2e-diff
expect_exit 0 tug diff -f "$manifests/shop.yaml"
test ! -s "$out" || fail "tug diff wrote a diff of objects that do not differ" "$out"
has "$err" '^configmap/shop-config in e2e-diff: no differences' '^deployment/shop in e2e-diff: no differences'
expect_exit 1 tug diff -f "$work/shop-changed.yaml"
has "$out" '^-  greeting: hello$' '^\+  greeting: goodbye$' '^\+ +- image: nginx:1\.28-alpine$' \
  '^data\.greeting +e2e +tug +conflicts with e2e'
expect_exit 1 tug diff cm/shop-config -f "$work/shop-changed.yaml"
lacks "$out" 'deployment/shop'
expect_exit 1 tug diff -f "$work/shop-changed.yaml" -o json
has "$out" '"same": false' '"origin": "manifest"'
expect_exit 2 tug diff -f "$work/no-such-file.yaml"
expect_exit 0 tug diff deploy/shop --managed-fields
has "$out" '^e2e +Apply ' 'spec\.template\.spec\.containers\[name="shop"\]\.image'

echo "--- diff skips Secrets without reading their data, and shows a new object whole"
cat > "$work/shop-new.yaml" <<'EOF'
apiVersion: v1
kind: Secret
metadata:
  name: shop-secret
  namespace: e2e-diff
stringData:
  token: e2e-diff-secret-3c9d
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: shop-new
  namespace: e2e-diff
data:
  colour: green
EOF
expect_exit 1 tug diff -f "$work/shop-new.yaml"
has "$err" 'skipped secret/shop-secret'
has "$out" '^configmap/shop-new in e2e-diff$' '\(not there yet\)' '^\+  colour: green$'
lacks "$out" 'e2e-diff-secret'
lacks "$err" 'e2e-diff-secret'

echo "--- diff works on a read-only session, and changes nothing"
tug connect "$cluster" --read-only
expect_exit 1 tug diff -f "$work/shop-changed.yaml"
test "$(kubectl -n e2e-diff get configmap shop-config -o jsonpath='{.data.greeting}')" = hello ||
  fail "tug diff changed the live object"
tug connect "$cluster"

echo "--- tug drift: the same on both clusters, then objects that differ or are on one only"
tug ns e2e-diff
expect_exit 0 tug drift --against "$cluster_b"
has "$out" '^No drift: [0-9]+ objects of .*the CRDs and the API versions match'
kb apply --server-side --field-manager=e2e -f "$work/shop-changed.yaml"
kb -n e2e-diff create configmap only-on-b --from-literal=where=b
expect_exit 1 tug drift --against "$cluster_b"
has "$out" "^configmap +only-on-b +only on $cluster_b" '^configmap +shop-config +differs +data\.greeting' \
  '^deployment +shop +differs +spec\.template\.spec\.containers\[name="shop"\]\.image' '^Drift: 3 of [0-9]+ objects'
expect_exit 1 tug drift --against "$cluster_b" configmaps -o json
has "$out" '"state": "only-there"' '"state": "differs"'
lacks "$out" '"kind": "deployment"'
expect_exit 1 tug diff deploy/shop --against "$cluster_b"
has "$out" "^\+\+\+ $cluster_b\$" '^\+ +- image: nginx:1\.28-alpine$'
expect_exit 2 tug drift --against no-such-cluster

echo "--- tug support bundle: a namespace, with no Secret data, env or ConfigMap values or tokens"
kubectl create namespace e2e-bundle
kubectl apply -f "$manifests/bundle.yaml"
kubectl -n e2e-bundle rollout status deploy/api --timeout=180s
eventually 30 "the api container's log" matches 'serving' kubectl -n e2e-bundle logs deploy/api
tug ns e2e-bundle
mkdir "$work/preview"
(cd "$work/preview" && expect_exit 0 tug support bundle namespace/e2e-bundle --preview)
has "$out" '^README\.md ' '^manifest\.json ' '^objects/e2e-bundle/secrets/vault\.yaml .*\(metadata only\)'
has "$err" 'nothing written$'
test -z "$(ls -A "$work/preview")" || fail "support bundle --preview wrote files" "$out"
expect_exit 0 tug support bundle namespace/e2e-bundle --output "$work/bundle.tar.gz" --redact-pattern 'cust-[0-9]+'
has "$out" "^$work/bundle\.tar\.gz\$"
test -n "$(find "$work/bundle.tar.gz" -perm 600)" || fail "the bundle can be read by others than its owner"
mkdir "$work/bundle"
tar -xzf "$work/bundle.tar.gz" -C "$work/bundle"
bundle="$(find "$work/bundle" -mindepth 1 -maxdepth 1 -type d)"
for f in README.md manifest.json doctor.json events.json cluster/version.json \
  objects/e2e-bundle/secrets/vault.yaml objects/e2e-bundle/configmaps/settings.yaml \
  objects/e2e-bundle/deployments.apps/api.yaml why/deployment-api.json tree/deployment-api.json; do
  test -f "$bundle/$f" || fail "the bundle has no $f"
done
api_log="$(find "$bundle/logs" -name api.log)"
has "$api_log" 'api started' 'Bearer <redacted>' 'serving <redacted>'
has "$bundle/objects/e2e-bundle/deployments.apps/api.yaml" '^ +value: <redacted>$'
for private in e2e-secret-data-7f3a "$(printf e2e-secret-data-7f3a | base64 | tr -d '=')" \
  e2e-configmap-value-91c2 e2e-env-value-5b8e e2e-bearer-token-44d1 cust-80417; do
  if grep -rqF -- "$private" "$work/bundle"; then
    fail "the bundle holds $private: $(grep -rlF -- "$private" "$work/bundle" | tr '\n' ' ')"
  fi
done

echo "--- tug check: the baseline pack's advisory findings, and a workload that passes them all"
expect_exit 0 tug check --list
has "$out" '^baseline 1\.0\.0 +resources +warning +advisory'
expect_exit 0 tug check --pack baseline
has "$out" 'against baseline 1\.0\.0: [0-9]+ advisory findings' 'deploy/api, container api: baseline/resources' \
  'deploy/api, container api: baseline/readiness-probe' 'deploy/api, container api: baseline/run-as-non-root'
kubectl create namespace e2e-check
kubectl apply -f "$manifests/checked.yaml"
expect_exit 0 tug check deploy/checked -n e2e-check -o json
has "$out" '"passed": true' '"findings": \[\]'
expect_exit 1 tug check --pack no-such-pack

echo "--- tug api resources and deprecations"
expect_exit 0 tug api resources widget
has "$out" '^widgets +wdg +e2e\.tug\.dev +v1,v1alpha1 +Namespaced +Widget +e2e'
expect_exit 0 tug api resources --group e2e.tug.dev -o name
test "$(cat "$out")" = widgets.e2e.tug.dev || fail "tug api resources --group e2e.tug.dev -o name" "$out"
kubectl apply -f - <<'EOF'
apiVersion: e2e.tug.dev/v1alpha1
kind: Widget
metadata:
  name: legacy
  namespace: e2e-why
spec:
  size: 1
EOF
expect_exit 0 tug api deprecations
has "$out" '^e2e\.tug\.dev/v1alpha1 +Widget +- +e2e\.tug\.dev/v1 +1 +deprecated by its definition'
expect_exit 0 tug api deprecations -o json
has "$out" '"groupVersion": "e2e.tug.dev/v1alpha1"' '"objects": 1'

echo "--- tug crd explain and schema"
expect_exit 0 tug crd explain widget
has "$out" '^Stored versions: +v1$' '^v1alpha1 +yes +- +yes' '^v1 +yes +yes +- +status' \
  '^deprecated v1alpha1: e2e\.tug\.dev/v1alpha1 Widget is deprecated' '^Ready +string +\.status\.conditions'
expect_exit 0 tug crd schema wdg
has "$out" '^  size +integer +yes +Size is how many parts the widget has\.' \
  '^  colour +string +Colour is the widget.s colour\. One of: red, green, blue\. Default: green\.'
expect_exit 0 tug crd schema widgets.e2e.tug.dev --field spec.colour
has "$out" '^spec\.colour \(string\) in Widget e2e\.tug\.dev/v1' 'Default: green'
expect_exit 1 tug crd explain no-such-crd

echo "--- tug profile up and down"
cat > "$work/profile.yaml" <<EOF
cluster: $cluster
namespace: e2e-why
safety: read-only
aliases:
  web-logs: logs web --tail 1
forwards:
  - name: web
    target: web
    ports: ["18085:80"]
  - name: web-deploy
    target: deploy/web
    ports: ["18086:80"]
EOF
tug profile edit e2e < "$work/profile.yaml"
tug ns default
expect_exit 0 tug profile up e2e
has "$out" "^Profile e2e: connected to $cluster, namespace e2e-why, read-only" \
  '^Forwarding localhost:18085 -> 80 to web ' '^Forwarding localhost:18086 -> 80 to deploy/web '
eventually 15 "the forward of web" serves http://localhost:18085
eventually 15 "the forward of deploy/web" serves http://localhost:18086
expect_exit 0 tug status
has "$out" '^namespace: +e2e-why$' '^mode: +read-only$' '^profile: +e2e$'
expect_exit 0 tug profile run web-logs
has "$out" 'GET / HTTP'
expect_exit 1 tug scale web 2
has "$err" 'read-only'
expect_exit 0 tug profile down e2e
has "$out" '^Stopped e2e\.web$' '^Stopped e2e\.web-deploy$'
if serves http://localhost:18085; then
  fail "the profile's forward still answers after tug profile down"
fi

echo "--- tug pf profile starts and stops a profile's forwards without connecting"
tug connect "$cluster"
expect_exit 0 tug pf profile up e2e
eventually 15 "the forward of web" serves http://localhost:18085
expect_exit 0 tug pf profile ls e2e -o json
has "$out" '"name": "e2e.web"' '"name": "e2e.web-deploy"' '"state": "forwarding"' '"inProfile": true'
expect_exit 0 tug status
lacks "$out" '^profile:'
expect_exit 0 tug pf profile down e2e
if serves http://localhost:18085; then
  fail "the profile's forward still answers after tug pf profile down"
fi

echo "--- and starts none of them while another program has one's port"
python3 -m http.server 18086 --bind 127.0.0.1 > /dev/null 2>&1 &
squatter=$!
eventually 15 "a program on port 18086" serves http://127.0.0.1:18086
expect_exit 1 tug pf profile up e2e
kill "$squatter"
has "$err" 'local port in use: e2e\.web-deploy wants 18086'
if serves http://localhost:18085; then
  fail "tug pf profile up started a forward although another's port was taken"
fi
tug daemon stop

echo "--- team catalogs: follow a signed catalog kept in a local git repository"
(
  export TUG_HOME="$work/home-team"
  unset TUG_SESSION TUG_SESSION_PID TUG_SESSION_DIR KUBECONFIG
  eval "$(tug init bash)"
  tug import kubeconfig "$work/kind.yaml"
  server="$(kubectl config view --raw --kubeconfig "$work/kind.yaml" -o jsonpath='{.clusters[0].cluster.server}')"
  key="$(tug team keygen --name e2e)"
  repo="$work/team"
  git init -q -b main "$repo"
  # publish writes version $1 of the catalog, with unlocks of at most $2,
  # signs it and commits it.
  publish() {
    cat > "$repo/team.yaml" <<EOF
apiVersion: tug.team/v1alpha1
kind: TeamCatalog
version: $1
description: tug's end-to-end test
clusters:
  - match: {provider: kubeconfig, server: "$server"}
    name: e2e-prod
    aliases: [ep]
    env: prod
    labels: {team: e2e}
policies:
  - name: prod-read-only
    selector: {env: prod}
    readOnly: true
    maxUnlock: $2
EOF
    tug team sign "$repo/team.yaml" --name e2e
    commit "version $1"
  }
  commit() {
    git -C "$repo" add -A
    git -C "$repo" -c user.name=e2e -c user.email=e2e@example.invalid -c commit.gpgsign=false commit -qm "$1"
  }
  publish 1 10m
  expect_exit 0 tug team verify "$repo/team.yaml" --public-key "$key"
  has "$out" 'is version 1, signed by that key: 0 accounts, 1 cluster, 0 plugin indexes, 1 policy'

  expect_exit 1 tug team add e2e --git "$repo" --public-key "$key"
  has "$err" "cluster e2e-prod: renamed from $cluster; aliases ep; labels env=prod,team=e2e" 'pass --yes'
  expect_exit 0 tug team ls -o name
  test ! -s "$out" || fail "tug team add followed the team without --yes" "$out"
  other="$(tug team keygen --name other)"
  expect_exit 1 tug team add e2e --git "$repo" --public-key "$other" --yes
  has "$err" 'signature does not match'
  expect_exit 0 tug team add e2e --git "$repo" --public-key "$key" --yes
  has "$out" '^Added team e2e, version 1'

  expect_exit 0 tug ls -l team=e2e -o name
  test "$(cat "$out")" = e2e-prod || fail "tug ls -l team=e2e" "$out"
  expect_exit 0 tug connect ep
  has "$out" '^Connected to e2e-prod, .*read-only'
  expect_exit 1 tug scale web 2 -n e2e-why
  has "$err" 'e2e-prod is read-only'
  expect_exit 1 tug unlock 1h
  has "$err" 'at most 10m, as policy prod-read-only of team e2e says'

  echo "--- a tampered catalog is refused, and the last good version stays"
  sed 's/maxUnlock: 10m/maxUnlock: 24h/' "$repo/team.yaml" > "$work/tampered.yaml"
  mv "$work/tampered.yaml" "$repo/team.yaml"
  commit tampered
  expect_exit 1 tug team verify "$repo/team.yaml" --public-key "$key"
  expect_exit 1 tug team sync
  has "$err" 'signature does not match' 'keeping version 1'
  expect_exit 1 tug unlock 1h
  has "$err" 'at most 10m'

  echo "--- a new version waits for review, and applies with --yes"
  publish 2 5m
  expect_exit 1 tug team sync
  has "$err" 'version 1 → version 2' 'unlocks up to 10m → env=prod: read-only, unlocks up to 5m' 'waiting for review'
  expect_exit 0 tug team sync --yes
  has "$out" 'version 2 applied'
  expect_exit 1 tug unlock 8m
  has "$err" 'at most 5m'
  expect_exit 0 tug team remove e2e
  has "$out" '^Removed team e2e'
)
