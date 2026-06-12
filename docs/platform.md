# ILM Platform — getting started

This guide walks you end-to-end through deploying and testing an **ILM Platform** with
the ILM operator: install the operator, create the prerequisite Secrets, apply a
`Platform`, observe it converge, and tear it down. It uses only fields that are shipped
today (`otilm.com/v1alpha1`).

The `Platform` CRD manages the ILM platform itself (Core, auth,
fe-administrator, the Kong API gateway, and the optional utils), wires them to
your database and message broker, and optionally fronts them with an edge (Ingress or
Gateway API) and a first-admin bootstrap.

> Looking for the connector side? See the [Connector quick start](../README.md#create-a-connector).
> Working examples for every variant below live in [`config/samples/`](../config/samples/);
> the full annotated field reference (shipped surface + the few remaining design targets)
> is in [`docs/design/examples/platform-cr-reference.yaml`](design/examples/platform-cr-reference.yaml).

---

## 1. Install the operator and CRDs

The operator and its CRDs (Connector + Platform + Proxy) install together. Pick one path.

### kubectl apply (release manifest)

The simplest path — a single self-contained manifest (Namespace `ilm-operator-system`, both
CRDs, RBAC, and the manager Deployment, with the operator image pinned to the release). Needs
nothing but `kubectl`, and **cert-manager is not required to install the operator** (only for
cert-managed edges, see step 2):

```bash
kubectl apply -f https://github.com/OmniTrustILM/operator/releases/download/<version>/ilm-operator.yaml
```

Replace `<version>` with a [release tag](https://github.com/OmniTrustILM/operator/releases).
For CRD-first / GitOps installs, apply `ilm-operator.crds.yaml` (CRDs only) first. See
[`deploy/README.md`](../deploy/README.md) for the full comparison of install paths.

### Helm (installs the operator **and** the CRDs)

```bash
helm install ilm-operator deploy/charts/ilm-operator \
  --namespace ilm-system --create-namespace
```

The chart installs the CRDs by default (`crd.install: true` in
[`values.yaml`](../deploy/charts/ilm-operator/values.yaml)).

### Make (from a checkout)

```bash
# Install just the CRDs into the cluster in ~/.kube/config:
make install

# Build, then deploy the operator (CRDs + RBAC + Deployment) with your image.
# `make deploy` installs into the ilm-operator-system namespace.
make docker-build IMG=<registry>/ilm-operator:<tag>
make deploy       IMG=<registry>/ilm-operator:<tag>
```

For local development you can run the operator **outside** the cluster against your
current kube-context (CRDs must already be installed via `make install`):

```bash
make install
make run
```

Confirm the CRDs are registered:

```bash
kubectl get crd platforms.otilm.com connectors.otilm.com
```

---

## 2. Prerequisites (explicit and honest)

The operator manages the platform workloads. The **stateful backends are yours to
provide** in external mode, or the operator can **provision PostgreSQL for you** via the
CloudNativePG operator (managed mode). A few edge features need cluster add-ons. The
operator never installs these add-ons itself.

| You want… | You need | If it's missing |
|---|---|---|
| Any platform (the baseline) | An external (or managed) PostgreSQL and an external AMQP 1.0 broker, reachable from the cluster | The Platform goes `Degraded` if a required credentials Secret is missing |
| **Managed PostgreSQL** (`database.mode: managed`) | The **CloudNativePG operator** (`postgresql.cnpg.io`) installed in the cluster | The database is **not provisioned**, `DatabaseReady=False` (reason `CloudNativePGNotInstalled`) — **not** a hard failure; it self-heals once CloudNativePG appears. (Set `database.mode: external` to bring your own instead.) |
| **Managed RabbitMQ** (`messaging.mode: managed`) | The **RabbitMQ Cluster Operator** **and** the **Messaging Topology Operator** (`rabbitmq.com`) installed in the cluster | The broker is **not provisioned**, `MessagingReady=False` (reason `RabbitMQNotInstalled` if the Cluster Operator is absent, `TopologyOperatorNotInstalled` if the Topology Operator is absent) — **not** a hard failure; it self-heals once the operators appear. (Set `messaging.mode: external` to bring your own instead.) |
| Edge with `tls.source: internal` / `letsEncrypt` / `issuerRef` | **cert-manager** (`cert-manager.io`) | Edge is **skipped**, `EdgeReady=False` (reason `CertManagerNotInstalled`) — **not** a hard failure; the rest of the platform still converges and the edge self-heals once cert-manager appears |
| Edge with `type: ingress` | An ingress controller for your `className` (e.g. **ingress-nginx** for `nginx`) | The Ingress object exists but no traffic is routed until a controller programs it |
| Edge with `type: gatewayAPI` | **Gateway API CRDs** (`gateway.networking.k8s.io`) | Edge is **skipped**, `EdgeReady=False` (reason `GatewayAPINotInstalled`); self-heals once the CRDs appear |
| Edge with `tls.source: secret` (bring-your-own) | A `kubernetes.io/tls` Secret you create | — (no cert-manager needed) |
| `registerAdmin.certificate` with `source: generated` | **cert-manager** (to issue the admin client cert) | Admin cert not issued, `AdminCertReady=False` (reason `CertManagerNotInstalled`); non-fatal |
| `registerAdmin.password` (enabled) | **managed Keycloak** (the operator creates the realm user via its admin API) | Required at admission: a CEL rule rejects `password.enabled` unless `keycloak.mode: managed`. Realm user pending → `AdminUserReady=False`; non-fatal |

cert-manager presence is **detected** (via the cluster's served API groups). Its absence
degrades only the dependent feature (edge / admin cert), never the whole platform.

---

## 3. Create the prerequisite Secrets

Create these in the **same namespace** as the Platform. The keys below are the **exact
keys the operator reads** — they are not arbitrary.

```bash
kubectl create namespace ilm
```

**Database credentials** — keys must be `username` and `password`:

```bash
kubectl create secret generic ilm-db -n ilm \
  --from-literal=username=ilm \
  --from-literal=password='<db-password>'
```

**Messaging (broker) credentials** — keys must be `username` and `password`:

```bash
kubectl create secret generic ilm-messaging -n ilm \
  --from-literal=username=ilm \
  --from-literal=password='<broker-password>'
```

> A `kubernetes.io/basic-auth` Secret works too — it stores the same `username`/`password`
> keys. A generic Secret with those two keys is the simplest form.

> **Bring-your-own keys.** If your Secret already exists with different key names (an
> External-Secrets / Vault sync, or a CNPG-shaped Secret using `POSTGRES_USER` /
> `POSTGRES_PASSWORD`), you don't have to rename anything — point the operator at the keys:
> ```yaml
> spec:
>   database:
>     credentials:
>       secretRef: ilm-db
>       usernameKey: POSTGRES_USER
>       passwordKey: POSTGRES_PASSWORD
> ```
> The same `usernameKey`/`passwordKey` overrides exist on `messaging.credentials`, and
> `caKey` / `apiKey` / `certKey` / `privateKeyKey` on the other typed references (see the
> table below). The defaults are unchanged, so existing CRs need no edits.

The following are **optional**, only needed for specific features:

**Trusted CA bundle** (`spec.common.trustedCertificates.secretRef`) — key must be `ca.crt`:

```bash
kubectl create secret generic ilm-trusted-ca -n ilm \
  --from-file=ca.crt=path/to/ca-bundle.pem
```

**Provisioning API key** (`spec.provisioning.apiKeySecretRef`) — key must be `provisioningApiKey`:

```bash
kubectl create secret generic ilm-provisioning -n ilm \
  --from-literal=provisioningApiKey='<api-key>'
```

**Admin client certificate** (`spec.registerAdmin.certificate.secretRef`, only for
`certificate.source: provided`) — a `kubernetes.io/tls` Secret with keys `tls.crt` and
`tls.key`:

```bash
kubectl create secret tls ilm-admin-cert -n ilm \
  --cert=path/to/admin.crt --key=path/to/admin.key
```

**Admin password** (`spec.registerAdmin.password.secretRef`, only for the password method) —
a generic Secret with key `password` (override with `passwordKey`):

```bash
kubectl create secret generic ilm-admin-password -n ilm \
  --from-literal=password='<admin-password>'
```

> For edge TLS via cert-manager (`internal`/`letsEncrypt`/`issuerRef`) and for
> `registerAdmin.certificate: { source: generated }`, the TLS Secret is created **by
> cert-manager** — you do not create it. The operator never copies secret values into rendered
> objects; it references them by `secretKeyRef`/volume only, and never logs them. The admin
> password is likewise read read-only and handed to Keycloak once — never minted by the
> operator, and never placed in any rendered object, status, condition, event, or log.

**How the first admin signs in.** `registerAdmin` bootstraps the admin by one or both
methods — pick whichever fits how your admin authenticates:

- **Client certificate (mTLS), `registerAdmin.certificate`.** The operator registers the admin
  in Core with a client certificate and the admin presents it at the edge, which forwards it to
  Core via the client-cert header (`spec.core.clientCertHeader`, default `ssl-client-cert`).
  With `certificate.source: generated`, the credential is the certificate cert-manager issues
  into the `admin-certificate-secret` Secret (`tls.crt`/`tls.key`) in the platform namespace;
  with `certificate.source: provided`, it is your `certificate.secretRef` Secret:

  ```bash
  kubectl get secret admin-certificate-secret -n ilm -o jsonpath='{.data.tls\.crt}' | base64 -d > admin.crt
  kubectl get secret admin-certificate-secret -n ilm -o jsonpath='{.data.tls\.key}'  | base64 -d > admin.key
  curl --cert admin.crt --key admin.key https://<host>/...   # or load the pair into your browser
  ```

- **Password (Keycloak realm user), `registerAdmin.password`.** The operator creates an
  idempotent Keycloak realm user carrying the **superadmin** attribute
  (`attributes.groups: ["superadmin"]`, which the operator's default realm surfaces as the
  `roles` claim Core grants superadmin on), with the password from
  `registerAdmin.password.secretRef`. The admin then signs in with **username + password via
  OIDC**. This method requires `keycloak.mode: managed` (the operator needs the Keycloak admin
  API to create the user) and is reported by the `AdminUserReady` condition. The operator
  **never mints or logs the password** — it reads the Secret read-only and hands the value to
  Keycloak once.

`certificate` defaults **on** (the historical single behaviour); set `certificate.enabled:
false` to run **password-only**. Enable both sub-blocks to let the admin sign in either way.

> The managed Keycloak's own admin-console credentials live in the Keycloak Operator-generated
> `<platform>-keycloak-initial-admin` Secret — for administering Keycloak itself, distinct from
> the `registerAdmin.password` user that signs into ILM.

### Secret-key reference (verified against the operator code)

The in-Secret keys below are the **defaults**; for the typed external infra references
they are **user-mappable** so a bring-your-own (External-Secrets / Vault / CNPG-shaped)
Secret needn't rename its keys. The mapping covers the INPUT key only — the target env-var
names and the composed .NET/JDBC connection strings stay fixed application contracts. For a
`managed` database/messaging the keys are the upstream operator's generated-Secret
convention and are NOT user-mappable.

| Spec field | Secret type | Keys the operator reads | Key override (default) |
|---|---|---|---|
| `spec.database.credentials.secretRef` (external) | generic / `basic-auth` | `username`, `password` | `usernameKey` (`username`), `passwordKey` (`password`) |
| `spec.messaging.credentials.secretRef` (external) | generic / `basic-auth` | `username`, `password` | `usernameKey` (`username`), `passwordKey` (`password`) |
| `spec.common.trustedCertificates.secretRef` | generic | `ca.crt` | `caKey` (`ca.crt`) |
| `spec.provisioning.apiKeySecretRef` | generic | `provisioningApiKey` | `apiKey` (`provisioningApiKey`) |
| `spec.registerAdmin.certificate.secretRef` (certificate.source=provided) | `kubernetes.io/tls` | `tls.crt`, `tls.key` | `certKey` (`tls.crt`), `privateKeyKey` (`tls.key`) |
| `spec.registerAdmin.password.secretRef` (password method) | generic | `password` | `passwordKey` (`password`) |

---

## 4. Apply a Platform

Start with the smoke test — external DB + broker, no edge
([`config/samples/platform_minimal_external.yaml`](../config/samples/platform_minimal_external.yaml)).
Edit `database.host`, `messaging.host`, etc. to match your environment first.

```bash
kubectl apply -f config/samples/platform_minimal_external.yaml
```

Other ready-to-edit variants in [`config/samples/`](../config/samples/):

| Sample | What it adds |
|---|---|
| `platform_quickstart.yaml` | Everything managed (DB + RabbitMQ + Keycloak), zero Secrets to pre-create — the apply-and-go demo |
| `platform_minimal_external.yaml` | External DB + broker, no edge — the smoke test |
| `platform_edge_letsencrypt.yaml` | Ingress edge, `tls.source: letsEncrypt` (cert-manager + ingress-nginx) |
| `platform_edge_issuerref.yaml` | Ingress edge signed by an existing cert-manager Issuer/ClusterIssuer |
| `platform_edge_byo_secret.yaml` | Ingress edge with a bring-your-own TLS Secret (no cert-manager) |
| `platform_gatewayapi.yaml` | Gateway API edge (HTTPRoute on an existing Gateway, or operator-owned) |
| `platform_registeradmin_provided.yaml` | First-admin bootstrap with a caller-supplied admin cert |
| `platform_registeradmin_generated.yaml` | First-admin bootstrap with a cert-manager-issued admin cert |
| `platform_vault_injection.yaml` | Per-component `podAnnotations` driving the Vault Agent Injector |
| `platform_full.yaml` | Kitchen-sink of every implemented field (the authoritative "what can I set") |

---

## 5. Observe it converge

```bash
kubectl get platform -n ilm
```

The list view shows `Phase`, `Version` (`status.observedVersion`), and `Ready` (the
`Available` condition); with `-o wide` it adds the `Edge` (`EdgeReady`) column:

```text
NAME   PHASE        VERSION   READY   AGE
ilm    Progressing  2.18.0    False   20s
# …shortly after the Deployments report ready…
ilm    Running      2.18.0    True    2m
```

The printer columns are: **Phase**, **Version** (`.status.observedVersion`), **Ready**
(`Available`), **Edge** (`EdgeReady`, wide-only), **Age**.

```bash
kubectl describe platform ilm -n ilm
```

### Phase

`spec` → reconcile → `.status.phase`, one of:

- **`Progressing`** — children are applied but at least one required Deployment has not
  yet reached its desired ready replicas.
- **`Running`** — readiness is **measured** as met (not merely asserted).
- **`Degraded`** — a fatal error (e.g. a referenced credentials Secret is missing).

### Conditions

The platform sets two **core** conditions that drive the phase, plus a set of **adjunct**
conditions that report a single feature without ever flipping the platform to `Degraded`.

| Condition | Meaning |
|---|---|
| **`Available`** | All **required** components are ready. The required set is **Core + auth** (readiness is gated on a functional auth provider). `True` ⇒ Phase `Running`. |
| **`Progressing`** | A required Deployment is still rolling out. Mirrors the not-yet-`Available` state. |
| **`EdgeReady`** | The edge (Ingress / Gateway API + cert-manager objects) reconciled. `False` with reason `CertManagerNotInstalled` or `GatewayAPINotInstalled` when a prerequisite is absent — a **non-fatal waiting state**, not a `Degraded`. Absent entirely when `edge` is nil/disabled. |
| **`AdminCertReady`** | The `registerAdmin.certificate` `source: generated` admin certificate reconciled (cert-manager). Non-fatal `False` while waiting; absent for `source: provided` or a disabled certificate method. The cert admin itself is then registered **in-pod** by Core's `postStart` hook (Core's local-admin API is localhost-only), so there is no separate registration condition. |
| **`AdminUserReady`** | The `registerAdmin.password` Keycloak realm user (with the superadmin attribute) was ensured via the Keycloak admin API. Non-fatal `False` while waiting for managed Keycloak or the password Secret; absent when the password method is disabled. |
| **`OIDCConfigured`** | Outcome of the Core↔Keycloak OIDC wiring (managed Keycloak only). Deferred (`False/WaitingForKeycloak` or `WaitingForCore`) until both are Ready; never drives the phase to `Degraded`. Absent for external/unmanaged Keycloak. |
| **`DatabaseReady`** | Managed PostgreSQL (CloudNativePG) provisioning. `False/CloudNativePGNotInstalled` (CRD absent) or `False/WaitingForDatabase` (Cluster/app Secret not ready); `True` once the Cluster is Ready. Adjunct — never blocks `Available`. Absent for an external database. |
| **`MessagingReady`** | Managed RabbitMQ provisioning. `False/RabbitMQNotInstalled` or `False/TopologyOperatorNotInstalled` (a CRD absent) or `False/WaitingForMessaging`; `True` once the cluster + core-user Secret exist. Adjunct. Absent for an external broker. |
| **`KeycloakReady`** | Managed Keycloak provisioning. `False/KeycloakOperatorNotInstalled`, `False/WaitingForKeycloak`, or `False/RealmImportConfigMapMissing`; `True` once the Keycloak CR is Ready. Adjunct. Absent for external/unmanaged Keycloak. |
| **`ServiceMonitorsReady`** | Per-component Prometheus `ServiceMonitor` rendering (when a component sets `metrics.serviceMonitor.enabled`). `False` with reason `PrometheusOperatorNotInstalled` when the `monitoring.coreos.com` CRD is absent — non-fatal. Gated (absent) when no component requests a ServiceMonitor. |
| **`<Infra>UpgradeBlocked`** | One per managed dependency — **`DatabaseUpgradeBlocked`** / **`MessagingUpgradeBlocked`** / **`KeycloakUpgradeBlocked`**. Set `True/MajorUpgradeNeedsAck` (plus a `Warning` Event) when you bump a **running** managed cluster's `version` across a MAJOR boundary without setting that block's `upgradeAcknowledged: true`; the operator then holds the cluster at its current version instead of passing the new one through. Set it to acknowledge the upgrade; the condition clears. Absent when no major bump is pending. |

The adjunct steps each re-check on their own timescale; while `Progressing` the reconcile
requeues to self-heal. Messages and events carry **no secret material or connection
coordinates** — only generic reasons.

Useful follow-ups:

```bash
kubectl get deploy -n ilm                 # core, auth, fe-administrator, api-gateway, (utils)
kubectl get events -n ilm --sort-by=.lastTimestamp | tail
```

---

## 6. Tear down

```bash
kubectl delete platform ilm -n ilm
```

The operator adds a **finalizer** (`platform.otilm.com/finalizer`) before doing any
work, so deletion runs an orderly teardown handler before the object is removed.

`spec.deletionPolicy` (default **`Retain`**) selects what happens to **managed
(upstream-operator) infrastructure**:

- **`Retain`** — leaves managed infrastructure and its data intact (the safe default).
- **`Delete`** — reclaims managed infrastructure.

The operator's own namespaced children (Deployments, Services, ServiceAccounts,
ConfigMaps, Secrets, Ingress) are reclaimed by **owner-reference garbage collection under
both policies**. The policy is **load-bearing for the managed (upstream-operator)
infrastructure** — the CloudNativePG `Cluster`, the `RabbitmqCluster` + its topology, and
the `Keycloak` CR: those carry **no owner reference** and are **excluded from the
operator's prune**, so `Retain` (the default) leaves them and their data intact (a
`Warning` Event names the retained resource) while `Delete` reclaims them. See the managed
sections below for the per-dependency detail. The deletion handler also records a
lifecycle Event naming only the policy.

