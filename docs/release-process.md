# Releasing the ILM Operator

This is the repeatable runbook for cutting an operator release. It assumes you have read
[CONTRIBUTING.md](../CONTRIBUTING.md) — in particular that `main` is the development line, that
the chart on `main` stays at `X.Y.Z-develop`, and that a release exists only as a **tag**.

Two version lines are in play and they are independent:

- the **operator** version — `vX.Y.Z`, the git tag, the image tag, and the chart version;
- the **platform** version — `2.19.0` and friends, the ILM release a BOM bundle in
  `pkg/bom/bom.go` describes. One operator build supports a range of them.

An operator release usually — but not necessarily — ships a newly released platform bundle. The
ordering rules for platform versions themselves live in
[docs/design/platform-versioning.md](design/platform-versioning.md) and in the `DefaultVersion`
doc comment in `pkg/bom/bom.go`; this page covers the operator's own release mechanics.

## Set these once

Every command below uses these variables. Set them in the shell you run the release from.

```bash
version=1.0.0                      # the operator release version, no leading "v"
tag="v${version}"
platform_version=2.19.0            # the ILM platform version this release must support
charts=/path/to/helm-charts        # a local checkout of OmniTrustILM/helm-charts
```

## 1. Pre-release checklist

**`main` must be green before anything else happens.** The release branch itself is watched by
no workflow — pushing it triggers nothing — so `main` is the last point at which CI validates
the code you are about to tag.

Run the full quality sequence on a clean checkout of `main`:

```bash
git switch main
git pull
make lint
make coverage                          # runs make test, then enforces the 80% threshold
helm lint deploy/charts/ilm-operator
make bundle
make trivy
```

Then run the **gate** tier — the managed e2e, with the real CloudNativePG, RabbitMQ and Keycloak
operators:

```bash
make test-e2e-managed
```

Locally that runs all seven managed blocks serially and takes hours; the suite is
`ContinueOnFailure`, so one run reports every failure rather than stopping at the first. To
narrow a re-run, pass a single block:

```bash
make test-e2e-managed E2E_MANAGED_LABEL=matrix-migration
```

The blocks are `managed-postgres`, `managed-rabbitmq`, `managed-keycloak`, `full`,
`matrix-upgrade`, `matrix-preview` and `matrix-migration`. The faster option for a release gate
is to dispatch the **E2E Managed** workflow on GitHub Actions, which runs all seven in parallel,
each on its own Kind cluster.

**Verify the BOM against the tagged charts release.** The per-version contract in
`pkg/bom/bom.go` — image coordinates, wiring env-var names, Secret keys, managed messaging
topology — must match what the platform actually ships. Take that from the **rendered** output
of the **tagged** chart, never from eyeballed `values.yaml` defaults, which do not show what the
templates compose:

```bash
git -C "$charts" fetch --tags
git -C "$charts" worktree add "/tmp/ilm-charts-${platform_version}" "$platform_version"
helm dependency update "/tmp/ilm-charts-${platform_version}/charts/ilm"
helm template ilm "/tmp/ilm-charts-${platform_version}/charts/ilm" \
  > "/tmp/ilm-${platform_version}.rendered.yaml"

# The contract matrix: every image the release ships, and the env names Core consumes.
grep -E '^[[:space:]]+image:' "/tmp/ilm-${platform_version}.rendered.yaml" | sort -u
grep -E '^[[:space:]]+- name: [A-Z_]+$' "/tmp/ilm-${platform_version}.rendered.yaml" | sort -u
```

Compare that against the bundle for `$platform_version` and against the operator's own golden
renders under `test/golden/testdata`. Clean up the worktree when you are done:

```bash
git -C "$charts" worktree remove "/tmp/ilm-charts-${platform_version}"
```

This is the same method the version model was derived from — see
[docs/design/platform-versioning.md](design/platform-versioning.md), sections 1 and 6a.

### The documentation-site pre-flight (a pre-tag gate)

