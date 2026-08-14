package sandman

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var (
	internalPackagePathRe  = regexp.MustCompile(`internal/(review|cmd|batch|daemon|skill|prompt|runid)/`)
	internalGoIdentifierRe = regexp.MustCompile(`processPR|MarkSeen|ParseTrigger|launchReview|RunSession|PrepareReviewRun|runid\.|batch\.Request`)
	sandmanPathRe          = regexp.MustCompile(`\.sandman/`)
	issueTrackerJargonRe   = regexp.MustCompile(`issue #\d+|PR #\d+|GitHub issue|triage|kanban|ready-for-agent`)
	ghCliInProseRe         = regexp.MustCompile(`gh (issue|pr|api|repo) (create|view|list|edit|comment|close)`)
)

func readSkillMarkdown(t *testing.T) map[string]string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	files := map[string]string{}
	walkErr := filepath.WalkDir(wd, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !strings.EqualFold(filepath.Ext(path), ".md") {
			return nil
		}
		rel, err := filepath.Rel(wd, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files[rel] = string(data)
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk skill tree: %v", walkErr)
	}
	if len(files) == 0 {
		t.Fatalf("expected at least one .md file under %s", wd)
	}
	return files
}

func TestSkills_NoInternalPackagePaths(t *testing.T) {
	files := readSkillMarkdown(t)
	for path, text := range files {
		if loc := internalPackagePathRe.FindStringIndex(text); loc != nil {
			t.Errorf("%s contains forbidden internal package path %q at offset %d", path, text[loc[0]:loc[1]], loc[0])
		}
	}
}

func TestSkills_NoInternalGoIdentifiers(t *testing.T) {
	files := readSkillMarkdown(t)
	for path, text := range files {
		if loc := internalGoIdentifierRe.FindStringIndex(text); loc != nil {
			t.Errorf("%s contains forbidden internal Go identifier at offset %d: %q", path, loc[0], text[loc[0]:loc[1]])
		}
	}
}

func TestSkills_PreserveUserFacingPaths(t *testing.T) {
	files := readSkillMarkdown(t)
	hits := 0
	for path, text := range files {
		if sandmanPathRe.MatchString(text) {
			hits++
			t.Logf("%s preserves user-facing .sandman/ path", path)
		}
	}
	if hits == 0 {
		t.Fatalf("expected at least one .md to reference a .sandman/ path, found none across %d files", len(files))
	}
}

func TestSkills_NoIssueTrackerReferences(t *testing.T) {
	files := readSkillMarkdown(t)
	for path, text := range files {
		if loc := issueTrackerJargonRe.FindStringIndex(text); loc != nil {
			t.Errorf("%s contains forbidden issue tracker jargon %q at offset %d", path, text[loc[0]:loc[1]], loc[0])
		}
	}
}

