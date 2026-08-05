# Platform, Connector & Proxy samples

Working, copy-pasteable `otilm.com/v1alpha1` examples. Every `platform_*.yaml` here is a
**complete CR that passes apiserver validation** — pick the one closest to your scenario,
read its header comment (purpose / prerequisites / secrets / what it creates), adjust, and
`kubectl apply -f`.

- **New here?** Start with [`platform_minimal_external.yaml`](./platform_minimal_external.yaml)
  (bring-your-own infra; general guide: [`docs/platform.md`](../../docs/platform.md)) or
  [`platform_quickstart.yaml`](./platform_quickstart.yaml) (everything managed, apply-and-go —
  the demo; walkthrough: [`docs/quickstart.md`](../../docs/quickstart.md)).
- **Want every option explained, and "which sample fits which need"?** See
  [`docs/configuration.md`](../../docs/configuration.md) — the configuration reference + scenario cookbook.
- **Want the annotated every-field reference?** See
  [`docs/design/examples/platform-cr-reference.yaml`](../../docs/design/examples/platform-cr-reference.yaml).

## The two required choices

Every Platform sets `database` and `messaging`, each **`external`** (you bring the
connection + a credentials Secret) or **`managed`** (the operator provisions it via an
upstream operator, which *generates* the credentials). `keycloak` is optional and chooses the
same way. Mix them per dependency — see [`platform_mixed_infra.yaml`](./platform_mixed_infra.yaml).

| Dependency set to `managed` | Upstream operator you must install first |
|---|---|
| `database` (and the PgBouncer pooler) | CloudNativePG |
| `messaging` | RabbitMQ Cluster Operator + Messaging Topology Operator |
| `keycloak` | Keycloak Operator |

A missing operator is non-fatal: the platform reports `…Ready=False`/`…NotInstalled` and
self-heals once you install it. `external` mode needs none of these (a cert-managed `edge`
needs cert-manager + an ingress controller regardless). Install commands are in
[`docs/platform.md`](../../docs/platform.md).

## Pick a Platform sample by need

### Getting started
| Sample | Shows |
|---|---|
| [`platform_minimal_external.yaml`](./platform_minimal_external.yaml) | The smallest CR that converges — external DB + broker, no edge (the smoke test). |
| [`platform_quickstart.yaml`](./platform_quickstart.yaml) | Everything managed, `kubectl apply` and go (the demo). |
| [`platform_production.yaml`](./platform_production.yaml) | A realistic **production-tuned** starting point: managed + HA + sized resources/storage + HPA/PDB + edge + admin. |
| [`platform_full.yaml`](./platform_full.yaml) | The **kitchen-sink** of every shipped field with example values (a reference, *not* a recommendation). |

### Infrastructure mode (external vs managed)
| Sample | Shows |
|---|---|
| [`platform_managed_postgres.yaml`](./platform_managed_postgres.yaml) | Managed PostgreSQL via CloudNativePG (messaging external). |
| [`platform_managed_rabbitmq.yaml`](./platform_managed_rabbitmq.yaml) | Managed RabbitMQ via the RabbitMQ operators (DB external). |
| [`platform_managed_keycloak.yaml`](./platform_managed_keycloak.yaml) | Managed Keycloak via the Keycloak Operator (sharing the platform DB). |
| [`platform_mixed_infra.yaml`](./platform_mixed_infra.yaml) | **Mix modes per dependency**: external DB + managed messaging + managed Keycloak. |

### Connection pooling (PgBouncer, managed DB only)
| Sample | Shows |
|---|---|
| [`platform_managed_postgres.yaml`](./platform_managed_postgres.yaml) | Pooler **ON** — the default for a managed DB (the fleet would otherwise exhaust `max_connections`). |
| [`platform_managed_postgres_no_pooler.yaml`](./platform_managed_postgres_no_pooler.yaml) | Pooler **OFF** (`pgBouncer.managed: false`) — direct connection to `…-rw`. |