**This runs before the tag, and it is a gate, not a formality.** The pages under `docs/site/` are
pulled into https://docs.otilm.com at an **immutable ref** — the tag you are about to push (step
9). A page that fails the site build, or carries a broken link, cannot be repaired by a later
commit on `main`: the pinned tag keeps serving the broken page. The only remedy after the fact is
a **patch tag** (step 10). So prove the set builds while a tag can still be moved — that is, while
it does not exist yet.

Two checks, in order. First the link rule: within the synced set only relative links that stay
inside `docs/site/` — one level deep — are legal (`./upgrading.md` or
`./custom-resources/platform.md` from a top-level page, `../installation.md` or `./platform.md`
from a CR guide, `#anchor`), because a relative link that leaves the set resolves on GitHub and
breaks on the site.

```bash
SRC=docs/site python3 - <<'PY'
import os, re, sys
src = os.path.abspath(os.environ["SRC"])
pat = re.compile(r'\[[^\]]*\]\(([^)\s]+)(?:\s+"[^"]*")?\)')
files = sorted(os.path.join(r, n) for r, _, ns in os.walk(src)
               for n in ns if n.endswith(".md"))
bad = []
for f in files:
    rel = os.path.relpath(f, src)
    text = open(f, encoding="utf-8").read()
    for m in pat.finditer(text):
        t = m.group(1)
        if t.startswith("https://") or t.startswith("#") or t.startswith("%"):
            continue
        base = t.split("#")[0]
        if not (base.startswith("./") or base.startswith("../")):
            bad.append((rel, t, "not a relative link"))
            continue
        target = os.path.normpath(os.path.join(os.path.dirname(f), base))
        if not target.startswith(src + os.sep):
            bad.append((rel, t, "leaves the synced set"))
        elif not os.path.exists(target):
            bad.append((rel, t, "target does not exist"))
        elif target not in files:
            bad.append((rel, t, "target is not part of the synced set"))
for b in bad:
    print("VIOLATION:", *b)
print(f"files: {len(files)}  violations: {len(bad)}")
sys.exit(1 if bad else 0)
PY
```

Then the real site build, against a throwaway copy of the set in the site's own target directory.
A checkout of `OmniTrustILM/documentation` and a running Docker daemon (PlantUML rendering) are
prerequisites:

```bash
site=~/Development/GitHub/documentation
target="$site/docs/certificate-key/installation-guide/deployment/deployment-operator"

mkdir -p "$target/custom-resources"
cp docs/site/*.md "$target/"
cp docs/site/custom-resources/*.md "$target/custom-resources/"
(cd "$site" && node scripts/render-diagrams.mjs \
   && NODE_OPTIONS="--max_old_space_size=10240" yarn build)
```

`onBrokenLinks: 'throw'` makes a bad route-level link a hard build failure, and the site sets
`markdown.hooks.onBrokenMarkdownLinks: 'throw'` as well, so a bad **markdown** link fails the
build the same way — there are no ignorable warnings. Fix whatever appears in **this**
repository under `docs/site/`, then re-run. Fixing it on the site is never correct: the next sync
overwrites it.

One warning is expected and harmless — *"Cannot infer the update date for some files, as they are
not tracked by git"* — because the pre-flight copies are untracked in the site checkout.

Restore the site checkout **byte-identical** when you are done — the pre-flight must leave no
trace:

```bash
rm -rf "$target"
(cd "$site" && git status --short)     # must print nothing
```

Only once this is clean do you proceed to the release commit and the tag.

## 2. The platform-release trigger

The charts release **first**. `OmniTrustILM/helm-charts` tags `$platform_version`, which is what
publishes the platform's own artifacts; only then can the operator advertise that version.

The flip itself is an ordinary PR to `main`, not part of the release branch:

