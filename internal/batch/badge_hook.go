package batch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"github.com/rafaelromao/sandman/internal/prompt"
)

var sandmanBranchRE = regexp.MustCompile(`^sandman/`)
var badgeMarkerRE = regexp.MustCompile(`<!-- sandman-badge-pr -->`)
var prURLRE = regexp.MustCompile(`https://github\.com/[^/]+/[^/]+/pull/\d+`)

type ghCommander interface {
	runGh(ctx context.Context, args ...string) ([]byte, error)
}

type realGhCommander struct{}

func (realGhCommander) runGh(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "gh", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("gh %s: %w\n%s", strings.Join(args, " "), err, out)
	}
	return out, nil
}

type PRLister interface {
	ListMergedSandmanPRs(ctx context.Context) ([]MergedSandmanPR, error)
	HasBadgePR(ctx context.Context) (bool, error)
}

type MergedSandmanPR struct {
	Number      int
	HeadRefName string
	Title       string
}

type defaultPRLister struct {
	gh ghCommander
}

func (d *defaultPRLister) ListMergedSandmanPRs(ctx context.Context) ([]MergedSandmanPR, error) {
	out, err := d.gh.runGh(ctx, "pr", "list", "--state", "merged", "--limit", "100", "--json", "number,headRefName,title")
	if err != nil {
		return nil, err
	}
	var payloads []prPayloadList
	if err := json.Unmarshal(out, &payloads); err != nil {
		return nil, fmt.Errorf("parse merged prs: %w", err)
	}
	var result []MergedSandmanPR
	for _, p := range payloads {
		if sandmanBranchRE.MatchString(p.HeadRefName) {
			result = append(result, MergedSandmanPR{
				Number:      p.Number,
				HeadRefName: p.HeadRefName,
				Title:       p.Title,
			})
		}
	}
	return result, nil
}

func (d *defaultPRLister) HasBadgePR(ctx context.Context) (bool, error) {
	out, err := d.gh.runGh(ctx, "pr", "list", "--state", "all", "--limit", "100", "--json", "number,body")
	if err != nil {
		return false, err
	}
	var payloads []prPayloadBody
	if err := json.Unmarshal(out, &payloads); err != nil {
		return false, fmt.Errorf("parse all prs: %w", err)
	}
	for _, p := range payloads {
		if badgeMarkerRE.MatchString(p.Body) {
			return true, nil
		}
	}
	return false, nil
}

type prPayloadList struct {
	Number      int    `json:"number"`
	HeadRefName string `json:"headRefName"`
	Title       string `json:"title"`
}

type prPayloadBody struct {
	Number int    `json:"number"`
	Body   string `json:"body"`
}

type SandmanRunner interface {
	RunPrompt(ctx context.Context, promptText string) (prURL string, err error)
}

type defaultSandmanRunner struct {
	bin string
}

func newDefaultSandmanRunner() (*defaultSandmanRunner, error) {
	bin, err := os.Executable()
	if err != nil {
		bin, err = lookSandman()
		if err != nil {
			return nil, err
		}
	}
	return &defaultSandmanRunner{bin: bin}, nil
}

func lookSandman() (string, error) {
	path, err := exec.LookPath("sandman")
	if err != nil {
		return "", fmt.Errorf("sandman not found in PATH: %w", err)
	}
	return path, nil
}

func (r *defaultSandmanRunner) RunPrompt(ctx context.Context, promptText string) (string, error) {
	cmd := exec.CommandContext(ctx, r.bin, "run", "--prompt", promptText)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("sandman run --prompt: %w\n%s", err, out)
	}
	url := prURLRE.FindString(string(out))
	return url, nil
}

type BadgeHooker interface {
	MaybeSuggestBadge(ctx context.Context, results []AgentRunResult)
}

type nopBadgeHooker struct{}

func (n nopBadgeHooker) MaybeSuggestBadge(ctx context.Context, results []AgentRunResult) {}

type defaultBadgeHooker struct {
	prLister      PRLister
	sandmanRunner SandmanRunner
	writer        io.Writer
}

func newDefaultBadgeHooker(prLister PRLister, sandmanRunner SandmanRunner, writer io.Writer) *defaultBadgeHooker {
	return &defaultBadgeHooker{
		prLister:      prLister,
		sandmanRunner: sandmanRunner,
		writer:        writer,
	}
}

func (h *defaultBadgeHooker) MaybeSuggestBadge(ctx context.Context, results []AgentRunResult) {
	hasSuccess := false
	for _, r := range results {
		if r.Status == "success" {
			hasSuccess = true
			break
		}
	}
	if !hasSuccess {
		return
	}

	sandmanPRs, err := h.prLister.ListMergedSandmanPRs(ctx)
	if err != nil {
		fmt.Fprintf(h.writer, "Badge PR suggestion skipped: %v\n", err)
		return
	}
	if len(sandmanPRs) == 0 {
		return
	}

	hasBadge, err := h.prLister.HasBadgePR(ctx)
	if err != nil {
		fmt.Fprintf(h.writer, "Badge PR suggestion skipped: %v\n", err)
		return
	}
	if hasBadge {
		return
	}

	prTitles := make([]string, len(sandmanPRs))
	for i, pr := range sandmanPRs {
		prTitles[i] = fmt.Sprintf("%s (#%d)", pr.Title, pr.Number)
	}
	mergedPRsText := strings.Join(prTitles, "\n")
	badgePromptTemplate := prompt.DefaultBadgePrompt()
	badgePromptText := strings.ReplaceAll(badgePromptTemplate, "{{MERGED_PRS}}", mergedPRsText)

	prURL, err := h.sandmanRunner.RunPrompt(ctx, badgePromptText)
	if err != nil {
		fmt.Fprintf(h.writer, "Badge PR suggestion skipped: %v\n", err)
		return
	}

	if prURL != "" {
		fmt.Fprintf(h.writer, "Sandman suggested a Built with Sandman badge PR: %s (close it to dismiss)\n", prURL)
	}
}
