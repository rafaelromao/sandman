package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

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
	BaseRefName string
	UpdatedAt   time.Time
	// ClosedAt is when the pull request was closed; zero when it is open or
	// the lookup did not report it. Later comments or branch deletion move
	// UpdatedAt but never ClosedAt.
	ClosedAt time.Time
	// ReviewDecision, MergeStateStatus, and StatusCheckRollup are populated
	// by FindPRByBranch when available; they are empty when the underlying
	// `gh` invocation omits the columns (e.g. on PRs that never had a
	// review submitted or where the merge state is still being computed).
	// The T4 cheap-gate oracle reads these to decide whether to defer to T1
	// (Approved + CLEAN + green checks) or abstain (any other state).
	ReviewDecision    string
	MergeStateStatus  string
	StatusCheckRollup string
	// CIHasActiveChecks remains true when a failed check coexists with queued or
	// running siblings in the same current-head check rollup.
	CIHasActiveChecks bool
	// CIActiveWorkflowChecks preserves links for queued or running Actions jobs,
	// including rollups with no failed-check entry to identify their workflow.
	CIActiveWorkflowChecks []CIWorkflowCheck
	// CIActiveFailedWorkflowRunID is populated only when check execution links
	// identify exactly one workflow run containing both a failed job and an
	// active sibling. It is empty for unrelated, missing, or ambiguous links.
	CIActiveFailedWorkflowRunID string
	// CIFailedWorkflow identifies the sole failed workflow, even after all
	// its checks have completed. It is not evidence of an active rerun.
	CIFailedWorkflow *CIFailedWorkflow
	// CIMultipleFailedWorkflows is positive evidence of independent failures.
	CIMultipleFailedWorkflows bool
	// CIActiveFailedWorkflowRunFailedCheckNames preserves the failed check names
	// from that identified run so a recovery receipt for one job cannot defer
	// independent failures in the same workflow.
	CIActiveFailedWorkflowRunFailedCheckNames []string
	// CIActiveFailedWorkflowRunFailedChecks preserves each failed check's
	// display name and provider execution link. Names are not unique, so callers
	// must not use a display-name match alone to identify a failed job.
	CIActiveFailedWorkflowRunFailedChecks []CIFailedCheck
	// CIActiveFailedWorkflowRunCheckNames preserves every named check execution
	// from the identified workflow run, including active siblings. A rerun wait
	// must reject a selected job name that is ambiguous across that full set.
	CIActiveFailedWorkflowRunCheckNames []string
	// CIExecutionID identifies the observed check executions independently of
	// their status. A rerun can change this identity without changing the head.
	CIExecutionID      string
	linkedIssueNumber  int
	linkedIssueNumbers []int
}

// CIFailedCheck identifies one failed check execution in a workflow run.
// DetailsURL is the provider's execution link (which may contain a browser job
// number); it is not the GitHub API database ID used by rerun operations.
type CIFailedCheck struct {
	Name       string `json:"name"`
	DetailsURL string `json:"details_url"`
}

// CIWorkflowCheck identifies a workflow job check visible in the PR rollup.
type CIWorkflowCheck struct {
	Name       string
	DetailsURL string
	Repository string
	RunID      int64
}

var prClosingIssueRe = regexp.MustCompile(`\b(?i)(?:close(?:s|d)?|fix(?:es|ed)?|resolve(?:s|d)?)\s*:?\s*((?:#\d+)(?:(?:\s*,\s*(?:and\s+)?|\s+and\s+)#\d+)*)`)
var prImplementsIssueRe = regexp.MustCompile(`\b(?i)implements\s+#(\d+)`)
var prIssueNumberRe = regexp.MustCompile(`#(\d+)`)

