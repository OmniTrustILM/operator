# Migrating from the Helm umbrella chart to the Platform CR

This guide walks you from a running **ILM umbrella Helm chart** install to an
operator-managed **`Platform`** custom resource (`otilm.com/v1alpha1`). It is the
practical companion to the design in [`design/platform-operator.md`](design/platform-operator.md);
read that first if you want the *why*.

The migration is **data-safe** and, for the HTTP path, **zero-downtime**: the platform's
state lives in PostgreSQL and the broker, not in the stateless pods. You stand up the
operator-managed platform against the **same** infrastructure, drain the old consumers,
flip the edge, and decommission the chart.

There is one **unavoidable manual step**: **secrets are never auto-migrated.** The chart
keeps secrets inline in `values.yaml`; the CR keeps only *references* to Kubernetes
`Secret`s. You create those Secrets yourself (a deliberate human decision). The
`values2platform` converter scaffolds everything else and tells you exactly which Secrets
to create.

> **Honesty up front.** Expect manual intervention in two places, always: (1) extracting
> inline secrets into Secrets, and (2) any customization-heavy install (raw
> sidecars/initContainers, per-connector config, bundled-infra → managed decisions). The
> converter gets you ~90% of the way and flags the rest with `# TODO`/`# UNMAPPED`.

---

## Migration scenarios — decide per dependency

Migration is decided **per dependency**, not globally. Each stateful dependency (database,
messaging, Keycloak) was either *external* (you run it) or *chart-managed* (in-cluster) in
the chart, and each maps to an operator mode with a specific data-handling rule. Mix freely.

| Dependency | In your chart | → Operator | How nothing is lost |
|---|---|---|---|
| **Database** | external (you run PostgreSQL) | `database.mode: external` — same host/name/creds | **Re-point.** No data moves; Core self-migrates schema (Flyway); the operator never touches the DB. ← safest |
| **Database** | in-cluster / bundled PostgreSQL | (a) keep it as `external` pointed at the existing Service, **or** (b) `mode: managed` + `pg_dump`→restore into the new CloudNativePG cluster (§6b) | (a) is safest (no copy); (b) is a real data copy — back up + verify row counts first |
| **Messaging** | chart-managed RabbitMQ | `messaging.mode: managed` (RabbitMQ operators) | **Recreate.** RabbitMQ holds no business data — only topology (re-declared) + transient messages. Drain Core first. |
| **Messaging** | external broker | `messaging.mode: external` — same host/vhost/creds | **Re-point.** |
| **Keycloak** | chart-managed (shares the platform DB) | `keycloak.mode: managed`, same DB (`keycloak` schema) | **Re-point — realm identity must match** (see the Keycloak-realm section). Realm/users live in PostgreSQL; preserved as long as the DB is, and the operator targets the existing realm + `ilm` client. |
| **Keycloak** | external OIDC | `keycloak.mode: external` | **Re-point.** |

**"Everything managed" (the typical case): external DB + in-cluster RabbitMQ + in-cluster Keycloak** → keep the DB external (re-point), let the operator manage RabbitMQ (recreate) and Keycloak (re-point at the same DB). All durable data is in the external PostgreSQL, so it survives by simply not touching that database.

> **Golden rule:** **re-point at the data you already have** (external mode); never let the operator *re-provision* a store that holds live data. Re-provisioning (a fresh CloudNativePG / a fresh Keycloak DB) is a data copy **you** must perform and verify — see §6b.

## The safety contract — no change for your users

Done as below, migration is **transparent to end users**: same URL, same login, same
certificates, same data, same connectors. The operator preserves user-facing behavior
because all durable state stays in the database you keep. To *uphold* that guarantee, these
invariants must hold across the cut:

