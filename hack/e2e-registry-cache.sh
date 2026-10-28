#!/usr/bin/env bash
#
# e2e-registry-cache.sh — opt-in pull-through image cache for the e2e Kind clusters.
#
# Every e2e run starts on a FRESH Kind cluster (setup-test-e2e in the Makefile), so every run
# downloads every component image again into the new node. This script keeps one small registry
# container per upstream registry running on the host — the official `registry` image in proxy
# (pull-through) mode, its layers in a named Docker volume — and points a Kind node's containerd at
# them. The cluster is still brand new each run; only the image bytes outlive it.
#
#   ./hack/e2e-registry-cache.sh attach <cluster>   # start the mirrors, wire <cluster>'s node to them
#   ./hack/e2e-registry-cache.sh status             # show each mirror and how much it has cached
#   ./hack/e2e-registry-cache.sh down               # remove the mirror containers, keep the cached layers
#   ./hack/e2e-registry-cache.sh purge              # remove the mirror containers AND the cached layers
#
# Normally driven by the Makefile (`make test-e2e-managed E2E_REGISTRY_CACHE=1`); see CONTRIBUTING.md.
#
# The cache is an optimisation, so a mirror that cannot be started, or goes down later, costs time
# and never fails a run: containerd pulls that registry directly, as it does any registry not listed
# below. The one refusal is a cluster that was not created from hack/kind-registry-cache.yaml, which
# cannot use mirrors at all. The mirrors hold no credentials and cache only what a registry serves
# anonymously.
set -euo pipefail

# The official image, in its maintained 3.x line (the 2.x line ended at 2.8.3). Set
# E2E_REGISTRY_CACHE_IMAGE to run another one, e.g. a copy in an internal registry.
IMAGE="${E2E_REGISTRY_CACHE_IMAGE:-registry:3.1.2}"
NAME_PREFIX="ilm-operator-e2e-mirror"
# The Docker network Kind puts its nodes on (the one Kind itself reads this variable for).
KIND_NETWORK="${KIND_EXPERIMENTAL_DOCKER_NETWORK:-kind}"
# Records what a mirror was created from, so a changed image or upstream replaces it.
CONFIG_LABEL="com.otilm.e2e-registry-cache"
# containerd reads one <registry>/hosts.toml from here once the cluster has been created from
# hack/kind-registry-cache.yaml, which switches that on.
CERTS_DIR="/etc/containerd/certs.d"

# "registry as containerd names it|upstream URL its mirror proxies". An image from a registry that
# is not listed is pulled directly, as without the cache.
REGISTRIES=(
  "docker.io|https://registry-1.docker.io"
  "quay.io|https://quay.io"
  "ghcr.io|https://ghcr.io"
  "hub.omnitrustregistry.com|https://hub.omnitrustregistry.com"
)

die() {
  echo "e2e-registry-cache: $*" >&2
  exit 1
}

warn() {
  echo "e2e-registry-cache: WARNING: $*" >&2
}

# mirror_name <registry>: docker.io -> ilm-operator-e2e-mirror-docker-io (container and volume).
mirror_name() {
  echo "${NAME_PREFIX}-${1//./-}"
}

# The helpers below are called from conditions (`|| true`, `if`), where the shell ignores `set -e`
# inside them, so each reports failure through its return status instead.

# start_mirror <registry> <upstream>: create the mirror, start it if it is stopped, or replace it
# when its image or upstream changed. The cached layers live in the volume, so a replaced mirror
# keeps them.
start_mirror() {
  local name want have
  name="$(mirror_name "$1")"
  want="${IMAGE} $2"
  if have="$(docker inspect -f "{{index .Config.Labels \"${CONFIG_LABEL}\"}}" "${name}" 2>/dev/null)"; then
    if [ "${have}" = "${want}" ]; then
      [ "$(docker inspect -f '{{.State.Running}}' "${name}")" = "true" ] || docker start "${name}" >/dev/null
      return
    fi
    docker rm -f "${name}" >/dev/null || return 1
  fi
  # TTL 0: the default drops a layer a week after it was fetched, which would turn a warm cache
  # cold without notice. `purge` is how cached layers are removed.
  docker run -d --name "${name}" --label "${CONFIG_LABEL}=${want}" \
    -v "${name}:/var/lib/registry" \
    -e "REGISTRY_PROXY_REMOTEURL=$2" -e REGISTRY_PROXY_TTL=0 -e REGISTRY_LOG_LEVEL=info \
    "${IMAGE}" >/dev/null || docker inspect "${name}" >/dev/null 2>&1 # lost a race with a parallel run
}

# wait_ready <container>: the proxy answers on /v2/ once it has reached its upstream.
wait_ready() {
  for _ in $(seq 1 30); do
    if docker exec "$1" wget -q -O /dev/null http://127.0.0.1:5000/v2/ 2>/dev/null; then
      return 0
    fi
    # A mirror that exited, because its upstream was unreachable at startup, does not come back.
    [ "$(docker inspect -f '{{.State.Running}}' "$1" 2>/dev/null)" = "true" ] || break
    sleep 1
  done
  echo "e2e-registry-cache: mirror $1 did not become ready:" >&2
  docker logs --tail 5 "$1" >&2 || true
  return 1
}

# on_network <container>: whether the container is already on the Kind network.
on_network() {
  case " $(docker inspect -f '{{range $net, $cfg := .NetworkSettings.Networks}}{{$net}} {{end}}' "$1") " in
    *" ${KIND_NETWORK} "*) return 0 ;;
    *) return 1 ;;
  esac
}

