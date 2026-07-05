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

// TestDocGuard_ReviewRunPositivePhrasing is the positive side of the
// docguard: it asserts that key canonical phrasing exists in
// CONTEXT.md, docs/usage/portal.md, and the in-package reviewRunIDFor
// doc comment. A future drift that removes these phrases will fail
// the build even if no forbidden wording has been added.
func TestDocGuard_ReviewRunPositivePhrasing(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Fatalf("locate repo root: %v", err)
	}

	checks := []struct {
		path   string
		needle string
	}{
		{filepath.Join(root, "CONTEXT.md"), "ADR-0030"},
		{filepath.Join(root, "CONTEXT.md"), "<shortid>-<ts>-PR<pr>"},
		{filepath.Join(root, "CONTEXT.md"), "<shortid>-<ts>-<linkedIssue>-PR<pr>"},
		{filepath.Join(root, "docs/usage/portal.md"), "ADR-0030"},
		{filepath.Join(root, "internal/review", "runid.go"), "ADR-0030"},
		{filepath.Join(root, "internal/review", "runid.go"), "reviewRunIDFor"},
	}

	for _, c := range checks {
		body, err := os.ReadFile(c.path)
		if err != nil {
			t.Errorf("read %s: %v", c.path, err)
			continue
		}
		if !strings.Contains(string(body), c.needle) {
			rel, _ := filepath.Rel(root, c.path)
			t.Errorf("%s must contain %q per issue #1552", rel, c.needle)
		}
	}
}

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
	// The docguard test file and its shell mirror must reference the
	// forbidden phrases to assert on them; exempt both from the scan.
	if rel == "internal/review/docguard_test.go" || rel == "scripts/check-review-docs.sh" {
		return nil
	}
	// Test files may contain forbidden phrases only as part of an
	// explicit negative-check (e.g. TestReviewRunIDFor_NoLiteralReview).
	// The allow-list below covers the legitimate phrasings ("negative
	// check", "must never", "never return", "never writes"); any other
	// forbidden phrase on a _test.go line is still flagged.
	isTest := strings.HasSuffix(rel, "_test.go")
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	lineNo := 0
	for scanner.Scan() {
		_ = isTest
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

// TestADRSelfPostFilter_DocumentsNewModel pins the positive phrasing of
// the self-post filter section in ADR-0014 after the new model
// introduced by issues #1756, #1757, and #1759. The ADR must use the
// canonical phrases for the run-log grep, the bot's own review run
// log, the per-PR scoping, and the on-disk composite key. A future
// drift that loses any of these phrases will fail the build even if
// no forbidden wording has been added.
func TestADRSelfPostFilter_DocumentsNewModel(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Fatalf("locate repo root: %v", err)
	}
	adrPath := filepath.Join(root, "docs", "adr", "0014-sandman-review-daemon-and-guard.md")
	body, err := os.ReadFile(adrPath)
	if err != nil {
		t.Fatalf("read ADR-0014: %v", err)
	}
	text := string(body)
	for _, phrase := range []string{
		"run-log",
		"bot's own review run log",
		"per-PR",
		"pr-<N>-<sha>",
	} {
		if !strings.Contains(text, phrase) {
			t.Errorf("ADR-0014 must contain %q per issues #1756/#1757/#1759", phrase)
		}
	}
}

// TestADRSelfPostFilter_NoLongerReferencesWrapper pins the negative
// side: ADR-0014 must not present the old `record_review_posted`
// wrapper, the old `Step 4b` ownership, or the skill-side ownership
// claim in canonical-style prose. The wrapper names are allowed only
// inside the §Ownership note (issue #1757) historical-context
// paragraph; anywhere else in the file the wrapper references must
// be absent.
//
// The test reads ADR-0014 line by line, classifies each line by the
// ADR heading structure, and flags any forbidden phrase that lives
// outside the §Ownership note paragraph.
func TestADRSelfPostFilter_NoLongerReferencesWrapper(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Fatalf("locate repo root: %v", err)
	}
	adrPath := filepath.Join(root, "docs", "adr", "0014-sandman-review-daemon-and-guard.md")
	body, err := os.ReadFile(adrPath)
	if err != nil {
		t.Fatalf("read ADR-0014: %v", err)
	}
	lines := strings.Split(string(body), "\n")

	forbidden := []string{
		"record_review_posted",
		"Step 4b",
	}

	inOwnershipNote := false
	for i, line := range lines {
		// A new H2 (`## `) or H3 (`### `) heading closes any
		// previously-open historical-context paragraph.
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "### ") || strings.HasPrefix(trimmed, "## ") {
			inOwnershipNote = strings.HasPrefix(trimmed, "### Ownership note (issue #1757)")
		}
		if !inOwnershipNote {
			for _, phrase := range forbidden {
				if strings.Contains(line, phrase) {
					t.Errorf("ADR-0014 line %d: forbidden canonical-style wrapper reference %q outside §Ownership note (issue #1757) historical-context paragraph", i+1, phrase)
				}
			}
		}
	}
}

