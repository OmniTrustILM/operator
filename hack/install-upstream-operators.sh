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
KEYCLOAK_VERSION="26.6.3"

CERT_MANAGER_URL="https://github.com/cert-manager/cert-manager/releases/download/${CERT_MANAGER_VERSION}/cert-manager.yaml"
CNPG_URL="https://github.com/cloudnative-pg/cloudnative-pg/releases/download/v${CNPG_VERSION}/cnpg-${CNPG_VERSION}.yaml"
RABBITMQ_CLUSTER_URL="https://github.com/rabbitmq/cluster-operator/releases/download/${RABBITMQ_CLUSTER_VERSION}/cluster-operator.yml"
RABBITMQ_TOPOLOGY_URL="https://github.com/rabbitmq/messaging-topology-operator/releases/download/${RABBITMQ_TOPOLOGY_VERSION}/messaging-topology-operator-with-certmanager.yaml"
KEYCLOAK_BASE="https://raw.githubusercontent.com/keycloak/keycloak-k8s-resources/${KEYCLOAK_VERSION}/kubernetes"

# CRD + operator-Deployment coordinates used by the verify pass: "label|crd|namespace/deployment".
COMPONENTS=(
  "cert-manager|certificates.cert-manager.io|cert-manager/cert-manager-webhook"
  "CloudNativePG (PostgreSQL)|clusters.postgresql.cnpg.io|cnpg-system/cnpg-controller-manager"
  "RabbitMQ Cluster Operator|rabbitmqclusters.rabbitmq.com|rabbitmq-system/rabbitmq-cluster-operator"
  "RabbitMQ Messaging Topology Operator|vhosts.rabbitmq.com|rabbitmq-system/messaging-topology-operator"
  "Keycloak Operator|keycloaks.k8s.keycloak.org|keycloak/keycloak-operator"
)

wait_avail() { # namespace deployment
  echo "  waiting for deployment/$2 in $1 to become Available..."
  kubectl wait --for=condition=Available "deployment/$2" -n "$1" --timeout=5m
}

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

  echo "==> Keycloak Operator ${KEYCLOAK_VERSION} (installed as published in 'keycloak', set to watch all namespaces)"
  kubectl create namespace keycloak --dry-run=client -o yaml | kubectl apply -f -
  for f in keycloaks.k8s.keycloak.org-v1.yml keycloakrealmimports.k8s.keycloak.org-v1.yml kubernetes.yml; do
    kubectl apply -n keycloak -f "${KEYCLOAK_BASE}/${f}"
  done
  # The Keycloak Operator ships NAMESPACE-SCOPED (it watches only its own 'keycloak' namespace),
  # unlike the cluster-scoped CloudNativePG / RabbitMQ operators. Rather than fork the published
  # manifest, install it AS-IS and configure it to watch ALL namespaces (the documented
  # Java-Operator-SDK setting) so a managed Platform in ANY namespace has its Keycloak CR
  # reconciled by this single operator. Watching other namespaces requires ALL of the operator's
  # RBAC cluster-wide — both its controller ClusterRoles AND the OPERATIONAL permissions the
  # published manifest only grants via a NAMESPACED Role in 'keycloak' (pods, pods/log, jobs,
  # services, secrets, statefulsets, ingresses, servicemonitors). Without the operational grant
  # the operator 403s listing pods in other namespaces and never builds the Keycloak StatefulSet.
  echo "    configuring the Keycloak Operator to watch all namespaces"
  # (a) the two controller ClusterRoles, bound cluster-wide:
  for crb in "keycloak-operator-allns-controller:keycloakcontroller-cluster-role" \
             "keycloak-operator-allns-realmimport:keycloakrealmimportcontroller-cluster-role"; do
    kubectl create clusterrolebinding "${crb%%:*}" --clusterrole="${crb##*:}" \
      --serviceaccount=keycloak:keycloak-operator --dry-run=client -o yaml | kubectl apply -f -
  done
  # (b) the operational permissions as a cluster-wide ClusterRole. Rules mirror the published
  #     keycloak-operator-role (Keycloak 26.6.x) — re-check on a Keycloak Operator version bump.
  kubectl apply -f - <<'RBAC'
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: keycloak-operator-allns-role
rules:
  - apiGroups: ["apps"]
    resources: ["statefulsets"]
    verbs: ["get", "list", "watch", "create", "delete", "patch", "update"]
  - apiGroups: [""]
    resources: ["configmaps"]
    verbs: ["get", "list", "watch"]
  - apiGroups: [""]
    resources: ["secrets", "services"]
    verbs: ["get", "list", "watch", "create", "delete", "patch", "update"]
  - apiGroups: [""]
    resources: ["pods"]
    verbs: ["list"]
  - apiGroups: [""]
    resources: ["pods/log"]
    verbs: ["get"]
  - apiGroups: ["batch"]
    resources: ["jobs"]
    verbs: ["get", "list", "watch", "create", "delete", "patch", "update"]
  - apiGroups: ["networking.k8s.io"]
    resources: ["ingresses"]
    verbs: ["get", "list", "watch", "create", "delete", "patch", "update"]
  - apiGroups: ["monitoring.coreos.com"]
    resources: ["servicemonitors"]
    verbs: ["get", "list", "watch", "create", "delete", "patch", "update"]
RBAC
  kubectl create clusterrolebinding keycloak-operator-allns-role \
    --clusterrole=keycloak-operator-allns-role --serviceaccount=keycloak:keycloak-operator \
    --dry-run=client -o yaml | kubectl apply -f -
  # (c) flip the watch scope to all namespaces on both controllers.
  kubectl set env deployment/keycloak-operator -n keycloak \
    QUARKUS_OPERATOR_SDK_CONTROLLERS_KEYCLOAKCONTROLLER_NAMESPACES=JOSDK_ALL_NAMESPACES \
    QUARKUS_OPERATOR_SDK_CONTROLLERS_KEYCLOAKREALMIMPORTCONTROLLER_NAMESPACES=JOSDK_ALL_NAMESPACES
  wait_avail keycloak keycloak-operator
}

verify() {
  echo ""
  echo "==> Upstream-operator readiness"
  local ok=0 missing=0
  printf "  %-38s %-10s %s\n" "COMPONENT" "CRD" "OPERATOR"
  for entry in "${COMPONENTS[@]}"; do
    IFS='|' read -r label crd nsdep <<<"${entry}"
    local ns="${nsdep%%/*}" dep="${nsdep##*/}" crd_state dep_state
    if kubectl get crd "${crd}" >/dev/null 2>&1; then crd_state="present"; else crd_state="MISSING"; fi
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
