#!/bin/sh
# Mint a kubeconfig for the grid-environment-operator service account.
#
# The operator never runs as cluster-admin: apply config/rbac with an
# admin kubeconfig once, then use this script to mint a scoped kubeconfig
# for devcontainer runs. The admin kubeconfig is used only to mint the
# token, never to run the operator.
#
# Usage: tools/dev-kubeconfig.sh <admin-kubeconfig> [duration]
#   duration defaults to 24h.
set -eu

ADMIN_KUBECONFIG="${1:?usage: dev-kubeconfig.sh <admin-kubeconfig> [duration]}"
DURATION="${2:-24h}"

NAMESPACE=grid-environment-operator
SERVICE_ACCOUNT=grid-environment-operator

TOKEN=$(kubectl --kubeconfig "$ADMIN_KUBECONFIG" create token \
	"$SERVICE_ACCOUNT" -n "$NAMESPACE" --duration="$DURATION")

SERVER=$(kubectl --kubeconfig "$ADMIN_KUBECONFIG" config view \
	-o jsonpath='{.clusters[0].cluster.server}')

# Prefer the admin kubeconfig's embedded CA (--flatten resolves file
# references); insecure admin kubeconfigs carry none, so fall back to the
# cluster's published root CA, which signs the API endpoint's serving
# certificate.
CA=$(kubectl --kubeconfig "$ADMIN_KUBECONFIG" config view --flatten \
	-o jsonpath='{.clusters[0].cluster.certificate-authority-data}')
if [ -z "$CA" ]; then
	CA=$(kubectl --kubeconfig "$ADMIN_KUBECONFIG" -n default get cm \
		kube-root-ca.crt -o jsonpath='{.data.ca\.crt}' | base64 -w0)
fi

cat <<EOF
apiVersion: v1
kind: Config
clusters:
- name: $SERVICE_ACCOUNT
  cluster:
    server: $SERVER
    certificate-authority-data: $CA
users:
- name: $SERVICE_ACCOUNT
  user:
    token: $TOKEN
contexts:
- name: $SERVICE_ACCOUNT
  context:
    cluster: $SERVICE_ACCOUNT
    user: $SERVICE_ACCOUNT
current-context: $SERVICE_ACCOUNT
EOF
