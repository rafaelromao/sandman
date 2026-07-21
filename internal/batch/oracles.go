package batch

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/rafaelromao/sandman/internal/sandbox"
)

// PreFilterOracle is the L1 + DiffSubset pre-filter. It runs the cheap
// `git merge-base --is-ancestor B M` check first; on L1-true it
// abstains. On L1-false it computes the DiffSubset between the branch
// HEAD and origin/main; if the branch's diff is not a subset of
// origin/main's, the oracle rejects. Sub-second, zero REST.
type PreFilterOracle struct {
	// RepoDir is the git working copy whose refs the oracle queries.
	// The orchestrator wires this to the same worktree the run executes
	// in.
	RepoDir string
	// BaseRef is the local ref for the base branch (typically
	// origin/main). It must be resolvable in RepoDir; the orchestrator
	// keeps it in sync via SyncBaseBranch.
	BaseRef string
	// HeadRef is the local ref for the issue's branch (typically
	// `HEAD` after WorktreeSandbox.Start).
	HeadRef string
}

// Run executes the PreFilter oracle. See the issue body for the full
// contract; the high-level outcome is:
//
//   - L1 true (base is ancestor of head) → OracleAbstain (the change
//     is exactly on main, so further verification is unnecessary; the
//     Decision oracle will abstain too because it only runs on diffs).
//   - L1 false + subset of main → OracleAbstain (the change is part
//     of main; no proof either way).
//   - L1 false + not a subset → OracleReject (the branch has lines
//     that are not in main; the Decision oracle cannot prove).
//   - L1 error → OracleAbstain with a logged warning (transient
//     network / git errors must not block the run).
func (p *PreFilterOracle) Run(in VerifyInput) (OracleResult, OracleCheck, error) {
	dir := p.repoDir(in)
	if dir == "" {
		return OracleAbstain, OracleCheck{Name: "pre-filter"}, nil
	}
	base := p.baseRef(in)
	head := p.headRef(in)
	if base == "" || head == "" {
		return OracleAbstain, OracleCheck{Name: "pre-filter"}, nil
	}
	ancestor, err := sandbox.GitMergeBaseIsAncestor(dir, head, base)
	if err != nil {
		return OracleAbstain, OracleCheck{Name: "pre-filter", Details: map[string]any{"error": err.Error()}}, nil
	}
	if ancestor {
		// head is a descendant of base → the change is on main.
		return OracleAbstain, OracleCheck{Name: "pre-filter", Details: map[string]any{"l1": true}}, nil
	}
	subset, err := sandbox.DiffSubset(dir, head, base)
	if err != nil {
		return OracleAbstain, OracleCheck{Name: "pre-filter", Details: map[string]any{"error": err.Error()}}, nil
	}
	mainSet, err := sandbox.DiffSubset(dir, base, head)
	if err != nil {
		return OracleAbstain, OracleCheck{Name: "pre-filter", Details: map[string]any{"error": err.Error()}}, nil
	}
	if isSubset(subset, mainSet) {
		return OracleAbstain, OracleCheck{Name: "pre-filter", Details: map[string]any{"l1": false, "subset": true}}, nil
	}
	return OracleReject, OracleCheck{Name: "pre-filter", Details: map[string]any{"l1": false, "subset": false, "files": fileNames(subset)}}, nil
}

func (p *PreFilterOracle) repoDir(in VerifyInput) string {
	if p.RepoDir != "" {
		return p.RepoDir
	}
	return in.WorkDir
}

func (p *PreFilterOracle) baseRef(in VerifyInput) string {
	if p.BaseRef != "" {
		return p.BaseRef
	}
	return "origin/main"
}

func (p *PreFilterOracle) headRef(in VerifyInput) string {
	if p.HeadRef != "" {
		return p.HeadRef
	}
	return "HEAD"
}

// isSubset reports whether every (path, hunk) in `a` appears in `b`.
// The L1 check covers the simple case; this is the L2 fallback.
func isSubset(a, b sandbox.DiffSet) bool {
	if len(a.Files) == 0 {
		return true
	}
	index := map[string]map[string]struct{}{}
	for _, f := range b.Files {
		hunks := map[string]struct{}{}
		for _, h := range f.Hunks {
			hunks[h] = struct{}{}
		}
		index[f.Path] = hunks
	}
	for _, f := range a.Files {
		hunks, ok := index[f.Path]
		if !ok {
			return false
		}
		for _, h := range f.Hunks {
			if _, ok := hunks[h]; !ok {
				return false
			}
		}
	}
	return true
}

