// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 NVIDIA Corporation

// Command release validates and stages Karta release artifacts.
package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"

	"golang.org/x/mod/modfile"
)

const rootModule = "github.com/dsx-ai-factory/workload-map"

var semanticVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

type artifact struct {
	Name   string         `json:"name"`
	Path   string         `json:"path"`
	Goos   string         `json:"goos"`
	Goarch string         `json:"goarch"`
	Type   string         `json:"type"`
	Extra  map[string]any `json:"extra"`
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "release:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("a subcommand is required")
	}
	switch args[0] {
	case "validate-version":
		return runValidateVersion(args[1:])
	case "stage-operator":
		return runStageOperator(args[1:])
	case "operator-platforms":
		return runOperatorPlatforms(args[1:])
	case "verify-artifacts":
		return runVerifyArtifacts(args[1:])
	case "verify-operator-images":
		return runVerifyOperatorImages(args[1:])
	case "worktree-fingerprint":
		return runWorktreeFingerprint(args[1:])
	default:
		return fmt.Errorf("unknown subcommand %q", args[0])
	}
}

func runValidateVersion(args []string) error {
	flags := flag.NewFlagSet("validate-version", flag.ContinueOnError)
	root := flags.String("root", ".", "repository root")
	version := flags.String("version", "", "normalized product version")
	requireTags := flags.Bool("require-tags", false, "require all synchronized tags at HEAD")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if !semanticVersion.MatchString(*version) {
		return fmt.Errorf("version %q must match X.Y.Z without a leading v", *version)
	}
	if err := validateModuleVersions(*root, *version); err != nil {
		return err
	}
	if *requireTags {
		if err := validateTags(*root, *version); err != nil {
			return err
		}
	}
	fmt.Printf("synchronized module version v%s is valid\n", *version)
	return nil
}

func validateModuleVersions(root, version string) error {
	want := "v" + version
	for _, moduleFile := range []string{"cli/go.mod", "operator/go.mod"} {
		contents, err := os.ReadFile(filepath.Join(root, moduleFile))
		if err != nil {
			return err
		}
		file, err := modfile.Parse(moduleFile, contents, nil)
		if err != nil {
			return fmt.Errorf("parse %s: %w", moduleFile, err)
		}
		if len(file.Replace) != 0 {
			return fmt.Errorf("%s contains a publication-unsafe replace directive", moduleFile)
		}
		if len(file.Exclude) != 0 {
			return fmt.Errorf("%s contains a publication-unsafe exclude directive", moduleFile)
		}
		got := ""
		for _, requirement := range file.Require {
			if requirement.Mod.Path == rootModule {
				got = requirement.Mod.Version
				break
			}
		}
		if got != want {
			return fmt.Errorf("%s requires %s %s, want %s", moduleFile, rootModule, got, want)
		}
	}
	return nil
}

