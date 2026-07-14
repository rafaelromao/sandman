package batch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/rafaelromao/sandman/internal/paths"
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

// BadgeControlFileReader reports whether the local control file that
// marks "the Built with Sandman badge PR has been proposed in this
// checkout" is present. The post-batch BadgeHooker consults the
// tracking file as the first gate; on a hit, no API call is made.
type BadgeControlFileReader interface {
	HasBadgeControlFile() bool
}

// BadgeControlFileWriter writes the local control file that marks
// "the Built with Sandman badge PR has been proposed in this
// checkout". The post-batch BadgeHooker calls Write synchronously
// after a successful prompt run so the next batch in this checkout
// short-circuits at the read gate without invoking the API at all.
// The default implementation writes the file atomically via
// temp-file + os.Rename.
type BadgeControlFileWriter interface {
	Write() error
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

// HasBadgePR consults the GitHub REST pulls endpoint for the current
// repository via `gh api --paginate`. The scan walks every page of
// the response until the marker is found or end-of-results. Hand-rolled
// cursor parsing was removed when issue #2195 surfaced that `gh pr
// list --json` (which routes through GraphQL and has no usable
// `Link: …; rel="next"` header) silently stopped paginating after
// page 1 once a repo exceeded the most-recent 100 PRs. `gh api
// --paginate` honours the REST `Link` header transparently and is
// the supported pagination primitive.
//
// The endpoint is passed as a single positional argument and the
// array response is flattened to JSON Lines via `-q '.[]'` so the
// Go-side decoder can stream the body as one PR object per line —
// matching the shape the in-agent prompt in
// `internal/prompt/badge_prompt.md` already specifies.
func (d *defaultPRLister) HasBadgePR(ctx context.Context) (bool, error) {
	args := []string{"api", "--paginate", "-q", ".[]", "repos/{owner}/{repo}/pulls?state=all&per_page=100"}
	out, err := d.gh.runGh(ctx, args...)
	if err != nil {
		return false, fmt.Errorf("badge marker scan: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var p prPayloadBody
		if err := dec.Decode(&p); err != nil {
			if err == io.EOF {
				return false, nil
			}
			return false, fmt.Errorf("badge marker scan: decode prs: %w", err)
		}
		if badgeMarkerRE.MatchString(p.Body) {
			return true, nil
		}
	}
}

// badgeControlFilePath returns the absolute path to the badge control
// file under the given layout, matching every other .sandman/*
// persisted artifact.
func badgeControlFilePath(layout paths.Layout) string {
	return layout.BadgeControlFilePath()
}

// defaultBadgeControlFileReader is the production implementation of
// BadgeControlFileReader. It looks for the control file under the
// layout-resolved SandmanDir (not process cwd) so the read survives
// worktree or subdir invocations.
type defaultBadgeControlFileReader struct {
	layout paths.Layout
}

// HasBadgeControlFile reports whether the control file is present
// under the resolved SandmanDir. Stat errors other than "not exist"
// are swallowed and reported as "absent" — a transient filesystem
// hiccup must never block the spawn path. The control file is the
// first gate evaluated by the post-batch hook: when it returns true,
// the marker-comment scan and the child-runner spawn are both
// skipped entirely. The marker-comment query acts as the
// authoritative fallback only on a fresh checkout where the file
// does not yet exist.
func (d *defaultBadgeControlFileReader) HasBadgeControlFile() bool {
	_, err := os.Stat(badgeControlFilePath(d.layout))
	if err != nil {
		return false
	}
	return true
}

// defaultBadgeControlFileWriter is the production implementation of
// BadgeControlFileWriter. It writes the control file under the
// layout-resolved SandmanDir atomically via temp-file + os.Rename so
// readers never observe a half-written file.
type defaultBadgeControlFileWriter struct {
	layout paths.Layout
}

// Write creates the control file at <sandmanDir>/state/.built_with_sandman
// using the atomic temp-file + rename pattern. The file is intentionally
// empty — its mere existence is the signal.
func (d *defaultBadgeControlFileWriter) Write() error {
	controlPath := badgeControlFilePath(d.layout)
	dir := filepath.Dir(controlPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir for badge control file: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".built_with_sandman.XXXXXX")
	if err != nil {
		return fmt.Errorf("create temp file for badge control file: %w", err)
	}
	tmpName := tmp.Name()
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("close temp file for badge control file: %w", err)
	}
	if err := os.Rename(tmpName, controlPath); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("rename badge control file: %w", err)
	}
	return nil
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
	RunPrompt(ctx context.Context, promptText, branch string) (prURL string, err error)
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

// badgeBranchName is the stable sidecar branch used for the Built with
// Sandman badge PR. It is the contract honored by the post-batch badge
// hook so that re-runs do not produce timestamped prompt-only branches.
const badgeBranchName = "sandman/built-with-sandman"

func (r *defaultSandmanRunner) RunPrompt(ctx context.Context, promptText, branch string) (string, error) {
	args := []string{"run", "--prompt", promptText, "--branch", branch}
	cmd := exec.CommandContext(ctx, r.bin, args...)
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
	controlReader BadgeControlFileReader
	controlWriter BadgeControlFileWriter
	sandmanRunner SandmanRunner
}

func newDefaultBadgeHooker(prLister PRLister, controlReader BadgeControlFileReader, controlWriter BadgeControlFileWriter, sandmanRunner SandmanRunner) *defaultBadgeHooker {
	return &defaultBadgeHooker{
		prLister:      prLister,
		controlReader: controlReader,
		controlWriter: controlWriter,
		sandmanRunner: sandmanRunner,
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

	// Tracking file is the first gate. If a previous badge sidecar run
	// already marked this checkout, neither the API scan nor the spawn
	// runs. The marker-comment PR check below remains authoritative
	// for fresh checkouts where the file does not yet exist on disk.
	// (Issue #2195, slice 3.)
	if h.controlReader.HasBadgeControlFile() {
		return
	}

	sandmanPRs, err := h.prLister.ListMergedSandmanPRs(ctx)
	if err != nil {
		// Silent on every failure path. The hook is fire-and-forget;
		// the next batch retries harmlessly.
		return
	}
	if len(sandmanPRs) == 0 {
		return
	}

	hasBadge, err := h.prLister.HasBadgePR(ctx)
	if err != nil {
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

	prURL, err := h.sandmanRunner.RunPrompt(ctx, badgePromptText, badgeBranchName)
	if err != nil || prURL == "" {
		return
	}

	// Synchronously write the tracking file so the next batch in this
	// checkout short-circuits at the control-file gate without invoking
	// the API at all. (Issue #2195, slice 4.)
	_ = h.controlWriter.Write()
}
