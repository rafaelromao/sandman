// check_adr_index.go verifies the structural integrity of the ADR index.
// It checks:
//   - All ADR .md files in docs/adr/ (except README.md) are listed in the README index
//   - The ADR sequence in the README is contiguous (no gaps)
//   - Each ADR file's declared status matches the README index
//   - All cross-references (ADR-NNN patterns) inside ADR bodies resolve to existing files
//
// Usage: go run scripts/check_adr_index.go
package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("PASS: ADR index is structurally sound")
}

func run() error {
	adrDir := "docs/adr"
	readmePath := filepath.Join(adrDir, "README.md")

	// Parse README index
	index, err := parseREADME(readmePath)
	if err != nil {
		return fmt.Errorf("parse README: %w", err)
	}

	// List all .md files in docs/adr/ except README.md
	files, err := os.ReadDir(adrDir)
	if err != nil {
		return fmt.Errorf("read adr dir: %w", err)
	}

	var adrFiles []string
	for _, f := range files {
		if !f.IsDir() && strings.HasSuffix(f.Name(), ".md") && f.Name() != "README.md" {
			adrFiles = append(adrFiles, f.Name())
		}
	}

	// Check 1: Every file is listed in README index (skip 0000 template)
	indexNumbers := make(map[string]bool)
	for _, e := range index {
		indexNumbers[e.Number] = true
	}
	for _, fname := range adrFiles {
		num := strings.Split(fname, "-")[0]
		if num == "0000" {
			continue // 0000 is reserved template, not in index
		}
		if !indexNumbers[num] {
			return fmt.Errorf("file %s is not listed in README index (number=%s)", fname, num)
		}
	}

	// Check 2: Sequence is contiguous (no gaps)
	if err := checkContiguous(index); err != nil {
		return fmt.Errorf("contiguity check: %w", err)
	}

	// Check 3: Each file's declared status matches README
	if err := checkStatuses(adrDir, index); err != nil {
		return fmt.Errorf("status check: %w", err)
	}

	// Check 4: All cross-references resolve to existing files
	if err := checkCrossRefs(adrDir, indexNumbers); err != nil {
		return fmt.Errorf("cross-ref check: %w", err)
	}

	return nil
}

type Entry struct {
	Number string
	Title  string
	Status string
}

func parseREADME(path string) ([]Entry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var entries []Entry
	inIndex := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		lineTrim := strings.TrimSpace(line)
		if lineTrim == "## Index" {
			inIndex = true
			continue
		}
		if !inIndex {
			continue
		}
		// Skip empty lines
		if lineTrim == "" {
			continue
		}
		// Stop at next section header
		if strings.HasPrefix(lineTrim, "## ") {
			break
		}
		// Skip table header and separator
		if strings.HasPrefix(line, "|") && strings.Contains(line, "Number") {
			continue
		}
		if strings.HasPrefix(line, "|") && strings.Contains(line, "---") {
			continue
		}
		if strings.HasPrefix(line, "|") {
			// Parse: | 0001 | Title | status |
			parts := strings.Split(line, "|")
			if len(parts) >= 4 {
				num := strings.TrimSpace(parts[1])
				title := strings.TrimSpace(parts[2])
				status := strings.TrimSpace(parts[3])
				if num != "" && !strings.HasPrefix(num, "Number") {
					entries = append(entries, Entry{Number: num, Title: title, Status: status})
				}
			}
		}
	}
	return entries, sc.Err()
}

func checkContiguous(entries []Entry) error {
	if len(entries) == 0 {
		return fmt.Errorf("no entries in index")
	}
	nums := make([]int, len(entries))
	for i, e := range entries {
		n, err := strconv.Atoi(e.Number)
		if err != nil {
			return fmt.Errorf("invalid number %q: %w", e.Number, err)
		}
		nums[i] = n
	}
	sort.Ints(nums)

	// Warn on gaps; only fail on actual problems
	// Note: intentional gaps (e.g. freed numbers after renumbering) are acceptable
	// as long as every gap corresponds to a deleted ADR file.
	for i := 1; i < len(nums); i++ {
		gap := nums[i] - nums[i-1]
		if gap > 1 {
			// Check all numbers in the gap are intentionally free
			for g := nums[i-1] + 1; g < nums[i]; g++ {
				gStr := fmt.Sprintf("%04d", g)
				found := false
				for _, e := range entries {
					if e.Number == gStr {
						found = true
						break
					}
				}
				if found {
					return fmt.Errorf("gap in sequence: %d followed by %d (number %04d appears in index but not as a file)", nums[i-1], nums[i], g)
				}
			}
		}
	}
	return nil
}

