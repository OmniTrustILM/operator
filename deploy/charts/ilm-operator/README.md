# ILM Operator Helm Chart

> This repository is part of the open-source project ILM. You can find more information about the project at [ILM Operator](https://github.com/OmniTrustILM/operator) repository, including the contribution guide.

This Helm chart deploys the ILM Connector Operator, which manages ILM platform connectors via Kubernetes Custom Resource Definitions (CRDs).

## Prerequisites

- Kubernetes 1.28+
- Helm 3.8.0+

## Using this Chart

### Installation

**Create `values.yaml`**

> **Note**
> You can also use `--set` options for the helm to apply configuration for the chart.

Copy the default `values.yaml` from the Helm chart and modify the values accordingly:
```bash
helm show values oci://harbor.3key.company/ilm-helm/ilm-operator > values.yaml
```
Now edit the `values.yaml` according to your desired state, see [Configurable parameters](#configurable-parameters) for more information.

**Install**

For the basic installation, run:
```bash
helm install --namespace ilm-system --create-namespace -f values.yaml ilm-operator oci://harbor.3key.company/ilm-helm/ilm-operator
```

By default, the chart will install the CRDs required for the operator to work properly. If you want to skip the installation of the CRDs, you can use the `--set crd.install=false` option.

### Upgrade

> **Warning**
> Be sure that you always save your previous configuration!

For upgrading the installation, update your configuration and run:
```bash
helm upgrade --namespace ilm-system -f values.yaml ilm-operator oci://harbor.3key.company/ilm-helm/ilm-operator
```

### Uninstall

You can use the `helm uninstall` command to uninstall the application:
```bash
helm uninstall --namespace ilm-system ilm-operator
```

## Configurable parameters

You can find current values in the [values.yaml](values.yaml).
You can also specify each parameter using the `--set` or `--set-file` argument to `helm install`.

The following values may be configured:

### Global

| Parameter    | Default value | Description                      |
|--------------|---------------|----------------------------------|
| replicaCount | `1`           | Number of operator replicas      |
| nodeSelector | `{}`          | Node selector for operator pods  |
| tolerations  | `[]`          | Tolerations for operator pods    |
| affinity     | `{}`          | Affinity rules for operator pods |

### Image

| Parameter         | Default value  | Description                               |
|-------------------|----------------|-------------------------------------------|
| image.registry    | `docker.io`    | Docker registry for the operator image    |
| image.repository  | `otilm`        | Docker image repository                   |
| image.name        | `ilm-operator` | Docker image name                         |
| image.tag         | `""`           | Docker image tag (defaults to appVersion) |
| image.pullPolicy  | `IfNotPresent` | Image pull policy                         |
| image.pullSecrets | `[]`           | Array of secret names for image pull      |

### CRD

| Parameter   | Default value | Description                         |
|-------------|---------------|-------------------------------------|
| crd.install | `true`        | Install/upgrade CRDs with the chart |

### Service Account & RBAC

| Parameter                  | Default value | Description                                             |
|----------------------------|---------------|---------------------------------------------------------|
| serviceAccount.create      | `true`        | Create service account for the operator                 |
| serviceAccount.name        | `""`          | Service account name (auto-generated if empty)          |
| serviceAccount.annotations | `{}`          | Annotations for the service account                     |
| rbac.create                | `true`        | Create RBAC resources (ClusterRole, ClusterRoleBinding) |

### Operator Configuration

| Parameter                              | Default value | Description                               |
|----------------------------------------|---------------|-------------------------------------------|
| leaderElection.enabled                 | `true`        | Enable leader election for HA deployments |
| resources.requests.cpu                 | `100m`        | CPU request for operator pod              |
| resources.requests.memory              | `128Mi`       | Memory request for operator pod           |
| resources.limits.memory                | `256Mi`       | Memory limit for operator pod             |
| securityContext.runAsNonRoot           | `true`        | Run operator as non-root user             |
| securityContext.readOnlyRootFilesystem | `true`        | Read-only root filesystem                 |

### Pod Disruption Budget

| Parameter                        | Default value | Description                 |
|----------------------------------|---------------|-----------------------------|
| podDisruptionBudget.enabled      | `false`       | Enable PDB for the operator |
| podDisruptionBudget.minAvailable | `1`           | Minimum available pods      |

### Metrics

| Parameter                      | Default value | Description                      |
|--------------------------------|---------------|----------------------------------|
| metrics.enabled                | `false`       | Enable metrics endpoint          |
| metrics.service.port           | `8080`        | Metrics service port             |
| metrics.serviceMonitor.enabled | `false`       | Create Prometheus ServiceMonitor |