// TestADR_CrossReferencesConsistent asserts the cross-ADR consistency
// invariant: only ADR-0014 may name the old self-post wrapper, the
// step-4b wrapper, the record_review_posted helper, or the run-log
// grep helper. No other ADR under docs/adr/ should reference the
// recording site or the SelfPostStore. If a future ADR introduces a
// new mention of SelfPostStore, this test will surface it for review.
//
// The test also pins that any reference to the wrapper names inside
// ADR-0014 lives only in the §Ownership note paragraph.
func TestADR_CrossReferencesConsistent(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Fatalf("locate repo root: %v", err)
	}
	adrDir := filepath.Join(root, "docs", "adr")
	entries, err := os.ReadDir(adrDir)
	if err != nil {
		t.Fatalf("read adr dir: %v", err)
	}

	storeOrWrapperPhrases := []string{
		"SelfPostStore",
		"self-posted.json",
		"record_review_posted",
		"Step 4b",
		"extractBodiesFromLog",
	}

	const adr0014 = "0014-sandman-review-daemon-and-guard.md"
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		if e.Name() == adr0014 {
			continue
		}
		path := filepath.Join(adrDir, e.Name())
		body, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("read %s: %v", e.Name(), err)
			continue
		}
		for _, phrase := range storeOrWrapperPhrases {
			if strings.Contains(string(body), phrase) {
				t.Errorf("ADR %s must not reference %q; only ADR-0014 owns the SelfPostStore / wrapper references", e.Name(), phrase)
			}
		}
	}

	// ADR-0014's wrapper references must live inside the
	// §Ownership note historical-context paragraph.
	adrBody, err := os.ReadFile(filepath.Join(adrDir, adr0014))
	if err != nil {
		t.Fatalf("read ADR-0014: %v", err)
	}
	lines := strings.Split(string(adrBody), "\n")
	inOwnershipNote := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "### ") || strings.HasPrefix(trimmed, "## ") {
			inOwnershipNote = strings.HasPrefix(trimmed, "### Ownership note (issue #1757)")
		}
		if inOwnershipNote {
			continue
		}
		for _, phrase := range []string{"record_review_posted", "Step 4b"} {
			if strings.Contains(line, phrase) {
				t.Errorf("ADR-0014 line %d: wrapper reference %q outside §Ownership note historical-context paragraph", i+1, phrase)
			}
		}
	}
}

// TestCONTEXT_GlossaryHasNewTerms asserts that CONTEXT.md carries
// glossary entries (or pointers) for `Review run log` and
// `SelfPostStore` that reflect the new self-post filter contract
// introduced by issues #1756, #1757, and #1759. The `Review run
// log` entry may be a pointer to the existing `Saved Run Log`
// entry (which already pins the on-disk path); the `SelfPostStore`
// entry may be a pointer to the `Review daemon state` paragraph,
// but the paragraph itself must reflect the new contract — it must
// NOT describe the old `pr-review SKILL.md Step 4 wrapper` claim.
func TestCONTEXT_GlossaryHasNewTerms(t *testing.T) {
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

	mustContain(t, text, "**Review run log**:")
	mustContain(t, text, "**SelfPostStore**:")

	// Positive phrasing on the new SelfPostStore contract.
	mustContain(t, text, "(prNumber, sha256(body))")

	// Negative phrasing: the stale claim that the skill-side
	// wrapper records the hash at posting time must not appear.
	if strings.Contains(text, "SKILL.md Step 4 wrapper records the hash at posting time") {
		t.Errorf("CONTEXT.md must not describe the legacy pr-review SKILL.md Step 4 wrapper claim (issues #1757, #1759)")
	}
}
