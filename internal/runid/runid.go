package runid

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"
)

var batchesDirRoot = ".sandman/batches"

var shortIDFunc = func() string {
	return fmt.Sprintf("%04x", time.Now().UnixNano()%0xFFFF)
}

var timeFunc = func() time.Time { return time.Now() }

type Kind int

const (
	KindIssue Kind = iota
	KindReview
	KindAutoSelect
	KindPromptOnly
)

func (k Kind) String() string {
	switch k {
	case KindIssue:
		return "issue"
	case KindReview:
		return "review"
	case KindAutoSelect:
		return "auto-select"
	case KindPromptOnly:
		return "prompt-only"
	default:
		return "unknown"
	}
}

func NewBatch() (ts, shortid string, err error) {
	return NewBatchIn(batchesDirRoot)
}

func NewBatchIn(baseBatchesDir string) (ts, shortid string, err error) {
	ts = timeFunc().Format("060102150405")
	for attempt := 0; attempt < 16; attempt++ {
		shortid = shortIDFunc()
		entries, err := os.ReadDir(baseBatchesDir)
		if err != nil {
			if os.IsNotExist(err) {
				return ts, shortid, nil
			}
			return "", "", fmt.Errorf("read batches dir: %w", err)
		}
		collision := false
		for _, entry := range entries {
			if entry.IsDir() && strings.HasPrefix(entry.Name(), ts+"-"+shortid) {
				collision = true
				break
			}
		}
		if !collision {
			return ts, shortid, nil
		}
	}
	return "", "", fmt.Errorf("failed to generate unique shortid after 16 attempts")
}

func NewBatchID(kind Kind, n int, firstSubject string, ts, shortid string) string {
	switch kind {
	case KindIssue:
		if n <= 1 {
			return fmt.Sprintf("%s-%s-%s", ts, shortid, firstSubject)
		}
		return fmt.Sprintf("%s-%s-%s+%d", ts, shortid, firstSubject, n-1)
	case KindReview:
		return fmt.Sprintf("%s-%s-PR%s", ts, shortid, firstSubject)
	case KindAutoSelect:
		return fmt.Sprintf("%s-%s-auto-%d", ts, shortid, n)
	case KindPromptOnly:
		if firstSubject == "" {
			return fmt.Sprintf("%s-%s-prompt", ts, shortid)
		}
		return fmt.Sprintf("%s-%s-prompt-%s", ts, shortid, firstSubject)
	default:
		return ""
	}
}

func NewRunID(kind Kind, subject string, ts, shortid string) string {
	if kind == KindPromptOnly {
		if subject == "" {
			return fmt.Sprintf("%s-%s-prompt", ts, shortid)
		}
		return fmt.Sprintf("%s-%s-prompt-%s", ts, shortid, subject)
	}
	if subject == "" {
		return fmt.Sprintf("%s-%s", ts, shortid)
	}
	return fmt.Sprintf("%s-%s-%s", ts, shortid, subject)
}

var userRunIDRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

func IsValidUserRunID(s string) error {
	if s == "" {
		return fmt.Errorf("run id cannot be empty")
	}
	if len(s) > 64 {
		return fmt.Errorf("run id cannot exceed 64 characters")
	}
	if !userRunIDRe.MatchString(s) {
		return fmt.Errorf("run id must contain only alphanumerics, hyphens, and underscores")
	}
	return nil
}

var canonicalPrefixRe = regexp.MustCompile(`^\d{12}-[0-9a-f]{4}($|-)`)
var canonicalAutoSelectRe = regexp.MustCompile(`^\d{12}-[0-9a-f]{4}-auto-\d+$`)
var canonicalReviewRe = regexp.MustCompile(`^\d{12}-[0-9a-f]{4}(-\d+-)?-PR\d+$`)
var canonicalMultiIssueRe = regexp.MustCompile(`^\d{12}-[0-9a-f]{4}-\d+\+\d+$`)
var canonicalSingleIssueRe = regexp.MustCompile(`^\d{12}-[0-9a-f]{4}-\d+$`)
var canonicalPromptOnlySegmentRe = regexp.MustCompile(`^\d{12}-[0-9a-f]{4}-prompt(-|$)`)

func KindFromDirName(name string) (Kind, bool) {
	// Canonical format only: every classified name must start with the
	// <ts>-<sid> prefix. Anything that doesn't is rejected, so legacy
	// <sid>-<ts> and {ts}-{sid}-issues-first/... shapes return (0, false).
	if !canonicalPrefixRe.MatchString(name) {
		return 0, false
	}
	switch {
	case canonicalAutoSelectRe.MatchString(name):
		return KindAutoSelect, true
	case canonicalReviewRe.MatchString(name):
		return KindReview, true
	case canonicalMultiIssueRe.MatchString(name):
		return KindIssue, true
	case canonicalPromptOnlySegmentRe.MatchString(name):
		return KindPromptOnly, true
	case canonicalSingleIssueRe.MatchString(name):
		return KindIssue, true
	}
	// Canonical <ts>-<sid> with no other marker: treat as prompt-only
	// (with no subject / userid) so the same fallback applies.
	return KindPromptOnly, true
}
