# ILM Operator

A Kubernetes operator that manages the **ILM (Identity Lifecycle Management) platform** and
its **connectors** declaratively, through Custom Resources (`otilm.com/v1alpha1`):

- **`Platform`** — deploys and wires the ILM platform itself: Core, auth,
  auth-opa-policies, scheduler, fe-administrator, the Kong API gateway, and the
  optional utils. It connects them to your **database** and **message broker**
  (either referenced as *external* infrastructure or *managed* for you via upstream
  operators), with an optional **edge** (Ingress / Gateway API + cert-manager TLS), an
  optional **managed Keycloak** OIDC provider, and an optional first-admin bootstrap.
- **`Connector`** — deploys an ILM connector: Deployment, Service, ServiceAccount, health
  probes, config injection, metrics, an optional PodDisruptionBudget, and optional
  registration with the ILM platform.

One operator binary runs both controllers.

## Why an operator?

Managing the platform and its connectors as Kubernetes resources gives you a single
declarative source of truth with full lifecycle management: rolling updates on spec change,
automatic redeployment when a referenced Secret/ConfigMap changes, drift correction, rich
status conditions, and safe deletion. The platform's stateful infrastructure is **delegated
to upstream operators** (CloudNativePG, the RabbitMQ operators, the Keycloak Operator) when
managed, or referenced when external — never re-templated. No sensitive value ever lives in a
Custom Resource; credentials are Secret references only.

## Quick start

The fastest path to a running platform is the **everything-managed** quickstart — the
operator provisions PostgreSQL, RabbitMQ, and Keycloak for you, and there are **no Secrets to
pre-create**:

```bash
# 1. Install the operator (CRDs install with it)
helm install ilm-operator deploy/charts/ilm-operator \
  --namespace ilm-system --create-namespace

# 2. Install the upstream operators (the only prerequisites) and cert-manager —
#    run `make install-upstream-operators` (pinned, validated versions) or
#    see docs/quickstart.md for the manual apply commands.

# 3. Apply the everything-managed Platform (edit common.hostName / edge.host first)
kubectl create namespace ilm
kubectl apply -f config/samples/platform_quickstart.yaml

# 4. Watch it converge (Phase Progressing -> Running; READY = the Available condition)
kubectl get platform -n ilm -w
```

**See the full [Quickstart](docs/quickstart.md)** for the prerequisite install commands and a
walkthrough, or the [Platform getting-started guide](docs/platform.md) for external
infrastructure, edge variants, the exact Secret keys, all status conditions, and the full
customization surface.

### Create a Connector

```yaml
apiVersion: otilm.com/v1alpha1
kind: Connector
metadata:
  name: x509-compliance-provider
  namespace: default
spec:
  image:
    repository: hub.omnitrustregistry.com/ilm/x509-compliance-provider
    tag: "1.3.1"
  service:
    port: 8080
  env:
    - name: LOG_LEVEL
      value: "INFO"
```

```bash
kubectl apply -f connector.yaml
kubectl get connectors -o wide
```

See the [samples index](config/samples/README.md) for more Connector and Platform examples.

## Features

### Platform

- **Whole-platform deployment** from one `Platform` CR — every stateless component, wired to
  your database and broker.
- **External or managed infrastructure** — bring your own PostgreSQL / RabbitMQ / Keycloak,
  or let the operator provision them via CloudNativePG, the RabbitMQ Cluster + Messaging
  Topology operators, and the Keycloak Operator.
- **Capability detection & graceful gating** — a missing upstream operator or edge CRD is a
  non-fatal, self-healing waiting state (an adjunct condition + requeue), never a cryptic
  apply failure.
- **Pluggable edge** — Ingress (`ingress-nginx`) or Gateway API, with cert-manager TLS
  (`internal` / `letsEncrypt` / `issuerRef`) or a bring-your-own TLS Secret.
- **Tested version bundles** — `spec.version` selects a tested set of component images +
  wiring + managed topology; one operator build supports a range of platform versions.
- **Managed-infra upgrade guard** — a major-version bump of a running managed cluster is
  guarded behind an explicit acknowledgement.