The switch is `database.pgBouncer.managed`. Full explanation:
[`docs/configuration.md` → Connection pooling](../../docs/configuration.md#connection-pooling-pgbouncer).

### Versions & upgrades
| Sample | Shows |
|---|---|
| [`platform_2170.yaml`](./platform_2170.yaml) | Pin the **platform** bundle (`spec.version`) to ILM 2.17.0. |
| [`platform_2190.yaml`](./platform_2190.yaml) | Pin the **platform** bundle (`spec.version`) to ILM 2.19.0 — a **preview** bundle: opt-in only via an explicit `spec.version`, not yet advertised or eligible as the default. |
| [`platform_managed_pinned_versions.yaml`](./platform_managed_pinned_versions.yaml) | Pin each **managed engine** version (PostgreSQL / RabbitMQ / Keycloak) + the major-upgrade guard. |

See [`docs/versions.md`](../../docs/versions.md) for the supported matrix and
[`docs/upgrades.md`](../../docs/upgrades.md) for upgrade procedures.

### High availability & scale
| Sample | Shows |
|---|---|
| [`platform_high_availability.yaml`](./platform_high_availability.yaml) | The HA profile (one flag) + a per-component HPA and PDB override. |
| [`platform_production.yaml`](./platform_production.yaml) | HA + multi-instance managed infra + sized resources together. |

### Edge & TLS
| Sample | Shows |
|---|---|
| [`platform_edge_issuerref.yaml`](./platform_edge_issuerref.yaml) | Ingress edge signed by an existing cert-manager Issuer/ClusterIssuer. |
| [`platform_edge_letsencrypt.yaml`](./platform_edge_letsencrypt.yaml) | Ingress edge with Let's Encrypt (ACME) TLS. |
| [`platform_edge_byo_secret.yaml`](./platform_edge_byo_secret.yaml) | Ingress edge with a bring-your-own TLS Secret (no cert-manager). |
| [`platform_gatewayapi.yaml`](./platform_gatewayapi.yaml) | Gateway API edge (HTTPRoute) instead of Ingress. |

### First-admin bootstrap
| Sample | Shows |
|---|---|
| [`platform_registeradmin_generated.yaml`](./platform_registeradmin_generated.yaml) | Admin via a cert-manager-**generated** client certificate (mTLS). |
| [`platform_registeradmin_provided.yaml`](./platform_registeradmin_provided.yaml) | Admin via a **provided** client-certificate Secret. |
| [`platform_registeradmin_password.yaml`](./platform_registeradmin_password.yaml) | Admin via a **password** (Keycloak realm user; needs managed Keycloak). |
| [`platform_registeradmin_both.yaml`](./platform_registeradmin_both.yaml) | Both methods at once (certificate + password). |

### Secret injection
| Sample | Shows |
|---|---|
| [`platform_vault_injection.yaml`](./platform_vault_injection.yaml) | Inject secrets with the HashiCorp Vault Agent Injector (fleet-wide passthrough). |

### Other
| Sample | Shows |
|---|---|
| [`platform_quickstart_develop_latest.yaml`](./platform_quickstart_develop_latest.yaml) | Everything managed on the ILM `develop-latest` images (maintainer/CI use). |

## Connector samples

The operator also manages standalone `Connector` CRs:
[`connector_minimal.yaml`](./connector_minimal.yaml),
[`connector_full.yaml`](./connector_full.yaml),
[`connector_with_registration.yaml`](./connector_with_registration.yaml).

## Proxy samples

The `Proxy` CR deploys the ILM proxy — the outbound-only broker bridge for restricted
network zones — from a provisioning-issued config token Secret (normally part of the
manifest the ILM UI renders; design: [`docs/design/proxy-operator.md`](../../docs/design/proxy-operator.md)):
[`proxy_minimal.yaml`](./proxy_minimal.yaml) (token Secret ref only — the common case),
[`proxy_full.yaml`](./proxy_full.yaml) (every optional knob, annotated).

> `kustomization.yaml`, `v1alpha1_platform.yaml`, `v1alpha1_connector.yaml`, and
> `v1alpha1_proxy.yaml` are Kustomize/scaffolding entries, not curated examples — use
> the named samples above.
