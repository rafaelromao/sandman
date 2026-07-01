package review

import (
	"bufio"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestDocGuard_ReviewRunIdentity is the CI guard that pins the
// review-run-identity invariants from issue #1552:
//
//   - ADR-0030 is the cited source of truth for review RunIDs.
//   - docs do not describe `runs/review` or `RunID: "review"` as canonical;
//     both are legacy aliases replaced by issue #1551.
//   - review runs are described as ordinary AgentRuns with canonical
//     per-row RunIDs.
//   - inline comments in internal/review do not claim review runs use
//     a special review-only identifier.
//
// The guard scans:
//
//   - docs/**/*.md  (excluding docs/adr/** — ADRs may describe the
//     historical legacy alias as context, not as canonical);
//   - internal/review/**/*.go (function-level doc comments and
//     adjacent prose; test fixture carve-outs are explicit via the
//     "// docguard:legacy-allowed" marker).
//
// A line is flagged only when the forbidden phrase appears together
// with canonical-style framing. Historical-context mentions like
// "legacy", "alias", "replaces", "rejected", or "no longer used" are
// permitted, as is any line explicitly marked as a legacy carve-out.
func TestDocGuard_ReviewRunIdentity(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Fatalf("locate repo root: %v", err)
	}

	forbidden := []forbiddenPhrase{
		{
			phrase:    "runs/review",
			canonical: []string{"canonical", "as the run folder", "as the run directory", "is the run folder"},
			allow:     []string{"legacy", "alias", "replaced", "replaces", "rejected", "no longer used", "intentionally NOT", "intentionally not", "NOT consulted", "NOT used", "explicitly NOT", "must not", "is not canonical", "no per-row RunID folders", "no per-PR subdirectories", "no per-PR folders", "negative check", "never holds", "must NOT use"},
		},
		{
			phrase:    `RunID: "review"`,
			canonical: []string{"canonical", "use this", "use it", "writes this", "written as"},
			allow:     []string{"legacy", "alias", "replaced", "replaces", "rejected", "no longer used", "intentionally NOT", "intentionally not", "explicitly NOT", "must not", "is not canonical", "must never", "never return", "never writes", "negative check"},
		},
	}

	paths, err := scanPaths(root)
	if err != nil {
		t.Fatalf("scan paths: %v", err)
	}

	var failures []string
	for _, p := range paths {
		if err := checkFile(t, root, p, forbidden); err != nil {
			failures = append(failures, err.Error())
		}
	}
	if len(failures) > 0 {
		t.Fatalf("docguard found forbidden wording:\n%s", strings.Join(failures, "\n"))
	}
}

// forbiddenPhrase pairs a phrase with a list of canonical-style words
// (which would mean the phrase is being presented as canonical, not as
// historical context) and a list of allow-words (which mark the line
// as historical/legacy and thus permitted).
type forbiddenPhrase struct {
	phrase    string
	canonical []string
	allow     []string
}

func checkFile(t *testing.T, root, path string, forbidden []forbiddenPhrase) error {
	t.Helper()
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return nil
	}
	if strings.HasPrefix(rel, "docs/adr/") {
		return nil
	}
	if strings.HasSuffix(rel, "_test.go") {
		if rel != "internal/review/runid_test.go" {
			return nil
		}
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := scanner.Text()
		if !strings.Contains(line, "// docguard:legacy-allowed") &&
			!strings.Contains(line, "docguard:legacy-allowed") {
			for _, fp := range forbidden {
				if !strings.Contains(line, fp.phrase) {
					continue
				}
				lower := strings.ToLower(line)
				if containsAny(lower, fp.allow) {
					continue
				}
				if !containsAny(lower, fp.canonical) {
					continue
				}
				return &docguardError{path: rel, line: lineNo, msg: line}
			}
		}
	}
	return scanner.Err()
}

type docguardError struct {
	path string
	line int
	msg  string
}

func (e *docguardError) Error() string {
	return e.path + ":" + strconv.Itoa(e.line) + ": forbidden canonical-style wording: " + e.msg
}

func containsAny(s string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}

func repoRoot() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	dir := cwd
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", os.ErrNotExist
}

func scanPaths(root string) ([]string, error) {
	var paths []string
	docsRoot := filepath.Join(root, "docs")
	if err := filepath.WalkDir(docsRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if rel, _ := filepath.Rel(docsRoot, path); rel == "adr" || strings.HasPrefix(rel, "adr"+string(filepath.Separator)) {
				return fs.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".md") {
			paths = append(paths, path)
		}
		return nil
	}); err != nil {
		return nil, err
	}

	reviewRoot := filepath.Join(root, "internal", "review")
	if err := filepath.WalkDir(reviewRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if strings.HasSuffix(path, ".go") {
			paths = append(paths, path)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return paths, nil
}

// TestDocGuard_ReviewRunGlossaryExists asserts the positive side of
// issue #1552: CONTEXT.md carries a "Review run" glossary entry that
// cites ADR-0030 and pins both per-row RunID templates. If a future
// refactor removes the entry, this test catches it.
func TestDocGuard_ReviewRunGlossaryExists(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Fatalf("locate repo root: %v", err)
	}
	contextPath := filepath.Join(root, "CONTEXT.md")
	body, err := os.ReadFile(contextPath)
	if err != nil {
		t.Fatalf("read CONTEXT.md: %v", err)
	}
	text := string(body)
	mustContain(t, text, "**Review run**:")
	mustContain(t, text, "ADR-0030")
	mustContain(t, text, "<shortid>-<ts>-PR<pr>")
	mustContain(t, text, "<shortid>-<ts>-<linkedIssue>-PR<pr>")
}

func mustContain(t *testing.T, haystack, needle string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Errorf("CONTEXT.md must contain %q per issue #1552", needle)
	}
}