func stripCodeFences(text string) string {
	var b strings.Builder
	inFence := false
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

func TestSkills_NoGhCliReferencesInProse(t *testing.T) {
	files := readSkillMarkdown(t)
	for path, text := range files {
		prose := stripCodeFences(text)
		if loc := ghCliInProseRe.FindStringIndex(prose); loc != nil {
			t.Errorf("%s contains forbidden gh CLI reference in prose %q at offset %d", path, prose[loc[0]:loc[1]], loc[0])
		}
	}
}

func TestSkills_NoOperatorResponseDirectives(t *testing.T) {
	files := readSkillMarkdown(t)
	forbidden := []struct {
		name    string
		pattern string
	}{
		{name: "operator question", pattern: `(?i)\bask (the )?user\b`},
		{name: "operator direction request", pattern: `(?i)\bask for (direction|clarification|approval|confirmation|feedback)\b`},
		{name: "operator response request", pattern: `(?i)\b(request|await|wait for|seek)\s+(the\s+)?(user|operator)\b`},
		{name: "operator-dependent stop", pattern: `(?i)\b(stop|pause|wait)( and)? ask\b`},
		{name: "operator decision gate", pattern: `(?i)\b(user|operator)'?s?\s+(approval|confirmation|clarification|direction|feedback|decision|review)\b`},
		{name: "operator decision source", pattern: `(?i)\b(approval|confirmation|clarification|direction|feedback|decision|review)\s+from\s+(the\s+)?(user|operator)\b`},
		{name: "operator satisfaction gate", pattern: `(?i)\buser (is )?(satisfied|confirmed|approval|confirmation|direction|clarification|feedback|review)\b`},
		{name: "operator status report", pattern: `(?i)\b(report|tell) .* to the user\b`},
		{name: "operator stop choice", pattern: `(?i)\bexplicit user stop\b`},
	}

	for _, rule := range forbidden {
		re := regexp.MustCompile(rule.pattern)
		for path, text := range files {
			for _, loc := range re.FindAllStringIndex(text, -1) {
				if allowlistedSkillCommunication(path, text, loc) {
					continue
				}
				t.Errorf("%s contains forbidden %s %q at offset %d", path, rule.name, text[loc[0]:loc[1]], loc[0])
			}
		}
	}
}

func allowlistedSkillCommunication(path, text string, loc []int) bool {
	if path != "pr-review/SKILL.md" {
		return false
	}
	lineStart := strings.LastIndex(text[:loc[0]], "\n") + 1
	lineEnd := strings.Index(text[loc[1]:], "\n")
	if lineEnd < 0 {
		lineEnd = len(text)
	} else {
		lineEnd += loc[1]
	}
	line := strings.ToLower(text[lineStart:lineEnd])
	if strings.Contains(line, "user") || strings.Contains(line, "operator") || !strings.Contains(line, "{{review_command}}") {
		return false
	}
	for _, phrase := range []string{
		"asking the reviewer to clarify",
		"ask the reviewer to clarify",
		"reviewer-directed clarification",
	} {
		if strings.Contains(line, phrase) {
			return true
		}
	}
	return false
}

func TestSkills_DirectiveAllowlistIsReviewerScoped(t *testing.T) {
	cases := []struct {
		name string
		path string
		text string
		want bool
	}{
		{
			name: "reviewer clarification",
			path: "pr-review/SKILL.md",
			text: "ask the reviewer to clarify with {{REVIEW_COMMAND}}",
			want: true,
		},
		{
			name: "operator clarification",
			path: "pr-review/SKILL.md",
			text: "ask the user for clarification with {{REVIEW_COMMAND}}",
			want: false,
		},
		{
			name: "same wording in another skill",
			path: "implement/SKILL.md",
			text: "ask for clarification from the reviewer with {{REVIEW_COMMAND}}",
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			start := strings.Index(strings.ToLower(tc.text), "clarif")
			loc := []int{start, start + len("clarif")}
			if got := allowlistedSkillCommunication(tc.path, tc.text, loc); got != tc.want {
				t.Fatalf("allowlistedSkillCommunication() = %t, want %t", got, tc.want)
			}
		})
	}
}

func TestSkills_ReviewerClarificationUsesReviewCommand(t *testing.T) {
	text, ok := readSkillMarkdown(t)["pr-review/SKILL.md"]
	if !ok {
		t.Fatal("expected pr-review/SKILL.md")
	}
	for _, phrase := range []string{
		"reviewer to clarify",
		"{{REVIEW_COMMAND}}",
		"reviewer-directed",
	} {
		if !strings.Contains(text, phrase) {
			t.Errorf("pr-review skill must preserve reviewer-directed clarification phrase %q", phrase)
		}
	}
}

func TestSkillsDocumentation_DescribesAFKWorkflow(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "usage", "skills.md"))
	if err != nil {
		t.Fatalf("read user-facing skills documentation: %v", err)
	}
	text := string(data)
	if !strings.Contains(text, "AFK") {
		t.Fatal("user-facing skills documentation must identify the shared workflow as AFK")
	}
	for _, forbidden := range []string{
		"interactive Sandman-guided session",
		"answer questions",
		"steer the work in real time",
	} {
		if strings.Contains(text, forbidden) {
			t.Errorf("user-facing skills documentation must not advertise operator interaction %q", forbidden)
		}
	}
}