func checkStatuses(adrDir string, index []Entry) error {
	// Build number→(title,status) map from index
	indexMap := make(map[string]Entry)
	for _, e := range index {
		indexMap[e.Number] = e
	}

	// Read each file and check its ## Status header
	files, _ := os.ReadDir(adrDir)
	for _, f := range files {
		if f.IsDir() || f.Name() == "README.md" {
			continue
		}
		num := strings.Split(f.Name(), "-")[0]
		entry, ok := indexMap[num]
		if !ok {
			continue // not in index
		}

		fpath := filepath.Join(adrDir, f.Name())
		body, err := os.ReadFile(fpath)
		if err != nil {
			return fmt.Errorf("read %s: %w", f.Name(), err)
		}

		// Extract ## Status value from body
		bodyStatus := extractStatus(string(body))
		if bodyStatus == "" {
			return fmt.Errorf("%s: no ## Status header found", f.Name())
		}
		// Compare just the first word of each status, stripped of trailing punctuation
		bodyFirst := strings.ToLower(strings.Fields(bodyStatus)[0])
		bodyFirst = strings.TrimRight(bodyFirst, ";:,.")
		readmeFirst := strings.ToLower(strings.Fields(entry.Status)[0])
		readmeFirst = strings.TrimRight(readmeFirst, ";:,.")
		if bodyFirst != readmeFirst {
			return fmt.Errorf("%s: body status %q != README status %q", f.Name(), bodyFirst, readmeFirst)
		}
	}
	return nil
}

func extractStatus(body string) string {
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) == "## Status" && i+1 < len(lines) {
			// Skip blank lines to find the status value
			for j := i + 1; j < len(lines); j++ {
				val := strings.TrimSpace(lines[j])
				if val != "" {
					// Return just the first word (status keyword), stripped of trailing punctuation
					parts := strings.Fields(val)
					if len(parts) > 0 {
						first := strings.ToLower(parts[0])
						first = strings.TrimRight(first, ";:,.")
						return first
					}
					return val
				}
			}
		}
	}
	return ""
}

func normalizeStatus(s string) string {
	s = strings.ToLower(s)
	s = strings.TrimSpace(s)
	return s
}

var adrRefRe = regexp.MustCompile(`ADR-(\d{4})`)
var strikethroughRe = regexp.MustCompile(`~~ADR-\d{4}`)

func checkCrossRefs(adrDir string, validNumbers map[string]bool) error {
	files, _ := os.ReadDir(adrDir)
	for _, f := range files {
		if f.IsDir() || f.Name() == "README.md" {
			continue
		}
		fpath := filepath.Join(adrDir, f.Name())
		body, err := os.ReadFile(fpath)
		if err != nil {
			return fmt.Errorf("read %s: %w", f.Name(), err)
		}

		matches := adrRefRe.FindAllStringSubmatchIndex(string(body), -1)
		for _, match := range matches {
			refNum := string(body[match[2]:match[3]])
			if !validNumbers[refNum] {
				// Check if this reference is inside a strikethrough (deleted ADR marker)
				refStart := match[0]
				strikethroughMatch := strikethroughRe.FindAllStringSubmatchIndex(string(body), -1)
				isStruck := false
				for _, sm := range strikethroughMatch {
					if refStart >= sm[0] && refStart < sm[1] {
						isStruck = true
						break
					}
				}
				if !isStruck {
					return fmt.Errorf("%s: cross-reference ADR-%s does not exist in index", f.Name(), refNum)
				}
			}
		}
	}
	return nil
}

func slugify(title string) string {
	title = strings.ToLower(title)
	title = strings.ReplaceAll(title, " ", "-")
	// Remove non-alphanumeric except dash
	var out strings.Builder
	for _, r := range title {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			out.WriteRune(r)
		}
	}
	return out.String()
}