---

## Customizing the platform

Beyond the connection and edge wiring above, the `Platform` exposes a rich customization
surface. Everything here is **optional** — unset, the operator's derived defaults apply.

> **Looking for the complete option list, or "which sample fits my need"?** See the
> [configuration reference & scenario cookbook](./configuration.md) — every field, plus a
> scenario-to-sample index. This section covers the common knobs in context; the reference
> goes deep on connection pooling, managed engine versions, production sizing, and more.

### Platform version (`spec.version`)

The operator carries a **bill-of-materials of tested version bundles** — each pins every
component image, the env-var wiring, and the managed-RabbitMQ topology for one platform
release. `spec.version` selects a bundle:

```yaml
spec:
  version: "2.18.0"   # omit / "" pins the operator's NEWEST at creation (no auto-upgrade; bump to upgrade)
```

- Empty (the default) = the operator's newest version, so an existing CR keeps today's
  behaviour.
- One operator build supports a **range** of versions — run a canary version in one
  namespace, or take an operator fix without moving the platform.
- It is **not** a fixed enum (the supported set grows): an **unknown** version is a
  non-fatal `Degraded` with an actionable *supported-versions* message, never a crash.
- `status.observedVersion` reports the version the operator actually reconciled against
  (it also drives the `Version` printer column).

