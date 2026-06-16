package github

import "testing"

func TestIsIssueClosed(t *testing.T) {
	cases := []struct {
		name  string
		issue *Issue
		want  bool
	}{
		{name: "nil_issue_is_not_closed", issue: nil, want: false},
		{name: "empty_state_is_not_closed", issue: &Issue{State: ""}, want: false},
		{name: "lowercase_closed", issue: &Issue{State: "closed"}, want: true},
		{name: "uppercase_closed", issue: &Issue{State: "CLOSED"}, want: true},
		{name: "mixed_case_closed", issue: &Issue{State: "Closed"}, want: true},
		{name: "whitespace_around_closed", issue: &Issue{State: "  closed  "}, want: true},
		{name: "open_is_not_closed", issue: &Issue{State: "open"}, want: false},
		{name: "open_with_whitespace_is_not_closed", issue: &Issue{State: "  open  "}, want: false},
		{name: "unrelated_state_is_not_closed", issue: &Issue{State: "merged"}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsIssueClosed(tc.issue); got != tc.want {
				t.Errorf("IsIssueClosed(%+v) = %v, want %v", tc.issue, got, tc.want)
			}
		})
	}
}
