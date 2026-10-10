package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRefreshReleasePR(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq is required for the release automation")
	}
	head := strings.Repeat("a", 40)
	releasePR := `{"number":2698,"base":{"ref":"main"},"head":{"sha":"` + head + `","ref":"release-please--branches--main","repo":{"full_name":"owner/repo"}}}`
	forkPR := strings.Replace(releasePR, `"owner/repo"`, `"fork/repo"`, 1)
	for _, tc := range []struct {
		name, pulls, status                                  string
		compareFailure, updateFailure, wantUpdate, wantError bool
	}{
		{name: "no pending release", pulls: `[[]]`},
		{name: "already contains main", pulls: `[[` + releasePR + `]]`, status: "ahead"},
		{name: "identical head", pulls: `[[` + releasePR + `]]`, status: "identical"},
		{name: "unchanged release content with new test commit", pulls: `[[` + releasePR + `]]`, status: "diverged", wantUpdate: true},
		{name: "behind main", pulls: `[[` + releasePR + `]]`, status: "behind", wantUpdate: true},
		{name: "ignore fork branch", pulls: `[[` + forkPR + `]]`},
		{name: "paginated matching release", pulls: `[[` + forkPR + `],[` + releasePR + `]]`, status: "diverged", wantUpdate: true},
		{name: "ambiguous release branches", pulls: `[[` + releasePR + `,` + releasePR + `]]`, wantError: true},
		{name: "malformed API response", pulls: `invalid`, wantError: true},
		{name: "unknown comparison", pulls: `[[` + releasePR + `]]`, status: "unknown", wantError: true},
		{name: "comparison lookup failure", pulls: `[[` + releasePR + `]]`, compareFailure: true, wantError: true},
		{name: "concurrent head change rejected", pulls: `[[` + releasePR + `]]`, status: "diverged", updateFailure: true, wantUpdate: true, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			calls := filepath.Join(dir, "calls")
			fake := `#!/bin/sh
printf '%s\n' "$*" >> "$CALLS"
case "$*" in
  *'/pulls?'*) printf '%s\n' "$PULLS" ;;
  *'/compare/'*)
    [ "$COMPARE_FAILURE" != true ] || exit 1
    printf '{"status":"%s"}\n' "$COMPARE_STATUS" ;;
  *'/update-branch '*)
    [ "$UPDATE_FAILURE" != true ] || exit 1
    printf '{"message":"Updating pull request branch."}\n' ;;
  *) exit 2 ;;
esac
`
			if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(fake), 0755); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "sh", "refresh_release_pr.sh")
			cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"),
				"GITHUB_REPOSITORY=owner/repo", "CALLS="+calls, "PULLS="+tc.pulls,
				"COMPARE_STATUS="+tc.status, "COMPARE_FAILURE="+boolText(tc.compareFailure), "UPDATE_FAILURE="+boolText(tc.updateFailure))
			output, err := cmd.CombinedOutput()
			if (err != nil) != tc.wantError {
				t.Fatalf("error = %v, wantError = %t; output: %s", err, tc.wantError, output)
			}
			data, err := os.ReadFile(calls)
			if err != nil {
				t.Fatal(err)
			}
			updates := strings.Count(string(data), "/update-branch ")
			wantUpdates := 0
			if tc.wantUpdate {
				wantUpdates = 1
			}
			if updates != wantUpdates {
				t.Fatalf("updates = %d, want %d; calls: %s", updates, wantUpdates, data)
			}
			if tc.wantUpdate && !strings.Contains(string(data), "--method PUT repos/owner/repo/pulls/2698/update-branch -f expected_head_sha="+head) {
				t.Fatalf("branch update lost its optimistic head guard: %s", data)
			}
		})
	}
}

func boolText(value bool) string {
	if value {
		return "true"
	}
	return "false"
}