The platform version advances by **upgrading the operator** (then optionally moving
`spec.version` within the new build's range). For GitOps, pin the operator image tag and
set `spec.version` explicitly.

### Per-component overrides

**Every** component — `core`, `auth`, `scheduler`, `authOpaPolicies`,
`feAdministrator`, `utils`, `gateway` — embeds the same override surface (the same
shape a standalone `Connector` accepts). Set only what you need; an override **layers**
onto the operator's defaults (env is appended last-wins; secret/configmap refs, volumes,
and init/sidecar containers are appended):

```yaml
spec:
  core:
    image: { tag: 2.18.0 }          # per-field image override (registry/repository/name/tag/digest/command/args)
    replicas: 2                     # ignored when autoscaling is set (the HPA owns scaling)
    workloadType: Deployment        # Deployment (default) | StatefulSet
    resources: { requests: { cpu: 500m, memory: 1Gi }, limits: { memory: 2Gi } }
    env: [ { name: EXTRA_FLAG, value: "true" } ]   # non-sensitive inline env only
    secretRefs: []                  # mount/inject your own Secrets (env or volume) — see key-mapping below
    configMapRefs: []
    volumes: []                     # emptyDir volumes mounted into the main container
    probes: { liveness: {}, readiness: {}, startup: {} }
    securityContext: {}             # always SCC-hardened (fill-don't-replace)
    podAnnotations: {}              # e.g. Vault Agent / Istio sidecar injection
    podLabels: {}
    nodeSelector: {}
    affinity: {}
    tolerations: []
    initContainers: []              # appended; SCC-hardened
    sidecars: []                    # appended; SCC-hardened
    serviceAccount: { name: "", annotations: {} }   # e.g. an IRSA / workload-identity binding
    service: { port: 8080, type: ClusterIP }        # ClusterIP | NodePort | LoadBalancer
    metrics:
      enabled: true
      serviceMonitor: { enabled: true, interval: 30s }  # gated on the Prometheus operator CRD
```