- **Default-on NetworkPolicies, SCC-clean pods, read-only root filesystems**, automatic
  Core↔Keycloak OIDC wiring, optional first-admin bootstrap, and safe deletion
  (`deletionPolicy: Retain` protects managed data).

### Connector

- Declarative connector lifecycle (create / update / delete) with rolling updates.
- Secret/ConfigMap injection (as env vars or mounted volumes) with config-drift detection.
- Configurable health probes, optional PodDisruptionBudget, Prometheus metrics + optional
  ServiceMonitor, and optional platform registration.
- Security hardened — non-root, read-only root filesystem, dropped capabilities.

## Documentation

| Document | What it covers |
|---|---|
| [Quickstart](docs/quickstart.md) | The everything-managed apply-and-go path. |
| [Platform getting-started guide](docs/platform.md) | Install, prerequisites, Secret keys, conditions, the full customization surface, teardown. |
| [Configuration reference & scenario cookbook](docs/configuration.md) | Every Platform option explained, mapped to the matching sample. |
| [Platform versions](docs/versions.md) | Deploying a specific ILM release via `spec.version`. |
| [Platform upgrades](docs/upgrades.md) | The upgrade procedure and the managed-infra upgrade guard. |
| [Migrating from the Helm umbrella chart](docs/migration-from-helm.md) | Translating chart values to a `Platform` CR (the `values2platform` aid). |
| [Platform design specification](docs/design/platform-operator.md) | Architecture, security model, infra delegation, reconciliation. |
| [Connector design specification](docs/design/connector-operator.md) | The `Connector` CRD: schema, reconciliation flow, architecture. |
| [Platform CR field reference](docs/design/examples/platform-cr-reference.yaml) | Annotated full-surface reference (implemented vs. design-target). |
| [Operator install paths](deploy/README.md) | Installing the operator itself — `kubectl apply` release manifest, Helm chart, or OLM. |
| [CLAUDE.md](CLAUDE.md) | Development guide. |

## Installation

The operator and its CRDs install together. Choose one path.

### kubectl apply (release manifest)

Each release ships a single self-contained manifest — Namespace, both CRDs, RBAC, and the
manager Deployment, with the operator image pinned to that release. No cluster tooling beyond
`kubectl` is required, and **cert-manager is not needed to install the operator** (it is only a
prerequisite for cert-managed *platform* edges):

```bash
kubectl apply -f https://github.com/OmniTrustILM/operator/releases/download/<version>/ilm-operator.yaml
```

