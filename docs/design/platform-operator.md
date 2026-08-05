# ILM Platform Operator — Design Specification

**Scope:** the architecture, security model, and reconciliation design of the `Platform`
CRD (`otilm.com/v1alpha1`), which deploys and wires the ILM platform itself.

> This document describes the **current** design of the platform side of the ILM operator
> as a standalone product. For the end-user getting-started walkthrough see
> [`docs/platform.md`](../platform.md); for the connector side see
> [`connector-operator.md`](connector-operator.md).

## Overview

The `ilm-operator` manages ILM **connectors** via the `Connector` CRD and the **ILM
platform itself** via the `Platform` CRD. One operator binary, three independent controllers,
no coupling between them.

A `Platform` is a single namespaced custom resource that describes a whole ILM deployment:
the stateless application tier (Core, auth, auth-opa-policies, scheduler,
fe-administrator, the optional utils, the Kong API gateway, and the optional
provisioning service), its database and message broker (either referenced as **external**
infrastructure or **managed** for you via upstream operators), an optional **edge** (Ingress
or Gateway API with cert-manager TLS), an optional **managed Keycloak** OIDC provider, and an
optional first-admin bootstrap. The operator renders and continuously reconciles every owned
object, gates on the cluster capabilities each feature requires, and reports a precise status.

## Design principles

1. **A clean, de-nested API.** Platform singletons are top-level fields, not buried under
   wrappers. Anything that applies to *every* component lives under a single consolidated
   `spec.common` block; everything component-specific lives under that component's block.
2. **Three CRDs, one operator.** `Platform`, `Connector`, and `Proxy` are independent controllers
   sharing the same builder primitives. Nothing connector-related lives in the `Platform`
   CR, and nothing platform-related lives in the `Connector` CR.
3. **Mandatory services have no `enabled` flag.** Only `utils` (and the optional
   provisioning, edge, managed-infra, and Keycloak features) are opt-in.
4. **No sensitive values in the CR.** Credentials, keys, and certificates are `Secret`
   references only — never inline (see the Security model).
5. **Stateful infrastructure is delegated, never re-templated.** PostgreSQL, RabbitMQ, and
   Keycloak are referenced when `external` or provisioned via their upstream operators when
   `managed`. The operator never re-templates them in Go and never uses the Helm SDK.
6. **Deletion safety.** A finalizer governs teardown; `spec.deletionPolicy` (default
   `Retain`) protects managed infrastructure and its data.
7. **NetworkPolicies on by default.** A safe default-deny isolation model ships out of the
   box, opt-out for CNIs without NetworkPolicy support.
8. **Capabilities are detected and gated, never assumed.** A feature whose upstream CRD is
   not served is skipped with a non-fatal, actionable condition and a self-healing requeue —
   not a cryptic apply failure or a whole-platform `Degraded`.

## Architecture

One operator binary, three controllers (`Platform`, `Connector`, `Proxy`). Two rendering paths, both
continuously reconciled (watch / drift / status):

- **Native builders** for the stateless components — the shared component render model in
  `internal/builder/common`, consumed by both the `Platform` and `Connector` builders.
- **Upstream-operator composition** for stateful infrastructure: the operator creates the
  specialist's CR and wires consumers from its generated Service/Secret per an explicit
  readback contract.

No Helm SDK — native reconciliation and CR composition end to end.

### Platform spec — the clean API