func validateTags(root, version string) error {
	head, err := commandOutput("git", "-C", root, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	for _, tag := range []string{"v" + version, "cli/v" + version, "operator/v" + version} {
		commit, err := commandOutput("git", "-C", root, "rev-list", "-n", "1", tag)
		if err != nil {
			return fmt.Errorf("resolve tag %s: %w", tag, err)
		}
		if commit != head {
			return fmt.Errorf("tag %s points to %s, want HEAD %s", tag, commit, head)
		}
	}
	return nil
}

func runStageOperator(args []string) error {
	flags := flag.NewFlagSet("stage-operator", flag.ContinueOnError)
	dist := flags.String("dist", "dist", "GoReleaser dist directory")
	out := flags.String("out", "dist/operator-image", "image context directory")
	if err := flags.Parse(args); err != nil {
		return err
	}
	artifacts, err := readArtifacts(*dist)
	if err != nil {
		return err
	}
	operator, err := operatorArtifacts(artifacts)
	if err != nil {
		return err
	}
	for _, item := range operator {
		destination := filepath.Join(*out, item.Goos, item.Goarch, "karta-operator")
		if err := copyFile(item.Path, destination); err != nil {
			return err
		}
		fmt.Printf("staged %s\n", destination)
	}
	return nil
}

func runOperatorPlatforms(args []string) error {
	flags := flag.NewFlagSet("operator-platforms", flag.ContinueOnError)
	dist := flags.String("dist", "dist", "GoReleaser dist directory")
	if err := flags.Parse(args); err != nil {
		return err
	}
	artifacts, err := readArtifacts(*dist)
	if err != nil {
		return err
	}
	operator, err := operatorArtifacts(artifacts)
	if err != nil {
		return err
	}
	platforms := make([]string, 0, len(operator))
	for _, item := range operator {
		platforms = append(platforms, item.Goos+"/"+item.Goarch)
	}
	fmt.Println(strings.Join(platforms, ","))
	return nil
}

func readArtifacts(dist string) ([]artifact, error) {
	contents, err := os.ReadFile(filepath.Join(dist, "artifacts.json"))
	if err != nil {
		return nil, err
	}
	var artifacts []artifact
	if err := json.Unmarshal(contents, &artifacts); err != nil {
		return nil, fmt.Errorf("decode artifacts.json: %w", err)
	}
	return artifacts, nil
}

func operatorArtifacts(artifacts []artifact) ([]artifact, error) {
	var result []artifact
	for _, item := range artifacts {
		if item.Type == "Binary" && extraID(item) == "karta-operator" {
			result = append(result, item)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Goos+"/"+result[i].Goarch < result[j].Goos+"/"+result[j].Goarch
	})
	want := []string{"linux/amd64", "linux/arm64"}
	if len(result) != len(want) {
		return nil, fmt.Errorf("found %d operator binaries, want %d", len(result), len(want))
	}
	for i, item := range result {
		if got := item.Goos + "/" + item.Goarch; got != want[i] {
			return nil, fmt.Errorf("operator platform %q, want %q", got, want[i])
		}
		if filepath.Base(item.Path) != "karta-operator" {
			return nil, fmt.Errorf("operator artifact %q has unexpected filename", item.Path)
		}
	}
	return result, nil
}

func extraID(item artifact) string {
	value, _ := item.Extra["ID"].(string)
	return value
}

func copyFile(source, destination string) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(output, input); err != nil {
		_ = output.Close()
		return err
	}
	return output.Close()
}

func runVerifyArtifacts(args []string) error {
	flags := flag.NewFlagSet("verify-artifacts", flag.ContinueOnError)
	dist := flags.String("dist", "dist", "GoReleaser dist directory")
	version := flags.String("version", "", "expected normalized version")
	if err := flags.Parse(args); err != nil {
		return err
	}
	artifacts, err := readArtifacts(*dist)
	if err != nil {
		return err
	}
	if _, err := operatorArtifacts(artifacts); err != nil {
		return err
	}
	expected := expectedArchiveNames(*version)
	archives := map[string]artifact{}
	for _, item := range artifacts {
		if item.Type != "Archive" {
			continue
		}
		if extraID(item) != "karta" {
			return fmt.Errorf("unexpected public archive %s from %s", item.Name, extraID(item))
		}
		archives[item.Name] = item
	}
	if len(archives) != len(expected) {
		return fmt.Errorf("found %d CLI archives, want %d", len(archives), len(expected))
	}
	for name := range expected {
		item, ok := archives[name]
		if !ok {
			return fmt.Errorf("missing archive %s", name)
		}
		if err := verifyArchive(item.Path); err != nil {
			return fmt.Errorf("verify %s: %w", name, err)
		}
	}
	if err := verifyChecksums(filepath.Join(*dist, "checksums.txt"), archives); err != nil {
		return err
	}
	if err := verifyCask(filepath.Join(*dist, "homebrew", "Casks", "karta.rb"), *version, archives); err != nil {
		return err
	}
	verified, skipped, err := verifyHostVersions(artifacts, *version)
	if err != nil {
		return err
	}
	fmt.Printf("verified four CLI archives, checksums, and two internal operator binaries for %s\n", *version)
	if len(verified) != 0 {
		fmt.Printf("verified host executable versions: %s\n", strings.Join(verified, ", "))
	}
	for _, id := range skipped {
		fmt.Printf("skipped %s --version: no %s/%s artifact\n", id, runtime.GOOS, runtime.GOARCH)
	}
	return nil
}