1. In `pkg/bom/bom.go`, set `Released: true` on the bundle for `$platform_version`.
2. Move `DefaultVersion` to it in the **same** PR. The convention is that release day sets both
   together, so a version-less fresh install lands on the version you just released.
   `DefaultVersion` must always name a released bundle — `TestDefaultVersionIsReleased` enforces
   that much mechanically. Leaving the default trailing on an older release is *possible* (the
   two flags are independent, so a released bundle can be reachable by explicit `spec.version`
   before the default moves) but it is the **exception**: do it only deliberately, and say why in
   the PR description.
3. Update the supported-version table (the engine matrix) and the default marker in the
   **canonical** user guide — [docs/site/upgrading.md](site/upgrading.md), under its
   *Supported versions* heading — and drop the preview wording from any sample that pinned the
   bundle while it was a preview. `docs/site/` is the canonical source for every end-user fact;
   `docs/versions.md` and `docs/upgrades.md` are pre-absorption originals awaiting reduction to
   stubs, so do not add or correct facts there.
4. `make test` — the BOM tests, the samples specs and the golden renders all move with this.

Merge that PR to `main` and let CI go green before cutting the release branch.

## 3. The release commit

Cut the release branch from `main` and set the chart's `version` **and** `appVersion`. Nothing
automates either one, and **they are not the same string**:

- **`version`** is the chart's own SemVer — plain `X.Y.Z`, no `v`. This is what
  `publish_chart.yaml` checks (it must not end in `-develop`) and what the chart is published
  under.
- **`appVersion`** is what the chart resolves the operator **image tag** from. The chart's
  helper is `{{- $tag := default .Chart.AppVersion .Values.image.tag -}}`
  (`templates/_helpers.tpl`), and `values.yaml` ships `image.tag: ""`, so out of the box the
  rendered image is `hub.omnitrustregistry.com/ilm/operator:<appVersion>`. **`appVersion` must
  therefore be the literal tag the registry carries.**

Images carry the **plain SemVer, no `v`** — the registry convention every other ILM image
follows (`core:2.19.0`, `scheduler:1.1.1`). `publish_docker.yaml` overrides the org workflow's
default tag rules with the `v`-stripping semver pattern for exactly this reason, and
`release.yaml` pins the install manifests to
`hub.omnitrustregistry.com/ilm/operator:${image_version}` (the git tag with the `v` stripped)
and fails the release if that exact image is not pullable. Only the **git tag** and the GitHub
release keep the `v` prefix — Go modules require `vX.Y.Z` tags, and the CLI pins this module by
them. So for a release `1.0.0`: git tag `v1.0.0`, chart `version: 1.0.0`,
`appVersion: "1.0.0"`, image `operator:1.0.0`.

```bash
git switch main
git pull
git switch -c "release/${tag}"

# Chart version and appVersion are both the plain SemVer — the image tag carries no "v".
CHART_VERSION="$version" yq -i \
  '.version = strenv(CHART_VERSION) | .appVersion = strenv(CHART_VERSION)' \
  deploy/charts/ilm-operator/Chart.yaml

grep -E '^(version|appVersion):' deploy/charts/ilm-operator/Chart.yaml
# version: 1.0.0
# appVersion: "1.0.0"

# The image reference that the chart will actually render:
helm template ilm-operator deploy/charts/ilm-operator \
  | grep -oE 'hub\.omnitrustregistry\.com/[^"]*' | sort -u
# hub.omnitrustregistry.com/ilm/operator:1.0.0

git commit -am "chore(release): ilm-operator ${tag}"
git push -u origin "release/${tag}"
```

If you get this wrong the chart installs cleanly and then fails at pull time with
`ImagePullBackOff` on a tag that was never published — which is exactly the failure the
rehearsal below is designed to catch before real users see it.

**Never merge this branch into `main`.** The merge would push a non-`-develop` chart version to
`main`, and `publish_chart.yaml`'s development-version check fails exactly that. The escape
hatch that lets a release branch exist at all is in `test_chart.yaml`, which skips the
`-develop` requirement for PRs whose head branch starts with `release` and instead requires a
non-`-develop` version — it makes a release PR *reviewable*, not mergeable.

