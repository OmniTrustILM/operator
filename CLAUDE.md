# ILM Operator - Development Guide

## Project Overview

The ILM Operator is a standalone Kubernetes operator that manages the ILM platform and its connectors via Custom Resource Definitions (CRDs). One binary, three independent controllers.

- **API Group:** `otilm.com`
- **Kinds:** `Connector`, `Platform`, and `Proxy` (all v1alpha1) — all implemented
- **Tooling:** Operator SDK v1.42.2, Kubebuilder v4, controller-runtime
- **Language:** Go 1.26+

## Contributing & releases

`main` is the development line (the chart stays `X.Y.Z-develop`; releases exist only as tags cut from `release/*`). The contributor workflow is in [CONTRIBUTING.md](CONTRIBUTING.md); the release runbook is in [docs/release-process.md](docs/release-process.md).

## Quick Start Commands

```bash
# Build the operator binary
make build

# Run unit and integration tests
make test

# Run linter (uses .golangci.yml config)
make lint

# Build Docker image
make docker-build

# Generate manifests (CRDs, RBAC)
make manifests

# Generate deep-copy methods
make generate

# Generate and validate OLM bundle
make bundle
```

## Quality Verification Workflow

Before committing or pushing changes, run the full quality check sequence:

```bash
# 1. Lint — must be 0 issues (config: .golangci.yml)
make lint

# 2. Tests — all must pass
make test

# 3. Coverage — must be >= 80%
make coverage

# 4. Helm chart — must lint cleanly
helm lint deploy/charts/ilm-operator

# 5. OLM bundle — must validate
make bundle

# 6. Docker image — must build
make docker-build

# 7. Trivy vulnerability scan — no HIGH/CRITICAL vulnerabilities
#    The targets pass explicit flags (TRIVY_FLAGS in the Makefile) that mirror the
#    org-default policy the shared CI workflow applies — it neutralizes repo-local
#    Trivy config, so there is deliberately no config/trivy.yaml to drift from it.
#    If vulnerabilities are found in Go dependencies, fix them with:
#      go get <package>@latest && go mod tidy
make trivy
#    Or scan just the filesystem (faster, no Docker build needed):
make trivy-fs

# 8. SonarQube (optional, runs ephemeral Docker container)
#    Checks: coverage, duplication (<3%), code smells, security hotspots
make sonar
```

In CI (GitHub Actions), the following checks run automatically:
- `golangci-lint` with checkstyle report uploaded for SonarCloud
- Unit + integration tests with coverage threshold verification
- SonarCloud analysis (on push to main, or via dispatch pattern for PRs)
- E2E tests on Kind cluster (after lint + test pass)
- Docker image build + Trivy vulnerability scan
- Helm chart lint + install test

## Linting Configuration

The linter config is at `.golangci.yml` (golangci-lint v2 format). Key linters enabled:
- `errcheck`, `govet`, `staticcheck` — correctness
- `gosec` — security
- `dupl` — code duplication (threshold: 100 tokens)
- `goconst` — repeated string literals
- `revive` — style and documentation
- `noctx`, `bodyclose` — HTTP best practices
- `misspell`, `unconvert`, `unparam` — code quality

Exclusions: `dupl` and `gosec` are suppressed in test files; `noctx` is suppressed in `test/`; `zz_generated` files are excluded entirely.

## Testing Commands

**Development loop vs. release gate.** The fast inner loop is `make test` (envtest + unit, ~1 min, deterministic) and `make lint` — validate every change here and move on. The Kind **e2e is a pre-release / nightly gate, NOT the development loop**: never iterate against it (tens of minutes, real upstream operators, a single failure is slow to surface). Controller/builder bugs are deterministic — when the e2e catches one, the fix is to add a fast envtest/builder assertion so the inner loop catches that class next time, then move on. Run `make test-e2e-managed` intentionally, as a final gate (it is `ContinueOnFailure`, so one run reports every failure).

```bash
# Run unit + integration tests (envtest)
make test

# Run E2E tests on Kind cluster
make test-e2e

# Run tests with coverage verification (80% threshold)
make coverage

# Create Kind cluster for development
make kind-cluster

# Load operator image into Kind
make kind-load IMG=ilm-operator:dev

# Delete Kind cluster
make prune-kind-cluster

# Export Kind cluster logs for debugging
make kind-export-logs

# Run local SonarQube analysis (ephemeral Docker container)
# Starts SonarQube, runs scanner, prints results, stops container
make sonar
```

## Project Structure

