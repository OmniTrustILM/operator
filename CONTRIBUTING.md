# Contributing to the ILM Operator

This page is the contributor's entry point: how the development line works, how a change gets
from a branch to a merged PR, and what the repository enforces along the way. The engineering
reference — quality gates, testing tiers, the per-kind pattern for new CRDs, and the platform
invariants — is [CLAUDE.md](CLAUDE.md); this page links to it rather than restating it.

Cutting a release is a separate, ordered procedure: see
[docs/release-process.md](docs/release-process.md).

## The development model

**`main` is the development line.** It is the operator's equivalent of the `-develop` versions
the Helm charts use: every change merges to `main` first, and `main` never carries a release
version. Releases are cut from short-lived `release/*` branches and exist only as tags.

| | `main` | `release/*` |
| --- | --- | --- |
| Chart version in `deploy/charts/ilm-operator/Chart.yaml` | `X.Y.Z-develop` — CI rejects anything else | `X.Y.Z` — CI rejects `-develop` |
| What a push produces | the rolling **develop** operator image (and, for a chart change, the develop chart) | nothing — no workflow watches these branches |
| Merged back into `main` | — | **never** |

Three consequences worth internalising:

- **The chart stays on `-develop`.** `deploy/charts/ilm-operator/Chart.yaml` is `0.1.0-develop`
  today, and two workflows enforce it. `test_chart.yaml` fails a chart-touching PR whose head
  branch is not `release*` when the version does not end in `-develop`; `publish_chart.yaml`
  repeats the check on the merge push to `main`. Do not "helpfully" bump the chart to a release
  version in a feature PR — that bump belongs to the release commit and nowhere else.
- **Merging publishes a develop build, not a release.** A merge to `main` that touches operator
  code runs `publish_docker.yaml`, which builds and pushes the operator image through the shared
  organisation workflow. The rolling develop tag is the one the repository's own committed
  install manifests are pinned to — `hub.omnitrustregistry.com/ilm/operator:develop-latest`
  (`DEV_MANIFEST_IMG` in the `Makefile`, applied to `deploy/manifests/` by `make manifests`).
- **Nothing on `main` is a release until a tag exists.** The flat install manifests, their cosign
  signatures, and the GitHub release itself come from `release.yaml`, which runs only for a tag
  and refuses to publish when that tag does not exist. There is no "release from `main`".

## Making a change

### 1. Branch off `main`

```bash
git switch main
git pull
git switch -c feat/proxy-pdb-defaults   # <type>/<short-description>
```

### 2. Work the fast loop

`make lint` and `make test` (envtest + unit + golden renders, about a minute) are the loop. Run
them after every meaningful edit:

```bash
make lint
make test
```

`make test` runs `manifests`, `generate`, `fmt` and `vet` first, so it regenerates CRDs, RBAC,
the chart's embedded CRDs, the committed development manifests and the deep-copy code **in
place**. Check `git status` afterwards and commit whatever it regenerated.

