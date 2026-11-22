# The core suite: doctor, the conformance suite, the local provider,
# passthrough, daily commands, tugd, named port-forwards, the read-only guard
# and the audit log. Sourced by scripts/e2e-kind.sh.

echo "--- doctor finds nothing wrong with a healthy cluster"
tug doctor

echo "--- the built-in provider passes the plugin conformance suite"
tug plugin test kubeconfig --account kind

echo "--- the local provider finds the cluster through the kind CLI"
(
  export TUG_HOME="$work/home-local"
  tug import local --account kind
  tug ls -l distro=kind -o name | grep -qx "kind-$KIND_CLUSTER"
  tug plugin test local --account kind
)

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

echo "--- named port-forwards survive a pod restart"
tug pf web 18083:80 --name web
for _ in $(seq 1 30); do
  curl -fsS http://localhost:18083 > "$work/named1.html" 2>/dev/null && break
  sleep 1
done
grep -q "Welcome to nginx" "$work/named1.html" || { echo "FAIL: named port-forward"; tug pf ls; exit 1; }
kubectl delete pod -l app=web --wait=false
kubectl rollout status deploy/web --timeout=180s
for _ in $(seq 1 60); do
  curl -fsS http://localhost:18083 > "$work/named2.html" 2>/dev/null && break
  sleep 1
done
grep -q "Welcome to nginx" "$work/named2.html" || { echo "FAIL: named port-forward after a pod restart"; tug pf ls; exit 1; }
tug pf ls | grep -q "^web " || { echo "FAIL: pf ls"; exit 1; }
tug pf stop web

echo "--- tugd refuses kubectl writes on a read-only session"
tug connect "kind-$KIND_CLUSTER" --proxy --read-only
if kubectl delete ns default --wait=false 2> "$work/delete-proxy.err"; then
  echo "FAIL: kubectl delete allowed through tugd on a read-only session" >&2
  exit 1
fi
grep -q "read-only" "$work/delete-proxy.err" || { echo "FAIL: unexpected refusal"; cat "$work/delete-proxy.err"; exit 1; }

echo "--- tugd on a read-only session: port-forward works, exec does not"
if kubectl exec deploy/web -- true 2> "$work/exec-proxy.err"; then
  echo "FAIL: kubectl exec allowed through tugd on a read-only session" >&2
  exit 1
fi
grep -q "read-only" "$work/exec-proxy.err" || { echo "FAIL: unexpected exec refusal"; cat "$work/exec-proxy.err"; exit 1; }
kubectl port-forward deploy/web 18084:80 > "$work/pf-ro.log" 2>&1 &
pf_ro=$!
for _ in $(seq 1 30); do
  curl -fsS http://localhost:18084 > "$work/page-ro.html" 2>/dev/null && break
  sleep 1
done
kill "$pf_ro" 2>/dev/null || true
grep -q "Welcome to nginx" "$work/page-ro.html" || { echo "FAIL: port-forward through tugd on a read-only session"; cat "$work/pf-ro.log"; exit 1; }

echo "--- a session survives tugd restarting, and never goes around it"
tug daemon stop
kubectl get ns default -o name   # the credential plugin starts tugd again
tug daemon status | grep -q "sessions:  1" || { echo "FAIL: tugd did not come back for the session"; tug daemon status; exit 1; }

echo "--- the audit log has the session, its refusals and tugd's writes"
tug audit -o json > "$work/audit.json"
grep -Eq '"intent": *"kubectl exec"' "$work/audit.json" || { echo "FAIL: no kubectl exec in the audit log"; cat "$work/audit.json"; exit 1; }
grep -Eq '"status": *101' "$work/audit.json" || { echo "FAIL: no upgraded exec in the audit log"; cat "$work/audit.json"; exit 1; }
test "$(tug audit --event refused -o json | grep -c '"reason": "read-only"')" -ge 2 || { echo "FAIL: refusals not recorded"; tug audit; exit 1; }
tug audit --event session-start -o json | grep -q '"proxy": true' || { echo "FAIL: proxied session start not recorded"; tug audit; exit 1; }
if grep -q "nginx -v" "$work/audit.json"; then
  echo "FAIL: the audit log holds an exec's command" >&2
  exit 1
fi
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
