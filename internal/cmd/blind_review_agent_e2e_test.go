//go:build e2e

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/batch"
	"github.com/rafaelromao/sandman/internal/config"
	"github.com/rafaelromao/sandman/internal/github"
	"github.com/rafaelromao/sandman/internal/prompt"
	"github.com/rafaelromao/sandman/internal/review"
	"github.com/rafaelromao/sandman/internal/reviewpub"
	"github.com/rafaelromao/sandman/internal/skill"
	"github.com/rafaelromao/sandman/internal/testenv"
)

// These live scenarios are deliberately opt-in: both execute a real managed
// Run and may make provider requests. Keep the provider/model pin explicit so
// the local override can be changed without changing release coverage.
func TestBlindReviewManagedRun_DefaultUsesRealChildSession(t *testing.T) {
	requireSandmanAgentE2E(t)
	model := testenv.ResolveTestModel("opencode", ciRemediationAgentModel)
	home := prepareBlindReviewAgentHarness(t, model)
	runBlindReviewManagedScenario(t, home, model, "")
}

func TestBlindReviewManagedRun_ConfiguredCommandPostsTriggerWithoutChild(t *testing.T) {
	requireSandmanAgentE2E(t)
	model := testenv.ResolveTestModel("opencode", ciRemediationAgentModel)
	home := prepareBlindReviewAgentHarness(t, model)
	runBlindReviewManagedScenario(t, home, model, "/sandman review")
}