func verifyCask(path, version string, archives map[string]artifact) error {
	contents, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	cask := string(contents)
	if !strings.Contains(cask, `version "`+version+`"`) {
		return fmt.Errorf("homebrew Cask does not declare version %s", version)
	}
	for _, arch := range []string{"amd64", "arm64"} {
		name := "karta_" + version + "_darwin_" + arch + ".tar.gz"
		item, ok := archives[name]
		if !ok {
			return fmt.Errorf("homebrew Cask archive %s is missing", name)
		}
		digest, err := fileSHA256(item.Path)
		if err != nil {
			return err
		}
		url := "https://github.com/run-ai/karta/releases/download/v#{version}/karta_#{version}_darwin_" + arch + ".tar.gz"
		if !strings.Contains(cask, `sha256 "`+digest+`"`) || !strings.Contains(cask, `url "`+url+`"`) {
			return fmt.Errorf("homebrew Cask does not use the URL and checksum for %s", name)
		}
	}
	return nil
}

func expectedArchiveNames(version string) map[string]struct{} {
	result := map[string]struct{}{}
	for _, platform := range []string{"linux_amd64", "linux_arm64", "darwin_amd64", "darwin_arm64"} {
		result["karta_"+version+"_"+platform+".tar.gz"] = struct{}{}
	}
	return result
}

func verifyArchive(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		return err
	}
	defer gzipReader.Close()
	entries := map[string]struct{}{}
	tarReader := tar.NewReader(gzipReader)
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if header.Typeflag == tar.TypeReg {
			entries[header.Name] = struct{}{}
		}
	}
	want := []string{"LICENSE", "NOTICE", "README.md", "THIRD_PARTY_LICENSES", "karta"}
	if len(entries) != len(want) {
		return fmt.Errorf("archive contains %v, want %v", sortedKeys(entries), want)
	}
	for _, name := range want {
		if _, ok := entries[name]; !ok {
			return fmt.Errorf("archive is missing %s", name)
		}
	}
	return nil
}

func sortedKeys(values map[string]struct{}) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func verifyChecksums(path string, archives map[string]artifact) error {
	contents, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	checksums := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(contents)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return fmt.Errorf("invalid checksum line %q", line)
		}
		checksums[fields[1]] = fields[0]
	}
	if len(checksums) != len(archives) {
		return fmt.Errorf("checksums.txt covers %d files, want %d", len(checksums), len(archives))
	}
	for name, item := range archives {
		want, ok := checksums[name]
		if !ok {
			return fmt.Errorf("checksums.txt is missing %s", name)
		}
		got, err := fileSHA256(item.Path)
		if err != nil {
			return err
		}
		if got != want {
			return fmt.Errorf("checksum for %s is %s, want %s", name, got, want)
		}
	}
	return nil
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func verifyHostVersions(artifacts []artifact, version string) ([]string, []string, error) {
	var verified []string
	var skipped []string
	for _, id := range []string{"karta", "karta-operator"} {
		var path string
		for _, item := range artifacts {
			if item.Type == "Binary" && extraID(item) == id && item.Goos == runtime.GOOS && item.Goarch == runtime.GOARCH {
				path = item.Path
				break
			}
		}
		if path == "" {
			skipped = append(skipped, id)
			continue
		}
		output, err := commandOutput(path, "--version")
		if err != nil {
			return nil, nil, fmt.Errorf("run %s --version: %w", id, err)
		}
		if output != version {
			return nil, nil, fmt.Errorf("%s --version = %q, want %q", id, output, version)
		}
		verified = append(verified, id)
	}
	return verified, skipped, nil
}

func runVerifyOperatorImages(args []string) error {
	flags := flag.NewFlagSet("verify-operator-images", flag.ContinueOnError)
	dist := flags.String("dist", "dist", "GoReleaser dist directory")
	image := flags.String("image", "", "base image reference without architecture suffix")
	containerTool := flags.String("container-tool", "docker", "container CLI")
	if err := flags.Parse(args); err != nil {
		return err
	}
	artifacts, err := readArtifacts(*dist)
	if err != nil {
		return err
	}
	operator, err := operatorArtifacts(artifacts)
	if err != nil {
		return err
	}
	for _, item := range operator {
		if err := verifyOperatorImage(item, *image+"-"+item.Goarch, *containerTool); err != nil {
			return err
		}
	}
	return nil
}