```yaml
apiVersion: otilm.com/v1alpha1
kind: Platform
metadata: { name: ilm, namespace: ilm }
spec:
  version: ""                               # selects the platform version bundle; empty = the operator's default

  # ── cross-component config (applies to EVERY component) ──
  common:
    hostName: ilm.example.com               # the canonical public FQDN (drives edge host, Keycloak/OIDC, CORS)
    image: { registry: hub.omnitrustregistry.com, repository: ilm, pullSecrets: [], tag: "", pullPolicy: IfNotPresent }
    proxy: { enabled: false, http: "", https: "", noProxy: "" }
    logging: { level: INFO }
    trustedCertificates: { secretRef: ilm-trusted-ca }   # caKey optional (default ca.crt)
    # fleet-wide pod-template passthrough:
    initContainers: []                      # run before EVERY component's main container (SCC-hardened)
    sidecars: []                            # run alongside EVERY component's main container (SCC-hardened)
    volumes: []
    volumeMounts: []
    additionalPorts: []
    additionalEnvFrom: { secrets: [], configMaps: [] }   # whole-Secret/ConfigMap envFrom — NAMES only

  additionalEnv: []                         # non-sensitive {name,value} env applied to every component

  # ── platform-wide settings ──
  deletionPolicy: Retain                    # Retain | Delete — cascade of *managed* infrastructure
  networkPolicy: { enabled: true, ingressNamespace: ingress-nginx }   # default-on; ingressNamespace default shown
  highAvailability: { enabled: false }      # true → multi-replica + PDB + anti-affinity on the STATELESS components

  # ── database (REQUIRED): external connection OR managed (CloudNativePG) ──
  database:
    mode: external                          # external | managed
    host: postgres.example.com; port: 5432; name: ilmdb
    credentials: { secretRef: ilm-db }      # usernameKey/passwordKey optional (default username/password)
    # pgBouncer is a managed-DB CNPG Pooler (ON by default); opt out with { managed: false }. Ignored for external.
    # managed:                              # required when mode=managed
    #   instances: 3; version: "18"
    #   upgradeAcknowledged: false          # opt in to a MAJOR PG version bump of a RUNNING cluster
    #   storage: { size: 100Gi, storageClass: "" }
    #   resources: {}
    #   overrides: { /* JSON-merge (RFC 7396) into the CloudNativePG Cluster spec */ }

  # ── messaging (REQUIRED): AMQP client connection; broker external OR managed ──
  messaging:
    mode: external                          # external | managed
    brokerType: rabbitmq                    # rabbitmq | servicebus (AMQP dialect/auth)
    host: rabbitmq.example.com; port: 5672; virtualHost: ilm
    credentials: { secretRef: ilm-messaging }   # usernameKey/passwordKey optional (default username/password)
    management: { expose: false }           # expose the broker management UI on the gateway /mq (managed RabbitMQ only)
    # managed:                              # required when mode=managed
    #   replicas: 3; version: "4.3.1"
    #   upgradeAcknowledged: false          # opt in to a MAJOR RabbitMQ version bump of a RUNNING cluster
    #   storage: { size: 20Gi }; resources: {}
    #   overrides: { /* JSON-merge (RFC 7396) into the RabbitmqCluster spec */ }

  # ── keycloak (OPTIONAL): external OIDC (configured in app DB) OR managed (Keycloak Operator) ──
  # keycloak:
  #   mode: managed
  #   realm: ilm                            # the platform realm name (default "ilm")
  #   managed:                              # required when mode=managed
  #     instances: 2; version: "26.6.3"
  #     upgradeAcknowledged: false          # opt in to a MAJOR Keycloak version bump of a RUNNING instance
  #     realmImport: { configMapRef: ilm-realm, key: realm.json }   # optional, create-only
  #     overrides: { /* JSON-merge (RFC 7396) into the Keycloak spec */ }

  # ── provisioning (OPTIONAL; off by default) — remote-proxy support, a platform concern ──
  # provisioning:
  #   mode: external                        # external (point Core at your own provisioner) | deploy
  #   apiURL: https://provisioner.example.com
  #   apiKeySecretRef: ilm-provisioning     # key: provisioningApiKey
  #   deploy:                               # mode=deploy renders the bundled provisioning-rabbitmq (needs brokerType=rabbitmq)
  #     bootstrapSecretRef: ilm-provisioning-bootstrap

  # ── edge (OPTIONAL): external entry + TLS ──
  edge:
    enabled: true
    type: ingress                           # ingress | gatewayAPI
    className: nginx                        # Ingress edge only
    host: ilm.example.com                   # external FQDN; REQUIRED when enabled (overrides common.hostName for this edge)
    tls: { source: letsEncrypt, letsEncrypt: { email: "", environment: production } }
    annotations: {}
    # gatewayAPI edge: set gatewayAPI.gatewayClassName (operator owns its Gateway) OR
    #   gatewayAPI.parentRef (attach the HTTPRoute to an existing Gateway).

  # ── first-admin bootstrap (OPTIONAL; off by default) ── two independent methods:
  # certificate (mTLS, defaults ON): source=provided → you supply the admin client-cert Secret
  #   (tls.crt/tls.key) via certificate.secretRef; source=generated → cert-manager issues it
  #   (the operator mints NOTHING). Set certificate.enabled=false to run password-only.
  # password (a Keycloak realm user with the superadmin attribute; OFF by default): the operator
  #   creates the user via the Keycloak admin API with the password from password.secretRef
  #   (read-only, never minted/logged). REQUIRES keycloak.mode=managed; tracked by AdminUserReady.
  registerAdmin:
    enabled: true
    username: ""
    name: ""
    lastName: ""                            # admin surname; recommended for the password method (Keycloak otherwise prompts to complete the profile on first login)
    email: ""
    certificate: { enabled: true, source: provided, secretRef: ilm-admin-cert }
    password: { enabled: false, secretRef: ilm-admin-password }

  # ── per-component overrides (OPTIONAL; operator defaults otherwise) ──
  # EVERY component embeds the same override surface (the same shape a Connector accepts):
  # image (registry/repository/name/tag/digest/command/args), replicas, workloadType
  # (Deployment|StatefulSet), autoscaling (HPA), podDisruptionBudget, resources, env,
  # secretRefs/configMapRefs (with key mapping), volumes, probes, securityContext,
  # podAnnotations/podLabels, nodeSelector/affinity/tolerations, initContainers/sidecars,
  # serviceAccount, service, metrics (+ ServiceMonitor).
  # core ALSO carries the Core-only field clientCertHeader (default "ssl-client-cert").
  core: {}                                  # e.g. { service: { type: NodePort }, replicas: 4, resources: {}, env: [] }
  auth: {}
  authOpaPolicies: {}
  scheduler: {}
  feAdministrator: { url: { api: "", base: "", login: "", logout: "" } }
  utils: { enabled: false }          # the only optional service
  gateway: {}                               # cors/logging/trustedIps + the per-component override surface (always Kong)
```