```
api/v1alpha1/          - CRD types: connector_types.go, platform_types.go, proxy_types.go, and common_types.go (shared building-block specs: ImageSpec, EnvVar, Secret/ConfigMap refs + key mappings, ServiceSpec, ProbeSpec, SecurityContextSpec, PDBSpec, VolumeSpec, MetricsSpec)
cmd/
  main.go              - Operator entrypoint
  values2platform/     - Helm-values → Platform CR migration converter (CLI)
internal/
  builder/
    common/            - CRD-agnostic builders: Component render model, ResolveImage, Build{Deployment,StatefulSet,Service,ServiceAccount,PDB,HPA,ServiceMonitor}, SCC-clean pod security
    connector/         - Connector-specific builders (Deployment, Service, SA, PDB, ServiceMonitor)
    platform/          - Platform-specific builders (component resolution, edge, managed-infra CRs)
    proxy/             - Proxy-specific builders (token-only Deployment, Service with http+api ports, SA, PDB, ServiceMonitor at /metrics)
  checksum/            - Configuration checksum utility for drift detection
  controller/
    connector/         - Connector reconciler (+ watches)
    platform/          - Platform reconciler (capability gates, prune, OIDC wiring, lifecycle, the managed-infra upgrade guard, the messaging-migration phase machine: fence → drain → cutover → cleanup)
    proxy/             - Proxy reconciler — pure consumer of the provisioning-issued config token; no platform calls
  monitoring/          - Prometheus metrics registration + event recorder helpers
  rabbitmq/            - Minimal RabbitMQ Management API client (drain-criteria reads for the messaging migration; credentials from the managed cluster's Secret, never logged)
  registration/        - ILM platform registration client (connector → platform) + OIDC wiring
  version/             - Build version info (injected via ldflags)
pkg/                   - Importable packages (the operator's public surface, also consumed by the CLI)
  bom/                 - Versioned bill-of-materials: per-component image coordinates + the wiring profile (env-var names, connection-string template, Secret-key names) + managed topology, as data
  capabilities/        - Generic RESTMapper-based upstream-CRD detector (reused by the Platform controller's managed-infra gates)
  convert/             - Helm-values → Platform CR conversion used by cmd/values2platform
config/
  crd/bases/           - Generated CRD YAML (connectors + platforms + proxies)
  rbac/                - Generated RBAC roles
  samples/             - Example Connector, Platform, and Proxy CRs (README.md indexes the variants)
  manifests/           - OLM CSV base and kustomization
  scorecard/           - OLM scorecard test configuration
deploy/charts/         - Helm chart for operator deployment
bundle/                - OLM bundle (generated by make bundle)
test/e2e/              - End-to-end tests
docs/
  site/                - Synced user guide: five top-level pages (the overview router; installation = the operator install only; upgrading; migration-from-helm; troubleshooting) + the four CR guides under custom-resources/ (platform (first run + scenario guide) + platform-options, connector, proxy)
  design/              - Design specs
hack/                  - Helper scripts (sonar-local.sh)
.golangci.yml          - Linter configuration (golangci-lint v2)
sonar-project.properties - SonarCloud/SonarQube configuration
```

## Code Style Guidelines

- Follow standard Go conventions and `go fmt`
- Use `golangci-lint` (v2) for linting — config in `.golangci.yml`
- Use table-driven tests with `testify` assertions
- Use `controller-runtime` logging (`logr`) — no `fmt.Println`
- Keep reconciler logic thin; delegate to builder packages
- All exported types and functions must have doc comments
- Builders are pure functions: given a component/connector spec, return a K8s resource
- Prefer **Server-Side Apply** (with a stable field manager, e.g. `ilm-operator`) for the render-then-apply model: the Platform reconciler render-applies *all* its owned children (Deployments/Services/ServiceAccounts/ConfigMaps) via SSA, so it manages only the fields it sends and API-defaulted fields never churn. SSA is also required for resources **co-owned** with other actors — HPA-managed workloads (omit `.spec.replicas` so the HPA owns scaling) and upstream-operator CRs — so the operator never clobbers fields it does not own. `controllerutil.CreateOrUpdate` remains fine for simple single-owner cases (e.g. the Connector reconciler, or a reconcile-time composed Secret)
- Use `meta.SetStatusCondition` for condition management (set `observedGeneration` on each condition)

## Platform operator

