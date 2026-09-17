// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 NVIDIA Corporation

package main

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Release validation", func() {
	Describe("module versions", func() {
		It("accepts synchronized inline requirements", func() {
			root := GinkgoT().TempDir()
			writeModuleFiles(root, "require "+rootModule+" v1.2.3\n")
			Expect(validateModuleVersions(root, "1.2.3")).To(Succeed())
		})

		It("accepts synchronized block requirements", func() {
			root := GinkgoT().TempDir()
			writeModuleFiles(root, "require (\n\t"+rootModule+" v1.2.3\n)\n")
			Expect(validateModuleVersions(root, "1.2.3")).To(Succeed())
		})

		It("validates modules from an explicit repository root", func() {
			root := GinkgoT().TempDir()
			writeModuleFiles(root, "require "+rootModule+" v1.2.3\n")
			Expect(runValidateVersion([]string{"--root", root, "--version", "1.2.3"})).To(Succeed())
		})

		DescribeTable("rejects publication-unsafe module files",
			func(body, message string) {
				root := GinkgoT().TempDir()
				writeModuleFiles(root, body)
				Expect(validateModuleVersions(root, "1.2.3")).To(MatchError(ContainSubstring(message)))
			},
			Entry("mismatched root version",
				"require "+rootModule+" v1.2.2\n", "v1.2.2, want v1.2.3"),
			Entry("single-line replacement",
				"require "+rootModule+" v1.2.3\nreplace "+rootModule+" => ../\n", "replace directive"),
			Entry("block replacement after the requirement",
				"require "+rootModule+" v1.2.3\nreplace (\n\t"+rootModule+" => ../\n)\n", "replace directive"),
			Entry("block replacement before the requirement",
				"replace (\n\t"+rootModule+" => ../\n)\nrequire "+rootModule+" v1.2.3\n", "replace directive"),
			Entry("exclusion",
				"exclude "+rootModule+" v1.2.2\nrequire "+rootModule+" v1.2.3\n", "exclude directive"),
		)
	})

	It("validates synchronized tags from an explicit repository root", func() {
		root := GinkgoT().TempDir()
		Expect(os.WriteFile(filepath.Join(root, "tracked"), []byte("contents"), 0o644)).To(Succeed())
		commands := [][]string{
			{"init"},
			{"add", "tracked"},
			{"-c", "user.name=Karta Test", "-c", "user.email=karta@example.com", "-c", "commit.gpgsign=false", "commit", "-m", "test"},
			{"-c", "tag.gpgSign=false", "tag", "v1.2.3"},
			{"-c", "tag.gpgSign=false", "tag", "cli/v1.2.3"},
			{"-c", "tag.gpgSign=false", "tag", "operator/v1.2.3"},
		}
		for _, args := range commands {
			_, err := commandOutput("git", append([]string{"-C", root}, args...)...)
			Expect(err).NotTo(HaveOccurred())
		}
		Expect(validateTags(root, "1.2.3")).To(Succeed())
	})

	Describe("operator artifacts", func() {
		validArtifacts := func() []artifact {
			return []artifact{
				{Name: "karta-operator", Path: "amd64/karta-operator", Goos: "linux", Goarch: "amd64", Type: "Binary", Extra: map[string]any{"ID": "karta-operator"}},
				{Name: "karta-operator", Path: "arm64/karta-operator", Goos: "linux", Goarch: "arm64", Type: "Binary", Extra: map[string]any{"ID": "karta-operator"}},
				{Name: "karta", Path: "cli/karta", Goos: "linux", Goarch: "amd64", Type: "Binary", Extra: map[string]any{"ID": "karta"}},
			}
		}

		It("selects the exact operator matrix", func() {
			got, err := operatorArtifacts(validArtifacts())
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(HaveLen(2))
			Expect([]string{got[0].Goarch, got[1].Goarch}).To(Equal([]string{"amd64", "arm64"}))
		})

		It("rejects a missing platform", func() {
			_, err := operatorArtifacts(validArtifacts()[:1])
			Expect(err).To(MatchError(ContainSubstring("found 1 operator binaries, want 2")))
		})

		It("rejects an extra platform", func() {
			artifacts := append(validArtifacts(), artifact{
				Name: "karta-operator", Path: "s390x/karta-operator", Goos: "linux", Goarch: "s390x", Type: "Binary", Extra: map[string]any{"ID": "karta-operator"},
			})
			_, err := operatorArtifacts(artifacts)
			Expect(err).To(MatchError(ContainSubstring("found 3 operator binaries, want 2")))
		})

		It("rejects a renamed executable", func() {
			artifacts := validArtifacts()
			artifacts[0].Path = "amd64/operator"
			_, err := operatorArtifacts(artifacts)
			Expect(err).To(MatchError(ContainSubstring("unexpected filename")))
		})
	})

	It("resolves GoReleaser artifact paths from the project root", func() {
		root := GinkgoT().TempDir()
		dist := filepath.Join(root, "dist")
		Expect(os.MkdirAll(dist, 0o755)).To(Succeed())
		contents := `[{"name":"karta","path":"dist/karta_linux_amd64/karta","type":"Binary"}]`
		Expect(os.WriteFile(filepath.Join(dist, "artifacts.json"), []byte(contents), 0o644)).To(Succeed())

		artifacts, err := readArtifacts(dist)
		Expect(err).NotTo(HaveOccurred())
		Expect(artifacts).To(HaveLen(1))
		Expect(artifacts[0].Path).To(Equal(filepath.Join(dist, "karta_linux_amd64", "karta")))
	})

	Describe("artifact contents", func() {
		It("accepts the exact CLI archive surface", func() {
			path := filepath.Join(GinkgoT().TempDir(), "karta.tar.gz")
			writeArchive(path, []string{"karta", "LICENSE", "NOTICE", "README.md", "THIRD_PARTY_LICENSES"})
			Expect(verifyArchive(path)).To(Succeed())
		})

		It("rejects an archive without NOTICE", func() {
			path := filepath.Join(GinkgoT().TempDir(), "karta.tar.gz")
			writeArchive(path, []string{"karta", "LICENSE", "README.md", "THIRD_PARTY_LICENSES"})
			Expect(verifyArchive(path)).To(MatchError(ContainSubstring("archive contains")))
		})

		It("rejects an extra public file", func() {
			path := filepath.Join(GinkgoT().TempDir(), "karta.tar.gz")
			writeArchive(path, []string{"karta", "LICENSE", "NOTICE", "README.md", "THIRD_PARTY_LICENSES", "karta-operator"})
			Expect(verifyArchive(path)).To(MatchError(ContainSubstring("archive contains")))
		})

		It("verifies the checksum manifest exactly", func() {
			directory := GinkgoT().TempDir()
			archives := map[string]artifact{}
			var manifest strings.Builder
			for name, contents := range map[string]string{"one.tar.gz": "one", "two.tar.gz": "two"} {
				path := filepath.Join(directory, name)
				Expect(os.WriteFile(path, []byte(contents), 0o644)).To(Succeed())
				digest, err := fileSHA256(path)
				Expect(err).NotTo(HaveOccurred())
				fmt.Fprintf(&manifest, "%s  %s\n", digest, name)
				archives[name] = artifact{Name: name, Path: path}
			}
			manifestPath := filepath.Join(directory, "checksums.txt")
			Expect(os.WriteFile(manifestPath, []byte(manifest.String()), 0o644)).To(Succeed())
			Expect(verifyChecksums(manifestPath, archives)).To(Succeed())

			Expect(os.WriteFile(manifestPath, []byte("deadbeef  one.tar.gz\n"), 0o644)).To(Succeed())
			Expect(verifyChecksums(manifestPath, archives)).To(MatchError(ContainSubstring("covers 1 files, want 2")))
		})

		It("verifies the Homebrew Cask URLs and checksums", func() {
			directory := GinkgoT().TempDir()
			archives := map[string]artifact{}
			var cask strings.Builder
			cask.WriteString("version \"1.2.3\"\n")
			for _, arch := range []string{"amd64", "arm64"} {
				name := "karta_1.2.3_darwin_" + arch + ".tar.gz"
				path := filepath.Join(directory, name)
				Expect(os.WriteFile(path, []byte(arch), 0o644)).To(Succeed())
				digest, err := fileSHA256(path)
				Expect(err).NotTo(HaveOccurred())
				fmt.Fprintf(&cask, "sha256 \"%s\"\n", digest)
				fmt.Fprintf(&cask, "url \"https://github.com/run-ai/karta/releases/download/v#{version}/karta_#{version}_darwin_%s.tar.gz\"\n", arch)
				archives[name] = artifact{Name: name, Path: path}
			}
			path := filepath.Join(directory, "karta.rb")
			Expect(os.WriteFile(path, []byte(cask.String()), 0o644)).To(Succeed())
			Expect(verifyCask(path, "1.2.3", archives)).To(Succeed())

			Expect(os.WriteFile(path, []byte("version \"1.2.3\"\n"), 0o644)).To(Succeed())
			Expect(verifyCask(path, "1.2.3", archives)).To(MatchError(ContainSubstring("does not use the URL and checksum")))
		})
	})

	It("copies an executable without changing its bytes", func() {
		directory := GinkgoT().TempDir()
		source := filepath.Join(directory, "source")
		destination := filepath.Join(directory, "nested", "destination")
		Expect(os.WriteFile(source, []byte("operator bytes"), 0o644)).To(Succeed())
		Expect(copyFile(source, destination)).To(Succeed())
		contents, err := os.ReadFile(destination)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(contents)).To(Equal("operator bytes"))
		info, err := os.Stat(destination)
		Expect(err).NotTo(HaveOccurred())
		Expect(info.Mode().Perm()).To(Equal(os.FileMode(0o755)))
	})

	It("verifies host executable versions", func() {
		path := filepath.Join(GinkgoT().TempDir(), "version-command")
		Expect(os.WriteFile(path, []byte("#!/bin/sh\nprintf '1.2.3\\n'\n"), 0o755)).To(Succeed())
		artifacts := []artifact{
			{Path: path, Goos: runtime.GOOS, Goarch: runtime.GOARCH, Type: "Binary", Extra: map[string]any{"ID": "karta"}},
			{Path: path, Goos: runtime.GOOS, Goarch: runtime.GOARCH, Type: "Binary", Extra: map[string]any{"ID": "karta-operator"}},
		}
		verified, skipped, err := verifyHostVersions(artifacts, "1.2.3")
		Expect(err).NotTo(HaveOccurred())
		Expect(verified).To(Equal([]string{"karta", "karta-operator"}))
		Expect(skipped).To(BeEmpty())

		_, _, err = verifyHostVersions(artifacts, "1.2.4")
		Expect(err).To(MatchError(ContainSubstring("want \"1.2.4\"")))

		verified, skipped, err = verifyHostVersions(artifacts[:1], "1.2.3")
		Expect(err).NotTo(HaveOccurred())
		Expect(verified).To(Equal([]string{"karta"}))
		Expect(skipped).To(Equal([]string{"karta-operator"}))
	})
})

func writeModuleFiles(root, body string) {
	GinkgoHelper()
	for _, module := range []string{"cli", "operator"} {
		Expect(os.MkdirAll(filepath.Join(root, module), 0o755)).To(Succeed())
		contents := "module " + rootModule + "/" + module + "\n\ngo 1.26.3\n\n" + body
		Expect(os.WriteFile(filepath.Join(root, module, "go.mod"), []byte(contents), 0o644)).To(Succeed())
	}
}

func writeArchive(path string, names []string) {
	GinkgoHelper()
	file, err := os.Create(path)
	Expect(err).NotTo(HaveOccurred())
	gzipWriter := gzip.NewWriter(file)
	tarWriter := tar.NewWriter(gzipWriter)
	for _, name := range names {
		contents := []byte(name)
		Expect(tarWriter.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(contents)), Typeflag: tar.TypeReg})).To(Succeed())
		_, err := tarWriter.Write(contents)
		Expect(err).NotTo(HaveOccurred())
	}
	Expect(tarWriter.Close()).To(Succeed())
	Expect(gzipWriter.Close()).To(Succeed())
	Expect(file.Close()).To(Succeed())
}
