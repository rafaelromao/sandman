package review

import "testing"

func TestIsReviewRequest(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{
			name: "implementor trigger with focus",
			body: "/sandman review focus on tests",
			want: true,
		},
		{
			name: "implementor trigger standalone",
			body: "/sandman review",
			want: true,
		},
		{
			name: "mention-prefixed trigger",
			body: "@bot /sandman review focus x",
			want: true,
		},
		{
			name: "mention-prefixed with thanks for the review",
			body: "@sandman-bot thanks for the review!",
			want: true,
		},
		{
			name: "leading whitespace then slash trigger",
			body: "   /sandman review",
			want: true,
		},
		{
			name: "slash-prefixed unrelated contains review",
			body: "/foo review something",
			want: true,
		},
		{
			name: "case-insensitive Review in slash-prefixed body",
			body: "/sandman Review with focus",
			want: true,
		},
		{
			name: "bot-shaped self-post does not start with slash or at",
			body: "## Previous review progress\nFirst review pass on PR #1809.",
			want: false,
		},
		{
			name: "plain LGTM human review",
			body: "LGTM, no blockers.",
			want: false,
		},
		{
			name: "human reviewer with body review word but no slash/at prefix",
			body: "Looks good overall. One nit on the review helper.",
			want: false,
		},
		{
			name: "empty body",
			body: "",
			want: false,
		},
		{
			name: "slash without review word",
			body: "/some-command",
			want: false,
		},
		{
			name: "at without review word",
			body: "@alice ping",
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := IsReviewRequest(tc.body)
			if got != tc.want {
				t.Errorf("IsReviewRequest(%q) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}