// LinkedIssueNumber returns the linked issue number for the PR.
// It first checks the native closingIssuesReferences metadata from GitHub,
// then falls back to searching the PR body for any GitHub closing keyword.
func (pr *PR) LinkedIssueNumber() int {
	if pr.linkedIssueNumber > 0 {
		return pr.linkedIssueNumber
	}
	if pr.Body == "" {
		return 0
	}
	if match := prImplementsIssueRe.FindStringSubmatch(pr.Body); len(match) > 1 {
		if n, err := strconv.Atoi(strings.TrimSpace(match[1])); err == nil {
			return n
		}
	}
	for _, match := range prClosingIssueRe.FindAllStringSubmatch(pr.Body, -1) {
		if len(match) < 2 {
			continue
		}
		if references := prIssueNumberRe.FindStringSubmatch(match[1]); len(references) > 1 {
			if n, err := strconv.Atoi(strings.TrimSpace(references[1])); err == nil {
				return n
			}
		}
	}
	return 0
}

// ClosesIssue reports whether the PR has GitHub closing intent for issueNumber.
// Native closing metadata wins for manually linked PRs; the body fallback
// recognizes every closing-keyword form documented by GitHub.
func (pr *PR) ClosesIssue(issueNumber int) bool {
	if pr == nil || issueNumber <= 0 {
		return false
	}
	if pr.linkedIssueNumber == issueNumber {
		return true
	}
	for _, number := range pr.linkedIssueNumbers {
		if number == issueNumber {
			return true
		}
	}
	for _, match := range prClosingIssueRe.FindAllStringSubmatch(pr.Body, -1) {
		if len(match) < 2 {
			continue
		}
		for _, reference := range prIssueNumberRe.FindAllStringSubmatch(match[1], -1) {
			if len(reference) < 2 {
				continue
			}
			if number, err := strconv.Atoi(reference[1]); err == nil && number == issueNumber {
				return true
			}
		}
	}
	return false
}

// EnsureClosingReference returns a PR body with closing intent for issueNumber.
// A standalone non-closing reference is repaired in place; otherwise the
// canonical closing line is prepended without disturbing the existing body.
func EnsureClosingReference(body string, issueNumber int) (string, bool) {
	if issueNumber <= 0 || (&PR{Body: body}).ClosesIssue(issueNumber) {
		return body, false
	}

	canonical := fmt.Sprintf("Closes #%d", issueNumber)
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		for _, reference := range prIssueNumberRe.FindAllStringSubmatch(line, -1) {
			if len(reference) < 2 {
				continue
			}
			number, err := strconv.Atoi(reference[1])
			if err == nil && number == issueNumber {
				lines[i] = canonical
				return strings.Join(lines, "\n"), true
			}
		}
	}

	if strings.TrimSpace(body) == "" {
		return canonical, true
	}
	return canonical + "\n\n" + body, true
}

// PRComment holds a PR conversation comment fetched from the GitHub REST API.
type PRComment struct {
	ID          string
	Body        string
	AuthorLogin string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// PRReview holds a formal pull-request review supplied to the review daemon.
type PRReview struct {
	ID          string
	Body        string
	State       string
	CommitID    string
	AuthorLogin string
	CreatedAt   time.Time
}

// PRReviewComment holds an inline pull-request review comment supplied to the
// review daemon. The path and line identify the reviewed location when GitHub
// provides them.
type PRReviewComment struct {
	ID          string
	Body        string
	Path        string
	Line        int
	CommitID    string
	AuthorLogin string
	CreatedAt   time.Time
}

// PRReviewLister is an optional capability for clients that can supply formal
// review history without widening the core Client interface.
type PRReviewLister interface {
	ListPRReviews(ctx context.Context, number int) ([]PRReview, error)
}

// PRReviewCommentLister is an optional capability for clients that can supply
// inline review history without widening the core Client interface.
type PRReviewCommentLister interface {
	ListPRReviewComments(ctx context.Context, number int) ([]PRReviewComment, error)
}

// CIWorkflowJob is a job in the latest attempt of a GitHub Actions workflow
// run. Database IDs are used by the rerun endpoint; browser URLs are not.
type CIWorkflowJob struct {
	ID                 int64            `json:"id"`
	RunID              int64            `json:"run_id"`
	RunAttempt         int              `json:"run_attempt"`
	Name               string           `json:"name"`
	Status             string           `json:"status"`
	Conclusion         string           `json:"conclusion"`
	RunnerID           int64            `json:"runner_id"`
	RunnerName         string           `json:"runner_name"`
	CheckRunURL        string           `json:"check_run_url"`
	Steps              []CIWorkflowStep `json:"steps"`
	runnerIDObserved   bool
	runnerNameObserved bool
	stepsObserved      bool
}

func (j *CIWorkflowJob) UnmarshalJSON(data []byte) error {
	type workflowJobAlias CIWorkflowJob
	var decoded workflowJobAlias
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	*j = CIWorkflowJob(decoded)
	_, j.runnerIDObserved = fields["runner_id"]
	_, j.runnerNameObserved = fields["runner_name"]
	_, j.stepsObserved = fields["steps"]
	return nil
}

// CIWorkflowStep is one executed or skipped step in a workflow job.
type CIWorkflowStep struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	Number     int    `json:"number"`
}