func verifyOperatorImage(binary artifact, image, containerTool string) error {
	architecture, err := commandOutput(containerTool, "image", "inspect", "--format", "{{.Architecture}}", image)
	if err != nil {
		return err
	}
	if architecture != binary.Goarch {
		return fmt.Errorf("image %s architecture is %s, want %s", image, architecture, binary.Goarch)
	}
	containerName := fmt.Sprintf("karta-operator-identity-%s-%d", binary.Goarch, os.Getpid())
	if _, err := commandOutput(containerTool, "create", "--platform", binary.Goos+"/"+binary.Goarch, "--name", containerName, image); err != nil {
		return err
	}
	defer func() {
		_ = exec.Command(containerTool, "rm", "-f", containerName).Run()
	}()
	extracted := filepath.Join(os.TempDir(), containerName)
	defer os.Remove(extracted)
	if _, err := commandOutput(containerTool, "cp", containerName+":/karta-operator", extracted); err != nil {
		return err
	}
	binaryDigest, err := fileSHA256(binary.Path)
	if err != nil {
		return err
	}
	imageDigest, err := fileSHA256(extracted)
	if err != nil {
		return err
	}
	if binaryDigest != imageDigest {
		return fmt.Errorf("%s binary digest %s differs from image digest %s", binary.Goarch, binaryDigest, imageDigest)
	}
	fmt.Printf("verified %s %s\n", binary.Goarch, binaryDigest)
	return nil
}

func commandOutput(name string, args ...string) (string, error) {
	output, err := commandOutputBytes(name, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}

func commandOutputBytes(name string, args ...string) ([]byte, error) {
	output, err := exec.Command(name, args...).Output()
	if err != nil {
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			stderr := strings.TrimSpace(string(exitError.Stderr))
			if stderr != "" {
				return nil, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, stderr)
			}
		}
		return nil, fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return output, nil
}

func runWorktreeFingerprint(args []string) error {
	flags := flag.NewFlagSet("worktree-fingerprint", flag.ContinueOnError)
	root := flags.String("root", ".", "repository root")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("worktree-fingerprint does not accept arguments")
	}
	fingerprint, err := worktreeFingerprint(*root)
	if err != nil {
		return err
	}
	fmt.Println(fingerprint)
	return nil
}

func worktreeFingerprint(root string) (string, error) {
	output, err := commandOutputBytes("git", "-C", root, "ls-files", "--cached", "--others", "--exclude-standard", "-z")
	if err != nil {
		return "", fmt.Errorf("list worktree files: %w", err)
	}
	return fingerprintPaths(root, nulSeparatedPaths(output))
}

func nulSeparatedPaths(output []byte) []string {
	paths := strings.Split(strings.TrimSuffix(string(output), "\x00"), "\x00")
	if len(paths) == 1 && paths[0] == "" {
		return nil
	}
	return paths
}

func fingerprintPaths(root string, paths []string) (string, error) {
	paths = append([]string(nil), paths...)
	sort.Strings(paths)
	hash := sha256.New()
	for _, path := range paths {
		fullPath := filepath.Join(root, filepath.FromSlash(path))
		info, err := os.Lstat(fullPath)
		if errors.Is(err, os.ErrNotExist) {
			if _, err := fmt.Fprintf(hash, "deleted\x00%s\x00", path); err != nil {
				return "", err
			}
			continue
		}
		if err != nil {
			return "", fmt.Errorf("inspect %s: %w", path, err)
		}
		if _, err := fmt.Fprintf(hash, "%s\x00%s\x00", path, info.Mode()); err != nil {
			return "", err
		}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(fullPath)
			if err != nil {
				return "", fmt.Errorf("read symlink %s: %w", path, err)
			}
			if _, err := io.WriteString(hash, target); err != nil {
				return "", err
			}
		case info.Mode().IsRegular():
			file, err := os.Open(fullPath)
			if err != nil {
				return "", fmt.Errorf("open %s: %w", path, err)
			}
			_, copyErr := io.Copy(hash, file)
			closeErr := file.Close()
			if copyErr != nil {
				return "", fmt.Errorf("hash %s: %w", path, copyErr)
			}
			if closeErr != nil {
				return "", fmt.Errorf("close %s: %w", path, closeErr)
			}
		}
		if _, err := hash.Write([]byte{0}); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
