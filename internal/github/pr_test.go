package github

import "testing"

func TestPR_LinkedIssueNumber(t *testing.T) {
	cases := []struct {
		name string
		pr   PR
		want int
	}{
		{name: "empty_body", pr: PR{Body: ""}, want: 0},
		{name: "no_keyword", pr: PR{Body: "This PR adds a feature"}, want: 0},
		{name: "fixes_keyword", pr: PR{Body: "Fixes #42"}, want: 42},
		{name: "closes_keyword", pr: PR{Body: "Closes #100"}, want: 100},
		{name: "resolves_keyword", pr: PR{Body: "Resolves #7"}, want: 7},
		{name: "lowercase", pr: PR{Body: "fixes #42"}, want: 42},
		{name: "multiline", pr: PR{Body: "## Changes\n\nSome changes.\n\nFixes #55"}, want: 55},
		{name: "multiple_matches", pr: PR{Body: "Fixes #10 and closes #20"}, want: 10},
		{name: "no_number", pr: PR{Body: "Fixes #"}, want: 0},
		{name: "mid_word_no_match", pr: PR{Body: "prefixes #42"}, want: 0},
		{name: "with_space", pr: PR{Body: "Fixes  #42"}, want: 42},
		{name: "implements_keyword", pr: PR{Body: "Implements #42"}, want: 42},
		{name: "implements_lowercase", pr: PR{Body: "implements #99"}, want: 99},
		{name: "implements_with_closing", pr: PR{Body: "Implements #10 and closes #20"}, want: 10},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.pr.LinkedIssueNumber()
			if got != tc.want {
				t.Errorf("LinkedIssueNumber() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestPR_LinkedIssueNumber_NativeClosingReference(t *testing.T) {
	pr := PR{Body: "", linkedIssueNumber: 99}
	got := pr.LinkedIssueNumber()
	if got != 99 {
		t.Errorf("LinkedIssueNumber() = %d, want 99 (from native closingIssuesReferences)", got)
	}
}

func TestPR_LinkedIssueNumber_NativeTakesPrecedence(t *testing.T) {
	pr := PR{Body: "Fixes #10", linkedIssueNumber: 99}
	got := pr.LinkedIssueNumber()
	if got != 99 {
		t.Errorf("LinkedIssueNumber() = %d, want 99 (native should take precedence over body)", got)
	}
}

func TestPR_ClosesIssueRecognizesGitHubClosingKeywords(t *testing.T) {
	for _, body := range []string{
		"Close #348",
		"Closes #348",
		"Closed #348",
		"Fix #348",
		"Fixes: #348",
		"Fixed #348",
		"Resolve #348",
		"Resolves #348",
		"Resolved: #348",
		"Closes #348, #349",
		"Resolves #348 and #349",
	} {
		t.Run(body, func(t *testing.T) {
			if !(&PR{Body: body}).ClosesIssue(348) {
				t.Fatalf("ClosesIssue(348) = false for %q", body)
			}
		})
	}

	for _, body := range []string{"Refs #348", "Implements #348", "Closes #349"} {
		t.Run(body, func(t *testing.T) {
			if (&PR{Body: body}).ClosesIssue(348) {
				t.Fatalf("ClosesIssue(348) = true for non-closing body %q", body)
			}
		})
	}
}

func TestPR_ClosesIssueRecognizesEveryIssueInClosingReferenceList(t *testing.T) {
	pr := &PR{Body: "Fixes #10, #15, and #20"}
	for _, issueNumber := range []int{10, 15, 20} {
		if !pr.ClosesIssue(issueNumber) {
			t.Fatalf("ClosesIssue(%d) = false", issueNumber)
		}
	}
}

func TestEnsureClosingReference(t *testing.T) {
	cases := []struct {
		name        string
		body        string
		wantBody    string
		wantChanged bool
	}{
		{
			name:        "repairs refs line in place",
			body:        "Refs #348\n\nAcceptance evidence.",
			wantBody:    "Closes #348\n\nAcceptance evidence.",
			wantChanged: true,
		},
		{
			name:        "preserves valid closing reference",
			body:        "Fixes: #348\n\nAcceptance evidence.",
			wantBody:    "Fixes: #348\n\nAcceptance evidence.",
			wantChanged: false,
		},
		{
			name:        "prepends when reference is missing",
			body:        "Acceptance evidence.",
			wantBody:    "Closes #348\n\nAcceptance evidence.",
			wantChanged: true,
		},
		{
			name:        "does not replace another issue",
			body:        "Refs #349\n\nAcceptance evidence.",
			wantBody:    "Closes #348\n\nRefs #349\n\nAcceptance evidence.",
			wantChanged: true,
		},
		{
			name:        "redacts an unsupported verb",
			body:        "Addresses #348\n\nAcceptance evidence.",
			wantBody:    "Closes #348\n\nAcceptance evidence.",
			wantChanged: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, changed := EnsureClosingReference(tc.body, 348)
			if body != tc.wantBody {
				t.Fatalf("body = %q, want %q", body, tc.wantBody)
			}
			if changed != tc.wantChanged {
				t.Fatalf("changed = %v, want %v", changed, tc.wantChanged)
			}
		})
	}
}

// TestPR_ReviewFieldsRoundTrip verifies the slice-1 PR struct extension:
// the three new fields (ReviewDecision, MergeStateStatus, StatusCheckRollup)
// survive construction so the T4 cheap-gate oracle can read them off the
// existing FindPRByBranch result without re-fetching.
func TestPR_ReviewFieldsRoundTrip(t *testing.T) {
	pr := PR{
		Number:            17,
		State:             "open",
		HeadRefName:       "issue-2165/spec",
		ReviewDecision:    "APPROVED",
		MergeStateStatus:  "CLEAN",
		StatusCheckRollup: "success",
	}
	if pr.ReviewDecision != "APPROVED" {
		t.Errorf("ReviewDecision = %q, want APPROVED", pr.ReviewDecision)
	}
	if pr.MergeStateStatus != "CLEAN" {
		t.Errorf("MergeStateStatus = %q, want CLEAN", pr.MergeStateStatus)
	}
	if pr.StatusCheckRollup != "success" {
		t.Errorf("StatusCheckRollup = %q, want success", pr.StatusCheckRollup)
	}
}
