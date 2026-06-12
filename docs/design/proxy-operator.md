# ILM Proxy CRD — Design Specification

> Status: **accepted; operator-side implementation complete on this branch**
> (`api/v1alpha1/proxy_types.go`,
> `internal/builder/proxy`, `internal/controller/proxy`). Platform-side items (the
> `manifest` installation format, refresh semantics, token TTL policy) remain open —
> see "Required platform-side changes". The design follows the per-kind pattern
> established by `Connector` and `Platform` (see `CLAUDE.md`, "How to add a new CRD").
> Validated against the local checkouts of the proxy (`CZERTAINLY/Proxy`), Core,
> interfaces, and fe-administrator repositories as of 2026-06-12.

## Overview

The **ILM proxy** (today at github.com/CZERTAINLY/Proxy; OmniTrustILM is the planned
post-rebrand home) is an optional, standalone component deployed in restricted network
zones — typically an internal data center that the ILM platform (often running in a
cloud) cannot reach inbound. The proxy bridges the platform's message broker (RabbitMQ
or Azure Service Bus, AMQP 1.0) to connectors running behind it: it consumes
connector-request messages from its dedicated queues, calls the connector's REST API
over local HTTP, and publishes responses back. All connectivity is **outbound-only**
from the restricted zone (`amqp://`, `amqps://`, or `wss://` on 443 for strict egress,
with `HTTPS_PROXY` support; note `wss://` is Azure-Service-Bus-only in the proxy
today).

This spec adds a `Proxy` CRD (group `otilm.com`, `v1alpha1`) reconciled by the same
operator binary as `Connector` and `Platform`. The operator instance that reconciles a
`Proxy` CR runs **on the target (restricted-zone) cluster** — the same operator that
manages the `Connector` CRs living behind that proxy. The platform-cluster operator has
no role in proxy deployment: the proxy simply does not run there.

```
Platform cluster                                  Restricted-zone (target) cluster
┌──────────────────────────────────┐              ┌─────────────────────────────────────┐
│ ILM Core ──REST──► provisioning  │              │ ILM operator                        │
│   │        (dedicated queues +   │              │   ├─ reconciles Proxy CR ──► proxy pod
│   │         per-proxy credential,│              │   └─ reconciles Connector CRs ──► connector pods
│   ▼         mints config token)  │              │                                  ▲  │
│ UI renders manifest              │              │        proxy pod ── local HTTP ──┘  │
│ (show-once Secret + Proxy CR)    │              │            │                        │
└──────────────────────────────────┘              └────────────┼────────────────────────┘
        │                                                      │ outbound only (amqps/wss)
        │            user downloads, kubectl-applies           ▼
        └───────────────────────────────────────────►  message broker
                                                       (platform-managed RabbitMQ,
                                                        or external, e.g. Azure Service Bus)
```

**The steady-state flow is strictly one-way.** Core + provisioning prepare everything
the proxy needs (queues, credential, config token, rendered manifest); the `Proxy` CR
is a pure consumer of what was delivered. The Proxy reconciler's reconciliation loop
never calls the platform — no registration client (the `Connector` reconciler's
registration POST has no proxy equivalent), no platform API, no polling, no shared
state. The proxy *binary* is what connects to the broker; the operator only runs it
and reports on it. (The only contemplated exception is the future enrollment-token
exchange — a deliberate, one-shot outbound bootstrap call, after which reconciliation
remains platform-free; see Security model §6.)

## Design principles

1. **The proxy's config contract is the interface — and the contract is the config
   token.** The proxy boots from a single JWT (`PROXY_CONFIG_TOKEN`) whose `config`
   claim carries its entire configuration — broker URL, queue coordinates,
   credentials, tuning — optionally HMAC-signed and verified via
   `PROXY_TOKEN_SIGNING_KEY` (Proxy repo, `internal/config/token.go`, `loader.go`).
   The loader *selects one config source* (token parameter > `PROXY_CONFIG_TOKEN`
   env > config file); **in token mode, configuration comes exclusively from the
   token — `PROXY_*` viper env overrides are not layered on top** (verified:
   `LoadConfigFromToken` builds its viper without `AutomaticEnv`). The provisioning
   service mints this token; the CRD passes it through. The operator never re-models
   proxy configuration in Go, and the proxy binary requires no changes for operator
   support.
