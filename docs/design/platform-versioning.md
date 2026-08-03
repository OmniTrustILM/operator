# Platform versioning, templates & upgrades — design

> Status: **steps 1–3 implemented (committed, green): the 2.17.0 + 2.18.0 bundles are shipped,
> with wiring/feature-gating + the downgrade guard green; remaining: the upgrade + e2e
> version-matrix (steps 4–5)**. Captures the version-management
> architecture for the `Platform` CRD: how the operator wires each supported ILM platform
> version, how the default version is chosen, and how upgrades/downgrades are handled.

## 1. Problem (validated against `helm-charts` 2.17.0 → 2.18.0)

The operator ships a **single** BOM bundle (`pkg/bom/bom.go`) keyed `"2.17.0"` that pins
2.17.0 image tags **but emits the 2.18.0 configuration contract**. Confirmed against the
published charts (`git diff 2.17.0..2.18.0` in `OmniTrustILM/helm-charts`):

| Aspect | 2.17.0 | 2.18.0 | Operator's current wiring |
|--------|--------|--------|---------------------------|
| Core broker env | `RABBITMQ_HOST/PORT/USERNAME/PASSWORD` | `BROKER_HOST/PORT/USERNAME/PASSWORD/VIRTUAL_HOST` | `BROKER_*` → **2.18.0** |
| Core provisioning/proxy env | *(absent)* | `PROVISIONING_API_KEY`, `PROVISIONING_API_URL`, `PROXY_ENABLED`, `PROXY_INSTANCE_ID` | present → **2.18.0** |
| `provisioning-rabbitmq` component | *(does not exist)* | new chart | present → **2.18.0** |
| Umbrella chart / branding | `charts/czertainly`, `czertainlydb`/`czertainlyuser` | `charts/ilm`, `ilmdb`/`ilmuser` | ILM → **2.18.0** |
| Broker user model | single user | multi-user (`provisioner`/`proxy`/`core`) | multi-user → **2.18.0** |

