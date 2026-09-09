<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 NVIDIA Corporation -->

# kube-linter: GoReleaser pipeline boundaries

## TL;DR

- C1: Separate the release tooling into its own Go module. kube-linter gives GoReleaser a dedicated module, lock file, install directory, and Dependabot entry. Karta should also make nested-module checks explicit.
- C2: Keep GoReleaser as the owner of normal CLI and operator binary builds and exercise that path in presubmit. kube-linter does this for its CLI. Karta should not maintain a second cross-build implementation.
- C3: Keep `act` branch triggers and dry-run policy out of the real release workflow. kube-linter has a tag-only publishing workflow and a separate local snapshot command. A separate safe entry workflow can reuse the same lower-level release unit.
- C4: Enforce the exact release platform tuple set, not only the matrix shape. kube-linter declares an exact six-binary matrix, but its downstream tests cover only two amd64 artifacts. Karta can keep shape checks as diagnostics, not as the public contract.

## How it works

kube-linter keeps application dependencies in `go.mod` (https://github.com/stackrox/kube-linter/blob/main/go.mod). GoReleaser is absent from that module. It has a dedicated `tool-imports/goreleaser/go.mod` (https://github.com/stackrox/kube-linter/blob/main/tool-imports/goreleaser/go.mod) with module path `github.com/stackrox/kube-linter/tool-imports/goreleaser` and a direct pin on `github.com/goreleaser/goreleaser/v2 v2.17.0`. That module currently has 330 indirect requirements in the same file. Isolating it keeps that release-tool graph out of the product module.

The tool module uses the conventional build-tag import in `tool-imports/goreleaser/tools.go` (https://github.com/stackrox/kube-linter/blob/main/tool-imports/goreleaser/tools.go). Its blank import makes the GoReleaser command a module dependency. `tool-imports/goreleaser/empty.go` (https://github.com/stackrox/kube-linter/blob/main/tool-imports/goreleaser/empty.go) leaves an ordinary package when the `tools` tag is not enabled. `.github/dependabot.yaml` (https://github.com/stackrox/kube-linter/blob/main/.github/dependabot.yaml#L23-L38) gives the GoReleaser and golangci-lint modules separate update entries.

The root `Makefile` (https://github.com/stackrox/kube-linter/blob/main/Makefile#L1-L41) includes both nested modules in `deps`. It enters `tool-imports/goreleaser` and installs the pinned tool into the repository-local `.gobin`. The application build target then runs that binary as `goreleaser build --snapshot --clean`. It copies the host OS amd64 artifact from `dist` into `.gobin/kube-linter`. See `Makefile` (https://github.com/stackrox/kube-linter/blob/main/Makefile#L82-L89).

This makes GoReleaser the normal CLI build owner, not only the publisher. Local development documentation tells contributors to use `make build` and then run `.gobin/kube-linter`. See `README.md` (https://github.com/stackrox/kube-linter/blob/main/README.md#L61-L78). End-to-end tests also depend on that generated binary through the same Make target. See `Makefile` (https://github.com/stackrox/kube-linter/blob/main/Makefile#L99-L110).

There is no aggregate `check` target in kube-linter's `Makefile` (https://github.com/stackrox/kube-linter/blob/main/Makefile). Presubmit composes the checks in `.github/workflows/build.yaml` (https://github.com/stackrox/kube-linter/blob/main/.github/workflows/build.yaml#L18-L78). Its first product step is `make build`, so each pull request invokes the pinned local GoReleaser in snapshot build mode. The same job separately runs lint, generation drift detection, unit tests, Go end-to-end tests, and Bats tests.

The GoReleaser build is the matrix compiler. `.goreleaser.yaml` (https://github.com/stackrox/kube-linter/blob/main/.goreleaser.yaml#L13-L29) selects Linux, Windows, and Darwin, each on amd64 and arm64. There are no ignore entries, so the declared binary set is the six-tuple cross-product. It also centralizes `CGO_ENABLED=0`, `-mod=readonly`, `-trimpath`, version linker flags, and reproducible modification timestamps.

The same configuration owns release packaging and images. It defines raw binaries and tar archives in `.goreleaser.yaml` (https://github.com/stackrox/kube-linter/blob/main/.goreleaser.yaml#L32-L45). It defines standard and Alpine image variants, each for Linux amd64 and arm64, in `.goreleaser.yaml` (https://github.com/stackrox/kube-linter/blob/main/.goreleaser.yaml#L47-L96). It also owns binary signing, image signing, and GitHub-native changelog generation in `.goreleaser.yaml` (https://github.com/stackrox/kube-linter/blob/main/.goreleaser.yaml#L99-L133).

Presubmit compiles the full GoReleaser binary matrix because `make build` does not filter the configured targets. It gives stronger functional treatment to only two artifacts. The build job uploads Linux amd64 for the SARIF job and Windows amd64 for a native Windows sanity test. See `.github/workflows/build.yaml` (https://github.com/stackrox/kube-linter/blob/main/.github/workflows/build.yaml#L87-L165). Darwin and both arm64 variants have no separate runtime assertion in the checked workflows.

The production workflow is narrow. `.github/workflows/release.yaml` (https://github.com/stackrox/kube-linter/blob/main/.github/workflows/release.yaml#L1-L21) triggers only on `v*` tags and grants publishing permissions. It logs into registries, installs signing and Buildx support, then invokes `goreleaser release`. See `.github/workflows/release.yaml` (https://github.com/stackrox/kube-linter/blob/main/.github/workflows/release.yaml#L23-L74). It has no pull request trigger, manual trigger, branch trigger, `act` condition, or snapshot branch.

The dry run is therefore a build command, not an alternate branch through the publishing graph. `make build` calls GoReleaser's snapshot build mode in `Makefile` (https://github.com/stackrox/kube-linter/blob/main/Makefile#L82-L89). The real release workflow always calls release mode in `.github/workflows/release.yaml` (https://github.com/stackrox/kube-linter/blob/main/.github/workflows/release.yaml#L65-L74).

There is one important version split. Local and presubmit builds install v2.17.0 from `tool-imports/goreleaser/go.mod` (https://github.com/stackrox/kube-linter/blob/main/tool-imports/goreleaser/go.mod#L1-L5). The release workflow asks `goreleaser/goreleaser-action` for `version: latest` in `.github/workflows/release.yaml` (https://github.com/stackrox/kube-linter/blob/main/.github/workflows/release.yaml#L65-L70). The checked snapshot and the publishing run therefore do not have a guaranteed tool-version identity.

## Relevance to Karta

C1 should be accepted. A dedicated `hack/release` Go module is the right boundary when helper code or a tool import exists only for release work. kube-linter shows that GoReleaser alone brings a large dependency graph in `tool-imports/goreleaser/go.mod` (https://github.com/stackrox/kube-linter/blob/main/tool-imports/goreleaser/go.mod). Karta should keep that graph out of its product module and give it an independent lock file.

Karta should improve the nested-module check boundary. kube-linter's generic `%.sum: %.mod` recipe runs `go mod tidy` and `go mod verify` without entering the matched module, and its CI diff check names only root `go.mod` and `go.sum`. See `Makefile` (https://github.com/stackrox/kube-linter/blob/main/Makefile#L1-L11). Karta should explicitly run tidy, verify, tests, and build from inside `hack/release`, then fail if that module's `go.mod` or `go.sum` changes.

C2 should be accepted for binary builds. kube-linter uses one GoReleaser configuration for local build flags, cross-compilation, release archives, images, signatures, and publishing. The presubmit calls the same config through `make build`. See `Makefile` (https://github.com/stackrox/kube-linter/blob/main/Makefile#L82-L89), `.goreleaser.yaml` (https://github.com/stackrox/kube-linter/blob/main/.goreleaser.yaml), and `.github/workflows/build.yaml` (https://github.com/stackrox/kube-linter/blob/main/.github/workflows/build.yaml#L44-L57). For Karta, one GoReleaser build definition should own both CLI and operator executables if both are released binaries.

GoReleaser should not own unrelated checks. kube-linter's workflow calls generation, lint, unit tests, and end-to-end tests as separate Make targets after the snapshot build. See `.github/workflows/build.yaml` (https://github.com/stackrox/kube-linter/blob/main/.github/workflows/build.yaml#L44-L78). Karta's `make check` can compose those concerns while using GoReleaser specifically for the normal binary-build step.

C3 should use separate workflow entrypoints. Keep Karta's real release entrypoint tag-only and publishing-capable. Put `act`, pull request, and branch dry runs in a non-publishing workflow with read-only permissions. Both can call the same reusable job, Make target, or script. kube-linter supports this direction by separating the snapshot build in `Makefile` (https://github.com/stackrox/kube-linter/blob/main/Makefile#L82-L89) from its tag-only release workflow (https://github.com/stackrox/kube-linter/blob/main/.github/workflows/release.yaml#L1-L21).

C4 should use an exact set contract. Karta should compare normalized `(goos, goarch)` tuples against the intentionally supported public set. List order should not matter, but missing, extra, and duplicate tuples should fail. Shape validation can still report malformed values or missing dimensions. kube-linter demonstrates why shape alone is weak: the configuration visibly intends six binaries in `.goreleaser.yaml` (https://github.com/stackrox/kube-linter/blob/main/.goreleaser.yaml#L13-L29), while workflow consumers select only Linux amd64 and Windows amd64 by exact generated paths in `.github/workflows/build.yaml` (https://github.com/stackrox/kube-linter/blob/main/.github/workflows/build.yaml#L87-L97).

An exact static contract and execution coverage solve different problems. The exact-set check prevents accidental support changes. A GoReleaser snapshot proves every tuple cross-compiles. Native smoke tests can remain selective. kube-linter uses that layered pattern in `.github/workflows/build.yaml` (https://github.com/stackrox/kube-linter/blob/main/.github/workflows/build.yaml#L44-L45) and its Linux and Windows consumer jobs in `.github/workflows/build.yaml` (https://github.com/stackrox/kube-linter/blob/main/.github/workflows/build.yaml#L99-L165), but it lacks an independent exact-set assertion.

## Evidence

- `go.mod` (https://github.com/stackrox/kube-linter/blob/main/go.mod) is the product module and does not import GoReleaser.
- `tool-imports/goreleaser/go.mod` (https://github.com/stackrox/kube-linter/blob/main/tool-imports/goreleaser/go.mod) isolates and pins GoReleaser.
- `tool-imports/goreleaser/go.sum` (https://github.com/stackrox/kube-linter/blob/main/tool-imports/goreleaser/go.sum) locks the separate release-tool graph.
- `tool-imports/goreleaser/tools.go` (https://github.com/stackrox/kube-linter/blob/main/tool-imports/goreleaser/tools.go) retains the tool dependency behind the `tools` build tag.
- `tool-imports/goreleaser/empty.go` (https://github.com/stackrox/kube-linter/blob/main/tool-imports/goreleaser/empty.go) keeps the package loadable without that tag.
- `.github/dependabot.yaml` (https://github.com/stackrox/kube-linter/blob/main/.github/dependabot.yaml) updates the product, lint-tool, and GoReleaser modules separately.
- `Makefile` (https://github.com/stackrox/kube-linter/blob/main/Makefile) installs pinned tools, runs the snapshot build, and wires the generated CLI into local end-to-end tests.
- `.goreleaser.yaml` (https://github.com/stackrox/kube-linter/blob/main/.goreleaser.yaml) owns the six binary targets, two image platforms, packaging, version flags, and signing.
- `.github/workflows/build.yaml` (https://github.com/stackrox/kube-linter/blob/main/.github/workflows/build.yaml) is the presubmit and regular build path. It exercises GoReleaser before the other checks.
- `.github/workflows/release.yaml` (https://github.com/stackrox/kube-linter/blob/main/.github/workflows/release.yaml) is the tag-only publishing path.
- `README.md` (https://github.com/stackrox/kube-linter/blob/main/README.md) documents `make build` as the developer build command.
- `RELEASE.md` (https://github.com/stackrox/kube-linter/blob/main/RELEASE.md) documents the human release process, but its tag advice conflicts with the current workflow trigger.

## Lessons for Karta

1. Put GoReleaser and release-only helper dependencies in `hack/release/go.mod` and `hack/release/go.sum`.
2. Install the pinned tool from inside that module into a repository-local tool directory.
3. Add a dedicated nested-module check. Do not assume root `go test ./...`, tidy, or verify crosses a module boundary.
4. Let GoReleaser own ordinary CLI and operator executable builds, cross-build flags, artifact naming, archives, and release upload.
5. Make `make check` invoke a GoReleaser snapshot or build-only target. Keep generation, lint, and tests as explicit sibling checks.
6. Use the same pinned GoReleaser version in local builds, presubmit, and production release.
7. Keep the production workflow tag-only. Put `act` accommodations in a separate non-publishing entrypoint and share the implementation below it.
8. Express the public platform contract as an exact unordered tuple set. Validate matrix shape as an additional error-quality check.
9. Cross-compile the complete set in focused presubmit. Run native behavior tests only where they add value.
10. Keep release documentation and workflow tag patterns under an automated consistency check.

## What NOT to copy

- Do not use `version: latest` in production while presubmit uses a module-pinned GoReleaser. kube-linter has that version split between `tool-imports/goreleaser/go.mod` (https://github.com/stackrox/kube-linter/blob/main/tool-imports/goreleaser/go.mod#L1-L5) and `.github/workflows/release.yaml` (https://github.com/stackrox/kube-linter/blob/main/.github/workflows/release.yaml#L65-L70).
- Do not use a nested-module dependency target that runs tidy and verify only from the repository root. kube-linter's recipe does so in `Makefile` (https://github.com/stackrox/kube-linter/blob/main/Makefile#L1-L11).
- Do not treat successful amd64 consumer jobs as an exact release-matrix assertion. kube-linter declares arm64 and Darwin outputs but does not separately consume them in `.github/workflows/build.yaml` (https://github.com/stackrox/kube-linter/blob/main/.github/workflows/build.yaml#L87-L165).
- Do not put branch-only `act` conditions beside registry login, signing, or release upload. kube-linter's production release workflow (https://github.com/stackrox/kube-linter/blob/main/.github/workflows/release.yaml) stays free of such branches.
- Do not let release documentation drift from workflow triggers. `RELEASE.md` (https://github.com/stackrox/kube-linter/blob/main/RELEASE.md#L5-L18) says not to prefix tags with `v`, while `.github/workflows/release.yaml` (https://github.com/stackrox/kube-linter/blob/main/.github/workflows/release.yaml#L3-L6) accepts only `v*` tags.
- Do not copy the duplicate cosign installation in `.github/workflows/release.yaml` (https://github.com/stackrox/kube-linter/blob/main/.github/workflows/release.yaml#L23-L53).