func TestSkills_AutonomousRecoveryLaddersRemainExplicit(t *testing.T) {
	files := readSkillMarkdown(t)
	checks := map[string][]string{
		"implement/SKILL.md": {
			".sandman/task.md",
			"structured blocker",
			"next executable action",
			"the next run continues from the durable blocker",
		},
		"back-merge/SKILL.md": {
			".sandman/task.md",
			"preserve it, inspect the status and diff",
			"record the exact blocker and next executable action",
		},
		"code-review/SKILL.md": {
			"record the blocked self-review",
			"only the supplied packet",
			"Do not run `grep`, `rg`, or `find`",
			"`.sandman/task.md` and the run log",
		},
		"pr-review/SKILL.md": {
			".sandman/task.md",
			"reviewer-directed clarification",
			"60-minute budget per PR head SHA",
			"at most 3 fix-and-push attempts",
			"ci_deadline",
			"ci_fix_attempts",
			"The request envelope fixes that value",
			"review-wait-v1.sh",
			"REVIEW_TIMEOUT",
			"max 10 passes",
		},
	}

	for path, phrases := range checks {
		text, ok := files[path]
		if !ok {
			t.Fatalf("expected %s", path)
		}
		for _, phrase := range phrases {
			if !strings.Contains(text, phrase) {
				t.Errorf("%s must retain autonomous recovery behavior %q", path, phrase)
			}
		}
	}
}

func TestCodeReviewSkill_SeparatesSelfAndDaemonContexts(t *testing.T) {
	text, ok := readSkillMarkdown(t)["code-review/SKILL.md"]
	if !ok {
		t.Fatal("expected code-review/SKILL.md")
	}

	for _, phrase := range []string{
		"## Self-review context",
		"parent workflow supplies one bounded review packet",
		"## Standards",
		"## Spec",
		"## Pull-request review context",
		"Those inputs are authoritative.",
		"current review worktree",
		"atomic temp-file-and-rename write",
		"invoking pull-request review prompt defines the decision output contract",
	} {
		if !strings.Contains(text, phrase) {
			t.Errorf("code-review skill missing context contract %q", phrase)
		}
	}

	for _, forbidden := range []string{
		"trigger, poll, fetch, post to, merge, commit to, push to, or remediate",
	} {
		if !strings.Contains(text, "Do not "+forbidden) {
			t.Errorf("code-review daemon context must forbid pull-request orchestration %q", forbidden)
		}
	}
}

func TestCodeReviewSkill_SelfReviewUsesBoundedCommonPacket(t *testing.T) {
	text, ok := readSkillMarkdown(t)["code-review/SKILL.md"]
	if !ok {
		t.Fatal("expected code-review/SKILL.md")
	}

	start := strings.Index(text, "## Self-review context")
	end := strings.Index(text, "## Pull-request review context")
	if start < 0 || end < start {
		t.Fatal("could not isolate self-review context")
	}
	selfReview := text[start:end]

	for _, phrase := range []string{
		"parent workflow supplies one bounded review packet",
		"fixed-point identity",
		"commit list",
		"git diff <fixed-point>...HEAD",
		"staged tracked diff",
		"unstaged tracked diff",
		"untracked paths and their contents",
		"capture the packet once",
		"same packet to both reviewers",
		"only the supplied packet",
		"Do not run `grep`, `rg`, or `find`",
		"whole-repository searches",
	} {
		if !strings.Contains(selfReview, phrase) {
			t.Errorf("self-review context missing bounded-packet contract %q", phrase)
		}
	}

	for _, forbidden := range []string{
		"### 2. Identify review sources",
		"Find the originating work item",
		"Collect documented standards from",
	} {
		if strings.Contains(selfReview, forbidden) {
			t.Errorf("self-review context must not delegate broad discovery %q", forbidden)
		}
	}
}