If a fix has to be made during the release, land it on `main` first as a normal PR and
cherry-pick it onto the release branch. The release branch is not a place to develop.

## 4. Rehearse the tag chain (optional)

This step is **optional** — a disposable dry run of the tag chain, worth considering after any
change to `publish_docker.yaml`, `release.yaml` or `publish_chart.yaml`, because those links
only ever fire on a tag, so a mistake in any of them is invisible until the moment it matters.
The alternative is tagging directly (section 5) and accepting that a failed link means deleting
and re-cutting the release tag.

One trap to know either way: a `workflow_run`-triggered workflow (the manifests publish) always
executes the definition from the **default branch**, not from the tagged commit — so a
`release.yaml` change that is only on the release branch does not take effect. Land workflow
changes on `main` first; a manifests run that failed for this reason is recovered with
`workflow_dispatch` against the existing tag after the fix merges, no re-tag needed.

Push a scratch pre-release tag from the release branch:

```bash
git switch "release/${tag}"
git tag -a "${tag}-rc.1" -m "ilm-operator ${tag}-rc.1"
git push origin "${tag}-rc.1"
```

Watch the chain end to end in GitHub Actions:

1. **Publish Docker image** builds and pushes `hub.omnitrustregistry.com/ilm/operator:${tag}-rc.1`.
2. **Publish release manifests** starts only after that workflow *succeeds* for a tag. It
   regenerates `ilm-operator.yaml` and `ilm-operator.crds.yaml` from `config/` with the image
   pinned to the tag, re-verifies that the image is pullable (retrying for registry propagation),
   cosign-signs both manifests and `checksums.txt`, and creates the GitHub release. Recognised
   pre-release suffixes — `rc`, `alpha`, `beta`, `preview`, `develop`, `dev`, `pre` — are marked
   as GitHub pre-releases automatically.
3. **Publish Chart** runs its release-version check (the chart must not be `-develop`, which the
   release commit already handled), `ct lint`, then packages, pushes and cosign-signs the chart
   to `oci://hub.omnitrustregistry.com/ilm-helm`.

### Confirm the published image tag format, then fix `appVersion`

The rehearsal proves the tag rules end to end: `publish_docker.yaml`'s override strips the `v`,
so the rehearsal must have published the **plain** form:

```bash
docker pull "hub.omnitrustregistry.com/ilm/operator:${version}-rc.1" # expected: plain SemVer
docker pull "hub.omnitrustregistry.com/ilm/operator:${tag}-rc.1"     # must NOT exist
```

The first must succeed and the second must fail. If it is the other way around, the tag-rules
override in `publish_docker.yaml` has regressed — fix that rather than `appVersion`. If
`appVersion` disagrees with what was actually published, amend the release commit before tagging
for real:

```bash
git switch "release/${tag}"
APP_VERSION="$version" yq -i '.appVersion = strenv(APP_VERSION)' \
  deploy/charts/ilm-operator/Chart.yaml
git commit --amend --no-edit -a
git push --force-with-lease
```

Then re-render and confirm the chart points at an image that exists:

```bash
helm template ilm-operator deploy/charts/ilm-operator \
  | grep -oE 'hub\.omnitrustregistry\.com/[^"]*' | sort -u
```

### Then clean up the rehearsal

Three things to know before you delete the rehearsal tag:

- The rehearsal tag publishes the chart at the **final** version `$version`, because the release
  commit already stripped `-develop`. That is expected.
- Chart change detection on a tag diffs against the *previous* tag. If the rehearsal tag is
  still present when you push the real tag and no chart file changed between them, the chart
  push is skipped as "no changes" — the artifact is already in the registry at the right
  version, but the real tag's run will not show a chart push.
