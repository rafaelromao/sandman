package github

// Issue holds metadata fetched from GitHub.
type Issue struct {
	Number int
	State  string
	Title  string
	Body   string
	Labels []string
	// BlockedBy is populated by FetchIssue. SearchIssues leaves it empty.
	BlockedBy []int
}

// PR holds pull request metadata fetched from GitHub.
type PR struct {
	Number      int
	State       string
	Title       string
	Body        string
	Merged      bool
	HeadRefName string
	HeadRefOid  string
}

// Client wraps gh CLI for GitHub operations.
type Client interface {
	FetchIssue(number int) (*Issue, error)
	FetchIssueDependencies(number int) ([]int, error)
	FetchPR(number int) (*PR, error)
	FindPRByBranch(branch string) (*PR, error)
	SearchIssues(query string) ([]Issue, error)
}
