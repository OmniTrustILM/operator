# ILM Connector Operator - Development Guide

## Project Overview

The ILM Connector Operator is a Kubernetes operator that manages ILM platform connectors via Custom Resource Definitions (CRDs).

- **API Group:** `otilm.com`
- **Kind:** `Connector` (v1alpha1)
- **Tooling:** Operator SDK, Kubebuilder, controller-runtime
- **Language:** Go 1.25+

## Quick Start Commands

```bash
# Build the operator binary
make build

# Run unit and integration tests
make test

# Run linter
make lint

# Build Docker image
make docker-build

# Generate manifests (CRDs, RBAC)
make manifests

# Generate deep-copy methods
make generate
```

## Testing Commands

```bash
# Run unit + integration tests
make test

# Run E2E tests (requires Kind)
make test-e2e

# Run tests with coverage verification (80% threshold)
make coverage

# Create Kind cluster for development
make kind-cluster

# Load operator image into Kind
make kind-load

# Delete Kind cluster
make prune-kind-cluster

# Export Kind cluster logs for debugging
make kind-export-logs
```

## Project Structure

```
api/v1alpha1/          - CRD type definitions (ConnectorSpec, ConnectorStatus)
cmd/main.go            - Operator entrypoint
internal/
  builder/             - Kubernetes resource builders (Deployment, Service, SA, PDB, ServiceMonitor)
  checksum/            - Configuration checksum utility for drift detection
  controller/          - Connector reconciler (main control loop)
  monitoring/          - Prometheus metrics registration
  platform/            - ILM platform registration client
  version/             - Build version info (injected via ldflags)
config/
  crd/bases/           - Generated CRD YAML
  rbac/                - Generated RBAC roles
  samples/             - Example Connector CRs
deploy/charts/         - Helm chart for operator deployment
test/e2e/              - End-to-end tests
```

## Code Style Guidelines

- Follow standard Go conventions and `go fmt`
- Use `golangci-lint` (v2) for linting -- config in `.golangci.yml`
- Use table-driven tests with `testify` assertions
- Use `controller-runtime` logging (`logr`) -- no `fmt.Println`
- Keep reconciler logic thin; delegate to builder packages
- All exported types and functions must have doc comments

## Quality Requirements

- **Code coverage:** minimum 80% (enforced by `make coverage`)
- **Code duplication:** less than 3%
- **Linting:** zero warnings from `golangci-lint run`
- **Tests:** all tests must pass before committing

## Design Spec

See `docs/superpowers/specs/2026-04-11-ilm-connector-operator-design.md` for the full design specification including CRD schema, reconciliation flow, and architecture decisions.