- **Deleting the git tag does not undo what was published.** The rc image, the rc chart and any
  floating registry tag the publish moved (a `latest`-style tag, if the shared workflow maintains
  one for tag builds) all stay as the rehearsal left them. Check the registry after a rehearsal
  and, if a floating tag was moved to the rc image, re-point it once the real release is out —
  git has no way to restore it for you.

With that understood, delete the rehearsal tag and its GitHub pre-release before the real tag.
That keeps the release list clean and restores the change-detection baseline:

```bash
git push origin ":refs/tags/${tag}-rc.1"
git tag -d "${tag}-rc.1"
gh release delete "${tag}-rc.1" --yes
```

## 5. Tag the release

```bash
git switch "release/${tag}"
git tag -a "$tag" -m "ilm-operator ${tag}"
git push origin "$tag"
```

The same three workflows run, this time producing a full (non-pre-release) GitHub release with
notes generated from the merged PRs' labels (`.github/release.yml`). Verify all three artifacts
before announcing anything:

```bash
# The image the manifests pin.
docker manifest inspect "hub.omnitrustregistry.com/ilm/operator:${tag}"

# The install manifests and their checksums.
base="https://github.com/OmniTrustILM/operator/releases/download/${tag}"
curl -fsSLO "${base}/ilm-operator.yaml"
curl -fsSLO "${base}/ilm-operator.crds.yaml"
curl -fsSLO "${base}/checksums.txt"
shasum -a 256 -c checksums.txt          # GNU coreutils: sha256sum -c checksums.txt

# The chart (run `helm registry login hub.omnitrustregistry.com` first if the pull is refused).
helm pull "oci://hub.omnitrustregistry.com/ilm-helm/ilm-operator" --version "$version"

# The image the PUBLISHED chart resolves must itself be pullable — this is the check that
# catches an appVersion that does not match the registry's tag format.
tar -xzf "ilm-operator-${version}.tgz"
helm template ilm-operator ./ilm-operator \
  | grep -oE 'hub\.omnitrustregistry\.com/[^"]*' | sort -u \
  | xargs -n1 docker manifest inspect >/dev/null && echo "chart image OK"
```

Each manifest also ships a `.sig`; verify with the organisation's cosign public key if you have
it locally:

```bash
cosign verify-blob --key cosign.pub --signature ilm-operator.yaml.sig ilm-operator.yaml
```

## 6. After the tag

`main` stays on `-develop`. The release branch is **not** merged back — the tag pins its commit
permanently, so the branch has no further purpose once the tag is pushed:

```bash
git switch main                          # you cannot delete the branch you are standing on
git push origin --delete "release/${tag}"
git branch -D "release/${tag}"
```

Nothing else on `main` changes. The next merge to `main` publishes a develop image again, and
the chart there is still `X.Y.Z-develop`.

## 7. CLI follow-up

`ilmctl` (`OmniTrustILM/cli`) depends on this repository as an ordinary published Go module,
pinned in its `go.mod`. Once the operator tag exists, pin it **exactly** — never `@latest`, which
would let the CLI's rendered CRs drift away from the operator version it targets:

```bash
cd /path/to/cli
go get "github.com/OmniTrustILM/operator@${tag}"
go mod tidy
make test
```

Commit that on a normal PR to the CLI's `main`, then tag the CLI release.

## 8. Post-release verification

Verify the published artifacts on a **clean** cluster, and install the operator from the
**published release assets** — not with `make deploy` from the working tree, which would test a
locally built image rather than the one you just shipped.

**Fresh install of the new default:**

```bash
kind create cluster --name ilm-release-check

kubectl apply --server-side -f \
  "https://github.com/OmniTrustILM/operator/releases/download/${tag}/ilm-operator.yaml"
kubectl -n ilm-operator-system rollout status \
  deploy/ilm-operator-controller-manager --timeout=180s

make install-upstream-operators        # CloudNativePG, RabbitMQ, Keycloak, cert-manager
kubectl create namespace ilm
kubectl apply -f config/samples/platform_quickstart.yaml
kubectl -n ilm get platform -w
```

