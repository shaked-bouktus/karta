<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 NVIDIA Corporation -->

# kubeconform: GoReleaser pipeline boundaries

## TL;DR

kubeconform gives Karta a useful split of responsibilities.

- C1: Separate `hack/release` into its own Go module if it contains release-only code or dependencies. kubeconform gives its auxiliary `openapi2jsonschema` program its own `go.mod`, `go.sum`, vendor tree, Makefile, tests, and CI job. The root module stays focused on the shipped library and CLI.
- C2: Keep GoReleaser as the owner of normal CLI and operator release builds. Keep direct `go build` targets for local development. Exercise the GoReleaser build in presubmit with a snapshot and one representative target.
- C3: Keep `act` simulation outside the real release workflow. kubeconform has no `act` branches. One workflow runs ordinary verification on every push and gates the publishing job only on version tags.
- C4: Validate matrix shape by default. kubeconform declares OS and architecture dimensions, while its presubmit compiles one target. Use an exact tuple assertion only if Karta promises a fixed public platform set.

## How it works

The root module is the public kubeconform codebase. Its `go.mod` (https://github.com/yannh/kubeconform/blob/master/go.mod#L1-L17) contains the validator dependencies. The auxiliary OpenAPI converter has a separate `openapi2jsonschema-go/go.mod` (https://github.com/yannh/kubeconform/blob/master/openapi2jsonschema-go/go.mod#L1-L5) with only `gopkg.in/yaml.v3`. Both modules have their own vendor metadata. The root `vendor/modules.txt` (https://github.com/yannh/kubeconform/blob/master/vendor/modules.txt) and nested `openapi2jsonschema-go/vendor/modules.txt` (https://github.com/yannh/kubeconform/blob/master/openapi2jsonschema-go/vendor/modules.txt) do not mix dependencies.

There is no root `go.work`. The root `Makefile` (https://github.com/yannh/kubeconform/blob/master/Makefile#L7-L15) runs `go test ./...` and `go build ./...` from the root module. Those commands do not include the nested module. The nested `openapi2jsonschema-go/Makefile` (https://github.com/yannh/kubeconform/blob/master/openapi2jsonschema-go/Makefile#L7-L37) owns that module's test, build, acceptance, and dependency-update commands.

GoReleaser crosses the module boundary deliberately. The first build points at `./cmd/kubeconform`. The second build sets `dir: ./openapi2jsonschema-go` and `main: .` in `.goreleaser.yml` (https://github.com/yannh/kubeconform/blob/master/.goreleaser.yml#L2-L49). The release archive selects only the `kubeconform` build in the archive configuration (https://github.com/yannh/kubeconform/blob/master/.goreleaser.yml#L50-L57). The converter remains available to container assembly because both runtime Dockerfiles copy both binaries: `Dockerfile` (https://github.com/yannh/kubeconform/blob/master/Dockerfile#L12-L15) and `Dockerfile-alpine` (https://github.com/yannh/kubeconform/blob/master/Dockerfile-alpine#L10-L13).

The root Makefile states the ownership rule directly. Docker build targets are development helpers. Release artifacts and images are produced by GoReleaser. The presubmit wrapper invokes the pinned GoReleaser container with `build --clean --single-target --snapshot`, then copies the Linux amd64 kubeconform binary for acceptance tests. The publishing wrapper invokes `goreleaser release --clean`. See `Makefile` (https://github.com/yannh/kubeconform/blob/master/Makefile#L17-L39).

The main workflow has three jobs in `.github/workflows/main.yml` (https://github.com/yannh/kubeconform/blob/master/.github/workflows/main.yml#L1-L58). One tests kubeconform, builds it through the GoReleaser snapshot path, and runs acceptance tests. A separate job tests and builds the nested converter from its own directory. The final GoReleaser job depends on both verification jobs and runs only for `refs/tags/v...`. It then configures QEMU and Buildx, logs in to GHCR, and calls the same `make release` wrapper.

The release matrix has two levels. Both Go builds list `windows`, `linux`, and `darwin` as OS dimensions and `amd64`, `arm`, and `arm64` as architecture dimensions in `.goreleaser.yml` (https://github.com/yannh/kubeconform/blob/master/.goreleaser.yml#L2-L49). The configuration does not enumerate an allowlist of OS and architecture tuples. Container builds are narrower and explicit. Four Docker entries cover Linux amd64 and arm64 for scratch and Alpine images, and four manifests join those exact variants in `.goreleaser.yml` (https://github.com/yannh/kubeconform/blob/master/.goreleaser.yml#L59-L117).

## Relevance to Karta

C1 should be decided by dependency and lifecycle ownership. A Karta `hack/release` helper is analogous to kubeconform's converter when it is used to prepare or assemble releases but is not imported by the CLI or operator. A nested module prevents release tooling from changing the product modules' dependency graph. It also makes test and dependency update commands explicit. The separate module should still be wired into GoReleaser when its binary is an input to a released image or artifact, as kubeconform does with the converter in `.goreleaser.yml` (https://github.com/yannh/kubeconform/blob/master/.goreleaser.yml#L26-L49).

C2 should preserve one production build authority. Karta's direct Go build targets can stay fast and convenient for development. GoReleaser should define release entrypoints, build flags, version injection, archive selection, images, and checksums. Presubmit should call a snapshot build through that configuration. kubeconform's `Makefile` (https://github.com/yannh/kubeconform/blob/master/Makefile#L10-L39) demonstrates this boundary. Its workflow (https://github.com/yannh/kubeconform/blob/master/.github/workflows/main.yml#L10-L17) proves that the release-configured binary can feed later acceptance tests.

C3 should keep simulation mechanics separate. kubeconform does not alter release behavior for `act`. Its production workflow has a simple event contract: verification runs on push, while publishing is tag-gated in `.github/workflows/main.yml` (https://github.com/yannh/kubeconform/blob/master/.github/workflows/main.yml#L1-L42). The only manual dispatch in the repository belongs to the independent site workflow in `.github/workflows/site.yml` (https://github.com/yannh/kubeconform/blob/master/.github/workflows/site.yml#L1-L8). Karta should use a separate `act` workflow or local harness if local simulation needs fake events, reduced permissions, or skipped publishing. That keeps production conditions reviewable.

C4 should distinguish configuration shape from product policy. A shape check can require builds for both Karta binaries, non-empty OS and architecture dimensions, static build settings, archives, checksums, and container targets. That matches kubeconform's declarative dimensions and single-target presubmit. An exact tuple test is justified only when the supported release platforms are a compatibility promise. In that case, Karta should enumerate and test its own promised tuples rather than infer them from separate `goos` and `goarch` lists.

## Evidence

- Module separation: `go.mod` (https://github.com/yannh/kubeconform/blob/master/go.mod#L1-L17), `openapi2jsonschema-go/go.mod` (https://github.com/yannh/kubeconform/blob/master/openapi2jsonschema-go/go.mod#L1-L5), and `openapi2jsonschema-go/vendor/modules.txt` (https://github.com/yannh/kubeconform/blob/master/openapi2jsonschema-go/vendor/modules.txt).
- Independent local paths: root `Makefile` (https://github.com/yannh/kubeconform/blob/master/Makefile#L7-L15) and nested `openapi2jsonschema-go/Makefile` (https://github.com/yannh/kubeconform/blob/master/openapi2jsonschema-go/Makefile#L7-L40).
- GoReleaser build ownership: `Makefile` (https://github.com/yannh/kubeconform/blob/master/Makefile#L17-L39) and `.goreleaser.yml` (https://github.com/yannh/kubeconform/blob/master/.goreleaser.yml#L1-L49).
- Presubmit and publication gates: `.github/workflows/main.yml` (https://github.com/yannh/kubeconform/blob/master/.github/workflows/main.yml#L4-L58).
- Archive and checksum contracts: `.goreleaser.yml` (https://github.com/yannh/kubeconform/blob/master/.goreleaser.yml#L50-L57) and `.goreleaser.yml` (https://github.com/yannh/kubeconform/blob/master/.goreleaser.yml#L119-L123).
- Container platform contract: `.goreleaser.yml` (https://github.com/yannh/kubeconform/blob/master/.goreleaser.yml#L59-L117), `Dockerfile` (https://github.com/yannh/kubeconform/blob/master/Dockerfile#L4-L15), and `Dockerfile-alpine` (https://github.com/yannh/kubeconform/blob/master/Dockerfile-alpine#L1-L13).
- Separate auxiliary tests: `.github/workflows/main.yml` (https://github.com/yannh/kubeconform/blob/master/.github/workflows/main.yml#L19-L35), `openapi2jsonschema-go/Dockerfile.bats` (https://github.com/yannh/kubeconform/blob/master/openapi2jsonschema-go/Dockerfile.bats#L1-L15), and `openapi2jsonschema-go/acceptance.bats` (https://github.com/yannh/kubeconform/blob/master/openapi2jsonschema-go/acceptance.bats#L1-L81).

## Lessons for Karta

1. Put release-only Go dependencies in a nested module. Give it its own lock data, tests, and dependency-update path.
2. Let GoReleaser own the reproducible product build. Let Make expose thin development, presubmit, and release wrappers.
3. Run a cheap GoReleaser snapshot in presubmit. Use one representative target when the purpose is configuration and integration validation.
4. Gate publication after all product and release-helper checks. Do not let a tag bypass ordinary verification.
5. Keep `act` accommodations out of production release conditions. Test local simulation through a separate entrypoint.
6. Treat binary dimensions and container tuples as different contracts. Containers often support a narrower set.
7. Assert exact release tuples only when they are promised to users. Otherwise assert the required matrix shape and let the GoReleaser configuration remain the source of truth.

## What NOT to copy

- Do not copy a nested module without also copying its separate CI ownership. Root `go test ./...` does not cover it.
- Do not assume a single-target snapshot proves every release platform. kubeconform's presubmit validates one Linux amd64 path in `Makefile` (https://github.com/yannh/kubeconform/blob/master/Makefile#L34-L36).
- Do not infer an exact binary tuple set from separate OS and architecture lists. The kubeconform file specifies dimensions, not an explicit tuple allowlist.
- Do not duplicate release image construction in ad hoc Make targets. kubeconform reserves image publication for GoReleaser in `Makefile` (https://github.com/yannh/kubeconform/blob/master/Makefile#L17-L39).
- Do not package every helper binary into every archive. kubeconform builds the converter for image assembly but restricts archives to the user-facing kubeconform build in `.goreleaser.yml` (https://github.com/yannh/kubeconform/blob/master/.goreleaser.yml#L26-L57).
- Do not add local-runner branches to a security-sensitive publish job. Keep the tag and dependency gates as direct as kubeconform's release job (https://github.com/yannh/kubeconform/blob/master/.github/workflows/main.yml#L37-L58).
