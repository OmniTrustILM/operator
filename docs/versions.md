# Platform versions — deploying a specific ILM release

The ILM operator does not pin a single platform version. It carries a **bill-of-materials
(BOM) of tested version bundles** — each bundle pins, for one ILM platform release,
everything that release needs to deploy correctly:

- every component's container image (Core, auth, scheduler,
  fe-administrator, auth-opa-policies, the OPA sidecar, the Kong gateway, utils, and
  the bundled provisioning service);
- that release's **wiring profile** (the env-var names, the connection-string template, and
  the default Secret keys the operator uses to wire the components);
- that release's **managed-RabbitMQ messaging topology** (vhost, users, permissions,
  exchanges, queues, bindings);
- the **managed-infrastructure default versions** (PostgreSQL, RabbitMQ, Keycloak) the
  operator provisions for that release.

## Supported versions

This operator build ships the following tested bundles. The engine columns are the
**managed-infrastructure defaults** the operator provisions when you omit a per-engine
`version` (override per engine as described in [`configuration.md`](./configuration.md)).

| Platform version | PostgreSQL | RabbitMQ | Keycloak |
| ---------------- | ---------- | -------- | -------- |
| **2.18.0** (default) | 18 | 4.3.1 | 26.6.3 |
| 2.17.0 | 16 | 4.2.0 | 26.4.0 |

`2.18.0` is the operator's **newest** bundle, so it is the default selected when
`spec.version` is empty at creation.

You select a bundle with **`spec.version`**.

```yaml
apiVersion: otilm.com/v1alpha1
kind: Platform
metadata: { name: ilm, namespace: ilm }
spec:
  version: "2.18.0"     # a tested bundle; omit/"" pins the operator's NEWEST at creation (no auto-upgrade)
  database:  { mode: managed }
  messaging: { mode: managed }
```

## How a version is resolved

The operator follows a **pin-on-create** policy, so upgrading the *operator* never silently
upgrades a *running platform*:

- **`spec.version` set to a supported version** → that bundle is used. Setting it to a newer
  version is the explicit — and only — way to **upgrade** the platform.
- **`spec.version` empty, first reconcile** → the operator resolves its **NEWEST** shipped
  bundle and records it on `status.observedVersion`, **pinning** it.
- **`spec.version` empty, thereafter** → the operator keeps using the **pinned**
  `status.observedVersion`, *not* whatever a newer operator build defaults to. A platform
  created today stays on today's version even after you upgrade the operator binary; it moves
  only when you set `spec.version` explicitly (see [upgrades](upgrades.md)).
- **`spec.version` older than the running version** → the platform goes **`Degraded`** with
  reason `DowngradeForbidden` and applies nothing. A stateful platform that already migrated
  its schema cannot be rolled back safely; set `spec.version` back to the running version (or
  higher).
- **`spec.version` set to an unknown version** → the platform goes **`Degraded`** with reason
  `UnsupportedVersion` and an actionable message that lists the versions this operator build
  supports (see below). This is a deterministic configuration mistake, not a crash — the
  operator records the condition and stops without busy-looping until you edit the spec.

The version the operator actually reconciled against is reported on
**`status.observedVersion`** and surfaced in the **`Version`** printer column:

```bash
kubectl get platform -n ilm
# NAME   PHASE     VERSION   READY   AGE
# ilm    Running   2.18.0    True    3m
```

`status.observedVersion` always reflects the *resolved* version — the concrete version
string even when `spec.version` is empty (the pinned version: the operator's newest at
creation, held steady thereafter).

## The supported-range model

Because the BOM is a **map keyed by version** (not a single compile-time constant), **one
operator build supports a RANGE of platform versions**. That gives you two useful properties:

- **Canary a version.** Run a newer (or older) platform version in one namespace while the
  rest of your fleet stays put — each `Platform` is a per-namespace singleton pinned to its
  own `spec.version`.
- **Take an operator fix without moving the platform.** Upgrading the operator binary to pick
  up a controller bug-fix does not force a platform version change: keep `spec.version`
  pinned and only the operator changes.

The supported set is intentionally **not** a fixed CRD enum (a CEL `enum` would have to be
regenerated every time a version is added, and would reject a value the running operator
actually supports). Instead the value is validated **at runtime** against the bundles the
running operator carries. The unknown-version `Degraded` message enumerates them, so the
supported list is always discoverable from a live cluster:

```bash
kubectl describe platform ilm -n ilm
# ...
# Phase:  Degraded
# Conditions:
#   Type      Status  Reason              Message
#   Degraded  True    UnsupportedVersion  platform version "9.9.9" is not supported by this
#                                         operator; supported versions: 2.17.0, 2.18.0
```

A `Warning` Event with reason `UnsupportedVersion` is recorded alongside the condition.

## Adding platform versions

The platform version advances by **upgrading the operator** — a newer operator build ships
additional (and newer) bundles in its BOM. After upgrading the operator you can move
`spec.version` to any version the new build supports. See [upgrades.md](upgrades.md) for the
full upgrade procedure, including the major-version guard for managed infrastructure.

## GitOps recommendation

For reproducible deployments, **pin the operator image tag** (do not track a floating
`latest`) **and** set `spec.version` explicitly. That way both halves of the contract — the
operator binary and the platform release it reconciles — are version-controlled, and a
platform upgrade is an explicit, reviewable change to one of those two values.

## Component image overrides still apply

`spec.version` selects the *defaults*. You can still override an individual component's image
on top of the bundle — `spec.common.image` for fleet-wide registry/repository, or a
per-component `spec.<component>.image` (per field: `registry`/`repository`/`name`/`tag`/
`digest`). A `digest` pins the image immutably and wins over `tag`. Overrides layer on top of
the selected bundle; they do not change the resolved `status.observedVersion`.

## See also

- [`configuration.md`](./configuration.md) — the complete configuration reference & scenario
  cookbook (every Platform option, mapped to a matching sample).
