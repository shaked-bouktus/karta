<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 NVIDIA Corporation -->

# Argo CD: GoReleaser pipeline boundaries

## TL;DR

- C1: Use a separate internal release-tool module when its dependencies are not product dependencies. Add an explicit root check for that module. Argo CD isolates its previous-release helper, but its normal checks do not cover the helper module.
- C2: Do not make GoReleaser the owner of normal CLI and operator builds or the full presubmit. Keep those paths under Make and `go build`. Let GoReleaser own release CLI cross-compilation, names, checksums, changelog generation, and upload.
- C3: Keep act-specific and non-publishing dry-run branches out of the real release workflow. Put them in a separate entry workflow and share the underlying build unit. Argo CD uses this split for container PR builds.
- C4: Enforce the exact public release platform set as well as its shape. A cheap dry run may build only a representative platform. Exact configuration validation and full matrix execution are different controls.

## How it works

Argo CD has three Go modules. The root product module is in `go.mod` (https://github.com/argoproj/argo-cd/blob/master/go.mod). GitOps Engine is another product module. The small release-note helper has its own `hack/get-previous-release/go.mod` (https://github.com/argoproj/argo-cd/blob/master/hack/get-previous-release/go.mod). The helper imports `golang.org/x/mod/semver` and has direct unit tests in `hack/get-previous-release/get-previous-version-for-release-notes_test.go` (https://github.com/argoproj/argo-cd/blob/master/hack/get-previous-release/get-previous-version-for-release-notes_test.go).

The helper module has independent dependency pins. It uses `golang.org/x/mod v0.21.0` and `testify v1.9.0`. The root module uses `golang.org/x/mod v0.37.0` and `testify v1.11.1`. Its Go directives also differ: `1.26.1` in the helper and `1.26.4` at the root. This proves dependency isolation. It also shows that isolation creates another update surface.

The release workflow invokes the helper directly to set `GORELEASER_PREVIOUS_TAG`. See `.github/workflows/release.yaml` (https://github.com/argoproj/argo-cd/blob/master/.github/workflows/release.yaml#L146-L160). The root `Makefile` (https://github.com/argoproj/argo-cd/blob/master/Makefile#L442-L470) explicitly enters the GitOps Engine module during tests. It has no equivalent target for `hack/get-previous-release`. The ordinary CI workflow runs root `go mod tidy`, `make build-local`, and `make test-local`. See `.github/workflows/ci-build.yaml` (https://github.com/argoproj/argo-cd/blob/master/.github/workflows/ci-build.yaml#L65-L126) and the unit-test job (https://github.com/argoproj/argo-cd/blob/master/.github/workflows/ci-build.yaml#L158-L228). A nested module is excluded from root `go list ./...`, so the helper tests are not part of those root commands.

Normal builds remain Make-owned. `cli-local`, `argocd-all`, `server`, `repo-server`, and `controller` use `go build` in the `Makefile` (https://github.com/argoproj/argo-cd/blob/master/Makefile#L306-L357). `build-local` compiles the root packages. `pre-commit-local` composes code generation, build, lint, and tests. See the `Makefile` check paths (https://github.com/argoproj/argo-cd/blob/master/Makefile#L431-L460) and `pre-commit-local` (https://github.com/argoproj/argo-cd/blob/master/Makefile#L598-L604). The main CI workflow calls these Make paths. It does not call GoReleaser.

GoReleaser has a narrower release role. `.goreleaser.yaml` (https://github.com/argoproj/argo-cd/blob/master/.goreleaser.yaml#L5-L60) builds `./cmd`, injects release metadata, emits platform-qualified binary names, creates SHA-256 checksums, and configures the GitHub release. The tag-only release workflow (https://github.com/argoproj/argo-cd/blob/master/.github/workflows/release.yaml#L1-L9) runs pinned GoReleaser with `release --clean`. It then feeds GoReleaser artifact hashes into provenance generation. See the GoReleaser job (https://github.com/argoproj/argo-cd/blob/master/.github/workflows/release.yaml#L124-L214).

Container and operator delivery is outside GoReleaser. The release workflow calls a reusable image workflow with four Linux platforms and `push: true`. The reusable `.github/workflows/image-reuse.yaml` (https://github.com/argoproj/argo-cd/blob/master/.github/workflows/image-reuse.yaml#L1-L25) accepts `platforms` and `push`, then sends both to Docker Buildx in its build step (https://github.com/argoproj/argo-cd/blob/master/.github/workflows/image-reuse.yaml#L175-L202). This keeps image build, registry login, push, and signing under a separate owner.

The real release workflow has no pull request, manual, act, or snapshot branch. It starts only on `v*` tags. Its image call always publishes. Its GoReleaser call always uses release mode. The snapshot stanza in `.goreleaser.yaml` (https://github.com/argoproj/argo-cd/blob/master/.goreleaser.yaml#L94-L96) is not exercised by the checked workflows.

Argo CD puts non-publishing image validation in a separate `.github/workflows/image.yaml` (https://github.com/argoproj/argo-cd/blob/master/.github/workflows/image.yaml#L1-L14). That entry workflow reuses `image-reuse.yaml` with `push: false` for pull requests. It defaults pull requests to `linux/amd64`. A label opts into the full four-platform image build. Push events use all four platforms and publish. See the platform selection and calls (https://github.com/argoproj/argo-cd/blob/master/.github/workflows/image.yaml#L85-L128).

The CLI release matrix is an exact seven-tuple set. It is Linux on amd64, arm64, s390x, and ppc64le; Darwin on amd64 and arm64; and Windows on amd64. GoReleaser expresses this as an OS and architecture cross-product with five exclusions in `.goreleaser.yaml` (https://github.com/argoproj/argo-cd/blob/master/.goreleaser.yaml#L25-L44). The legacy `release-cli` Make target spells out the same seven outputs in `Makefile` (https://github.com/argoproj/argo-cd/blob/master/Makefile#L318-L326). Installation documentation names Linux amd64, both Darwin variants, and Windows amd64 in `docs/cli_installation.md` (https://github.com/argoproj/argo-cd/blob/master/docs/cli_installation.md). The signed-asset guide lists all seven release artifacts in `docs/operator-manual/signed-release-assets.md` (https://github.com/argoproj/argo-cd/blob/master/docs/operator-manual/signed-release-assets.md#L9-L24).

There is no checked presubmit that compares the GoReleaser matrix with the Make matrix or documentation. The workflow inventory describes CI and release as separate concerns in `.github/workflows/README.md` (https://github.com/argoproj/argo-cd/blob/master/.github/workflows/README.md#L1-L24). The normal CI paths validate Go code and images, but do not invoke GoReleaser.

## Relevance to Karta

C1 should be accepted with a coverage condition. A separate `hack/release` module is a good boundary for release-only parsing, GitHub metadata, or GoReleaser support libraries. Those dependencies should not enlarge Karta's runtime module. The root `make check` must explicitly enter that module for test, build, and tidy verification. Module isolation without check isolation is incomplete.

C2 should be rejected as stated. GoReleaser should not become Karta's universal build front door. Normal CLI and operator builds should stay usable through Make and direct Go tooling. Presubmit should test product packages without requiring release state, tags, tokens, changelog access, or artifact upload behavior. GoReleaser should own the final distributable CLI artifacts. Container or operator images should remain under the image build path.

C3 should use two entrypoints and one implementation. Keep the real workflow tag-triggered and publishing-oriented. Put act and pull request dry runs in a separate workflow. Both entrypoints can call the same reusable workflow, Make target, or script with an explicit publish parameter. The dry-run entrypoint should have read-only permissions and fake release metadata. This follows Argo CD's `image.yaml` plus `image-reuse.yaml` pattern without mixing local-runner conditions into production release jobs.

C4 should use an exact set contract. Validate allowed OS and architecture values, reject duplicates, and compare the resulting tuples as a set against Karta's supported release matrix. Do not make list order part of the contract. A separate act smoke test can execute one representative tuple. CI can reserve full cross-compilation for a focused job or release candidate. This catches accidental platform deletion without requiring every local dry run to pay the full matrix cost.

## Evidence

- `hack/get-previous-release/go.mod` (https://github.com/argoproj/argo-cd/blob/master/hack/get-previous-release/go.mod) isolates the release helper and its versions.
- `hack/get-previous-release/get-previous-version-for-release-notes.go` (https://github.com/argoproj/argo-cd/blob/master/hack/get-previous-release/get-previous-version-for-release-notes.go) contains the release-only tag-selection command.
- `hack/get-previous-release/get-previous-version-for-release-notes_test.go` (https://github.com/argoproj/argo-cd/blob/master/hack/get-previous-release/get-previous-version-for-release-notes_test.go) tests its release-series rules.
- `go.mod` (https://github.com/argoproj/argo-cd/blob/master/go.mod) shows the independent root module versions.
- `Makefile` (https://github.com/argoproj/argo-cd/blob/master/Makefile) owns local builds, tests, release prechecks, and a legacy release matrix.
- `.goreleaser.yaml` (https://github.com/argoproj/argo-cd/blob/master/.goreleaser.yaml) owns released CLI metadata, exact platforms, binary names, checksums, and GitHub release content.
- `.github/workflows/ci-build.yaml` (https://github.com/argoproj/argo-cd/blob/master/.github/workflows/ci-build.yaml) runs ordinary build, lint, tidy, code generation, and tests without GoReleaser.
- `.github/workflows/release.yaml` (https://github.com/argoproj/argo-cd/blob/master/.github/workflows/release.yaml) is the tag-only publishing graph.
- `.github/workflows/image.yaml` (https://github.com/argoproj/argo-cd/blob/master/.github/workflows/image.yaml) is the separate pull request and branch image entrypoint.
- `.github/workflows/image-reuse.yaml` (https://github.com/argoproj/argo-cd/blob/master/.github/workflows/image-reuse.yaml) is the shared publish-controlled image implementation.
- `docs/cli_installation.md` (https://github.com/argoproj/argo-cd/blob/master/docs/cli_installation.md) makes platform-specific CLI names user-facing.
- `docs/operator-manual/signed-release-assets.md` (https://github.com/argoproj/argo-cd/blob/master/docs/operator-manual/signed-release-assets.md) treats the release assets as a documented set.

## Lessons for Karta

1. Give the release helper its own module only if it brings release-only dependencies or needs an independent dependency lifecycle.
2. Add a root target such as `check-release-tools`. It should enter the nested module, run tests, build it, run `go mod tidy`, and fail on a diff.
3. Keep one authoritative product build path. GoReleaser should call or reproduce only the final release build settings that it must control.
4. Give GoReleaser ownership of artifact names, version linker flags, archives or raw binaries, checksums, and release upload.
5. Keep operator image builds under Docker or the existing image target. Do not route them through GoReleaser merely to make one tool look universal.
6. Keep the production release workflow small and tag-only. Put act accommodations in a non-publishing workflow that reuses the same lower-level unit.
7. Test the exact platform tuple set statically. Test matrix shape too. Run only a representative tuple in cheap local or pull request smoke tests when full execution is expensive.
8. Make intentional platform changes update config, tests, and user-facing release documentation in one review.

## What NOT to copy

- Do not create a nested release module and leave its tests outside the root presubmit. Argo CD explicitly covers its GitOps Engine module but not its release-helper module.
- Do not let Go directives and dependency pins drift silently across modules. Argo CD's helper and root already show version differences.
- Do not duplicate the exact platform list in Make, GoReleaser, and documentation without an automated drift check.
- Do not keep two independent production cross-compilation owners. Argo CD's `release-cli` and GoReleaser currently spell out the same seven CLI targets.
- Do not treat a GoReleaser snapshot stanza as proof of a dry run. A workflow must actually invoke snapshot or build-only behavior.
- Do not place act-specific conditions around publish, signing, provenance, and post-release mutation steps in the real workflow. Use a separate safe entrypoint with shared internals.
