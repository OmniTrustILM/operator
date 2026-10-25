#!/usr/bin/env bash
#
# install-upstream-operators.sh — install (or verify) the upstream operators the managed
# ILM Platform depends on, pinned to the versions the operator's e2e validates against.
#
#   ./hack/install-upstream-operators.sh           # install everything, then verify (idempotent)
#   ./hack/install-upstream-operators.sh verify     # only report what is present/ready
#
# The ILM operator DETECTS each of these at runtime and waits (a non-fatal condition) if one is
# missing, so order/timing is not critical — but a managed Platform only reaches Available once
# its dependencies (CloudNativePG, the RabbitMQ Cluster + Messaging Topology operators, the
# Keycloak Operator) are installed. cert-manager is installed FIRST because the Messaging
# Topology Operator's admission webhook and the platform's internal-CA edge both need it.
set -euo pipefail

# Pinned versions — keep in sync with test/utils/utils.go (the versions the e2e installs).
CERT_MANAGER_VERSION="v1.20.2"
CNPG_VERSION="1.29.1"
RABBITMQ_CLUSTER_VERSION="v2.21.0"
RABBITMQ_TOPOLOGY_VERSION="v1.19.2"
KEYCLOAK_VERSION="26.8.1"

CERT_MANAGER_URL="https://github.com/cert-manager/cert-manager/releases/download/${CERT_MANAGER_VERSION}/cert-manager.yaml"
CNPG_URL="https://github.com/cloudnative-pg/cloudnative-pg/releases/download/v${CNPG_VERSION}/cnpg-${CNPG_VERSION}.yaml"
RABBITMQ_CLUSTER_URL="https://github.com/rabbitmq/cluster-operator/releases/download/${RABBITMQ_CLUSTER_VERSION}/cluster-operator.yml"
RABBITMQ_TOPOLOGY_URL="https://github.com/rabbitmq/messaging-topology-operator/releases/download/${RABBITMQ_TOPOLOGY_VERSION}/messaging-topology-operator-with-certmanager.yaml"
KEYCLOAK_BASE="https://github.com/keycloak/keycloak-k8s-resources/kubernetes/cluster-wide?ref=${KEYCLOAK_VERSION}"

# CRDs + operator-Deployment coordinates used by the verify pass: "label|crds|namespace/deployment".
COMPONENTS=(
  "cert-manager|certificates.cert-manager.io|cert-manager/cert-manager-webhook"
  "CloudNativePG (PostgreSQL)|clusters.postgresql.cnpg.io|cnpg-system/cnpg-controller-manager"
  "RabbitMQ Cluster Operator|rabbitmqclusters.rabbitmq.com|rabbitmq-system/rabbitmq-cluster-operator"
  "RabbitMQ Messaging Topology Operator|vhosts.rabbitmq.com|rabbitmq-system/messaging-topology-operator"
  "Keycloak Operator|keycloaks.k8s.keycloak.org keycloakrealmimports.k8s.keycloak.org keycloakoidcclients.k8s.keycloak.org keycloaksamlclients.k8s.keycloak.org|keycloak/keycloak-operator"
)

wait_avail() { # namespace deployment
  echo "  waiting for deployment/$2 in $1 to become Available..."
  kubectl wait --for=condition=Available "deployment/$2" -n "$1" --timeout=5m
}

install_keycloak() (
  # Keep the existing installation namespace while using upstream's cluster-wide RBAC and
  # watch configuration for all four controllers. The subshell cleans up the overlay on error.
  keycloak_overlay="$(mktemp -d)"
  trap 'rm -rf -- "${keycloak_overlay}"' EXIT
  cat >"${keycloak_overlay}/kustomization.yaml" <<EOF
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
namespace: keycloak
resources:
  - ${KEYCLOAK_BASE}
EOF
  kubectl create namespace keycloak --dry-run=client -o yaml | kubectl apply -f -
  kubectl apply --server-side -k "${keycloak_overlay}"
  kubectl rollout status deployment/keycloak-operator -n keycloak --timeout=5m

  # These grants were created by older versions of this script. Upstream now supplies them.
  kubectl delete clusterrolebinding keycloak-operator-allns-controller \
    keycloak-operator-allns-realmimport keycloak-operator-allns-role --ignore-not-found
  kubectl delete clusterrole keycloak-operator-allns-role --ignore-not-found
)

install() {
  echo "==> cert-manager ${CERT_MANAGER_VERSION} (first — the Topology webhook + the internal-CA edge need it)"
  kubectl apply -f "${CERT_MANAGER_URL}"
  wait_avail cert-manager cert-manager-webhook

  echo "==> CloudNativePG ${CNPG_VERSION} (managed PostgreSQL)"
  kubectl apply --server-side -f "${CNPG_URL}"
  wait_avail cnpg-system cnpg-controller-manager

  echo "==> RabbitMQ Cluster Operator ${RABBITMQ_CLUSTER_VERSION} (managed broker)"
  kubectl apply -f "${RABBITMQ_CLUSTER_URL}"
  wait_avail rabbitmq-system rabbitmq-cluster-operator

  echo "==> RabbitMQ Messaging Topology Operator ${RABBITMQ_TOPOLOGY_VERSION} (vhost/users/queues — needs cert-manager)"
  kubectl apply -f "${RABBITMQ_TOPOLOGY_URL}"
  wait_avail rabbitmq-system messaging-topology-operator

  echo "==> Keycloak Operator ${KEYCLOAK_VERSION} (watching all namespaces from 'keycloak')"
  install_keycloak
}

verify() {
  echo ""
  echo "==> Upstream-operator readiness"
  local ok=0 missing=0
  local -a crds
  printf "  %-38s %-10s %s\n" "COMPONENT" "CRD" "OPERATOR"
  for entry in "${COMPONENTS[@]}"; do
    IFS='|' read -r label crd nsdep <<<"${entry}"
    local ns="${nsdep%%/*}" dep="${nsdep##*/}" crd_state dep_state
    read -r -a crds <<<"${crd}"
    if kubectl get crd "${crds[@]}" >/dev/null 2>&1; then crd_state="present"; else crd_state="MISSING"; fi
    if kubectl get deployment "${dep}" -n "${ns}" >/dev/null 2>&1 &&
       [ "$(kubectl get deployment "${dep}" -n "${ns}" -o jsonpath='{.status.availableReplicas}' 2>/dev/null)" != "" ] &&
       [ "$(kubectl get deployment "${dep}" -n "${ns}" -o jsonpath='{.status.availableReplicas}')" -ge 1 ] 2>/dev/null; then
      dep_state="ready"
    else
      dep_state="NOT READY"
    fi
    printf "  %-38s %-10s %s\n" "${label}" "${crd_state}" "${dep_state}"
    if [ "${crd_state}" = "present" ] && [ "${dep_state}" = "ready" ]; then ok=$((ok+1)); else missing=$((missing+1)); fi
  done
  echo ""
  if [ "${missing}" -eq 0 ]; then
    echo "  ✅ all ${ok} upstream operators are installed and ready"
  else
    echo "  ❌ ${missing} of $((ok+missing)) upstream operators are missing or not ready — run without 'verify' to install"
    return 1
  fi
}

case "${1:-install}" in
  verify) verify ;;
  install) install; verify ;;
  *) echo "usage: $0 [install|verify]" >&2; exit 2 ;;
esac