The `VERSION` printer column must show the bundle you just released — that is the check that
`DefaultVersion` moved and that a version-less CR lands on it.

**A live upgrade from the previous version**, whenever the new bundle migrates rather than just
rolling images — a renamed managed vhost, exchanges or queues means the messaging-migration
phase machine runs (fence, drain, cut over, clean up). Bring up a platform on the previous
version, move `spec.version`, and watch `status.upgrade` walk the phases to completion. The
procedure and what to expect are in [docs/site/upgrading.md](site/upgrading.md), under *Worked
example: 2.18.0 to 2.19.0 (the messaging migration)*; the same path is covered automatically by
the `matrix-migration` e2e block.

```bash
kind delete cluster --name ilm-release-check
```

## 9. Bump the documentation-site pin

The user guide under `docs/site/` is synced into https://docs.otilm.com by
`docusaurus-plugin-remote-content` at an immutable ref. Move it to this release:

1. In `OmniTrustILM/documentation`, open `docusaurus.config.js` and set
   `operatorDocsRef` to the tag you just pushed (`vX.Y.Z`) and `operatorVersion` to
   `X.Y.Z`.
2. Run `yarn docusaurus download-remote-operator-docs` and commit the refreshed pages
   under `docs/certificate-key/installation-guide/deployment/deployment-operator/`.
3. Run `NODE_OPTIONS="--max_old_space_size=10240" yarn build`.
4. Open the PR against the `documentation` branch with screenshots of the changed pages
   (the site has no PR preview).

This step is a **pin bump only**. It pulls the pages the tag already froze; it cannot change them.
That is why the [pre-flight in step 1](#the-documentation-site-pre-flight-a-pre-tag-gate) runs
before the tag, and why it is not optional.

**If the build fails here, or a page is wrong on the site**, do **not** try to fix it by
committing to `docs/site/` on `main`. `operatorDocsRef` points at
`vX.Y.Z`, and a tag pins its commit permanently (the same immutability the branch cleanup in step
6 relies on) — the sync will keep pulling the broken page no matter what lands on `main`
afterwards. Retagging is not an option either: the tag has published images, manifests and a chart
under that name.

A documentation defect discovered after the tag is therefore a **patch release**, exactly like a
code defect: land the fix on `main`, cut `release/vX.Y.Z+1` at the released tag, cherry-pick, tag,
and then repeat this step against the new tag ([10. Patch releases](#10-patch-releases)). The one
thing you can do in the meantime is hold the pin at the previous tag, so the site keeps serving
the last good copy of the guide rather than the broken one.

## 10. Patch releases

A patch is the same procedure with a shorter start. Land the fix on `main` first as a normal PR,
then cut a release branch **at the released tag** — not at `main`, which by then carries
unreleased work — and cherry-pick:

Identify the merged fix by its **exact commit SHA**. Do not reach for a branch name: `git fetch
--tags` updates tags and remote-tracking refs, *not* your local `main`, so `git rev-parse main`
can silently resolve to a stale commit and cherry-pick the wrong thing — or nothing you meant.

```bash
git fetch origin --tags

# Take the SHA from the merged PR, or read it off the up-to-date remote-tracking ref:
git log --oneline -5 origin/main
fix_commit=REPLACE_WITH_THE_MERGED_FIX_SHA

git switch -c release/v1.0.1 v1.0.0
git cherry-pick "$fix_commit"
git show --stat HEAD          # confirm you picked what you meant to pick
```

Then set the chart's `version` to `1.0.1` and `appVersion` to the matching image tag (step 3),
rehearse only if the workflows changed (step 4), tag `v1.0.1` (step 5), and clean up (step 6).
Patch tags are plain semver — there is no separate channel and no suffix.

**A documentation-only patch is still a full patch release**, because the site pin can only point
at a tag. Run the site pre-flight in step 1 before tagging — it is the check that would have caught
the defect — and finish with step 9 against the new tag, so `operatorDocsRef` moves off the broken
one.
