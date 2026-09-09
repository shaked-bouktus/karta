<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 NVIDIA Corporation -->

# GoReleaser pipeline boundaries: what the panel does, and what Karta should do

## Purpose and method

This decision covers four questions from the issue 208 review:

- C1: whether `hack/release` should have its own Go module.
- C2: whether GoReleaser should own normal executable builds and presubmit builds.
- C3: whether local `act` branches should remain in the production release workflow.
- C4: whether Karta should enforce the exact release platform set or only its shape.

The council inspected six local checkouts. It used no network access and did not
fetch or update any repository. The panel contains Helm, Argo CD, Stern,
kubeconform, kube-linter, and cert-manager. These projects cover mature CLIs,
multi-binary systems, Kubernetes controllers, and both large and small
GoReleaser pipelines.

The council is evidence, not a vote. A common external pattern can still be a
poor fit when it conflicts with Karta's explicit product requirements.

## Comparison matrix

| Project | Release dependency boundary | Normal build owner | Safe release rehearsal | Platform contract |
|---|---|---|---|---|
| Helm | Installs GoReleaser outside the product module | Direct `go build`; GoReleaser for cross-builds | Real tag and canary publication only | Exact 12-target set across config, installer, and release notes |
| Argo CD | Separate Go module for a release-note helper | Direct Make builds; GoReleaser for CLI release artifacts | Separate non-publishing image workflow | Exact seven CLI tuples, with representative smoke builds |
| Stern | Pinned executable tools outside the product graph | Direct native build; reduced and full GoReleaser snapshots | Snapshot target in CI, tag-only release workflow | Full matrix in config, reduced matrix in presubmit |
| kubeconform | Separate module for an auxiliary release input | Direct development build and GoReleaser presubmit snapshot | Snapshot build before the tag-gated publishing job | Dimension-based binary matrix and exact container tuples |
| kube-linter | Dedicated GoReleaser tool module | GoReleaser owns the normal CLI build and runs in presubmit | Snapshot Make target, tag-only release workflow | Exact six binary tuples and exact image tuples |
| cert-manager | Release commands installed outside product modules | Direct Make builds, no active GoReleaser path | Unsigned local artifacts and separate credentialed upload | Exact five-architecture production set |

Representative evidence:

- Helm `Makefile` separates native and GoReleaser cross-builds:
  https://github.com/helm/helm/blob/main/Makefile
- Argo CD isolates its release helper:
  https://github.com/argoproj/argo-cd/blob/master/hack/get-previous-release/go.mod
- Stern has reduced and full snapshot targets:
  https://github.com/stern/stern/blob/master/Makefile
- kubeconform crosses a nested module boundary from one GoReleaser config:
  https://github.com/yannh/kubeconform/blob/master/.goreleaser.yml
- kube-linter isolates and pins GoReleaser in a tool module:
  https://github.com/stackrox/kube-linter/blob/main/tool-imports/goreleaser/go.mod
- cert-manager discovers and checks nested modules explicitly:
  https://github.com/cert-manager/cert-manager/blob/master/make/_shared/go/01_mod.mk

## Options, ranked

### C1: release helper module

1. Give `hack/release` its own module and explicit checks.
   This prevents release-helper imports from becoming direct dependencies of
   Karta's public root module. Argo CD, kubeconform, kube-linter, and
   cert-manager all show that isolation must be paired with explicit
   nested-module checks. Effort is low. The main risk is forgetting that root
   `go test ./...` does not cross the boundary.
2. Keep the helper in the root module.
   This has fewer files, but it exposes release-only dependency choices as
   public library dependency choices. It is a poor fit for Karta.

Decision: implement option 1 now. Add `hack/release/go.mod` and `go.sum`, keep
the module outside `go.work` so workspace MVS cannot override its pins, remove
the direct `x/mod` requirement from the root module, and make the root presubmit
test the helper explicitly.

### C2: executable build ownership

1. Keep GoReleaser as Karta's one executable compiler path.
   kube-linter proves this model can serve local builds and presubmit. It avoids
   two independent copies of build flags, version linker flags, and target
   definitions. The cost is a larger tool download and slower first build.
2. Use direct `go build` locally and reserve GoReleaser for release artifacts.
   Helm, Argo CD, Stern, and cert-manager prefer this split. It is faster, but it
   conflicts with Karta's requirement that GoReleaser own both released
   executable builds and would reintroduce duplicate build definitions.

Decision: keep option 1. GoReleaser owns CLI and operator compilation. Make
continues to own lint, tests, generation, and orchestration. The library remains
a Go package and is compiled by its normal Go tests.

### C3: production workflow and local act rehearsal

1. Use separate workflow entrypoints with shared lower-level build logic.
   Every panel project keeps non-publishing checks outside its credentialed
   release entrypoint. This is the cleaner long-term trust boundary.
2. Keep the current `ACT` branches in `push-artifacts.yaml` for issue 208.
   This directly rehearses the workflow being changed. It has the production
   file, but avoids introducing a reusable-workflow abstraction before the
   first production run supplies real GitHub evidence.

Decision: keep option 2 for issue 208. Revisit option 1 after the first real
release workflow run. Any later split must preserve one shared Make and
GoReleaser implementation and must keep the dry-run entrypoint unable to
publish.

### C4: release platform validation

1. Enforce the exact unordered tuple set and also validate its shape.
   Helm, Argo CD, kube-linter, and cert-manager treat platforms as public
   compatibility commitments. Missing, extra, or duplicate tuples should fail.
   List order should not matter.
2. Validate only matrix shape.
   Stern and kubeconform support reduced presubmit execution. Shape-only static
   validation is cheaper to maintain, but it permits an architecture to vanish
   silently.

Decision: keep option 1. Karta promises CLI archives for Linux and macOS on
AMD64 and ARM64, plus Linux operator images on AMD64 and ARM64. The exact tests
protect that promise. Runtime smoke tests may still use only host-compatible
artifacts.

## Decision for Karta

- Implement C1 as a separate checked release-helper module.
- Keep C2 unchanged. GoReleaser remains the compiler path for both executables.
- Keep C3 unchanged in issue 208. Record the workflow split as a later cleanup,
  not as a release prerequisite.
- Keep C4 unchanged. Preserve exact unordered platform assertions.

This combination follows the strongest dependency-isolation and platform-safety
lessons without discarding Karta's explicit single-build-owner and local `act`
requirements.

## What NOT to do

- Do not create a nested module without adding it to presubmit.
- Do not add a second set of CLI or operator build flags to make local builds
  superficially faster.
- Do not move `act` into a separate workflow that duplicates production logic.
- Do not let a shape-only test redefine the supported release platforms.
- Do not copy unpinned GoReleaser installations or `latest` release actions.
- Do not add signing, SBOM, or provenance behavior as part of these decisions.

## Per-repo index

- `./goreleaser-pipeline-boundaries-research/helm.md`
- `./goreleaser-pipeline-boundaries-research/argo-cd.md`
- `./goreleaser-pipeline-boundaries-research/stern.md`
- `./goreleaser-pipeline-boundaries-research/kubeconform.md`
- `./goreleaser-pipeline-boundaries-research/kube-linter.md`
- `./goreleaser-pipeline-boundaries-research/cert-manager.md`
