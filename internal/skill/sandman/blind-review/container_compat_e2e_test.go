//go:build e2e

package blindreview_test

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/sandbox"
	"github.com/rafaelromao/sandman/internal/scaffold"
)

type containerCompatPrompter struct{}

func (containerCompatPrompter) Confirm(string) (bool, error) { return true, nil }
func (containerCompatPrompter) Select(_ string, choices []string) (string, error) {
	return choices[0], nil
}

func TestBlindReviewDefaultContainerCompatibility(t *testing.T) {
	if _, err := exec.LookPath("podman"); err != nil {
		t.Skipf("podman unavailable: %v", err)
	}
	if out, err := exec.Command("podman", "version").CombinedOutput(); err != nil {
		t.Skipf("podman unavailable: %v\n%s", err, out)
	}

	type graphQLRequest struct {
		Variables struct {
			Owner     string  `json:"owner"`
			Repo      string  `json:"repo"`
			Number    int     `json:"number"`
			EndCursor *string `json:"endCursor"`
		} `json:"variables"`
	}
	var mu sync.Mutex
	var cursors []string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/graphql" {
			http.Error(w, "unexpected GitHub API path", http.StatusNotFound)
			return
		}
		if !strings.Contains(r.Header.Get("Authorization"), "fixture-token") {
			http.Error(w, "missing fixture authorization", http.StatusUnauthorized)
			return
		}
		var request graphQLRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "invalid GraphQL request", http.StatusBadRequest)
			return
		}
		variables := request.Variables
		if variables.Owner != "owner" || variables.Repo != "repo" || variables.Number != 17 {
			http.Error(w, "unexpected GraphQL variables", http.StatusBadRequest)
			return
		}
		cursor := ""
		if variables.EndCursor != nil {
			cursor = *variables.EndCursor
		}
		mu.Lock()
		cursors = append(cursors, cursor)
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if cursor == "" {
			_, _ = fmt.Fprintln(w, `{"data":{"repository":{"pullRequest":{"closingIssuesReferences":{"nodes":[],"pageInfo":{"hasNextPage":true,"endCursor":"cursor-1"}}}}}}`)
		} else if cursor == "cursor-1" {
			_, _ = fmt.Fprintln(w, `{"data":{"repository":{"pullRequest":{"closingIssuesReferences":{"nodes":[{"number":42}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}}}}`)
		} else {
			http.Error(w, "unexpected GraphQL cursor", http.StatusBadRequest)
		}
	}))
	defer server.Close()

	fixture := t.TempDir()
	certPath := filepath.Join(fixture, "mock-api-ca.pem")
	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if err := os.WriteFile(certPath, cert, 0600); err != nil {
		t.Fatal(err)
	}

	repo := t.TempDir()
	if err := (&scaffold.Scaffolder{}).Scaffold(
		repo, scaffold.Options{BuildTools: "generic"}, containerCompatPrompter{},
	); err != nil {
		t.Fatalf("generate default scaffold: %v", err)
	}
	dockerfile := filepath.Join(repo, ".sandman", "Dockerfile")
	generated, err := os.ReadFile(dockerfile)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(generated), "\n")
	aptLayer := -1
	for i, line := range lines {
		if strings.HasPrefix(line, "RUN apt-get update && apt-get install -y --no-install-recommends ") {
			aptLayer = i
			break
		}
	}
	if aptLayer < 0 || !strings.Contains(lines[aptLayer], " gh ") || !strings.Contains(lines[aptLayer], " jq ") {
		t.Fatal("generated default scaffold lacks the Debian gh/jq shared-tools layer")
	}
	// Keep the generated Debian base and shared package layer while omitting
	// unrelated language, agent, and mise downloads from this focused fixture.
	if err := os.WriteFile(dockerfile, []byte(strings.Join(lines[:aptLayer+1], "\n")+"\n"), 0600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Minute)
	defer cancel()
	runtime := sandbox.NewContainerRuntime("podman")
	image, err := runtime.BuildImage(ctx, repo)
	if err != nil {
		t.Fatalf("build generated Debian toolchain layer: %v", err)
	}
	t.Cleanup(func() { _ = exec.Command("podman", "rmi", "-f", image).Run() })

	helperDir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	worktree := filepath.Join(fixture, "worktree")
	if err := os.MkdirAll(worktree, 0700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "init", worktree).CombinedOutput(); err != nil {
		t.Fatalf("initialize prompt fixture worktree: %v\n%s", err, out)
	}
	if err := os.MkdirAll(filepath.Join(fixture, "bin"), 0700); err != nil {
		t.Fatal(err)
	}
	fakeGH := `#!/bin/sh
set -eu
if [ "$1" = api ] && [ "$2" = graphql ]; then
  exec /usr/bin/gh "$@"
fi
case "$1 $2" in
  "pr view") cat /fixture/pr.json ;;
  "issue view")
    [ "$3" = 42 ] || { printf '%s\n' '{"body":"Fallback criterion 99 must not win"}'; exit 0; }
    printf '%s\n' '{"body":"Native acceptance criterion from issue 42"}' ;;
  "api --paginate") printf '[]\n' ;;
  *) exec /usr/bin/gh "$@" ;;
esac
`
	if err := os.WriteFile(filepath.Join(fixture, "bin", "gh"), []byte(fakeGH), 0700); err != nil {
		t.Fatal(err)
	}
	pr := `{"number":17,"title":"Container compatibility","body":"Closes #99","state":"OPEN","headRefOid":"abc","baseRefName":"main"}`
	if err := os.WriteFile(filepath.Join(fixture, "pr.json"), []byte(pr), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture, "decision.md"), []byte("## Decision\nAPPROVED\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runner := `#!/bin/sh
set -eu
export PATH=/fixture/bin:$PATH
export HOME=/tmp/gh-home
export GH_CONFIG_DIR=/tmp/gh-config
export GH_HOST="$FIXTURE_GH_HOST"
export GH_ENTERPRISE_TOKEN=fixture-token
export GH_TOKEN=
export GITHUB_TOKEN=
export GH_PROMPT_DISABLED=1
export SSL_CERT_FILE=/fixture/mock-api-ca.pem
mkdir -p "$HOME" "$GH_CONFIG_DIR"
gh --version | grep -q '^gh version 2\.23\.0 '
test "$(jq --version)" = jq-1.6
cd /fixture/worktree
sh /helpers/review-prompt-v1.sh --repository owner/repo --pull-request 17 --head-sha abc --run-dir /fixture/report > /fixture/prompt.md
grep -Fq 'Native acceptance criterion from issue 42' /fixture/prompt.md
! grep -Fq 'Fallback criterion 99 must not win' /fixture/prompt.md
python3 /helpers/test_transport.py
sh /helpers/publish-v1.sh --decision-file /fixture/decision.md --head-sha abc --print-body > /fixture/publication.md
jq -e -R 'contains("<!-- sandman-verdict:approved -->") and contains("<!-- sandman-publication:")' /fixture/publication.md >/dev/null
`
	runnerPath := filepath.Join(fixture, "run.sh")
	if err := os.WriteFile(runnerPath, []byte(runner), 0700); err != nil {
		t.Fatal(err)
	}

	ghHost := strings.TrimPrefix(server.URL, "https://")
	run := exec.CommandContext(ctx, "podman", "run", "--rm", "--network", "host",
		"--volume", fixture+":/fixture",
		"--volume", helperDir+":/helpers:ro",
		"--workdir", "/fixture/worktree",
		"--env", "FIXTURE_GH_HOST="+ghHost,
		image, "/bin/sh", "/fixture/run.sh")
	output, err := run.CombinedOutput()
	if err != nil {
		t.Fatalf("generated-container blind-review compatibility: %v\n%s", err, output)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(cursors) != 2 || cursors[0] != "" || cursors[1] != "cursor-1" {
		t.Fatalf("packaged gh did not paginate the mock GraphQL response: %#v", cursors)
	}
}