The image fields are per-field: `digest` pins the image immutably (and wins over `tag`),
and `command`/`args` override the container ENTRYPOINT/CMD.

> **Note.** `core` additionally carries the Core-only field `clientCertHeader`.
> Provisioning is a platform-level concern at `spec.provisioning` (not on `core`).
> `additionalPorts` is **not** a per-component field — it is a fleet-wide passthrough
> (`spec.common.additionalPorts`, below).

### Bring-your-own Secret keys (key-mapping)

A `secretRefs` / `configMapRefs` entry consumes a Secret/ConfigMap as **env** or a
**volume**, mapping individual keys you choose to env-var names or mount paths:

```yaml
spec:
  core:
    secretRefs:
      - name: my-extra-secret
        type: env
        keys:
          - { secretKey: MY_KEY, envVar: APP_TOKEN }   # inject env APP_TOKEN from key MY_KEY
      - name: my-tls
        type: volume
        mountPath: /etc/extra-tls
```

This is separate from the **typed infra** key-mapping on `database.credentials` /
`messaging.credentials` (and the `caKey` / `apiKey` / `certKey` / `privateKeyKey`
overrides) — see the [secret-key reference](#secret-key-reference-verified-against-the-operator-code)
above. No secret values ever go in the CR; everything is by reference.

### Cross-component config (`spec.common`)

**One placement rule: anything that applies to _every_ component lives under `spec.common`.**
That is the shared image defaults, the platform's public `hostName`, the outbound `proxy`,
the log level (`logging`), the trusted-CA bundle (`trustedCertificates`), the fleet-wide
pod-template passthrough (init/sidecar containers, volumes, ports, envFrom), and fleet-wide
**scheduling + pod metadata** (`nodeSelector`/`affinity`/`tolerations`/`podAnnotations`/`podLabels`).
Each component's own block (`core`, `auth`, …) **overrides/augments** what `common` sets:

```yaml
spec:
  common:
    hostName: ilm.example.com   # the platform's canonical public FQDN (see precedence below)
    image:                      # shared image defaults for all components (each component overrides per field)
      registry: hub.omnitrustregistry.com
      repository: ilm
      pullSecrets: [ regcred ]
    logging: { level: INFO }
    proxy: { enabled: false }
    trustedCertificates: { secretRef: ilm-trusted-ca }
    # ── fleet-wide pod-template passthrough — use it for cluster-wide injections (a Vault
    #    Agent sidecar, an Istio proxy, a custom-CA init container, an OpenTelemetry
    #    collector) applied to every component: ──
    initContainers: []          # run before EVERY component's main container; SCC-hardened
    sidecars: []                # run alongside EVERY component's main container; SCC-hardened
    volumes: []                 # pod-level volumes added to every component
    volumeMounts: []            # extra mounts on every main container
    additionalPorts: []         # extra named container ports on every main container
    additionalEnvFrom:          # whole-Secret / whole-ConfigMap envFrom — NAMES only, no values
      secrets: [ platform-shared-env ]
      configMaps: [ platform-shared-config ]
    # ── fleet-wide scheduling + pod metadata — applied to every (stateless) component; each
    #    component overrides/augments (nodeSelector merges, tolerations append, affinity
    #    replaces + beats the HA default). Managed infra is scheduled via each block's overrides. ──
    nodeSelector: { workload: platform }
    tolerations: [ { key: dedicated, operator: Equal, value: ilm, effect: NoSchedule } ]
    affinity: {}                # corev1 node/pod (anti-)affinity
    podAnnotations: { sidecar.istio.io/inject: "true" }   # e.g. fleet-wide service-mesh injection
    podLabels: { cost-center: pki }
```

`common` init/sidecar containers are **SCC-hardened** (OpenShift `restricted-v2`, fill-AND-
force) like every other container — a `common` container can never weaken pod security.

#### The public hostname and its precedence (`PlatformHost`)

`common.hostName` is the **single source of truth** for the platform's external FQDN. The
operator resolves the effective host as **`PlatformHost = edge.host` (when set) `|`
`common.hostName`** — i.e. an explicit `edge.host` overrides `common.hostName` for that
edge, otherwise the edge falls back to `common.hostName`. The resolved host drives:

- the **edge** Ingress/HTTPRoute host and the TLS certificate SAN;
- Keycloak **`KC_HOSTNAME`** and the `ilm` OIDC client's redirect / web-origin /
  post-logout URIs (managed Keycloak);
- the in-pod OIDC registration script's browser-facing issuer/auth/logout URLs;
- the gateway **CORS origin** default (`https://<hostName>`, wildcard when no host).

Set `common.hostName` **even when you run your own ingress** (`edge.enabled: false`) so
Keycloak/OIDC/CORS still know the public address. When the edge **is** enabled, `edge.host`
is required by the CRD (keep it equal to `common.hostName`, or set only `common.hostName`
with the edge disabled).

> **Why the fe-administrator URLs don't use `hostName`.** The fe-administrator runtime URLs
> are intentionally **host-relative** (`/api`, `/login`, `/logout`, served from the same
> origin behind the gateway). They therefore do **not** embed
> `hostName` — set them only to change the *paths* (`spec.feAdministrator.url`), not the host.

You can also set `spec.additionalEnv` (a list of non-sensitive `{name,value}` env applied
to every component). **JVM tuning is plain env now** — the special-case `spec.javaOpts` was
removed. Set `JAVA_OPTS` per JVM component (e.g. `spec.core.env: [{ name: JAVA_OPTS, value:
"-XX:MaxRAMPercentage=75.0" }]` on `core`/`auth`/`scheduler`), or fleet-wide
via a Secret/ConfigMap referenced from `spec.common.additionalEnvFrom`.

### High availability, PodDisruptionBudgets, autoscaling

**`spec.highAvailability.enabled: true`** applies HA **defaults** to the **stateless**
components: a multi-replica count, a PodDisruptionBudget (`minAvailable: 1`), and pod
anti-affinity spreading replicas across nodes. Every default is **overridable** per
component (`enabled` is the only field on the profile today):

```yaml
spec:
  highAvailability:
    enabled: true
```

Override or set availability per component:

```yaml
spec:
  core:
    # Autoscaling: the operator renders an HPA and OMITS .spec.replicas (so the HPA owns the
    # count and Server-Side Apply never clobbers it). Autoscaling wins over replicas / HA default.
    autoscaling:
      minReplicas: 2
      maxReplicas: 6
      targetCPUUtilization: 75       # 1..100; needs a cpu resource request. Also: targetMemoryUtilization
  authOpaPolicies:
    podDisruptionBudget:
      enabled: true
      minAvailable: 2                # OR maxUnavailable (mutually exclusive; minAvailable wins if both set)
```

Stateful **managed** infrastructure HA is **not** governed by this profile — size it via
each managed block's own count (`database.managed.instances`, `messaging.managed.replicas`,
`keycloak.managed.instances`).

