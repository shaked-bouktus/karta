<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 NVIDIA Corporation -->

# cert-manager: release pipeline boundaries

## TL;DR

- C1: Separate Karta's internal `hack/release` Go code when it imports release-only libraries. cert-manager has no checked-in release-helper module. It gets equivalent dependency isolation by pinning the external `cmrel` command and installing it with `GOWORK=off`. Karta should use a nested module because its helper source lives in the repository.
- C2: Do not make GoReleaser the owner of normal CLI or operator builds, or of the main presubmit. cert-manager does not use GoReleaser for builds or releases. Make owns direct Go builds, container assembly, checks, and release staging. GoReleaser is only present as an unused tool pin inherited from shared Make machinery.
- C3: Keep `act` branches out of the credentialed production release workflow. cert-manager has no `act` convention. Its safe local path is an unsigned Make target. Its signed upload path is invoked by a tag-triggered Cloud Build configuration. Share lower-level commands, but separate the entrypoints.
- C4: Enforce Karta's exact public release platform set, not only matrix shape. cert-manager treats Linux on `amd64`, `arm64`, `s390x`, `ppc64le`, and 32-bit ARM v7 as a concrete artifact contract across binaries, images, bundles, and metadata.

## How it works

cert-manager is a multi-module Go repository. The root product module is declared in `go.mod` (https://github.com/cert-manager/cert-manager/blob/master/go.mod#L1-L9). Each shipped process has a binary-specific module. For example, the controller module replaces the root module with the local checkout in `cmd/controller/go.mod` (https://github.com/cert-manager/cert-manager/blob/master/cmd/controller/go.mod#L1-L19). The end-to-end test suite also has its own dependency graph in `test/e2e/go.mod` (https://github.com/cert-manager/cert-manager/blob/master/test/e2e/go.mod#L1-L20).

There is no `hack/release/go.mod` or other checked-in release-helper module. Release-only Go tooling is provisioned as executable dependencies instead. The shared tool file pins both GoReleaser and `cmrel` in `make/_shared/tools/00_mod.mk` (https://github.com/cert-manager/cert-manager/blob/master/make/_shared/tools/00_mod.mk#L158-L178). It maps those names to their Go package paths in the same file (https://github.com/cert-manager/cert-manager/blob/master/make/_shared/tools/00_mod.mk#L399-L431). Its generic installer runs `go install package@version` with `GOWORK=off` and a temporary `GOBIN`, then moves the executable into a versioned tool cache. See `make/_shared/tools/00_mod.mk` (https://github.com/cert-manager/cert-manager/blob/master/make/_shared/tools/00_mod.mk#L457-L472). This keeps tool dependencies out of the product modules without maintaining another source module in this repository.

The repository is deliberate about covering its real nested modules. `generate-go-mod-tidy` discovers every `go.mod` outside generated tool content and enters each module. The lint target does the same. See `make/_shared/go/01_mod.mk` (https://github.com/cert-manager/cert-manager/blob/master/make/_shared/go/01_mod.mk#L43-L63) and `make/_shared/go/01_mod.mk` (https://github.com/cert-manager/cert-manager/blob/master/make/_shared/go/01_mod.mk#L126-L140). `verify-modules` calls `cmrel validate-gomod`, and is registered in the aggregate verification path in `make/ci.mk` (https://github.com/cert-manager/cert-manager/blob/master/make/ci.mk#L15-L19). Tests explicitly enter the binary and integration modules rather than assuming root `./...` covers them. See `make/test.mk` (https://github.com/cert-manager/cert-manager/blob/master/make/test.mk#L40-L62).

Normal builds are Make-owned direct Go builds. `server-binaries` selects the component targets. The controller target spells out five Linux outputs and invokes `go build` once per architecture with explicit `GOOS`, `GOARCH`, and `GOARM=7` for 32-bit ARM. See `make/server.mk` (https://github.com/cert-manager/cert-manager/blob/master/make/server.mk#L15-L37). The same pattern is repeated for the other controller-side processes in `make/server.mk` (https://github.com/cert-manager/cert-manager/blob/master/make/server.mk#L39-L109).

Presubmit is also Make-owned. `ci-presubmit` delegates to the aggregate `verify` target in `make/ci.mk` (https://github.com/cert-manager/cert-manager/blob/master/make/ci.mk#L85-L90). The aggregate builds its dependency list from generation and verification targets, and verifies generated files in a temporary workspace in `make/_shared/generate-verify/02_mod.mk` (https://github.com/cert-manager/cert-manager/blob/master/make/_shared/generate-verify/02_mod.mk#L23-L47). Go tests are a separate CI lane. `test-ci` covers the root packages, selected binary modules, integration tests, third-party code, and live DNS tests in `make/test.mk` (https://github.com/cert-manager/cert-manager/blob/master/make/test.mk#L40-L62).

GoReleaser is not a build or release owner in cert-manager. The local tree has no GoReleaser configuration. The only GoReleaser references are the version pin and generic package mapping in `make/_shared/tools/00_mod.mk` (https://github.com/cert-manager/cert-manager/blob/master/make/_shared/tools/00_mod.mk#L161-L163) and `make/_shared/tools/00_mod.mk` (https://github.com/cert-manager/cert-manager/blob/master/make/_shared/tools/00_mod.mk#L415-L418). No cert-manager Make target invokes the generated GoReleaser variable or prerequisite. This is shared tool capability, not evidence of use.

There is also no normal CLI release in this repository. `RELEASE.md` says that `cmctl` moved to its own repository after cert-manager 1.15. Current cert-manager releases contain component images and a Helm chart. See `RELEASE.md` (https://github.com/cert-manager/cert-manager/blob/master/RELEASE.md#L11-L17). This makes cert-manager a controller-oriented comparison, not a precedent for Karta's CLI artifact implementation.

The safe release check and the real release path share Make internals but have different entrypoints. `release-artifacts` builds binaries, the chart, containers, and unsigned manifests for local validation. The file says it is useful for checking all platforms and is not the actual release command. `release-artifacts-signed`, `release`, and `upload-release` add signing, metadata, and GCS upload. See `make/release.mk` (https://github.com/cert-manager/cert-manager/blob/master/make/release.mk#L28-L67).

The production orchestration is not a GitHub Actions release workflow. A tag-triggered Cloud Build configuration vendors the pinned Go toolchain and runs `make ... upload-release` with signing and bucket settings. It stages the output but does not publish automatically. See `gcb/build_cert_manager.yaml` (https://github.com/cert-manager/cert-manager/blob/master/gcb/build_cert_manager.yaml#L1-L25). That job uses a dedicated service account and release storage in `gcb/build_cert_manager.yaml` (https://github.com/cert-manager/cert-manager/blob/master/gcb/build_cert_manager.yaml#L32-L43). The local unsigned target does not need those credentials.

The repository has no `act` branch or local GitHub Actions release emulation. Its checked-in GitHub workflows cover periodic vulnerability scanning and scorecards, not release orchestration. The vulnerability workflow invokes a Make verification target in `.github/workflows/govulncheck.yaml` (https://github.com/cert-manager/cert-manager/blob/master/.github/workflows/govulncheck.yaml#L1-L28). The absence of `act` means cert-manager supports only the broader separation principle: keep a safe build-only entrypoint away from the credential-bearing release trigger.

The production platform set is exact. `ARCHS` names `amd64 arm64 s390x ppc64le arm`, while `BINS` names all five shipped processes in `make/containers.mk` (https://github.com/cert-manager/cert-manager/blob/master/make/containers.mk#L15-L25). Every component image target requires all five architecture-specific outputs. See `make/containers.mk` (https://github.com/cert-manager/cert-manager/blob/master/make/containers.mk#L51-L117). Release bundles and their metadata also enumerate those exact five architectures in `make/release.mk` (https://github.com/cert-manager/cert-manager/blob/master/make/release.mk#L78-L114).

The exact set is enforced structurally by dependencies, not by a standalone matrix assertion test. The release target cannot complete unless each enumerated binary, image, bundle, and metadata file exists. cert-manager also has an experimental `ko` path whose platform input is flexible and defaults to `linux/amd64` in `make/ko.mk` (https://github.com/cert-manager/cert-manager/blob/master/make/ko.mk#L15-L46). That experimental convenience does not define the production release contract.

## Relevance to Karta

C1 should be accepted when Karta's helper imports release-only libraries. cert-manager demonstrates the desired boundary without a local helper module: third-party release commands are exact-version executables installed with `GOWORK=off`. Karta cannot use that exact pattern for checked-in Go source. A nested `hack/release` module is the direct way to prevent its parser, changelog, GitHub, or GoReleaser support libraries from entering Karta's product dependency graph. Karta's root check must explicitly tidy, lint, build, and test that module. cert-manager's repository-wide module loops show why module creation and module coverage are one decision.

C2 should be rejected as a universal ownership rule. cert-manager gives GoReleaser no execution role at all. Direct Go builds are the normal developer path. Make verification and separate Go test targets form presubmit. Make also owns production cross-compilation and packaging. For Karta, the useful lesson is to keep one clear owner per artifact boundary. If Karta retains GoReleaser, give it the release-parity CLI cross-build, archive, checksum, and release metadata work. Keep ordinary local CLI and operator compilation, unit tests, generation checks, lint, and container builds behind Make and standard Go or image tooling. A GoReleaser config check or non-publishing snapshot can be one presubmit check without becoming the whole presubmit.

C3 should use separate entry workflows with shared lower-level implementation. cert-manager's `release-artifacts` and `upload-release` targets reuse the same graph, but only the latter is reached by the tagged, credentialed Cloud Build path. Karta should put `act` event shims, fake tags, and non-publishing behavior in a separate workflow. That workflow can call the same reusable workflow or Make target as production with a hard-coded publish-disabled contract. The production workflow should remain small, tag-oriented, and free from local-runner conditionals around signing, provenance, or upload.

C4 should use an exact unordered set contract for public release platforms. cert-manager's five Linux architectures flow through the whole artifact graph. Removing one changes what users can pull and what release bundles exist. Karta should compare the configured GoReleaser tuples with its declared supported set, reject duplicates, and treat order as irrelevant. A shape check remains useful for schema rules such as valid OS names, valid architecture names, and required binary IDs. It is not enough to catch an accidental deletion or addition. Cheap `act` execution may use one representative tuple, while a static presubmit assertion protects the full exact set.

## Evidence

- `go.mod` (https://github.com/cert-manager/cert-manager/blob/master/go.mod) is the root product dependency graph.
- `cmd/controller/go.mod` (https://github.com/cert-manager/cert-manager/blob/master/cmd/controller/go.mod) is one of five binary-specific modules that imports the local root module.
- `test/e2e/go.mod` (https://github.com/cert-manager/cert-manager/blob/master/test/e2e/go.mod) isolates end-to-end test dependencies.
- `make/_shared/tools/00_mod.mk` (https://github.com/cert-manager/cert-manager/blob/master/make/_shared/tools/00_mod.mk) pins tools and installs Go tools outside repository workspaces.
- `make/_shared/go/01_mod.mk` (https://github.com/cert-manager/cert-manager/blob/master/make/_shared/go/01_mod.mk) discovers nested modules for tidy, lint, and vulnerability checks.
- `make/server.mk` (https://github.com/cert-manager/cert-manager/blob/master/make/server.mk) directly compiles the controller-side binaries for the production Linux architecture set.
- `make/containers.mk` (https://github.com/cert-manager/cert-manager/blob/master/make/containers.mk) builds all component images for the exact five-architecture set.
- `make/ci.mk` (https://github.com/cert-manager/cert-manager/blob/master/make/ci.mk) makes `verify` the non-test presubmit and validates module layout with `cmrel`.
- `make/test.mk` (https://github.com/cert-manager/cert-manager/blob/master/make/test.mk) explicitly tests the root and selected nested modules.
- `make/release.mk` (https://github.com/cert-manager/cert-manager/blob/master/make/release.mk) separates unsigned local artifacts from signed release creation and upload.
- `gcb/build_cert_manager.yaml` (https://github.com/cert-manager/cert-manager/blob/master/gcb/build_cert_manager.yaml) is the tagged, credential-bearing release build entrypoint.
- `RELEASE.md` (https://github.com/cert-manager/cert-manager/blob/master/RELEASE.md) identifies images and the Helm chart as current artifacts and says the CLI moved out.
- `make/ko.mk` (https://github.com/cert-manager/cert-manager/blob/master/make/ko.mk) is a flexible experimental image path, not the production matrix definition.

## Lessons for Karta

1. Isolate release-only imported dependencies from the root product module.
2. Give every nested module an explicit root-level tidy, lint, build, and test path.
3. Pin third-party Go tools by exact version and install them with workspace isolation.
4. Keep ordinary product builds and the main presubmit independent of release tags, signing keys, and upload credentials.
5. Give GoReleaser a narrow role if Karta uses it: release-parity binary artifacts and their metadata.
6. Reuse build primitives between dry runs and production, but use separate safe and credentialed entrypoints.
7. Assert the exact public platform tuple set statically. Run a smaller representative set only when execution cost demands it.
8. Treat a platform change as a release compatibility change that must update config, tests, and documentation together.

## What NOT to copy

- Do not copy cert-manager's GoReleaser setup as proof that GoReleaser is integrated. The checked-in pin is unused in this repository.
- Do not copy the whole multi-module layout. Binary-specific modules solve cert-manager's dependency and packaging needs, not Karta's release-helper decision by themselves.
- Do not place `act` conditionals inside production signing and upload steps. cert-manager supplies no evidence that this is safe.
- Do not confuse the experimental, configurable `ko` platform input with the exact production release set.
- Do not duplicate an exact matrix in several files without a drift check. cert-manager enforces its list through Make dependencies, but the architecture names are still repeated.
- Do not assume a root `go test ./...` covers nested modules. cert-manager explicitly enters modules because it does not.
