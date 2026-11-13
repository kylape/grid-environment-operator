#!/bin/sh
# One-time (per cluster) dev setup: CRD, service-account RBAC, and a minted
# kubeconfig for running the operator without admin credentials.
#
# The admin kubeconfig is used only for cluster-scoped setup (CRD,
# ClusterRole, binding, token minting) — never to run the operator or the
# dev loop.
#
# Usage: tools/dev-setup.sh <admin-kubeconfig> [token-duration]
#   token-duration defaults to 24h. Re-run whenever the CRD or RBAC
#   changes, or when the token expires.
set -eu

ADMIN="${1:?usage: dev-setup.sh <admin-kubeconfig> [token-duration]}"
DURATION="${2:-24h}"

ROOT=$(cd "$(dirname "$0")/.." && pwd)
OUT="$ROOT/.dev-kubeconfig"

# Cluster-scoped setup: CRD first, then the service account, generated
# role, and binding.
kubectl --kubeconfig "$ADMIN" apply -f "$ROOT/config/crd"
kubectl --kubeconfig "$ADMIN" apply -f "$ROOT/config/rbac"

# Mint the scoped kubeconfig the operator and dev loop run with.
"$ROOT/tools/dev-kubeconfig.sh" "$ADMIN" "$DURATION" > "$OUT"
chmod 600 "$OUT"

# Sanity-check the minted identity against the permissions reconciliation
# actually needs — catch RBAC mistakes at setup, not mid-reconcile.
for check in \
	"create aigrids.aigrid.llm.redhat.com" \
	"create llmworkloadenvironments.llm.redhat.com" \
	"create networkpolicies.networking.k8s.io" \
	"create deployments.apps" \
	"create services" \
	"get secrets"; do
	if ! kubectl --kubeconfig "$OUT" auth can-i $check >/dev/null 2>&1; then
		echo "ERROR: service account cannot $check — check config/rbac" >&2
		exit 1
	fi
done

echo "Setup complete."
echo "  Operator:  KUBECONFIG=$OUT go run ./cmd/manager"
echo "  Dev loop:  kubectl --kubeconfig $OUT -n <namespace-with-pull-secret> apply -f config/samples/"
