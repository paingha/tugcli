#!/usr/bin/env bash
# End-to-end test against a kind cluster (Phase 0 exit criterion: `tug get pods`
# passes through to a kind cluster). Runs with an isolated TUG_HOME and the
# encrypted file keyring, so it never touches the real keychain.
set -euo pipefail

: "${TUG_BIN:?set TUG_BIN to the tug binary}"
: "${KIND_CLUSTER:=tug-e2e}"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

export TUG_HOME="$work/home"
export TUG_KEYRING_BACKEND=file
export TUG_KEYRING_PASSPHRASE=e2e
export NO_COLOR=1
unset TUG_SESSION TUG_SESSION_PID KUBECONFIG

# kind's kubeconfig embeds a client key; tug must move it out of the files it writes.
kind get kubeconfig --name "$KIND_CLUSTER" > "$work/kind.yaml"

PATH="$(dirname "$TUG_BIN"):$PATH"
eval "$(tug init bash)"

tug import kubeconfig "$work/kind.yaml"
tug ls
tug connect "kind-$KIND_CLUSTER"

echo "--- doctor finds nothing wrong with a healthy cluster"
tug doctor

echo "--- the built-in provider passes the plugin conformance suite"
tug plugin test kubeconfig --account kind

echo "--- passthrough"
tug get pods -A
tug get nodes -o name

echo "--- direct kubectl follows the session"
kubectl get ns default -o name

echo "--- tug ns uses the in-process client"
tug ns kube-system
test "$(tug status -o json | grep -c '"namespace": "kube-system"')" = 1

echo "--- no secrets in files tug wrote"
key="$(kubectl config view --raw --kubeconfig "$work/kind.yaml" -o jsonpath='{.users[0].user.client-key-data}')"
if grep -rqF "$key" "$TUG_HOME/data/catalog.yaml" "$KUBECONFIG"; then
  echo "FAIL: client key found in a file tug wrote" >&2
  exit 1
fi

echo "--- daily commands on a sample workload"
tug ns default
kubectl create deployment web --image=nginx:1.27-alpine --port=80
kubectl rollout status deploy/web --timeout=180s

tug logs web --tail 5
tug sh web -- nginx -v

tug pf web 18080:80 > "$work/pf.log" 2>&1 &
pf_pid=$!
for _ in $(seq 1 30); do
  curl -fsS http://localhost:18080 > "$work/page.html" 2>/dev/null && break
  sleep 1
done
kill "$pf_pid" 2>/dev/null || true
grep -q "Welcome to nginx" "$work/page.html" || { echo "FAIL: port-forward"; cat "$work/pf.log"; exit 1; }

tug scale web 2
kubectl rollout status deploy/web --timeout=180s
test "$(kubectl get deploy web -o jsonpath='{.spec.replicas}')" = 2

tug restart web
kubectl rollout status deploy/web --timeout=180s
test -n "$(kubectl get deploy web -o jsonpath='{.spec.template.metadata.annotations.kubectl\.kubernetes\.io/restartedAt}')"

tug rollback web
kubectl rollout status deploy/web --timeout=180s
test -z "$(kubectl get deploy web -o jsonpath='{.spec.template.metadata.annotations.kubectl\.kubernetes\.io/restartedAt}')"

echo "--- tugd: plain kubectl through the local API proxy"
tug connect "kind-$KIND_CLUSTER" --proxy
kubectl get pods -A > /dev/null
kubectl exec deploy/web -- nginx -v
kubectl port-forward deploy/web 18082:80 > "$work/pf-proxy.log" 2>&1 &
pf_proxy=$!
for _ in $(seq 1 30); do
  curl -fsS http://localhost:18082 > "$work/page-proxy.html" 2>/dev/null && break
  sleep 1
done
kill "$pf_proxy" 2>/dev/null || true
grep -q "Welcome to nginx" "$work/page-proxy.html" || { echo "FAIL: port-forward through tugd"; cat "$work/pf-proxy.log"; exit 1; }
timeout 5 kubectl logs -f deploy/web --tail 1 > "$work/logs-proxy.txt" || true
test -s "$work/logs-proxy.txt" || { echo "FAIL: logs -f through tugd"; exit 1; }

echo "--- tugd refuses kubectl writes on a read-only session"
tug connect "kind-$KIND_CLUSTER" --proxy --read-only
if kubectl delete ns default --wait=false 2> "$work/delete-proxy.err"; then
  echo "FAIL: kubectl delete allowed through tugd on a read-only session" >&2
  exit 1
fi
grep -q "read-only" "$work/delete-proxy.err" || { echo "FAIL: unexpected refusal"; cat "$work/delete-proxy.err"; exit 1; }
tug daemon stop

echo "--- read-only guard"
tug connect "kind-$KIND_CLUSTER" --read-only
if tug delete ns default --wait=false 2>/dev/null; then
  echo "FAIL: delete allowed on a read-only session" >&2
  exit 1
fi
if tug restart web 2>/dev/null; then
  echo "FAIL: restart allowed on a read-only session" >&2
  exit 1
fi
if tug sh web -- true 2>/dev/null; then
  echo "FAIL: exec allowed on a read-only session" >&2
  exit 1
fi
tug logs web --tail 1 # reads still work

echo "PASS"
