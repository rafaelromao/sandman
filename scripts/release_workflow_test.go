package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestReleaseBootstrapUsesCuratedChangelogAndExactInitialVersion(t *testing.T) {
	config := readRepositoryFile(t, "../release-please-config.json")
	var releaseConfig struct {
		Packages map[string]struct {
			ChangelogPath string `json:"changelog-path"`
			ReleaseAs     string `json:"release-as"`
		} `json:"packages"`
	}
	if err := json.Unmarshal([]byte(config), &releaseConfig); err != nil {
		t.Fatalf("parse release-please-config.json: %v", err)
	}

	root := releaseConfig.Packages["."]
	if root.ChangelogPath != "CHANGELOG.md" {
		t.Fatalf("bootstrap changelog path = %q, want CHANGELOG.md", root.ChangelogPath)
	}
	if root.ReleaseAs != "1.0.0" {
		t.Fatalf("bootstrap release-as = %q, want 1.0.0", root.ReleaseAs)
	}

	manifest := readRepositoryFile(t, "../.release-please-manifest.json")
	var versions map[string]string
	if err := json.Unmarshal([]byte(manifest), &versions); err != nil {
		t.Fatalf("parse .release-please-manifest.json: %v", err)
	}
	if len(versions) != 1 || versions["."] != "1.0.0" {
		t.Fatalf("bootstrap manifest = %#v, want {\".\": \"1.0.0\"}", versions)
	}
}

func TestReleaseBootstrapPreservesCuratedChangelogAndNoDevNull(t *testing.T) {
	if _, err := os.Stat("../dev/null"); err == nil {
		t.Fatal("release automation must not create repository file dev/null")
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat repository file dev/null: %v", err)
	}

	changelog := readRepositoryFile(t, "../CHANGELOG.md")
	for _, required := range []string{
		"## [1.0.0] - 2026-07-22",
		"### Added",
		"`rust` BuildToolsPreset.",
		"### Changed",
		"Standard open-source project files: `CONTRIBUTING.md`",
		"### Fixed",
		"`--continue` no longer carries forward a stale",
		"### Removed",
		"`--ralph` flag",
		"[1.0.0]: https://github.com/rafaelromao/sandman/releases/tag/v1.0.0",
	} {
		if !strings.Contains(changelog, required) {
			t.Errorf("curated changelog missing %q", required)
		}
	}

	releasing := readRepositoryFile(t, "../docs/development/releasing.md")
	if strings.Contains(releasing, "/dev/null") {
		t.Fatal("release guide must not describe /dev/null as the changelog path")
	}
	if !strings.Contains(releasing, "After `v1.0.0` is created, the release workflow removes that override") {
		t.Fatal("release guide must require removing the bootstrap override after v1.0.0")
	}
}

func TestReleaseWorkflowPublishesConfiguredReleaseArtifacts(t *testing.T) {
	release := readRepositoryFile(t, "../.github/workflows/release.yml")
	for _, required := range []string{
		"release_created == 'true'",
		"args: release --clean",
		"GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}",
		"name: Remove first-release override",
		"refs/tags/v1.0.0",
		"jq 'del(.packages[\".\"][\"release-as\"])'",
		"git commit -m \"chore(release): remove first-release override\"",
		"HEAD:main",
	} {
		if !strings.Contains(release, required) {
			t.Errorf("release workflow missing %q", required)
		}
	}

	goreleaser := readRepositoryFile(t, "../.goreleaser.yml")
	for _, required := range []string{
		"id: linux-amd64",
		"id: darwin-amd64",
		"id: darwin-arm64",
		"format: tar.gz",
		"checksums.txt",
		"sandman_{{ .Version }}_{{ .Os }}_{{ .Arch }}",
	} {
		if !strings.Contains(goreleaser, required) {
			t.Errorf("GoReleaser config missing %q", required)
		}
	}
}

func TestBinaryInstallationDocumentationMatchesReleaseContract(t *testing.T) {
	install := readRepositoryFile(t, "../docs/get-started/install.md")
	for _, required := range []string{
		"https://github.com/rafaelromao/sandman/releases/download/v${VERSION}",
		"sandman_<version>_<os>_<arch>.tar.gz",
		"sandman_1.0.0_linux_amd64.tar.gz",
		"sandman_1.0.0_darwin_amd64.tar.gz",
		"sandman_1.0.0_darwin_arm64.tar.gz",
		"checksums.txt",
		"TARGET_ARCHIVE=\"sandman_${VERSION}_linux_amd64.tar.gz\"",
		"grep -F \"  ${TARGET_ARCHIVE}\" checksums.txt | sha256sum -c -",
		"grep -F \"  ${TARGET_ARCHIVE}\" checksums.txt | shasum -a 256 -c -",
		"VERSION=1.0.0",
		"sandman --version",
		"sandman 1.0.0",
		"go install github.com/rafaelromao/sandman/cmd/sandman@v1.0.0",
		"Install from source",
	} {
		if !strings.Contains(install, required) {
			t.Errorf("installation guide missing %q", required)
		}
	}
}

func TestReleaseWorkflowUsesCredentialThatTriggersReleasePRChecks(t *testing.T) {
	release := readRepositoryFile(t, "../.github/workflows/release.yml")
	ci := readRepositoryFile(t, "../.github/workflows/go.yml")
	ruleset := readRepositoryFile(t, "../.github/rulesets/main.json")
	contributing := readRepositoryFile(t, "../CONTRIBUTING.md")

	for _, required := range []string{
		"contents: write",
		"issues: write",
		"pull-requests: write",
		"name: Verify release credential",
		"RELEASE_PLEASE_TOKEN repository secret is required",
		"RELEASE_PLEASE_TOKEN: ${{ secrets.RELEASE_PLEASE_TOKEN }}",
		"token: ${{ secrets.RELEASE_PLEASE_TOKEN }}",
	} {
		if !strings.Contains(release, required) {
			t.Errorf("release workflow missing %q", required)
		}
	}

	for _, required := range []string{
		"  pull_request:",
		"semantic-pull-request:",
		"build:",
	} {
		if !strings.Contains(ci, required) {
			t.Errorf("CI workflow missing %q", required)
		}
	}

	for _, required := range []string{
		`"context": "CI / build (ubuntu-latest)"`,
		`"context": "CI / build (macos-latest)"`,
		`"context": "CI / semantic-pull-request"`,
	} {
		if !strings.Contains(ruleset, required) {
			t.Errorf("main ruleset missing required check %q", required)
		}
	}

	for _, required := range []string{
		"RELEASE_PLEASE_TOKEN",
		"Contents",
		"Issues",
		"Pull requests",
		"do not start `pull_request` workflows",
	} {
		if !strings.Contains(contributing, required) {
			t.Errorf("maintainer documentation missing %q", required)
		}
	}
}

func readRepositoryFile(t *testing.T, path string) string {
	t.Helper()

	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(contents)
}
