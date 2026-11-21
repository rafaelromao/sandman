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
// as a raw `gh` invocation somewhere else in production code.

var issueClosingMethodRe = regexp.MustCompile(`(?i)(clos(e|ed|ing).*(issue|work.?item))|((issue|work.?item).*clos(e|ed|ing))|reopen`)

func TestClientHasNoWayToCloseAnIssue(t *testing.T) {
	for _, typ := range []reflect.Type{
		reflect.TypeOf((*Client)(nil)).Elem(),
		reflect.TypeOf((*CLIClient)(nil)),
	} {
		for i := 0; i < typ.NumMethod(); i++ {
			name := typ.Method(i).Name
			if issueClosingMethodRe.MatchString(name) {
				t.Errorf("%s exposes %s: Sandman must not be able to change a work item's state", typ, name)
			}
		}
	}
}

var issueClosingInvocationRes = []*regexp.Regexp{
	regexp.MustCompile(`"issue",\s*"close"`),
	regexp.MustCompile(`issue close`),
	regexp.MustCompile(`state=closed`),
	regexp.MustCompile(`"state":\s*"closed"`),
}

func TestProductionCodeNeverClosesAnIssue(t *testing.T) {
	repoRoot := filepath.Join("..", "..")
	for _, dir := range []string{"internal", "cmd"} {
		root := filepath.Join(repoRoot, dir)
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			// This guard names the forbidden invocations on purpose.
			if filepath.Base(path) == "no_issue_close_test.go" {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, re := range issueClosingInvocationRes {
				if loc := re.FindIndex(data); loc != nil {
					t.Errorf("%s contains %q: Sandman must not close work items", path, data[loc[0]:loc[1]])
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
}
