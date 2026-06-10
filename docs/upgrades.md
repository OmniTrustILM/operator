# Upgrading the ILM platform

The ILM platform version is selected by **`spec.version`** (see [versions.md](versions.md)),
and the set of versions a given operator build can reconcile is carried in the operator's
bill-of-materials. An upgrade therefore has two layers:

1. **The operator binary** — newer operator builds ship newer (and additional) platform
   version bundles. Bumping the platform version often means upgrading the operator first.
2. **`spec.version`** — moving it to a version the running operator supports rolls the
   platform's stateless tier to that release's images and wiring.

This page describes the upgrade procedure and the **safety guard** that protects managed
(operator-provisioned) infrastructure from a one-way major-version jump.

## The basic upgrade

```yaml
spec:
  version: "2.18.0"     # was "2.17.0"
```

1. **Upgrade the operator** if the target platform version is newer than what the running
   operator supports. (Check the supported set: an unknown `spec.version` is `Degraded` with
   a message listing the supported versions — see [versions.md](versions.md).)
2. **Set `spec.version`** to the target release and apply.
3. The operator re-resolves the version bundle and **render-applies** the new component
   images, wiring, and (for managed RabbitMQ) any topology changes. The stateless components
   roll via Deployment rollouts; a configuration change rolls Core via a `checksum/config`
   pod-template annotation.
4. Watch it converge — `Phase` returns to `Running` and `status.observedVersion` shows the
   new version:

```bash
kubectl apply -f platform.yaml
kubectl get platform -n ilm -w
# NAME   PHASE         VERSION   READY   AGE
# ilm    Progressing   2.18.0    False   …
# ilm    Running       2.18.0    True    …
```

The stateless tier is genuinely stateless — all platform state lives in PostgreSQL and the
broker — so a stateless-only version bump (new application images, same major infra
versions) is an ordinary rolling update.

## Worked example: 2.17.0 → 2.18.0

A single operator build carries **both** bundles, so this upgrade is a `spec.version` change
only — **no operator upgrade required**. Starting from
[`config/samples/platform_2170.yaml`](../config/samples/platform_2170.yaml):

```yaml
spec:
  version: "2.18.0"     # was "2.17.0"
  # optional — adopt the 2.18.0 provisioning service (see "what you must decide" below):
  provisioning:
    mode: deploy
    deploy: { bootstrapSecretRef: ilm-provisioning-bootstrap }
```

```bash
# (only if adopting provisioning) create its bootstrap Secret first — by reference, never inlined:
kubectl create secret generic ilm-provisioning-bootstrap -n ilm \
  --from-literal=securityApiKey=<api-key> \
  --from-literal=tokenSigningKey=<a-32+char-signing-key>
kubectl apply -f platform.yaml
kubectl get platform ilm -n ilm -w   # observedVersion 2.17.0 → 2.18.0, READY True
```

### What the operator does for you (automatic)

| Change | How it is handled |
|---|---|
| **Broker env rename** `RABBITMQ_*` → `BROKER_*` (+ new `BROKER_VIRTUAL_HOST`) | The 2.18.0 wiring is applied on re-render; Core's Deployment gets the new env and rolls. No user action. |
| **Application images** `core:2.17.0`→`2.18.0`, `scheduler:1.0.5`→`1.1.0`, `frontend-administrator:2.17.0`→`2.18.0` | Rolled out by the Deployment update. (Every other component tag is identical between the two releases.) |
| **Managed RabbitMQ topology** single user → five-user (administrator/provisioner/proxy/core/monitor) + the `czertainly-proxy` exchange + the `time-quality.*` queues/bindings | The Messaging Topology Operator reconciles **additively**: the four new users, the proxy exchange, and the time-quality objects are created; Core keeps using the `core` user. |
| **Database schema** | Core 2.18.0 self-migrates on boot (Flyway). The operator orders the rollout and gates readiness; it never runs SQL itself. |

### What YOU must decide / do

- **Provisioning (new in 2.18.0)** — to use the bundled provisioning-rabbitmq service, set
  `spec.provisioning.mode: deploy` and supply its `bootstrapSecretRef`. On 2.17.0 a
  `provisioning` block is ignored (the component did not exist); after the upgrade it renders.
- **Back up PostgreSQL first** — the Core schema migration is one-way (see the limitation
  below). Verify a restorable backup before applying.

### Implications and limitations

- **The upgrade is one-way — downgrades are REFUSED.** Once `status.observedVersion` is
  `2.18.0`, setting `spec.version` back to `2.17.0` (or any older version) makes the platform
  `Degraded` with reason **`DowngradeForbidden`** and applies nothing; the running 2.18.0 keeps
  serving. A stateful platform that has migrated its schema cannot be rolled back in place —
  recover from a backup if you must return to 2.17.0.
- **Brief unavailability during the rollout** — Core and the frontend roll like any Deployment
  update; with `replicas: 1` (the sample) there is a short gap. Use the HA profile (Core/gateway
  ≥ 2 replicas + PDBs) for a zero-downtime rollout.
- **The single→five-user topology change is additive, not destructive** — the 2.17.0 `core`
  user and its queues are preserved; the new provisioner/proxy/monitor users are created idle
  and are used only once you enable provisioning.
- **The CZERTAINLY→ILM rebrand is transparent at the operator boundary** — both releases'
  images are republished under `hub.omnitrustregistry.com/ilm/*`, so image names do not change
  for you. Some app-internal names persist (the `com.czertainly` log package, the `czertainlydb`
  default DB name) and are cosmetic.

## Managed-infrastructure major-version upgrades (the guard)