- **Database preserved untouched** — same instance, same data (external mode), with a verified backup taken first.
- **Public hostname unchanged** — `spec.common.hostName` = your current FQDN, the same TLS cert (or a fresh cert for the *same* host), the same edge behavior incl. mTLS / `auth-tls`.
- **Keycloak realm + client identity preserved** — same realm name and the `ilm` client, so OIDC **issuer URLs and login are byte-identical** for users and external integrations. (Rebranding the realm name *does* change issuer URLs — see the Keycloak-realm section.)
- **Connector service URLs unchanged** — each `Connector` CR keeps the **same Service name/URL** Core's database has registered, so authorities/discovery/compliance keep resolving.
- **Admin identity preserved** — it already exists in the database; cert-based admin login keeps working through the same mTLS edge.

Not user-visible but worth knowing: the broker is recreated (transient state only), pod names/labels change, and connectors become separate CRs (same workloads, same URLs). None of these change the API, UI, login, or stored data.

---

## 0. Prerequisites

- The ILM operator is installed in the cluster (its CRDs — `platforms.otilm.com`,
  `connectors.otilm.com` — are present: `kubectl get crd | grep otilm`).
- `helm`, `kubectl`, and a checkout of this repo (for the `values2platform` converter).
- cert-manager installed if you use `edge.tls.source` of `internal` / `letsEncrypt` /
  `issuerRef` (the chart's ingress TLS).
- For **managed** infrastructure (optional, see step 6b): the relevant upstream operators
  — CloudNativePG (database), the RabbitMQ Cluster + Messaging Topology operators
  (messaging), the Keycloak Operator (Keycloak).

---

## 1. The mapping: Helm values → Platform CR

`Platform` is a single namespaced CR. Below is the field-by-field mapping the converter
implements. Status:

- **COVERED** — the converter maps it automatically.
- **PARTIAL** — mapped, but you must finish a decision (a managed-mode block, a missing
  host, a bring-your-own TLS Secret name).
- **SECRET → REF** — the converter emits a Secret *reference* + a create-secret `# TODO`;
  the plaintext is never copied (see step 2).
- **CONNECTOR CRD** — not part of `Platform`; becomes a separate `Connector` resource.
- **UNMAPPED** — no CR equivalent; flagged `# UNMAPPED`.

### Common / shared (`spec.common`)

Everything that applies to **every** component now lives under one consolidated
`spec.common` (the former root `image`/`proxy`/`logging`/`trustedCertificates` scalars and
the old `spec.global` passthrough), plus the new canonical `spec.common.hostName`.

| Helm value | Platform CR field | Status |
|---|---|---|
| `image.{registry,repository,name,tag,pullPolicy}` | `spec.common.image.*` (shared image) | COVERED |
| `global.image.pullSecrets` | `spec.common.image.pullSecrets` | COVERED |
| `image.probes.*` | per-component `spec.<component>.probes` | PARTIAL (move per component) |
| `global.hostName` / `hostName` | `spec.common.hostName` (canonical public FQDN) | COVERED |
| `additionalEnv.variables` | `spec.additionalEnv` | COVERED |
| `logging.level` | `spec.common.logging.level` | COVERED |
| `logging.audit` | — | UNMAPPED (platform-config, not operator-modeled) |
| `global.httpProxy` / `httpsProxy` / `noProxy` | `spec.common.proxy.{http,https,noProxy}` (+ `enabled`) | COVERED |
| `javaOpts` (if set) | per-component `JAVA_OPTS` **env** on the JVM components (`core`, `auth`, `scheduler`, and `provisioning.deploy`) — the special-case `spec.javaOpts` was **removed** | COVERED (mapped to env; not on fe/opa/gateway) |
| `global.initContainers` / `sidecarContainers` / `additionalVolumes` / `additionalVolumeMounts` / `additionalPorts` | `spec.common.{initContainers,sidecars,volumes,volumeMounts,additionalPorts}` | PARTIAL (`# TODO(customization)` — move raw pod-spec fragments by hand) |
| `global.additionalEnv.secrets` / `configMaps` | `spec.common.additionalEnvFrom.{secrets,configMaps}` (NAMES only) | PARTIAL (`# TODO`) |

> **Non-released / `develop-latest` images.** `spec.version` selects a *tested bundle* — the
> env wiring, the managed-RabbitMQ topology, and the **default** component image tags for a
> release. It is independent of the image you actually run: to track a moving tag (e.g.
> `develop-latest`) pin `spec.version` to the closest bundle for the contract and set
> `spec.common.image.tag` (which the converter does from `image.tag`). A specific managed
> broker version is `messaging.managed.version`, not the bundle default.

### Database

| Helm value | Platform CR field | Status |
|---|---|---|
| `global.database.host` | `spec.database.host` (`mode: external`) | COVERED |
| `global.database.port` | `spec.database.port` | COVERED |
| `global.database.name` | `spec.database.name` | COVERED |
| `global.database.username` / `password` | `spec.database.credentials.secretRef` → **`ilm-db`** | SECRET → REF |
| `global.database.pgBouncer.enabled` | `spec.database.pgBouncer.managed` | COVERED (managed-DB Pooler is a managed-mode decision) |
| `pgBouncer.section.*` (raw `pgbouncer.ini`) | — | UNMAPPED (not a CR field; see the pooling note) |

> **⚠️ External database + connection pooling.** The chart fronts even an *external* database
> with a bundled PgBouncer (`pg-bouncer-service`). The operator does **not** — a managed pooler
> is a *managed*-database feature only (`database.pgBouncer.managed`), and `mode: external`
> connects Core **directly**. The ILM fleet (core + auth + scheduler + Keycloak) opens enough
> pools to exhaust PostgreSQL's default `max_connections`, so for an external DB you must either
> **(a)** raise the server's `max_connections` to cover the fleet, **or (b)** keep a PgBouncer
> (transaction mode) in front and point `spec.database.host` at *that pooler's* Service instead
> of the raw server. This is the one operational behavior `mode: external` does not carry over
> from the chart.

### Messaging

| Helm value | Platform CR field | Status |
|---|---|---|
| `global.messaging.host` | `spec.messaging.host` (`mode: external`) | COVERED |
| `global.messaging.port` / `virtualHost` | `spec.messaging.{port,virtualHost}` | COVERED |
| `global.messaging.username` / `password` | `spec.messaging.credentials.secretRef` → **`ilm-messaging`** | SECRET → REF |
| `global.messaging.remoteAccess` | `spec.messaging.management.expose` (the gateway still renders `/mq`) | COVERED |
| *(bundled RabbitMQ subchart — no external host)* | `spec.messaging` (external) **or** `mode: managed` | PARTIAL — see step 6 |

### Keycloak

| Helm value | Platform CR field | Status |
|---|---|---|
| `global.keycloak.enabled: true` | `spec.keycloak.mode: managed` (+ `realm`) | PARTIAL (fill `keycloak.managed`) |
| `global.keycloak.clientSecret` | generated by managed Keycloak + read back; or `Secret` ref | SECRET → REF |
| `keycloakInternal.*` (bundled Keycloak image/theme/args/logging) | `spec.keycloak.managed.*` (+ `overrides`) | PARTIAL (`# TODO(customization)`) |

### Edge (ingress) and TLS

| Helm value | Platform CR field | Status |
|---|---|---|
| `ingress.enabled` | `spec.edge.enabled` | COVERED |
| `ingress.class` | `spec.edge.className` | COVERED |
| `global.hostName` / `hostName` | `spec.common.hostName` (the edge inherits it via `PlatformHost`; set `spec.edge.host` only to override per-edge) | COVERED |
| `ingress.annotations` | `spec.edge.annotations` | COVERED |
| `ingress.certificate.source: internal` | `spec.edge.tls.source: internal` | COVERED |
| `ingress.certificate.source: letsencrypt` + `letsEncrypt.{email,environment}` | `spec.edge.tls.source: letsEncrypt` + `letsEncrypt.*` | COVERED |
| `ingress.certificate.source: external` | `spec.edge.tls.source: secret` (set `secretRef`) | PARTIAL (`# TODO` — set your TLS Secret name) |

### API gateway (Kong)

| Helm value | Platform CR field | Status |
|---|---|---|
| `apiGateway.trustedIps` | `spec.gateway.trustedIps` (comma-split → list) | COVERED |
| `apiGateway.logging.request` | `spec.gateway.logging.request` | COVERED |
| `apiGateway.logging.level` | — | UNMAPPED (Kong log level not modeled) |
| `apiGateway.cors.{enabled,origins,exposedHeaders}` | `spec.gateway.cors.*` | COVERED |
| `apiGateway.hostAliases` | — | UNMAPPED (operator derives host-aliases from `edge.host` / managed Keycloak) |

### Per-component overrides

The chart's per-service blocks map to each component's shared override surface. Core's
image is the chart's top-level `image` block; every other component reads its own block.

| Helm value (per service) | Platform CR field | Status |
|---|---|---|
| `image` (top-level) | `spec.core.image` | COVERED |
| `authService.*` | `spec.auth.*` | COVERED |
| `authOpaPolicies.*` | `spec.authOpaPolicies.*` | COVERED |
| `schedulerService.*` | `spec.scheduler.*` | COVERED |
| `feAdministrator.*` | `spec.feAdministrator.*` | COVERED |
| `utilsService.*` / `global.utils.enabled` | `spec.utils.*` (+ `enabled`) | COVERED |
| `<service>.image.{registry,repository,name,tag,pullPolicy}` | `spec.<component>.image.*` | COVERED |
| `<service>.replicaCount` / `replicas` | `spec.<component>.replicas` | COVERED |
| `<service>.resources` | `spec.<component>.resources` | COVERED |
| `<service>.additionalEnv.variables` | `spec.<component>.env` | COVERED |
| `<service>.logging.level` | `spec.<component>.env` (`LOGGING_LEVEL_*`) or `spec.common.logging.level` | PARTIAL (`# TODO`) |

### First-admin bootstrap and provisioning

| Helm value | Platform CR field | Status |
|---|---|---|
| `registerAdmin.enabled` | `spec.registerAdmin.enabled` | COVERED |
| `registerAdmin.admin.{username,name,email}` | `spec.registerAdmin.{username,name,email}` | COVERED |
| `registerAdmin.admin.certificate` (inline PEM) | `spec.registerAdmin.certificate.secretRef` → **`ilm-admin-cert`** (`kubernetes.io/tls`) | SECRET → REF |
| `ilm.admin.{username,name,email}` (chart's realm-seeded password admin) | `spec.registerAdmin.{username,name,email}` | COVERED |
| `ilm.admin.password` (chart seeds a Keycloak realm password user) | `spec.registerAdmin.password.secretRef` → **`ilm-admin-password`** (generic, key `password`); requires `keycloak.mode: managed` | SECRET → REF |
| `global.provisioning.apiUrl` | `spec.provisioning.apiURL` (top-level, `mode: external`) | COVERED |
| `global.provisioning.apiKey` | `spec.provisioning.apiKeySecretRef` → **`ilm-provisioning`** | SECRET → REF |

### Connectors (a separate CRD)

The connector subcharts are **not** part of the `Platform` CR — they are managed by the
**`Connector`** CRD (one resource per connector). The converter flags them with
`# TODO(customization)`; convert each to a `Connector` (see
[`design/connector-operator.md`](design/connector-operator.md)).

`commonCredentialProvider`, `ejbcaNgConnector`, `externalAuthorityProvider`,
`pyAdcsConnector`, `otpkiConnector`, `hashicorpVaultConnector`,
`timestampFormattingConnector`, `x509ComplianceProvider`, `cryptosenseDiscoveryProvider`,
`ctLogsDiscoveryProvider`, `networkDiscoveryProvider`, `keystoreEntityProvider`,
`softwareCryptographyProvider`, `emailNotificationProvider` (its `smtp.*` credentials stay
inline-secrets *there*), `webhookNotificationProvider`, `registerConnectors`.

---

## 2. Prerequisite Secrets (the manual part)

The converter never copies a plaintext secret. For every inline secret in your values it
emits a Secret **reference** in the CR and a `# TODO: create the following Secrets` block
at the top of its output, with a ready-to-edit `kubectl create secret` line. The default
Secret names and the keys the operator reads (its *wiring profile*):

| Secret (default name) | Keys the operator reads | Replaces (values path) | Create with |
|---|---|---|---|
| `ilm-db` | `username`, `password` | `global.database.username` / `password` | `kubectl create secret generic ilm-db -n <ns> --from-literal=username='<DB_USER>' --from-literal=password='<DB_PASSWORD>'` |
| `ilm-messaging` | `username`, `password` | `global.messaging.username` / `password` | `kubectl create secret generic ilm-messaging -n <ns> --from-literal=username='<MQ_USER>' --from-literal=password='<MQ_PASSWORD>'` |
| `ilm-trusted-ca` | `ca.crt` | `global.trusted.certificates` (PEM) | `kubectl create secret generic ilm-trusted-ca -n <ns> --from-file=ca.crt=./trusted-ca.pem` |
| `ilm-admin-cert` | `tls.crt`, `tls.key` | `registerAdmin.admin.certificate` (PEM) | `kubectl create secret tls ilm-admin-cert -n <ns> --cert=./admin.crt --key=./admin.key` |
| `ilm-admin-password` | `password` | `ilm.admin.password` (the realm-seeded admin user) | `kubectl create secret generic ilm-admin-password -n <ns> --from-literal=password='<ADMIN_PASSWORD>'` |
| `ilm-provisioning` | `provisioningApiKey` | `global.provisioning.apiKey` | `kubectl create secret generic ilm-provisioning -n <ns> --from-literal=provisioningApiKey='<PROVISIONING_API_KEY>'` |
| `ilm-keycloak-client` | `clientSecret` | `global.keycloak.clientSecret` | usually unnecessary — managed Keycloak generates this and the operator reads it back |

> **Bring-your-own keys.** If your Secret already exists (e.g. from External Secrets, Vault,
> or a CloudNativePG-shaped Secret with `POSTGRES_USER` / `POSTGRES_PASSWORD`), you do
> **not** have to rename keys: set the in-Secret key overrides on the CR —
> `database.credentials.usernameKey` / `passwordKey`, `messaging.credentials.*`,
> `common.trustedCertificates.caKey`, `registerAdmin.certificate.certKey` /
> `registerAdmin.certificate.privateKeyKey`, `registerAdmin.password.passwordKey`,
> `provisioning.apiKey`.

### Extracting inline secrets from your current values

The PEM/credential material you need is already in your `values.yaml`. Extract it to files
(never commit these), create the Secrets, then delete the files:

```bash
# Trusted CA bundle (global.trusted.certificates):
#   copy the PEM blocks into ./trusted-ca.pem, then:
kubectl create secret generic ilm-trusted-ca -n <ns> --from-file=ca.crt=./trusted-ca.pem

# Admin client cert (registerAdmin.admin.certificate is the cert; the matching private key
# lives wherever you generated it):
kubectl create secret tls ilm-admin-cert -n <ns> --cert=./admin.crt --key=./admin.key

# DB / messaging / provisioning credentials (from global.database / messaging / provisioning):
kubectl create secret generic ilm-db -n <ns> \
  --from-literal=username='<DB_USER>' --from-literal=password='<DB_PASSWORD>'

rm -f ./trusted-ca.pem ./admin.crt ./admin.key   # do not leave plaintext on disk
```

If you are migrating against the **running** chart's infrastructure, the DB and broker
credentials are the **same** ones the chart already uses — reuse them so the
operator-managed platform connects to the same database and broker.

---

## 3. Run the converter

The converter is a `go run`-able CLI in this repo. It reads your `values.yaml` and writes
a scaffolded `Platform` CR to stdout.

```bash
# from the operator repo root
go run ./cmd/values2platform -f /path/to/your/values.yaml -namespace ilm > platform.yaml

# (positional form also works)
go run ./cmd/values2platform /path/to/your/values.yaml > platform.yaml
```

Flags: `-f` (values path; or positional), `-name` (CR name, default `ilm`), `-namespace`
(CR + scaffolded-Secrets namespace, default `ilm`).

Then **read the output top to bottom**:

1. The header `# TODO: create the following Secrets` block — create each Secret (step 2).
2. The CR body — review every field.
3. The footer `# TODO(customization)` and `# UNMAPPED` notes — close each hole by hand
   (managed-mode blocks, raw sidecars, connectors, a missing `edge.host`, etc.).

### Example output (excerpt)

Running it against a representative `values.yaml` produces (trimmed):

```yaml
# ---------------------------------------------------------------------------
# Platform CR scaffolded by values2platform (best-effort).
# ...
# TODO: create the following Secrets (edit the placeholder values):
#   * ilm-db  (keys: username, password)
#     database credentials (was global.database.username/password)
#     kubectl create secret generic ilm-db -n ilm --from-literal=username='<DB_USER>' ...
#   * ilm-trusted-ca  (keys: ca.crt)
#     kubectl create secret generic ilm-trusted-ca -n ilm --from-file=ca.crt=./trusted-ca.pem
# ---------------------------------------------------------------------------
apiVersion: otilm.com/v1alpha1
kind: Platform
metadata:
  name: ilm
  namespace: ilm
spec:
  common:                          # everything applied to EVERY component lives here
    hostName: ilm.example.com      # <-- canonical public FQDN (from global.hostName)
    image: { registry: harbor.example.com, repository: ilm, tag: "2.18.0" }
    trustedCertificates:
      secretRef: ilm-trusted-ca    # <-- reference, not the PEM
  database:
    credentials:
      secretRef: ilm-db            # <-- reference, not the password
    host: db.example.com
    mode: external
    name: ilmdb
  core:
    env:
      - { name: JAVA_OPTS, value: "-XX:MaxRAMPercentage=75.0" }   # <-- was the removed spec.javaOpts
  edge:
    enabled: true
    className: nginx               # edge.host omitted → the edge uses common.hostName (set it only to override per-edge)
    tls:
      source: letsEncrypt
      letsEncrypt: { email: ops@example.com, environment: production }
  # ... per-component image overrides, gateway, registerAdmin, ...

# ---------------------------------------------------------------------------
# NOTES — gaps the converter could not (or should not) map automatically.
# TODO(customization): javaOpts is no longer a CR field; mapped to per-component JAVA_OPTS env ...
# TODO(customization): connectors (ejbcaNgConnector, ...) -> the SEPARATE Connector CRD
# TODO(customization): messaging: no external broker host found ... set messaging.host ...
# UNMAPPED: logging.audit (no Platform CR field)
# ---------------------------------------------------------------------------
```

### Validate the CR against the live CRD (recommended)

After creating the Secrets and closing the holes, validate structurally with a
server-side dry-run (no objects created):

```bash
kubectl apply --dry-run=server -f platform.yaml
```

The apiserver enforces the CR's cross-field rules — so a still-open hole surfaces here as a
clear message rather than a late failure. For example, a converted CR that still says
`messaging.mode: external` with no host, or `keycloak.mode: managed` with no `managed`
block, is rejected with exactly that message. Close the hole and re-run.

---

## 4. Eyeball the rendered objects before cutover (optional)

Before you cut over against live infrastructure, it is worth a quick look at what the
operator will render for the components you customized — to catch an env var you forgot to
map or a missing annotation **before** a live cutover. A server-side dry-run prints the
admitted CR (with defaults applied) without creating anything:

```bash
kubectl apply --dry-run=server -o yaml -f platform.yaml
```

If you want to compare against your current chart output for a specific component, render the
chart with `helm template` and diff the relevant Deployment/Service/ConfigMap by eye. The
operator is the single source of truth going forward, so this is a one-time confidence check,
not an ongoing requirement.

---

## 5. Detach the chart's bundled stateful infrastructure (only if you used it)

If your chart install used the **bundled** RabbitMQ / Keycloak / PgBouncer (rather than
external infra), you must keep their data before uninstalling the chart. Deleting a
StatefulSet does **not** delete its `volumeClaimTemplate` PVCs, but the chart owns the
StatefulSet, so a `helm uninstall` stops those pods.

**Exact order** (do not skip the verification):

1. `helm upgrade` to add `helm.sh/resource-policy: keep` to the RabbitMQ / Keycloak /
   PgBouncer resources (so `helm uninstall` later leaves them).
2. **Verify** the annotation is present on those resources (`kubectl get sts ... -o yaml`).
3. **Verify** the PVs' `reclaimPolicy` is `Retain` (not `Delete`) before any PVC-level
   operation: `kubectl get pv -o custom-columns=NAME:.metadata.name,RECLAIM:.spec.persistentVolumeReclaimPolicy`.
4. Only **then** proceed with the operator deployment below and, at the end, `helm uninstall`.

> Adopting existing bundled infra *into* operator-`managed` mode is a separate, later step;
> `managed` mode is primarily for **new** installs. For migration, the low-risk path is to
> point the CR at the **running** infrastructure in `external` mode (step 6a).

---

## Keycloak realm: preserve or rebrand (managed Keycloak only)

**Skip this if your Keycloak is external.** For a **managed** Keycloak pointed at your
existing database, the realm and its users already live in that DB — but the operator is
hard-wired to realm **`ilm`** with OIDC client **`ilm`** (both platform versions). If your
existing realm has a different name/client, the operator's *create-only* realm import would
create a **second, empty `ilm` realm** beside your populated one and **login would break**.
Reconcile this **before §6**.

**Step 1 — find your realm name**, against the same DB:

```sql
SELECT name FROM keycloak.realm;
```

**If it is already `ilm`** (recent charts import the `ilm` realm) → nothing to do; the
operator targets it and reads back the `ilm` client secret.

**If it is `CZERTAINLY`** (or anything else), pick one:

- **Option A — keep the realm name (recommended; zero user-visible change).** Add an `ilm`
  client to the *existing* realm (confidential, audience `ilm`, your redirect URIs), then set
  `spec.keycloak.realm: <existing-name>` so the operator targets your realm. **OIDC issuer
  URLs stay `…/realms/<existing-name>/…`** — no change for users or external clients. (The
  operator still requires an `ilm` *client* in that realm — it reads that client's secret.)
- **Option B — rebrand to `ilm`.** Run the chart's realm upgrade scripts
  (`charts/keycloak-internal/scripts/update_realm_from_*.py`, in order, through `…2.18.0`)
  against the **live** Keycloak *before* handoff; the last one renames the realm to `ILM` and
  the client to `ilm`. ⚠️ This **changes the OIDC issuer URL** to `…/realms/ilm/…`, so update
  any *external* OIDC integrations that hardcoded the issuer. Internal Core login is fine.

> The operator never renames a realm (its import is create-only); the choice above is a
> one-time action on the Keycloak/data side, which the operator only *reads*.

**Validate** after applying the CR (§6): `OIDCConfigured=True` on the Platform. `False` with
reason `OIDCConfigFailed` means the `ilm` client was not found in the targeted realm — fix
Option A/B before cutting browser traffic over.

---

## 6. Cut over: drain-then-switch

State lives in PostgreSQL and the broker. The rule that makes this safe: **the old chart
release and the new operator-managed platform must not consume the same queues at the same
time.** So you drain the old consumers before flipping traffic.

### 6a. Stand up the operator-managed platform against the **same** infrastructure (external mode)

Point the CR's `database` / `messaging` (and Keycloak, if external) at the **running**
infrastructure the chart uses, with the Secrets you created in step 2 (the same
credentials). Apply the CR:

```bash
kubectl apply -f platform.yaml
kubectl get platform -n ilm -w        # wait for Phase: Running / Available=True
```

The operator now renders its own stateless tier (Core, auth, scheduler,
fe-administrator, auth-opa-policies, gateway, optional utils) talking to the same DB and
broker. At this point **both** the chart and the operator-managed platform are running.
Verify the new platform's health before flipping any traffic.

### 6b. (Alternative) Adopt managed infrastructure

If instead you want the operator to **provision** the database/broker/Keycloak via the
upstream operators, set the relevant `…mode: managed` and fill each `managed` block (see
`config/samples/platform_managed_*.yaml`). This is heavier — **a re-provisioned store is a
new, empty store, so you migrate the data yourself**:

- **Database → managed CloudNativePG:** `pg_dump` the live DB and restore into the new
  cluster's app database **before** pointing Core at it; verify row counts; only then
  decommission the source. Keep the `keycloak` schema in the dump — a managed Keycloak
  shares the platform DB, so the realm/users travel with it (then honor the realm-identity
  rule in the Keycloak-realm section).
- **Messaging → managed:** no copy needed — RabbitMQ is recreated (topology re-declared).

For an in-place, no-data-loss migration prefer **external mode** (§6a): re-point at the data
you already have. Managed mode is best for new installs or a deliberate, verified data move.

### 6c. Drain the old release's consumers

Scale the **chart's** Core and scheduler consumers to zero so in-flight broker work drains
and the old release stops consuming:

```bash
kubectl scale deploy/<chart-core> deploy/<chart-scheduler> -n <chart-ns> --replicas=0
```

(Use the chart's actual Deployment names; the chart prefixes them by release.)

### 6d. Flip the edge

Move external traffic to the operator-managed gateway. With the operator's `edge` enabled
and pointed at the same `host`, switch DNS / the ingress to the operator's Ingress (or, if
both edges target the same host and class, the operator's Ingress takes over as the chart's
is removed). HTTP is stateless, so this is zero-downtime.

### 6e. Decommission the chart

Once the operator-managed platform serves all traffic and the old consumers are drained:

```bash
helm uninstall <release> -n <chart-ns>
```

If you detached bundled stateful infra in step 5, confirm those resources (and their PVCs)
survived the uninstall.

---

## 7. Post-migration checklist

**Before cutover (data-safety gates):**
- [ ] A verified **PostgreSQL backup** exists (including the `keycloak` schema); the DB you re-point at is the live one and is **not** scheduled for deletion.
- [ ] Every `# TODO`/`# UNMAPPED` from the converter output is resolved or consciously accepted.
- [ ] All prerequisite Secrets exist in the platform namespace with the right keys.
- [ ] `kubectl apply --dry-run=server -f platform.yaml` is clean.
- [ ] **Managed Keycloak:** the existing realm name is known and reconciled (realm already `ilm`, or Option A/B done) — see the Keycloak-realm section.
- [ ] **Connectors:** one `Connector` CR per enabled connector, each with the **same image** and the **same Service name/URL** Core's DB has registered (so registrations don't dangle).

**After applying the CR, before flipping traffic (the safety contract holds):**
- [ ] `kubectl get platform` shows `Running` / `Available=True` (and `EdgeReady=True` for an edge).
- [ ] **Managed Keycloak:** `OIDCConfigured=True` (the `ilm` client secret was read from your realm).
- [ ] **Edge unchanged:** `diff` the operator-rendered Ingress against the chart's — same host, same TLS, **every `auth-tls`/mTLS annotation present**, and a client-cert (mTLS) admin login actually succeeds.
- [ ] `spec.common.hostName` equals your production FQDN and the issued cert + Keycloak redirect URIs match it.
- [ ] **User-transparency smoke test:** existing user login works, an API call returns existing data, a connector round-trip (e.g. an authority/discovery op) succeeds, and the cert inventory shows your existing certificates.

**Decommission:**
- [ ] Old chart consumers drained (broker queues empty) before the chart is uninstalled.
- [ ] The chart release is uninstalled; the external DB and any retained stateful infra (and PVCs) survived.

---

## What stays manual, by design

- **Secret extraction** — always. The tool will not move plaintext for you.
- **Bundled-infra → managed decisions** — the converter emits a note; you choose
  external-against-running-infra (the migration path) or managed (new-install path).
- **Raw pod-spec passthrough** (`global.initContainers` / `sidecarContainers` / volumes) —
  moved by hand to `spec.common.*` so nothing is silently mangled.
- **Connectors** — a separate `Connector` CRD per connector.
- **Anything flagged `# UNMAPPED`** — no CR equivalent (e.g. `logging.audit`, Kong log
  level, `apiGateway.hostAliases`).

---

## See also

- [`configuration.md`](configuration.md) — the complete configuration reference & scenario
  cookbook (every Platform option, mapped to a matching sample), for resolving the converter's
  `# TODO`/`# UNMAPPED` notes by hand.
