# Quickstart — a running ILM platform in a few commands

This is the fastest path to a running ILM platform: an **everything-managed** deployment
where the operator provisions PostgreSQL, RabbitMQ, and Keycloak for you, and bootstraps the
first admin from a cert-manager-issued certificate. **There are no Secrets to pre-create** —
the upstream operators mint their own credentials and the ILM operator reads them back and
wires every component.

It uses [`config/samples/platform_quickstart.yaml`](../config/samples/platform_quickstart.yaml)
unchanged (except the public hostname).

## 1. Install the ILM operator

Pick one path (the CRDs install with the operator):

```bash
# kubectl apply — self-contained release manifest (installs into ilm-operator-system)
kubectl apply -f https://github.com/OmniTrustILM/operator/releases/download/<version>/ilm-operator.yaml

# …or Helm (configurable; pick your own namespace)
helm install ilm-operator deploy/charts/ilm-operator \
  --namespace ilm-system --create-namespace

# …or from a checkout
make deploy IMG=<registry>/ilm-operator:<tag>
```

Replace `<version>` with a [release tag](https://github.com/OmniTrustILM/operator/releases).
The release manifest needs nothing but `kubectl` — cert-manager is only a prerequisite for
cert-managed platform edges (step 3), not for installing the operator.

Confirm the CRDs are registered:

```bash
kubectl get crd platforms.otilm.com connectors.otilm.com
```

## 2. Install the upstream operators (the only prerequisites)

The everything-managed quickstart delegates the stateful infrastructure to upstream
operators. The ILM operator **detects** each of them and waits if one is missing — it never
fails — so you can install them before or after applying the Platform.

**Recommended — one command, pinned to the validated versions, idempotent:**

```bash
make install-upstream-operators
```

This installs cert-manager (first), CloudNativePG, the RabbitMQ Cluster + Messaging Topology
operators, and the Keycloak Operator at the exact versions the operator's e2e validates
against, waits for each to become Available, and prints a readiness summary. Re-running it is
safe (it re-applies and re-verifies). To check an existing cluster without changing anything:

```bash
make verify-upstream-operators
```

<details><summary>Or install manually with <code>kubectl apply</code> (pinned versions)</summary>

```bash
# cert-manager — install FIRST: the Messaging Topology Operator's webhook and the platform's
# internal-CA edge both depend on it. The ILM operator never installs cert-manager itself.
kubectl apply -f \
  https://github.com/cert-manager/cert-manager/releases/download/v1.20.2/cert-manager.yaml

# CloudNativePG (managed PostgreSQL)
kubectl apply --server-side -f \
  https://github.com/cloudnative-pg/cloudnative-pg/releases/download/v1.29.1/cnpg-1.29.1.yaml

# RabbitMQ Cluster Operator + Messaging Topology Operator (managed messaging)
kubectl apply -f \
  https://github.com/rabbitmq/cluster-operator/releases/download/v2.21.0/cluster-operator.yml
kubectl apply -f \
  https://github.com/rabbitmq/messaging-topology-operator/releases/download/v1.19.2/messaging-topology-operator-with-certmanager.yaml

# Keycloak Operator — installed as published in 'keycloak'.
KC=https://raw.githubusercontent.com/keycloak/keycloak-k8s-resources/26.6.3/kubernetes
kubectl create namespace keycloak --dry-run=client -o yaml | kubectl apply -f -
for f in keycloaks.k8s.keycloak.org-v1.yml keycloakrealmimports.k8s.keycloak.org-v1.yml kubernetes.yml; do
  kubectl apply -n keycloak -f "$KC/$f"
done
# Then widen it to watch ALL namespaces so a Platform in 'ilm' is reconciled. This needs the
# operator's full RBAC (controller + operational) bound cluster-wide plus JOSDK_ALL_NAMESPACES —
# several objects, so just use the installer, which does exactly this:
#   make install-upstream-operators       (or: ./hack/install-upstream-operators.sh)
```

</details>

> **Managed Keycloak needs a Keycloak Operator watching the Platform's namespace.** The Keycloak
> Operator ships namespace-scoped (unlike the cluster-scoped CloudNativePG / RabbitMQ operators),
> so it is installed as published in `keycloak` and then configured to **watch all namespaces** —
> one operator then serves a Platform in any namespace, the same cluster-scoped model as the
> others. Watching other namespaces requires the operator's controller **and** operational RBAC
> bound cluster-wide (the published manifest grants the latter only via a namespaced Role), which
> is why `make install-upstream-operators` is the recommended path. See the
> [Keycloak Operator install docs](https://www.keycloak.org/operator/installation).

> Want a no-cert-manager first run? Drop the `registerAdmin` block from the sample and skip
> cert-manager — and either drop the `edge` block (run behind your own ingress) or set its
> `tls.source: secret` with a bring-your-own TLS Secret.

## 3. Apply the Platform

```bash
kubectl create namespace ilm
# edit common.hostName / edge.host to your FQDN first
kubectl apply -f config/samples/platform_quickstart.yaml
```

That is the whole sample — everything-managed, zero Secrets:

```yaml
apiVersion: otilm.com/v1alpha1
kind: Platform
metadata: { name: ilm, namespace: ilm }
spec:
  common:
    hostName: ilm.example.com         # the platform's canonical public FQDN
  database:
    mode: managed                     # CloudNativePG mints the app Secret
    managed: { instances: 1, version: "18", storage: { size: 20Gi } }
    pgBouncer: { managed: true }                  # pool the managed DB (else Postgres exhausts connections)
  messaging:
    mode: managed                     # the Topology Operator mints the per-user Secrets
    brokerType: rabbitmq
    managed: { replicas: 1, storage: { size: 10Gi } }   # version omitted → operator deploys its matched image
  keycloak:
    mode: managed                     # the Keycloak Operator mints the admin Secret;
    realm: ilm                        #   the OIDC client secret is generated + relayed
    managed: { instances: 1 }         # version omitted → operator deploys its matched image
  registerAdmin:                      # first admin from a cert-manager-issued client cert
    enabled: true
    username: admin
    name: Platform Administrator       # first name
    lastName: Administrator            # surname (else Keycloak prompts to complete the profile at first login)
    email: admin@example.com
    certificate:                      # the mTLS method (defaults on); see platform.md for the
      source: generated               #   password method (a Keycloak realm user) alternative
  edge:                               # optional turnkey internal-CA TLS edge (needs cert-manager)
    enabled: true                     # edge.host omitted → the edge uses common.hostName
    type: ingress
    className: nginx
    tls: { source: internal }         # operator-managed internal CA
  gateway:                            # REQUIRED behind the edge: trust the ingress so Kong builds
    trustedIps: ["0.0.0.0/0", "::/0"] #   https://<host> OAuth redirects (scope to your CIDR in prod)
```

No credentials are ever inlined in the CR, status, conditions, events, or logs.

## 4. Watch it converge

```bash
kubectl get platform -n ilm -w
# NAME   PHASE         VERSION   READY   AGE
# ilm    Progressing   2.18.0    False   …
# ilm    Running       2.18.0    True    …
```

`READY` is the `Available` condition (Core + auth ready). Until an upstream operator
appears, the matching adjunct condition (`DatabaseReady` / `MessagingReady` / `KeycloakReady`)
reports `…NotInstalled` and the platform **waits** — it never fails, and converges with no
operator restart once the operator is installed.

```bash
kubectl describe platform ilm -n ilm        # conditions, including managed-infra readiness
kubectl get deploy -n ilm                   # core, auth, fe-administrator, api-gateway, …
```

## 5. Access the platform

Once `PHASE=Running`, everything is reachable through the **edge** at your `common.hostName`,
over HTTPS, via the API gateway:

| What | URL |
|------|-----|
| ILM administration UI | `https://<host>/administrator/` |
| Keycloak admin console (managed Keycloak) | `https://<host>/kc/admin/` |
| Keycloak OIDC issuer | `https://<host>/kc/realms/ilm` |
| RabbitMQ management UI | `https://<host>/mq/` — only when `messaging.management.expose: true` (the quickstart leaves it off) |

The managed Keycloak is served under `/kc`, and the gateway routes `/kc` to it, so browser login
and the admin console both work through the single public host. The login pages render the **ILM
theme** out of the box (the operator applies it for a managed Keycloak; see
[`docs/platform.md`](platform.md)).

> **`spec.gateway.trustedIps` is required behind the edge.** The quickstart sets
> `["0.0.0.0/0", "::/0"]` so the API gateway (Kong) honours the ingress's `X-Forwarded-*` headers
> and builds correct `https://<host>` OAuth redirects. Without it the login redirect falls back to
> the internal `http://…:8000` and the browser SSO round-trip breaks. Scope it to your ingress
> controller's pod CIDR in production if you'd rather not trust all sources.

### Credentials (read them back — the operator prints nothing)

```bash
# Keycloak admin console (Keycloak Operator-minted: <platform>-keycloak-initial-admin)
kubectl get secret ilm-keycloak-initial-admin -n ilm -o jsonpath='{.data.username}' | base64 -d ; echo
kubectl get secret ilm-keycloak-initial-admin -n ilm -o jsonpath='{.data.password}' | base64 -d ; echo
```

The ILM platform admin is the first admin you bootstrapped via `registerAdmin` — the certificate
admin (mTLS) by default, or the password admin (a Keycloak realm user) if you chose that method.
See [`docs/platform.md`](platform.md) for both.

### Routing external traffic to the edge

Reaching `https://<host>` needs an ingress controller **and** a way for traffic to that host to
land on it:

- **Cloud clusters** — the ingress controller's `LoadBalancer` Service gets an external IP
  automatically; point your DNS (or a wildcard record) at it. Done.
- **Kind / bare clusters (no cloud load balancer)** — the `LoadBalancer` Service stays `<pending>`
  and the Ingress `ADDRESS` stays blank (cosmetic, not an error). Give the host a path to the
  ingress one of these ways:

  - **Recommended — create the Kind cluster with host port-mappings, then install an ingress
    controller.** `https://<host>` then works end to end, so browser login completes:

    ```bash
    cat <<'EOF' | kind create cluster --name ilm --config -
    kind: Cluster
    apiVersion: kind.x-k8s.io/v1alpha4
    nodes:
      - role: control-plane
        extraPortMappings:
          - { containerPort: 80,  hostPort: 80,  protocol: TCP }
          - { containerPort: 443, hostPort: 443, protocol: TCP }
    EOF
    # Install ingress-nginx using the manifest the project documents for Kind:
    #   https://kubernetes.github.io/ingress-nginx/deploy/#quick-start
    # Then map the host to localhost (the port-mappings forward :80/:443 into the cluster):
    echo "127.0.0.1 ilm.example.com" | sudo tee -a /etc/hosts
    ```

  - **Or run [`cloud-provider-kind`](https://github.com/kubernetes-sigs/cloud-provider-kind)** to
    assign the `LoadBalancer` Service a reachable IP, then map the host to that IP in `/etc/hosts`.

  - **Quick check without an edge** — port-forward the gateway and hit the UI/API directly. Use
    this for poking the API, not for completing SSO (the OIDC login redirect still targets
    `https://<host>`):

    ```bash
    kubectl port-forward -n ilm svc/api-gateway 8000:8000
    # http://localhost:8000/administrator/  ·  http://localhost:8000/kc/admin/
    ```

## 6. Tear down

```bash
kubectl delete platform ilm -n ilm
```

`spec.deletionPolicy` defaults to **`Retain`**, so deleting the Platform **leaves the managed
CloudNativePG / RabbitMQ / Keycloak infrastructure and its data intact**. Set it to `Delete`
to also reclaim the managed infrastructure.

## Next steps

- The full getting-started guide (external infra, edge variants, the exact Secret keys, all
  conditions, customization) — [`docs/platform.md`](platform.md).
- Configuration reference & scenario cookbook (every Platform option, mapped to a matching
  sample) — [`docs/configuration.md`](configuration.md).
- Deploying a specific platform version — [`docs/versions.md`](versions.md).
- Upgrading the platform and the managed-infra upgrade guard — [`docs/upgrades.md`](upgrades.md).
- Other ready-to-edit samples — [`config/samples/`](../config/samples/).