func fileNames(ds sandbox.DiffSet) []string {
	out := make([]string, 0, len(ds.Files))
	for _, f := range ds.Files {
		out = append(out, f.Path)
	}
	return out
}

// CheapGateOracle is the API-side pre-check. It reads reviewDecision /
// mergeStateStatus / statusCheckRollup off the supplied PR and returns
// OracleDeferDecision when all three are positive (the Decision oracle
// should make the final call) or OracleAbstain otherwise.
// CHANGES_REQUESTED abstains — sandman-pr-review Hard Rule 8 owns that path.
type CheapGateOracle struct{}

// Run executes the CheapGate oracle. It is pure: it does not shell out
// and does not call any GitHub API; the orchestrator fetches the PR
// once and reuses the result across the oracles.
func (c *CheapGateOracle) Run(in VerifyInput) (OracleResult, OracleCheck, error) {
	if in.PR == nil {
		return OracleAbstain, OracleCheck{Name: "cheap-gate", Details: map[string]any{"reason": "no-pr"}}, nil
	}
	if strings.EqualFold(in.PR.ReviewDecision, "CHANGES_REQUESTED") {
		return OracleAbstain, OracleCheck{Name: "cheap-gate", Details: map[string]any{"review": in.PR.ReviewDecision}}, nil
	}
	if !strings.EqualFold(in.PR.ReviewDecision, "APPROVED") {
		return OracleAbstain, OracleCheck{Name: "cheap-gate", Details: map[string]any{"review": in.PR.ReviewDecision}}, nil
	}
	if !strings.EqualFold(in.PR.MergeStateStatus, "CLEAN") {
		return OracleAbstain, OracleCheck{Name: "cheap-gate", Details: map[string]any{"merge": in.PR.MergeStateStatus}}, nil
	}
	if !strings.EqualFold(in.PR.StatusCheckRollup, "success") {
		return OracleAbstain, OracleCheck{Name: "cheap-gate", Details: map[string]any{"checks": in.PR.StatusCheckRollup}}, nil
	}
	return OracleDeferDecision, OracleCheck{Name: "cheap-gate", Details: map[string]any{"review": in.PR.ReviewDecision, "merge": in.PR.MergeStateStatus, "checks": in.PR.StatusCheckRollup}}, nil
}

// DecisionOracle runs the structured `## Acceptance criteria` test plan
// inside a worktree pinned to `origin/main` HEAD. It is the only oracle
// that can produce `Verified`; see the issue body for the full
// architecture summary.
type DecisionOracle struct {
	// Runner is the function that executes one AC line. Production
	// wires it to sandbox.NewWorktreeSandbox(...).Exec; tests inject
	// a fake that records the calls and returns a canned output.
	Runner func(ctx context.Context, dir, line string) (string, error)
}

// Run executes the Decision oracle. The contract is:
//
//   - No `## Acceptance criteria` section → OracleNoSignal.
//   - AC section present but no parseable `go test -run` lines → OracleNoSignal.
//   - All lines pass (exit 0) → OracleVerified.
//   - Any line fails (exit non-zero) → OracleFailed.
func (d *DecisionOracle) Run(in VerifyInput) (OracleResult, OracleCheck, error) {
	if in.Issue == nil {
		return OracleNoSignal, OracleCheck{Name: "decision", Details: map[string]any{"reason": "no-issue"}}, nil
	}
	lines := ParseAcceptanceCriteria(in.Issue.Body)
	if len(lines) == 0 {
		return OracleNoSignal, OracleCheck{Name: "decision", Details: map[string]any{"reason": "no-ac"}}, nil
	}
	runner := d.Runner
	if runner == nil {
		runner = defaultDecisionRunner
	}
	ran := 0
	for _, line := range lines {
		ran++
		out, err := runner(in.Context, in.WorkDir, line)
		if err != nil {
			return OracleFailed, OracleCheck{Name: "decision", Details: map[string]any{"ran": ran, "total": len(lines), "failed": line, "output": truncate(out, 512)}}, fmt.Errorf("decision line %q: %w", line, err)
		}
	}
	return OracleVerified, OracleCheck{Name: "decision", Details: map[string]any{"ran": ran, "total": len(lines)}}, nil
}

func defaultDecisionRunner(ctx context.Context, dir, line string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	cmd := exec.CommandContext(ctx, "sh", "-c", line)
	cmd.Dir = dir
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		return buf.String(), err
	}
	return buf.String(), nil
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
