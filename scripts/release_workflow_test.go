package main

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestReleaseConfigUsesAutomaticRCVersioningAfterRC1(t *testing.T) {
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
	if root.ReleaseAs != "" {
		t.Fatalf("release-as = %q, want removed after RC1", root.ReleaseAs)
	}

	manifest := readRepositoryFile(t, "../.release-please-manifest.json")
	var versions map[string]string
	if err := json.Unmarshal([]byte(manifest), &versions); err != nil {
		t.Fatalf("parse .release-please-manifest.json: %v", err)
	}
	version := versions["."]
	if len(versions) != 1 || !strings.HasPrefix(version, "1.0.0-rc.") {
		t.Fatalf("release manifest = %#v, want generated 1.0.0-rc.N state", versions)
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
	for _, forbidden := range []string{
		"release-as: 1.0.0-rc.1",
		"The next Release Please run should then advance to `v1.0.0-rc.2`.",
	} {
		if strings.Contains(releasing, forbidden) {
			t.Errorf("release guide must not retain bootstrap-specific versioning guidance %q", forbidden)
		}
	}
	for _, required := range []string{
		"now calculates later RC versions",
		"all four platform archives",
	} {
		if !strings.Contains(releasing, required) {
			t.Errorf("release guide missing automatic-versioning guidance %q", required)
		}
	}
}

func TestReleasePolicyDocumentationDescribesAutomaticVersioningAfterRC1(t *testing.T) {
	for _, path := range []string{"../AGENTS.md", "../CONTRIBUTING.md"} {
		documentation := readRepositoryFile(t, path)
		if strings.Contains(documentation, "release-as: 1.0.0-rc.1") {
			t.Errorf("%s must not retain the RC1 bootstrap override", path)
		}
		if !strings.Contains(documentation, "subsequent RC versions automatically") {
			t.Errorf("%s must describe automatic subsequent RC versioning", path)
		}
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
		"`sandman_<version>_linux_arm64.tar.gz`",
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

func TestReleaseGuideMatchesVerifiedRC1BinaryOutput(t *testing.T) {
	releasing := readRepositoryFile(t, "../docs/development/releasing.md")
	for _, required := range []string{
		"sandman 1.0.0-rc.1",
		"released binary reports version `1.0.0-rc.1`",
	} {
		if !strings.Contains(releasing, required) {
			t.Errorf("release guide missing verified binary output %q", required)
		}
	}
	if strings.Contains(releasing, "sandman v1.0.0-rc.1") {
		t.Error("release guide must not add a v prefix to release binary output")
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
		"VERSION=$(curl -fsSL https://api.github.com/repos/rafaelromao/sandman/releases",
		"sandman --version",
		"go install github.com/rafaelromao/sandman/cmd/sandman@v1.0.0-rc.1",
		"curl -fsSL https://raw.githubusercontent.com/rafaelromao/sandman/main/scripts/install.sh | sh -s -- --include-prerelease",
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
		`"context": "Native Compatibility / Native Compatibility (darwin/amd64)"`,
		`"context": "Native Compatibility / Native Compatibility (darwin/arm64)"`,
		`"context": "Native Compatibility / Native Compatibility (linux/arm64)"`,
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

func TestReleaseValidationWorkflowsRunOnlyForReleasePleaseBranch(t *testing.T) {
	for _, path := range []string{
		"../.github/workflows/full-regression-linux.yml",
		"../.github/workflows/native-compatibility.yml",
	} {
		workflow := readRepositoryFile(t, path)
		for _, required := range []string{
			"  push:",
			"branches: [release-please--branches--main]",
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

	linux := readRepositoryFile(t, "../.github/workflows/full-regression-linux.yml")
	if !strings.Contains(linux, "SANDMAN_RUN_AGENT_E2E=1 SANDMAN_TEST_PROVIDERS=all SANDMAN_E2E_GATES=all go test -tags e2e -timeout 90m ./...") {
		t.Error("Linux full regression workflow must preserve the canonical E2E command")
	}
	for _, required := range []string{
		`SANDMAN_FULL_REGRESSION: "1"`,
		"name: Install opencode CLI",
		"curl -fsSL https://opencode.ai/install | bash",
		"name: Provision opencode auth",
		"secrets.OPENCODE_API_KEY",
		`export CONTAINERS_CONF="$RUNNER_TEMP/containers.conf"`,
		`echo "CONTAINERS_CONF=$CONTAINERS_CONF" >> "$GITHUB_ENV"`,
		"name: Configure Podman runtime",
		`runtime = "runc"`,
		`test "$(podman info --format '{{.Host.OCIRuntime.Name}}')" = "runc"`,
	} {
		if !strings.Contains(linux, required) {
			t.Errorf("Linux full regression workflow missing %q", required)
		}
	}
}

func TestNativeCompatibilityCoversAllFourArches(t *testing.T) {
	workflow := readRepositoryFile(t, "../.github/workflows/native-compatibility.yml")

	for _, required := range []string{
		"name: Native Compatibility",
		"runs-on: macos-15-intel",
		"runs-on: macos-14",
		"runs-on: ubuntu-24.04-arm",
		"timeout-minutes: 45",
		"name: Build native Sandman (Intel)",
		"name: Build native Sandman (Apple Silicon)",
		"name: Build native Sandman (Linux arm64)",
		`test "$(go env GOOS)" = "darwin"`,
		`test "$(go env GOOS)" = "linux"`,
		`test "$(go env GOARCH)" = "amd64"`,
		`test "$(go env GOARCH)" = "arm64"`,
		`test "$(uname -m)" = "x86_64"`,
		`test "$(uname -m)" = "arm64"`,
		`test "$(uname -m)" = "aarch64"`,
		`go build -o "$RUNNER_TEMP/sandman" ./cmd/sandman`,
		"name: Test native socket compatibility",
		"TestResolvePortalPeerPIDReturnsCallerPID",
		"TestDiscoverPortalInstances_LongPathBindsAndDials",
		"TestPortal_RunStream_BridgesControlSocketToSSE",
		"TestAttach_(ReadsFromSocket|ExitsOnEOF|FindsLongPathReviewSock)",
		"name: Test native portal process",
		"TestPortal_E2E_TwoLiveRuns",
		"name: Test native portal abort endpoint",
		"TestPortal_E2E_AbortReturns404ForUnknownRun",
	} {
		if !strings.Contains(workflow, required) {
			t.Errorf("native compatibility workflow missing %q", required)
		}
	}

	darwinAmd64 := strings.Index(workflow, "runs-on: macos-15-intel")
	darwinArm64 := strings.Index(workflow, "runs-on: macos-14")
	linuxArm64 := strings.Index(workflow, "runs-on: ubuntu-24.04-arm")
	if darwinAmd64 == -1 || darwinArm64 == -1 || linuxArm64 == -1 {
		t.Fatal("native compatibility workflow must declare the darwin/amd64, darwin/arm64, and linux/arm64 jobs")
	}
	if darwinAmd64 > darwinArm64 || darwinArm64 > linuxArm64 {
		t.Fatal("native compatibility workflow must declare jobs in the order darwin/amd64, darwin/arm64, linux/arm64")
	}

	portalE2ETests := readRepositoryFile(t, "../internal/cmd/portal_e2e_test.go")
	for _, funcName := range []string{
		"TestPortal_E2E_TwoLiveRuns",
		"TestPortal_E2E_AbortReturns404ForUnknownRun",
	} {
		body, start := extractGoFunction(t, portalE2ETests, funcName)
		if start == -1 {
			t.Fatalf("native compatibility contract requires %s in portal_e2e_test.go", funcName)
		}
		if strings.Contains(body, `t.Skip("skip e2e in CI")`) || strings.Contains(body, `t.Skip("no container runtime available in CI")`) {
			t.Errorf("%s must not self-skip on CI; it is one of the hermetic cases the native compatibility suite must run", funcName)
		}
	}
}

func extractGoFunction(t *testing.T, source, name string) (string, int) {
	t.Helper()
	header := "func " + name + "("
	start := strings.Index(source, header)
	if start == -1 {
		return "", -1
	}
	body := source[start:]
	next := strings.Index(body, "\nfunc ")
	if next == -1 {
		return body, start
	}
	return body[:next], start
}

func TestNativeCompatibilityDoesNotDuplicateLinuxFullRegression(t *testing.T) {
	workflow := readRepositoryFile(t, "../.github/workflows/native-compatibility.yml")

	for _, forbidden := range []string{
		"Run full regression suite",
		"go test -race -v ./...",
		"SANDMAN_RUN_SMOKE_E2E=1",
		"SANDMAN_RUN_AGENT_E2E=1",
		"SANDMAN_E2E_GATES=all",
		"TestPresetMatrixHarness_",
		"matrix:",
		"name: Install Podman",
		"podman machine",
		"TestOpencodeSubagentPermissionAllowAll",
	} {
		if strings.Contains(workflow, forbidden) {
			t.Errorf("native compatibility workflow must leave exhaustive coverage to Linux and the native boundary to Darwin; found %q", forbidden)
		}
	}
}

func TestCIWorkflowKeepsOptInSuitesDisabled(t *testing.T) {
	workflow := readRepositoryFile(t, "../.github/workflows/go.yml")
	for _, required := range []string{
		`SANDMAN_TEST_PROVIDERS: ""`,
		`SANDMAN_RUN_SMOKE_E2E: "0"`,
		`SANDMAN_RUN_AGENT_E2E: "0"`,
		`SANDMAN_E2E_GATES: ""`,
		"run: go test -race -v ./...",
	} {
		if !strings.Contains(workflow, required) {
			t.Errorf("regular CI workflow missing opt-in suite exclusion %q", required)
		}
	}
	if strings.Contains(workflow, "SANDMAN_FULL_REGRESSION") {
		t.Fatal("regular CI must not set SANDMAN_FULL_REGRESSION; only the full-regression workflow disables CI skips")
	}
	if strings.Contains(workflow, "-tags smoke") || strings.Contains(workflow, "-tags e2e") {
		t.Fatal("regular CI must not compile or run smoke/e2e-tagged tests")
	}
	for _, required := range []string{
		`export CONTAINERS_CONF="$RUNNER_TEMP/containers.conf"`,
		`echo "CONTAINERS_CONF=$CONTAINERS_CONF" >> "$GITHUB_ENV"`,
		"name: Configure Podman runtime",
		`runtime = "runc"`,
		`test "$(podman info --format '{{.Host.OCIRuntime.Name}}')" = "runc"`,
	} {
		if !strings.Contains(workflow, required) {
			t.Errorf("regular CI workflow missing Podman runtime configuration %q", required)
		}
	}
}

func TestPlatformCoverageMapMatchesWorkflowsAndGoReleaser(t *testing.T) {
	ci := readRepositoryFile(t, "../.github/workflows/go.yml")
	linuxFull := readRepositoryFile(t, "../.github/workflows/full-regression-linux.yml")
	nativeCompat := readRepositoryFile(t, "../.github/workflows/native-compatibility.yml")
	goreleaser := readRepositoryFile(t, "../.goreleaser.yml")

	ciMatrix := extractOnSubBlock(t, ci, "build")
	for _, required := range []string{
		"- ubuntu-latest",
		"- macos-latest",
	} {
		if !strings.Contains(ciMatrix, required) {
			t.Errorf("CI build matrix must include %q for the four-platform coverage claim to hold", required)
		}
	}

	for _, required := range []string{
		"runs-on: macos-15-intel",
		"runs-on: macos-14",
		"runs-on: ubuntu-24.04-arm",
		`test "$(go env GOOS)" = "darwin"`,
		`test "$(go env GOOS)" = "linux"`,
		`test "$(go env GOARCH)" = "amd64"`,
		`test "$(go env GOARCH)" = "arm64"`,
		`test "$(uname -m)" = "x86_64"`,
		`test "$(uname -m)" = "arm64"`,
		`test "$(uname -m)" = "aarch64"`,
		"TestPortal_E2E_TwoLiveRuns",
		"TestPortal_E2E_AbortReturns404ForUnknownRun",
	} {
		if !strings.Contains(nativeCompat, required) {
			t.Errorf("native compatibility workflow must include %q so the four-platform coverage claim holds", required)
		}
	}

	if !strings.Contains(linuxFull, "SANDMAN_RUN_AGENT_E2E=1") || !strings.Contains(linuxFull, "SANDMAN_E2E_GATES=all") {
		t.Error("Full Regression - Linux must remain the exhaustive release regression authority (real-agent + all e2e gates)")
	}

	for _, required := range []string{
		"id: linux-amd64",
		"id: linux-arm64",
		"id: darwin-amd64",
		"id: darwin-arm64",
		"goos:\n      - linux",
		"goos:\n      - darwin",
	} {
		if !strings.Contains(goreleaser, required) {
			t.Errorf(".goreleaser.yml must declare %q so all four Sandman platforms match the four OpenCode CLI platforms", required)
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
