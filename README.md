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
  namespace: default
spec:
  image:
    repository: docker.io/czertainly/czertainly-x509-compliance-provider
    tag: "1.3.1"
  service:
    port: 8080
  # x509-compliance-provider is a legacy connector using /v1/health,
  # not the /v2/health/* endpoints used by newer connectors.
  # Override the default probes accordingly.
  probes:
    liveness:
      path: /v1/health
      initialDelaySeconds: 15
      periodSeconds: 10
      failureThreshold: 3
    readiness:
      path: /v1/health
      initialDelaySeconds: 5
      periodSeconds: 10
      failureThreshold: 3
    startup:
      path: /v1/health
      periodSeconds: 10
      failureThreshold: 45
  env:
    - name: SERVER_PORT
      value: "8080"
    - name: LOG_LEVEL
      value: "INFO"
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

- Go 1.26+
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
| `make sonar` | Run local SonarQube analysis |

### Run locally (outside cluster)

```bash
make install      # Install CRDs into the cluster
make run          # Run the operator outside the cluster
```

### Test on a local Kind cluster

```bash
# 1. Create a Kind cluster
make kind-cluster

# 2. Build the operator Docker image
make docker-build IMG=ilm-operator:dev

# 3. Load the image into Kind
make kind-load IMG=ilm-operator:dev

# 4. Deploy the operator (CRDs, RBAC, Deployment)
make deploy IMG=ilm-operator:dev

# 5. Verify the operator is running
kubectl get pods -n ilm-operator-system

# 6. Deploy a sample connector
kubectl apply -f config/samples/connector_minimal.yaml

# 7. Watch the connector status
kubectl get connectors -w

# 8. Verify the created resources
kubectl get deploy,svc,sa -l otilm.com/connector=x509-compliance-provider

# 9. Test drift correction (operator should revert the change)
kubectl patch svc x509-compliance-provider --type='json' \
  -p='[{"op":"replace","path":"/spec/ports/0/port","value":9999}]'
kubectl get svc x509-compliance-provider -o jsonpath='{.spec.ports[0].port}'

# 10. Clean up
kubectl delete -f config/samples/connector_minimal.yaml
make prune-kind-cluster
```

> **Note:** The x509-compliance-provider is a legacy connector that uses `/v1/health` instead of
> the default `/v2/health/*` probes. See the sample YAML for probe override configuration.
> Newer (NG) connectors use the default `/v2/health/liveness` and `/v2/health/readiness` paths.

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
- `spec.podAnnotations` -- arbitrary annotations on the pod template (e.g., Vault Agent Injector, Istio)
- `spec.podLabels` -- arbitrary labels on the pod template (merged with operator labels; operator labels take precedence)
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

## License

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details.
