package github

import "testing"

func TestPR_LinkedIssueNumber(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{name: "empty_body", body: "", want: 0},
		{name: "no_keyword", body: "This PR adds a feature", want: 0},
		{name: "fixes_keyword", body: "Fixes #42", want: 42},
		{name: "closes_keyword", body: "Closes #100", want: 100},
		{name: "resolves_keyword", body: "Resolves #7", want: 7},
		{name: "lowercase", body: "fixes #42", want: 42},
		{name: "multiline", body: "## Changes\n\nSome changes.\n\nFixes #55", want: 55},
		{name: "multiple_matches", body: "Fixes #10 and closes #20", want: 10},
		{name: "no_number", body: "Fixes #", want: 0},
		{name: "mid_word_no_match", body: "prefixes #42", want: 0},
		{name: "with_space", body: "Fixes  #42", want: 42},
		{name: "implements_keyword", body: "Implements #42", want: 42},
		{name: "implements_lowercase", body: "implements #99", want: 99},
		{name: "implements_with_closing", body: "Implements #10 and closes #20", want: 10},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pr := &PR{Body: tc.body}
			got := pr.LinkedIssueNumber()
			if got != tc.want {
				t.Errorf("LinkedIssueNumber() = %d, want %d", got, tc.want)
			}
		})
	}
}
