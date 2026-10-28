package skill

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type ciRerunFixture struct {
	PR      map[string]any   `json:"pr"`
	Run     map[string]any   `json:"run"`
	Jobs    []map[string]any `json:"jobs"`
	Mode    string           `json:"mode"`
	Actions []string         `json:"actions"`
	Calls   []string         `json:"calls"`
}

func TestCIRerunHelperRecordsOneDatabaseIDActionAndConfirmsNewAttempt(t *testing.T) {
	fixture := validCIRerunFixture()
	fixture.Run["run_attempt"] = 4 // attempt identity is evidence, not a cumulative veto.
	fixture.Run["head_sha"] = "tested-merge-sha"
	fixture.Run["pull_requests"] = []any{map[string]any{
		"number": 17,
		"head":   map[string]any{"sha": "source-head-sha"},
	}}
	fixture.Jobs[0]["id"] = 987654321 // database ID, not a browser URL's job number.
	fixture.Jobs[0]["run_attempt"] = 4
	fixture.Jobs = append(fixture.Jobs, map[string]any{
		"id": 876543210, "run_id": 12345, "run_attempt": 4,
		"name": "build-macos", "status": "completed", "conclusion": "failure",
	})
	fixture.Mode = "accept"

	got := runCIRerunHelper(t, &fixture)
	if got.exitCode != 0 {
		t.Fatalf("helper exit=%d stdout=%s stderr=%s fixture=%+v", got.exitCode, got.stdout, got.stderr, fixture)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(got.stdout), &result); err != nil {
		t.Fatalf("parse helper result %q: %v", got.stdout, err)
	}
	if result["state"] != "confirmed" || result["observed_attempt"] != float64(5) {
		t.Fatalf("rerun was not confirmed by a new execution read-back: %#v", result)
	}
	if len(fixture.Actions) != 1 || !strings.Contains(fixture.Actions[0], "/actions/jobs/987654321/rerun") {
		t.Fatalf("actions=%v, want one POST to the selected database job id", fixture.Actions)
	}
	if !strings.Contains(fixture.Actions[0], "POST") {
		t.Fatalf("recovery command was not recorded before fake state changed: %q", fixture.Actions[0])
	}
	if !strings.Contains(fixture.Actions[0], "source-head-sha") && !strings.Contains(got.stdout, "source-head-sha") {
		t.Fatalf("result lost source-head identity: command=%q result=%s", fixture.Actions[0], got.stdout)
	}
	if !strings.Contains(got.stdout, "tested-merge-sha") {
		t.Fatalf("result lost tested merge SHA distinct from PR source head: %s", got.stdout)
	}
	if strings.Contains(got.stdout, `"state":"success"`) {
		t.Fatalf("helper must confirm rerun acceptance, not claim all required checks are green: %s", got.stdout)
	}
	var aggregateRecomputed, independentFailureRetained bool
	for _, job := range fixture.Jobs {
		if job["name"] == "quality-gate" && job["run_attempt"] == float64(5) && job["status"] == "queued" {
			aggregateRecomputed = true
		}
		if job["name"] == "build-macos" && job["conclusion"] == "failure" {
			independentFailureRetained = true
		}
	}
	if !aggregateRecomputed || !independentFailureRetained {
		t.Fatalf("rerun did not reschedule its aggregate while preserving an independent required failure: jobs=%#v", fixture.Jobs)
	}
	if !strings.Contains(fixture.Calls[len(fixture.Calls)-1], "/actions/runs/12345") {
		t.Fatalf("expected read-back of the retained workflow-run identity, calls=%v", fixture.Calls)
	}
}

func TestSyncInstallsCIRecoveryHelperAndImplementationGuidance(t *testing.T) {
	home := t.TempDir()
	if err := Sync(SyncOptions{HomeDir: home, ReviewCommand: "/sandman review"}); err != nil {
		t.Fatalf("sync shared skills: %v", err)
	}
	root := filepath.Join(home, ".agents", "skills", embeddedSkillRoot)
	installed, err := os.ReadFile(filepath.Join(root, "implement", "ci-rerun-v1.sh"))
	if err != nil {
		t.Fatalf("read installed CI rerun helper: %v", err)
	}
	source, err := os.ReadFile(filepath.Join("sandman", "implement", "ci-rerun-v1.sh"))
	if err != nil {
		t.Fatalf("read embedded CI rerun helper: %v", err)
	}
	if string(installed) != string(source) {
		t.Fatal("installed CI rerun helper differs from embedded helper")
	}
	skillText, err := os.ReadFile(filepath.Join(root, "implement", "SKILL.md"))
	if err != nil {
		t.Fatalf("read installed implementation guidance: %v", err)
	}
	for _, required := range []string{
		"Failed current-head CI recovery",
		"ci-rerun-v1.sh",
		"workflow_dispatch` is not required",
		"A `confirmed` result means a new attempt was observed",
	} {
		if !strings.Contains(string(skillText), required) {
			t.Errorf("installed implementation guidance missing %q", required)
		}
	}
}