Unchanged across both: `JDBC_URL/USERNAME/PASSWORD`, `OPA_BASE_URL`, `AUTH_SERVICE_BASE_URL`,
`SCHEDULER_BASE_URL`, `HEADER_*`, `HTTP(S)_PROXY`/`NO_PROXY`, `ADMIN_CERT`,
`TRUSTED_CERTIFICATES`, `INTERNAL_OAUTH_SECRET`, `JAVA_OPTS`, `LOGGING_LEVEL_COM_CZERTAINLY`
(the app's internal package stays `com.czertainly` even in 2.18.0).

**Consequence:** deploying 2.17.0 images with the operator's 2.18.0 wiring leaves Core with no
`RABBITMQ_*` env → it never connects to the broker → the Platform never reaches `Available`.
This — not an `auth-db` composition bug — is why the full-managed e2e never converged;
the `auth-db "secret not found"` symptom was downstream churn of a thrashing,
version-mismatched bring-up (the operator's Secret compose is image-independent and works when
images match the wiring, which is why the quickstart works on 2.18.0 images).

## 2. Goals

1. The operator wires **each** supported platform version correctly from per-version **data**
   (env names, component set, topology) — never one version's contract hard-coded for all.
2. The default version is the operator's **newest** (2.18.0).
3. Upgrades are **explicit, ordered, and safe**; downgrades are **refused**; an operator
   upgrade never silently upgrades a running platform.

## 3. Architecture

### 3.1 Existing foundation (keep)

`bom.bundles` is already a **version-keyed map**; each `Bundle` carries that version's
`Components` (image coords), `Wiring` (env contract), `Messaging` (topology), and managed-infra
versions. `BundleFor(spec.version)` selects one (empty → `DefaultVersion`; unknown → degrade
with the supported-versions list); `status.ObservedVersion` records what was reconciled; a
major-version guard already gates **managed-infra** (CNPG/RabbitMQ/Keycloak) engine bumps.

### 3.2 Extensions (new)

1. **`Bundle` gains per-version capability data** so builders render only what a version has:
   - `RegistryDefaults` (the rebrand may publish 2.17.0 under a different registry/repo than 2.18.0).
   - `Features` / component-set flags (e.g. `HasProvisioning`, the broker user model) — the
     builders consult these instead of assuming the 2.18.0 shape.
2. **Wiring becomes genuinely per-version**: the current `wiring2170` is renamed/retagged to
   `wiring2180` (it *is* the 2.18.0 contract); a real `wiring2170` is authored from the 2.17.0
   chart (`RABBITMQ_*` core broker env, no provisioning/proxy env, no `BROKER_VIRTUAL_HOST`).
3. **Version policy (new):**
   - **pin-on-create** — when `spec.version` is empty, the operator follows the version recorded
     on `status.observedVersion` (the version first resolved), not the operator's current
     default, so a platform stays on the version it was created with across operator upgrades.
     The pin lives in **status, never the spec** (GitOps-safe — the operator does not mutate a
     field a GitOps actor owns); the resolved version is applied to an *in-memory* copy of the
     spec so the version-specific builders render the same bundle the reconciler gated on. An
     explicit `spec.version` change is the *only* upgrade trigger.
   - **downgrade = refuse** — the reconciler semver-compares the requested version against
     `status.ObservedVersion`; a lower version sets `Degraded` with an actionable reason and
     applies nothing (stateful downgrade after a schema migration is unsafe). CEL cannot read
     status, so the reconciler is the enforcement point; a validating webhook can add create-time
     UX later.
4. **Migration model (new):**
   - **Config/wiring changes** (env renames, added/removed components) are handled automatically
     by re-rendering against the new bundle → rolling update. No explicit migration.
   - **Data (DB schema)** — ILM Core self-migrates on boot (Flyway/Liquibase). The operator's job
     is **ordering + readiness-gating**, never running SQL.
   - **Breaking transitions** — an optional per-`(fromMajor.minor, toMajor.minor)` migration
     descriptor (ordering, pre/post hooks, `needs-ack`) the reconciler consults. Most transitions
     are "none / self-migrating + ordered".

## 4. Locked decisions (agreed)

- **pin-on-create** for an unset `spec.version`.
- **downgrade = refuse** (reconciler-enforced, semver vs `status.ObservedVersion`).
- **app-self-migrates + operator-orders** for data migrations.

## 5. What each supported version must provide (the per-version contract)

A changelog is the human *input* that drives authoring; the operator needs it **encoded** as
bundle data:

- image coordinates (+ `RegistryDefaults` when the version publishes elsewhere),
- an accurate `WiringProfile` — per-component env names, connection-string templates, Secret keys,
- the **component set + feature flags** for that version,
- the managed messaging topology + validated managed-infra engine versions,
- a migration descriptor for transitions *into* that version.

## 6. Implementation sequence (each step shippable)

1. **Relabel + default.** Rename the current bundle `2.17.0` → `2.18.0`, set its image tags to the
   2.18.0 published tags, `DefaultVersion = "2.18.0"`. Makes the operator honest and the quickstart
   correct. Update samples/docs that imply 2.17.0.
2. **Version policy.** pin-on-create defaulting + downgrade-refuse guard + `status` UX, with
   envtest coverage.
3. **Real 2.17.0 bundle.** Author from the 2.17.0 chart: `RABBITMQ_*` core wiring, no
   provisioning/proxy, 2.17.0 images + registry, 2.17.0 topology/DB naming; feature-gate
   `provisioning`/components in the builders. Builder unit tests assert per-version env output.
4. **Migrations.** Migration-descriptor framework + the 2.17.0→2.18.0 transition + an
   `docs/upgrades.md` procedure (incl. the CZERTAINLY→ILM data/topology notes).
5. **e2e version matrix.** 2.17.0-on-2.17.0, 2.18.0-on-2.18.0, and an upgrade test, plus the fast
   envtest assertions that lock the wiring/feature-gating + downgrade guard (so the inner loop
   catches a version-contract regression, not the Kind gate).

## 6a. The 2.17.0 contract (extracted from `helm-charts` tag 2.17.0, for step 3)

2.17.0 is the pre-rebrand **CZERTAINLY** release; its contract differs structurally from
2.18.0, so its bundle is a full parallel definition:

- **Images** — republished under the **same** registry/repository as 2.18.0
  (`hub.omnitrustregistry.com/ilm/*`, the global default), with the SAME image names and only
  the tags differing: `core:2.17.0`, `scheduler:1.0.5`, `frontend-administrator:2.17.0`, and
  `auth:1.6.3` / `auth-opa-policies:1.4.1` / `opa:1.10.0-static` / `curl:8.16.0` /
  `utils-service:1.0.2` / `kong:3.9.1` (identical to 2.18.0). **No `provisioning` image.**
  (The chart's own defaults are `docker.io/czertainly/czertainly-*`, but the operator pins the
  republished `hub.omnitrustregistry.com/ilm` copies, so NO per-bundle registry default is
  needed.)
- **Core broker env** — `RABBITMQ_HOST/PORT/USERNAME/PASSWORD`; **no** `*_VIRTUAL_HOST` env.
- **Absent in 2.17.0 Core** — `PROVISIONING_API_KEY/URL`, `PROXY_ENABLED`, `PROXY_INSTANCE_ID`
  (no provisioning service, no native proxy wiring).
- **Managed messaging** — a **single** broker user (`messaging.username/password`), not the
  2.18.0 multi-user topology (administrator/provisioner/proxy/core/monitor).
- **DB naming defaults** — `czertainlydb` / `czertainlyuser` (override-able; relevant only as
  the default DB name the connection string composes when unset).
- **Unchanged from 2.18.0** — `JDBC_URL/USERNAME/PASSWORD`, `OPA_BASE_URL`,
  `AUTH_SERVICE_BASE_URL`, `SCHEDULER_BASE_URL`, `HEADER_*`, `HTTP(S)_PROXY`/`NO_PROXY`,
  `ADMIN_CERT`, `TRUSTED_CERTIFICATES`, `INTERNAL_OAUTH_SECRET`, `LOGGING_LEVEL_COM_CZERTAINLY`.

### Step 3 build breakdown
1. ✅ **BOM struct** — feature flag `Bundle.HasProvisioning` (2.18.0 true, 2.17.0 false). No
   per-bundle registry default is needed — both versions share `hub.omnitrustregistry.com/ilm`.
2. ✅ **2.17.0 bundle** — same image NAMES as 2.18.0 with 2.17.0 tags (core:2.17.0,
   scheduler:1.0.5, frontend-administrator:2.17.0; the rest identical), no provisioning entry.
3. ✅ **`wiring2170`** — derived from wiring2180: messaging env → `RABBITMQ_*`,
   `MessagingVHostEnv: ""`, empty `Provisioning`/`ProxyEnabledEnv`/`ProxyInstanceIDEnv`. The
   builder now SKIPS any env whose wiring name is empty (the data-driven env feature gate).
4. ✅ **`messagingTopology2170`** — single `core`-role user (full perms) + the czertainly
   exchange + core.* queues/bindings (no proxy exchange). The managed-messaging builder loops
   over Users and the readback is role-keyed, so the single-user shape needs no builder change;
   only affects `messaging.mode=managed`.
5. ✅ **Provisioning component gate** — `ProvisioningDeploy` now also requires
   `bundle.HasProvisioning`, so 2.17.0 never renders the provisioning workload/env even if
   `provisioning.mode=deploy` is set. (A non-fatal "provisioning unavailable in this version"
   *condition* is a small follow-up; today it is silently skipped + documented.)
6. ✅ **Tests** — builder tests assert 2.17.0 renders `RABBITMQ_*`/no-provisioning/no-proxy +
   core:2.17.0, and that `ProvisioningDeploy` is version-gated; bom tests cover the new bundle.
   (A dedicated 2.17.0 golden snapshot is optional follow-up.)
7. ⏳ **Upgrade + e2e** (steps 4–5) — `config/samples/platform_2170.yaml` + the 2.17.0→2.18.0
   `docs/upgrades.md` worked example (done); an e2e version-matrix (2.17.0-on-2.17.0,
   2.18.0-on-2.18.0, upgrade) remains.

## 7. Open inputs

Resolved — the 2.17.0 and 2.18.0 contracts are captured in §6a (images, Core env, topology shape)
from `helm-charts` tags 2.17.0/2.18.0. Any remaining per-component env nuance (auth,
scheduler, fe-administrator) is read from the 2.17.0 chart as each component's wiring is authored
in step 3.
