# Deploying the ILM Operator

There are three ways to install the operator. All of them install the CRDs
(`connectors.otilm.com`, `platforms.otilm.com`) alongside the controller.

| Path | Best for | Source of truth |
|------|----------|-----------------|
| **`kubectl apply` release manifest** | Quick, opinionated install with no extra tooling | Generated from `config/` per release |
| **Helm chart** (`charts/ilm-operator`) | Configurable installs (namespace, replicas, ServiceMonitor, PDB, image overrides) | This directory |
| **OLM bundle** | OperatorHub / OpenShift OperatorHub | `config/manifests` + `bundle/` |

## `kubectl apply` release manifests

Each tagged release publishes two flat manifests as **GitHub release assets** (the same model
cert-manager, CloudNativePG, and the RabbitMQ operator use):

- **`ilm-operator.yaml`** — self-contained install: Namespace (`ilm-operator-system`), both
  CRDs, RBAC, the ServiceAccount, the manager Deployment, and the metrics Service, with the
  operator image pinned to that release. Applying it needs nothing but `kubectl`; cert-manager
  is *not* required to install the operator (it is only a prerequisite for cert-managed
  platform edges).
- **`ilm-operator.crds.yaml`** — the two CRDs only, for CRD-first / GitOps installs.

The operator image is published to a public registry, so the manifest needs no image pull
secret — `kubectl apply` works on any cluster:

```bash
kubectl apply -f https://github.com/OmniTrustILM/operator/releases/download/<version>/ilm-operator.yaml
```

These flat manifests are **not committed** to the repository — they are generated at release
time from `config/` (the same kustomize bases as `make deploy`) so they can never drift from
the operator code, and uploaded by [`.github/workflows/release.yaml`](../.github/workflows/release.yaml)
once the matching operator image has been published.

To produce them locally (e.g. to install a development build):

```bash
make build-installer      IMG=<your-registry>/ilm/operator:<tag>   # -> dist/install.yaml
make build-installer-crds                                          # -> dist/install-crds.yaml
```

### Upgrade

Apply the new release's manifest with **server-side apply** (the Platform CRD is large; server-side
apply avoids the client-side last-applied-annotation size limit and cleanly adopts existing objects):

```bash
kubectl apply --server-side --force-conflicts \
  -f https://github.com/OmniTrustILM/operator/releases/download/<new-version>/ilm-operator.yaml
```

### Uninstall

```bash
kubectl delete -f https://github.com/OmniTrustILM/operator/releases/download/<version>/ilm-operator.yaml
```

> **Warning:** the manifest includes the CRDs. Deleting it removes the `Connector` and `Platform`
> CRDs, which **cascade-deletes every Connector/Platform CR** in the cluster. To remove only the
> controller and keep your CRs, delete the namespace instead
> (`kubectl delete namespace ilm-operator-system`) and leave the CRDs in place.

### Verifying the assets

Each release also publishes `checksums.txt` and cosign signatures (`*.sig`). Verify integrity:

```bash
sha256sum -c checksums.txt           # run in a dir containing the downloaded manifests
```

and authenticity with the project's cosign public key (`cosign.pub`):

```bash
cosign verify-blob --key cosign.pub \
  --signature ilm-operator.yaml.sig ilm-operator.yaml
```

## Helm chart

The chart in [`charts/ilm-operator`](charts/ilm-operator) is the configurable install path and
the source of truth for it. See its [README](charts/ilm-operator/README.md).

```bash
helm install ilm-operator deploy/charts/ilm-operator \
  --namespace ilm-system --create-namespace
```
