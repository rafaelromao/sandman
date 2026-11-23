package sandman

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// readRepoFile reads a file with its whitespace collapsed, so a phrase matches
// wherever the Markdown happens to wrap it.
func readRepoFile(t *testing.T, elems ...string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(elems...))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(strings.Fields(string(data)), " ")
}

var glossaryTermPattern = regexp.MustCompile(`\*\*[^*]+\*\*:`)

// glossaryEntry returns the glossary entry for term: from its bold marker to the
// next entry's, or the end of the text.
func glossaryEntry(t *testing.T, text, term string) string {
	t.Helper()
	marker := "**" + term + "**:"
	start := strings.Index(text, marker)
	if start < 0 {
		t.Fatalf("glossary has no %q entry", term)
	}
	body := text[start+len(marker):]
	if next := glossaryTermPattern.FindStringIndex(body); next != nil {
		return text[start : start+len(marker)+next[0]]
	}
	return text[start:]
}

// The blind-review capability, its standalone cycle and the merge gate all
// describe the same two behaviours in user-facing terms: the one tolerated
// host footer block and the REST fallback chosen by probing the capability.
func TestBlindReviewDocsDescribeHostFooterToleranceAndRESTFallback(t *testing.T) {
	for path, phrases := range map[string][]string{
		filepath.Join("blind-review", "SKILL.md"): {
			"exactly one recognised host footer block",
			"REST equivalents",
			"probe",
			"never from the host or product name",
			"never posts a replacement",
		},
		filepath.Join("review-cycle", "blind-cycle.md"): {
			"exactly one recognised host footer block",
			"REST equivalents",
		},
		filepath.Join("pr-merge", "SKILL.md"): {
			"exactly one recognised host footer block",
			"REST equivalents",
			"required checks green",
		},
	} {
		text := readRepoFile(t, path)
		for _, phrase := range phrases {
			if !strings.Contains(text, phrase) {
				t.Errorf("%s does not describe %q", path, phrase)
			}
		}
	}
}

func TestBlindReviewEvidenceGlossaryAndADRDescribeTheTolerance(t *testing.T) {
	entry := glossaryEntry(t, readRepoFile(t, "..", "..", "..", "CONTEXT.md"), "Blind review evidence")
	if strings.Contains(entry, "**Review run**") || strings.Contains(entry, "**Blind review**:") {
		t.Fatalf("the glossary entry boundary leaked into a neighbouring entry: %.80q", entry)
	}
	for _, phrase := range []string{"recognised host footer block", "REST", "never by host"} {
		if !strings.Contains(entry, phrase) {
			t.Errorf("the Blind review evidence glossary entry does not describe %q", phrase)
		}
	}
	adr := readRepoFile(t, "..", "..", "..", "docs", "adr", "0059-blind-review-as-the-default-pull-request-review.md")
	for _, phrase := range []string{"Later change (annotated 2026-11-23, #3021)", "recognised host footer block", "REST"} {
		if !strings.Contains(adr, phrase) {
			t.Errorf("ADR-0059 does not carry %q", phrase)
		}
	}
	if agents := readRepoFile(t, "..", "..", "..", "AGENTS.md"); !strings.Contains(agents, "recognised host footer block") {
		t.Error("AGENTS.md Blind review lifecycle must mention the tolerated host footer block")
	}
}
