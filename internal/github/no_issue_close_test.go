package github

import (
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// Sandman never owns a work item's open/closed state: closing is the team's
// delivery flow (or the repository's own auto-close setting). These guards
// keep the capability from quietly coming back, either as a Client method or
// as a raw invocation anywhere Sandman ships: production Go, the shared
// skills and the default task prompt.

// issueStateMutatorRe matches method names that change a work item's state.
// Read-only lookups such as ListClosedIssues are deliberately allowed.
var issueStateMutatorRe = regexp.MustCompile(`(?i)^((close|reopen|resolve)(issue|workitem)|(issue|workitem)(close|reopen|resolve)|(update|set|edit)(issue|workitem)(state|status))`)

func TestClientHasNoWayToCloseAnIssue(t *testing.T) {
	for _, typ := range []reflect.Type{
		reflect.TypeOf((*Client)(nil)).Elem(),
		reflect.TypeOf((*CLIClient)(nil)),
	} {
		for i := 0; i < typ.NumMethod(); i++ {
			name := typ.Method(i).Name
			if issueStateMutatorRe.MatchString(name) {
				t.Errorf("%s exposes %s: Sandman must not be able to change a work item's state", typ, name)
			}
		}
	}
}

func TestIssueStateMutatorReAllowsReadOnlyLookups(t *testing.T) {
	for name, want := range map[string]bool{
		"CloseIssue":          true,
		"ReopenIssue":         true,
		"IssueClose":          true,
		"UpdateIssueState":    true,
		"ClosePR":             false, // closing a pull request is not a work item write
		"ListClosedIssues":    false,
		"FetchIssueState":     false,
		"FindClosingPRsLater": false,
	} {
		if got := issueStateMutatorRe.MatchString(name); got != want {
			t.Errorf("issueStateMutatorRe(%q) = %v, want %v", name, got, want)
		}
	}
}

// issueClosingInvocationRes match the ways code or shipped prose could close
// an issue: the gh argument vector or command, a state PATCH, or the GraphQL
// closeIssue mutation.
var issueClosingInvocationRes = []*regexp.Regexp{
	regexp.MustCompile(`"issue",\s*"close"`),
	regexp.MustCompile(`gh issue close`),
	regexp.MustCompile(`state=closed`),
	regexp.MustCompile(`"state":\s*"closed"`),
	regexp.MustCompile(`\bcloseIssue\b`),
}

// shippedExtensions are the production and shipped-content file types scanned.
var shippedExtensions = map[string]bool{".go": true, ".md": true, ".sh": true}

// findIssueClosingInvocations scans every shipped file under the given
// directories of root and returns one description per violation. Go test files
// are skipped: they may name the forbidden shapes, for example in a guard.
func findIssueClosingInvocations(t *testing.T, root string, dirs ...string) []string {
	t.Helper()
	var found []string
	for _, dir := range dirs {
		base := filepath.Join(root, dir)
		err := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == ".sandman" || d.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			if !shippedExtensions[filepath.Ext(path)] || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, re := range issueClosingInvocationRes {
				if loc := re.FindIndex(data); loc != nil {
					found = append(found, path+": "+string(data[loc[0]:loc[1]]))
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", base, err)
		}
	}
	return found
}

func TestShippedContentNeverClosesAnIssue(t *testing.T) {
	root := filepath.Join("..", "..")
	for _, violation := range findIssueClosingInvocations(t, root,
		"internal", "cmd", // production Go, shared skills and the default task prompt
	) {
		t.Errorf("%s: Sandman must not close work items", violation)
	}
}

// The scanner must actually see violations in every shipped file type, or the
// guard above could pass vacuously.
func TestIssueClosingScannerDetectsEachShape(t *testing.T) {
	root := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("internal/x/close.go", "package x\nvar args = []string{\"issue\", \"close\", \"1\"}\n")
	write("internal/x/patch.go", "package x\nconst q = \"-f state=closed\"\n")
	write("internal/x/mutation.go", "package x\nconst m = `mutation { closeIssue(input: {}) { clientMutationId } }`\n")
	write("internal/skill/sandman/demo/SKILL.md", "```bash\ngh issue close 12\n```\n")
	write("internal/skill/sandman/demo/helper.sh", "#!/bin/sh\ngh api -X PATCH repos/o/r/issues/1 -f state=closed\n")
	write("internal/prompt/default-task-prompt.md", "Close the work item with gh issue close.\n")
	// Allowed: test files and prose that merely mentions closure.
	write("internal/x/guard_test.go", "package x\nconst forbidden = \"gh issue close\"\n")
	write("internal/skill/sandman/demo/ok.md", "The tracker decides whether an issue closes on merge.\n")
	write("cmd/ok.go", "package cmd\n// when the issue closes, nothing happens\n")

	got := findIssueClosingInvocations(t, root, "internal", "cmd")
	for _, wantFile := range []string{
		"close.go", "patch.go", "mutation.go", "SKILL.md", "helper.sh", "default-task-prompt.md",
	} {
		hit := false
		for _, violation := range got {
			if strings.Contains(violation, wantFile) {
				hit = true
			}
		}
		if !hit {
			t.Errorf("scanner missed the violation in %s; found %v", wantFile, got)
		}
	}
	for _, violation := range got {
		for _, allowed := range []string{"guard_test.go", "ok.md", "ok.go"} {
			if strings.Contains(violation, string(filepath.Separator)+allowed+":") {
				t.Errorf("scanner flagged allowed content %s", violation)
			}
		}
	}
}
