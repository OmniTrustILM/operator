# ILM Platform — configuration reference & scenario cookbook

This is the **complete configuration reference** for the `otilm.com/v1alpha1` `Platform`:
every option, what it does, and the scenario (and sample) it belongs to. It is the deep
companion to the getting-started guide.

- **Just getting started?** Read [`platform.md`](./platform.md) first — install, prerequisites,
  the first `apply`, and how to watch it converge.
- **Want a CR to copy?** Every scenario below points at a validated sample in
  [`config/samples/`](../config/samples/) — see the [samples index](../config/samples/README.md).
- **Want the annotated every-field YAML?**
  [`docs/design/examples/platform-cr-reference.yaml`](./design/examples/platform-cr-reference.yaml).

Two invariants hold across **everything** here: **no secret values in the CR** (sensitive
data is always a `…SecretRef` to a `Secret`), and **managed data is safe by default**
(`deletionPolicy: Retain`). Connection coordinates and credentials never appear in the CR,
status, conditions, events, or logs.

---

## How configuration is organized

A `Platform` has four kinds of configuration:

1. **Platform version** — `spec.version` selects a tested bundle of component images + wiring.
   See [`platform.md` → Platform version](./platform.md#platform-version-specversion).
2. **Infrastructure** — `database`, `messaging`, and (optional) `keycloak`, each **`external`**
   (bring your own) or **`managed`** (operator-provisioned via an upstream operator). These are
   independent — [mix them per dependency](#infrastructure-external-vs-managed).
3. **Fleet-wide config** — `spec.common` applies to **every** component (shared image, public
   `hostName`, proxy, logging, trusted CA, pod-template passthrough).
4. **Per-component overrides** — each component (`core`, `auth`, `scheduler`, `authOpaPolicies`,
   `feAdministrator`, `utils`, `gateway`) embeds the same override surface.

> **The placement rule:** anything that applies to *every* component lives under
> `spec.common`; a per-component block overrides/augments it. See
> [`platform.md` → Cross-component config](./platform.md#cross-component-config-speccommon).

---

## Scenarios at a glance

| I want to… | Sample | Key fields |
|---|---|---|
| Smoke-test with my own DB + broker | [`platform_minimal_external.yaml`](../config/samples/platform_minimal_external.yaml) | `database/messaging.mode: external` |
| Apply-and-go, everything managed | [`platform_quickstart.yaml`](../config/samples/platform_quickstart.yaml) | all `mode: managed` |
| A production-tuned starting point | [`platform_production.yaml`](../config/samples/platform_production.yaml) | HA + managed + resources |
| See every field that exists | [`platform_full.yaml`](../config/samples/platform_full.yaml) | (reference, not a recommendation) |
| Let the operator run PostgreSQL | [`platform_managed_postgres.yaml`](../config/samples/platform_managed_postgres.yaml) | `database.mode: managed` |
| Let the operator run RabbitMQ | [`platform_managed_rabbitmq.yaml`](../config/samples/platform_managed_rabbitmq.yaml) | `messaging.mode: managed` |
| Let the operator run Keycloak | [`platform_managed_keycloak.yaml`](../config/samples/platform_managed_keycloak.yaml) | `keycloak.mode: managed` |
| Mix external + managed | [`platform_mixed_infra.yaml`](../config/samples/platform_mixed_infra.yaml) | per-dependency `mode` |
| Turn the DB pooler **off** | [`platform_managed_postgres_no_pooler.yaml`](../config/samples/platform_managed_postgres_no_pooler.yaml) | `database.pgBouncer.managed: false` |
| Pin upstream engine versions | [`platform_managed_pinned_versions.yaml`](../config/samples/platform_managed_pinned_versions.yaml) | `*.managed.version` |
| Run the stateless tier in HA | [`platform_high_availability.yaml`](../config/samples/platform_high_availability.yaml) | `highAvailability.enabled` |
| Expose an HTTPS edge | [`platform_edge_*.yaml`](../config/samples/) / [`platform_gatewayapi.yaml`](../config/samples/platform_gatewayapi.yaml) | `edge.*`, `edge.tls.source` |
| Bootstrap the first admin | [`platform_registeradmin_*.yaml`](../config/samples/) | `registerAdmin.*` |
| Inject secrets via Vault | [`platform_vault_injection.yaml`](../config/samples/platform_vault_injection.yaml) | `common.initContainers/sidecars` |

---

## Infrastructure: external vs managed

`database` and `messaging` are required; `keycloak` is optional. Each picks its mode
independently:

- **`external`** — you run the service and give the operator the connection coordinates plus a
  credentials `Secret` (`credentials.secretRef`). No upstream operator needed. The in-Secret
  key names are mappable (`usernameKey`/`passwordKey`) so a Vault/External-Secrets/CNPG-shaped
  Secret works without renaming keys.
- **`managed`** — the operator renders the upstream CR (CloudNativePG `Cluster`, `RabbitmqCluster`
  + topology, or `Keycloak`), and **reads back** the generated coordinates and credentials by
  reference. You create **no** credentials Secret for that dependency.

The depth for each managed dependency (prerequisites, what gets created, the `…Ready` condition,
deletion, overrides) lives in `platform.md`:
[Managed PostgreSQL](./platform.md#managed-postgresql-cloudnativepg) ·
[Managed RabbitMQ](./platform.md#managed-rabbitmq-rabbitmq-cluster--messaging-topology-operators) ·
[Managed Keycloak](./platform.md#managed-keycloak-keycloak-operator).

Mixing is common — keep the database on your existing managed-Postgres service while letting the
operator run the broker and Keycloak:

```yaml
spec:
  database:  { mode: external, host: postgres.corp.internal, name: ilm, credentials: { secretRef: ilm-db } }
  messaging: { mode: managed, brokerType: rabbitmq, managed: { replicas: 3, storage: { size: 20Gi } } }
  keycloak:  { mode: managed, realm: ilm, managed: { instances: 1 } }
```

→ [`platform_mixed_infra.yaml`](../config/samples/platform_mixed_infra.yaml). Prerequisites scale
with what is `managed`: here, only the RabbitMQ + Keycloak operators (no CloudNativePG).

---

## Connection pooling (PgBouncer)

**Applies to a managed database only.** An external database brings its own pooling.

**Why it exists.** The ILM fleet (core + auth + keycloak + scheduler) opens enough connection
pools to exhaust PostgreSQL's default `max_connections` (100). Without a pooler, `auth` returns
500s and Core crash-loops on its boot-time resource sync. So for a managed database the operator
fronts PostgreSQL with a **CloudNativePG Pooler (PgBouncer, transaction mode)** and wires every
component through the pooler Service (`<platform>-db-pooler`) instead of the cluster's read-write
Service (`<platform>-db-rw`). This mirrors the Helm chart, which ships PgBouncer for the same
reason.

**The pooler is ON by default for a managed database.** The switch is `database.pgBouncer.managed`:

| `pgBouncer` block | Pooler | Connection target |
|---|---|---|
| *omitted* | **ON** (recommended) | `<platform>-db-pooler` |
| `{ managed: true }` | **ON** (explicit; same as omitting) | `<platform>-db-pooler` |
| `{ managed: false }` | **OFF** | `<platform>-db-rw` (direct) |
| `{}` (empty) | **OFF** ⚠️ | `<platform>-db-rw` (direct) |

> ⚠️ **The empty-block trap.** An empty `pgBouncer: {}` leaves `managed` at its `false` default
> and **disables** the pooler. To customize the pooler (instances/parameters) while keeping it on,
> set `managed: true` *explicitly*. The field that controls the pooler is **`managed`** — there is
> no separate enable toggle to set.

**When to turn it off** (`pgBouncer.managed: false` →
[`platform_managed_postgres_no_pooler.yaml`](../config/samples/platform_managed_postgres_no_pooler.yaml)):
only with a specific reason — e.g. you raised the cluster's own `max_connections` via
`database.managed.overrides` (`spec.postgresql.parameters.max_connections`), or you front the DB
with your own external pooler. **If unsure, keep the default.**

**Tuning** (managed pooler on):

```yaml
spec:
  database:
    mode: managed
    managed: { instances: 3, version: "18", storage: { size: 100Gi } }
    pgBouncer:
      managed: true
      instances: 2                 # pooler pod count (default 1; raise for redundancy)
      parameters:                  # extra pgbouncer.ini settings, passed to the CNPG Pooler
        default_pool_size: "50"    # server-side connections per (user, db) pool
        max_client_conn: "2000"    # client-facing connection ceiling
```

`parameters` is a string→string map (pgbouncer.ini is all text). The operator sets safe defaults
(`server_reset_query = DISCARD ALL`, `server_reset_query_always = 1`, `max_prepared_statements`,
`ignore_startup_parameters`, `default_pool_size`, `max_client_conn`); anything you set in
`parameters` overrides the matching default (caller wins). Keep values non-sensitive.

---

## Managed engine versions

Each managed dependency has its own `version`, selecting the **upstream operator's container
image** for that engine:

| Field | Selects | Example |
|---|---|---|
| `database.managed.version` | PostgreSQL major (CloudNativePG image) | `"18"` |
| `messaging.managed.version` | RabbitMQ server version | `"4.3.1"` |
| `keycloak.managed.version` | Keycloak server version | `"26.6.3"` |

**Omit a `version`** to use the default pinned by the selected platform bundle (`spec.version`) —
the latest engine version **validated** with that platform release. **Pinning** lets you choose a
different supported version or simply record the exact one you run (GitOps, change control). See
[`platform_managed_pinned_versions.yaml`](../config/samples/platform_managed_pinned_versions.yaml)
and the supported matrix in [`versions.md`](./versions.md).

**Major upgrades are guarded.** A fresh deploy and patch/minor bumps apply freely. A **major**
bump of an already-running managed instance (PostgreSQL `17→18`, RabbitMQ `3.x→4.x`, Keycloak
`25→26`) is a one-way, data-affecting operation with upstream prerequisites, so the operator
**blocks** it until you acknowledge it on that block:

```yaml
spec:
  database:
    managed:
      version: "18"
      upgradeAcknowledged: true    # required to upgrade a RUNNING cluster across a major
```

Until then the operator surfaces an actionable condition (`DatabaseUpgradeBlocked` /
`MessagingUpgradeBlocked` / `KeycloakUpgradeBlocked`, reason `MajorUpgradeNeedsAck`) + a `Warning`
Event, and leaves the running version in place. The same `upgradeAcknowledged` field exists on
`messaging.managed` and `keycloak.managed`. Reset it to `false` after the upgrade completes. Read
[`upgrades.md`](./upgrades.md) before acknowledging a major bump.

---

## High availability & scaling

`spec.highAvailability.enabled: true` applies HA **defaults** to the **stateless** tier (core,
auth, scheduler, fe-administrator, auth-opa-policies, utils, gateway): a multi-replica floor, a
`PodDisruptionBudget` (minAvailable 1), and node-spread anti-affinity. Every default is
overridable per component (set `replicas`, `podDisruptionBudget`, `autoscaling`, or `affinity`).

**Stateful managed-infra HA is separate** — it is the upstream operators' concern, sized via each
managed block's own count: `database.managed.instances`, `messaging.managed.replicas`,
`keycloak.managed.instances`.

See [`platform.md` → High availability](./platform.md#high-availability-poddisruptionbudgets-autoscaling)
for the per-component HPA/PDB mechanics, and
[`platform_high_availability.yaml`](../config/samples/platform_high_availability.yaml).

---

## Production sizing

`highAvailability` gives the stateless tier its *replica count, PDB, and anti-affinity*; **it does
not set resource requests/limits or storage** — size those yourself.
[`platform_production.yaml`](../config/samples/platform_production.yaml) ties it together; the
guidance:

- **Pin `spec.version`** so upgrades are deliberate, not implicit.
- **Resources.** Set `resources.requests` (for scheduling) and `resources.limits` (to cap blast
  radius) per component and per managed block (`database.managed.resources`,
  `messaging.managed.resources`). Requests are required for HPA CPU/memory targets to work.
- **Stateless replicas.** Either a fixed `replicas`, or `autoscaling` (an HPA — the operator then
  omits `.spec.replicas` so the HPA owns the count). Autoscaling needs a matching resource request.
- **Stateful redundancy.** `database.managed.instances: 3` (1 primary + 2 standbys),
  `messaging.managed.replicas: 3` (quorum), `keycloak.managed.instances: 2`.
- **Storage.** Set `storage.size` and pin a fast `storage.storageClass` for managed DB/broker.
- **Pooler.** Keep it on; raise `pgBouncer.instances` for redundancy and tune `parameters` (above).
- **Edge.** Use a real issuer (`tls.source: issuerRef` to a corporate CA, or `letsEncrypt` with
  `environment: production`), and set `gateway.trustedIps` to your LB/ingress range so
  `X-Forwarded-*` headers are honored.

---

## Edge & TLS

`spec.edge` exposes the platform over HTTPS. `type` is `ingress` (default) or `gatewayAPI`
(HTTPRoute). `edge.tls.source` chooses how the serving certificate is obtained:

| `tls.source` | Cert comes from | Needs cert-manager? | Sample |
|---|---|---|---|
| `internal` (default) | a self-signed CA via cert-manager | yes | — |
| `letsEncrypt` | ACME (needs `letsEncrypt.email`) | yes | [`platform_edge_letsencrypt.yaml`](../config/samples/platform_edge_letsencrypt.yaml) |
| `issuerRef` | any existing Issuer/ClusterIssuer | yes | [`platform_edge_issuerref.yaml`](../config/samples/platform_edge_issuerref.yaml) |
| `secret` | a bring-your-own TLS Secret | **no** | [`platform_edge_byo_secret.yaml`](../config/samples/platform_edge_byo_secret.yaml) |

A Gateway API edge ([`platform_gatewayapi.yaml`](../config/samples/platform_gatewayapi.yaml))
either owns its Gateway (`gatewayAPI.gatewayClassName`) or attaches to an existing one
(`gatewayAPI.parentRef`). The served host comes from `edge.host` or, when unset, the canonical
`common.hostName`. Full walkthrough: [`platform.md`](./platform.md).

---

## Secrets & key-mapping

No secret value ever goes in the CR. Two reference surfaces:

- **Typed infra credentials** — `database.credentials` / `messaging.credentials` (external mode),
  with `usernameKey`/`passwordKey` mapping; plus `common.trustedCertificates.caKey`,
  `provisioning.apiKey`, and the `registerAdmin` cert/password keys. See the
  [secret-key reference](./platform.md#secret-key-reference-verified-against-the-operator-code).
- **Arbitrary per-component Secrets/ConfigMaps** — `<component>.secretRefs` / `configMapRefs`
  consume a Secret/ConfigMap as **env** or a **volume** with per-key mapping. See
  [`platform.md` → Bring-your-own Secret keys](./platform.md#bring-your-own-secret-keys-key-mapping).

For Vault Agent injection (sidecar writes secrets to a shared volume), see
[`platform_vault_injection.yaml`](../config/samples/platform_vault_injection.yaml) and the
fleet-wide `common.initContainers`/`sidecars` passthrough.

---

## Other platform-wide knobs

| Field | Default | What it does |
|---|---|---|
| `deletionPolicy` | `Retain` | `Retain` leaves managed infra + data on Platform delete; `Delete` reclaims it. |
| `networkPolicy.enabled` | `true` | Default-deny ingress isolation (opt-out). `ingressNamespace` names the ingress controller's namespace (default `ingress-nginx`). |
| `provisioning.mode` | `external` | `external` points Core at your provisioner (`apiURL` + `apiKeySecretRef`); `deploy` renders the bundled provisioning-rabbitmq service (requires `messaging.brokerType: rabbitmq`). |
| `messaging.management.expose` | `false` | Publish the RabbitMQ management UI on `/mq` (managed RabbitMQ only). |
| `registerAdmin.enabled` | `false` | First-admin bootstrap by certificate (mTLS) and/or password (managed-Keycloak realm user). |
| `gateway.cors` / `gateway.logging` / `gateway.trustedIps` | off / off / — | Kong CORS plugin, request logging, and trusted client IP ranges for `X-Forwarded-*`. |
| `common.proxy` | off | Outbound HTTP(S) proxy for all components (`PROXY_ENABLED` + `HTTP(S)_PROXY`/`NO_PROXY`). |
| `additionalEnv` | — | Non-sensitive env applied to every component (merged before each component's own env). |

---

## Complete option index

Every field of `spec`, with where to read more. Defaults are the apiserver defaults.

### Top level (`spec.*`)

| Field | Default | Purpose |
|---|---|---|
| `version` | newest bundle | Platform version bundle ([versions.md](./versions.md)). |
| `common` | — | Fleet-wide config (see below). |
| `database` | *required* | DB connection — `external`/`managed` (+ `pgBouncer`). |
| `messaging` | *required* | AMQP broker — `external`/`managed` (+ `management.expose`). |
| `keycloak` | external/none | OIDC provider — `external`/`managed`. |
| `highAvailability` | off | Stateless-tier HA defaults. |
| `additionalEnv` | — | Fleet-wide non-sensitive env. |
| `networkPolicy` | enabled | Default-deny isolation (opt-out). |
| `core` | — | Core component overrides + `clientCertHeader`. |
| `provisioning` | external | Remote-proxy provisioning wiring. |
| `auth` | — | Auth overrides + `create`/`syncPolicy`. |
| `scheduler` / `authOpaPolicies` / `feAdministrator` / `utils` / `gateway` | — | Component overrides (`utils.enabled` opt-in; `gateway` adds CORS/logging/trustedIps; `feAdministrator` adds `url`). |
| `edge` | none | External HTTPS edge (Ingress / Gateway API + TLS). |
| `registerAdmin` | off | First-admin bootstrap. |
| `deletionPolicy` | `Retain` | Managed-infra deletion behavior. |

### `spec.common.*`

| Field | Purpose |
|---|---|
| `image` | Shared image (registry/repository/name/tag/digest/pullPolicy/pullSecrets/command/args) for all components. |
| `hostName` | Canonical public FQDN (edge host, Keycloak `KC_HOSTNAME`, OIDC URIs, CORS origin). |
| `proxy` | Outbound HTTP(S) proxy for all components. |
| `logging.level` | Platform log level (default `INFO`). |
| `trustedCertificates` | CA bundle (`TRUSTED_CERTIFICATES`) by `secretRef` (+ `caKey`). |
| `initContainers` / `sidecars` / `volumes` / `volumeMounts` / `additionalPorts` | Fleet-wide pod-template passthrough (SCC-hardened). |
| `additionalEnvFrom` | Whole-Secret / whole-ConfigMap `envFrom` (names only). |
| `nodeSelector` / `affinity` / `tolerations` | Fleet-wide scheduling for all (stateless) components; a component's own overrides/augments (nodeSelector **merges**, tolerations **append**, affinity **replaces** + beats the HA default). Managed infra is scheduled via each block's `overrides`. |
| `podAnnotations` / `podLabels` | Fleet-wide pod annotations/labels (e.g. mesh injection, cost/team labels); a component's own win on key collision, operator-managed keys always win. |

### Per-component overrides (`spec.<component>.*`, shared by all components)

| Field | Purpose |
|---|---|
| `image` | Per-field image override (falls back to `common.image`, then the bundle). |
| `replicas` | Fixed replica count (ignored when `autoscaling` is set). |
| `workloadType` | `Deployment` (default) or `StatefulSet`. |
| `podDisruptionBudget` | PDB (`enabled` + `minAvailable`/`maxUnavailable`). |
| `autoscaling` | HPA (`minReplicas`/`maxReplicas` + CPU/memory target). |
| `resources` | Container requests/limits. |
| `env` | Non-sensitive inline env (last-wins). |
| `secretRefs` / `configMapRefs` | Mount/inject Secrets/ConfigMaps (env or volume, key-mapped). |
| `volumes` | Extra emptyDir volumes mounted into the main container. |
| `probes` | Liveness/readiness/startup overrides. |
| `securityContext` | Always SCC-hardened (fill-don't-replace). |
| `podAnnotations` / `podLabels` | Pod-template metadata. |
| `nodeSelector` / `affinity` / `tolerations` | Scheduling. |
| `initContainers` / `sidecars` | Appended, SCC-hardened. |
| `serviceAccount` | Override SA name + annotations (e.g. IRSA/workload-identity). |
| `service` | Service `port`/`type`. |
| `metrics` | Metrics endpoint + optional Prometheus `ServiceMonitor`. |

### Managed-infra blocks

| Field | Purpose |
|---|---|
| `database.managed` | `instances`, `version`, `upgradeAcknowledged`, `storage`, `resources`, `backup`, `overrides`. |
| `database.pgBouncer` | `managed` (the pooler switch), `instances`, `parameters`. |
| `messaging.managed` | `replicas`, `version`, `upgradeAcknowledged`, `storage`, `resources`, `overrides`. |
| `keycloak.managed` | `instances`, `version`, `upgradeAcknowledged`, `logLevel`, `realmImport`, `overrides`. |

---

## Where to look next

- [`platform.md`](./platform.md) — getting started, prerequisites, the managed-infra deep dives.
- [`versions.md`](./versions.md) — supported platform + engine version matrix.
- [`upgrades.md`](./upgrades.md) — platform and managed-infra upgrade procedures.
- [`config/samples/`](../config/samples/) ([index](../config/samples/README.md)) — validated example CRs.
- [`design/examples/platform-cr-reference.yaml`](./design/examples/platform-cr-reference.yaml) — annotated every-field reference.
