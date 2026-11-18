package blindreview_test

import (
	"bytes"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestGHGraphQLPaginationAgainstLocalAPI(t *testing.T) {
	gh, err := exec.LookPath("gh")
	if err != nil {
		t.Skipf("gh unavailable: %v", err)
	}

	type request struct {
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
		if body.Variables.Owner != "owner" || body.Variables.Repo != "repo" || body.Variables.Number != 17 {
			http.Error(w, "unexpected GraphQL variables", http.StatusBadRequest)
			return
		}
		cursor := ""
		if body.Variables.EndCursor != nil {
			cursor = *body.Variables.EndCursor
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

	certPath := filepath.Join(t.TempDir(), "mock-api-ca.pem")
	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if err := os.WriteFile(certPath, cert, 0o600); err != nil {
		t.Fatal(err)
	}
	parsedURL := strings.TrimPrefix(server.URL, "https://")
	query := `query($owner:String!,$repo:String!,$number:Int!,$endCursor:String){repository(owner:$owner,name:$repo){pullRequest(number:$number){closingIssuesReferences(first:100,after:$endCursor){nodes{number} pageInfo{hasNextPage endCursor}}}}}`
	home := t.TempDir()
	cmd := exec.CommandContext(t.Context(), gh, "api", "graphql", "--paginate",
		"-F", "owner=owner", "-F", "repo=repo", "-F", "number=17", "-f", "query="+query)
	cmd.Env = append(os.Environ(),
		"GH_HOST="+parsedURL,
		"GH_ENTERPRISE_TOKEN=fixture-token",
		"GH_TOKEN=",
		"GITHUB_TOKEN=",
		"GH_PROMPT_DISABLED=1",
		"GH_NO_UPDATE_NOTIFIER=1",
		"SSL_CERT_FILE="+certPath,
		"HOME="+home,
		"XDG_CONFIG_HOME="+filepath.Join(home, ".config"),
		"GH_CONFIG_DIR="+filepath.Join(home, ".config", "gh"),
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gh api GraphQL pagination: %v\n%s", err, output)
	}
	type page struct {
		Data struct {
			Repository struct {
				PullRequest struct {
					ClosingIssuesReferences struct {
						Nodes []struct {
							Number int `json:"number"`
						} `json:"nodes"`
					} `json:"closingIssuesReferences"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
	}
	var pages []page
	decoder := json.NewDecoder(bytes.NewReader(output))
	for {
		var decoded page
		err := decoder.Decode(&decoded)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("decode paginated GraphQL output: %v\n%s", err, output)
		}
		pages = append(pages, decoded)
	}
	if len(pages) != 2 || len(pages[1].Data.Repository.PullRequest.ClosingIssuesReferences.Nodes) != 1 ||
		pages[1].Data.Repository.PullRequest.ClosingIssuesReferences.Nodes[0].Number != 42 {
		t.Fatalf("unexpected paginated GraphQL output: %s", output)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(cursors) != 2 || cursors[0] != "" || cursors[1] != "cursor-1" {
		t.Fatalf("gh did not follow the cursor chain: %#v", cursors)
	}
}