func TestCIRerunHelperDoesNotMutateWhileSiblingExecutionIsActive(t *testing.T) {
	fixture := validCIRerunFixture()
	fixture.Run["status"] = "in_progress"
	fixture.Jobs = append(fixture.Jobs, map[string]any{
		"id": 987654322, "run_id": 12345, "run_attempt": 1,
		"name": "build-macos", "status": "in_progress", "conclusion": nil,
	})

	got := runCIRerunHelper(t, &fixture)
	if got.exitCode != 0 || !strings.Contains(got.stdout, `"state":"waiting"`) || !strings.Contains(got.stdout, `"reason":"workflow-still-running"`) {
		t.Fatalf("active sibling did not produce a runtime-owned prerequisite wait: exit=%d stdout=%s stderr=%s", got.exitCode, got.stdout, got.stderr)
	}
	if len(fixture.Actions) != 0 {
		t.Fatalf("active workflow was mutated; actions=%v", fixture.Actions)
	}
}

func TestCIRerunHelperSuppressesDuplicateWhileRerunIsActive(t *testing.T) {
	fixture := validCIRerunFixture()
	fixture.Run["run_attempt"] = 2
	fixture.Run["status"] = "in_progress"
	fixture.Jobs[0]["id"] = 987654321
	fixture.Jobs[0]["run_attempt"] = 2
	fixture.Jobs[0]["status"] = "in_progress"
	fixture.Jobs[0]["conclusion"] = nil
	fixture.Jobs = []map[string]any{{
		"id": 987654321, "run_id": 12345, "run_attempt": 2,
		"name": "opencode-canary", "status": "in_progress", "conclusion": nil,
	}}

	got := runCIRerunHelper(t, &fixture)
	if got.exitCode != 0 || !strings.Contains(got.stdout, `"state":"waiting"`) {
		t.Fatalf("active rerun was not recognized: exit=%d stdout=%s stderr=%s", got.exitCode, got.stdout, got.stderr)
	}
	if len(fixture.Actions) != 0 {
		t.Fatalf("duplicate mutation while rerun active: %v", fixture.Actions)
	}
}

func TestCIRerunHelperFailsClosedOnStaleIdentityAndDeniedOrAmbiguousSubmission(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*ciRerunFixture)
		mode   string
		state  string
		posts  int
	}{
		{name: "wrong repository", mutate: func(f *ciRerunFixture) { f.Run["repository"].(map[string]any)["full_name"] = "other/repo" }, state: "refused"},
		{name: "wrong pull request", mutate: func(f *ciRerunFixture) {
			f.Run["pull_requests"] = []any{map[string]any{"number": 99, "head": map[string]any{"sha": "source-head-sha"}}}
		}, state: "refused"},
		{name: "stale source head", mutate: func(f *ciRerunFixture) { f.PR["head"].(map[string]any)["sha"] = "new-source-head" }, state: "refused"},
		{name: "wrong tested SHA", mutate: func(f *ciRerunFixture) { f.Run["head_sha"] = "different-merge-sha" }, state: "refused"},
		{name: "wrong run id", mutate: func(f *ciRerunFixture) { f.Run["id"] = 555 }, state: "refused"},
		{name: "wrong job id", mutate: func(f *ciRerunFixture) { f.Jobs[0]["id"] = 111 }, state: "refused"},
		{name: "denied mutation", mode: "denied", state: "refused", posts: 1},
		{name: "ambiguous mutation", mode: "ambiguous", state: "confirmed", posts: 1},
		{name: "unconfirmed mutation", mode: "uncertain", state: "uncertain", posts: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := validCIRerunFixture()
			fixture.Mode = tt.mode
			if tt.mutate != nil {
				tt.mutate(&fixture)
			}
			got := runCIRerunHelper(t, &fixture)
			var result map[string]any
			if err := json.Unmarshal([]byte(got.stdout), &result); err != nil {
				t.Fatalf("parse result %q: %v (stderr %s)", got.stdout, err, got.stderr)
			}
			if result["state"] != tt.state {
				t.Fatalf("state=%v, want %q; result=%#v", result["state"], tt.state, result)
			}
			if len(fixture.Actions) != tt.posts {
				t.Fatalf("POST count=%d, want %d; actions=%v", len(fixture.Actions), tt.posts, fixture.Actions)
			}
		})
	}
}

type ciRerunHelperResult struct {
	stdout   string
	stderr   string
	exitCode int
}