Replace `<version>` with a [release tag](https://github.com/OmniTrustILM/operator/releases)
(e.g. `v0.1.0`). The operator installs into the `ilm-operator-system` namespace. The operator
image is published to a public registry, so no image pull secret is required.

For GitOps or CRD-first installs (e.g. applying the CRDs ahead of the controller, or with
`--server-side` on upgrades), apply the CRDs-only manifest:

```bash
kubectl apply -f https://github.com/OmniTrustILM/operator/releases/download/<version>/ilm-operator.crds.yaml
```

> This flat manifest is the quick, opinionated install. For configurable installs (custom
> namespace, replicas, ServiceMonitor, PodDisruptionBudget, image overrides), use the Helm
> chart below.

### Helm

```bash
helm install ilm-operator deploy/charts/ilm-operator \
  --namespace ilm-system --create-namespace
```

The chart installs the CRDs by default (`crd.install: true`).

### OLM

```bash
# Install OLM on the cluster (if not already installed)
operator-sdk olm install

# Build and push the bundle image, then deploy via OLM
make bundle-build bundle-push BUNDLE_IMG=<registry>/ilm-operator-bundle:v0.0.1
operator-sdk run bundle <registry>/ilm-operator-bundle:v0.0.1
```

### Prerequisites

- Kubernetes 1.28+
- `kubectl` configured with cluster access
- Helm 3.x (for the Helm install path)
- For **managed** platform infrastructure: the relevant upstream operators (CloudNativePG,
  the RabbitMQ Cluster + Messaging Topology operators, the Keycloak Operator) and, for
  cert-managed edges or a generated admin cert, cert-manager. The operator **detects** these
  and waits for them — it never installs them. See [docs/quickstart.md](docs/quickstart.md).

### OpenShift

The operator and every workload it renders run under OpenShift's default `restricted-v2` SCC
unchanged — non-root, no hard-coded UID (OpenShift assigns the namespace's range), all
capabilities dropped, `seccompProfile: RuntimeDefault` — so no custom SCC or extra RBAC is
required. Install via OperatorHub (the OLM bundle) or Helm. For the edge use `edge.type: ingress`
(the OpenShift router reconciles the Ingress and publishes the Route) or `gatewayAPI`; a native
OpenShift `Route` edge type is not yet supported. See
[docs/platform.md → Running on OpenShift](docs/platform.md#running-on-openshift).

## Development

Requires Go 1.26+, Docker (or a compatible runtime), and Kind for local testing.

The inner development loop is `make test` (envtest + unit tests) and `make lint`. The Kind
end-to-end suite is a pre-release / nightly **gate**, not the iteration loop.

| Target | Description |
|--------|-------------|
| `make build` | Build the operator binary |
| `make test` | Run unit and integration tests (envtest) |
| `make lint` | Run golangci-lint (config: `.golangci.yml`) |
| `make coverage` | Run tests and verify the 80% coverage threshold |
| `make manifests` | Generate CRDs and RBAC manifests |
| `make generate` | Generate DeepCopy methods |
| `make docker-build` | Build the Docker image |
| `make bundle` | Generate and validate the OLM bundle |
| `make kind-cluster` | Create a Kind cluster for development |
| `make kind-load IMG=ilm-operator:dev` | Load the operator image into Kind |
| `make test-e2e` | Run the connector end-to-end suite on Kind |
| `make test-e2e-managed` | Run the fully-managed platform end-to-end suite on Kind |
| `make trivy` / `make trivy-fs` | Trivy vulnerability scan (image / filesystem) |
| `make sonar` | Run local SonarQube analysis |

### Run locally against a cluster

```bash
make install      # install the CRDs into the cluster in ~/.kube/config
make run          # run the operator outside the cluster
```

## Project structure

```
api/v1alpha1/          CRD types: connector_types.go, platform_types.go, common_types.go
cmd/
  main.go              Operator entrypoint
  values2platform/     Helm-values → Platform CR migration converter
internal/
  builder/
    common/            CRD-agnostic builders (Component model, Deployment/Service/HPA/…, SCC-clean security)
    connector/         Connector-specific builders
    platform/          Platform-specific builders (component resolution, edge, infra CRs)
  bom/                 Versioned bill-of-materials (image coordinates + wiring + managed topology)
  checksum/            Configuration checksum utility (drift detection)
  controller/
    connector/         Connector reconciler
    platform/          Platform reconciler (gates, prune, OIDC, lifecycle)
  platform/            capabilities/ — the generic RESTMapper-based upstream-CRD detector
  registration/        ILM platform registration client (connector → platform) + OIDC wiring
  monitoring/          Prometheus metrics + event recorder helpers
  version/             Build version info (injected via ldflags)
config/                CRDs, RBAC, samples, OLM manifests, scorecard
deploy/charts/         Helm chart for operator deployment
bundle/                OLM bundle (generated by make bundle)
test/e2e/              End-to-end tests
docs/                  Project documentation (see the table above)
```

## Architecture

The operator follows the standard controller-runtime pattern. Each controller watches its CR
and reconciles the desired state, delegating resource construction to pure builder functions.

```
Platform / Connector CR --> Reconciler --> Builders --> Kubernetes resources
                                |                        (+ upstream-operator CRs for managed infra)
                                +--> capability detection & gating
                                +--> Secret/ConfigMap watch (config-drift)
                                +--> status conditions & events
```

The Platform reconciler render-applies all of its owned children via **Server-Side Apply**
with a stable field manager, so it manages only the fields it sends and co-owned resources
(HPA-scaled workloads, upstream CRs) are never clobbered. The Connector reconciler uses
`controllerutil.CreateOrUpdate` for its simpler single-owner case.

## License

This project is licensed under the MIT License — see the [LICENSE](LICENSE) file for details.
