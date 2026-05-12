package github

// Issue holds metadata fetched from GitHub.
type Issue struct {
	Number int
	Title  string
	Body   string
	Labels []string
}

// Client wraps gh CLI for GitHub operations.
type Client interface {
	FetchIssue(number int) (*Issue, error)
	SearchIssues(query string) ([]Issue, error)
}
