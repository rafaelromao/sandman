package sandman

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readPRReviewSkill(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	path := filepath.Join(wd, "pr-review", "SKILL.md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func readPRReviewPrompt(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	path := filepath.Join(wd, "..", "..", "prompt", "default_pr_review_prompt.md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func readADR0013(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	path := filepath.Join(wd, "..", "..", "..", "docs", "adr", "0013-sandman-review-daemon-and-guard.md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func TestPRReviewSkill_NoSelfPostWrappers(t *testing.T) {
	text := readPRReviewSkill(t)

	forbidden := []string{
		"record_review_posted",
		"record_review_posted_fallback",
		"record_trigger_posted",
		"self-posted.json",
	}
	for _, term := range forbidden {
		if strings.Contains(text, term) {
			t.Errorf("pr-review SKILL.md must not reference %q after issue #1757 removal", term)
		}
	}

	if strings.Contains(text, "#### Step 4b") {
		t.Errorf("pr-review SKILL.md must not contain the `#### Step 4b` heading after issue #1757 removal")
	}
}

func TestPRReviewSkill_NoInternalPaths(t *testing.T) {
	text := readPRReviewSkill(t)

	forbidden := []string{
		"internal/review/",
		"SelfPostStore",
		"internal/review.",
		"internal/review\\",
	}
	for _, term := range forbidden {
		if strings.Contains(text, term) {
			t.Errorf("pr-review SKILL.md must not reference internal path or symbol %q in prose", term)
		}
	}
}

func TestPRReviewSkill_PromptRulePreserved(t *testing.T) {
	prompt := readPRReviewPrompt(t)

	required := []string{
		"sandman-code-review",
		"pull-request review context",
		"the surrounding workflow owns those actions",
	}
	for _, phrase := range required {
		if !strings.Contains(prompt, phrase) {
			t.Errorf("default PR review prompt must retain canonical daemon-redaction phrasing %q", phrase)
		}
	}

	for _, phrase := range []string{"gh pr view", "gh pr checks", "gh pr diff", "gh api", "git push"} {
		if strings.Contains(prompt, phrase) {
			t.Errorf("default PR review prompt must not direct the daemon reviewer to orchestrate a pull request with %q", phrase)
		}
	}
}

func TestPRReviewPrompt_UsesDaemonReviewContextOnly(t *testing.T) {
	prompt := readPRReviewPrompt(t)

	for _, phrase := range []string{
		"Load `sandman-code-review` and use its pull-request review context",
		"The current pull request is",
		"The current worktree is the review worktree",
		"You are read-only",
	} {
		if !strings.Contains(prompt, phrase) {
			t.Errorf("daemon review prompt missing context boundary %q", phrase)
		}
	}

	for _, forbidden := range []string{
		"Standalone self-review",
		"parent workflow supplies one bounded review packet",
		"git diff <fixed-point>...HEAD",
		"construct or delegate a self-review packet",
	} {
		if strings.Contains(prompt, forbidden) {
			t.Errorf("daemon review prompt must not select standalone self-review behavior %q", forbidden)
		}
	}
}

func TestPRReviewSkill_BehavioralSmoke(t *testing.T) {
	text := readPRReviewSkill(t)

	anchors := []struct {
		name    string
		substr  string
		message string
	}{
		{
			name:    "polling loop",
			substr:  "#### Step 5: Wait for this confirmed request",
			message: "polling loop heading must remain",
		},
		{
			name:    "head-SHA tracking",
			substr:  ".sandman/state/<N>.head_sha",
			message: "head-SHA tracking path must remain",
		},
		{
			name:    "addressed-comments tracking",
			substr:  ".sandman/state/<N>.addressed_comments",
			message: "addressed-comments tracking path must remain",
		},
		{
			name:    "re-request on SHA change",
			substr:  "always allow re-requesting",
			message: "re-request-on-SHA-change rule must remain",
		},
		{
			name:    "stale request check",
			substr:  "#### Step 3: Check if SHA changed",
			message: "Step 3 stale-request check must remain",
		},
		{
			name:    "trigger post body",
			substr:  `gh pr comment <N> --repo <owner/repo> --body "{{REVIEW_COMMAND}}"`,
			message: "Step 4 trigger-post command must remain",
		},
	}
	for _, a := range anchors {
		if !strings.Contains(text, a.substr) {
			t.Errorf("%s: missing %q\nfull text:\n%s", a.message, a.substr, text)
		}
	}

	prefixGuard := "do NOT prefix it with the review command"
	if !strings.Contains(text, prefixGuard) {
		t.Errorf("pr-review SKILL.md must retain the prefix-guard bullet verbatim; missing %q", prefixGuard)
	}
}

// TestPRReviewSkill_StaleApprovalHardRule pins the rule that prevents an old
// informal approval from gating PR-Merge after a new commit has landed. The
// regression case (run 260720131929-8d25-257) showed the agent classifying a
// 2.5-day-old APPROVED comment as Case C informal approval and merging the PR
// 30 seconds later, even though the user had just posted a fresh
// `/sandman review` trigger against the new head SHA.
//
// Three rules must remain in the skill text:
//
//  1. Case C approval is only valid when its createdAt is after the head
//     SHA recorded in `.sandman/state/<N>.head_sha` — a SHA change makes
//     every prior approval stale.
//  2. An unanswered `/sandman review` trigger sitting above an older
//     APPROVED comment blocks Case C classification — the trigger itself
//     is a fresh request that must receive a response first.
//  3. Case C classification requires at least one full polling cycle
//     (cumulative sleep ≥ 240 s) since the most recent trigger post —
//     a single 120 s poll cannot observe a meaningful response window.
func TestPRReviewSkill_StaleApprovalHardRule(t *testing.T) {
	text := readPRReviewSkill(t)

	anchors := []struct {
		name    string
		substr  string
		message string
	}{
		{
			name:    "approval-vs-current-SHA gate",
			substr:  "its `createdAt` is **after** the SHA recorded in `.sandman/state/<N>.head_sha`",
			message: "Case C must require the approval's createdAt to be after the head SHA recorded at the last /sandman review post",
		},
		{
			name:    "stale-approval-after-SHA-change rule",
			substr:  "every prior approval timestamp is stale",
			message: "Step 3 SHA change must explicitly mark prior approval timestamps stale",
		},
		{
			name:    "pending-trigger-beats-older-approval rule",
			substr:  "an implementor `{{REVIEW_COMMAND}}` trigger that has not yet received a response",
			message: "Case C must not classify an older APPROVED comment below an unanswered /sandman review trigger",
		},
		{
			name:    "minimum-poll-budget rule",
			substr:  "at least 240 s of cumulative sleep",
			message: "Case C must require at least one full polling cycle (240 s cumulative) before classifying",
		},
	}

	for _, a := range anchors {
		if !strings.Contains(text, a.substr) {
			t.Errorf("%s: missing %q\nfull text:\n%s", a.message, a.substr, text)
		}
	}
}

func TestPRReviewSkill_SameCredentialDecisionIsResponse(t *testing.T) {
	text := readPRReviewSkill(t)

	required := []string{
		"Do not filter it out because the author shares your GitHub login",
		"whose body does not begin with `{{REVIEW_COMMAND}}`",
		"regardless of author",
	}
	for _, phrase := range required {
		if !strings.Contains(text, phrase) {
			t.Errorf("pr-review SKILL.md must preserve same-credential decision handling %q", phrase)
		}
	}

	forbidden := []string{
		"whose author is not the agent itself",
		"any non-agent author",
		"comments from non-agent author",
	}
	for _, phrase := range forbidden {
		if strings.Contains(text, phrase) {
			t.Errorf("pr-review SKILL.md must not filter reviewer decisions by author %q", phrase)
		}
	}
}

func TestPRReviewSkill_UsesConfigurableCurrentAgentRunTimeout(t *testing.T) {
	text := readPRReviewSkill(t)

	for _, phrase := range []string{
		"current AgentRun task context",
		"REVIEW_TIMEOUT",
		"1800 seconds",
		"absolute wall-clock",
		"120 seconds",
		"60 seconds",
		"30 seconds",
		"240 seconds",
		"sleep that lands exactly on the configured budget is permitted",
		"new review request starts a fresh counter",
		"observed response counts",
		"next executable action",
	} {
		if !strings.Contains(text, phrase) {
			t.Errorf("pr-review SKILL.md must describe configurable review timeout behavior %q", phrase)
		}
	}

	for _, forbidden := range []string{"900s", "900 s", "15 minutes"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("pr-review SKILL.md must not retain fixed review budget %q", forbidden)
		}
	}
}

func TestPRReviewSkill_UsesVersionedRequestWait(t *testing.T) {
	text := readPRReviewSkill(t)

	for _, phrase := range []string{
		"review-wait-v1.sh",
		"review-observe-v1.sh",
		"head_sha=\"$headRefOid\"",
		"protocol:\"review-wait/v1\"",
		"--request-file",
		"trigger_id=\"$trigger_url\"",
		"select((.url // \"\") == $trigger_url)",
		"trigger_id",
		"trigger_created_at",
		"started_unix_seconds",
		"elapsed_seconds",
		"state:\"unavailable\"",
		"raw evidence is available for the existing Step 6 classifier",
		"does not require a host `sandman` binary",
		"post a follow-up beginning with `{{REVIEW_COMMAND}}`",
	} {
		if !strings.Contains(text, phrase) {
			t.Errorf("pr-review SKILL.md must describe the versioned request wait %q", phrase)
		}
	}
	if strings.Count(text, "gh pr comment <N> --repo <owner/repo> --body \"{{REVIEW_COMMAND}}\"") != 1 {
		t.Fatal("pr-review SKILL.md must post exactly one trigger per request")
	}

	waitStart := strings.Index(text, "#### Step 5: Wait for this confirmed request")
	waitEnd := strings.Index(text, "#### Step 5a:")
	if waitStart < 0 || waitEnd < waitStart {
		t.Fatal("could not isolate versioned wait section")
	}
	waitSection := text[waitStart:waitEnd]
	for _, forbidden := range []string{
		"gh api repos/<owner>/<repo>/pulls/<N>/reviews",
		"gh api repos/<owner>/<repo>/pulls/<N>/comments",
		"review_sleep_elapsed",
	} {
		if strings.Contains(waitSection, forbidden) {
			t.Errorf("versioned wait section must not open-code %q", forbidden)
		}
	}
}

func TestPRReviewSkill_UsesRequestScopedClassification(t *testing.T) {
	text := readPRReviewSkill(t)

	for _, phrase := range []string{
		`classification.protocol == "review-classification/v1"`,
		"active-to-next-trigger window",
		"classification.formal.approval_evidence",
		"classification.formal.requested_changes",
		"classification.request_state == \"superseded\"",
		"head_status: \"stale\"",
		"The pull-request-wide `reviewDecision` aggregate",
		"boundary_evidence",
	} {
		if !strings.Contains(text, phrase) {
			t.Errorf("pr-review SKILL.md must describe request-scoped classification %q", phrase)
		}
	}

	step6 := strings.Index(text, "#### Step 6: Read and classify feedback")
	step7 := strings.Index(text, "#### Step 7: Apply fixes")
	if step6 < 0 || step7 < step6 {
		t.Fatal("could not isolate classification section")
	}
	classification := text[step6:step7]
	approvalStart := strings.Index(classification, "**A. Formal approval detected?**")
	approvalEnd := strings.Index(classification, "**B. Formal changes requested?**")
	if approvalStart < 0 || approvalEnd < approvalStart {
		t.Fatal("could not isolate formal approval classification")
	}
	approval := classification[approvalStart:approvalEnd]
	if !strings.Contains(approval, `classification.request_state == "active"`) {
		t.Fatal("formal approval must require an active request")
	}
	for _, forbidden := range []string{
		"- `reviewDecision: APPROVED`",
		"- `reviewDecision: CHANGES_REQUESTED`",
		"OR any entry returned by the reviews endpoint",
	} {
		if strings.Contains(classification, forbidden) {
			t.Errorf("Step 6 must not classify request state from unscoped evidence %q", forbidden)
		}
	}
}

func TestPRReviewSkill_PreservesCommitlessCommentedApproval(t *testing.T) {
	text := readPRReviewSkill(t)
	step6 := strings.Index(text, "#### Step 6: Read and classify feedback")
	step7 := strings.Index(text, "#### Step 7: Apply fixes")
	if step6 < 0 || step7 < step6 {
		t.Fatal("could not isolate classification section")
	}
	classification := text[step6:step7]
	approvalStart := strings.Index(classification, "**C. Informal approval (implicit approval without formal review)?**")
	approvalEnd := strings.Index(classification, "**D. Still pending?**")
	if approvalStart < 0 || approvalEnd < approvalStart {
		t.Fatal("could not isolate informal approval classification")
	}
	approval := classification[approvalStart:approvalEnd]
	for _, phrase := range []string{
		"For a request-scoped formal `COMMENTED` review",
		"`head_status: \"unknown\"` remains eligible",
		"current-head formal `APPROVED` records create mechanical approval evidence",
	} {
		if !strings.Contains(approval, phrase) {
			t.Errorf("informal approval must preserve commitless COMMENTED compatibility %q", phrase)
		}
	}
}

func TestPRReviewSkill_FailsClosedOnUntrustedRequestPair(t *testing.T) {
	text := readPRReviewSkill(t)

	for _, phrase := range []string{
		"The request envelope is the authoritative identity record",
		"persisted_head_sha=$(jq -er '.head_sha' \"$request_file\")",
		"recorded_head_sha=$(tr -d '\\r\\n' <\"$head_file\")",
		"If either artifact is missing, malformed, or mismatched, fail closed",
		"Do not\nsilently repair it by posting another trigger",
		"started_unix_seconds",
		"elapsed_seconds",
	} {
		if !strings.Contains(text, phrase) {
			t.Errorf("pr-review SKILL.md must pin fail-closed request identity behavior %q", phrase)
		}
	}

	step3 := strings.Index(text, "#### Step 3: Check if SHA changed")
	step4 := strings.Index(text, "#### Step 4: Delegate review")
	if step3 < 0 || step4 < step3 {
		t.Fatal("could not isolate request identity validation before trigger posting")
	}
	validation := text[step3:step4]
	if strings.Contains(validation, `gh pr comment <N> --repo <owner/repo> --body "{{REVIEW_COMMAND}}"`) {
		t.Fatal("request identity validation must not post a trigger")
	}
}

func TestPRReviewSkill_UsesFailClosedTriggerGuardBeforeEveryCommandPost(t *testing.T) {
	text := readPRReviewSkill(t)

	for _, phrase := range []string{
		"review-trigger-guard-v1.sh",
		"review-trigger/v1",
		"Before every command-prefixed post",
		"This includes the Step 5 reviewer clarification/follow-up",
		"unanswered-trigger",
		"ambiguously ordered remote evidence also refuses",
		"external-gate state",
		"The guard is read-only",
	} {
		if !strings.Contains(text, phrase) {
			t.Errorf("pr-review SKILL.md must describe fail-closed trigger delivery %q", phrase)
		}
	}

	step4 := strings.Index(text, "#### Step 4: Delegate review")
	step5 := strings.Index(text, "#### Step 5: Wait for this confirmed request")
	if step4 < 0 || step5 < step4 {
		t.Fatal("could not isolate trigger-post section")
	}
	step4Text := text[step4:step5]
	guard := strings.Index(step4Text, "review-trigger-guard-v1.sh")
	post := strings.Index(step4Text, `gh pr comment <N> --repo <owner/repo> --body "{{REVIEW_COMMAND}}"`)
	if guard < 0 || post < 0 || guard > post {
		t.Fatal("Step 4 must run the trigger guard before posting the command-prefixed request")
	}
}

func TestPRReviewSkill_PersistsConfirmedTriggerIdentityAfterGuardedPost(t *testing.T) {
	text := readPRReviewSkill(t)
	for _, phrase := range []string{
		`trigger_id="$trigger_url"`,
		`trigger_created_at`,
		`--arg trigger_id "$trigger_id"`,
		`--arg trigger_created_at "$trigger_created_at"`,
		`def valid_timestamp`,
		`Only after this confirmation, atomically write the request envelope`,
		`The request envelope is the atomic request record`,
		`does not create a second request lifecycle authority`,
		`clarification_url=$(gh pr comment`,
		`'.head_sha = $head_sha | .trigger_id = $trigger_id | .trigger_created_at = $trigger_created_at'`,
		`request-envelope-missing`,
		`[ "$guard_reason" != "head-changed" ]`,
		`do not create another request`,
		`latest confirmed`,
	} {
		if !strings.Contains(text, phrase) {
			t.Errorf("pr-review SKILL.md must preserve confirmed request identity %q", phrase)
		}
	}

	guard := strings.Index(text, "review-trigger-guard-v1.sh")
	guardCall := strings.Index(text, "require_review_trigger_delivery ||")
	post := strings.Index(text, `gh pr comment <N> --repo <owner/repo> --body "{{REVIEW_COMMAND}}"`)
	confirm := strings.Index(text, "trigger_created_at=$(gh pr view")
	persist := strings.Index(text, "Only after this confirmation, atomically write the request envelope")
	if guard < 0 || guardCall < 0 || guardCall > post || post < guard || confirm < post || persist < confirm {
		t.Fatal("the guarded post must confirm and persist the same trigger identity before waiting")
	}
	followUpStart := strings.Index(text, "When the follow-up is allowed and posted")
	if followUpStart < 0 {
		t.Fatal("could not isolate follow-up delivery section")
	}
	followUpEnd := strings.Index(text[followUpStart:], "#### Step 5a:")
	if followUpEnd < 0 {
		t.Fatal("could not isolate follow-up delivery section")
	}
	followUp := text[followUpStart : followUpStart+followUpEnd]
	followUpGuard := strings.Index(followUp, "require_review_trigger_delivery")
	followUpPost := strings.Index(followUp, "clarification_url=$(gh pr comment")
	if followUpGuard < 0 || followUpPost < 0 || followUpGuard > followUpPost {
		t.Fatal("follow-up delivery must run the trigger guard before posting")
	}
}

func TestPRReviewSkill_ADRNotesDaemonOwnership(t *testing.T) {
	text := readADR0013(t)

	// Post-#1845 the trust boundary is the daemon transform, not the
	// LLM prompt. ADR-0013 §Daemon-side redaction must name the
	// daemon as the sole poster (reads decision.md, redacts, posts
	// via gh pr comment) and the agent as the writer of the canonical
	// body file. The old "sole authoritative record" SelfPostStore
	// ownership note was removed by S7 because the store itself is
	// gone.
	required := []string{
		"The daemon posts the review comment",
		"<worktree>/decision.md",
		"RedactBody",
		"daemon-side transform that runs out-of-band of the LLM",
		"#1845",
	}
	for _, phrase := range required {
		if !strings.Contains(text, phrase) {
			t.Errorf("ADR-0013 must record the daemon-as-poster ownership after issue #1845; missing %q", phrase)
		}
	}

	if strings.Contains(text, "the `pr-review` SKILL.md Step 4b wrapper hashes the bot's review-body") {
		t.Errorf("ADR-0013 must no longer claim the pr-review SKILL.md Step 4b wrapper is the primary record site (issue #1757)")
	}
	if strings.Contains(text, "the skill no longer maintains `.sandman/reviews/self-posted.json`") {
		t.Errorf("ADR-0013 must no longer reference the deleted self-posted.json store (issues #1845/#1848)")
	}
}