### Workload type (Deployment vs StatefulSet)

Per component, `workloadType` selects the apps/v1 kind: `Deployment` (default, fits the
stateless components — interchangeable pods, parallel rollout) or `StatefulSet` (stable
per-pod identity under a headless Service, ordered one-at-a-time rollout). Switching the
kind on a running component is **not** a seamless in-place mutation — the operator applies
the new kind and prunes the old, so the component briefly restarts (safe: platform state
lives in the database/broker, not the pod). The StatefulSet path carries no
`volumeClaimTemplates` today (the components are stateless).

### NetworkPolicy (default-on, opt-out)

`spec.networkPolicy` is **enabled by default** — omit the block and the operator renders a
**safe default-deny** set (`networking.k8s.io/v1`): an ingress default-deny that allows
only **same-namespace** traffic to the platform's pods (denying cross-namespace/external
ingress), an **edge → api-gateway** allow from the ingress-controller namespace, and
**permissive egress** (so managed-infra / external connectivity is never broken).

```yaml
spec:
  networkPolicy:
    enabled: true                  # default; set false to render NONE (e.g. a CNI without NetworkPolicy support)
    ingressNamespace: ingress-nginx  # the ingress-controller / Gateway namespace (default shown)
```

Set `ingressNamespace` to your ingress controller's namespace (e.g. a Gateway API
implementation's namespace). Tighter, allow-listed egress is a deliberate future hardening
knob, not on by default.

### Provisioning the bundled service (`provisioning.mode: deploy`)

The optional remote-proxy provisioning has two modes. `mode: external` (default) points
Core at **your own** provisioner via `apiURL`. `mode: deploy` makes the operator **render
the bundled `provisioning-rabbitmq` service** as a native, operator-managed component
(broker-wired via the platform messaging connection) and points Core at it. It is
RabbitMQ-specific, so `mode: deploy` **requires** `messaging.brokerType: rabbitmq`:

```yaml
spec:
  provisioning:
    mode: deploy
    deploy:
      # bootstrapSecretRef (REQUIRED): a Secret with the API key + JWT signing key.
      bootstrapSecretRef: ilm-provisioning-bootstrap
      apiKeyKey: securityApiKey            # in-Secret key for the API key (default)
      tokenSigningKeyKey: tokenSigningKey  # in-Secret key for the JWT signing key (default)
      # provisioner/proxy broker creds default to the platform messaging credentials
      # (managed: the Topology-generated provisioner/proxy Secrets); override if needed.
```

The JWT signing key and API key are **always** a referenced Secret — never inlined in the
CR, status, conditions, or logs.

---

## Managed PostgreSQL (CloudNativePG)

