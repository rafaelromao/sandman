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
