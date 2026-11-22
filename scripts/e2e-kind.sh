#!/usr/bin/env bash
# End-to-end test against kind clusters. Each suite is a file in scripts/e2e/:
#
#   core      doctor, the conformance suite, the local provider, passthrough,
#             daily commands, tugd, named port-forwards, the read-only guard
#             and the audit log
#   commands  why, tree, reconcile, watch, diff, drift, support bundle, check,
#             api, crd, profiles and team catalogs
#   flux      gitops, why and reconcile with Flux installed
#   argocd    gitops with Argo CD installed
#
# Usage: TUG_BIN=bin/tug scripts/e2e-kind.sh [suite...]
#
# With no suite named, every suite runs, in the order above. The suites run
# with an isolated TUG_HOME and the encrypted file keyring, so they never
# touch the real keychain. They expect new clusters: they create namespaces
# and install CRDs, Flux and Argo CD. commands also needs a second cluster,
# KIND_CLUSTER_B, to compare with. flux and argocd download their releases,
# and what they deliver, from GitHub.
set -euo pipefail

: "${TUG_BIN:?set TUG_BIN to the tug binary}"
: "${KIND_CLUSTER:=tug-e2e}"
: "${KIND_CLUSTER_B:=$KIND_CLUSTER-b}"
: "${FLUX_VERSION:=v2.10.0}"
: "${ARGOCD_VERSION:=v3.6.1}"

e2e="$(cd "$(dirname "$0")/e2e" && pwd)"
manifests="$e2e/manifests"
if [ $# -eq 0 ]; then
  set -- core commands flux argocd
fi
for suite in "$@"; do
  if [ ! -f "$e2e/$suite.sh" ]; then
    echo "no suite named $suite; the suites are core, commands, flux and argocd" >&2
    exit 2
  fi
done

TUG_BIN="$(cd "$(dirname "$TUG_BIN")" && pwd)/$(basename "$TUG_BIN")"
[ -x "$TUG_BIN" ] || { echo "TUG_BIN $TUG_BIN is not an executable" >&2; exit 2; }

work="$(mktemp -d)"
# The suites run tug by name, so put TUG_BIN first on PATH as tug, whatever
# its file is called and whatever other tug is installed.
mkdir "$work/bin"
ln -s "$TUG_BIN" "$work/bin/tug"
PATH="$work/bin:$PATH"
# cleanup stops what a failed check may leave running: background jobs such
# as port-forwards, and tugd.
cleanup() {
  local pids
  pids="$(jobs -p)"
  if [ -n "$pids" ]; then
    # shellcheck disable=SC2086 # one argument per job
    kill $pids 2> /dev/null || true
  fi
  tug daemon stop > /dev/null 2>&1 || true
  rm -rf "$work"
}
trap cleanup EXIT

export TUG_HOME="$work/home"
export TUG_KEYRING_BACKEND=file
export TUG_KEYRING_PASSPHRASE=e2e
export NO_COLOR=1
unset TUG_SESSION TUG_SESSION_PID KUBECONFIG

# shellcheck source=e2e/lib.sh
source "$e2e/lib.sh"

# kind's kubeconfig embeds a client key; tug must move it out of the files it writes.
kind get kubeconfig --name "$KIND_CLUSTER" > "$work/kind.yaml"
cluster="kind-$KIND_CLUSTER"
if [[ " $* " == *" commands "* ]]; then
  if ! kind get kubeconfig --name "$KIND_CLUSTER_B" > "$work/kind-b.yaml"; then
    echo "the commands suite compares two clusters; create the second with: kind create cluster --name $KIND_CLUSTER_B" >&2
    exit 1
  fi
  cluster_b="kind-$KIND_CLUSTER_B"
fi

eval "$(tug init bash)"

tug import kubeconfig "$work/kind.yaml"
if [ -f "$work/kind-b.yaml" ]; then
  tug import kubeconfig "$work/kind-b.yaml"
fi
tug ls

for suite in "$@"; do
  echo "=== $suite"
  tug connect "$cluster"
  # shellcheck source=/dev/null
  source "$e2e/$suite.sh"
done

echo "PASS"