**Do not iterate against the Kind e2e suite.** It is a pre-release and nightly *gate*, not the
development loop — see [CLAUDE.md → Testing Commands](CLAUDE.md#testing-commands) for the tier
split and for what to do when the e2e catches a bug (add the fast assertion that would have
caught it, then move on).

### 3. Run the full quality sequence before opening the PR

Each of these is a gate somewhere in CI or at release time; running them locally is cheaper than
discovering them on the PR. See
[CLAUDE.md → Quality Verification Workflow](CLAUDE.md#quality-verification-workflow) for what
each one checks and how to react when it fails.

```bash
make lint                              # 0 issues
make coverage                          # runs make test, then enforces the 80% threshold
helm lint deploy/charts/ilm-operator   # the chart must lint clean
make bundle                            # regenerates and validates the OLM bundle
make trivy-fs                          # no HIGH/CRITICAL findings (make trivy also builds the image)
make sonar                             # optional: local SonarQube in an ephemeral container
```

### 4. Commit and open the PR

Commit messages are plain, descriptive, present-tense summaries of what the change does. A
conventional-commit type prefix (`feat`, `fix`, `docs`, `chore`, `refactor`, optionally scoped —
`feat(platform):`) is the prevailing style in the log; CI does not enforce it. Nothing in this
repository parses commit messages, so clarity beats ceremony.

Label the PR (`new-feature`, `enhancement`, `bug`) — `.github/release.yml` groups the
auto-generated release notes by those labels, so an unlabelled PR lands under "Other".

### What CI runs on your PR

| Workflow | When | What it does |
| --- | --- | --- |
| **Test** (`test.yaml`) | every PR, except chart-only changes | `golangci-lint`; `make test` plus an inline 80% coverage check; builds the operator image once; runs the **fast** e2e tier (`!managed`) on Kind from that image |
| **Test Docker image** (`test_docker_image.yaml`) | every PR, except chart-only changes | the shared organisation container test + scan |
| **Test Chart** (`test_chart.yaml`) | PRs touching `deploy/charts/**` | the `-develop` version check, `ct lint`, and `ct install` on Kind |
| **E2E Managed** (`e2e-managed.yaml`) | PRs touching the managed-infra paths listed in the workflow; nightly at 02:30 UTC; on demand | the seven managed blocks in parallel, each on its own Kind cluster with the real CloudNativePG / RabbitMQ / Keycloak operators |
| **Sonar** (`dispatch-sonar.yaml` → `sonar.yaml`) | after **Test** succeeds on a PR | SonarCloud analysis with the coverage and lint reports from that run |

Note that **Test** ignores `deploy/charts/**`: a chart-only PR is validated by **Test Chart**
and nothing else, and a code-only PR never runs the chart jobs.

## Where things live

The directory map and the responsibilities of each package are in
[CLAUDE.md → Project Structure](CLAUDE.md#project-structure): CRD types in `api/v1alpha1/`, pure
builder functions in `internal/builder/`, thin reconcilers in `internal/controller/`, and the
importable surface (`pkg/bom`, `pkg/capabilities`, `pkg/convert`) that the `ilmctl` CLI consumes.
Adding a Kind follows a fixed seven-step pattern — types, builders, controller, apply strategy,
capability gating, wiring in `cmd/main.go`, tests — documented in
[CLAUDE.md → How to add a new CRD](CLAUDE.md#how-to-add-a-new-crd-the-per-kind-pattern); `Proxy`
was added exactly that way. Platform work additionally has non-negotiable invariants (no secrets
in CRs, SCC-clean pods, deletion safety, gated upstream dependencies, migrate-never-clobber for
managed brokers) in [CLAUDE.md → Platform operator](CLAUDE.md#platform-operator).

## Changing the platform contract between releases

A new ILM platform version is a **data** change: a new bundle in `pkg/bom/bom.go` carrying that
version's component images, wiring profile (env-var names, connection-string template, Secret
keys) and managed messaging topology. You do not have to wait for release day to land it.

A bundle whose platform artifacts are not published yet lands as a **preview** — `Released:
false`. A preview bundle is deliberately hard to reach: it resolves only via an explicit
`spec.version`, `SupportedVersions()` excludes it so it is never advertised or defaulted, and
`DefaultVersion` may not name it (`TestDefaultVersionIsReleased` enforces that). That is the
point — the version contract can land, be reviewed, be rendered and be exercised by the e2e
version matrix ahead of release day, without any chance of a fresh install landing on it.

Release day is then a data-only flip — `Released: true` **and** `DefaultVersion` moved to that
bundle in the same PR — carried out as part of the release cycle, not as part of your contract
PR. (Holding the default back on an older release is possible, but it is a deliberate exception
that has to be justified in the PR.) The full rules are in
[CLAUDE.md → Released vs preview version bundles](CLAUDE.md#released-vs-preview-version-bundles);
the model and the ordering are in
[docs/design/platform-versioning.md](docs/design/platform-versioning.md) and the `DefaultVersion`
doc comment in `pkg/bom/bom.go`. A sample that pins a preview bundle must say so in the sample.

## Docs, samples, and generated files

- **Docs** live in `docs/` and are linked from the table in [README.md](README.md). User-facing
  behaviour changes belong in the matching guide — `docs/versions.md` for the supported-version
  matrix, `docs/upgrades.md` for upgrade procedure and guards, `docs/configuration.md` for CR
  fields — not only in a design document.
- **Samples are tested, not decorative.** The samples spec in each controller package applies
  every matching `config/samples/` file to the envtest apiserver, so a sample that violates a CEL
  validation rule fails `make test`. The globs differ: Platform and Proxy use `*platform*.yaml`
  and `*proxy*.yaml` (which also cover the `v1alpha1_*` kustomize stubs), while Connector uses
  the `connector_*.yaml` prefix, deliberately excluding `v1alpha1_connector.yaml` because that
  stub's spec is intentionally empty. Name a new Connector sample `connector_*.yaml` or it is
  silently untested, and add it to the index in `config/samples/README.md`.
- **Generated files are never hand-edited.** Regenerate them:

  ```bash
  # Deep-copy code, CRDs, RBAC, the chart's embedded CRDs, deploy/manifests.
  make generate manifests

  # Golden render snapshots, after an intended change to rendered output.
  UPDATE_GOLDEN=1 go test ./test/golden/...
  ```

  Review the regenerated diff before committing — for the goldens especially, the diff *is* the
  review: it shows exactly how the rendered output changed.
- `make bundle` rewrites the image in `config/manager/kustomization.yaml`. With the default
  `IMG` it rewrites it to the value already committed, so the tree stays clean; if you pass an
  `IMG` override, restore that file before committing.

## Releasing

Contributors do not tag. When a release is due, follow
[docs/release-process.md](docs/release-process.md) — it covers the pre-release gate, the
`Released: true` flip, the release branch and its chart-version commit, the tag, and the
post-release verification.
