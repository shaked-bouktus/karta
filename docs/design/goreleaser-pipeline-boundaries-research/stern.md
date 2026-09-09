<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 NVIDIA Corporation -->

# Stern: GoReleaser pipeline boundaries

## TL;DR

- C1: Separate Karta's internal `hack/release` module when it has release-only Go dependencies. Stern avoids a second module because its third-party tools are executables installed at exact versions, not imported libraries. Its product-aware README helper stays in the product module because it imports `cmd` (`Makefile`, `hack/update-readme/update-readme.go`, `go.mod`).
- C2: Give GoReleaser ownership of release artifacts and the release-parity snapshot, not every normal build or all presubmit work. Stern uses `go build` for the local binary, `go test` plus lint for code checks, and GoReleaser for cross-build and distribution paths (`Makefile`).
- C3: Keep `act` dry-run branches out of the real release workflow. Stern's release workflow is tag-only. Its safe dry run is a separate CI call to a non-publishing Make target (`.github/workflows/release.yaml`, `.github/workflows/ci.yaml`, `Makefile`).
- C4: Enforce release matrix shape in presubmit, not every exact platform pair. Stern's CI snapshot keeps the Unix and Windows build groups and their archive paths, but reduces their OS and architecture lists. The full exact matrix remains declarative in `.goreleaser.yaml` (`Makefile`, `.goreleaser.yaml`).

## How it works

Stern has one Go module. `go.mod` contains the application dependency graph. The Makefile pins GoReleaser, golangci-lint, the Krew validator, and yq as versioned `go install package@version` commands. Their binaries go under `hack/tools/bin`, which `.gitignore` excludes. This isolates tool resolution from the application module without a tools module (`go.mod`, `Makefile`, `.gitignore`).

The Go code under `hack/update-readme` is different. It imports `github.com/stern/stern/cmd` and dependencies already used by the product. `make update-readme` runs that source in the root module. Stern therefore follows an import-boundary rule in practice: a product-aware generator shares the product module, while standalone third-party tools are installed as pinned executables (`hack/update-readme/update-readme.go`, `Makefile`).

The Makefile exposes distinct build lanes (`Makefile`):

- `build` invokes `go build -o dist/stern .`.
- `test` depends on lint and then invokes `go test -v ./...`.
- `build-cross` invokes `goreleaser build --snapshot --clean`.
- `dist` rewrites the GoReleaser config in memory, then runs a clean, non-publishing snapshot release.
- `dist-all` runs the full config as a clean, non-publishing snapshot release.
- `release` runs the real clean release and skips GoReleaser validation.

The checked-in GoReleaser config owns release metadata and artifact construction. It injects version data through linker flags. It defines separate `unix` and `windows` build IDs. It then maps those build IDs into tar and zip archives, checksums, container tags, a Krew manifest, and a WinGet update (`.goreleaser.yaml`, `cmd/flag_version.go`).

Presubmit composes the lanes instead of choosing one owner for everything. CI tidies the module, verifies generated README content, runs lint and tests, builds the native binary, runs the reduced GoReleaser distribution snapshot, and validates the generated Krew manifest (`.github/workflows/ci.yaml`, `Makefile`, `hack/verify-readme.sh`).

The release workflow does less. A `v*` tag triggers checkout, Go setup, registry login, `make release`, and a Krew index update. It has no pull request branch, `workflow_dispatch`, dry-run input, or `act` branch. The repository's other two workflows cover normal CI and Markdown-only skipped CI (`.github/workflows/release.yaml`, `.github/workflows/ci.yaml`, `.github/workflows/ci_for_skipped.yaml`).

The full platform declaration is exact in `.goreleaser.yaml`. The Unix build lists Linux and Darwin with amd64, arm, and arm64. The Windows build lists Windows with amd64 and arm64. The `dist` target changes the Unix OS list to Linux and changes every build architecture list to amd64 before invoking GoReleaser. This still exercises both build IDs. It also exercises the tar and Windows zip archive branches. It does not rebuild every declared release pair (`.goreleaser.yaml`, `Makefile`).

## Relevance to Karta

C1 should be decided from dependency direction, not directory naming. Stern's `hack/` directory does not automatically imply a module. The README helper belongs to the product graph because it imports product command code. The downloaded release tools avoid that graph through exact-version `go install` commands (`hack/update-readme/update-readme.go`, `Makefile`, `go.mod`). For Karta, an internal release helper with release-only library imports should use a separate module. A thin helper that must compile against product packages should remain in the corresponding product module. GoReleaser itself should stay an installed tool rather than becoming a product-module dependency.

C2 should use split ownership. Normal developer builds and unit checks should remain direct Go targets. GoReleaser should own the cross-platform binaries, linker metadata, archives, checksums, images, and publication graph. Presubmit should call a non-publishing GoReleaser snapshot in addition to native build and tests (`Makefile`, `.goreleaser.yaml`, `.github/workflows/ci.yaml`). This catches release configuration breakage without making routine Go feedback depend entirely on the release engine.