A **MAJOR** version bump of an **already-running, operator-managed** dependency is a
different class of operation. It is one-way, data-affecting, and carries upstream
prerequisites:

- **PostgreSQL** (CloudNativePG) — a major bump runs a one-way data migration.
- **RabbitMQ** — e.g. 3.x → 4.x requires feature flags enabled and all queues migrated to
  quorum queues *first*.
- **Keycloak** — a major bump runs a realm/database migration.

So the operator does **not** blindly pass a major increase of a *running* managed cluster's
version straight through to the upstream operator. It **guards** the change.

### What the guard does

When you bump a managed block's `version` (e.g. `database.managed.version`,
`messaging.managed.version`, `keycloak.managed.version`) across a **major** boundary on a
cluster that is **already running**, and you have **not** acknowledged the upgrade, the
operator:

1. **Holds the cluster at its current version** — it re-pins the currently-running engine
   image onto the rendered upstream CR so the apply does **not** bump the engine. The rest of
   the platform keeps converging on the healthy running version.
2. Sets a **`<Infra>UpgradeBlocked` condition** — one per managed dependency:
   **`DatabaseUpgradeBlocked`**, **`MessagingUpgradeBlocked`**, or
   **`KeycloakUpgradeBlocked`** — to `True` with reason **`MajorUpgradeNeedsAck`**.
3. Records a **`Warning` Event** with an actionable message naming the version move, the
   field to set, and the upstream operator whose upgrade prerequisites to review.
4. **Requeues** so the change applies as soon as you acknowledge it.

The condition is **adjunct** — exactly like `DatabaseReady` / `MessagingReady` /
`KeycloakReady`. It **never** flips the whole `Platform` to `Degraded`; `Available` and the
phase are unaffected while the cluster stays on its current version.

```bash
kubectl describe platform ilm -n ilm
# Conditions:
#   Type                     Status  Reason               Message
#   DatabaseUpgradeBlocked   True    MajorUpgradeNeedsAck  major upgrade 16→17 requires
#       spec.database.managed.upgradeAcknowledged=true; review CloudNativePG upgrade
#       prerequisites before acknowledging
```

### What is NOT guarded

- **First creation** — a brand-new managed cluster (no running version yet) applies the
  requested version freely; there is nothing to migrate.
- **Patch / minor changes** — a same-major change (e.g. `16.3` → `16.4`, or `4.0` → `4.1`)
  applies freely.
- **A version the operator omitted** — if the managed block left `version` empty *and* the
  operator did not pin an engine image (so the upstream operator's own default is running),
  the running version is unknown-to-the-operator and is not treated as a detectable major
  jump.

Only a strictly-greater **major** of a readable running version is gated.

### Acknowledging the upgrade — the safe sequence

1. **Review the upstream operator's major-upgrade prerequisites** for the dependency you are
   bumping (PostgreSQL/CNPG major upgrade notes; the RabbitMQ 3.x→4.x feature-flag +
   quorum-queue migration; the Keycloak major-upgrade migration). The `Warning` Event names
   the upstream operator to check.
2. **Take a backup** of the data (the operator does not do this for you; for a managed
   PostgreSQL cluster configure CNPG backups, and verify a restorable backup exists before a
   major bump).
3. **Set the acknowledgement** on that managed block and re-apply:

   ```yaml
   spec:
     database:
       managed:
         version: "17"               # the new major
         upgradeAcknowledged: true   # opt in to the major upgrade of the RUNNING cluster
   ```

4. The operator now passes the new major through to the upstream operator, which performs the
   upgrade/migration, and clears the `<Infra>UpgradeBlocked` condition.
5. **Reset `upgradeAcknowledged` to `false`** after the upgrade completes, so a *future*
   accidental major bump is guarded again. (The flag is an explicit one-time opt-in, not a
   permanent setting.)

### Per-dependency acknowledgement fields

| Managed dependency | Version field | Acknowledgement field | Condition |
|---|---|---|---|
| PostgreSQL (CloudNativePG) | `spec.database.managed.version` | `spec.database.managed.upgradeAcknowledged` | `DatabaseUpgradeBlocked` |
| RabbitMQ | `spec.messaging.managed.version` | `spec.messaging.managed.upgradeAcknowledged` | `MessagingUpgradeBlocked` |
| Keycloak | `spec.keycloak.managed.version` | `spec.keycloak.managed.upgradeAcknowledged` | `KeycloakUpgradeBlocked` |

## Deletion and downgrades

- **Platform-version downgrades are REFUSED.** Setting `spec.version` to a version older than
  the running `status.observedVersion` makes the platform `Degraded` with reason
  `DowngradeForbidden` and applies nothing — a stateful platform that has migrated its schema
  cannot be rolled back in place (recover from a backup to return to an older version).
- **Managed-engine major downgrades** are likewise not performed by the guard (it gates only a
  strictly-greater major); a downgrade is its own data operation and is out of scope.
- **Deletion safety is independent of upgrades.** `spec.deletionPolicy` (default `Retain`)
  governs what happens to managed infrastructure when the `Platform` is deleted — see the
  [getting-started guide](platform.md#6-tear-down). Retained managed clusters keep their data.

## Notes

- All upgrade-related conditions, events, and logs carry only the two version strings and the
  remedy field path — never a secret value or a connection coordinate.
- Connectors (the `Connector` CRD) version independently from the platform, via each
  connector's own `spec.image`.
- Every Platform option (including the managed-block `version`/`upgradeAcknowledged` fields)
  is documented in the configuration reference & scenario cookbook —
  [`configuration.md`](configuration.md).
