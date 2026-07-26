package main

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestReleaseConfigUsesPrereleaseBaselineAndExactInitialVersion(t *testing.T) {
	config := readRepositoryFile(t, "../release-please-config.json")
	var releaseConfig struct {
		Versioning     string `json:"versioning"`
		Prerelease     bool   `json:"prerelease"`
		PrereleaseType string `json:"prerelease-type"`
		Packages       map[string]struct {
			ChangelogPath string `json:"changelog-path"`
			ReleaseAs     string `json:"release-as"`
		} `json:"packages"`
	}
	if err := json.Unmarshal([]byte(config), &releaseConfig); err != nil {
		t.Fatalf("parse release-please-config.json: %v", err)
	}
	if releaseConfig.Versioning != "prerelease" || !releaseConfig.Prerelease || releaseConfig.PrereleaseType != "rc" {
		t.Fatalf("release config prerelease settings = (%q, %t, %q), want (prerelease, true, rc)", releaseConfig.Versioning, releaseConfig.Prerelease, releaseConfig.PrereleaseType)
	}

	root := releaseConfig.Packages["."]
	if root.ChangelogPath != "CHANGELOG.md" {
		t.Fatalf("bootstrap changelog path = %q, want CHANGELOG.md", root.ChangelogPath)
	}
	if root.ReleaseAs != "1.0.0-rc.1" && root.ReleaseAs != "" {
		t.Fatalf("initial release-as = %q, want 1.0.0-rc.1 or removed", root.ReleaseAs)
	}

	manifest := readRepositoryFile(t, "../.release-please-manifest.json")
	var versions map[string]string
	if err := json.Unmarshal([]byte(manifest), &versions); err != nil {
		t.Fatalf("parse .release-please-manifest.json: %v", err)
	}
	version := versions["."]
	if len(versions) != 1 || (version != "0.2.0" && !strings.HasPrefix(version, "1.0.0-rc.")) {
		t.Fatalf("release manifest = %#v, want baseline 0.2.0 or generated 1.0.0-rc.N release", versions)
	}
}

func TestReleaseBaselinePreservesCuratedChangelogAndNoDevNull(t *testing.T) {
	if _, err := os.Stat("../dev/null"); err == nil {
		t.Fatal("release automation must not create repository file dev/null")
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat repository file dev/null: %v", err)
	}

	changelog := readRepositoryFile(t, "../CHANGELOG.md")
	for _, required := range []string{
		"## [0.2.0] - 2026-07-22",
		"### Added",
		"`rust` BuildToolsPreset.",
		"### Changed",
		"Standard open-source project files: `CONTRIBUTING.md`",
		"### Fixed",
		"`--continue` no longer carries forward a stale",
		"### Removed",
		"`--ralph` flag",
		"[0.2.0]: https://github.com/rafaelromao/sandman/releases/tag/v0.2.0",
	} {
		if !strings.Contains(changelog, required) {
			t.Errorf("curated changelog missing %q", required)
		}
	}

	releasing := readRepositoryFile(t, "../docs/development/releasing.md")
	if strings.Contains(releasing, "/dev/null") {
		t.Fatal("release guide must not describe /dev/null as the changelog path")
	}
	if !strings.Contains(releasing, "After `v1.0.0-rc.1` is created, remove the one-time override") {
		t.Fatal("release guide must require removing the prerelease override after v1.0.0-rc.1")
	}
}

