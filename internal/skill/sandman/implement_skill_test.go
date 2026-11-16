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

// The implementation capability states who owns what at the handoff: local
// verification and publication are the implementor's, external observation is
// the runtime's in a managed run. Contract tests pin the guidance exists and is
// stable; they cannot prove that an agent follows it.
func TestImplementSkillDefinesTheHandoffBoundary(t *testing.T) {
	text := strings.Join(strings.Fields(readSkillFile(t, "implement/SKILL.md")), " ")
	for _, required := range []string{
		"### Handoff boundary",
		"Local verification and publication are yours; external observation belongs to the runtime in a Sandman-managed run.",
		"a background command that is still running",
		"an unresolved test failure",
		"an unpushed commit",
		"an incomplete publication, review-request delivery or blind review round",
		"never skip, interrupt, weaken or abbreviate it to hand off sooner",
		"Publishing durable progress early",
		"does not end your turn",
		"If local work remains after early publication, say so explicitly instead of implying that the runtime is already observing an external wait.",
		"record the durable checkpoint in `.sandman/task.md` and return immediately in a managed run",
		"Do not launch redundant verification, and do not poll CI or review from the agent session to wait for them.",
		"A standalone composition continues into the review cycle, which owns its own polling.",
		"local verification in progress, external CI or review awaiting observation, and ready for merge",
		`Never present an earlier "CI is running" or "review is pending" statement as current`,
		"label it as the last observation with its timestamp",
		"In a managed run the runtime, not your summary, makes the post-attempt decision.",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("implement skill is missing the handoff contract %q", required)
		}
	}
	for _, forbidden := range []string{
		"Do not publish the pull request until every local command finishes",
		"postpone publication",
	} {
		if strings.Contains(text, forbidden) {
			t.Errorf("implement skill must not serialize publication behind local verification: %q", forbidden)
		}
	}
}

// Delivering a review request is delivery, not observation. The capability
// stays stateless: the handoff guidance must not turn it into an observer.
func TestReviewRequestSkillStaysStatelessAndNeverObserves(t *testing.T) {
	text := strings.Join(strings.Fields(readSkillFile(t, "review-request/SKILL.md")), " ")
	for _, required := range []string{
		"Deliver one stateless delegated review request for a change request without owning review state, polling, or lifecycle decisions.",
		"Do not poll for feedback, classify approval or requested changes, or invoke the standalone review cycle.",
		"Do not create, edit, or delete review state files.",
		"Do not write lifecycle, await, dependency, terminal, or event state.",
		"this capability does not start its deadline or observation loop",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("review-request skill lost its stateless contract %q", required)
		}
	}
	if strings.Contains(text, "return immediately") {
		t.Error("the handoff boundary belongs to the implementation capability, not the stateless review-request capability")
	}
}

// The faster managed cadence is a runtime default. The standalone review cycle
// keeps its own plan, so the shared skill text must stay unchanged.
func TestReviewCycleSkillKeepsTheStandalonePollPlan(t *testing.T) {
	text := readSkillFile(t, "review-cycle/SKILL.md")
	if got := strings.Count(text, "--argjson poll_plan '[120,60,60,30]'"); got != 2 {
		t.Errorf("standalone review cycle plan occurrences = %d, want its two request builders unchanged", got)
	}
	if !strings.Contains(text, "The coordinator preserves the existing `120, 60, 60, then 30` poll plan") {
		t.Error("standalone review cycle no longer states its own poll plan")
	}
	if strings.Contains(text, "30-second default") {
		t.Error("the managed default cadence must not leak into the standalone review cycle")
	}
}