2. **Thin CRD, shared building blocks.** The spec reuses `common_types.go`
   (`ImageSpec`, `EnvVar`, `SecretRef`/`ConfigMapRef` + key mappings, `VolumeSpec`,
   `SecurityContextSpec`, `ProbeSpec`, `PDBSpec`; metrics uses a proxy-local block —
   see CRD design) and the CRD-agnostic builders in `internal/builder/common/` (the
   Component render model). The *configuration* surface stays token-only; the
   *deployment-shape* surface matches `Connector` (refs, volumes, scheduling).
3. **One-way dependency in steady state.** Platform-side lifecycle (queues,
   credential, token minting, revocation) is owned by Core + provisioning.
   Cluster-side lifecycle (workload, rollout, status) is owned by the operator.
   Reconciliation never crosses the boundary.
4. **No secrets in CRs.** The config token embeds broker credentials, so **the token
   is itself a secret** and gets the full secret discipline: `Secret` reference only,
   consumed via `secretKeyRef`, never inline, never in status/conditions/events/logs —
   matching the `Platform` invariant. The operator may parse the token's *expiry
   claim* for diagnostics but never logs or surfaces its config claims.
5. **No new install artifact.** The existing `ilm-operator` Helm chart (or OLM bundle)
   installs the controller; a `Proxy` CR deploys a proxy. A bootstrap/umbrella chart
   was considered and rejected (see "Alternatives considered").

## What already exists (platform side)

Unlike the first draft of this spec assumed, the platform side is largely
**implemented today** in Core, interfaces, and the admin UI:

- **Proxy domain in Core** — `Proxy` entity (name, description, auto-generated
  `code`, status, `lastActivity`), REST API `GET/POST/PUT/DELETE /v1/proxies`, and
  `GET /v1/proxies/{uuid}/instructions`. Status lifecycle: `INITIALIZED` →
  `PROVISIONING` → `WAITING_FOR_INSTALLATION` → `CONNECTED` (set by a
  `HealthCheckHandler` when the proxy's `health.check` message arrives over the
  broker). Proxy deletion is blocked while connectors reference it, and triggers a
  decommission call that removes the broker topology.
- **Provisioning client** — Core calls the provisioning service
  (`POST /api/v1/proxies`, `DELETE /api/v1/proxies/{code}`, and
  `GET /api/v1/proxies/{code}/installation?format=…`, `X-API-Key` auth). The
  **`format` parameter is the extension point this design rides on**: today it
  renders a shell/Helm command; the operator path adds a `manifest` format.
