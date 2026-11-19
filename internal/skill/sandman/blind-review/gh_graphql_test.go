package blindreview_test

import (
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
)

func TestPromptHelperPreservesLiteralGraphQLIdentifiers(t *testing.T) {
	gh, err := exec.LookPath("gh")
	if err != nil {
		t.Skipf("gh unavailable: %v", err)
	}
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skipf("jq unavailable: %v", err)
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skipf("POSIX shell unavailable: %v", err)
	}

	type request struct {
		Variables struct {
			Owner     string  `json:"owner"`
			Repo      string  `json:"repo"`
			Number    int     `json:"number"`
			EndCursor *string `json:"endCursor"`
		} `json:"variables"`
	}
	type graphQLCall struct {
		owner  string
		repo   string
		cursor string
	}
	var mu sync.Mutex
	var calls []graphQLCall
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/graphql") {
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("Authorization"); !strings.Contains(got, "fixture-token") {
			http.Error(w, "missing fixture authorization", http.StatusUnauthorized)
			return
		}
		var body request
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid GraphQL request", http.StatusBadRequest)
			return
		}
		validRepository := (body.Variables.Owner == "owner" && body.Variables.Repo == "repo") ||
			(body.Variables.Owner == "123" && body.Variables.Repo == "true") ||
			(body.Variables.Owner == "true" && body.Variables.Repo == "123")
		if !validRepository || body.Variables.Number != 17 {
			http.Error(w, "unexpected GraphQL variables", http.StatusBadRequest)
			return
		}
		cursor := ""
		if body.Variables.EndCursor != nil {
			cursor = *body.Variables.EndCursor
		}
		mu.Lock()
		calls = append(calls, graphQLCall{
			owner: body.Variables.Owner, repo: body.Variables.Repo, cursor: cursor,
		})
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

	certPath := filepath.Join(t.TempDir(), "mock-api-ca.pem")
	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if err := os.WriteFile(certPath, cert, 0o600); err != nil {
		t.Fatal(err)
	}
	helperDir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	fixture := t.TempDir()
	worktree := filepath.Join(fixture, "worktree")
	if out, err := exec.Command("git", "init", worktree).CombinedOutput(); err != nil {
		t.Fatalf("initialize prompt fixture worktree: %v\n%s", err, out)
	}
	binDir := filepath.Join(fixture, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(worktree, 0o700); err != nil {
		t.Fatal(err)
	}
	pr := `{"number":17,"title":"Container compatibility","body":"Closes #99","state":"OPEN","headRefOid":"abc","baseRefName":"main"}`
	if err := os.WriteFile(filepath.Join(fixture, "pr.json"), []byte(pr), 0o600); err != nil {
		t.Fatal(err)
	}
	fakeGH := `#!/bin/sh
set -eu
if [ "${1-}" = api ] && [ "${2-}" = graphql ]; then
  exec "$REAL_GH" "$@"
fi
case "${1-} ${2-}" in
  "pr view") cat "$FIXTURE_DIR/pr.json" ;;
  "issue view") printf '%s\n' '{"body":"Native acceptance criterion from issue 42"}' ;;
  "api --paginate") printf '[]\n' ;;
  *) exec "$REAL_GH" "$@" ;;
esac
`
	if err := os.WriteFile(filepath.Join(binDir, "gh"), []byte(fakeGH), 0o700); err != nil {
		t.Fatal(err)
	}

	parsedURL := strings.TrimPrefix(server.URL, "https://")
	home := t.TempDir()
	configDir := filepath.Join(home, ".config", "gh")
	env := append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"REAL_GH="+gh,
		"FIXTURE_DIR="+fixture,
		"GH_HOST="+parsedURL,
		"GH_ENTERPRISE_TOKEN=fixture-token",
		"GH_TOKEN=",
		"GITHUB_TOKEN=",
		"GH_PROMPT_DISABLED=1",
		"GH_NO_UPDATE_NOTIFIER=1",
		"SSL_CERT_FILE="+certPath,
		"HOME="+home,
		"XDG_CONFIG_HOME="+filepath.Join(home, ".config"),
		"GH_CONFIG_DIR="+configDir,
	)
	repositories := []string{"owner/repo", "123/true", "true/123"}
	for _, repository := range repositories {
		cmd := exec.CommandContext(t.Context(), "sh",
			filepath.Join(helperDir, "review-prompt-v1.sh"),
			"--repository", repository, "--pull-request", "17",
			"--head-sha", "abc", "--run-dir", filepath.Join(fixture, "report"))
		cmd.Dir = worktree
		cmd.Env = env
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("render prompt for repository %q: %v\n%s", repository, err, output)
		}
		prompt := string(output)
		if !strings.Contains(prompt, "Native acceptance criterion from issue 42") ||
			strings.Contains(prompt, "Fallback criterion 99 must not win") {
			t.Fatalf("prompt for repository %q did not preserve native linkage:\n%s", repository, prompt)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(calls) != len(repositories)*2 {
		t.Fatalf("gh did not paginate each repository identity: %#v", calls)
	}
	for i, repository := range repositories {
		owner, repo, _ := strings.Cut(repository, "/")
		first, second := calls[i*2], calls[i*2+1]
		if first.owner != owner || first.repo != repo || first.cursor != "" ||
			second.owner != owner || second.repo != repo || second.cursor != "cursor-1" {
			t.Fatalf("gh sent invalid variables or cursor sequence for %q: %#v, %#v", repository, first, second)
		}
	}
}
