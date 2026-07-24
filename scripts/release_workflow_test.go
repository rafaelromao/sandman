package main

import (
	"os"
	"strings"
	"testing"
)

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