The operator manages the **ILM platform itself** via the `Platform` CRD, alongside `Connector` and `Proxy`. Design: `docs/design/platform-operator.md`; CR examples + reference: `docs/design/examples/`; end-user guides: `docs/site/` is now canonical. The synced set is **nine pages**: five top-level, in `sidebar_position` order — `overview.md` (a router: what the operator is, the three CRs, and a routing table; it restates nothing another page owns), `installation.md` (the operator install only — requirements, upstream prerequisites, install channels, verify, upgrade, removal), `upgrading.md`, `migration-from-helm.md`, `troubleshooting.md` — plus the four CR guides under `custom-resources/` (the site's Custom Resources sidebar group), in their own `sidebar_position` order — `platform.md` (the first-run walkthrough `Run your first platform` + the scenario guide), `platform-options.md` (the `Platform` field index), `connector.md`, `proxy.md`. The set was absorbed from six pre-absorption originals under `docs/` and then restructured: `quickstart.md` + `platform.md` → `installation.md`, whose first-platform walkthrough later moved on to `platform.md`; `configuration.md` → `platform.md` + `platform-options.md`; `upgrades.md` + `versions.md` → `upgrading.md`; `platform.md`'s observe/degraded half → `troubleshooting.md`; `connector.md` and `proxy.md` are new per-CR guides; only `migration-from-helm.md` keeps its original name. Those six originals are now stubs that redirect into `docs/site/`; write user-facing documentation only under `docs/site/`. Invariants to uphold in platform code:

- **No secrets in CRs.** Sensitive values are `Secret` references only (`*SecretRef`) — never inline. Read referenced Secrets read-only and inject via `secretKeyRef`/`envFrom` (never copy values into rendered objects). Never put secret values *or* connection coordinates (host/port/URI) in status, conditions, events, or logs.
- **SCC-clean pods (OpenShift `restricted-v2`).** `runAsNonRoot: true`, **no hard-coded `runAsUser`**, drop **all** capabilities, `seccompProfile: RuntimeDefault`, no privilege escalation.
- **Deletion safety.** Add the finalizer first; `spec.deletionPolicy` (default `Retain`) must leave managed (upstream-operator) infrastructure and its data intact. Clean cluster-scoped artifacts (webhook config, ClusterRoles) via finalizer/labels — not owner-reference GC (cross-namespace ownerRefs are invalid).
- **Stateful infra is delegated** to upstream operators (CloudNativePG; RabbitMQ Cluster + Messaging Topology; Keycloak) when `managed`, or referenced when `external` — never re-templated in Go, never via the Helm SDK.
- **Upstream-operator dependencies are detected (RESTMapper) and gated, never assumed.** A required upstream CRD that is not served means skip the dependent objects, surface a non-fatal `False` condition with an actionable reason, and requeue to self-heal — not a cryptic apply failure or a whole-Platform `Degraded`. cert-manager is external/prerequisite (cluster-singleton, never installed by the operator) and required only for cert-managed edge modes (`tls.source` `internal`/`letsEncrypt`); Gateway API CRDs only for `type=gatewayAPI`; BYO `tls.source=secret` needs neither. The detector (`pkg/capabilities`) is generic and reused for the managed-infra (CNPG/RabbitMQ/Keycloak) checks.
- **Managed-broker version moves migrate, never clobber.** When a version change renames the managed vhost/exchanges, the Platform controller runs the messaging-migration phase machine (Fencing → Draining → CuttingOver → CleaningUp, recorded in `status.upgrade`): producers are fenced to zero, the source vhost must drain (bounded by `drainTimeout`, abortable by reverting `spec.version`), and nothing from the target renders before the cutover. Hard-won invariants: a status write must never clobber an in-memory spec re-pin (`writeStatus` preserves `p.Spec`); the cutover must release every fenced workload Core's init containers poll (provisioning AND scheduler) before rolling Core — enforced by the `CoreInitServiceDependencies` class-invariant test; a fenced workload may be released only once its LIVE pod template is the target's (the gate runs before the pass's apply, so releasing early would restart a producer on the source template); and a workload the cutover waits on that resolves to 0 replicas is surfaced as an actionable error, never waited on (the phase has no deadline).
- **Admission validation.** The CRD's CEL `XValidation` rules are the create-time guard today (e.g. an external DB missing host/credentials, an enabled edge missing host, `provisioning.mode=deploy` on a non-rabbitmq broker); the per-namespace `Platform` singleton is a runtime guard (`AnotherPlatformExists`). A validating webhook that also rejects inline secrets and enforces the singleton at create-time (self-managed serving cert, `failurePolicy: Fail`) is future work — keep CR-level invariants enforced by CEL until then.

### Released vs preview version bundles

Each platform version is a bundle in `pkg/bom/bom.go` carrying a `Released` flag. A **preview** bundle (`Released: false`) is one whose platform artifacts are not published yet: it exists so the version contract can land, be reviewed and be rendered ahead of release day, and it is deliberately hard to reach.

- It resolves **only** via an explicit `spec.version`; `SupportedVersions()` excludes it, so it never appears in advertised or defaulted output.
- `DefaultVersion` must name a **released** bundle (`TestDefaultVersionIsReleased` enforces this) — an empty `spec.version` can never land on a preview.
- A **live** platform reaches a preview only by the same explicit `spec.version` opt-in, and the move is governed like any other version move — including the messaging migration when the managed topology changes between the two bundles. There is no separate preview-upgrade guard.

Release day is a data-only flip: set `Released: true` on the bundle and move `DefaultVersion` to it. Treat a preview bundle as opt-in, maintainer-facing surface — samples that use one must say so.

## Quality Requirements

- **Code coverage:** minimum 80% overall (enforced by `make coverage`)
  - `internal/builder/` — target 90%+ (pure functions)
  - `internal/checksum/` — target 95%+ (pure functions)
  - `internal/controller/` — target 80%+ (envtest-based)
  - `internal/registration/` — target 90%+ (mocked HTTP)
  - `pkg/capabilities` — target 90%+ (RESTMapper-based, not HTTP)
- **Code duplication:** less than 3% (enforced by SonarCloud/SonarQube)
- **Linting:** zero warnings from `make lint`
- **SonarQube:** Quality Gate must pass (0 issues target)
- **Helm chart:** must pass `helm lint`
- **OLM bundle:** must pass `operator-sdk bundle validate ./bundle`
- **Tests:** all tests must pass before committing

## How to add a new CRD (the per-kind pattern)

The operator is structured so a new Kind follows the same shape as `Connector`, `Platform`, and `Proxy` (`Proxy` was added exactly this way). The pattern, end to end:

1. **Types** — add `api/v1alpha1/<kind>_types.go` (Spec/Status, kubebuilder markers, printer columns). Reuse the shared building-block specs in `common_types.go` (`ImageSpec`, `EnvVar`, ref + key-mapping types, `ProbeSpec`, `MetricsSpec`, …) rather than re-declaring them. Run `make generate manifests`.
2. **Builders** — put pure, unit-tested builder functions under `internal/builder/<kind>/`, composing the CRD-agnostic primitives in `internal/builder/common/` (the Component render model, `ResolveImage`, the Deployment/StatefulSet/Service/SA/PDB/HPA/ServiceMonitor builders, SCC-clean pod security). Builders take a spec and return a K8s object — no client calls.
3. **Controller** — add `internal/controller/<kind>/` with a thin reconciler: `For(<Kind>)`, `Owns(...)` the children, `Watches(Secret/ConfigMap)` for config-drift. Delegate all rendering to the builders; use `meta.SetStatusCondition` (with `observedGeneration`) for status.
4. **Apply strategy** — prefer **Server-Side Apply** (stable field manager `ilm-operator`, `ForceOwnership`) for a render-then-apply, multi-child or co-owned model; `controllerutil.CreateOrUpdate` is fine for a simple single-owner case.
5. **Capabilities** — gate any dependency on an upstream CRD through the generic detector in `pkg/capabilities` (skip + non-fatal condition + requeue, never assume-and-fail).
6. **Wire it up** — register the scheme and the reconciler in `cmd/main.go`; add RBAC markers; add samples under `config/samples/`.
7. **Tests** — table-driven builder unit tests (target 90%+) and an envtest controller suite (reconcile, drift, watch, finalizer, conditions). `make test` is the loop; the Kind e2e is the gate.

## Design Specs

- `docs/design/connector-operator.md` — the `Connector` CRD: schema, reconciliation flow, architecture.
- `docs/design/platform-operator.md` — the `Platform` CRD: architecture, security model, infra delegation (`external`/`managed`), pluggable edge, reconciliation. Validated against production operators.
- `docs/design/platform-versioning.md` — the managed-infra version/BOM model and upgrade-guard design.
- `docs/design/proxy-operator.md` — the `Proxy` CRD: config-token contract, credential delivery, reconciliation, Connector `proxyRef` future work.
- `docs/design/examples/` — the annotated `Platform` CR field reference + worked example shapes.
