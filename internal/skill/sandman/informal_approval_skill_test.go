package sandman

import (
	"os"
	"strings"
	"testing"
)

// An eligible current-head informal approval is full approval. The merge and
// review-request skills must never send the implementor after a formal review
// the author cannot obtain from their own account.
func TestSkills_InformalApprovalIsFullApproval(t *testing.T) {
	for path, required := range map[string][]string{
		"pr-merge/SKILL.md": {
			"Either\nof these is full approval on its own",
			"an eligible informal approval",
			"A formal review is never required",
			"do not ask the\nreviewer or a maintainer for a formal review, a review decision, or a\npublication workaround",
		},
		"review-request/SKILL.md": {
			"It never asks the reviewer for a formal\n   approval, a review decision, publication work",
			"an eligible informal approval already satisfies the\n   approval gate",
		},
	} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, phrase := range required {
			if !strings.Contains(string(data), phrase) {
				t.Errorf("%s is missing the informal-approval contract %q", path, phrase)
			}
		}
	}
}
