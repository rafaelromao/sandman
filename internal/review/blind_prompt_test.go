package review

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
)

func TestBlindPromptTemplateMatchesDaemon(t *testing.T) {
	body, err := os.ReadFile("../skill/sandman/blind-review/review-prompt.md")
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != prompt.DefaultPRReviewPrompt() {
		t.Fatal("bundled blind review template differs from daemon template")
	}
}

func TestBlindPromptHelperMatchesDaemon(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq unavailable")
	}
	helper, err := filepath.Abs("../skill/sandman/blind-review/review-prompt-v1.sh")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.ReadFile(helper); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name           string
		prior          bool
		noIssue        bool
		bodyLink       int
		nativeIssue    int
		nativePage2    bool
		linkageError   bool
		linkageBad     bool
		linkagePartial bool
		override       string
		emptyOverride  bool
		historyError   bool
	}{
		{name: "first"},
		{name: "prior", prior: true},
		{name: "unlinked", noIssue: true},
		{name: "native-link-precedes-body-fallback", bodyLink: 99, nativeIssue: 42},
		{name: "native-link-on-later-page", bodyLink: 99, nativeIssue: 42, nativePage2: true},
		{name: "linkage-lookup-failure-does-not-fall-back", bodyLink: 99, linkageError: true},
		{name: "linkage-invalid-response-does-not-fall-back", bodyLink: 99, linkageBad: true},
		{name: "linkage-incomplete-pagination-does-not-fall-back", bodyLink: 99, linkagePartial: true},
		{name: "override", override: "Custom: {{PR_TITLE}}\n{{ACCEPTANCE_CRITERIA}}\n{{PRIOR_REVIEW_CONTEXT}}\n"},
		{name: "empty-override", emptyOverride: true},
		{name: "whitespace-override", override: " \t\r\n"},
		{name: "unicode-whitespace-override", override: "\u0085\u00a0\u1680\u2000\u2028\u2029\u202f\u205f\u3000"},
		{name: "history-error", historyError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			git := exec.Command("git", "init", "-q", dir)
			if output, err := git.CombinedOutput(); err != nil {
				t.Fatalf("git init: %v\n%s", err, output)
			}
			gh := `#!/bin/sh
set -eu
case "$*" in
  "pr view "*)
    body="Closes #${BODY_LINK:-42}"
    [ "$NO_ISSUE" = false ] || body="No linked work item."
    jq -cn --arg body "$body" '{number:17,title:"A {{key}}",body:$body,state:"OPEN",headRefOid:"abc",baseRefName:"main",closingIssuesReferences:[]}' ;;
  "api graphql "*)
    [ "$GRAPHQL_ERROR" = false ] || exit 11
    printf '%s\n' "$GRAPHQL_RESPONSE" ;;
  "issue view "*)
    [ "$3" != 99 ] || { printf '%s\n' '{"body":"Wrong issue"}'; exit 0; }
    printf '%s\n' '{"body":"Meet {{criteria}}"}' ;;
  *"issues/17/comments")
    [ "$HISTORY_ERROR" = false ] || exit 11
    if [ "$PRIOR" = true ]; then
      printf '%s\n' '[{"body":"/sandman review","created_at":"2026-01-01T00:00:00Z"},{"body":"Human review","created_at":"2026-01-02T00:00:00Z"},{"body":"Blind publication","created_at":"2026-01-03T00:00:00Z"}]'
    else printf '[]\n'; fi ;;
  *"pulls/17/reviews")
    if [ "$PRIOR" = true ]; then printf '%s\n' '[{"state":"COMMENTED","body":"Formal"}]'; else printf '[]\n'; fi ;;
  *"pulls/17/comments")
    if [ "$PRIOR" = true ]; then printf '%s\n' '[{"path":"file.go","line":7,"body":"Inline"}]'; else printf '[]\n'; fi ;;
  *) exit 10 ;;
esac
`
			if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(gh), 0755); err != nil {
				t.Fatal(err)
			}
			bodyLink := tc.bodyLink
			if bodyLink == 0 {
				bodyLink = 42
			}
			body := fmt.Sprintf("Closes #%d", bodyLink)
			if tc.noIssue {
				body = "No linked work item."
			}
			graphqlResponse := `{"data":{"repository":{"pullRequest":{"closingIssuesReferences":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}}}`
			if tc.nativeIssue > 0 && tc.nativePage2 {
				graphqlResponse = fmt.Sprintf("{\"data\":{\"repository\":{\"pullRequest\":{\"closingIssuesReferences\":{\"nodes\":[],\"pageInfo\":{\"hasNextPage\":true,\"endCursor\":\"cursor-1\"}}}}}}\n{\"data\":{\"repository\":{\"pullRequest\":{\"closingIssuesReferences\":{\"nodes\":[{\"number\":%d}],\"pageInfo\":{\"hasNextPage\":false,\"endCursor\":null}}}}}}", tc.nativeIssue)
			} else if tc.nativeIssue > 0 {
				graphqlResponse = fmt.Sprintf(`{"data":{"repository":{"pullRequest":{"closingIssuesReferences":{"nodes":[{"number":%d}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}}}`, tc.nativeIssue)
			} else if tc.linkageBad {
				graphqlResponse = `{"errors":[{"message":"partial data"}],"data":{"repository":null}}`
			} else if tc.linkagePartial {
				graphqlResponse = `{"data":{"repository":{"pullRequest":{"closingIssuesReferences":{"nodes":[],"pageInfo":{"hasNextPage":true,"endCursor":"cursor-1"}}}}}}`
			}
			cfg := prompt.RenderConfig{}
			if tc.override != "" || tc.emptyOverride {
				cfg.ReviewPromptFile = filepath.Join(dir, ".sandman", "reviews", "review-prompt.md")
				if err := os.MkdirAll(filepath.Dir(cfg.ReviewPromptFile), 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(cfg.ReviewPromptFile, []byte(tc.override), 0600); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.Command("sh", helper, "--repository", "owner/repo", "--pull-request", "17", "--head-sha", "abc", "--run-dir", "/outside/report")
			cmd.Dir = dir
			boolString := func(b bool) string {
				if b {
					return "true"
				}
				return "false"
			}
			cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"),
				"PRIOR="+boolString(tc.prior), "NO_ISSUE="+boolString(tc.noIssue), "HISTORY_ERROR="+boolString(tc.historyError),
				fmt.Sprintf("BODY_LINK=%d", bodyLink), "GRAPHQL_ERROR="+boolString(tc.linkageError),
				"GRAPHQL_RESPONSE="+graphqlResponse)
			got, err := cmd.CombinedOutput()
			if tc.linkageError || tc.linkageBad || tc.linkagePartial {
				want := "REVIEW_PROMPT_LINKAGE_INVALID"
				if tc.linkageError {
					want = "REVIEW_PROMPT_LINKAGE_LOOKUP_FAILED"
				}
				if err == nil || !strings.Contains(string(got), want) {
					t.Fatalf("linkage failure must preserve %s diagnostic: err=%v\n%s", want, err, got)
				}
				return
			}
			if tc.historyError {
				if err == nil {
					t.Fatal("history lookup failure must refuse to render")
				}
				return
			}
			if err != nil {
				t.Fatalf("render: %v\n%s", err, got)
			}
			var comments []github.PRComment
			var reviews []github.PRReview
			var inline []github.PRReviewComment
			if tc.prior {
				comments = []github.PRComment{
					{Body: "/sandman review", CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
					{Body: "Human review", CreatedAt: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)},
					{Body: "Blind publication", CreatedAt: time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC)},
				}
				reviews = []github.PRReview{{State: "COMMENTED", Body: "Formal"}}
				inline = []github.PRReviewComment{{Path: "file.go", Line: 7, Body: "Inline"}}
			}
			exists, history := PriorReviewContext(comments, reviews, inline)
			pr := &github.PR{Number: 17, Title: "A {{key}}", Body: body}
			criteria := "Meet {{criteria}}"
			if tc.noIssue {
				pr.Body, criteria = "No linked work item.", ""
			}
			want, err := (&prompt.Engine{}).RenderReview(cfg, ReviewPromptData(pr, &github.Issue{Number: 42, Body: criteria}, "", "/outside/report", exists, history))
			if err != nil {
				t.Fatal(err)
			}
			want = ReviewSnapshotHeader("abc", "main") + want
			if string(got) != want {
				t.Fatalf("helper render differs from daemon\n%s", firstPromptDifference(string(got), want))
			}
		})
	}
}

func firstPromptDifference(got, want string) string {
	g, w := strings.Split(got, "\n"), strings.Split(want, "\n")
	for i := 0; i < len(g) && i < len(w); i++ {
		if g[i] != w[i] {
			return "got: " + g[i] + "\nwant: " + w[i]
		}
	}
	return "line counts differ"
}
