package main

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/rafaelromao/sandman/internal/scaffold"
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
		"skip-github-release: true",
		"uses: actions/setup-go@v5",
		"go-version-file: .go-version",
		"version: v2.10.1",
		"args: release --clean --draft",
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
	if !strings.Contains(setupGoBlock, "if: steps.prepare.outputs.complete != 'true'") {
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
		"build-linux:",
		"build-macos:",
	} {
		if !strings.Contains(ci, required) {
			t.Errorf("CI workflow missing %q", required)
		}
	}

	for _, required := range []string{
		`"context": "CI / build (ubuntu-latest)"`,
		`"context": "CI / build (macos-latest)"`,
		`"context": "CI / portal-continuity-browser"`,
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

func TestReleaseWorkflowRefreshesUnchangedPendingBranch(t *testing.T) {
	release := readRepositoryFile(t, "../.github/workflows/release.yml")
	checkout := strings.Index(release, "name: Checkout release automation")
	refresh := strings.Index(release, "name: Refresh pending release branch")
	setupGo := strings.Index(release, "uses: actions/setup-go@v5")
	if checkout < 0 || refresh < checkout || setupGo < refresh {
		t.Fatal("release branch refresh must run after checkout and before release publication")
	}
	block := release[checkout:strings.Index(release, "\n  candidate:")]
	for _, required := range []string{"GH_TOKEN: ${{ secrets.RELEASE_PLEASE_TOKEN }}", "run: sh scripts/refresh_release_pr.sh", "timeout-minutes: 5", "persist-credentials: false"} {
		if !strings.Contains(block, required) {
			t.Errorf("release refresh missing %q", required)
		}
	}
	if strings.Contains(block, "if:") {
		t.Fatal("release refresh must also run when release content is unchanged")
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

	linux := readRepositoryFile(t, "../.github/workflows/full-regression-linux.yml") + readRepositoryFile(t, "../.github/workflows/release-regression-tier.yml")
	for _, required := range []string{
		`SANDMAN_FULL_REGRESSION: "1"`,
		"tier: unit", "tier: smoke", "tier: integration", "tier: live",
		"name: Install pinned agent CLIs", "opencode-ai@1.18.0", "@anthropic-ai/claude-code@2.1.283",
		"secrets.OPENCODE_API_KEY", "secrets.CLAUDE_CODE_OAUTH_TOKEN",
		"name: Set up Node", "name: Set up Chromium",
		"name: Validate prerequisites and record versions",
		"python3 scripts/release_report.py", "if: ${{ always() }}",
		"needs: [unit, smoke, integration, live]", `test "$LIVE_RESULT" = success`,
		`export CONTAINERS_CONF="$RUNNER_TEMP/containers.conf"`,
		`echo "CONTAINERS_CONF=$CONTAINERS_CONF" >> "$GITHUB_ENV"`,
		"name: Configure Podman runtime", `runtime = "runc"`,
		`test "$(podman info --format '{{.Host.OCIRuntime.Name}}')" = "runc"`,
	} {
		if !strings.Contains(linux, required) {
			t.Errorf("Linux full regression workflow missing %q", required)
		}
	}
	runner := readRepositoryFile(t, "release_tier.sh")
	for _, required := range []string{"SANDMAN_RUN_AGENT_E2E=1", "-tags e2e", "-skip", "-run", "--kill-after=30s", "PIPESTATUS[0]", "exit-code", "-count=1"} {
		if !strings.Contains(runner, required) {
			t.Errorf("release tier runner missing %q", required)
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
		"run: bash scripts/ci_report.sh race-linux make build-check test-ci",
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

func TestLinuxCIGatesPortalLogContinuityOnRequiredBrowserCoverage(t *testing.T) {
	workflow := readRepositoryFile(t, "../.github/workflows/go.yml")
	ruleset := readRepositoryFile(t, "../.github/rulesets/main.json")
	browserTests := readRepositoryFile(t, "../internal/cmd/portal_log_continuity_browser_test.go")
	for _, required := range []string{
		"portal-continuity-browser:\n    runs-on: ubuntu-latest",
		"uses: actions/setup-node@v4",
		"node-version: 22",
		"uses: browser-actions/setup-chrome@v2",
		"name: Portal continuity browser gate",
		`SANDMAN_REQUIRE_PORTAL_BROWSER: "1"`,
		"SANDMAN_PORTAL_CHROMIUM: ${{ steps.setup-chrome.outputs.chrome-path }}",
		"run: bash scripts/ci_report.sh portal-browser make test-browser",
		"run: bash scripts/ci_report.sh race-macos make build-check test-ci",
	} {
		if !strings.Contains(workflow, required) {
			t.Errorf("CI workflow is missing required Portal browser gate contract %q", required)
		}
	}
	if !strings.Contains(ruleset, `"context": "CI / portal-continuity-browser"`) {
		t.Fatal("main ruleset must require the Portal continuity browser status")
	}
	if !strings.Contains(browserTests, "func TestPortalLogContinuityBrowser_TabRoundTrips") ||
		!strings.Contains(browserTests, `for _, tab := range []string{"events", "details"}`) ||
		!strings.Contains(readRepositoryFile(t, "../internal/cmd/portal_subject_switch_repro_test.go"), "func TestPortalReviewSubjectSwitch_PreservesPendingPerRunOutputAcrossRoundTrip") {
		t.Fatal("Linux CI must include actual-page tab continuity and pending per-RunID subject round-trip cases")
	}
}

func TestPodmanInstallIsBoundedAcrossLinuxWorkflows(t *testing.T) {
	for _, workflowPath := range []string{
		"../.github/workflows/go.yml",
		"../.github/workflows/release-regression-tier.yml",
	} {
		workflow := readRepositoryFile(t, workflowPath)
		for _, required := range []string{
			"timeout-minutes: 5",
			"if command -v podman >/dev/null 2>&1; then",
			"sudo env DEBIAN_FRONTEND=noninteractive timeout --signal=TERM --kill-after=30s 3m bash -e -c '",
			"apt-get -o Acquire::Retries=2 -o Acquire::http::Timeout=30 -o Acquire::https::Timeout=30 update",
			"apt-get -o Acquire::Retries=2 -o Acquire::http::Timeout=30 -o Acquire::https::Timeout=30 -o DPkg::Lock::Timeout=120 install -y --no-install-recommends podman",
		} {
			if !strings.Contains(workflow, required) {
				t.Errorf("%s: Podman setup must contain bounded command %q", workflowPath, required)
			}
		}
	}
}

func TestPlatformCoverageMapMatchesWorkflowsAndGoReleaser(t *testing.T) {
	ci := readRepositoryFile(t, "../.github/workflows/go.yml")
	linuxFull := readRepositoryFile(t, "../.github/workflows/full-regression-linux.yml") + readRepositoryFile(t, "../.github/workflows/release-regression-tier.yml")
	nativeCompat := readRepositoryFile(t, "../.github/workflows/native-compatibility.yml")
	goreleaser := readRepositoryFile(t, "../.goreleaser.yml")

	for job, runner := range map[string]string{"build-linux": "ubuntu-latest", "build-macos": "macos-latest"} {
		block := extractCIJob(t, ci, job)
		for _, required := range []string{"name: build (" + runner + ")", "runs-on: " + runner} {
			if !strings.Contains(block, required) {
				t.Errorf("%s must preserve %q", job, required)
			}
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

	if !strings.Contains(readRepositoryFile(t, "release_tier.sh"), "SANDMAN_RUN_AGENT_E2E=1") || !strings.Contains(linuxFull, "SANDMAN_E2E_GATES: all") {
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
	// Keep behavior contracts independent of Dependabot's verified SHA updates.
	return regexp.MustCompile(`(?m)(uses: [\w/-]+)@[a-f0-9]{40} # (v[^\s]+|latest)`).ReplaceAllString(string(contents), "$1@$2")
}

func TestQualityGateRejectsUnsuccessfulDependencies(t *testing.T) {
	for _, event := range []string{"pull_request", "push"} {
		base := map[string]string{"SECURITY_RESULT": "success", "WORKFLOW_RESULT": "success", "STATIC_RESULT": "success", "CONTRACT_RESULT": "success", "LINUX_BUILD_RESULT": "success", "MACOS_BUILD_RESULT": "success", "PORTAL_RESULT": "success", "PACKAGING_RESULT": "success", "TITLE_RESULT": "success"}
		if event == "push" {
			base["TITLE_RESULT"] = "skipped"
		}
		run := func(t *testing.T, values map[string]string, wantSuccess bool) {
			t.Helper()
			cmd := exec.Command("bash", "quality_gate.sh")
			// Isolate the evaluator from ambient job results and provider credentials.
			cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "EVENT_NAME=" + event}
			for key, value := range values {
				cmd.Env = append(cmd.Env, key+"="+value)
			}
			output, err := cmd.CombinedOutput()
			if (err == nil) != wantSuccess {
				t.Fatalf("success=%t, want %t: %v\n%s", err == nil, wantSuccess, err, output)
			}
		}
		t.Run(event+"/all-required-pass", func(t *testing.T) { run(t, base, true) })
		for key := range base {
			for _, result := range []string{"failure", "cancelled", "skipped", "", "unknown"} {
				t.Run(fmt.Sprintf("%s/%s/%s", event, key, result), func(t *testing.T) {
					values := make(map[string]string)
					for k, v := range base {
						values[k] = v
					}
					if result == "" {
						delete(values, key)
					} else {
						values[key] = result
					}
					run(t, values, event == "push" && key == "TITLE_RESULT" && result == "skipped")
				})
			}
		}
	}
}

func TestCIQualityGateWorkflowContract(t *testing.T) {
	ci := readRepositoryFile(t, "../.github/workflows/go.yml")
	for _, required := range []string{
		"types: [opened, synchronize, reopened, edited]",
		"group: ${{ github.workflow }}-${{ github.event.pull_request.number || github.run_id }}",
		"cancel-in-progress: ${{ github.event_name == 'pull_request' }}",
		"needs: [security, workflow-validation, static-analysis, contracts, build-linux, build-macos, portal-continuity-browser, goreleaser, semantic-pull-request]",
		"if: ${{ always() }}", "run: bash scripts/quality_gate.sh",
		"LINUX_BUILD_RESULT: ${{ needs.build-linux.result }}",
		"MACOS_BUILD_RESULT: ${{ needs.build-macos.result }}",
		"PORTAL_RESULT: ${{ needs.portal-continuity-browser.result }}",
		"PACKAGING_RESULT: ${{ needs.goreleaser.result }}",
		"TITLE_RESULT: ${{ needs.semantic-pull-request.result }}",
		"EVENT_NAME: ${{ github.event_name }}",
		"STATIC_RESULT: ${{ needs.static-analysis.result }}",
		"CONTRACT_RESULT: ${{ needs.contracts.result }}",
	} {
		if !strings.Contains(ci, required) {
			t.Errorf("CI missing %q", required)
		}
	}
	for job, minutes := range map[string]int{"static-analysis": 15, "contracts": 15, "build-linux": 25, "build-macos": 25, "goreleaser": 20, "portal-continuity-browser": 15, "semantic-pull-request": 5, "quality-gate": 2} {
		block := extractCIJob(t, ci, job)
		if !strings.Contains(block, fmt.Sprintf("timeout-minutes: %d", minutes)) {
			t.Errorf("%s missing bounded timeout", job)
		}
		if strings.Contains(block, "continue-on-error:") {
			t.Errorf("%s must not tolerate failures", job)
		}
		if job != "semantic-pull-request" && regexp.MustCompile(`(?m)^    if:`).MatchString(block) && job != "quality-gate" {
			t.Errorf("%s must run for PRs and main pushes", job)
		}
	}
	gate := extractCIJob(t, ci, "quality-gate")
	if !strings.Contains(gate, "if: ${{ always() }}") || !strings.Contains(gate, "needs: [security, workflow-validation, static-analysis, contracts, build-linux, build-macos, portal-continuity-browser, goreleaser, semantic-pull-request]") {
		t.Fatal("aggregate must always evaluate all mandatory dependencies")
	}
	title := extractCIJob(t, ci, "semantic-pull-request")
	if !strings.Contains(title, "if: github.event_name == 'pull_request'") {
		t.Fatal("title check must skip only events without a PR title")
	}
	// Independent jobs avoid GitHub's partial-rerun matrix aggregation limitation.
	linux := extractCIJob(t, ci, "build-linux")
	macos := extractCIJob(t, ci, "build-macos")
	if strings.Contains(ci, "needs.build.result") || strings.Contains(linux, "matrix:") || strings.Contains(macos, "matrix:") {
		t.Fatal("aggregate must use independent platform results")
	}
	for job, category := range map[string]string{"build-linux": "race-linux", "build-macos": "race-macos"} {
		block := extractCIJob(t, ci, job)
		if !strings.Contains(block, "bash scripts/ci_report.sh "+category+" make build-check test-ci") {
			t.Errorf("%s must use shared build/race validation", job)
		}
		if strings.Contains(block, "setup-chrome") || strings.Contains(block, "make staticcheck") {
			t.Errorf("%s duplicates separately owned validation", job)
		}
	}
	packaging := extractCIJob(t, ci, "goreleaser")
	setup, release := strings.Index(packaging, "uses: actions/setup-go@v5"), strings.Index(packaging, "uses: goreleaser/goreleaser-action@v6")
	if setup < 0 || release < setup || !strings.Contains(packaging, "go-version-file: .go-version") || !strings.Contains(packaging, "version: v2.10.1") || !strings.Contains(packaging, "args: release --snapshot --clean") {
		t.Fatal("mandatory snapshot must explicitly set up Go and use the release tool version")
	}
	if !strings.Contains(readRepositoryFile(t, "../.github/rulesets/main.json"), `"context": "CI / quality-gate"`) {
		t.Fatal("desired protection must include aggregate gate")
	}
}

func extractCIJob(t *testing.T, workflow, job string) string {
	t.Helper()
	header := "\n  " + job + ":\n"
	start := strings.Index(workflow, header)
	if start < 0 {
		t.Fatalf("missing job %s", job)
	}
	rest := workflow[start+len(header):]
	next := regexp.MustCompile(`(?m)^  [a-z][a-z-]*:`).FindStringIndex(rest)
	if next != nil {
		return rest[:next[0]]
	}
	return rest
}

func TestQualityGatePartialPlatformReruns(t *testing.T) {
	for _, results := range []struct {
		name, linux, macos string
		pass               bool
	}{
		{"both-failed", "failure", "failure", false},
		{"only-linux-rerun-passed", "success", "failure", false},
		{"only-macos-rerun-passed", "failure", "success", false},
		{"both-passed", "success", "success", true},
	} {
		t.Run(results.name, func(t *testing.T) {
			cmd := exec.Command("bash", "quality_gate.sh")
			cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "EVENT_NAME=pull_request", "LINUX_BUILD_RESULT=" + results.linux, "MACOS_BUILD_RESULT=" + results.macos, "SECURITY_RESULT=success", "WORKFLOW_RESULT=success", "STATIC_RESULT=success", "CONTRACT_RESULT=success", "PORTAL_RESULT=success", "PACKAGING_RESULT=success", "TITLE_RESULT=success"}
			output, err := cmd.CombinedOutput()
			if (err == nil) != results.pass {
				t.Fatalf("success=%t, want %t: %v\n%s", err == nil, results.pass, err, output)
			}
		})
	}
}

func TestCIValidationOwnershipAndReports(t *testing.T) {
	ci := readRepositoryFile(t, "../.github/workflows/go.yml")
	makefile := readRepositoryFile(t, "../Makefile")
	for _, required := range []string{
		"check: fmt-check build-check vet staticcheck contracts test",
		"go test -race -covermode=atomic -coverprofile=reports/coverage.out -json -skip '$(PORTAL_BROWSER_TESTS)' -timeout=20m ./...",
		"SANDMAN_REQUIRE_PORTAL_BROWSER=1 go test -tags visual -json ./internal/cmd -run '$(PORTAL_BROWSER_TESTS)' -count=1 -timeout=10m",
		"go run scripts/check_adr_index.go",
		"AgentSelection", "Ownership", "Lifecycle", "Cancellation",
		"ProductionGoDoesNotInvokeRepositoryWideWorktreePrune",
		"go test -json ./scripts ./internal/skill/...",
	} {
		if !strings.Contains(makefile, required) {
			t.Errorf("shared validation missing %q", required)
		}
	}
	for job, command := range map[string]string{"static-analysis": "make fmt-check vet staticcheck", "contracts": "make contracts", "portal-continuity-browser": "make test-browser", "build-linux": "make build-check test-ci", "build-macos": "make build-check test-ci"} {
		block := extractCIJob(t, ci, job)
		for _, required := range []string{command, "bash scripts/ci_report.sh", "if: ${{ always() }}", "uses: actions/upload-artifact@v4", "path: reports/"} {
			if !strings.Contains(block, required) {
				t.Errorf("%s missing %q", job, required)
			}
		}
	}
	if strings.Count(ci, "uses: browser-actions/setup-chrome@v2") != 1 {
		t.Fatal("dedicated browser job must be the only Chromium owner")
	}
	for _, job := range []string{"build-linux", "build-macos"} {
		if !strings.Contains(extractCIJob(t, ci, job), "uses: actions/setup-node@v4") {
			t.Errorf("%s must preserve Node production-module coverage", job)
		}
	}
}

// Every test that reaches a Chromium helper belongs to the sole browser job.
// Parse all test files, including the visual build tag, so new page assertions
// cannot silently skip after being left in a job without a browser.
func TestPortalBrowserInventoryMatchesChromiumScenarios(t *testing.T) {
	inventory := strings.Fields(readRepositoryFile(t, "portal_browser_tests.txt"))
	selected := make(map[string]bool)
	for _, name := range inventory {
		if selected[name] {
			t.Errorf("duplicate browser test %s", name)
		}
		selected[name] = true
	}
	files, err := filepath.Glob("../internal/cmd/*_test.go")
	if err != nil {
		t.Fatal(err)
	}
	found := make(map[string]bool)
	for _, path := range files {
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, declaration := range file.Decls {
			fn, ok := declaration.(*ast.FuncDecl)
			if !ok || !strings.HasPrefix(fn.Name.Name, "Test") || fn.Body == nil {
				continue
			}
			// This separately opted-in binary/restart scenario was never a default
			// PR gate; do not turn on additional tiers while classifying coverage.
			if fn.Name.Name == "TestPortalBuiltBinary_BrowserContinuityReloadsAndRecoversAfterRestart" {
				continue
			}
			browser := false
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				callee, ok := call.Fun.(*ast.Ident)
				if !ok {
					return true
				}
				switch callee.Name {
				case "runPortalChromium", "runPortalChromiumWithVirtualTimeBudget", "renderVisual", "runPortalContinuityBrowser", "runPortalWaitingDurationBrowser", "runPortalWaitingAbortBrowser":
					browser = true
				case "requirePortalContinuityTool":
					if len(call.Args) == 2 {
						if literal, ok := call.Args[1].(*ast.BasicLit); ok && literal.Value == `"chromium"` {
							browser = true
						}
					}
				}
				return true
			})
			if browser {
				found[fn.Name.Name] = true
				if !selected[fn.Name.Name] {
					t.Errorf("Chromium scenario %s missing from shared browser inventory", fn.Name.Name)
				}
			}
		}
	}
	for name := range selected {
		if !found[name] {
			t.Errorf("browser inventory contains unknown or non-browser test %s", name)
		}
	}
	makefile := readRepositoryFile(t, "../Makefile")
	if !strings.Contains(makefile, "PORTAL_BROWSER_TESTS := ^($(shell paste -sd '|' scripts/portal_browser_tests.txt))$$") {
		t.Fatal("race and browser selectors must derive from the shared browser inventory")
	}
}

func TestSecurityWorkflowContract(t *testing.T) {
	version := strings.TrimSpace(readRepositoryFile(t, "../.go-version"))
	if !strings.Contains(readRepositoryFile(t, "../go.mod"), "toolchain go"+version) {
		t.Fatal("CI and local patched toolchains must match")
	}
	paths, err := filepath.Glob("../.github/workflows/*.yml")
	if err != nil {
		t.Fatal(err)
	}
	external := regexp.MustCompile(`uses: ([^\s]+)`)
	pinned := regexp.MustCompile(`^[\w/-]+@[a-f0-9]{40}$`)
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, match := range external.FindAllStringSubmatch(string(data), -1) {
			if !strings.HasPrefix(match[1], "./") && !pinned.MatchString(match[1]) {
				t.Errorf("%s: unpinned action %s", path, match[1])
			}
		}
		if !strings.Contains(string(data), "\npermissions:") {
			t.Errorf("%s lacks explicit permissions", path)
		}
	}
	ci := readRepositoryFile(t, "../.github/workflows/go.yml")
	for _, required := range []string{"SECURITY_RESULT: ${{ needs.security.result }}", "WORKFLOW_RESULT: ${{ needs.workflow-validation.result }}", "uses: ./.github/workflows/security-check.yml", "make workflow-check"} {
		if !strings.Contains(ci, required) {
			t.Errorf("missing %s", required)
		}
	}
	scan := readRepositoryFile(t, "../.github/workflows/security-check.yml")
	for _, required := range []string{"fetch-depth: 0", "persist-credentials: false", "govulncheck@v1.1.4", "gitleaks/v8@v8.24.3", "make security-check", "timeout-minutes: 15"} {
		if !strings.Contains(scan, required) {
			t.Errorf("missing %s", required)
		}
	}
	if strings.Contains(scan, "upload-artifact") || strings.Contains(scan, "continue-on-error") {
		t.Fatal("security must fail closed without uploading secret diagnostics")
	}
}

func TestCoverageCollectionAndPolicyOwnership(t *testing.T) {
	ci := readRepositoryFile(t, "../.github/workflows/go.yml")
	for _, platform := range []string{"linux", "macos"} {
		job := extractCIJob(t, ci, "build-"+platform)
		for _, required := range []string{"fetch-depth: 0", "COVERAGE_TEST_RESULT: ${{ steps.race.outcome }}", "python3 scripts/coverage_report.py --platform " + platform, "name: coverage-" + platform, "reports/coverage.out"} {
			if !strings.Contains(job, required) {
				t.Errorf("%s coverage contract missing %q", platform, required)
			}
		}
	}
	makefile := readRepositoryFile(t, "../Makefile")
	if !strings.Contains(makefile, "-race -covermode=atomic -coverprofile=reports/coverage.out") || !strings.Contains(makefile, "python3 scripts/coverage_report_test.py") {
		t.Fatal("coverage must reuse atomic race collection and test policy in contracts")
	}
}

func TestReleaseManifestInventoriesTaggedScenarios(t *testing.T) {
	var manifest struct {
		Scenarios []struct{ Package, Test, Tier string }
	}
	if err := json.Unmarshal([]byte(readRepositoryFile(t, "release_scenarios.json")), &manifest); err != nil {
		t.Fatal(err)
	}
	required := make(map[string]string)
	for _, scenario := range manifest.Scenarios {
		key := scenario.Tier + ":" + scenario.Package + ":" + scenario.Test
		if required[key] != "" {
			t.Errorf("duplicate manifest scenario %s", key)
		}
		required[key] = scenario.Test
	}
	err := filepath.WalkDir("../internal", func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		source := readRepositoryFile(t, path)
		if !strings.HasPrefix(source, "//go:build e2e") {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		pkg := "github.com/rafaelromao/sandman/" + strings.TrimPrefix(filepath.ToSlash(filepath.Dir(path)), "../")
		for _, declaration := range file.Decls {
			fn, ok := declaration.(*ast.FuncDecl)
			if !ok || !strings.HasPrefix(fn.Name.Name, "Test") {
				continue
			}
			invokesLive := false
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				name, ok := call.Fun.(*ast.Ident)
				if ok && (name.Name == "requireSandmanAgentE2E" || name.Name == "requirePresetMatrixE2E" || name.Name == "runE2ERun") {
					invokesLive = true
				}
				return true
			})
			if invokesLive && required["live:"+pkg+":"+fn.Name.Name] == "" {
				t.Errorf("real-provider scenario must belong to live tier: %s:%s", pkg, fn.Name.Name)
			}
			found := false
			for _, tier := range []string{"integration", "live", "claude"} {
				if required[tier+":"+pkg+":"+fn.Name.Name] != "" {
					found = true
				}
			}
			if !found {
				t.Errorf("tagged release scenario missing from manifest: %s:%s", pkg, fn.Name.Name)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	native := readRepositoryFile(t, "../.github/workflows/native-compatibility.yml")
	for _, platform := range []string{"linux-amd64", "linux-arm64", "darwin-amd64", "darwin-arm64"} {
		block := extractCIJob(t, native, platform)
		for _, required := range []string{"python3 scripts/release_report.py native", "if: ${{ always() }}", "-json -count=1", "source-sha.txt", "versions.txt"} {
			if !strings.Contains(block, required) {
				t.Errorf("native %s missing %s", platform, required)
			}
		}
	}
}

func TestReleaseTierJobsRemainIndependent(t *testing.T) {
	workflow := readRepositoryFile(t, "../.github/workflows/full-regression-linux.yml")
	for _, tier := range []string{"unit", "smoke", "integration", "live"} {
		block := extractCIJob(t, workflow, tier)
		if !strings.Contains(block, "uses: ./.github/workflows/release-regression-tier.yml") || strings.Contains(block, "needs:") {
			t.Errorf("release tier %s must run independently", tier)
		}
		if tier != "live" && strings.Contains(block, "secrets:") {
			t.Errorf("%s must receive no provider secrets", tier)
		}
		gate := extractCIJob(t, workflow, "release-gate")
		if !strings.Contains(gate, "needs."+tier+".result") {
			t.Errorf("release gate must inspect %s explicitly", tier)
		}
	}
	if strings.Contains(workflow, "matrix:") {
		t.Fatal("separate jobs must preserve per-tier outcomes on partial reruns")
	}
}

func TestPublicationUsesSameRunValidatedSource(t *testing.T) {
	release := readRepositoryFile(t, "../.github/workflows/release.yml")
	for _, job := range []string{"quality", "regression", "native"} {
		block := extractCIJob(t, release, job)
		if !strings.Contains(block, "needs: candidate") || !strings.Contains(block, "uses: ./.github/workflows/") {
			t.Errorf("%s must validate the caller source through a local reusable workflow", job)
		}
	}
	gate := extractCIJob(t, release, "publication-gate")
	for _, required := range []string{"always()", "needs: [candidate, quality, regression, native]", "toJSON(needs)", "python3 scripts/publication_gate.py"} {
		if !strings.Contains(gate, required) {
			t.Errorf("publication gate missing %s", required)
		}
	}
	config := readRepositoryFile(t, "../.goreleaser.yml")
	for _, required := range []string{"use_existing_draft: true", "draft: true"} {
		if !strings.Contains(config, required) {
			t.Errorf("GoReleaser must reuse and retain the prepared draft: missing %s", required)
		}
	}
	publish := extractCIJob(t, release, "publish")
	for _, required := range []string{"needs: [candidate, publication-gate]", "ref: ${{ needs.candidate.outputs.sha }}", "release --clean --draft", "python3 scripts/publication_finalize.py"} {
		if !strings.Contains(publish, required) {
			t.Errorf("publisher missing %s", required)
		}
	}
	maintenance := extractCIJob(t, release, "release-please")
	if strings.Contains(maintenance, "needs:") || !strings.Contains(maintenance, "skip-github-release: true") {
		t.Fatal("release PR maintenance must be independent and cannot publish")
	}
	if !strings.Contains(readRepositoryFile(t, "../.github/rulesets/main.json"), `"strict_required_status_checks_policy": true`) {
		t.Fatal("main must require up-to-date validation")
	}
}

func TestClaudeProviderE2EsRequireManualDispatch(t *testing.T) {
	workflow := readRepositoryFile(t, "../.github/workflows/full-regression-linux.yml")
	manual := extractCIJob(t, workflow, "claude")
	for _, required := range []string{"github.event_name == 'workflow_dispatch' && inputs.run_claude", "tier: claude", "secrets.CLAUDE_CODE_OAUTH_TOKEN"} {
		if !strings.Contains(manual, required) {
			t.Errorf("manual Claude job missing %q", required)
		}
	}
	dispatch := extractOnSubBlock(t, workflow, "workflow_dispatch")
	if !strings.Contains(dispatch, "run_claude:") || !strings.Contains(dispatch, "default: false") {
		t.Fatal("Claude E2Es must require explicit opt-in on manual dispatch")
	}
	for _, source := range []string{extractCIJob(t, workflow, "live"), extractCIJob(t, workflow, "release-gate"), readRepositoryFile(t, "../.github/workflows/release.yml")} {
		if strings.Contains(source, "CLAUDE_CODE_OAUTH_TOKEN") || strings.Contains(source, "needs.claude") {
			t.Error("automatic release validation must not require Claude credentials or results")
		}
	}
	var manifest struct{ Scenarios []struct{ Test, Tier string } }
	if err := json.Unmarshal([]byte(readRepositoryFile(t, "release_scenarios.json")), &manifest); err != nil {
		t.Fatal(err)
	}
	for _, scenario := range manifest.Scenarios {
		if scenario.Test == "TestPresetMatrixHarness_RealClaudeAgentReuseSession" && scenario.Tier != "claude" {
			t.Fatal("Claude provider scenario must belong only to the manual tier")
		}
	}
}

func TestReleaseOpenCodePinMatchesScaffoldFallback(t *testing.T) {
	var manifest struct {
		Providers map[string]struct {
			CLI string `json:"cli"`
		} `json:"providers"`
	}
	if err := json.Unmarshal([]byte(readRepositoryFile(t, "release_scenarios.json")), &manifest); err != nil {
		t.Fatal(err)
	}
	pin := manifest.Providers["opencode"].CLI
	if pin != scaffold.DefaultBuiltInAgentVersion("opencode") {
		t.Fatalf("release OpenCode %q differs from scaffold fallback %q", pin, scaffold.DefaultBuiltInAgentVersion("opencode"))
	}
	workflow := readRepositoryFile(t, "../.github/workflows/release-regression-tier.yml")
	if !strings.Contains(workflow, "npm install -g opencode-ai@"+pin) || !strings.Contains(workflow, "$(opencode --version)\" = "+pin) {
		t.Fatal("release installation and prerequisite check must match the manifest pin")
	}
}