**Minimal CRs:** managed → `spec: { database: { mode: managed }, messaging: { mode: managed } }`;
external → the two blocks with `host`/`name`/`credentials.secretRef`. HA → add
`highAvailability: { enabled: true }`.

### Platform status

```yaml
status:
  phase: Running                  # Progressing | Running | Degraded  (the enum the code emits)
  observedVersion: "2.18.0"       # the resolved platform version (spec.version, or the operator's default when unset)
  observedGeneration: 3
  conditions:
    # Core (drive the phase):
    - { type: Available,   status: "True" }    # required set: Core + auth (a functional auth provider)
    - { type: Progressing, status: "False" }
    # Adjunct (never flip the platform to Degraded; each reports one feature):
    - { type: EdgeReady,            status: "True" }   # absent when edge nil/disabled
    - { type: DatabaseReady,        status: "True" }   # managed DB only
    - { type: MessagingReady,       status: "True" }   # managed broker only
    - { type: KeycloakReady,        status: "True" }   # managed Keycloak only
    - { type: OIDCConfigured,       status: "True" }   # Core↔Keycloak wiring (managed Keycloak)
    - { type: AdminCertReady,       status: "True" }   # registerAdmin.certificate source=generated
    #   (the cert admin is then registered IN-POD by Core's postStart — no separate condition)
    - { type: AdminUserReady,       status: "True" }   # registerAdmin.password (managed Keycloak realm user)
    - { type: ServiceMonitorsReady, status: "True" }   # per-component ServiceMonitor(s)
    # - { type: <Infra>UpgradeBlocked, status: "True", reason: MajorUpgradeNeedsAck }
    #   one per managed dep (Database/Messaging/Keycloak) when a MAJOR bump needs upgradeAcknowledged
# The printer columns are: Phase, Version (observedVersion), Ready (Available), Edge (EdgeReady, wide), Age.
# Conditions carry only names, phases, checksums — never secret values, never connection coordinates (host/port/URI).
# Each condition also carries its own observedGeneration (set via meta.SetStatusCondition) so stale conditions are detectable.
```

## Security model

### 1. No sensitive values in the CR

A CR lives in etcd, `kubectl get -o yaml`, GitOps, and audit logs — so secrets must never
enter its spec.

- **References only.** Credentials/keys/certs/tokens are `Secret` references
  (`credentials.secretRef`, `secretRef`, `apiKeySecretRef`). The typed external infra
  references accept optional in-Secret **key overrides** (`usernameKey`/`passwordKey`,
  `caKey`, `apiKey`, `certKey`/`privateKeyKey`) defaulting to the operator's wiring keys, so
  a bring-your-own (External-Secrets/Vault/CNPG-shaped) Secret needn't be renamed — only the
  INPUT key is mapped; output env names and composed connection strings stay fixed contracts.
  The CRD schema **rejects** an inline plaintext value where a reference is required; a
  validating webhook (see §4) will reinforce this at admission as future work.
- **No copying.** Env config wires `valueFrom.secretKeyRef`/`envFrom.secretRef`; the kubelet
  injects at pod start. Secret values never appear in the rendered objects, the CR, or
  operator memory beyond a transient validation read. Volume secrets are mounted, not
  templated.