Instead of bringing your own database, the operator can **provision PostgreSQL for you**
via the [CloudNativePG](https://cloudnative-pg.io/) operator. Set `database.mode: managed`
and describe the cluster:

```yaml
spec:
  database:
    mode: managed
    managed:
      instances: 3            # 1 primary + 2 hot standbys
      version: "18"           # major PG version (latest supported by CloudNativePG)
      storage:
        size: 100Gi
        # storageClass: fast-ssd   # optional; omit for the cluster default
      # resources: { requests: { cpu: "2", memory: 4Gi }, limits: { memory: 8Gi } }
    # Connection pooler (PgBouncer) — ON BY DEFAULT for a managed DB (the fleet would
    # otherwise exhaust max_connections). Omit the block to keep it; set managed: false to
    # opt out; set managed: true (+ instances/parameters) to customize it. An EMPTY block
    # disables it. See docs/configuration.md → Connection pooling.
    # pgBouncer: { managed: true, instances: 2 }
  messaging:                  # messaging shown external here; managed RabbitMQ is also supported (mode: managed)
    mode: external
    host: rabbitmq.example.com
    port: 5672
    virtualHost: ilm
    credentials:
      secretRef: ilm-messaging
```

A complete, validated example is
[`config/samples/platform_managed_postgres.yaml`](../config/samples/platform_managed_postgres.yaml).

**Prerequisite — install CloudNativePG.** The operator detects the `postgresql.cnpg.io`
CRDs; it does **not** install them. Until CloudNativePG is present the Platform reports
`DatabaseReady=False` (reason `CloudNativePGNotInstalled`) and waits — it never fails, and
converges with no operator restart once CloudNativePG is installed. The recommended path is
`make install-upstream-operators` (`make verify-upstream-operators` to check), which installs
the full set — cert-manager, CloudNativePG, the RabbitMQ operators, and the Keycloak Operator
— pinned to the validated versions and idempotent. To install CloudNativePG manually:

```bash
kubectl apply --server-side -f \
  https://github.com/cloudnative-pg/cloudnative-pg/releases/download/v1.29.1/cnpg-1.29.1.yaml
```

**What the operator creates.** A CloudNativePG `Cluster` named `<platform>-db` (e.g.
`ilm-db`) with an `ilm` application database and owner role. CloudNativePG generates the
`<platform>-db-app` Secret (`username`/`password`); the operator reads it back and wires
Core, scheduler, and auth to the `<platform>-db-rw` Service and that
Secret via `secretKeyRef` — **exactly** as it wires an external database. You create **no
database Secret** in managed mode (only the messaging Secret). No credentials or
connection coordinates are ever placed in the CR, status, conditions, events, or logs.

**`DatabaseReady` condition.** Adjunct, like `EdgeReady` — it never blocks the platform's
`Available` condition: `False/CloudNativePGNotInstalled` (CRD absent),
`False/WaitingForDatabase` (Cluster provisioning or its app Secret not yet generated),
`True` once the Cluster is Ready and the app Secret exists.

**Deletion (`deletionPolicy`).** With the default **`Retain`**, deleting the Platform
**leaves the CloudNativePG Cluster and its data intact** (a `Warning` Event names the
retained database; you reclaim it manually). With **`Delete`**, the operator deletes the
Cluster on Platform deletion (CloudNativePG then garbage-collects its PVCs). The managed
Cluster carries **no owner reference** and is **excluded from the operator's prune**, so a
transient reconcile hiccup can never delete your database.

**Overrides.** `database.managed.overrides` is a JSON-merge patch (RFC 7396) applied onto
the rendered CloudNativePG `Cluster` spec — an escape hatch for CNPG settings the typed
surface does not expose. Operator-owned fields are rejected
(`metadata.name`/`namespace`/`ownerReferences`, `spec.bootstrap.initdb.database`/`owner`)
so the readback contract cannot be broken.

---

## Managed RabbitMQ (RabbitMQ Cluster + Messaging Topology operators)

Instead of bringing your own broker, the operator can **provision RabbitMQ for you** via
the [RabbitMQ Cluster Operator](https://www.rabbitmq.com/kubernetes/operator/operator-overview)
and the [Messaging Topology Operator](https://www.rabbitmq.com/kubernetes/operator/using-topology-operator).
Set `messaging.mode: managed` and describe the cluster:

```yaml
spec:
  messaging:
    mode: managed
    # virtualHost: czertainly   # optional; the vhost the operator provisions (default "czertainly")
    managed:
      replicas: 3            # RabbitMQ nodes (a clustered broker)
      version: "4.3.1"       # RabbitMQ version (selects the RabbitmqCluster image); omit to use the bundle default
      storage:
        size: 20Gi
        # storageClass: fast-ssd   # optional; omit for the cluster default
      # resources: { requests: { cpu: "1", memory: 2Gi }, limits: { memory: 4Gi } }
  database:                  # the database can be external (or managed — they combine)
    mode: external
    host: postgres.example.com
    port: 5432
    name: ilm
    credentials:
      secretRef: ilm-db
```

A complete, validated example is
[`config/samples/platform_managed_rabbitmq.yaml`](../config/samples/platform_managed_rabbitmq.yaml).

**Prerequisite — install both RabbitMQ operators.** The operator detects the `rabbitmq.com`
CRDs; it does **not** install them. Until the **Cluster Operator** is present the Platform
reports `MessagingReady=False` (reason `RabbitMQNotInstalled`); until the **Messaging
Topology Operator** is present, reason `TopologyOperatorNotInstalled`. It never fails, and
converges with no operator restart once the operators are installed. The recommended path is
`make install-upstream-operators` (pinned, validated versions); to install the two RabbitMQ
operators manually:

```bash
kubectl apply -f \
  https://github.com/rabbitmq/cluster-operator/releases/download/v2.21.0/cluster-operator.yml
kubectl apply -f \
  https://github.com/rabbitmq/messaging-topology-operator/releases/download/v1.19.2/messaging-topology-operator-with-certmanager.yaml
```

**What the operator creates.** A `RabbitmqCluster` named `<platform>-messaging` (e.g.
`ilm-messaging`) plus the full messaging topology the platform requires. The topology is
**BOM-versioned** — for the default **2.18.0** bundle it is **1 vhost** (`czertainly` by
default), **5 users** (administrator, provisioner, proxy, core, monitor), their
**5 permission sets**, **2 exchanges** (`czertainly` direct, `czertainly-proxy` topic),
**10 queues** (the 7 `core`/`core.*` queues + the 3 `time-quality.*` queues), and
**9 bindings** (6 core + 3 time-quality). The legacy 2.17.0 bundle ships a single user. The
Messaging Topology Operator generates one `<user>-user-credentials` Secret per user; the operator wires Core
and scheduler to the `<platform>-messaging` Service and the **core-user** Secret via
`secretKeyRef` — **exactly** as it wires an external broker. You create **no broker Secret**
in managed mode. No credentials or connection coordinates are ever placed in the CR, status,
conditions, events, or logs.

**Management UI (`messaging.management.expose`).** Set `spec.messaging.management.expose:
true` to publish the broker's management UI through the API gateway on **`/mq`**. It is
honored **only** for managed RabbitMQ (`mode: managed`, `brokerType: rabbitmq`) — for an
external broker the operator does not own the management endpoint and renders no `/mq` route.

When enabled, the operator sets RabbitMQ's `management.path_prefix=/mq` (in the
RabbitmqCluster's `spec.rabbitmq.additionalConfig`) so RabbitMQ serves the UI under `/mq`
with `/mq`-prefixed assets — the same pattern as Keycloak's `/kc`. The gateway forwards `/mq/`
**unstripped** and additionally **redirects the bare `/mq` → `/mq/`** (a short route that
strips `/mq` to `/`, letting RabbitMQ's own `/` → `/mq/` redirect fire), so the UI loads
**with or without** the trailing slash. `path_prefix` affects only the management HTTP
listener (15672); AMQP (5672) carries no HTTP path, so broker clients are unaffected.
Toggling `expose` is a broker config change, so it triggers a **one-time RabbitMQ roll**.
Log in with a user that carries a management/admin tag — the managed `administrator` user
(Secret `<platform>-messaging-administrator-user-credentials`); the untagged `core`/`proxy`/
`monitor` users are rejected by the UI.

**`MessagingReady` condition.** Adjunct, like `DatabaseReady`/`EdgeReady` — it never blocks
the platform's `Available` condition: `False/RabbitMQNotInstalled` or
`False/TopologyOperatorNotInstalled` (a CRD absent), `False/WaitingForMessaging` (the cluster
provisioning or the core-user Secret not yet generated), `True` once the cluster is Ready and
the core-user Secret exists.

**Deletion (`deletionPolicy`).** With the default **`Retain`**, deleting the Platform
**leaves the `RabbitmqCluster`, its topology, and its data intact** (a `Warning` Event names
the retained broker). With **`Delete`**, the operator deletes the cluster and every topology
CR on Platform deletion (the Cluster Operator then garbage-collects its PVCs). The managed
objects carry **no owner reference** and are **excluded from the operator's prune**, so a
transient reconcile hiccup can never delete your broker.

**Overrides.** `messaging.managed.overrides` is a JSON-merge patch (RFC 7396) applied onto
the rendered `RabbitmqCluster` spec. Operator-owned fields are rejected
(`metadata.name`/`namespace`/`ownerReferences`, and `spec.rabbitmq.additionalConfig`/
`advancedConfig` — the default-user / imported-definitions wiring) so the topology and
readback contract cannot be broken.

---

## Managed Keycloak (Keycloak Operator)

Instead of configuring OIDC providers directly in the application database, the operator can
**provision Keycloak for you** via the [Keycloak Operator](https://www.keycloak.org/operator/installation).
Set `keycloak.mode: managed` and describe the instance:

```yaml
spec:
  keycloak:
    mode: managed
    realm: ilm                  # the platform realm name (default "ilm")
    managed:
      instances: 1              # Keycloak instances (>1 = clustered/HA)
      version: "26.6.3"         # Keycloak version (selects the Keycloak CR image); omit to use the bundle default
      # realmImport:            # optional, create-only import from a user ConfigMap
      #   configMapRef: ilm-realm
      #   key: ilm_realm.json
  database:                     # Keycloak SHARES this database (external OR managed)
    mode: external
    host: postgres.example.com
    port: 5432
    name: ilm
    credentials:
      secretRef: ilm-db
```

A complete, validated example is
[`config/samples/platform_managed_keycloak.yaml`](../config/samples/platform_managed_keycloak.yaml).

**Managed Keycloak shares the platform database.** The operator wires the Keycloak CR's
`spec.db` from the **same mode-agnostic readback** the rest of the platform uses
(`ResolveDatabaseConnection`): vendor `postgres`, the host/port/database of the resolved
connection (the external coordinates, or the CNPG `<cluster>-rw` Service for managed
PostgreSQL), the username/password **by Secret reference** (never inlined), and a dedicated
`keycloak` schema so Keycloak's tables do not collide with the platform's. You create **no
new Secret** for Keycloak — it consumes the platform DB-credentials Secret you already
provide. The pod runs **SCC-clean** (OpenShift `restricted-v2`): `runAsNonRoot`, all
capabilities dropped, `seccompProfile: RuntimeDefault`, no privilege escalation, no
hard-coded `runAsUser`.

**Realm import (create-only).** When `keycloak.managed.realmImport.configMapRef` is set, the
operator reads the realm representation JSON from that ConfigMap and creates a
`KeycloakRealmImport` **once**. It is **not** re-applied on every reconcile, so a realm you
later edit in Keycloak is never clobbered. A missing ConfigMap is non-fatal
(`KeycloakReady=False/RealmImportConfigMapMissing` + requeue, never a platform-wide
failure); a malformed realm JSON or a missing key is a clear configuration error.

**ILM login theme.** For a managed Keycloak the operator applies the **ilm** login theme out of
the box: it stages the theme image (`keycloak-theme`, resolved from the version bundle) into the
Keycloak pods via an SCC-clean `init-theme` init container that copies it under
`/opt/keycloak/themes`, and sets `loginTheme: ilm` in its bundled realm. The Keycloak Operator
merges the init container, the theme volume, and the mount onto its StatefulSet (through
`spec.unsupported.podTemplate`). Keycloak's built-in themes — the admin console included — are
unaffected: they ship in a classpath JAR, so the mount only *adds* the ilm theme. A platform
version that predates the theme renders none (a version-aware capability, like the rest of the
bundle). The theme image resolves under your `common.image` registry/repository, so an air-gapped
mirror redirects it along with every other ILM image.

> **Upgrading an existing realm.** Because the realm import is **create-only**, a realm imported
> *before* the theme was wired keeps its old `loginTheme` (Keycloak does not re-import it). Set it
> once via the admin API — e.g. from inside the Keycloak pod, so no credentials leave the pod:
>
> ```bash
> kubectl exec -n <ns> <platform>-keycloak-0 -c keycloak -- sh -c \
>   '/opt/keycloak/bin/kcadm.sh config credentials --server http://localhost:8080/kc \
>       --realm master --user "$KC_BOOTSTRAP_ADMIN_USERNAME" --password "$KC_BOOTSTRAP_ADMIN_PASSWORD" \
>    && /opt/keycloak/bin/kcadm.sh update realms/<realm> -s loginTheme=ilm'
> ```

**Prerequisite — install the Keycloak Operator.** The operator detects the
`k8s.keycloak.org` CRDs; it does **not** install them. Until the Keycloak Operator is present
the Platform reports `KeycloakReady=False` (reason `KeycloakOperatorNotInstalled`). It never
fails, and converges with no operator restart once the Keycloak Operator is installed.

**`KeycloakReady` condition.** Adjunct, like `DatabaseReady`/`MessagingReady`/`EdgeReady` —
it never blocks the platform's `Available` condition: `False/KeycloakOperatorNotInstalled` (a
CRD absent), `False/WaitingForKeycloak` (the Keycloak CR provisioning),
`False/RealmImportConfigMapMissing` (the import ConfigMap not yet present), `True` once the
Keycloak CR reports Ready.

**Deletion (`deletionPolicy`).** With the default **`Retain`**, deleting the Platform
**leaves the Keycloak CR, its realm import, and the realm's data intact** (a `Warning` Event
names the retained Keycloak). With **`Delete`**, the operator deletes the Keycloak CR and the
realm import on Platform deletion. The managed objects carry **no owner reference** and are
**excluded from the operator's prune**, so a transient reconcile hiccup can never delete your
Keycloak.

**Core OIDC wiring — automatic, no client secret to provide.** Once the managed Keycloak CR
and Core are **both Ready**, the operator wires Core's internal OIDC provider to Keycloak as a
native reconcile action (no in-pod registration script). It reads the
Keycloak admin credentials from the Operator-generated `<keycloak>-initial-admin` Secret,
**fetches the platform `ilm` client's secret from Keycloak's admin API** (so you provide **no**
client secret — Keycloak generates it), and idempotently `PUT`s Core's
`oauth2Providers/internal` config. The URLs follow a **split**: the browser-facing
issuer/authorization/logout URLs are built from the **external** platform host
(`PlatformHost` = `edge.host` | `common.hostName`, HTTPS) so split-horizon DNS does not
break redirects, while the back-channel token/jwks URLs use the **in-cluster** Keycloak
Service (HTTP). It is reported by the **`OIDCConfigured`** condition and
is an **adjunct** — like admin registration it never drives the phase to `Degraded`; a
not-yet-ready Keycloak/Core or a transient failure is a non-fatal `OIDCConfigured=False` +
requeue. The admin credentials, the fetched client secret, the Keycloak token, and all response
bodies never reach logs, status, conditions, or events. This wiring is validated **end-to-end**
on Kind (the full managed-platform e2e): with Core + a managed Keycloak both Ready and the `ilm`
client present in the imported realm, the operator fetches the client secret and `PUT`s Core's
provider, and `OIDCConfigured` reaches **`True`** automatically.

**Overrides.** `keycloak.managed.overrides` is a JSON-merge patch (RFC 7396) applied onto the
rendered `Keycloak` CR spec. Operator-owned fields are rejected
(`metadata.name`/`namespace`/`ownerReferences`, and `spec.db`/`spec.hostname` — the
database-sharing and edge-host wiring) so the shared-database contract cannot be broken.

---

## Behavior notes (lifecycle features)

- **Measured readiness.** `Available`/`Running` reflect the **measured** ready replicas
  of the required Deployments (Core + auth), not an optimistic assertion. A
  still-rolling-out Deployment keeps the platform `Progressing`.
- **De-rendered children are pruned.** Toggling a feature off removes the objects it had
  rendered. For example, setting `edge.enabled: false` (or removing the `edge` block)
  deletes the Ingress/Gateway/cert-manager objects the operator previously created;
  disabling `utils` removes its Deployment/Service. The operator reconciles the
  full owned set each pass and garbage-collects what is no longer desired.
- **Read-only root filesystem (all containers).** Every workload runs with a read-only
  root filesystem — the **nginx**-based fe-administrator, the **Kong** API gateway,
  **OPA**, the **init** containers, AND the JVM (Core, scheduler, utils)
  and .NET (auth) main containers. Each has its sole writable path backed by an
  in-memory `/tmp` ephemeral volume (auth additionally gets `TMPDIR=/tmp` so the
  .NET runtime's keyring/temp writes land there). This is validated end-to-end on Kind
  against the real ILM images (the full managed-platform e2e), where the app pods reach
  Ready with a read-only root and do not crash-loop.
- **Optional cert env vars are wired defensively.** Secret-backed env like
  `ADMIN_CERT` (when `registerAdmin` is enabled), `PROVISIONING_API_KEY`, and
  `TRUSTED_CERTIFICATES` are injected via `secretKeyRef` with the reference marked
  optional, so Core **starts** (rather than wedging on `CreateContainerConfigError`)
  while a referenced Secret is briefly absent, then picks the value up on a later roll.
- **Referenced Secrets are watched.** The operator watches the credentials Secrets a
  Platform references and re-reconciles on change, and a change to the composed
  configuration rolls Core via a `checksum/config` pod-template annotation.

---

## Running on OpenShift

The operator runs on OpenShift unchanged — it neither detects nor special-cases it.

- **SCC `restricted-v2` out of the box.** Every pod the operator renders (and the operator's
  own) is non-root, sets **no `runAsUser`** (OpenShift assigns the namespace's allocated UID),
  drops **all** capabilities, sets `seccompProfile: RuntimeDefault`, and forbids privilege
  escalation — so it runs under the **default `restricted-v2` SCC** with no custom SCC and no
  extra RBAC. A CR override cannot weaken this (the security context is fill-**and-force**).
- **Install** via **OperatorHub** (the bundled OLM package) or the Helm chart.
- **Edge.** Use `edge.type: ingress` — the OpenShift router reconciles the `Ingress` and
  publishes the Route for you — or `edge.type: gatewayAPI`. A native OpenShift **`Route`** edge
  type (`edge.type: route`) is **not yet supported**. cert-manager TLS modes work as on any
  cluster (OpenShift has a Red Hat cert-manager operator).
- **Managed infrastructure.** The CloudNativePG, RabbitMQ, Keycloak, and cert-manager operators
  all install from OperatorHub; the operator detects their CRDs and waits, exactly as elsewhere.

The `restricted-v2` guarantees are enforced in code and asserted by a builder unit test in CI;
the full e2e currently runs on Kind, so OpenShift admission itself is not yet exercised end-to-end.

---

## Where to look next

- [`docs/configuration.md`](configuration.md) — the complete configuration reference &
  scenario cookbook (every option, and which sample fits which need).
- [`config/samples/`](../config/samples/) ([index](../config/samples/README.md)) — working,
  validated `Platform` variants.
- [`docs/versions.md`](versions.md) — deploying different platform versions via `spec.version`.
- [`docs/upgrades.md`](upgrades.md) — the upgrade procedure and the managed-infra upgrade guard.
- [`docs/design/platform-operator.md`](design/platform-operator.md) — the full design and
  security model.
- [`docs/design/examples/platform-cr-reference.yaml`](design/examples/platform-cr-reference.yaml)
  — annotated field reference, split into implemented vs. design-target.