- **Admin UI** — proxy list/detail/create screens exist, including an installation
  instructions widget ("Run the Helm command below in the customer Kubernetes
  cluster") with copy-to-clipboard and refresh.
- **Connector-behind-proxy** — Core already auto-registers connectors that announce
  themselves through the proxy channel (`ConnectorRegistrationHandler`,
  `connector.proxy_uuid`), which grounds the Connector-CRD future work below.

### Required platform-side changes (small, additive)

1. **A `manifest` installation format** in the provisioning service: render
   `Namespace + Secret(config token) + Proxy CR` (format below) instead of a Helm
   command. Core and the UI pass the format through unchanged.
2. **A delivery-semantics decision** (open question, owned by the platform team):
   today the instructions endpoint is *re-fetchable* (UI refresh button). The agreed
   intent is show-once with nothing stored in Core. Since provisioning holds the
   signing key it can mint a fresh token on every fetch without storing anything — but
   a re-mint alone does **not** invalidate previously issued tokens (they embed the
   same broker credential). True show-once/rotate semantics therefore require
   re-fetch to also rotate the underlying broker credential ("refresh = rotation"),
   or the endpoint to become genuinely one-shot. Either is compatible with this spec;
   the lifecycle table below assumes refresh-rotates.
3. **A token-TTL policy.** The proxy validates token expiry at every boot
   (`ErrTokenExpired`) — an expired config token plus a pod restart is a crash loop.
   The platform must choose: long-lived/non-expiring config tokens (revocation
   happens at the broker, the credential being the revocable artifact), or short TTLs
   with an automated refresh path (future enrollment flow). Until then, rendered
   tokens should be long-lived.

## Credential delivery (the platform ↔ user ↔ cluster seam)

### What the platform prepares

When a user creates a proxy in the UI, Core calls provisioning, which creates the
**dedicated broker topology for this proxy** and a **credential scoped to exactly this
proxy's queue pair — consume on its request queue, publish on its response queue, and
nothing else**. Provisioning then mints the config token embedding that credential
plus all connection config. Properties:

- **Least privilege by construction** — blast radius of a leaked token is one proxy's
  queue pair.
- **Individually revocable** — deleting the proxy in the UI decommissions the
  topology and credential; rotating resets only this proxy.
- **Not stored in Core.** Core holds proxy metadata (name, code, status,
  lastActivity) — never the credential or token. The token exists in plaintext only
  in the rendered manifest in flight and in the target cluster's `Secret`; the broker
  holds a hash of the credential.

### What the user receives

The UI's installation instructions, at most three steps, valid on an empty cluster:

1. *(only if the target cluster does not yet run the ILM operator — no secrets in
   this command:)*

   ```bash
   helm install ilm-operator oci://<registry>/charts/ilm-operator \
     -n ilm-operator --create-namespace
   ```

2. A **"Download manifest" button** — not an on-screen copy block. The download is
   gated by the authenticated UI session and treated as show-once (see
   delivery-semantics decision above): *"this file contains the proxy's credentials;
   a holder can consume this proxy's queues."*

   ```bash
   kubectl apply -f proxy-<name>.yaml && rm proxy-<name>.yaml
   ```

   Download-then-apply (rather than copy-paste, `--set token=...`, or
   `--from-literal`) keeps the token out of shell history and terminal scrollback —
   the exposure Datadog's install docs warn about explicitly.

3. Verification, with closure on both sides:

   ```bash
   kubectl get proxy <name> -n <namespace> -w     # Phase → Running
   ```

   while the UI proxy detail page flips to **Connected** when the proxy's
   `health.check` arrives over the broker — the platform-side view is the
   authoritative confirmation.

### Manifest format (rendered by provisioning, consumed by the operator)

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: ilm-edge
---
apiVersion: v1
kind: Secret
metadata:
  name: proxy-dc-east-config
  namespace: ilm-edge
stringData:
  configToken: <JWT minted by provisioning — full proxy config incl. broker credential>
  tokenSigningKey: <HMAC key, present only when the platform signs tokens>
---
apiVersion: otilm.com/v1alpha1
kind: Proxy
metadata:
  name: dc-east
  namespace: ilm-edge
spec:
  configTokenSecretRef:
    name: proxy-dc-east-config
  # image omitted → operator resolves it from the BOM
  metrics:
    enabled: true
```

The Secret key names (`configToken`, `tokenSigningKey`) deliberately match the
existing proxy Helm chart's secret template, so the chart and CR paths share one
contract. Only the `Secret` is sensitive: the `Proxy` CR half is metadata Core can
re-render at any time.

### Lifecycle mapping (UI action → artifact → cluster effect)

| UI action | Platform side | Artifact rendered | Cluster side (operator) |
|---|---|---|---|
| Create proxy | Queues + credential created; token minted | **Full manifest** (Namespace + Secret + CR) | User applies; operator deploys; proxy connects; Core → `CONNECTED` |
| Rotate / refresh instructions | Credential rotated, **old one atomically invalidated**; fresh token minted | **Secret-only manifest** (CR already applied, unchanged) | User applies the new Secret; Secret-watch → checksum → rollout restarts the proxy — no further instructions |
| Lost manifest before apply | = Rotate | **Full manifest** re-rendered (re-renderable CR + fresh Secret) | User applies as in Create |
| Delete proxy (UI-first) | Topology + credential decommissioned (blocked while connectors reference the proxy) | — | Proxy loses broker auth; pods go unready; `Degraded` with an actionable reason; user deletes the CR at leisure |
| Delete CR first (cluster-first) | Queues idle; UI shows the proxy disconnected. **The credential remains valid until the proxy is deleted in the UI — UI deletion is the revocation step** | — | Workload removed via ownerRef GC. The user-applied Secret has no ownerRef and is **not** garbage-collected — the instructions must say to delete it explicitly |

Rotation is **intentionally disruptive**: between invalidation and applying the new
Secret the proxy is disconnected and status degrades — visibly, on both sides.
GitLab-style dual-credential zero-downtime rotation was considered and deferred;
atomic invalidation is the simpler, safer default, and the rotate-must-invalidate
rule also guarantees failed or abandoned downloads never leave orphaned-but-valid
credentials behind.

## CRD design

### API group and versioning

Group `otilm.com`, version `v1alpha1`, kind `Proxy` (plural `proxies`). Namespaced.
Multiple `Proxy` CRs per namespace/cluster are allowed (unlike the per-namespace
`Platform` singleton) — each proxy has its own queues and credential.

### Spec (sketch)

```go
// ProxySpec deploys one ILM proxy instance from a provisioning-issued config token.
type ProxySpec struct {
    // ConfigTokenSecretRef names the Secret holding the proxy's config token (and,
    // when token signing is enabled, the signing key). The operator injects the keys
    // as PROXY_CONFIG_TOKEN / PROXY_TOKEN_SIGNING_KEY via secretKeyRef (the signing
    // key as an optional reference). All broker configuration travels inside the
    // token; the CRD does not model it.
    ConfigTokenSecretRef ConfigTokenRef `json:"configTokenSecretRef"`

    // Image optionally overrides the BOM-resolved proxy image.
    Image *ImageSpec `json:"image,omitempty"`

    Replicas  *int32                       `json:"replicas,omitempty"`
    Resources *corev1.ResourceRequirements `json:"resources,omitempty"`

    // Env sets process-level environment on the proxy container — e.g.
    // HTTPS_PROXY/HTTP_PROXY/NO_PROXY for corporate egress, which are cluster-side
    // concerns the token cannot know (read by Go's http.ProxyFromEnvironment for
    // wss:// transport). It is NOT a proxy-configuration channel: in token mode the
    // proxy ignores PROXY_* viper env overrides entirely (design principle 1). The
    // shared EnvVar type is name/value only — never put secrets here.
    Env []EnvVar `json:"env,omitempty"`

    // SecretRefs/ConfigMapRefs/Volumes are DEPLOYMENT-SHAPE knobs (the Connector
    // pattern): env or mounted volumes by reference — e.g. a private-PKI CA bundle
    // for broker TLS, which cannot travel inside the token. They are not a proxy-
    // configuration channel either; the operator-reserved env names
    // (PROXY_CONFIG_TOKEN, PROXY_TOKEN_SIGNING_KEY) are filtered from every
    // projection so a ref can never redirect the token wiring. Referenced objects
    // join the config checksum, so rotating a CA bundle rolls the proxy.
    SecretRefs    []SecretRef    `json:"secretRefs,omitempty"`
    ConfigMapRefs []ConfigMapRef `json:"configMapRefs,omitempty"`
    Volumes       []VolumeSpec   `json:"volumes,omitempty"`

    // SecurityContext tunes readOnlyRootFilesystem only; the SCC-critical fields are
    // hardened by the shared builders and cannot be weakened from the CR.
    SecurityContext *SecurityContextSpec `json:"securityContext,omitempty"`

    // Scheduling and shutdown knobs for restricted zones (egress-pinned nodes,
    // in-flight message drain).
    TerminationGracePeriodSeconds *int64            `json:"terminationGracePeriodSeconds,omitempty"`
    NodeSelector                  map[string]string `json:"nodeSelector,omitempty"`
    Tolerations                   []corev1.Toleration `json:"tolerations,omitempty"`

    // Extra containers and workload identity (the Connector/ComponentSpec pattern).
    // Sidecars/init containers are SCC-hardened by the shared builders — a CR cannot
    // weaken restricted-v2 pod security; schemas are embedded opaquely to keep the
    // CRD compact. ServiceAccount binds a pre-created workload-identity SA and/or
    // stamps annotations (IRSA, GCP/Azure workload identity).
    InitContainers []corev1.Container   `json:"initContainers,omitempty"`
    Sidecars       []corev1.Container   `json:"sidecars,omitempty"`
    Affinity       *corev1.Affinity     `json:"affinity,omitempty"`
    ServiceAccount *ServiceAccountSpec  `json:"serviceAccount,omitempty"`

    // Shared blocks (common_types.go). Probes default to the proxy's /health, /ready,
    // and startup on :8080. Metrics uses a proxy-local block (not the shared
    // MetricsSpec) so the scrape path defaults to the proxy's /metrics — see the
    // metrics note below.
    Probes  *ProbeSpec        `json:"probes,omitempty"`
    Metrics *ProxyMetricsSpec `json:"metrics,omitempty"`
    PodDisruptionBudget *PDBSpec      `json:"podDisruptionBudget,omitempty"`
    PodAnnotations map[string]string `json:"podAnnotations,omitempty"`
    PodLabels      map[string]string `json:"podLabels,omitempty"`
}

// ConfigTokenRef locates the config token inside a Secret. Key defaults match the
// proxy Helm chart's secret template so both install paths share one contract.
type ConfigTokenRef struct {
    // Name of the Secret in the Proxy's namespace.
    Name string `json:"name"`
    // TokenKey is the Secret key holding the JWT. Defaults to "configToken".
    TokenKey string `json:"tokenKey,omitempty"`
    // SigningKeyKey is the Secret key holding the HMAC signing key, injected as an
    // optional secretKeyRef. Defaults to "tokenSigningKey".
    SigningKeyKey string `json:"signingKeyKey,omitempty"`
}
```

Schema-level guards are minimal because the token carries the configuration:
`configTokenSecretRef.name` is required; everything else is optional. Inline
credentials are rejected *by construction* — the schema has no username/password/URL
fields to misuse. (The proxy binary's own validation — e.g. `amqp.subscription`
required, `wss://` only for Azure Service Bus — applies to what provisioning puts in
the token, which is the right layer for it.)