- **No leakage.** Status, conditions, events, and logs carry only names/phases/checksums —
  never secret values, and never connection coordinates. Condition messages say *what*
  failed, never *where*/*with what*. Validation errors name the missing key, never its content.

### 2. RBAC and least privilege

The operator reads referenced `Secret`s **read-only** and never creates/mutates them. **Secret
access is namespace-scoped** — the `Platform` CR and its managed infrastructure live in the
**same namespace**, so a namespaced `Role` covers Secret reads (including operator-generated
infra Secrets). Broader cluster-scoped grants are limited to CRD discovery,
`Platform`/`Connector` watches, and leader-election leases.

### 3. NetworkPolicies (default-on)

`networkPolicy.enabled` (default **true**, opt-out) renders a **safe default-deny** isolation
model — secure but unable to break intra-platform traffic (`networking.k8s.io/v1`):

1. an **ingress default-deny** selecting all of the platform's pods (by
   `app.kubernetes.io/part-of=ilm` + instance) that allows ingress only from pods in the
   **same namespace** and denies all cross-namespace/external ingress — the high-value,
   low-risk isolation;
2. an **edge → api-gateway allow** permitting ingress to the api-gateway's consumer port from
   the ingress-controller namespace (`networkPolicy.ingressNamespace`, default
   `ingress-nginx`), so the edge entrypoint keeps working;
3. **permissive egress** (DNS + intra-namespace + DB/broker/Keycloak) so managed-infra /
   external connectivity is never broken.

The primary security win is the ingress default-deny; **tighter, allow-listed egress** (and
per-flow ingress between components) is a deliberate **future hardening knob**, not turned on
by default (a too-strict egress is the easiest way to break a deploy). Disable explicitly on
CNIs without NetworkPolicy support.

### 4. Admission & conversion webhooks

**Current state.** Create-time validation is enforced today by the CRD's CEL (`XValidation`)
rules — rejecting an inline plaintext value where a `Secret` reference is required, an external
database missing host/credentials, an enabled edge missing its host, and
`provisioning.mode=deploy` on a non-rabbitmq broker. The **per-namespace `Platform` singleton**
is enforced at runtime by the controller (only the oldest `Platform` reconciles; a second one
goes `Degraded` with `AnotherPlatformExists`). There is **no admission webhook deployed yet**.

**Planned.** A validating webhook will reinforce the invariants at admission time — rejecting
inline plaintext where a `Secret` reference is required, forbidden managed-infra `overrides`,
and the singleton (a second `Platform` rejected at create rather than admitted-then-degraded,
replacing the racy list-based runtime guard). The webhook **is** the operator, so its
availability is part of the model: `failurePolicy: Fail` (a webhook outage would block
new/changed CRs but never affect running platforms); the operator runs with a PDB
`minAvailable: 1`; bootstrap order is documented (operator Ready before the first CR). **TLS:**
the operator would manage its own webhook serving certificate — a self-signed CA + cert it
generates and **auto-rotates**, injecting the CA bundle into the
`ValidatingWebhookConfiguration` — so there is no external dependency; cert-manager (plain
Kubernetes) or OLM-injected certs (OpenShift) are supported alternatives. Rotation is sequenced
to avoid the documented `caBundle` race: **inject the new CA bundle (keeping the old CA in the
trust set during overlap) before retiring the old serving cert**, so `failurePolicy: Fail`
never produces transient `x509` admission failures mid-rotation.

### 5. Deletion safety

A finalizer `platform.otilm.com/finalizer` governs teardown (added as the first reconcile
step, before any dependent is created). `spec.deletionPolicy` (default **`Retain`**): deleting
the `Platform` CR does **not** delete managed stateful infra — the operator removes its own
resources and releases the finalizer while leaving the **upstream CRs intact** (e.g. the
CloudNativePG `Cluster`, which then keeps its own PVCs; the PV `reclaimPolicy` is an
independent second layer). `Delete` is an explicit opt-in to cascade. A `Warning` event +
condition explain retained infra. **Cluster-scoped artifacts the operator creates** (any `ClusterRole`, and the planned
`ValidatingWebhookConfiguration` once it lands) **cannot** carry an owner reference to the
namespaced `Platform` CR — cross-namespace/cluster-scoped ownerRefs are invalid and break
garbage collection — so they are cleaned up by **finalizer + labels**, not GC.

## Configuration model

**The simple model.** Two homes, one rule: **non-secret config inline in the CR; each secret
in its own `Secret`, referenced by name.** Credentials use standard Secret types
(`basic-auth`, `tls`) referenced once. **Each value has one home, so there is no precedence to
reason about.** Runtime operational settings (logging, audit, OIDC providers) live in the
application database, not the CR.

**One placement rule for cross-component config: `spec.common`.** Anything that applies to
every component — the shared image defaults, the public `hostName`, the outbound `proxy`, the
log level, the trusted-CA bundle, and the fleet-wide pod-template passthrough (init/sidecar
containers, volumes, ports, envFrom) — lives under `spec.common`. Each component's own block
(`core`, `auth`, …) **overrides/augments** what `common` sets.

**No hardcoded names — the generic mapping is canonical.** The operator hardcodes **no**
environment-variable names and **no** expected Secret/ConfigMap keys in its reconcile *logic*.
The canonical wiring mechanism is the `Connector` CRD's: supply Secrets/ConfigMaps with **your
own** key names plus a **mapping** (`secretRefs`/`configMapRefs` → `keys: [{ secretKey, envVar
| path }]`, or whole-object `envFrom`); the operator passes them through verbatim, so renaming
an env var is a mapping change in the CR — never an operator-code change. The typed
`database`/`messaging` fields are ergonomic **sugar**: their defaults — the connection-string
template, the default Secret key names, and the target env-var names — live in the operator's
**versioned wiring profile** (carried in the version bundle alongside image tags) as **data,
not Go literals**, and are **fully overridable** by the generic mapping (which takes
precedence). Standard Secret types (`basic-auth`/`tls`) stay the **default** for ecosystem
interop (cert-manager/External-Secrets/CNPG/RabbitMQ-operator/Vault) — defaults, not mandates.

**Precedence (advanced — only when combining sources for one value):** operator default <
`spec.common` shared < per-component override; multiple refs at one level → later in the list
wins (Kubernetes `envFrom` semantics).

**Replicas and autoscaling.** Per-component `replicas`; when a component sets `autoscaling`,
the operator **omits `.spec.replicas`** so the HPA owns scaling (no fight on reconcile).
`highAvailability` sets the stateless replica floor + PDB + anti-affinity in one place.

**Versioning.** Components do not share a tag. The operator carries a **bill-of-materials
(BOM) that is a map of tested version bundles** — each bundle pins every component image (and
the managed-infra default versions) plus that version's env wiring and managed-RabbitMQ
topology for one platform release. **`spec.version` selects a bundle** (empty = the operator's
NEWEST, so an existing CR is unchanged); `status.observedVersion` reports the resolved
version. So one operator build carries a **supported RANGE** of platform versions — you can
run a canary version in one namespace or take an operator fix without moving the platform.
An **unknown** `spec.version` is a deterministic user mistake that degrades with an actionable
supported-versions message (a **runtime check, not a CEL enum**, since the supported set grows
over time), never a crash. Overrides layer on top of the selected bundle: shared
`common.image.tag` then per-component `image.tag`, both winning over the bundle's coordinates.
Connectors version independently via their CRs. **A `Platform` is a per-namespace singleton** —
exactly **one `Platform` per namespace** (the operator renders children under clean, unscoped
names, so a second one would collide). To run more than one platform, use **separate
namespaces**; the operator manages a **fleet of namespace-scoped singletons**, each pinned to
its own `spec.version` within the range this operator build supports. (See
[`docs/versions.md`](../versions.md) for the version model and [`docs/upgrades.md`](../upgrades.md)
for the upgrade procedure.) **GitOps:** **pin the operator image tag** (do not track a floating
`latest`) and set `spec.version` explicitly.

## High availability

`highAvailability.enabled: true` applies one cross-cutting profile to the **stateless**
components (Core, auth, scheduler, fe-administrator, auth-opa-policies,
utils, gateway): a sane multi-replica count, a PodDisruptionBudget (`minAvailable: 1`),
and pod anti-affinity spreading replicas across nodes. Default is non-HA (single replicas,
minimal footprint). HA assumes a multi-node cluster across failure domains; the operator's own
HA (replicas + leader election) is set at install.

**Every HA default is overridable per component** — an explicit `replicas`,
`podDisruptionBudget`, or `affinity` on the component wins. Availability is therefore tuned
**per component** with the per-component knobs: `replicas`, `autoscaling` (an HPA — when set,
the operator **omits `.spec.replicas`** so the HPA owns scaling and Server-Side Apply never
fights it; `autoscaling` wins over `replicas` and over the HA-default count), and
`podDisruptionBudget` (`enabled` + `minAvailable` *or* `maxUnavailable`, mutually exclusive).
The per-component `workloadType: StatefulSet` is available where a stable per-pod identity or
ordered rollout is wanted.

**Stateful managed infrastructure HA is not governed by this profile** — it is the upstream
operators' concern, sized via each managed block's own count (`database.managed.instances`,
`messaging.managed.replicas`, `keycloak.managed.instances`) plus that dependency's `overrides`
for deep topology (synchronous replication, zone-aware placement).

## Components and rendering

### Stateless tier (native)

Core + the platform services + the **gateway**, via the shared builders
(Deployment/StatefulSet, Service, ServiceAccount, PDB, HPA, NetworkPolicy). Cross-cutting: the
trusted-certificates Secret; the optional first-admin bootstrap (`registerAdmin`) — the admin
client cert is *provided* or *generated* (a cert-manager `Certificate`, gated on cert-manager
like the edge; the operator mints nothing), Core's `ADMIN_CERT` is wired by `secretKeyRef`, and
the admin is registered via an idempotent **reconcile action** (not an in-pod hook/Job).

**Read-only root filesystem (all containers).** Every workload runs with a read-only root
filesystem — the nginx-based fe-administrator, the Kong API gateway, OPA, the init containers,
and the JVM (Core, scheduler, utils) and .NET (auth) main containers.
Each has its sole writable path backed by an in-memory `/tmp` ephemeral volume.

**Provisioning (optional — remote-proxy support).** The provisioning service creates
**per-proxy** broker topology for *remote* proxies/connectors (outside the cluster,
communicating over the broker); Core calls it via a REST API. It is **off by default** and
offered in two modes (`spec.provisioning`): `deploy` renders the bundled `provisioning-rabbitmq`
**natively** (a stateless ILM component, RabbitMQ-specific, so it requires
`messaging.brokerType: rabbitmq`); `external` points Core at your own provisioner (e.g. for
Azure Service Bus). It connects to the broker as the `provisioner` user (a `Secret` in external
messaging, or a Topology-Operator `User` in managed messaging); the API key Core uses is a
referenced `Secret`.

### Gateway (Kong)

Rendered natively in **DB-less mode**: the operator generates Kong's declarative config
(`kong.yml`) from the spec into a ConfigMap mounted via `KONG_DECLARATIVE_CONFIG`, plus the
Deployment/Service; config changes trigger a rollout via a config checksum. The Kong Ingress
Controller is not used. There is **no `gateway.type` field** — the gateway is always Kong (the
gateway block carries cors/logging/messaging/trustedIps + the per-component override surface).
`messaging.management.expose: true` publishes the managed-RabbitMQ management UI through the
gateway on `/mq`.

### Edge (pluggable)

External entry — TLS and passing a client cert to Core as a header:

- `ingress` (default) — `networking.k8s.io/v1`. Ingress has no standard mTLS; client-cert is
  controller-specific, so the Ingress edge is validated against `ingress-nginx`
  (`edge.className == nginx`) for the client-cert path.
- `gatewayAPI` — `Gateway` + `HTTPRoute`; portable.

The operator detects available edge APIs and renders accordingly. `edge.host` is the external
FQDN; it overrides `common.hostName` for that edge (see the PlatformHost precedence in
[`docs/platform.md`](../platform.md)). Client-cert forwarding is configured via
`spec.core.clientCertHeader` (default `ssl-client-cert`).

#### Edge upstream-operator / CRD prerequisites (detected, not assumed)

The edge depends on upstream operators / CRD bundles the platform operator does **not** install
(they are cluster-singleton infrastructure). Rather than assume they are served — which makes a
Server-Side Apply fail with a cryptic `no matches for kind` — the operator **detects** them (via
the manager's `RESTMapper`) and **gates the edge gracefully**. Each mode requires only what it
renders:

| Edge configuration | Requires |
| --- | --- |
| `type: ingress` + `tls.source: internal`, `letsEncrypt`, or `issuerRef` | **cert-manager** (`cert-manager.io` `Certificate`/`Issuer`) |
| `type: ingress` + `tls.source: secret` (BYO) | **nothing** beyond core networking — the Ingress references the caller's TLS Secret |
| `type: gatewayAPI` | **Gateway API CRDs** (`gateway.networking.k8s.io` `Gateway`/`HTTPRoute`); **plus cert-manager** when the operator renders its **own** Gateway (no `parentRef`) with a cert-managed TLS source. With a `parentRef`, only the `HTTPRoute` is rendered and TLS is the existing Gateway's concern |

**TLS sources (`spec.edge.tls.source`).** `internal` provisions a self-signed CA chain via
cert-manager; `letsEncrypt` provisions an ACME issuer; **`issuerRef`** points the cert-manager
ingress/gateway **shim** at **any existing** `Issuer`/`ClusterIssuer` you provide (Vault, a
corporate/intermediate CA, Venafi, an external-issuer group, …) — the operator creates **no**
`Issuer`/`Certificate` of its own, it only stamps the shim annotation and cert-manager mints the
leaf into the edge TLS Secret; `secret` is bring-your-own (no cert-manager). Because `issuerRef`
relies on the same shim as `internal`/`letsEncrypt`, it has the **same cert-manager
prerequisite** (gated identically) even though it renders no operator-created cert-manager object.

When a required prerequisite is **absent**, the operator does **not** add the edge objects to the
apply set, applies the **rest of the platform normally**, and sets a non-fatal **`EdgeReady:
False`** condition with reason `CertManagerNotInstalled` or `GatewayAPINotInstalled` and an
actionable message. It then **requeues** so the edge self-heals once the operator is installed —
the dynamic `RESTMapper` re-discovers the newly served CRD without an operator restart. A missing
edge prerequisite never flips the whole `Platform` to `Degraded`. The detector
(`pkg/capabilities`) is generic (it takes any `GroupKind`) and is the **reusable
basis for the managed-infra availability checks** (CNPG / RabbitMQ / Keycloak).

### Infrastructure (`external` or `managed`) — `curated + overrides`

Stateful systems are referenced or delegated to upstream operators — never re-templated in Go,
never Helm-wrapped.

| Dependency | `external` | `managed` |
|---|---|---|
| PostgreSQL | connection to an existing DB | CloudNativePG `Cluster` |
| Messaging (AMQP) | existing broker (RabbitMQ / Azure Service Bus) | RabbitMQ Cluster Operator `RabbitmqCluster` + Messaging Topology Operator (`Vhost`/`User`/`Permission`/`Queue`/`Binding`) |
| Keycloak / OIDC | external provider (app DB) | Keycloak Operator `Keycloak` + `KeycloakRealmImport` |
| PgBouncer | external DB's own pooling | CloudNativePG `Pooler` |

**Customization (`curated + overrides`).** Each managed dependency exposes **curated** fields
(instances/replicas, storage, resources, version) for the common case, plus an **`overrides`**
block merged into the upstream CR using **JSON Merge Patch (RFC 7396)** semantics — the upstream
CRs are CRDs, so strategic-merge tags do not apply, which means **arrays are *replaced*, not
appended** (part of the user-facing contract). The operator **protects its invariants** by
**rejecting** (the reconcile guard today, the planned validating webhook later — not a silent no-op) any `overrides` that touch
operator-owned fields: resource names, owner references, the credential/Service wiring it reads
back, and the database name / roles the readback contract assumes. For RabbitMQ specifically,
`default_user` / `default_pass` and imported `definitions` are forbidden overrides — they change
the `RabbitmqCluster.status.binding` Secret the Messaging Topology Operator authenticates with and
would silently lock topology management out.

**Readback contract — explicit, versioned, validated.** CloudNativePG: creds from `<cluster>-app`,
host from `<cluster>-rw`. RabbitMQ Cluster + Topology: broker from the `<cluster>` Service;
`core`/`provisioner`/`proxy` users from per-user Secrets. Keycloak Operator: admin from
`<keycloak>-initial-admin`; the platform's own OIDC **client secret** is read via the Keycloak
**admin API** (using those admin credentials) and wired to Core — Keycloak does not export client
secrets as Kubernetes Secrets, so this is an admin-API readback, not a Secret readback.

**Detection & gating.** `managed` requires the upstream operators present. The operator detects
the relevant CRDs (the same generic detector the edge uses); if a `managed` dependency's CRD is
absent, the reconciler sets the dependency's adjunct condition (`DatabaseReady` /
`MessagingReady` / `KeycloakReady` = `False` with a `…NotInstalled` reason) and **requeues** that
dependency rather than crashing. It self-heals once the upstream operator is installed, with no
operator restart.

**Managed-infra major-version upgrade guard.** A MAJOR version bump of an *already-running*
managed dependency is a one-way, data-affecting operation with upstream prerequisites (RabbitMQ
3.x→4.x needs feature flags + quorum-queue migration first; a PostgreSQL or Keycloak major runs a
one-way migration). So the operator does **not** pass a major increase of the *running* cluster's
version straight through to the upstream operator: it **blocks** it until that managed block's
**`upgradeAcknowledged: true`** is set, surfacing an actionable **`<Infra>UpgradeBlocked=True` /
`MajorUpgradeNeedsAck`** condition (`DatabaseUpgradeBlocked` / `MessagingUpgradeBlocked` /
`KeycloakUpgradeBlocked`) plus a `Warning` Event, and holding the cluster at its current version
(it re-pins the running engine image on the rendered CR so the apply does not bump it). **Patch/
minor changes and the FIRST creation** (no running version yet) apply freely regardless of the
flag. Reset `upgradeAcknowledged` to `false` after the upgrade completes. See
[`docs/upgrades.md`](../upgrades.md) for the full procedure.

**Credential rotation.** The `Watches(Secret)` predicate covers operator-owned infra Secrets (by
owner/label); rotation triggers a rolling restart of consumers. The mechanism differs by backend:
CNPG's `-app` Secret is watched and rotation rolls consumers, but **RabbitMQ managed-user rotation
goes through the Topology-Operator `User` CR (annotate/patch to reconcile), not by editing the
generated Secret** — the Cluster Operator does not propagate Secret edits to the running broker.

**Managed Keycloak realm — secret-safe.** OIDC `redirectUris` / web origins are populated from the
**external** host (`PlatformHost` = `edge.host` | `common.hostName`) so split-horizon DNS does not
break browser redirects (the operator-native Core↔Keycloak OIDC wiring builds the browser-facing
issuer/authorization/logout URLs from the external host over HTTPS, while the back-channel
token/jwks URLs use the in-cluster Keycloak Service). Secret handling, by type: (1) the platform's
own client secret is **generated by Keycloak and read back via the admin API** (you provide
nothing); (2) other realm secrets (SMTP/IdP/LDAP) use **placeholders in the realm structure** (safe
in a ConfigMap), with values substituted at import time from `Secret`-sourced env — the operator
**never** writes resolved secret values into the `KeycloakRealmImport` object. `KeycloakRealmImport`
is **create-only** — the operator creates it once and does **not** re-import (which would reset
client secrets), so a realm you later edit in Keycloak is never clobbered.

**Messaging is an AMQP client concern** for the consumers (Apache Qpid JMS); `brokerType` selects
only the dialect/auth.

### Connectors

Entirely the `Connector` CRD's domain; nothing connector-related is in the `Platform` CR.

## Reconciliation, finalizers, and RBAC

`SetupWithManager`: `For(Platform)`, `Owns(Deployment/StatefulSet/Service/ServiceAccount/PDB/HPA/
Job/ConfigMap/NetworkPolicy/Ingress)` (+ `Gateway`/`HTTPRoute` and upstream CRs when present),
`Watches(Secret/ConfigMap)` (incl. operator-owned infra Secrets). Flow: ensure finalizer →
resolve the version bundle → resolve config (merge `common`→component; validate referenced Secrets
without logging values) → gate + reconcile managed infrastructure (create upstream CRs in
`managed`; read back per contract; wire `external`) → reconcile stateless components → edge →
NetworkPolicies → first-admin / OIDC reconcile actions → status. On deletion, the finalizer
enforces `deletionPolicy`.

**Apply strategy:** the Platform reconciler render-applies **all** of its owned children
(Deployments/StatefulSets/Services/ServiceAccounts/ConfigMaps/PDBs/HPAs/NetworkPolicies/edge
objects and the upstream infra CRs) via **Server-Side Apply** with a stable field manager
(`ilm-operator`, `ForceOwnership`), so it manages only the fields it sends and API-defaulted fields
never churn — and so resources **co-owned** with other actors are handled correctly: an
HPA-managed workload omits `.spec.replicas` (the HPA owns scaling) and the upstream infra CRs are
written alongside their own controllers without clobbering. The single exception is the
reconcile-time **composed Secret** (the auth connection string), which uses
`controllerutil.CreateOrUpdate`. **RBAC** is namespace-scoped for Secret reads; no blanket Secret
write.

**Pruning de-rendered children.** The reconciler tracks the desired set of owned keys each pass
and garbage-collects what is no longer desired. Toggling a feature off removes the objects it had
rendered (e.g. `edge.enabled: false` deletes the Ingress/Gateway/cert-manager objects; disabling
`utils` removes its Deployment/Service).

**Transient-vs-fatal error contract.** The reconciler splits failures into two classes so a flaky
dependency never wedges the platform: a **transient** error (a not-yet-served upstream CRD, a
managed cluster still provisioning, a referenced Secret briefly absent, a retryable upstream-API
call) is **non-fatal** — it surfaces on the relevant *adjunct* condition (`EdgeReady` /
`DatabaseReady` / `MessagingReady` / `KeycloakReady` / `OIDCConfigured` / `AdminUserReady` =
`False` with a *waiting* reason) and **requeues** to self-heal, while `Available` and the phase are
unaffected; a **fatal/deterministic** error (a missing **required** credentials Secret, an unknown
`spec.version`, an invalid configuration) drives the phase to **`Degraded`** with an actionable
message. Messages and events carry only generic reasons — never secret values or connection
coordinates.

## API versioning and CRD graduation

`v1alpha1` is the current served and stored version. A future `v1beta1`/`v1` is introduced
`served: true` (not storage) first; the old version stays `served` until all stored objects are
converted, with a conversion webhook sharing the §Security-4 availability model. Conversion
functions are kept trivial and **lossless round-trip tests** are required when a new version lands.

## OpenShift compatibility

The operator runs on OpenShift unchanged; it does **not** detect or special-case OpenShift —
it simply renders pods that satisfy the default `restricted-v2` SCC. All rendered pods (and the
operator's own) are `runAsNonRoot`, set **no hard-coded `runAsUser`** (OpenShift assigns the
namespace's allocated UID), drop all capabilities, set `seccompProfile: RuntimeDefault`, and
forbid privilege escalation — fill-**and-force**, so a CR override cannot weaken it (asserted by
the builder SCC test in CI). RBAC is standard `ClusterRole`/`ClusterRoleBinding`, so no SCC `use`
grant is required. The edge is a `networking.k8s.io` Ingress (the OpenShift router reconciles it
and publishes the Route) or a Gateway API HTTPRoute; a native OpenShift `Route` edge type
(`edge.type: route`) is reserved, not yet implemented. OLM is built in — managed-infra
prerequisites install cleanly from OperatorHub; the delegation targets are OpenShift-friendly
(CNPG certified; Red Hat builds Keycloak + its operator; cert-manager has a Red Hat operator).
The `restricted-v2` guarantees are enforced in code and unit-tested; the full e2e runs on Kind,
so OpenShift admission itself is not yet exercised end-to-end.

## Portability (non-Kubernetes)

The operator is Kubernetes-only, but the platform must still run under Docker Compose and bare JVM.
Any Kubernetes-specific capability the platform relies on is **optional and detected** (a missing
Secret/file is a fallback, not fatal). The operator introduces no platform dependency on Kubernetes.

## Testing and quality

- **Unit:** builders (component model, common→component merge, edge variants, HPA `replicas: nil`,
  Kong declarative config); upstream-CR mappers + readback contracts; the version-bundle resolution
  and the major-upgrade guard.
- **Integration (envtest):** reconciliation, drift, Secret/ConfigMap watch (incl. operator-owned
  infra Secrets), `managed` CR creation + readback with upstream CRDs, credential-rotation →
  restart, **deletion protection** (`Retain` leaves infra), capability gating, conditions, and the
  CRD's CEL validation rules.
- **Security tests:** CEL validation rejects inline secrets and the other create-time invariants
  (and, once the webhook lands, that it rejects inline secrets and a webhook outage blocks only
  new writes); no secret material or connection coordinates in any rendered
  object/status/event/log.
- **E2E (Kind):** a representative `Platform` in `external` and **fully-managed** modes (the
  full-managed e2e brings up CNPG + RabbitMQ + Keycloak against the real ILM images, asserts the
  app pods reach Ready with a read-only root, and validates the automatic OIDC wiring reaches
  `OIDCConfigured=True`); SCC compliance; NetworkPolicy enforcement; the existing connector E2E.
- **Development loop vs. release gate.** The fast inner loop is `make test` (envtest + unit) and
  `make lint`; the Kind e2e (`make test-e2e-managed`) is a pre-release / nightly gate, not the
  development loop. When the e2e catches a deterministic controller/builder bug, the fix adds a fast
  envtest/builder assertion so the inner loop catches that class next time.
- **Quality bar:** ≥80% coverage, <3% duplication, zero lint warnings, OLM bundle validates, Trivy
  clean (no HIGH/CRITICAL).

## Operator capability levels

I Basic Install — done · II Seamless Upgrades — version-bundle selection + the managed-infra
major-upgrade guard · III Full Lifecycle — PDBs, drift, events, conditions, finalizers, pruning ·
IV Deep Insights — metrics + per-component ServiceMonitors · V Auto Pilot — future.

## Prior-art validation

Cross-checked against production operators; the key decisions are directly validated:

- **CloudNativePG** deletes PVCs on `Cluster` deletion *by design* — confirming
  `deletionPolicy: Retain` + finalizer + PV-reclaim are *our* responsibility, not the upstream's.
- **Keycloak Operator v2** deliberately dropped the rich realm/client/user CRDs over DB-vs-CR
  drift, and `KeycloakRealmImport` is creation-only — confirming our DB-stance and one-shot import.
- **RabbitMQ** cluster+topology split and generated-Secret semantics confirm our delegation +
  readback model.
- Official guidance backs **level-triggered idempotent reconcile, one controller per Kind,
  finalizer-first, `observedGeneration`/standard conditions, and omitting `.spec.replicas` under an
  HPA**.

Lessons folded in: **Server-Side Apply for co-owned resources**; RabbitMQ **default-user /
imported-definitions forbidden overrides** + **`User`-CR-driven rotation**; **webhook cert-rotation
ordering**; **cluster-scoped artifact cleanup via finalizer/labels**; per-condition
`observedGeneration`; the `Retain` path leaving the upstream `Cluster` CR intact; and lossless
conversion round-trip tests.