func prepareBlindReviewAgentHarness(t *testing.T, model string) string {
	t.Helper()
	for _, dependency := range []string{"opencode", "jq", "python3", "curl"} {
		if _, err := exec.LookPath(dependency); err != nil {
			t.Fatalf("real review fixture needs %s: %v", dependency, err)
		}
	}
	preparePresetMatrixAgentHome(t)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	if err := os.MkdirAll(filepath.Join(home, ".local", "share"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := skill.Sync(skill.SyncOptions{HomeDir: home}); err != nil {
		t.Fatalf("sync skills: %v", err)
	}
	t.Setenv("SANDMAN_SKILL_ROOT", filepath.Join(home, ".agents", "skills", "sandman"))
	return home
}

func runBlindReviewManagedScenario(t *testing.T, home, model, reviewCommand string) {
	t.Helper()
	t.Logf("real review agent: model=%s variant=%s", model, os.Getenv("SANDMAN_TEST_VARIANT_OPENCODE"))
	repo := testenv.MkdirShort(t, "sm-blind-")
	initRunIntegrationRepoWithRemote(t, repo)
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte(".sandman/\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", ".gitignore")
	runGit(t, repo, "commit", "-m", "test: prepare review fixture")
	runGit(t, repo, "push", "origin", "main")
	head := strings.TrimSpace(runGit(t, repo, "rev-parse", "HEAD"))
	fixtureDir := t.TempDir()
	statePath := filepath.Join(fixtureDir, "comments.json")
	if err := os.WriteFile(statePath, []byte("[]"), 0600); err != nil {
		t.Fatal(err)
	}
	ghPath := filepath.Join(fixtureDir, "gh")
	if err := os.WriteFile(ghPath, []byte(blindReviewRecordingGH), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fixtureDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GH_TOKEN", "fake")
	t.Setenv("GITHUB_TOKEN", "fake")
	t.Setenv("GH_REPO", "owner/repo")
	t.Setenv("SANDMAN_BLIND_COMMENTS", statePath)
	t.Setenv("SANDMAN_BLIND_HEAD", head)
	t.Setenv("SANDMAN_BLIND_CRITERIA", blindReviewFixtureCriteria)
	t.Chdir(repo)

	if reviewCommand != "" {
		socket := ReviewSocketPath(filepath.Join(repo, ".sandman"))
		if err := os.MkdirAll(filepath.Dir(socket), 0755); err != nil {
			t.Fatal(err)
		}
		listener, err := net.Listen("unix", socket)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = listener.Close() })
		go func() {
			for {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				_ = conn.Close()
			}
		}()
	}

	agent := config.Agent{
		Name: "opencode", Model: model,
		Command: blindReviewOpenCodeCommand(model, os.Getenv("SANDMAN_TEST_VARIANT_OPENCODE")),
		Env: map[string]string{
			testenv.OpenCodeConfigEnv: testenv.OpenCodeSingleModelConfig(model),
			"OPENCODE_PERMISSION":     `{"external_directory":"allow"}`,
		},
	}
	pinOpenCodeToModel(&agent, model)
	cleanup := false
	cfg := &config.Config{
		DefaultAgent: "opencode", Agent: "opencode", DefaultModel: model,
		ReviewCommand: reviewCommand, ReviewCommandSet: true,
		Sandbox: "worktree", WorktreeDir: filepath.Join(repo, ".sandman", "worktrees"),
		CleanupWorktrees: &cleanup, Git: config.GitConfig{BaseBranch: "main"},
		AgentProviders: map[string]config.Agent{"opencode": agent},
	}
	store := &fakeStore{config: cfg}
	client := &blindReviewManagedGitHub{
		fakeGitHubClient: fakeGitHubClient{issues: map[int]*github.Issue{
			42: {Number: 42, Title: "Review fixture", State: "open", Body: blindReviewFixtureCriteria},
		}},
		head: head, commentsPath: statePath, reviewCommand: reviewCommand,
	}
	log := &recordingEventLog{}
	runner := batch.NewOrchestrator(client, &prompt.Engine{}, store, log)
	deps := Dependencies{
		BatchRunner: runner, ConfigStore: store, GitHubClient: client,
		Renderer: &prompt.Engine{}, EventLog: log, RepoRoot: repo,
		IsTTY: func() bool { return false },
	}
	task := `Review the existing published pull request 17 in owner/repo for work item {{ISSUE_NUMBER}}.
The checked-out branch is already published and matches the PR head; no source edits are needed.
The selected Review command is ` + "`{{REVIEW_COMMAND}}`" + `.
If empty, load sandman-blind-review and run exactly one full round, including publication.
If configured, load sandman-review-request and deliver exactly one confirmed trigger verbatim.
Do not implement or self-review, do not commit, do not merge, and do not poll.
Return successfully after confirmed publication/delivery; the managed runtime owns observation.
Retain the round's prompt and report files for the caller's test verification.
`
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	if deadline, ok := t.Deadline(); ok && time.Until(deadline.Add(-30*time.Second)) < 15*time.Minute {
		cancel()
		ctx, cancel = context.WithDeadline(context.Background(), deadline.Add(-30*time.Second))
	}
	defer cancel()
	var output bytes.Buffer
	cmd := NewRunCmd(deps)
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	cmd.SetArgs([]string{"--sandbox", "worktree", "--agent", "opencode", "--prompt", task, "42"})
	if err := cmd.ExecuteContext(ctx); err != nil {
		t.Fatalf("real managed review: %v\n%s", err, output.String())
	}
	comments, err := client.ListPRComments(ctx, 17)
	if err != nil {
		t.Fatal(err)
	}
	triggers, publications, approvals := 0, 0, 0
	for _, comment := range comments {
		if strings.HasPrefix(comment.Body, "/sandman review") {
			triggers++
		}
		if reviewpub.AnswersBlind(comment.Body, head) {
			verdict, ok := reviewpub.PublishedVerdict(comment.Body)
			if ok && verdict == reviewpub.Approved {
				approvals++
			}
			publications++
		}
	}
	children := blindReviewChildPrompts(t, home)
	if reviewCommand != "" {
		if triggers != 1 || publications != 0 || len(children) != 0 {
			t.Fatalf("delegated path: triggers=%d blind reports=%d child reviews=%d", triggers, publications, len(children))
		}
		return
	}
	if triggers != 0 || publications == 0 || approvals == 0 || len(children) != publications {
		t.Fatalf("blind path: triggers=%d reports=%d child reviews=%d\n%s", triggers, publications, len(children), output.String())
	}
	for _, child := range children {
		childPrompt, err := blindReviewAuthoritativePrompt(child.Prompt, child.Reads)
		if err != nil {
			t.Fatalf("child review input: %v", err)
		}
		runDir := ""
		for _, line := range strings.Split(childPrompt, "\n") {
			if strings.HasPrefix(line, "- RunDir: ") {
				runDir = strings.TrimPrefix(line, "- RunDir: ")
			}
		}
		if runDir == "" {
			prefix := childPrompt
			if len(prefix) > 100 {
				prefix = prefix[:100]
			}
			t.Fatalf("child prompt omitted its report destination; starts %q", prefix)
		}
		var prior []github.PRComment
		for _, comment := range comments {
			if comment.CreatedAt.Before(child.CreatedAt) {
				prior = append(prior, comment)
			}
		}
		priorExists, priorContext := review.PriorReviewContext(prior, nil, nil)
		want, err := (&prompt.Engine{}).RenderReview(prompt.RenderConfig{}, prompt.PRData{
			Number: 17, Title: "Review fixture", Body: "Closes #42",
			AcceptanceCriteria: blindReviewFixtureCriteria, RunDir: runDir,
			PriorReviewExists: priorExists, PriorReviewContext: priorContext,
		})
		if err != nil {
			t.Fatal(err)
		}
		want = review.ReviewSnapshotHeader(head, "main") + want
		if childPrompt != want {
			index := 0
			for index < len(childPrompt) && index < len(want) && childPrompt[index] == want[index] {
				index++
			}
			gotEnd, wantEnd := min(index+80, len(childPrompt)), min(index+80, len(want))
			t.Fatalf("child authoritative input is not the exact daemon prompt (got %d bytes, want %d); first difference at byte %d: got %q, want %q",
				len(childPrompt), len(want), index, childPrompt[index:gotEnd], want[index:wantEnd])
		}
	}
}

const blindReviewFixtureCriteria = "Review-only fixture: no source edits are required. Assess the existing clean checkout."

type blindReviewManagedGitHub struct {
	fakeGitHubClient
	head, commentsPath, reviewCommand string
}

func (g *blindReviewManagedGitHub) FindPRByBranch(ctx context.Context, branch string) (*github.PR, error) {
	comments, err := g.ListPRComments(ctx, 17)
	if err != nil {
		return nil, err
	}
	pr := &github.PR{
		Number: 17, Title: "Review fixture", Body: "Closes #42", State: "open",
		HeadRefName: branch, HeadRefOid: g.head, BaseRefName: "main",
		StatusCheckRollup: "success", MergeStateStatus: "CLEAN",
	}
	// Model the external merger only after the real agent's mutation. This
	// fixture never manufactures child sessions or review publication bodies.
	latestBlind := ""
	for _, comment := range comments {
		if g.reviewCommand != "" && strings.HasPrefix(comment.Body, g.reviewCommand) {
			pr.State, pr.Merged = "merged", true
		}
		if reviewpub.AnswersBlind(comment.Body, g.head) {
			latestBlind = comment.Body
		}
	}
	if verdict, published := reviewpub.PublishedVerdict(latestBlind); g.reviewCommand == "" && published && verdict == reviewpub.Approved {
		pr.State, pr.Merged = "merged", true
	}
	return pr, nil
}

func (g *blindReviewManagedGitHub) ListPRComments(context.Context, int) ([]github.PRComment, error) {
	var raw []struct {
		ID        string    `json:"url"`
		Body      string    `json:"body"`
		CreatedAt time.Time `json:"createdAt"`
	}
	data, err := os.ReadFile(g.commentsPath)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	comments := make([]github.PRComment, 0, len(raw))
	for _, comment := range raw {
		comments = append(comments, github.PRComment{
			ID: comment.ID, Body: comment.Body, CreatedAt: comment.CreatedAt, UpdatedAt: comment.CreatedAt,
		})
	}
	return comments, nil
}

type blindReviewChildInput struct {
	Prompt    string
	Reads     []string
	CreatedAt time.Time
}

func blindReviewChildPrompts(t *testing.T, home string) []blindReviewChildInput {
	t.Helper()
	db := filepath.Join(home, ".local", "share", "opencode", "opencode.db")
	query := `import json, sqlite3, sys
db = sqlite3.connect("file:" + sys.argv[1] + "?mode=ro", uri=True)
print(json.dumps([row[0] for row in db.execute("select id from session where parent_id is not null")]))
`
	output, err := exec.Command("python3", "-c", query, db).CombinedOutput()
	if err != nil {
		t.Fatalf("read isolated OpenCode child identities: %v\n%s", err, output)
	}
	var ids []string
	if err := json.Unmarshal(output, &ids); err != nil {
		t.Fatal(err)
	}
	var prompts []blindReviewChildInput
	for _, id := range ids {
		// A regular file avoids the CLI's large console export being truncated
		// on an asynchronous stdout pipe when the process exits.
		exportFile := filepath.Join(t.TempDir(), "session.json")
		file, err := os.Create(exportFile)
		if err != nil {
			t.Fatal(err)
		}
		command := exec.Command("opencode", "export", id)
		command.Stdout = file
		runErr := command.Run()
		closeErr := file.Close()
		if runErr != nil || closeErr != nil {
			t.Fatalf("export child %s: %v / %v", id, runErr, closeErr)
		}
		output, err := os.ReadFile(exportFile)
		if err != nil {
			t.Fatal(err)
		}
		var session struct {
			Info struct {
				ParentID string `json:"parentID"`
				Time     struct {
					Created int64 `json:"created"`
				} `json:"time"`
			} `json:"info"`
			Messages []struct {
				Info struct {
					Role string `json:"role"`
				} `json:"info"`
				Parts []struct {
					Type  string `json:"type"`
					Text  string `json:"text"`
					Tool  string `json:"tool"`
					State struct {
						Status string `json:"status"`
						Input  struct {
							FilePath string `json:"filePath"`
							Path     string `json:"path"`
						} `json:"input"`
					} `json:"state"`
				} `json:"parts"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(output, &session); err != nil {
			t.Fatalf("decode child export: %v\n%s", err, output)
		}
		if session.Info.ParentID == "" {
			t.Fatalf("child %s lacks parentID", id)
		}
		if session.Info.Time.Created <= 0 {
			t.Fatalf("child %s lacks a creation timestamp for its history snapshot", id)
		}
		var reads []string
		for _, message := range session.Messages {
			for _, part := range message.Parts {
				if part.Type == "tool" && part.Tool == "read" && part.State.Status == "completed" {
					if path := part.State.Input.FilePath; path != "" {
						reads = append(reads, path)
					}
					if path := part.State.Input.Path; path != "" {
						reads = append(reads, path)
					}
				}
			}
		}
		for _, message := range session.Messages {
			if message.Info.Role != "user" {
				continue
			}
			var text strings.Builder
			for _, part := range message.Parts {
				if part.Type == "text" {
					text.WriteString(part.Text)
				}
			}
			prompts = append(prompts, blindReviewChildInput{
				Prompt: text.String(), Reads: reads, CreatedAt: time.UnixMilli(session.Info.Time.Created),
			})
			break
		}
	}
	return prompts
}

const blindReviewRecordingGH = `#!/usr/bin/env python3
import datetime, json, os, subprocess, sys, tempfile
args = sys.argv[1:]
path = os.environ["SANDMAN_BLIND_COMMENTS"]
with open(path) as f:
    comments = json.load(f)
def emit(value):
    data = json.dumps(value)
    if "--jq" in args:
        expr = args[args.index("--jq") + 1]
        subprocess.run(["jq", "-r", expr], input=data, text=True, check=True)
    else:
        print(data)
pr = dict(number=17, title="Review fixture", body="Closes #42", state="OPEN",
          headRefOid=os.environ["SANDMAN_BLIND_HEAD"], baseRefName="main",
          closingIssuesReferences=[dict(number=42)], comments=comments,
          reviews=[], reviewDecision="", mergeStateStatus="CLEAN",
          mergeable="MERGEABLE",
          statusCheckRollup=[dict(__typename="CheckRun", name="fixture",
                                  status="COMPLETED", conclusion="SUCCESS")])
if args[:2] == ["pr", "view"]:
    emit(pr)
elif args[:2] == ["pr", "list"]:
    emit([pr])
elif args[:2] == ["pr", "checks"]:
    emit([dict(name="fixture", state="SUCCESS", bucket="pass")])
elif args[:2] == ["pr", "comment"]:
    if "--body-file" in args:
        with open(args[args.index("--body-file") + 1]) as f:
            body = f.read()
    else:
        body = args[args.index("--body") + 1]
    url = "https://github.com/owner/repo/pull/17#issuecomment-" + str(len(comments) + 1)
    stamp = datetime.datetime.now(datetime.timezone.utc).isoformat().replace("+00:00", "Z")
    comments.append(dict(id=str(len(comments)+1), url=url, body=body, createdAt=stamp))
    fd, temp = tempfile.mkstemp(dir=os.path.dirname(path))
    with os.fdopen(fd, "w") as f:
        json.dump(comments, f)
    os.replace(temp, path)
    print(url)
elif args[:2] == ["issue", "view"]:
    emit(dict(number=42, title="Review fixture", body=os.environ["SANDMAN_BLIND_CRITERIA"], state="OPEN"))
elif args[:2] == ["auth", "status"]:
    print("Logged in to fake GitHub")
elif args[:2] == ["repo", "view"]:
    emit(dict(nameWithOwner="owner/repo", defaultBranchRef=dict(name="main")))
elif args and args[0] == "api":
    endpoint = next((x for x in args[1:] if x.startswith("repos/")), "")
    if endpoint.endswith("issues/17/comments"):
        emit([dict(id=c["id"], html_url=c["url"], body=c["body"], created_at=c["createdAt"],
                   updated_at=c["createdAt"], user=dict(login="sandman")) for c in comments])
    elif endpoint.endswith("pulls/17/reviews") or endpoint.endswith("pulls/17/comments"):
        emit([])
    else:
        raise SystemExit("Unsupported fake API: " + repr(args))
else:
    raise SystemExit("Unsupported fake gh command: " + repr(args))
`

func blindReviewOpenCodeCommand(model, variant string) string {
	command := openCodeRunCommand(model)
	if variant == "" {
		return command
	}
	return strings.Replace(command, "opencode run ", "opencode run --variant "+shellQuote(variant)+" ", 1)
}

func TestBlindReviewFixtureModelAndVariantArePinned(t *testing.T) {
	for _, tc := range []struct {
		variant string
		want    string
	}{
		{"", "opencode run --pure"},
		{"low", "opencode run --variant 'low' --pure"},
		{"a'b", `opencode run --variant 'a'"'"'b' --pure`},
	} {
		t.Run(fmt.Sprintf("variant_%q", tc.variant), func(t *testing.T) {
			got := blindReviewOpenCodeCommand(ciRemediationAgentModel, tc.variant)
			if !strings.HasPrefix(got, tc.want) {
				t.Fatalf("command = %q; want prefix %q", got, tc.want)
			}
		})
	}
	if got := blindReviewOpenCodeCommand(ciRemediationAgentModel, ""); !strings.Contains(got, "-m opencode/big-pickle") {
		t.Fatalf("committed default model changed: %s", got)
	}
	t.Run("recording-gh", func(t *testing.T) {
		for _, dependency := range []string{"python3", "jq"} {
			if _, err := exec.LookPath(dependency); err != nil {
				t.Skipf("fixture needs %s", dependency)
			}
		}
		helperDir, err := filepath.Abs("../skill/sandman/blind-review")
		if err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir()
		initRunIntegrationRepo(t, dir)
		state := filepath.Join(dir, "comments.json")
		if err := os.WriteFile(state, []byte("[]"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(blindReviewRecordingGH), 0755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
		t.Setenv("SANDMAN_BLIND_COMMENTS", state)
		t.Setenv("SANDMAN_BLIND_HEAD", "abc")
		t.Setenv("SANDMAN_BLIND_CRITERIA", blindReviewFixtureCriteria)
		cmd := exec.Command("sh", filepath.Join(helperDir, "review-prompt-v1.sh"),
			"--repository", "owner/repo", "--pull-request", "17", "--head-sha", "abc", "--run-dir", "/report")
		cmd.Dir = dir
		rendered, err := cmd.CombinedOutput()
		if err != nil || !bytes.Contains(rendered, []byte(blindReviewFixtureCriteria)) {
			t.Fatalf("recording prompt endpoints: %v\n%s", err, rendered)
		}
		decision := filepath.Join(dir, "decision.md")
		if err := os.WriteFile(decision, []byte("## Decision\nAPPROVED"), 0600); err != nil {
			t.Fatal(err)
		}
		cmd = exec.Command("sh", filepath.Join(helperDir, "publish-v1.sh"),
			"--repository", "owner/repo", "--pull-request", "17", "--head-sha", "abc", "--decision-file", decision)
		cmd.Dir = dir
		published, err := cmd.CombinedOutput()
		if err != nil || !bytes.Contains(published, []byte(`"verdict":"approved"`)) {
			t.Fatalf("recording publication endpoints: %v\n%s", err, published)
		}
		client := &blindReviewManagedGitHub{commentsPath: state}
		comments, err := client.ListPRComments(context.Background(), 17)
		if err != nil || len(comments) != 1 || !reviewpub.AnswersBlind(comments[0].Body, "abc") {
			t.Fatalf("host did not observe the exact fixture publication: %v %+v", err, comments)
		}
	})
}
