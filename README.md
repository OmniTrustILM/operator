# ILM Connector Operator

A Kubernetes operator that manages ILM (Identity Lifecycle Management) platform connectors through declarative Custom Resources. Define a `Connector` CR and the operator handles Deployment, Service, ServiceAccount, health probes, secrets, metrics, PDB, and optional platform registration.

## Features

- **Declarative connector management** -- define connectors as Kubernetes CRs
- **Full lifecycle management** -- create, update, and delete connector Deployments, Services, and ServiceAccounts
- **Rolling updates** -- automatic rollout on spec changes with configurable strategy
- **Secret/ConfigMap integration** -- inject as environment variables or mount as volumes
- **Configuration drift detection** -- checksum-based change detection triggers redeployment
- **Health probes** -- configurable liveness, readiness, and startup probes with sensible defaults
- **Pod Disruption Budgets** -- optional PDB creation for high availability
- **Prometheus metrics** -- operator and connector metrics with optional ServiceMonitor creation
- **Platform registration** -- optional automatic registration with the ILM platform API
- **Security hardened** -- non-root containers, read-only root filesystem, dropped capabilities

## Prerequisites

- Kubernetes 1.28+
- Helm 3.x (for Helm-based installation)
- kubectl configured with cluster access

## Quick Start

### Install the operator via Helm

```bash
helm install ilm-operator deploy/charts/ilm-operator \
  --namespace ilm-system --create-namespace
```

### Create a Connector

```yaml
apiVersion: otilm.com/v1alpha1
kind: Connector
metadata:
  name: x509-compliance-provider
spec:
  image:
    repository: docker.io/czertainly/czertainly-x509-compliance-provider
    tag: "2.13.0"
  service:
    port: 8080
  env:
    - name: SERVER_PORT
      value: "8080"
```

```bash
kubectl apply -f connector.yaml
kubectl get connectors
```

### Check status

```bash
kubectl get connector x509-compliance-provider -o wide
kubectl describe connector x509-compliance-provider
```

## Development

### Prerequisites

- Go 1.25+
- Docker or compatible container runtime
- Kind (for local testing)

### Make Targets

| Target | Description |
|--------|-------------|
| `make build` | Build the operator binary |
| `make test` | Run unit and integration tests |
| `make lint` | Run golangci-lint |
| `make docker-build` | Build the Docker image |
| `make manifests` | Generate CRDs and RBAC manifests |
| `make generate` | Generate DeepCopy methods |
| `make coverage` | Run tests and verify 80% coverage threshold |
| `make kind-cluster` | Create a Kind cluster for development |
| `make kind-load` | Load operator image into Kind cluster |
| `make test-e2e` | Run end-to-end tests |

### Run locally

```bash
make install      # Install CRDs into the cluster
make run          # Run the operator outside the cluster
```

## Architecture

The operator follows the standard Kubernetes operator pattern using controller-runtime:

```
Connector CR --> Reconciler --> Builders --> Kubernetes Resources
                    |
                    +--> Deployment Builder
                    +--> Service Builder
                    +--> ServiceAccount Builder
                    +--> PDB Builder
                    +--> ServiceMonitor Builder
                    +--> Platform Registration Client
```

The reconciler watches `Connector` resources and ensures the desired state (Deployment, Service, ServiceAccount, PDB, ServiceMonitor) matches the spec. Configuration checksums are computed from secrets and configmaps to trigger rolling updates when referenced data changes.

## CRD Reference

The `Connector` CRD (`otilm.com/v1alpha1`) supports:

- `spec.image` -- container image configuration (repository, tag, pullPolicy, pullSecrets)
- `spec.replicas` -- desired replica count
- `spec.service` -- service configuration (port, type)
- `spec.resources` -- CPU/memory requests and limits
- `spec.env` -- environment variables
- `spec.secretRefs` -- secret references (as env vars or volume mounts)
- `spec.configMapRefs` -- configmap references (as env vars or volume mounts)
- `spec.volumes` -- additional ephemeral volumes
- `spec.probes` -- liveness, readiness, startup probe configuration
- `spec.securityContext` -- pod security context
- `spec.lifecycle` -- termination grace period and PDB settings
- `spec.metrics` -- metrics endpoint and ServiceMonitor configuration
- `spec.registration` -- platform registration settings

See `config/samples/` for example CRs:
- [`connector_minimal.yaml`](config/samples/connector_minimal.yaml) -- minimal configuration
- [`connector_full.yaml`](config/samples/connector_full.yaml) -- comprehensive example with all features
- [`connector_with_registration.yaml`](config/samples/connector_with_registration.yaml) -- platform registration example

## Links

- [Design Specification](docs/design/connector-operator.md)
- [Helm Chart](deploy/charts/ilm-operator/)
- [CLAUDE.md Development Guide](CLAUDE.md)