func TestReleaseWorkflowPublishesConfiguredReleaseArtifacts(t *testing.T) {
	release := readRepositoryFile(t, "../.github/workflows/release.yml")
	for _, required := range []string{
		"release_created == 'true'",
		"uses: actions/setup-go@v5",
		"go-version-file: go.mod",
		"version: v2.10.1",
		"args: release --clean",
		"GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}",
	} {
		if !strings.Contains(release, required) {
			t.Errorf("release workflow missing %q", required)
		}
	}
	for _, forbidden := range []string{"Remove first-release override", "refs/tags/v1.0.0", "release-as\\\": \\\"1.0.0\\\"", "jq 'del(.packages"} {
		if strings.Contains(release, forbidden) {
			t.Errorf("release workflow still contains bootstrap cleanup %q", forbidden)
		}
	}

	checkout := strings.Index(release, "uses: actions/checkout@v4")
	setupGo := strings.Index(release, "uses: actions/setup-go@v5")
	goreleaserStep := strings.Index(release, "uses: goreleaser/goreleaser-action@v6")
	if checkout == -1 || setupGo == -1 || goreleaserStep == -1 || checkout > setupGo || setupGo > goreleaserStep {
		t.Fatal("release workflow must set up Go after checkout and before GoReleaser")
	}

	setupGoBlock := release[setupGo:goreleaserStep]
	if !strings.Contains(setupGoBlock, "if: steps.release-please.outputs.release_created == 'true'") {
		t.Fatal("release workflow Go setup must be gated on release creation")
	}

	releasing := readRepositoryFile(t, "../docs/development/releasing.md")
	for _, required := range []string{
		"Go `1.25.0` from `go.mod`",
		"GoReleaser `v2.10.1`",
		"production release job explicitly installs",
	} {
		if !strings.Contains(releasing, required) {
			t.Errorf("release guide missing %q", required)
		}
	}

	goreleaser := readRepositoryFile(t, "../.goreleaser.yml")
	for _, required := range []string{
		"id: linux-amd64",
		"id: linux-arm64",
		"id: darwin-amd64",
		"id: darwin-arm64",
		"format: tar.gz",
		"checksums.txt",
		"sandman_{{ .Version }}_{{ .Os }}_{{ .Arch }}",
		"prerelease: auto",
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
		"checksums.txt",
		"TARGET_ARCHIVE=\"sandman_${VERSION}_linux_amd64.tar.gz\"",
		"`sandman_<version>_linux_amd64.tar.gz`",
		"`sandman_<version>_linux_arm64.tar.gz`",
		"`sandman_<version>_darwin_amd64.tar.gz`",
		"`sandman_<version>_darwin_arm64.tar.gz`",
		"grep -F \"  ${TARGET_ARCHIVE}\" checksums.txt | sha256sum -c -",
		"grep -F \"  ${TARGET_ARCHIVE}\" checksums.txt | shasum -a 256 -c -",
		"VERSION=$(curl -fsSL https://api.github.com/repos/rafaelromao/sandman/releases/latest",
		"sandman --version",
		"go install github.com/rafaelromao/sandman/cmd/sandman@latest",
		"curl -fsSL https://raw.githubusercontent.com/rafaelromao/sandman/main/scripts/install.sh | sh",
		"--install-dir DIRECTORY",
		"Build from a checkout",
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
	for _, forbidden := range []string{
		`"context": "Full Regression - Linux / Full Regression Suite (Ubuntu)"`,
		`"context": "Full Regression - macOS / Full Regression Suite (macOS)"`,
	} {
		if strings.Contains(ruleset, forbidden) {
			t.Errorf("main ruleset must not require release-only check %q", forbidden)
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

func TestFullRegressionWorkflowsRunOnlyForReleasePleaseBranch(t *testing.T) {
	for _, path := range []string{
		"../.github/workflows/full-regression-linux.yml",
		"../.github/workflows/full-regression-macos.yml",
	} {
		workflow := readRepositoryFile(t, path)
		for _, required := range []string{
			"  push:",
			"branches: [release-please--branches--main]",
			"SANDMAN_RUN_AGENT_E2E=1 SANDMAN_TEST_PROVIDERS=all SANDMAN_E2E_GATES=all go test -tags e2e -timeout 90m ./...",
		} {
			if !strings.Contains(workflow, required) {
				t.Errorf("%s missing %q", path, required)
			}
		}
		if strings.Contains(workflow, "  pull_request:") {
			t.Errorf("%s must not run on every pull request", path)
		}
		if strings.Contains(workflow, "Real-agent Preset Matrix") {
			t.Errorf("%s must run the real-agent matrix as part of E2E", path)
		}
	}
}

func TestMacOSFullRegressionPreparesPodmanOnIntelRunner(t *testing.T) {
	workflow := readRepositoryFile(t, "../.github/workflows/full-regression-macos.yml")

	for _, required := range []string{
		"runs-on: macos-15-intel",
		"CONTAINERS_MACHINE_PROVIDER: applehv",
		"name: Install Podman",
		"podman-installer-macos-amd64.pkg",
		"a30e5010e59aea43f6d808eff29166d31b216d5d7c3991bb038e00c0acdb0b27",
		"shasum -a 256 -c -",
		"sudo installer -pkg \"$podman_pkg\" -target /",
		"echo /opt/podman/bin >> \"$GITHUB_PATH\"",
		"name: Initialize Podman machine",
		"podman machine init \\",
		"--cpus 3",
		"--memory 8192",
		"--disk-size 60",
		"--now",
		"name: Validate Podman",
		"podman info",
		"podman run --rm alpine:latest uname -a",
		"name: Configure short temporary paths",
		"TMPDIR=/private/tmp/sandman-ci",
		"name: Prewarm container image",
		"podman pull alpine:latest",
	} {
		if !strings.Contains(workflow, required) {
			t.Errorf("macOS full regression workflow missing %q", required)
		}
	}

	setupGo := strings.Index(workflow, "name: Set up Go")
	installPodman := strings.Index(workflow, "name: Install Podman")
	validatePodman := strings.Index(workflow, "name: Validate Podman")
	prewarm := strings.Index(workflow, "name: Prewarm container image")
	regression := strings.Index(workflow, "name: Run full regression suite")
	if setupGo == -1 || installPodman == -1 || validatePodman == -1 || prewarm == -1 || regression == -1 ||
		setupGo > installPodman || installPodman > validatePodman || validatePodman > prewarm || prewarm > regression {
		t.Fatal("macOS full regression workflow must prepare Podman before running tests")
	}
}

func TestMacOSFullRegressionCollectsDiagnosticsAndStopsPodman(t *testing.T) {
	workflow := readRepositoryFile(t, "../.github/workflows/full-regression-macos.yml")

	for _, required := range []string{
		"name: Podman diagnostics",
		"uname -a",
		"arch",
		"podman version",
		"podman machine list",
		"podman machine inspect",
		"df -h",
		"name: Stop Podman machine",
		"if: always()",
		"podman machine stop || true",
	} {
		if !strings.Contains(workflow, required) {
			t.Errorf("macOS full regression workflow missing %q", required)
		}
	}

	diagnostics := strings.Index(workflow, "name: Podman diagnostics")
	regression := strings.Index(workflow, "name: Run full regression suite")
	stop := strings.Index(workflow, "name: Stop Podman machine")
	if diagnostics == -1 || regression == -1 || stop == -1 || diagnostics > regression || regression > stop {
		t.Fatal("macOS full regression workflow must collect diagnostics before tests and stop Podman afterward")
	}
}

func TestMacOSFullRegressionPreservesAllRegressionCommands(t *testing.T) {
	workflow := readRepositoryFile(t, "../.github/workflows/full-regression-macos.yml")

	for _, command := range []string{
		`go test -race -v ./...`,
		`SANDMAN_TEST_PROVIDERS=all SANDMAN_RUN_SMOKE_E2E=1 go test -tags smoke -timeout 60m ./internal/cmd -run Smoke`,
		`SANDMAN_RUN_AGENT_E2E=1 SANDMAN_TEST_PROVIDERS=all SANDMAN_E2E_GATES=all go test -tags e2e -timeout 90m ./...`,
	} {
		if !strings.Contains(workflow, command) {
			t.Errorf("macOS full regression workflow missing regression command %q", command)
		}
	}
}

func TestCIWorkflowRunsOnPullRequestsToAnyBranch(t *testing.T) {
	ci := readRepositoryFile(t, "../.github/workflows/go.yml")

	pullRequestBlock := extractOnSubBlock(t, ci, "pull_request")
	for _, forbidden := range []string{
		"branches: [main]",
		"branches-ignore:",
	} {
		if strings.Contains(pullRequestBlock, forbidden) {
			t.Errorf("CI workflow pull_request trigger must not restrict by base branch; found %q in pull_request block:\n%s", forbidden, pullRequestBlock)
		}
	}

	pushBlock := extractOnSubBlock(t, ci, "push")
	if !strings.Contains(pushBlock, "branches: [main]") {
		t.Errorf("CI workflow push trigger must remain restricted to [main] so direct pushes to feature branches do not start CI independently of a PR; push block was:\n%s", pushBlock)
	}
}

func TestContributorDocumentationDescribesCIPullRequestScope(t *testing.T) {
	contributing := readRepositoryFile(t, "../CONTRIBUTING.md")
	agents := readRepositoryFile(t, "../AGENTS.md")

	if !strings.Contains(contributing, "CI runs on pull requests to any branch") {
		t.Error("CONTRIBUTING.md must state that CI runs on pull requests to any branch, not only on pull requests targeting main")
	}
	if !strings.Contains(agents, "CI runs on pull requests to any branch") {
		t.Error("AGENTS.md must state that CI runs on pull requests to any branch, not only on pull requests targeting main")
	}
}

func extractOnSubBlock(t *testing.T, workflow, key string) string {
	t.Helper()
	headerRe := regexp.MustCompile(`(?m)^  ` + regexp.QuoteMeta(key) + `:\n`)
	match := headerRe.FindStringIndex(workflow)
	if match == nil {
		t.Fatalf("could not find %q block under on: in workflow", key)
	}
	rest := workflow[match[1]:]
	nextTopRe := regexp.MustCompile(`(?m)^[^ ]`)
	nextMatch := nextTopRe.FindStringIndex(rest)
	if nextMatch == nil {
		return rest
	}
	return rest[:nextMatch[0]]
}

func readRepositoryFile(t *testing.T, path string) string {
	t.Helper()

	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(contents)
}