// CIRerunClient is an optional capability for observing and rerunning one
// current-attempt Actions job. It is intentionally separate from Client so
// clients without this mutation surface remain valid and fail closed.
type CIRerunClient interface {
	ListWorkflowJobs(ctx context.Context, runID string) ([]CIWorkflowJob, error)
	RerunWorkflowJob(ctx context.Context, jobID int64) error
}

// ManagedCIRequest binds a managed-CI evidence lookup to the live pull request
// identity and the failed workflow/check execution observed in its rollup.
type ManagedCIRequest struct {
	Repository          string
	PullRequest         int
	BaseRefName         string
	HeadSHA             string
	FailedWorkflowRunID string
	FailedChecks        []CIFailedCheck
}

// ManagedCIFailureClassification distinguishes explicit hosted-runner
// acquisition failures from confirmed source failures and unexplained ones.
type ManagedCIFailureClassification string

const (
	ManagedCIRunnerShortage           ManagedCIFailureClassification = "runner-shortage"
	ManagedCIConfirmedCodeTestFailure ManagedCIFailureClassification = "confirmed-code-test-failure"
	ManagedCIDependentFailure         ManagedCIFailureClassification = "explicitly-dependent-failure"
	ManagedCIUnexplainedFailure       ManagedCIFailureClassification = "unexplained"
)

// ManagedCIExecution is the identity of one Actions workflow execution.
type ManagedCIExecution struct {
	Repository  string
	PullRequest int
	BaseRefName string
	HeadSHA     string
	WorkflowID  int64
	Event       string
	RunID       int64
	RunNumber   int64
	Attempt     int
	Status      string
	Conclusion  string
}

// ManagedCIFailureEvidence classifies one failed check execution by its
// provider job identity, never by a display name alone.
type ManagedCIFailureEvidence struct {
	JobID           int64
	RunID           int64
	Attempt         int
	CheckName       string
	DetailsURL      string
	Classification  ManagedCIFailureClassification
	CausedByChecks  []string
	CausedByResults []string
}

// ManagedCIJobEvidence identifies every job in the observed workflow attempt.
// It lets aggregate prerequisites bind against the full attempt, not only the
// failed checks currently visible in the PR rollup.
type ManagedCIJobEvidence struct {
	JobID      int64
	RunID      int64
	Attempt    int
	Name       string
	Status     string
	Conclusion string
}

// ManagedCIObservation contains read-only evidence for the observed workflow
// and any verified newer execution.
type ManagedCIObservation struct {
	Source              ManagedCIExecution
	SourceJobs          []ManagedCIJobEvidence
	Failures            []ManagedCIFailureEvidence
	Replacement         *ManagedCIExecution
	ReplacementFailures []ManagedCIFailureEvidence
}

// ManagedCIObserver is an optional read-only Actions evidence capability.
// Clients without it preserve ordinary CI lifecycle behavior.
// Implementations may return validated source evidence with an error when a
// later replacement lookup fails; the error leaves replacement evidence
// inconclusive.
type ManagedCIObserver interface {
	ObserveManagedCI(ctx context.Context, request ManagedCIRequest) (ManagedCIObservation, error)
}

