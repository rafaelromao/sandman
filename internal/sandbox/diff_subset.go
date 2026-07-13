package sandbox

import (
	"bufio"
	"fmt"
	"strings"
)

// DiffFile is one file's worth of changes between two refs. The Hunks
// slice captures the `@@` hunk header lines verbatim so a downstream
// pre-filter can compare (file, hunk) pairs between two diffs.
type DiffFile struct {
	Path  string
	Hunks []string
}

// DiffSet is the set of changed files and hunks between two refs. The
// shape mirrors `git diff a..b` after parsing `diff --git` headers and
// `@@` hunk markers; see DiffSubset for the constructor.
type DiffSet struct {
	Files []DiffFile
}

// DiffSubset runs `git diff a b` in repoDir and returns the parsed
// (file, hunk) set. The diff is the symmetric difference (changes on
// either side); the L1 predicate (gitMergeBaseIsAncestor) is checked
// separately at the call site. A repoDir that is not a git working
// copy surfaces the underlying `git diff` error wrapped with the args
// for debuggability.
func DiffSubset(repoDir, a, b string) (DiffSet, error) {
	out, err := runGitCommand(repoDir, "diff", a, b)
	if err != nil {
		return DiffSet{}, fmt.Errorf("git diff %s %s: %w", a, b, err)
	}
	return parseDiffSubset(string(out)), nil
}

// parseDiffSubset consumes the textual output of `git diff` and returns
// the (file, hunk) set. It is exposed for direct testing in this package
// but not part of the package's public surface.
func parseDiffSubset(raw string) DiffSet {
	var out DiffSet
	scanner := bufio.NewScanner(strings.NewReader(raw))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var current *DiffFile
	var pendingPath string
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "diff --git "):
			if current != nil {
				out.Files = append(out.Files, *current)
			}
			current = nil
			pendingPath = extractPathFromDiffGitHeader(line)
		case strings.HasPrefix(line, "+++ "):
			if current != nil {
				out.Files = append(out.Files, *current)
				current = nil
			}
			// prefer the +++ path; fall back to the diff --git header path
			path := strings.TrimPrefix(line, "+++ ")
			path = strings.TrimPrefix(path, "b/")
			if path == "/dev/null" {
				path = pendingPath
			}
			if path != "" {
				pendingPath = path
			}
		case strings.HasPrefix(line, "--- "):
			// skip — path comes from +++
		case strings.HasPrefix(line, "@@"):
			if current == nil && pendingPath != "" {
				current = &DiffFile{Path: pendingPath}
				pendingPath = ""
			}
			if current != nil {
				current.Hunks = append(current.Hunks, strings.TrimSpace(line))
			}
		}
	}
	if current != nil {
		out.Files = append(out.Files, *current)
	}
	return out
}

// extractPathFromDiffGitHeader returns the destination path (b/foo) from
// a `diff --git a/foo b/foo` header line.
func extractPathFromDiffGitHeader(line string) string {
	parts := strings.Fields(line)
	if len(parts) < 4 {
		return ""
	}
	dst := parts[3]
	return strings.TrimPrefix(dst, "b/")
}