func TestImplementSkill_SelfReviewParentSelectsBoundedContext(t *testing.T) {
	text, ok := readSkillMarkdown(t)["implement/SKILL.md"]
	if !ok {
		t.Fatal("expected implement/SKILL.md")
	}

	for _, phrase := range []string{
		"enumerate the changed paths",
		"select only standards sources relevant to those paths",
		"supply those standards sources explicitly",
		"authoritative task/specification context",
		"Standards reviewer",
		"Spec reviewer",
		"same common packet",
		"do not ask reviewers to explore",
	} {
		if !strings.Contains(text, phrase) {
			t.Errorf("implement workflow missing parent self-review contract %q", phrase)
		}
	}
}

func TestCodeReviewSkill_SelfReviewFailsClosedOnMissingPacket(t *testing.T) {
	text, ok := readSkillMarkdown(t)["code-review/SKILL.md"]
	if !ok {
		t.Fatal("expected code-review/SKILL.md")
	}

	start := strings.Index(text, "## Self-review context")
	end := strings.Index(text, "## Pull-request review context")
	if start < 0 || end < start {
		t.Fatal("could not isolate self-review context")
	}
	selfReview := text[start:end]

	for _, phrase := range []string{
		"missing or malformed",
		"record the blocked self-review in `.sandman/task.md`",
		"stop without reconstructing",
		"only the supplied packet",
		"must not invite repository exploration",
	} {
		if !strings.Contains(selfReview, phrase) {
			t.Errorf("self-review context missing fail-closed contract %q", phrase)
		}
	}
}

func TestCodeReviewSkill_DaemonContextDoesNotUseSelfReviewPacket(t *testing.T) {
	text, ok := readSkillMarkdown(t)["code-review/SKILL.md"]
	if !ok {
		t.Fatal("expected code-review/SKILL.md")
	}

	start := strings.Index(text, "## Pull-request review context")
	if start < 0 {
		t.Fatal("could not locate pull-request review context")
	}
	daemonReview := text[start:]
	for _, forbidden := range []string{
		"fixed-point identity",
		"same common packet",
		"authoritative task/specification context",
		"Do not run `grep`, `rg`, or `find`",
	} {
		if strings.Contains(daemonReview, forbidden) {
			t.Errorf("daemon review context must not inherit self-review packet rule %q", forbidden)
		}
	}

	for _, phrase := range []string{
		"Those inputs are authoritative.",
		"current review worktree",
		"atomic temp-file-and-rename write",
		"Do not trigger, poll, fetch, post to, merge, commit to, push to, or remediate",
	} {
		if !strings.Contains(daemonReview, phrase) {
			t.Errorf("daemon review context lost existing input/lifecycle contract %q", phrase)
		}
	}
}

func TestSkills_NoObsoleteSelfReviewSkillReference(t *testing.T) {
	obsolete := "sandman-self" + "-review"
	for path, text := range readSkillMarkdown(t) {
		if strings.Contains(text, obsolete) {
			t.Errorf("%s retains obsolete skill reference %q", path, obsolete)
		}
	}
}

func TestSkills_ImplementSkillStillReadable(t *testing.T) {
	files := readSkillMarkdown(t)
	const target = "implement/SKILL.md"
	text, ok := files[target]
	if !ok {
		t.Fatalf("expected %s to exist under skill tree, found %d files", target, len(files))
	}
	lines := strings.Split(text, "\n")
	var descLine string
	var h1Line string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if descLine == "" && strings.HasPrefix(trimmed, "description:") {
			descLine = trimmed
		}
		if h1Line == "" && strings.HasPrefix(trimmed, "# ") {
			h1Line = trimmed
		}
	}
	if descLine == "" {
		t.Errorf("%s has no non-empty frontmatter description line", target)
		return
	}
	if !strings.Contains(text, "End-to-end automation for implementing") {
		t.Errorf("%s missing the entry-point signal phrase %q", target, "End-to-end automation for implementing")
	}
	if h1Line == "" {
		t.Errorf("%s has no H1 heading", target)
		return
	}
	if !strings.HasPrefix(strings.TrimSpace(h1Line), "# implement") {
		t.Errorf("%s H1 %q does not start with the literal entry-point heading %q", target, h1Line, "# implement")
	}
}