func runCIRerunHelper(t *testing.T, fixture *ciRerunFixture) ciRerunHelperResult {
	t.Helper()
	root, err := os.MkdirTemp(".", "ci-rerun-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Errorf("remove test fixture directory: %v", err)
		}
	})
	fixturePath := filepath.Join(root, "fixture.json")
	data, err := json.Marshal(fixture)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixturePath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	gh := filepath.Join(bin, "gh")
	if err := os.WriteFile(gh, []byte(ciRerunFakeGH), 0o755); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join("sandman", "implement", "ci-rerun-v1.sh")
	args := []string{
		"--repository", "owner/repo", "--pull-request", "17", "--head-sha", "source-head-sha",
		"--tested-sha", "tested-merge-sha", "--run-id", "12345", "--job-id", "987654321",
		"--diagnosis", "transient-provider-timeout",
	}
	cmd := exec.Command("sh", append([]string{script}, args...)...)
	for _, item := range os.Environ() {
		if !strings.HasPrefix(item, "PATH=") && !strings.HasPrefix(item, "CI_RERUN_FIXTURE=") {
			cmd.Env = append(cmd.Env, item)
		}
	}
	cmd.Env = append(cmd.Env, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "CI_RERUN_FIXTURE="+fixturePath)
	probe := exec.Command(gh, "api", "repos/owner/repo/pulls/17")
	probe.Env = cmd.Env
	if output, err := probe.CombinedOutput(); err != nil {
		t.Fatalf("run fake GitHub CLI: %v: %s", err, output)
	}
	if err := os.WriteFile(fixturePath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, runErr := cmd.Output()
	result := ciRerunHelperResult{stdout: string(stdout)}
	if runErr != nil {
		result.stderr = runErr.Error()
		if exit, ok := runErr.(*exec.ExitError); ok {
			result.stderr += ": " + string(exit.Stderr)
			result.exitCode = exit.ExitCode()
		}
	}
	updated, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(updated, fixture); err != nil {
		t.Fatalf("read fixture updates: %v", err)
	}
	return result
}

func validCIRerunFixture() ciRerunFixture {
	return ciRerunFixture{
		PR: map[string]any{
			"number": 17, "state": "open",
			"head": map[string]any{"sha": "source-head-sha", "ref": "branch", "repo": map[string]any{"full_name": "owner/repo"}},
			"base": map[string]any{"repo": map[string]any{"full_name": "owner/repo"}},
		},
		Run: map[string]any{
			"id": 12345, "run_attempt": 1, "status": "completed", "conclusion": "failure",
			"event": "pull_request", "head_sha": "tested-merge-sha",
			"repository":    map[string]any{"full_name": "owner/repo"},
			"pull_requests": []any{map[string]any{"number": 17, "head": map[string]any{"sha": "source-head-sha"}}},
		},
		Jobs: []map[string]any{{
			"id": 987654321, "run_id": 12345, "run_attempt": 1,
			"name": "opencode-canary", "status": "completed", "conclusion": "failure", "head_sha": "tested-merge-sha",
		}},
		Mode: "accept",
	}
}

const ciRerunFakeGH = `#!/usr/bin/env python3
import json, os, sys
path = os.environ['CI_RERUN_FIXTURE']
with open(path) as f: state = json.load(f)
args = sys.argv[1:]
state['calls'] = state.get('calls') or []
state['calls'].append(' '.join(args))
with open(path, 'w') as f: json.dump(state, f)
if args[:1] != ['api']:
    sys.exit(9)
method = 'GET'
route = None
i = 1
while i < len(args):
    if args[i] in ('--method', '-X'):
        method = args[i + 1].upper(); i += 2
    elif args[i] == '-H':
        i += 2
    elif not args[i].startswith('-'):
        route = args[i]; i += 1
    else:
        i += 1
if method == 'POST':
    state['actions'] = state.get('actions') or []
    state['actions'].append(method + ' ' + route)
    mode = state.get('mode')
    if mode in ('accept', 'ambiguous'):
        state['run']['run_attempt'] += 1
        state['run']['status'] = 'in_progress'
        state['run']['conclusion'] = None
        old = state['jobs'][0]
        other_jobs = [job for job in state['jobs'] if job['id'] != old['id']]
        state['jobs'] = [{
            'id': old['id'] + 1, 'run_id': old['run_id'],
            'run_attempt': state['run']['run_attempt'], 'name': old['name'],
            'status': 'queued', 'conclusion': None, 'head_sha': old.get('head_sha')
        }, {'id': old['id'] + 2, 'run_id': old['run_id'],
            'run_attempt': state['run']['run_attempt'], 'name': 'quality-gate',
            'status': 'queued', 'conclusion': None, 'head_sha': old.get('head_sha')}] + other_jobs
    with open(path, 'w') as f: json.dump(state, f)
    if mode in ('denied', 'ambiguous', 'uncertain'):
        message = 'HTTP 403 Resource not accessible by integration' if mode == 'denied' else 'request result intentionally ambiguous'
        print(message, file=sys.stderr)
        sys.exit(1)
    print('{}')
    sys.exit(0)
if route.endswith('/pulls/17'):
    print(json.dumps(state['pr']))
elif route.endswith('/actions/runs/12345') and '/jobs' not in route:
    print(json.dumps(state['run']))
elif '/actions/runs/12345/jobs' in route:
    print(json.dumps({'total_count': len(state['jobs']), 'jobs': state['jobs']}))
else:
    print('unknown read route', file=sys.stderr); sys.exit(2)
with open(path, 'w') as f: json.dump(state, f)
`
