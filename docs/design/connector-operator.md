# ILM Connector Operator — Design Specification

## Overview

Kubernetes Operator for managing ILM platform connectors via Custom Resource Definitions (CRDs). Replaces Helm sub-chart based connector management with a modular, operator-driven approach that enables connector installation and lifecycle management directly from the ILM platform UI.

**Capability Level:** Level III (Full Lifecycle), with Level IV (Deep Insights) foundation in place.

**Tooling:** Go 1.26, Operator SDK v1.42.2, Kubebuilder v4, controller-runtime.

**Motivation:** See [OmniTrustILM Discussion #57](https://github.com/orgs/OmniTrustILM/discussions/57). Key drivers:
- Helm sub-charts hit Kubernetes manifest size limits as connectors grow
- Connectors cannot be installed from the platform UI with Helm
- Configuration changes to Secrets/ConfigMaps don't trigger redeployment with Helm
- Separation of cluster admin (infrastructure) and platform admin (connectors) concerns

## Scope

### Implemented
- `Connector` CRD (`otilm.com/v1alpha1`) for deploying any ILM connector
- Operator that reconciles Connector CRs into Kubernetes resources (Deployment, Service, ServiceAccount, PDB, ServiceMonitor)
- Secret/ConfigMap watching with automatic rolling updates on change
- Optional connector registration with ILM platform Core API
- Helm chart for deploying the operator itself
- Comprehensive test suite (87% overall coverage, <3% duplication)
- CI/CD with SonarCloud quality gates, golangci-lint, Copilot reviews

### Future Work
- Multi-cluster / remote deployment (proxy integration)
- Horizontal Pod Autoscaler (HPA) management
- ILM Core changes for CR creation
- Connector deregistration on CR deletion
- Level IV alert rules, dashboards, and runbooks (foundation only)
- Non-connector ILM components (platform, core, etc.)

## CRD Design

### API Group and Versioning
- **Group:** `otilm.com`
- **Version:** `v1alpha1` (will graduate to `v1` after validation through real usage)
- **Kind:** `Connector`
- **Scope:** Namespaced

### Spec

```yaml
apiVersion: otilm.com/v1alpha1
kind: Connector
metadata:
  name: my-connector
  namespace: ilm
spec:
  # === REQUIRED ===

  # Container image
  image:
    repository: docker.io/czertainly/czertainly-common-credential-provider
    tag: "2.0.0"
    pullPolicy: IfNotPresent       # default
    pullSecrets: []                 # optional

  # Service configuration
  service:
    port: 8080                     # default
    type: ClusterIP                # default

  # === OPTIONAL ===

  # Deployment
  replicas: 1                      # default
  resources: {}                    # CPU/memory requests/limits

  # Pod metadata (for third-party integrations like Vault Agent Injector, Istio, etc.)
  podAnnotations:
    vault.hashicorp.com/agent-inject: "true"
    vault.hashicorp.com/role: "connector"
  podLabels:
    team: platform

  # Security
  securityContext:
    runAsNonRoot: true             # default
    readOnlyRootFilesystem: true   # default

  # Health probes (defaults to /v2/health/* per connector common interface spec)
  probes:
    liveness:
      path: /v2/health/liveness    # default
      initialDelaySeconds: 15
      periodSeconds: 10
      failureThreshold: 3
    readiness:
      path: /v2/health/readiness   # default
      initialDelaySeconds: 5
      periodSeconds: 10
      failureThreshold: 3
    startup:
      path: /v2/health/liveness    # default
      failureThreshold: 45
      periodSeconds: 10

  # Inline environment variables (non-sensitive)
  env:
    - name: LOGGING_LEVEL
      value: "INFO"
    - name: JAVA_OPTS
      value: "-Xms128m -Xmx512m"

  # Secret references (watched for changes)
  # type: env — mounts as environment variables
  #   - With keys: each secretKey is mapped to the specified envVar name
  #   - Without keys: all keys in the Secret are mounted as env vars using the key name as env var name
  # type: volume — mounts as files at mountPath
  #   - With keys: only specified keys are mounted, using path as filename
  #   - Without keys: all keys are mounted as files (standard K8s projected volume behavior)
  secretRefs:
    - name: my-connector-db-credentials
      type: env
      keys:
        - secretKey: username
          envVar: JDBC_USERNAME
        - secretKey: password
          envVar: JDBC_PASSWORD

    - name: my-connector-tls
      type: volume
      mountPath: /etc/ssl/custom
      keys:
        - secretKey: ca.crt
          path: ca.crt

  # ConfigMap references (watched for changes)
  # Same type/keys behavior as secretRefs
  configMapRefs:
    - name: my-connector-config
      type: env                    # All keys mounted as env vars (key name = env var name)

    - name: my-connector-files
      type: volume
      mountPath: /etc/connector/config

  # Ephemeral volumes
  volumes:
    - name: tmp
      mountPath: /tmp
      emptyDir:
        medium: Memory             # Memory or empty string for disk
        sizeLimit: 1Mi

  # Lifecycle (Level III)
  lifecycle:
    terminationGracePeriodSeconds: 30
    podDisruptionBudget:
      enabled: false
      minAvailable: 1

  # Metrics (Level IV foundation)
  metrics:
    enabled: false
    path: /v1/metrics              # default per connector common interface spec
    port: 8080                     # default, same as service port
    serviceMonitor:
      enabled: false
      interval: 30s
      labels: {}

  # Platform registration (optional)
  registration:
    platformUrl: "https://my-ilm.example.com/api"
    name: "My Network Discovery Provider"
    authType: none                 # none, basic, certificate, apiKey, jwt
    authAttributes: []             # Required if authType != none
    customAttributes: []
```

### Status

```yaml
status:
  phase: Running                   # Pending, Deploying, Running, Failed, Updating
  observedGeneration: 3
  replicas: 1
  readyReplicas: 1
  endpoint: "http://my-connector.ilm.svc.cluster.local:8080"
  currentImage: "docker.io/czertainly/czertainly-common-credential-provider:2.0.0"
  configChecksum: "sha256:abc123..."
  conditions:
    - type: Available
      status: "True"
      reason: DeploymentReady
      message: "Connector is running and ready"
      lastTransitionTime: "2026-04-11T10:00:00Z"
    - type: Progressing
      status: "False"
      reason: DeploymentComplete
      lastTransitionTime: "2026-04-11T10:00:00Z"
    - type: Degraded
      status: "False"
      lastTransitionTime: "2026-04-11T10:00:00Z"
  # Present only when spec.registration is configured
  registration:
    uuid: "7b55ge1c-844f-11dc-a8a3-0242ac120002"
    status: connected             # waitingForApproval, connected, failed, offline
    registeredAt: "2026-04-11T10:00:00Z"
```

**Print columns:** Phase, Ready (readyReplicas/replicas), Endpoint, Age.

**Status conditions:**
- `Available` — connector Deployment has minimum ready replicas
- `Progressing` — rollout in progress (image change, config change, scaling)
- `Degraded` — something is wrong (missing Secret, pods crashing, registration failed)

## Architecture

### Controller Pattern

Single controller per CRD, following Operator SDK best practices (Memcached tutorial pattern). The controller uses the `Owns()` pattern for child resources and `Watches()` for referenced Secrets/ConfigMaps.

```go
func (r *ConnectorReconciler) SetupWithManager(mgr ctrl.Manager) error {
    return ctrl.NewControllerManagedBy(mgr).
        For(&otilmv1alpha1.Connector{}).
        Owns(&appsv1.Deployment{}).
        Owns(&corev1.Service{}).
        Owns(&corev1.ServiceAccount{}).
        Owns(&policyv1.PodDisruptionBudget{}).
        Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.findConnectorsForSecret)).
        Watches(&corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(r.findConnectorsForConfigMap)).
        Complete(r)
}
```

ServiceMonitor ownership is conditional — only set up when Prometheus CRDs exist in the cluster.

### Resource Builders

Each Kubernetes resource type has a dedicated builder function in `internal/builder/`. Builders are pure functions: given a Connector CR spec, return the desired Kubernetes resource. This makes them independently unit-testable.

| Builder | Input | Output |
|---------|-------|--------|
| `BuildDeployment()` | Connector spec, checksums | `*appsv1.Deployment` |
| `BuildService()` | Connector spec | `*corev1.Service` |
| `BuildServiceAccount()` | Connector spec | `*corev1.ServiceAccount` |
| `BuildPDB()` | Connector spec | `*policyv1.PodDisruptionBudget` |
| `BuildServiceMonitor()` | Connector spec | `*monitoringv1.ServiceMonitor` |

### Reconciliation Flow

1. **Fetch** Connector CR. If not found, return (deleted — owner references cascade cleanup).
2. **Finalizer** — add if missing. If CR is being deleted, run finalizer logic (emit deletion event, clean up any operator-managed state), remove finalizer, return. The finalizer ensures graceful cleanup and provides an extension point for future cleanup needs (e.g., deregistration). Child resources are cleaned up automatically via owner references.
3. **Set phase** = `Deploying`, condition `Progressing` = True.
4. **Compute checksums** of all referenced Secrets and ConfigMaps. If any referenced Secret/ConfigMap is missing, set `Degraded` condition, requeue.
5. **Reconcile child resources** (create if missing, update if drifted from desired state):
   - a. ServiceAccount
   - b. Deployment (with checksum annotation on pod template to trigger rollout on config change)
   - c. Service
   - d. PodDisruptionBudget (if `lifecycle.podDisruptionBudget.enabled`)
   - e. ServiceMonitor (if `metrics.serviceMonitor.enabled`)
6. **Check Deployment status:**
   - All replicas ready → phase = `Running`, `Available` = True
   - Rolling update in progress → phase = `Updating`, `Progressing` = True
   - Pods failing → phase = `Failed`, `Degraded` = True
7. **Registration** — if connector is healthy AND `spec.registration` is configured AND not yet registered: call `POST /v2/connector/register` on platform Core API. Store returned UUID and status.
8. **Update CR status** (phase, conditions, endpoint, currentImage, configChecksum, registration).
9. **Emit Kubernetes event** (Deployed, Updated, Degraded, Recovered, Registered, etc.).
10. **Return** — no requeue unless waiting for rollout or registration retry.

### Drift Correction

If someone manually edits a child resource (Deployment, Service, etc.), the next reconciliation detects the difference between the actual state and the desired state from the CR spec, and reverts the child resource. This is the standard Kubernetes operator pattern — the CR is the source of truth.

### Secret/ConfigMap Change Detection

The controller watches all Secrets and ConfigMaps referenced by any Connector CR. When a watched resource changes:
1. The watch handler finds all Connector CRs that reference the changed resource.
2. Those Connector CRs are enqueued for reconciliation.
3. During reconciliation, new checksums are computed.
4. If checksums differ from the annotation on the Deployment pod template, the Deployment is updated with new checksums, triggering a rolling restart.

### Platform Registration

Isolated in `internal/platform/` package:
- `client.go` — HTTP client for ILM Core API
- `registration.go` — builds and sends `POST /v2/connector/register` request

Registration request fields (from ILM Core API):
- `name` — from `spec.registration.name`
- `version` — `v2` (hardcoded, all NG connectors)
- `url` — computed from the Service endpoint (`http://<name>.<namespace>.svc.cluster.local:<port>`)
- `authType` — from `spec.registration.authType`
- `authAttributes` — from `spec.registration.authAttributes`
- `customAttributes` — from `spec.registration.customAttributes`

Registration is optional. If `spec.registration` is omitted, the operator does not call the platform API. The platform can register the connector manually via UI or other means.

**Registration retry behavior:** If the registration call fails (platform unreachable, 5xx error), the operator sets the `Degraded` condition with a registration failure reason and requeues with exponential backoff (starting at 5s, max 5m). Registration is only attempted when the connector Deployment is healthy (all replicas ready). If registration returns a 4xx error (e.g., connector already registered, validation error), the operator sets the `Degraded` condition and does not retry — manual intervention is required.

## Connector Common Interfaces Coverage

All ILM connectors implement common interfaces. The operator's responsibility for each:

| Interface | Endpoint | Operator Role |
|-----------|----------|---------------|
| Info | `GET /v2/info` | Ensure connector is reachable via Service. Platform calls this for capability discovery. |
| Health | `GET /v2/health/liveness`, `GET /v2/health/readiness` | Configure as Kubernetes liveness and readiness probes on the Deployment. |
| Metrics | `GET /v1/metrics` | Optionally create ServiceMonitor for Prometheus scraping. |
| Logging | stdout (JSON, `connector.log` schema v1) | No action needed. Connectors output structured JSON logs to stdout. Standard Kubernetes log collectors handle the rest. Logging level configurable via `spec.env`. |
| Error Handling | RFC 9457 responses | No action needed. Platform consumes error responses from connectors directly. |

## RBAC

### Operator ClusterRole

| Resource | Verbs | Purpose |
|----------|-------|---------|
| `connectors.otilm.com` | get, list, watch, update, patch | Reconcile CRs, update status |
| `connectors.otilm.com/status` | update, patch | Status subresource |
| `connectors.otilm.com/finalizers` | update | Manage finalizers |
| `deployments.apps` | get, list, watch, create, update, patch, delete | Manage connector Deployments |
| `services` | get, list, watch, create, update, patch, delete | Manage connector Services |
| `serviceaccounts` | get, list, watch, create, update, patch, delete | Manage connector ServiceAccounts |
| `poddisruptionbudgets.policy` | get, list, watch, create, update, patch, delete | Manage PDBs |
| `secrets` | get, list, watch | Read referenced Secrets (never create/modify) |
| `configmaps` | get, list, watch | Read referenced ConfigMaps |
| `events` | create, patch | Emit Kubernetes events |
| `leases.coordination.k8s.io` | get, list, watch, create, update, patch, delete | Leader election |
| `servicemonitors.monitoring.coreos.com` | get, list, watch, create, update, patch, delete | Conditional: Prometheus ServiceMonitor |

### Security Principles
- Operator runs as non-root in distroless container
- Least privilege — only permissions listed above
- Secrets are read-only — operator never creates or modifies Secrets
- ServiceMonitor permissions conditional on Prometheus CRDs existing
- Leader election for single active controller in HA deployments
- Connector pods default to `runAsNonRoot: true`, `readOnlyRootFilesystem: true`

## Project Structure

```
operator/
├── api/
│   └── v1alpha1/
│       ├── connector_types.go          # Connector CRD spec/status Go types
│       ├── groupversion_info.go        # API group registration
│       └── zz_generated.deepcopy.go    # Auto-generated DeepCopy methods
│
├── cmd/
│   └── main.go                         # Entry point, manager setup
│
├── internal/
│   ├── controller/
│   │   ├── connector_controller.go     # Main reconciler
│   │   ├── connector_controller_test.go
│   │   ├── suite_test.go               # envtest suite setup
│   │   └── watches.go                  # Secret/ConfigMap watch handlers
│   │
│   ├── builder/
│   │   ├── common.go                   # Shared builder helpers (labels, annotations)
│   │   ├── common_test.go
│   │   ├── deployment.go               # Builds Deployment from CR spec
│   │   ├── deployment_test.go
│   │   ├── service.go                  # Builds Service
│   │   ├── service_test.go
│   │   ├── serviceaccount.go           # Builds ServiceAccount
│   │   ├── serviceaccount_test.go
│   │   ├── pdb.go                      # Builds PodDisruptionBudget
│   │   ├── pdb_test.go
│   │   ├── servicemonitor.go           # Builds ServiceMonitor
│   │   └── servicemonitor_test.go
│   │
│   ├── checksum/
│   │   ├── checksum.go                 # Computes checksums for Secrets/ConfigMaps
│   │   └── checksum_test.go
│   │
│   ├── platform/
│   │   ├── client.go                   # HTTP client for ILM Core API
│   │   ├── client_test.go
│   │   ├── registration.go             # Registration request/response logic
│   │   └── registration_test.go
│   │
│   ├── monitoring/
│   │   ├── metrics.go                  # Custom Prometheus metrics registration
│   │   └── events.go                   # Kubernetes event recorder helpers
│   │
│   └── version/
│       └── version.go                  # Build-time version injection via ldflags
│
├── config/
│   ├── crd/bases/                      # Generated CRD YAML
│   ├── rbac/                           # ClusterRole, ClusterRoleBinding, ServiceAccount
│   ├── manager/                        # Operator Deployment manifest
│   ├── samples/                        # Example Connector CRs
│   └── default/                        # Kustomize default overlay
│
├── deploy/
│   └── charts/
│       └── ilm-operator/
│           ├── Chart.yaml
│           ├── values.yaml
│           └── templates/
│               ├── _helpers.tpl
│               ├── deployment.yaml
│               ├── service-account.yaml
│               ├── cluster-role.yaml
│               ├── cluster-role-binding.yaml
│               ├── leader-election-role.yaml
│               ├── leader-election-role-binding.yaml
│               └── crds/
│
├── test/
│   ├── e2e/
│   │   ├── e2e_suite_test.go           # Ginkgo E2E suite setup
│   │   └── e2e_test.go                 # End-to-end test cases
│   └── utils/
│       └── utils.go                    # Shared E2E test utilities
│
├── Dockerfile                          # Multi-stage: Go builder + distroless
├── Makefile                            # Build, test, lint, kind cluster, deploy
├── .golangci.yml                       # Linter configuration
├── .github/
│   └── workflows/
│       ├── ci.yml                      # Lint, test, build on PR
│       └── release.yml                 # Build + push Docker image on tag
├── sonar-project.properties            # SonarCloud configuration
├── go.mod
├── go.sum
├── PROJECT                             # Kubebuilder project file
├── CLAUDE.md                           # Development guidelines for AI assistants
└── README.md                           # Project documentation
```

## Testing Strategy

### Test Pyramid

**Unit Tests** (per PR, `make test`)
- Builder functions: given CR spec, assert correct Kubernetes resource output (labels, annotations, env vars, volumes, probes, security context, owner references)
- Checksum computation: verify deterministic, change-sensitive behavior
- Platform client: HTTP request construction, response parsing, error handling (mocked HTTP)
- Status logic: phase transitions, condition updates
- RBAC: operator can perform required operations

**Integration Tests** (per PR, `make test` with envtest)
- Full reconciliation cycle: create CR → verify child resources → update CR → verify drift correction → delete CR → verify cleanup
- Secret/ConfigMap watch triggers reconciliation
- Checksum annotation change triggers Deployment rollout
- PDB created when enabled, absent when disabled
- ServiceMonitor created when enabled and Prometheus CRDs exist
- Finalizer lifecycle (add, execute, remove)
- Multiple Connector CRs in same and different namespaces
- Missing Secret/ConfigMap → Degraded condition

**E2E Tests** (`make test-e2e`, Kind cluster)
- Full lifecycle with real connector image (x509-compliance-provider — no database needed)
- Secret rotation → verify pod restart
- Leader election with multiple operator replicas
- RBAC enforcement in real cluster
- Testcontainers for PostgreSQL when testing DB-dependent connectors

### Coverage (Actuals)
- `internal/builder/` — 90%
- `internal/checksum/` — 100%
- `internal/platform/` — 97%
- `internal/controller/` — 83%
- **Overall: 87%**
- **Code duplication: <3%, enforced by SonarCloud**

### Test Infrastructure (Makefile)
- `make test` — unit + integration with envtest, coverage report
- `make test-e2e` — E2E on Kind cluster
- `make kind-cluster` — create Kind test cluster
- `make kind-load` — load operator image into Kind
- `make lint` — golangci-lint static analysis
- `make coverage` — verify 80% threshold
- `make sonar` — run local SonarQube analysis (requires a running SonarQube instance; mirrors the SonarCloud gate used in CI)

## Quality Assurance

### CI Pipeline (GitHub Actions)
1. **Lint** — golangci-lint with comprehensive ruleset (errcheck, gosec, govet, staticcheck, ineffassign, dupl, goconst, revive, misspell, goimports)
2. **Unit + Integration Tests** — `make test` with coverage report
3. **E2E Tests** — Kind cluster, full lifecycle
4. **SonarCloud Analysis** — coverage upload, duplication check, code smells, security hotspots, maintainability rating
5. **Build** — Docker image build verification

### Quality Gates (SonarCloud)
- Coverage: 80%+ on new code
- Duplication: <3%
- No new bugs, vulnerabilities, or security hotspots
- Maintainability rating: A

### Code Review
- Copilot automated reviews on PRs
- Code reviewer agent after major implementation steps

### Linting (.golangci.yml)
Enabled linters: errcheck, gosec, govet, staticcheck, ineffassign, dupl, goconst, revive, misspell, goimports, gofmt, unconvert, unparam, prealloc, bodyclose, noctx, exhaustive.

## Operator Deployment (Helm Chart)

The operator is delivered as a Docker container and deployed via Helm:

```yaml
# deploy/charts/ilm-operator/values.yaml
replicaCount: 1

image:
  registry: docker.io
  repository: otilm
  name: ilm-operator
  tag: ""                          # Defaults to appVersion
  pullPolicy: IfNotPresent
  pullSecrets: []

crd:
  install: true

serviceAccount:
  create: true
  name: ""
  annotations: {}

rbac:
  create: true

leaderElection:
  enabled: true

resources:
  requests:
    cpu: 100m
    memory: 128Mi
  limits:
    memory: 256Mi

securityContext:
  runAsNonRoot: true
  readOnlyRootFilesystem: true

podDisruptionBudget:
  enabled: false
  minAvailable: 1

metrics:
  enabled: false
  service:
    port: 8080
  serviceMonitor:
    enabled: false

watchNamespaces: []                # Empty = all namespaces

nodeSelector: {}
tolerations: []
affinity: {}
```

The Helm chart creates: operator Deployment, ServiceAccount, ClusterRole, ClusterRoleBinding, CRDs, and optionally ServiceMonitor + PDB for the operator itself.

## Operator Capability Level Progression

| Level | Name | Status | Features |
|-------|------|--------|----------|
| I | Basic Install | Implemented | Deploy connectors from CR, configure via spec, report status |
| II | Seamless Upgrades | Implemented | Rolling updates on spec change, Secret/ConfigMap checksum triggers, drift correction |
| III | Full Lifecycle | Implemented | PDB, graceful shutdown, finalizers, Kubernetes events, rich status conditions |
| IV | Deep Insights | Foundation | `/monitoring` package, event recorder, ServiceMonitor support, custom metrics registration. Alert rules, dashboards, and runbooks planned as fast follow-up. |
| V | Auto Pilot | Future Work | Auto-scaling, auto-healing, auto-tuning, abnormality detection |

## Connector Database Patterns

Not all connectors require a database. The CRD is database-agnostic — database configuration is handled through `spec.env` and `spec.secretRefs`:

| Pattern | Connectors | How to configure |
|---------|-----------|-----------------|
| No database | x509-compliance-provider | No database env/secrets needed |
| JDBC_URL | common-credential-provider, cryptosense-discovery-provider, ejbca-ng-connector, email-notification-provider, keystore-entity-provider, network-discovery-provider, software-cryptography-provider, webhook-notification-provider | `spec.env` for JDBC_URL, `spec.secretRefs` (type: env) for credentials |
| DATABASE_* env vars | ct-logs-discovery-provider, hashicorp-vault-connector, pyadcs-connector | `spec.secretRefs` (type: env) for individual DATABASE_* variables |

## Deployment Topology

The operator is cluster-local. It watches Connector CRs in its own cluster and reconciles them.

- **Same-cluster (standard on-prem):** ILM Core → Kubernetes API → creates Connector CR → operator reconciles
- **Remote/SaaS via proxy:** ILM Core → AMQP/WSS → Proxy (on-prem) → Kubernetes API → creates Connector CR → operator reconciles
- **Multi-cluster HA:** Each cluster runs its own operator instance. CRs are created in each cluster independently.

Multi-cluster CR distribution is out of MVP scope. The proxy and ILM Core handle routing; the operator is topology-agnostic.
