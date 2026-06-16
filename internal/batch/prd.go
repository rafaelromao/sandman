package batch

import (
	"context"
	"fmt"
	"io"
	"os"
	"regexp"

	"github.com/rafaelromao/sandman/internal/github"
)

// PRDResolver resolves PRD issues to their child issues during batch preparation.
type PRDResolver struct {
	client        github.Client
	warningWriter io.Writer
	sectionREs    []*regexp.Regexp
}

// NewPRDResolver returns a resolver that expands any PRD issues in the input
// into their child issues. The warning writer receives one line per expansion
// or per dropped candidate.
func NewPRDResolver(client github.Client, warningWriter io.Writer) *PRDResolver {
	if warningWriter == nil {
		warningWriter = os.Stderr
	}
	required := []string{"Problem Statement", "Solution", "User Stories"}
	sectionREs := make([]*regexp.Regexp, len(required))
	for i, name := range required {
		sectionREs[i] = regexp.MustCompile(`(?im)^##\s+` + regexp.QuoteMeta(name) + `\s*$`)
	}
	return &PRDResolver{client: client, warningWriter: warningWriter, sectionREs: sectionREs}
}

// IsPRD reports whether the body contains the three required PRD sections
// as H2 headings: Problem Statement, Solution, and User Stories.
func (r *PRDResolver) IsPRD(body string) bool {
	for _, re := range r.sectionREs {
		if !re.MatchString(body) {
			return false
		}
	}
	return true
}

// Resolve is the entry point for PRD expansion. It walks the input list
// and replaces each PRD with its accepted child issues, removing the PRD
// itself and deduplicating across PRDs and explicit inputs. Non-PRD
// issues pass through unchanged.
//
// Errors:
//   - `no child issues for PRD #<n>` if a PRD has no accepted children
//   - `nested PRD detected: #<child>` if a candidate child is itself a PRD
//   - any FetchIssue error encountered while loading a candidate child
func (r *PRDResolver) Resolve(ctx context.Context, issues []int) ([]int, error) {
	unique := uniqueIssues(issues)
	userInputSet := make(map[int]struct{}, len(unique))
	for _, num := range unique {
		userInputSet[num] = struct{}{}
	}
	out := make([]int, 0, len(unique))
	seen := make(map[int]struct{}, len(unique))
	addUnique := func(n int) bool {
		if _, ok := seen[n]; ok {
			return false
		}
		seen[n] = struct{}{}
		out = append(out, n)
		return true
	}
	for _, num := range unique {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		issue, err := r.client.FetchIssue(num)
		if err != nil {
			return nil, fmt.Errorf("fetch issue #%d: %w", num, err)
		}
		if issue == nil {
			return nil, fmt.Errorf("fetch issue #%d: not found", num)
		}
		if !r.IsPRD(issue.Body) {
			addUnique(num)
			continue
		}
		children, err := r.resolvePRDChildren(ctx, num, issue.Body, userInputSet)
		if err != nil {
			return nil, err
		}
		for _, child := range children {
			addUnique(child)
		}
		fmt.Fprintf(r.warningWriter, "expanded PRD #%d to %d child issues\n", num, len(children))
	}
	return out, nil
}

func (r *PRDResolver) resolvePRDChildren(ctx context.Context, parent int, body string, userInputSet map[int]struct{}) ([]int, error) {
	candidates := r.collectCandidates(parent, body)
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no child issues for PRD #%d", parent)
	}
	accepted := make([]int, 0, len(candidates))
	for _, child := range candidates {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if _, ok := userInputSet[child]; ok {
			accepted = append(accepted, child)
			continue
		}
		childIssue, err := r.client.FetchIssue(child)
		if err != nil {
			return nil, fmt.Errorf("fetch child #%d: %w", child, err)
		}
		if childIssue == nil {
			return nil, fmt.Errorf("fetch child #%d: not found", child)
		}
		if r.IsPRD(childIssue.Body) {
			return nil, fmt.Errorf("nested PRD detected: #%d", child)
		}
		ref, ok := ExtractParentReference(childIssue.Body)
		if !ok || ref != parent {
			fmt.Fprintf(r.warningWriter, "candidate #%d is not a child of PRD #%d, skipping\n", child, parent)
			continue
		}
		accepted = append(accepted, child)
	}
	if len(accepted) == 0 {
		return nil, fmt.Errorf("no child issues for PRD #%d", parent)
	}
	return accepted, nil
}

func (r *PRDResolver) collectCandidates(parent int, body string) []int {
	order := make([]int, 0)
	seen := make(map[int]struct{})
	add := func(nums []int) {
		for _, n := range nums {
			if n == parent {
				continue
			}
			if _, ok := seen[n]; ok {
				continue
			}
			seen[n] = struct{}{}
			order = append(order, n)
		}
	}
	add(ExtractIssueReferences(body))
	if comments, err := r.client.ListIssueComments(parent); err == nil {
		for _, c := range comments {
			add(ExtractIssueReferences(c.Body))
		}
	} else {
		fmt.Fprintf(r.warningWriter, "warning: could not list comments for PRD #%d: %v\n", parent, err)
	}
	if len(order) == 0 {
		if results, err := r.client.SearchIssues(prdSearchToken(parent)); err == nil {
			for _, issue := range results {
				add([]int{issue.Number})
			}
		} else {
			fmt.Fprintf(r.warningWriter, "warning: mention search for PRD #%d failed: %v\n", parent, err)
		}
	}
	return order
}
