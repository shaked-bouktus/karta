<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 NVIDIA Corporation -->

# Installing the Karta CLI

Karta publishes CLI archives for Linux and macOS on AMD64 and ARM64. The CLI and
operator use the same version as the root Karta Go module.

## Homebrew on macOS

```bash
brew install --cask run-ai/tap/karta
karta --version
```

## GitHub Release archive

Choose the archive that matches `uname -s` and `uname -m`:

| System | Architecture | Archive suffix |
|---|---|---|
| Linux | x86_64 | `linux_amd64.tar.gz` |
| Linux | aarch64 or arm64 | `linux_arm64.tar.gz` |
| macOS | x86_64 | `darwin_amd64.tar.gz` |
| macOS | arm64 | `darwin_arm64.tar.gz` |

For release `1.2.3`, download the matching
`karta_1.2.3_<os>_<arch>.tar.gz` file and `checksums.txt` from the
[GitHub Release](https://github.com/run-ai/karta/releases/tag/v1.2.3). Verify the
archive before extracting it:

```bash
grep '  karta_1.2.3_linux_amd64.tar.gz$' checksums.txt | sha256sum -c -
tar -xzf karta_1.2.3_linux_amd64.tar.gz
install -m 0755 karta "$HOME/.local/bin/karta"
karta --version
```

On macOS, use the matching Darwin archive and verify it with `shasum`:

```bash
grep '  karta_1.2.3_darwin_arm64.tar.gz$' checksums.txt | shasum -a 256 -c -
```

Each archive contains `karta`, `LICENSE`, `NOTICE`, `README.md`, and
`THIRD_PARTY_LICENSES`.

## Go installation

The nested CLI module can also be installed directly:

```bash
go install github.com/run-ai/karta/cli@v1.2.3
```

The Go tool names that executable `cli`, based on the final module path. It
reports the same product version from Go build information:

```bash
cli --version
```

Rename the executable to `karta` if the release archive and Homebrew command
name are preferred.

## Version output

A tagged `v1.2.3` build reports `1.2.3`. A development build from `main`
reports `0.0.0-main-<short-commit>`. A normal local Make build reports the
value from the release-tag-filtered command:

```bash
git describe --tags --always --dirty --match 'v[0-9]*.[0-9]*.[0-9]*'
```