# connect <container>: put the mirror on the Kind network, where nodes resolve it by name. A
# parallel run may connect it first, which is as good.
connect() {
  on_network "$1" || docker network connect "${KIND_NETWORK}" "$1" || on_network "$1"
}

# write_hosts <node> <registry> <upstream> <mirror>: containerd tries the mirror first and falls
# back to the upstream server, so a mirror that goes down later costs a failed connection, not the
# pull. The entry is written only once the node can reach the mirror.
write_hosts() {
  if ! docker exec "$1" curl -fsS -m 5 -o /dev/null "http://$4:5000/v2/"; then
    echo "e2e-registry-cache: node $1 cannot reach mirror $4 over the '${KIND_NETWORK}' Docker network" >&2
    return 1
  fi
  docker exec "$1" mkdir -p "${CERTS_DIR}/$2" || return 1
  docker exec -i "$1" sh -c "cat > '${CERTS_DIR}/$2/hosts.toml'" <<EOF || return 1
server = "$3"

[host."http://$4:5000"]
  capabilities = ["pull", "resolve"]
EOF
}

# wire_registry <registry> <upstream> <nodes>: wait for the registry's mirror and point every node
# (one name per line) at it.
wire_registry() {
  local name node
  name="$(mirror_name "$1")"
  wait_ready "${name}" || return 1
  connect "${name}" || return 1
  while IFS= read -r node; do
    write_hosts "${node}" "$1" "$2" "${name}" || return 1
  done <<<"$3"
}

attach() {
  local cluster="$1" nodes node entry registry upstream wired=0
  nodes="$(docker ps --filter "label=io.x-k8s.kind.cluster=${cluster}" --format '{{.Names}}')"
  [ -n "${nodes}" ] || die "no running Kind node for cluster '${cluster}'"
  while IFS= read -r node; do
    docker exec "${node}" grep -q config_path /etc/containerd/config.toml ||
      die "cluster '${cluster}' was not created from hack/kind-registry-cache.yaml, so its containerd ignores ${CERTS_DIR}; delete it (make prune-kind-cluster KIND_CLUSTER=${cluster}) and rerun with E2E_REGISTRY_CACHE=1"
  done <<<"${nodes}"

  # Start every mirror before waiting on any, so they initialise side by side. One that fails to
  # come up is skipped: its registry is pulled directly, which can cost the run time but not fail it.
  for entry in "${REGISTRIES[@]}"; do
    IFS='|' read -r registry upstream <<<"${entry}"
    start_mirror "${registry}" "${upstream}" || true
  done
  for entry in "${REGISTRIES[@]}"; do
    IFS='|' read -r registry upstream <<<"${entry}"
    if wire_registry "${registry}" "${upstream}" "${nodes}"; then
      wired=$((wired + 1))
    else
      warn "no mirror for ${registry}; its images are pulled directly"
    fi
  done
  echo "e2e registry cache: attached cluster '${cluster}' to ${wired} of ${#REGISTRIES[@]} mirrors"
  status || true
}

status() {
  local entry registry name state running size
  printf '  %-26s %-9s %s\n' REGISTRY STATE CACHED
  for entry in "${REGISTRIES[@]}"; do
    registry="${entry%%|*}"
    name="$(mirror_name "${registry}")"
    state=absent
    if running="$(docker inspect -f '{{.State.Status}}' "${name}" 2>/dev/null)"; then
      state="${running}"
    fi
    size="-"
    if [ "${state}" = running ]; then
      # du can lose a race with the proxy's temporary upload files; the table is only a report.
      size="$(docker exec "${name}" du -sh /var/lib/registry 2>/dev/null | cut -f1)" || size="?"
    elif docker volume inspect "${name}" >/dev/null 2>&1; then
      size="(volume kept)"
    fi
    printf '  %-26s %-9s %s\n' "${registry}" "${state}" "${size}"
  done
}

# remove_if_present <container|volume> <name>: a missing object is fine, but one that exists and
# cannot be removed (a volume still in use, say) fails with Docker's own message on stderr.
remove_if_present() {
  docker "$1" inspect "$2" >/dev/null 2>&1 || return 0
  if [ "$1" = volume ]; then
    docker volume rm "$2" >/dev/null
  else
    docker container rm -f "$2" >/dev/null
  fi
}

down() {
  local entry failed=0
  for entry in "${REGISTRIES[@]}"; do
    remove_if_present container "$(mirror_name "${entry%%|*}")" || failed=1
  done
  [ "${failed}" -eq 0 ] || die "could not remove every mirror container"
  echo "e2e registry cache: mirror containers removed; cached layers kept (purge deletes them)"
}

purge() {
  local entry failed=0
  down
  for entry in "${REGISTRIES[@]}"; do
    remove_if_present volume "$(mirror_name "${entry%%|*}")" || failed=1
  done
  [ "${failed}" -eq 0 ] || die "could not delete every cache volume, so the cache may still hold layers"
  echo "e2e registry cache: cached layers deleted"
}

usage() {
  echo "usage: $0 {attach <cluster>|status|down|purge}" >&2
  exit 2
}

case "${1:-}" in
  attach) [ $# -eq 2 ] || usage; attach "$2" ;;
  status) status ;;
  down) down ;;
  purge) purge ;;
  *) usage ;;
esac
