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

echo "--- read-only guard"
tug connect "kind-$KIND_CLUSTER" --read-only
if tug delete ns default --wait=false 2>/dev/null; then
  echo "FAIL: delete allowed on a read-only session" >&2
  exit 1
fi

echo "PASS"
