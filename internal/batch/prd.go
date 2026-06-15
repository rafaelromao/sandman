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
	client     github.Client
	infoWriter io.Writer
	sectionREs []*regexp.Regexp
}

// NewPRDResolver returns a resolver that expands any PRD issues in the input
// into their child issues. The info writer receives one line per expanded PRD.
func NewPRDResolver(client github.Client, infoWriter io.Writer) *PRDResolver {
	if infoWriter == nil {
		infoWriter = os.Stderr
	}
	required := []string{"Problem Statement", "Solution", "User Stories"}
	sectionREs := make([]*regexp.Regexp, len(required))
	for i, name := range required {
		sectionREs[i] = regexp.MustCompile(`(?im)^##\s+` + regexp.QuoteMeta(name) + `\s*$`)
	}
	return &PRDResolver{client: client, infoWriter: infoWriter, sectionREs: sectionREs}
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
	seen := make(map[int]struct{}, len(unique))
	out := make([]int, 0, len(unique))
	for _, num := range unique {
		if _, ok := seen[num]; ok {
			continue
		}
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
			seen[num] = struct{}{}
			out = append(out, num)
			continue
		}
		children, err := r.resolvePRDChildren(ctx, num, issue.Body)
		if err != nil {
			return nil, err
		}
		for _, child := range children {
			if _, ok := seen[child]; ok {
				continue
			}
			seen[child] = struct{}{}
			out = append(out, child)
		}
		fmt.Fprintf(r.infoWriter, "expanded PRD #%d to %d child issues\n", num, len(children))
	}
	return out, nil
}

func (r *PRDResolver) resolvePRDChildren(ctx context.Context, parent int, body string) ([]int, error) {
	candidates := r.collectCandidates(parent, body)
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no child issues for PRD #%d", parent)
	}
	accepted := make([]int, 0, len(candidates))
	for _, child := range candidates {
		if err := ctx.Err(); err != nil {
			return nil, err
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
			fmt.Fprintf(r.infoWriter, "candidate #%d is not a child of PRD #%d, skipping\n", child, parent)
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
	seen := make(map[int]struct{})
	var order []int
	add := func(nums []int) {
		for _, n := range nums {
			if _, ok := seen[n]; ok || n == parent {
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
	}
	if len(order) == 0 {
		if results, err := r.client.SearchIssues(prdSearchToken(parent)); err == nil {
			for _, issue := range results {
				if issue.Number == parent {
					continue
				}
				add([]int{issue.Number})
			}
		}
	}
	return order
}
