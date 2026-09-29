package sandman

import (
	"os"
	"strings"
	"testing"
)

func TestImplementSkillCompletesOwnedPublicationBeforeYield(t *testing.T) {
	data, err := os.ReadFile("implement/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, required := range []string{
		"Never yield for work this workflow owns",
		"Do not ask whether those actions are authorized",
		"A checklist transition such as `PR-Review` is not permission to stop before the PR exists",
		"A confirmed review request is ongoing from successful delivery, even before a reviewer begins",
		"Do not yield for a generic pending gate, missing checks, a stale head, an unconfirmed review comment, a lookup failure, or an exhausted operation budget",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("implement skill is missing autonomous wait contract %q", required)
		}
	}
}