Metrics note (**decided**): the proxy serves metrics at `/metrics` and there are no
plans to change that, while the shared `MetricsSpec` carries a kubebuilder default of
`/v1/metrics` on `path` — applied at admission, so the builder cannot distinguish a
defaulted value from user intent. The CRD therefore uses a proxy-local
`ProxyMetricsSpec` (the shared type's fields minus the reserved `port`) whose `path`
defaults to `/metrics`; the manifest does not need to set it.

### Status

```go
type ProxyStatus struct {
    Conditions         []metav1.Condition `json:"conditions,omitempty"`
    Phase              ProxyPhase         `json:"phase,omitempty"` // Pending|Deploying|Running|Failed|Updating|ScaledDown
    ObservedGeneration int64              `json:"observedGeneration,omitempty"`
    ReadyReplicas      int32              `json:"readyReplicas,omitempty"`
    ObservedVersion    string             `json:"observedVersion,omitempty"` // resolved image tag (BOM or override)
    ConfigChecksum     string             `json:"configChecksum,omitempty"`  // checksum of the referenced config Secret(s)/ConfigMap(s)
}
```

A deliberate `spec.replicas: 0` settles in phase `ScaledDown` with `Available: False`
and `Progressing: False` (reason `ScaledToZero`) — a pause, not a failure, and not a
requeue loop.

Conditions reuse the established `Connector` vocabulary (each with
`observedGeneration`, via `meta.SetStatusCondition`):

- `Available` — the Deployment is available. Because the proxy's readiness probe
  (`/ready`) returns 200 **only while the AMQP connection is active** (verified in
  the Proxy repo's HTTP server), `Available: True` also means broker-connected; on
  credential revocation or broker outage the pods go unready and `Available`
  degrades. The operator gets broker-connectivity signal for free, without ever
  touching the broker.
- `Progressing` — rollout in flight.
- `Degraded` — deterministic failures: a referenced Secret or ConfigMap missing
  (`MissingSecret`/`MissingConfigMap` — covers `configTokenSecretRef`, `secretRefs`,
  and `configMapRefs`), the token key absent (`MissingTokenKey`), the token expired
  (`ConfigTokenExpired`), or the Deployment's pods failing (`ReplicaFailure`). A
  Failed proxy also reports `Available: False`. For expiry the operator may parse
  the JWT's registered claims (`exp`) to set reason `ConfigTokenExpired` proactively
  instead of leaving the user to diagnose a crash loop — it reads the expiry only and
  never logs or surfaces the config claims.
- `ServiceMonitorReady` — adjunct condition for the capability gate (below), per the
  Platform convention: never flips overall readiness, actionable
  `...NotInstalled` reason, requeue to self-heal.

Conditions and events carry names, phases, reasons, and checksums — **never** secret
values or connection coordinates (host/port/URI), matching the `Platform` invariant.

Printer columns: `Phase`, `Version` (observedVersion), `Age`.

### Version resolution (BOM)

The proxy image joins the BOM (`internal/bom`) as a per-component coordinate. With no
`Platform` CR on the restricted-zone cluster to anchor `spec.version`, the rule is:
**resolve from the operator's active default bundle**, record the result in
`status.observedVersion`. Operator upgrades may therefore roll the proxy image to the
newer tested version — intentional for a stateless, protocol-compatible component,
and `spec.image` is the explicit pin for users who need to hold a version.
(A `spec.version` selector with pin-on-create semantics, as in
`platform-versioning.md`, was considered and deferred as YAGNI for a stateless
workload; revisit if proxy/platform version skew ever becomes breaking.)

## Architecture

### Controller

`internal/controller/proxy/` — a thin reconciler following the `Connector` shape:

- `For(&Proxy{})`, `Owns(Deployment, Service, ServiceAccount, PDB)`,
  `Watches(Secret/ConfigMap)` mapped through references for config-drift detection.
  **ServiceMonitor is deliberately not in `Owns`**: a controller-runtime watch on a
  GVK whose CRD is not served fails its ListWatch and stalls the manager's cache sync
  (the documented reason Platform omits it too). The ServiceMonitor stays unwatched;
  drift repair comes from periodic/triggered reconciles.
- Apply strategy: `controllerutil.CreateOrUpdate` — the Proxy is a simple
  single-owner case (the `Connector` precedent); no co-owned resources, no HPA.
- Config drift/rotation: the reconciler computes Secret/ConfigMap checksums via
  `internal/checksum` and the proxy builder stamps them as the pod-template
  annotation `otilm.com/config-checksum` (the `Connector` annotation); a changed
  config token rolls the Deployment.
- **Finalizer-first, like both siblings** (`otilm.com/finalizer`): the finalizer
  exists for the deletion event and as a cleanup extension point — not because
  anything is deregistered (the managed-proxies count is computed from the informer
  cache at scrape time, `internal/monitoring/managed_collector.go`, so deletion
  needs no gauge bookkeeping). There are **no
  platform calls on deletion**: the platform notices a deleted proxy by its queues
  going idle, and broker topology lifecycle is owned by the platform UI. Children are
  cleaned by ownerRef GC (the user-applied Secret is not owned and not touched).
- Capability gate: `ServiceMonitor` only, via `internal/platform/capabilities` —
  skip + adjunct `ServiceMonitorReady: False` + requeue when the CRD is absent. This
  follows the Platform controller's `gateServiceMonitors` precedent (and improves on
  Connector's current log-and-skip handling). No other upstream CRDs are involved.

### Builders

`internal/builder/proxy/` — pure functions composing `internal/builder/common/`:
Deployment (SCC-clean pod security: `runAsNonRoot`, no hard-coded `runAsUser`, drop
all capabilities, `seccompProfile: RuntimeDefault`), Service (two ports: `http` 8080
for health/metrics, `api` 8081 — the connector-facing registration endpoint),
ServiceAccount, optional PDB and ServiceMonitor. The config surface the
builders render is deliberately tiny:

| Source | Env |
|---|---|
| `configTokenSecretRef` (`tokenKey`) | `PROXY_CONFIG_TOKEN` via `secretKeyRef` |
| `configTokenSecretRef` (`signingKeyKey`) | `PROXY_TOKEN_SIGNING_KEY` via optional `secretKeyRef` |
| `spec.env` | pass-through for process-level vars (`HTTPS_PROXY` etc.) — not proxy configuration; the reserved names (`PROXY_CONFIG_TOKEN`, `PROXY_TOKEN_SIGNING_KEY`) are filtered here too |

Env names confirmed against the proxy's config loader
(`internal/config/loader.go:14-15`). The proxy's `PROXY_*` viper overrides
(`SetEnvPrefix("PROXY")` + `.`→`_`) apply only in file/env mode, which the CRD does
not use — configuration travels exclusively in the token.

### Reconciliation flow

1. Fetch `Proxy`; ensure finalizer; on deletion: emit the Deleting event, remove
   the finalizer (children cleaned by ownerRef GC).
2. Resolve image: `spec.image` override, else the operator's active BOM bundle.
3. Read the config-token Secret and every `spec.secretRefs`/`spec.configMapRefs`
   object read-only → compute the combined config checksum (values are hashed for
   drift detection, never copied into rendered objects); optionally parse the token's
   `exp` claim for the `ConfigTokenExpired` diagnostic.
4. Render children via builders; apply (`CreateOrUpdate`); gate the ServiceMonitor.
5. Read Deployment status → set `Available`/`Progressing`/`Degraded`, phase,
   `readyReplicas`, `observedVersion`.
6. Requeue on transient errors; Secret-watch triggers re-reconcile on rotation.

### RBAC

New rules, mirroring the `Connector` set: `proxies` get/list/watch/update/patch
(create/delete of the primary belongs to the user), `proxies/status` get/update/patch,
`proxies/finalizers` update. Children: `deployments`/`services`/`serviceaccounts`
get/list/watch/create/update/patch — no delete verb, ownerRef GC removes them;
`poddisruptionbudgets` and `servicemonitors` additionally carry delete, because the
reconciler explicitly prunes them when disabled. `secrets`/`configmaps` remain
**get/list/watch read-only**.

## Security model

1. **The config token is the secret.** It embeds the broker credential, so it gets
   full secret discipline: `Secret` reference only, `secretKeyRef` injection, never
   inline in the CR, never in status/conditions/events/logs; the operator reads the
   `exp` claim at most and never surfaces config claims.
2. **Per-proxy least privilege** — the credential inside the token is scoped to one
   proxy's queue pair; compromise of one restricted zone never exposes another's
   traffic.
3. **Show-once delivery, nothing stored centrally** — see "Required platform-side
   changes" for the open refresh-semantics decision; in all variants Core stores
   metadata only and rotation is the recovery path (recovery of an old secret is
   impossible by design).
4. **Outbound-only posture** — no inbound connectivity, no Ingress, no LoadBalancer;
   `wss://` on 443 + `HTTPS_PROXY` covers strict egress (Azure Service Bus today;
   RabbitMQ WebSocket is upstream future work). The restricted zone exposes nothing.
5. **SCC-clean pods** — runs under OpenShift `restricted-v2` unmodified, like all
   operator-rendered workloads.
6. **Future hardening — enrollment-token exchange.** The UI manifest carries only a
   short-TTL, single-purpose, individually revocable enrollment token plus a platform
   CA pin; the operator exchanges it over outbound HTTPS for the config token and
   stores it in a Secret it creates. The long-lived credential then never crosses the
   user's browser, clipboard, or filesystem, and the exchange channel doubles as
   automated rotation and token refresh (resolving the token-TTL question). This is
   the consensus pattern of Teleport join tokens, kubeadm bootstrap tokens, and
   Rancher cluster registration (§ Prior-art validation). It is a deliberate,
   one-shot exception to the no-platform-calls rule — steady-state reconciliation
   stays platform-free — and it is explicitly **not** a prerequisite: per-queue
   scoping + show-once + revocation is an accepted industry posture (GitLab agent
   tokens, Grafana Cloud access-policy tokens).

## Install paths

| Posture | How | When |
|---|---|---|
| Operator-managed (recommended) | `ilm-operator` Helm chart or OLM bundle (once per cluster), then apply the UI-rendered manifest | Any cluster that runs — or will run — `Connector` CRs behind the proxy |
| Standalone | The proxy repo's existing Helm chart (`deploy/charts/proxy`), which already accepts the same config token (`.Values.token`) | Clusters where installing CRDs / cluster-scoped RBAC is not possible |

Both paths consume the same provisioning-issued token and the same Secret key names —
one contract, two delivery vehicles. The provisioning `format` parameter selects
which instructions the UI shows (`shell` today, `manifest` for the operator path).
Whether to eventually deprecate the standalone chart is a product decision outside
this spec.

## Alternatives considered

1. **Bootstrap/umbrella chart** (proxy chart installs the operator as a conditional
   subchart + a `Proxy` CR): rejected. Once the operator chart and the CRD exist, the
   wrapper adds no capability — only a one-command UX — while inheriting real costs:
   templated-CRD lifecycle hazards (`helm uninstall` cascading to CR deletion), the
   same-release CRD→CR establishment race, operator-version coupling on shared
   clusters, and a second operator install path to keep coherent.
2. **Proxy managed from the platform cluster**: rejected. The operator is
   cluster-local by design; the proxy never runs on the platform cluster.
   Platform-side proxy concerns (queues, credential, token, revocation) already live
   in Core + provisioning.
3. **Modeling broker configuration in the CRD** (a `broker` block with URL, queue
   names, credentials ref): rejected after discovering the config-token contract.
   The token already carries all of it, provisioning already validates it, and a
   modeled block would be a second source of truth that can disagree with the token.
4. **An explicit-env configuration mode** (optional token + `PROXY_*` env/secretRef
   pass-through for tokenless setups): deferred. In token mode the proxy ignores
   viper env overrides entirely, and the schema requires the token — so such a mode
   would need `configTokenSecretRef` to become optional with a CEL "at least one
   config source" rule. Add it only if a real tokenless use case appears.
4. **Copy-paste credential commands in the UI** (`--set token=...`,
   `--from-literal=...`): rejected — shell-history/scrollback exposure; the
   download-then-apply flow avoids it entirely.

## Testing strategy

Per the repo's standard pyramid: table-driven builder unit tests (target 90%+,
golden-file assertions for the rendered Deployment, the `PROXY_CONFIG_TOKEN` /
optional `PROXY_TOKEN_SIGNING_KEY` wiring, and the checksum annotation), an envtest
controller suite (reconcile, finalizer, Secret-rotation rollout, drift repair,
conditions including `ConfigTokenExpired` and the missing-Secret `Degraded`,
ServiceMonitor capability gate), and the Kind e2e as the pre-release gate only.
`make test` is the inner loop.

## Future work

- **Connector proxy awareness** — `Connector.spec.registration.proxyRef`
  (same-namespace ref to a `Proxy` CR; CEL: exactly one of `platformUrl`|`proxyRef`),
  so a connector behind a proxy registers through the proxy instead of a direct
  `platformUrl` it cannot reach. The full path already exists: the proxy serves
  `POST /v1/connector/register` on its connector-facing API port (`api.port`,
  default 8081 — the Proxy builder exposes it on the Service) accepting the same
  payload the operator already sends to the platform, stamps `proxyCode` itself, and
  forwards a fire-and-forget `connector.register` broker message that Core's
  `ConnectorRegistrationHandler` consumes (`connector.proxy_uuid`). The operator
  reuses its existing registration client with a different base URL/path. Semantics:
  proxy mode is fire-and-forget — `200 OK` means submitted (condition reason
  `RegistrationSubmitted`, once per generation; no platform UUID returned), `503`
  means the proxy is not broker-connected → requeue, which also gives natural
  ordering with no explicit dependency. Strictly additive; the rest of the Connector
  CRD is unchanged; nothing in this spec depends on it.
- **Enrollment-token exchange** — Security model §6; requires a new Core endpoint.
- `values2proxy` converter (mirroring `cmd/values2platform`) if migration from the
  standalone proxy Helm chart is needed at scale.

## Prior-art validation

The credential-delivery design was checked against how established products instruct
users to connect a remote-cluster agent from a central UI:

- **GitLab Agent for Kubernetes** — UI-generated install command with a per-agent,
  revocable token; show-once display with explicit leak-consequence warning; two
  active tokens for zero-downtime rotation (this design deliberately chooses atomic
  invalidation instead; see lifecycle table).
- **Rancher cluster import** — one-line `kubectl apply -f https://…/<token>.yaml`
  server-rendered manifest; agent exchanges the registration token for
  cluster-specific credentials and discards it. (The future enrollment-flow shape.)
- **Teleport join tokens / kubeadm bootstrap tokens** — short-TTL, single-purpose,
  revocable bootstrap credential exchanged for a long-lived identity; CA pin in the
  copied command for mutual verification; public token-id vs. secret split so
  listings never leak the secret.
- **Datadog / Grafana Cloud** — explicit vendor guidance to keep keys out of
  command-line history (existing-Secret references, values-file download), and
  show-once token display. (The delivery-hygiene rules adopted above.)

Consensus properties adopted: the UI generates a complete artifact (no "go create a
secret first"); the credential is purpose-built and scoped, never a user credential
or kubeconfig; connectivity is agent-initiated outbound-only; revocation is
first-class in the UI; show-once display with the consequence of leakage spelled out.

## Open questions

1. **Refresh semantics** (platform team): re-fetch = rotate, or strict one-shot?
   (§ Required platform-side changes, item 2.)
2. **Config-token TTL policy** (platform team): long-lived tokens with broker-side
   revocation, vs. short TTLs once the enrollment/refresh flow exists. (Item 3.)

Both remaining questions are platform-side; neither blocks the operator-side
implementation. (The metrics-path question was resolved: the proxy stays at
`/metrics`, and the CRD uses a proxy-local metrics block — see CRD design.)