// ManagedCIExecutionFailureObserver diagnoses the failed jobs for one
// identity-verified, completed Actions execution without relying on the PR
// check rollup to expose those jobs.
type ManagedCIExecutionFailureObserver interface {
	DiagnoseManagedCIExecutionFailure(
		ctx context.Context,
		execution ManagedCIExecution,
	) ([]ManagedCIFailureEvidence, error)
}

// IssueComment holds an issue conversation comment fetched from the GitHub
// REST API. These are the comments posted on an issue (not a PR), used by
// Specification expansion to discover child references that live in the conversation
// rather than the issue body.
type IssueComment struct {
	ID        string
	Body      string
	CreatedAt time.Time
}

// Client wraps gh CLI for GitHub operations. Every method takes a
// context.Context so callers can cancel a hung gh invocation; the
// underlying CLIClient caps each call by its timeout and the caller's deadline,
// with bounded availability retries for reads. See internal/github/cli_client.go.
type Client interface {
	FetchIssue(ctx context.Context, number int) (*Issue, error)
	FetchIssueDependencies(ctx context.Context, number int) ([]int, error)
	FetchPR(ctx context.Context, number int) (*PR, error)
	FindPRByBranch(ctx context.Context, branch string) (*PR, error)
	SearchIssues(ctx context.Context, query string) ([]Issue, error)
	ListOpenPRs(ctx context.Context) ([]PR, error)
	ListPRComments(ctx context.Context, number int) ([]PRComment, error)
	AuthenticatedLogin(ctx context.Context) (string, error)
	ListIssueComments(ctx context.Context, number int) ([]IssueComment, error)
	ListSubIssues(ctx context.Context, parent int) ([]int, error)
	RepoName(ctx context.Context) (string, error)
	EditComment(ctx context.Context, commentID, body string) error
	EditPRBody(ctx context.Context, prNumber int, body string) error
	AddCommentReaction(ctx context.Context, commentID, content string) (string, error)
	AddIssueReaction(ctx context.Context, issueNumber int, content string) (string, error)
	RemoveCommentReaction(ctx context.Context, commentID, reactionID string) error
	RemoveIssueReaction(ctx context.Context, issueNumber int, reactionID string) error
	ClosePR(ctx context.Context, prNumber int) error
}

// IssueContentFetcher optionally provides a fresh issue payload without
// resolving BlockedBy. Callers that only render an AgentRun prompt should use
// it to avoid loading dependency metadata they do not consume.
type IssueContentFetcher interface {
	FetchIssueContent(ctx context.Context, number int) (*Issue, error)
}

// IssueStateFetcher optionally provides fresh state for a blocker recheck
// without resolving its dependency graph.
type IssueStateFetcher interface {
	FetchIssueState(ctx context.Context, number int) (string, error)
}

// RateLimitError marks a GitHub CLI failure that explicitly reports a rate
// limit. It retains the underlying command error for errors.Is/As callers.
type RateLimitError struct {
	Err        error
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string { return e.Err.Error() }
func (e *RateLimitError) Unwrap() error { return e.Err }

// IsRateLimited reports whether GitHub explicitly rejected a request for a
// primary or secondary rate limit.
func IsRateLimited(err error) bool {
	var target *RateLimitError
	return errors.As(err, &target)
}

// OpenIssueLister is the optional capability the Specification
// resolver type-asserts against to run the last-resort open-issue
// scan (see ADR-0044). Production `CLIClient` satisfies it via
// `gh api repos/<owner>/<repo>/issues?state=open --paginate` with
// a client-side pull-request filter; existing test fakes do not,
// which preserves their behaviour without modifying them. See
// `CLIClient.ListOpenIssues` for the full invocation and PR filter.
type OpenIssueLister interface {
	ListOpenIssues(ctx context.Context) ([]Issue, error)
}

// IssueCommentPoster is the optional capability the Specification
// resolver type-asserts against to persist auto-discovered children
// as a marker comment on the spec (see ADR-0044). Production
// `CLIClient` satisfies it via `gh issue comment`; existing test
// fakes do not, so the new harvest step silently no-ops on them.
type IssueCommentPoster interface {
	PostIssueComment(ctx context.Context, issueNumber int, body string) error
}