C3 should use a separate dry-run workflow or the normal CI workflow. Stern keeps the real workflow's trigger and permissions surface small: only version tags reach registry login and `make release`. The dry run is expressed as `make dist`, which is snapshot-only and skips publishing, and CI invokes it outside the release workflow (`.github/workflows/release.yaml`, `.github/workflows/ci.yaml`, `Makefile`). Karta can preserve the same Make target and GoReleaser config while giving `act` its own synthetic triggers.

C4 should validate shape in the fast presubmit lane. At minimum, every Karta GoReleaser build ID and materially different package path should survive the reduction. Exact platforms should stay explicit and reviewable in `.goreleaser.yaml`. A separate full snapshot can run less often. Stern's `dist` and `dist-all` split demonstrates this boundary (`Makefile`, `.goreleaser.yaml`). Add an exact-matrix assertion only if Karta treats each OS and architecture pair as a compatibility promise that must not change without an intentional test update. Stern does not make that stronger contract.

## Evidence

- `Makefile` - Pins all external build tools. Separates native build, tests, reduced snapshot distribution, full snapshot distribution, and publication. https://github.com/stern/stern/blob/master/Makefile
- `go.mod` - Holds the single product dependency graph. It does not declare GoReleaser, yq, golangci-lint, or the Krew validator as module requirements. https://github.com/stern/stern/blob/master/go.mod
- `.gitignore` - Excludes `hack/tools/bin` and `dist`, the tool-install and artifact outputs used by the Makefile. https://github.com/stern/stern/blob/master/.gitignore
- `hack/update-readme/update-readme.go` - Shows a hack helper that deliberately imports Stern's `cmd` package and runs in the root module. https://github.com/stern/stern/blob/master/hack/update-readme/update-readme.go
- `hack/verify-readme.sh` - Verifies generated README content through `make update-readme` without modifying the checked-in README. https://github.com/stern/stern/blob/master/hack/verify-readme.sh
- `.goreleaser.yaml` - Defines the exact release build groups, platform lists, linker metadata, archives, checksums, image tags, Krew output, and WinGet update. https://github.com/stern/stern/blob/master/.goreleaser.yaml
- `cmd/flag_version.go` - Defines the default version variables that GoReleaser replaces through linker flags. https://github.com/stern/stern/blob/master/cmd/flag_version.go
- `.github/workflows/ci.yaml` - Runs native checks and build, then the reduced `make dist` snapshot and Krew validation in presubmit. https://github.com/stern/stern/blob/master/.github/workflows/ci.yaml
- `.github/workflows/release.yaml` - Uses only a `v*` tag trigger. It runs the publishing target with release credentials and then updates Krew. https://github.com/stern/stern/blob/master/.github/workflows/release.yaml
- `.github/workflows/ci_for_skipped.yaml` - Handles Markdown-only changes separately and confirms that dry-run release branches are not hidden in an alternate release workflow. https://github.com/stern/stern/blob/master/.github/workflows/ci_for_skipped.yaml

## Lessons for Karta

1. Separate dependency graphs when imported libraries differ. Do not create a module only because code lives under `hack/` (`go.mod`, `hack/update-readme/update-readme.go`).
2. Pin executable tools at the installation boundary and keep generated binaries out of Git (`Makefile`, `.gitignore`).
3. Preserve direct `go build` and `go test` paths for fast, ordinary feedback. Add a GoReleaser snapshot as a release-contract check (`Makefile`, `.github/workflows/ci.yaml`).
4. Make the tag workflow boring. It should publish from the same checked-in GoReleaser config already exercised without publication (`.github/workflows/release.yaml`, `.github/workflows/ci.yaml`, `.goreleaser.yaml`).
5. Reduce costly presubmit matrices without erasing distinct build and packaging branches. Keep a full snapshot target available (`Makefile`, `.goreleaser.yaml`).
6. Treat exact platform membership as release policy. Treat build IDs, archive kinds, and artifact wiring as the minimum presubmit contract (`.goreleaser.yaml`, `Makefile`).

## What NOT to copy

- Do not copy Stern's lack of a separate module if Karta's release helper imports release-only libraries. Stern has no equivalent custom release dependency graph (`go.mod`, `Makefile`).
- Do not make the real release workflow serve local `act` branch events. Stern provides no precedent for that coupling. Its release path is tag-only (`.github/workflows/release.yaml`).
- Do not interpret the reduced snapshot as exact matrix enforcement. It deliberately removes Darwin and non-amd64 architectures before the snapshot (`Makefile`, `.goreleaser.yaml`).
- Do not let matrix reduction remove an entire build ID or package format. Stern retains Unix and Windows builds plus tar and zip paths in its reduced snapshot (`Makefile`, `.goreleaser.yaml`).
- Do not copy `release --skip=validate` unless Karta has a clearly enforced validation gate before tags can publish. Stern's tag workflow itself does not rerun tests or the snapshot before `make release` (`Makefile`, `.github/workflows/release.yaml`).
- Do not infer that one monolithic CI job is ideal for Karta. Stern is a single-binary CLI. Its workflow size does not prove the same layout fits a multi-binary CLI and operator (`.github/workflows/ci.yaml`, `.goreleaser.yaml`).
